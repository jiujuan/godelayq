package core

// 本文件是 TASK-R05 的用例集：验证 ConfigWatcher 的触发、合并、只认目标文件、
// rename/删除/关闭、状态保存与 WatcherError 归属、防抖窗口可改、并发读与 panic 隔离。
//
// 评审返工后补上的四组判据（详见卡 §10.2、§10.3、§10.5）：
//   - ctx 取消这条 Run 退出路径也要停计时器、关句柄（CtxCancelStopsScheduling，要求 11 的另一半）；
//   - ReloadFunc 的调用被 reloadMu 串行（ReloadCallsAreSerialized）；
//   - basename 取磁盘真实拼写，大小写错配的 -config 路径不会静默失效（ResolveDiskFileName、MatchesOnDiskNameCasing）；
//   - 第二次 Run 立即返回而不是 close-of-closed-channel panic（SecondRunDoesNotPanic）；
//   - 要求 3（同字节也调）与要求 9 的全字段透传各有断言；Close 后直接调 trigger 也短路。
//
// 两条判据约定（卡 §5 helper 约定 + 本机 Windows 时间抖动）：
//   - 一切等待都是"10ms 轮询 + 有界超时"，超时即 t.Fatal 并打印已收到的次数，绝不 sleep 后盲断言；
//   - 反向判断（"不该触发"）用带上下界的 expectStays，窗口内一超标立刻失败，不用时间等式。

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// syncBuf 是加锁的日志缓冲：slog 会写它，测试会读它，且写方在 watcher 协程里。
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// watcherFixture 是 §5 的统一测试台：在 t.TempDir() 里造一份真实的 config.yaml，
// 建 watcher 并 go Run(ctx)；onReload 记录调用次数、把一个可预设的 ReloadState 交回，
// 并可切 panicMode 复现 ReloadFunc panic。
//
// inFlight/maxInflight/hold 三个只给 TestConfigWatcher_ReloadCallsAreSerialized 用：
// hold>0 时 reload 会在体内停留一段时间并记录"同时在跑的重载数"的峰值，用来验 reloadMu 的串行化。
type watcherFixture struct {
	t         *testing.T
	dir       string
	path      string
	w         *ConfigWatcher
	cancel    context.CancelFunc
	logBuf    syncBuf
	calls     atomic.Int64
	preset    atomic.Pointer[ReloadState]
	presetErr atomic.Pointer[error]
	panicMode atomic.Bool

	hold        time.Duration
	inFlight    atomic.Int64
	maxInflight atomic.Int64
}

func newWatcherFixture(t *testing.T, debounce time.Duration) *watcherFixture {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("server:\n  port: 8080\n"), 0644); err != nil {
		t.Fatalf("创建目标文件失败: %v", err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatalf("求绝对路径失败: %v", err)
	}

	f := &watcherFixture{t: t, dir: dir, path: abs}
	def := ReloadState{Result: ReloadOK, LastAttemptAt: time.Now()}
	f.preset.Store(&def)

	logger := slog.New(slog.NewTextHandler(&f.logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	w, err := NewConfigWatcher(abs, debounce, f.reload, logger)
	if err != nil {
		t.Fatalf("NewConfigWatcher 失败: %v", err)
	}
	f.w = w

	ctx, cancel := context.WithCancel(context.Background())
	f.cancel = cancel
	go w.Run(ctx)

	t.Cleanup(func() {
		cancel()
		_ = w.Close()
	})
	return f
}

// reload 是交给 watcher 的 ReloadFunc：计数，panicMode 下 panic，
// 否则把预设状态与预设错误原样交回。hold>0 时先在体内停留，并记录同时在跑的重载数峰值。
func (f *watcherFixture) reload() (ReloadState, error) {
	f.calls.Add(1)
	if f.panicMode.Load() {
		panic("boom")
	}
	if hold := f.hold; hold > 0 {
		n := f.inFlight.Add(1)
		for {
			cur := f.maxInflight.Load()
			if n <= cur || f.maxInflight.CompareAndSwap(cur, n) {
				break
			}
		}
		time.Sleep(hold)
		f.inFlight.Add(-1)
	}
	var st ReloadState
	if p := f.preset.Load(); p != nil {
		st = *p
	}
	var err error
	if pe := f.presetErr.Load(); pe != nil {
		err = *pe
	}
	return st, err
}

// mustWrite 写目标文件。
func (f *watcherFixture) mustWrite(t *testing.T, content string) {
	t.Helper()
	if err := os.WriteFile(f.path, []byte(content), 0644); err != nil {
		t.Fatalf("写目标文件失败: %v", err)
	}
}

// mustTouch 写同目录里一个与目标无关的文件。
func (f *watcherFixture) mustTouch(t *testing.T, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, name), []byte(content), 0644); err != nil {
		t.Fatalf("写无关文件失败: %v", err)
	}
}

// waitFor 在超时内轮询直到调用次数达到 want；超时即 t.Fatal 并打印已收到的次数。
func (f *watcherFixture) waitFor(t *testing.T, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if int(f.calls.Load()) >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待 %d 次重载调用超时（%s），已收到 %d 次", want, timeout, f.calls.Load())
}

// expectStays 在 within 的静默窗口内轮询调用次数：一旦超过 want 立刻失败（打印当前次数），
// 窗口结束后再断言最终恰好等于 want。用于"合并成一次"与"完全不触发"两类反向判断。
func (f *watcherFixture) expectStays(t *testing.T, want int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if n := int(f.calls.Load()); n > want {
			t.Fatalf("期望调用次数不超过 %d，但窗口内已达到 %d 次", want, n)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if n := int(f.calls.Load()); n != want {
		t.Fatalf("静默窗口结束后期望调用次数为 %d，实际 %d 次", want, n)
	}
}

// waitForWatcherError 轮询直到 State().WatcherError 非空并返回它；超时即 t.Fatal。
func (f *watcherFixture) waitForWatcherError(t *testing.T, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if we := f.w.State().WatcherError; we != "" {
			return we
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待 WatcherError 超时（%s），当前 State=%+v", timeout, f.w.State())
	return ""
}

// waitForLog 轮询直到捕获的日志里出现 substr；超时即 t.Fatal 并打印当前日志。
// storeState 先换指针再记日志，重载交回错误的那条日志会晚于 State() 可见，故要轮询而非即时读。
func (f *watcherFixture) waitForLog(t *testing.T, substr string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Contains(f.logBuf.String(), substr) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待日志包含 %q 超时（%s），当前日志：%s", substr, timeout, f.logBuf.String())
}

// assertLoopRunning 用 loopDone 通道位点判事件循环是否仍在跑：loopDone 未关闭=未返回。
// 这是结构判据，不依赖进程级 goroutine 计数（见 §10.2 第 1 条）。
func (f *watcherFixture) assertLoopRunning(t *testing.T) {
	t.Helper()
	select {
	case <-f.w.loopDone:
		t.Fatal("事件循环已退出，但它不该退出")
	default:
	}
}

// waitForPendingTimer 轮询直到 timers 表里确实有一个在计时的防抖计时器。
// 给"关停/取消前必须先有东西可停"这类前置条件用：没有它，用例可能验的是一段根本没排上的空历史
// （首版 CloseIdempotent 就因此让变异 3 逃过判红，见 §10.5 D-R0501）。
func (f *watcherFixture) waitForPendingTimer(t *testing.T, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		f.w.mu.Lock()
		n := len(f.w.timers)
		f.w.mu.Unlock()
		if n > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待防抖计时器进入 timers 表超时（%s）", timeout)
}

// waitLoopReturned 等事件循环返回（loopDone 关闭）；超时即 t.Fatal，不用 sleep 盲等。
func (f *watcherFixture) waitLoopReturned(t *testing.T, timeout time.Duration) {
	t.Helper()
	select {
	case <-f.w.loopDone:
	case <-time.After(timeout):
		t.Fatalf("等待事件循环返回超时（%s）", timeout)
	}
}

// TestConfigWatcher_TriggersOnWrite 覆盖要求 1：写目标文件后一个窗口内收到一次调用。
func TestConfigWatcher_TriggersOnWrite(t *testing.T) {
	f := newWatcherFixture(t, 100*time.Millisecond)
	f.mustWrite(t, "server:\n  port: 9090\n")
	f.waitFor(t, 1, 2*time.Second)
}

// TestConfigWatcher_CoalescesBurstWrites 覆盖要求 2：连续 5 次快速写入只调一次。
func TestConfigWatcher_CoalescesBurstWrites(t *testing.T) {
	f := newWatcherFixture(t, 200*time.Millisecond)
	for i := 0; i < 5; i++ {
		f.mustWrite(t, fmt.Sprintf("server:\n  port: %d\n", 8080+i))
	}
	f.waitFor(t, 1, 2*time.Second)
	// 再等一个窗口，仍是 1 次：合并的判据是"次数 =="而不是时间等式。
	f.expectStays(t, 1, 250*time.Millisecond)
}

// TestConfigWatcher_IgnoresOtherFiles 覆盖要求 4：同目录其它文件与 config.yaml.bak 都不触发。
// 反向验证（卡 §5.3）：把 handleEvent 的 basename 精确相等改成前缀匹配，这条必然变红。
//
// 末尾有一次**正向对照**：同一个 watcher 随后写目标文件必须正好触发一次。
// 没有它，"监听器整个失灵 + 0 次调用"也能让前面的 expectStays 绿灯（沉默正是本系列要消灭的缺陷类）。
func TestConfigWatcher_IgnoresOtherFiles(t *testing.T) {
	f := newWatcherFixture(t, 100*time.Millisecond)
	f.mustTouch(t, "other.yaml", "unrelated\n")
	f.mustTouch(t, "config.yaml.bak", "backup\n")
	f.mustTouch(t, "config.yml~", "tilde\n")
	f.expectStays(t, 0, 400*time.Millisecond)

	// 正向对照：目标文件本身仍然会被认出来，且只多一次。
	f.mustWrite(t, "server:\n  port: 8081\n")
	f.waitFor(t, 1, 2*time.Second)
	f.expectStays(t, 1, 300*time.Millisecond)
}

// TestConfigWatcher_TriggersOnRenameReplace 覆盖要求 5：tmp→rename 覆盖触发一次，
// 且盯目录让后续普通写入仍能触发（共两次）。
func TestConfigWatcher_TriggersOnRenameReplace(t *testing.T) {
	f := newWatcherFixture(t, 150*time.Millisecond)

	tmp := filepath.Join(f.dir, "config.yaml.tmp")
	if err := os.WriteFile(tmp, []byte("server:\n  port: 7000\n"), 0644); err != nil {
		t.Fatalf("写临时文件失败: %v", err)
	}
	if err := os.Rename(tmp, f.path); err != nil {
		t.Fatalf("rename 覆盖失败: %v", err)
	}
	f.waitFor(t, 1, 2*time.Second)
	f.expectStays(t, 1, 300*time.Millisecond)

	// rename 之后 watcher 仍能收到后续事件——这条就是"盯目录"这一选择的证据。
	f.mustWrite(t, "server:\n  port: 8123\n")
	f.waitFor(t, 2, 2*time.Second)
}

// TestConfigWatcher_TriggersOnRemove 覆盖要求 6：删除目标文件也触发一次调用。
func TestConfigWatcher_TriggersOnRemove(t *testing.T) {
	f := newWatcherFixture(t, 100*time.Millisecond)
	if err := os.Remove(f.path); err != nil {
		t.Fatalf("删除目标文件失败: %v", err)
	}
	f.waitFor(t, 1, 2*time.Second)
}

// TestConfigWatcher_TriggersOnChmod 覆盖要求 7 的 Chmod 分支。
//
// 卡要求 7：watcher 不筛事件类型，Chmod 也应触发；但 Windows 没有 POSIX chmod，
// os.Chmod 改的是只读属性，fsnotify 是否投递事件、投成哪种 Op 都不可预测。
// 本机是 Windows，这条按平台差异显式 SKIP（绝不写成两边都过的假断言）；
// "写与 rename 必触发"由 TriggersOnWrite / TriggersOnRenameReplace 覆盖。
// 判据"不筛事件类型"若被改成只筛 Create|Write，TriggersOnRemove（Remove 事件）会先变红。
func TestConfigWatcher_TriggersOnChmod(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 无 POSIX chmod，os.Chmod 改的是只读属性，fsnotify 是否投递事件不可预测（卡要求 7 的平台受限项）")
	}
	f := newWatcherFixture(t, 100*time.Millisecond)
	if err := os.Chmod(f.path, 0600); err != nil {
		t.Fatalf("chmod 目标文件失败: %v", err)
	}
	f.waitFor(t, 1, 2*time.Second)
}

// TestConfigWatcher_TriggersOnIdenticalContent 覆盖要求 3：内容没变也必须调一次。
//
// 实现根本不做内容比较（卡 §3 第 3 条：判等价要知道"上一次真正生效的那份配置"，
// 那份权威在 R06 的 applied 手里），watcher 只比较事件。这条把"两次同字节写入 = 两次调用"
// 钉成可观测行为：写入间隔（150ms）大于窗口（100ms），所以两次落在两个不同的防抖窗口里。
// 谁日后加了内容哈希缓存，这条会判红。
func TestConfigWatcher_TriggersOnIdenticalContent(t *testing.T) {
	f := newWatcherFixture(t, 100*time.Millisecond)
	// 与 fixture 初建时写的字节完全相同："改回去了"与"没改过"在 watcher 眼里都是事件。
	const same = "server:\n  port: 8080\n"

	f.mustWrite(t, same)
	f.waitFor(t, 1, 2*time.Second)

	time.Sleep(150 * time.Millisecond)
	f.mustWrite(t, same)
	f.waitFor(t, 2, 2*time.Second)
}

// TestConfigWatcher_StoresStateFromReload 覆盖要求 9、12：ReloadFunc 交回的状态被**逐字段**原样存下
// （Result/Error/三份键清单/LastAttemptAt/LastAppliedAt/Enabled/WatchedPath），
// 返回错误时仍然存状态并记 error 日志。
//
// 每个字段都填、每个字段都断言：卡要求 9 明说"含三份键清单"，只验 Result/RejectedKeys 的话
// 一个把 state.Error 抹空的实现也能绿灯（见 §10.3 变异 10）。
func TestConfigWatcher_StoresStateFromReload(t *testing.T) {
	f := newWatcherFixture(t, 100*time.Millisecond)

	attempt := time.Now()
	applied := attempt.Add(-90 * time.Second)
	rejected := ReloadState{
		Enabled:       true,
		Result:        ReloadRejected,
		Error:         "reload rejected: 1 key in reject class",
		AppliedKeys:   []string{"logging.level"},
		IgnoredKeys:   []string{"server.port"},
		RejectedKeys:  []string{"server.auth.token"},
		LastAttemptAt: attempt,
		LastAppliedAt: applied,
	}
	f.preset.Store(&rejected)
	e := errors.New("reload rejected")
	f.presetErr.Store(&e)

	f.mustWrite(t, "server:\n  port: 1\n")
	f.waitFor(t, 1, 2*time.Second)

	st := f.w.State()
	if st.Result != ReloadRejected {
		t.Errorf("Result 应为 rejected，实际 %q", st.Result)
	}
	if st.Error != "reload rejected: 1 key in reject class" {
		t.Errorf("Error 应原样保留，实际 %q", st.Error)
	}
	if want := []string{"logging.level"}; !reflect.DeepEqual(st.AppliedKeys, want) {
		t.Errorf("AppliedKeys 应原样保留为 %v，实际 %v", want, st.AppliedKeys)
	}
	if want := []string{"server.port"}; !reflect.DeepEqual(st.IgnoredKeys, want) {
		t.Errorf("IgnoredKeys 应原样保留为 %v，实际 %v", want, st.IgnoredKeys)
	}
	if want := []string{"server.auth.token"}; !reflect.DeepEqual(st.RejectedKeys, want) {
		t.Errorf("RejectedKeys 应原样保留为 %v，实际 %v", want, st.RejectedKeys)
	}
	if !st.LastAttemptAt.Equal(attempt) {
		t.Errorf("LastAttemptAt 应为 %v，实际 %v", attempt, st.LastAttemptAt)
	}
	if !st.LastAppliedAt.Equal(applied) {
		t.Errorf("LastAppliedAt 应为 %v，实际 %v", applied, st.LastAppliedAt)
	}
	// Enabled 一类的业务字段本卡不解释、原样保存（归 R06），但它不能被存丢。
	if !st.Enabled {
		t.Error("Enabled 应原样保留为 true（本卡不解释业务开关，但也不丢）")
	}
	if st.WatchedPath != f.path {
		t.Errorf("WatchedPath 应为监听路径 %q，实际 %q", f.path, st.WatchedPath)
	}
	if st.WatcherError != "" {
		t.Errorf("WatcherError 应留给 watcher 写，ReloadFunc 交回的这份里没有，实际 %q", st.WatcherError)
	}
	f.waitForLog(t, "config reload returned error", 1*time.Second)
}

// TestConfigWatcher_SetDebounce 覆盖 §3 SetDebounce/Debounce 与卡 §5.7。
//
// 判据不写成"新窗口次数 < 旧窗口次数"这种跨两次运行的时间比较（本机 Windows 上易 flake）：
// 改成 Debounce() 立刻返回新值（确定性）+ 上界断言（短窗口下 N 次写入合并后仍严格少于写入次数），
// 长窗口（1s）下三次写入确定合并成一次。见 §10.2 第 2 条。
func TestConfigWatcher_SetDebounce(t *testing.T) {
	f := newWatcherFixture(t, 1*time.Second)
	if got := f.w.Debounce(); got != 1*time.Second {
		t.Fatalf("初始 Debounce 应为 1s，实际 %s", got)
	}

	// 长窗口：3 次间隔 20ms 的写入（跨度 40ms << 1s）确定合并成一次。
	for i := 0; i < 3; i++ {
		f.mustWrite(t, fmt.Sprintf("a: %d", i))
		time.Sleep(20 * time.Millisecond)
	}
	f.waitFor(t, 1, 3*time.Second)
	f.expectStays(t, 1, 300*time.Millisecond)

	// 运行期改窗口：Debounce() 立刻反映新值。
	f.w.SetDebounce(50 * time.Millisecond)
	if got := f.w.Debounce(); got != 50*time.Millisecond {
		t.Fatalf("SetDebounce(50ms) 后 Debounce 应为 50ms，实际 %s", got)
	}

	// 短窗口（50ms）：5 次间隔 10ms 的写入（跨度 40ms < 50ms）应合并，调用增量严格少于写入次数。
	base := int(f.calls.Load())
	for i := 0; i < 5; i++ {
		f.mustWrite(t, fmt.Sprintf("b: %d", i))
		time.Sleep(10 * time.Millisecond)
	}
	f.waitFor(t, base+1, 2*time.Second)
	quiet := time.Now().Add(400 * time.Millisecond)
	for time.Now().Before(quiet) {
		if d := int(f.calls.Load()) - base; d >= 5 {
			t.Fatalf("50ms 窗口下 5 次写入未合并，已产生 %d 次调用", d)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if d := int(f.calls.Load()) - base; d >= 5 || d < 1 {
		t.Fatalf("短窗口合并后的调用增量应落在 [1,4]，实际 %d", d)
	}

	// 非正数回默认窗口。
	f.w.SetDebounce(0)
	if got := f.w.Debounce(); got != DefaultReloadDebounce {
		t.Errorf("SetDebounce(0) 应回默认窗口，实际 %s", got)
	}
	f.w.SetDebounce(-1)
	if got := f.w.Debounce(); got != DefaultReloadDebounce {
		t.Errorf("SetDebounce(-1) 应回默认窗口，实际 %s", got)
	}
}

// TestConfigWatcher_CloseIdempotent 覆盖要求 8、11：Close 两次返回 nil、事件循环已返回、
// timers 清空、关闭后写文件不再触发任何调用、closed 短路本身生效。
//
// goroutine 清理用结构化判据（loopDone 已关闭 + timers 为空 + 关闭后无新调用），
// 不用 runtime.NumGoroutine 差值：同包其它测试会留下在途协程，进程级计数在本机
// -race -count=5 下会 ±1 抖动，差值断言立不住（见 §10.2 第 1 条，卡 §6 第三条的引用是反的）。
func TestConfigWatcher_CloseIdempotent(t *testing.T) {
	f := newWatcherFixture(t, 100*time.Millisecond)
	f.mustWrite(t, "x: 1\n")
	f.waitFor(t, 1, 2*time.Second)

	// 制造一个"正在计时"的防抖计时器再关停：这是"Close 必须清空 timers"这条结构判据要针对的状态。
	// 直接走内部 schedule（与事件循环消费文件事件后调用的是同一个入口），比"写文件后立刻 Close"
	// 更确定——后者可能与事件投递赛跑，Close 前根本没有计时器进表，判据就成了空断言。
	f.w.schedule(f.w.path)
	f.w.mu.Lock()
	pending := len(f.w.timers)
	f.w.mu.Unlock()
	if pending == 0 {
		t.Fatal("前置条件失败：schedule 后应有一个在计时的防抖计时器")
	}

	if err := f.w.Close(); err != nil {
		t.Fatalf("首次 Close 应返回 nil，实际 %v", err)
	}
	if err := f.w.Close(); err != nil {
		t.Fatalf("二次 Close 应幂等返回 nil，实际 %v", err)
	}

	// (a) 事件循环确已返回。
	select {
	case <-f.w.loopDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Close 之后事件循环未返回")
	}

	// (b) timers 表被清空（Close 里逐个 Stop 后删除）——这条正是"忘了停计时器"的判红点。
	f.w.mu.Lock()
	n := len(f.w.timers)
	f.w.mu.Unlock()
	if n != 0 {
		t.Fatalf("Close 之后 timers 应为空，实际残留 %d 个", n)
	}

	// (c) 关闭后写文件不再触发任何调用。
	f.mustWrite(t, "x: 2\n")
	f.expectStays(t, 1, 300*time.Millisecond)

	// (d) closed 短路本身：绕过计时器、直接打计时器到点后的入口 trigger，也不得新起一次调用。
	// (b)(c) 只能证明"表已空 + 新事件不排期"，删掉 trigger 里 closed 检查的变异要靠这条判红。
	// 它同时也把那个无法消掉的残余竞态钉在明处：回调若在 closed 置真的前一瞬通过检查，
	// 这一次调用仍会跑完——短路保证的是"不再新起"，不是"绝不重叠"（见 trigger 注释）。
	f.w.trigger()
	f.expectStays(t, 1, 200*time.Millisecond)
}

// TestConfigWatcher_WatcherErrorVisible 覆盖要求 10：事件通道被关闭（watcher 自身故障）时
// 记进 WatcherError 并有 error 日志；随后一次重载交回的新状态不应抹掉它（WatcherError 由 watcher 拥有）。
func TestConfigWatcher_WatcherErrorVisible(t *testing.T) {
	f := newWatcherFixture(t, 100*time.Millisecond)
	f.mustWrite(t, "x: 1\n")
	f.waitFor(t, 1, 2*time.Second)

	// 复现"事件通道已关闭"：同包测试直接关掉底层 fsnotify watcher。
	if err := f.w.watcher.Close(); err != nil {
		t.Fatalf("关闭 fsnotify watcher 失败: %v", err)
	}
	we := f.waitForWatcherError(t, 2*time.Second)
	if !strings.Contains(we, "event channel closed") {
		t.Errorf("WatcherError 应记录事件通道关闭，实际 %q", we)
	}
	if !strings.Contains(f.logBuf.String(), "config watcher error") {
		t.Error("WatcherError 应伴随一条 error 日志")
	}

	// 要求 10 后半句：下一次重载（storeState）必须把 WatcherError 原样带过去。
	// 事件循环已因通道关闭退出，这里直接走计时器到点后的同一入口 callReload，
	// 复现"重载成功但 WatcherError 不被抹掉"，与事件循环是否存活无关。
	f.w.callReload()
	st := f.w.State()
	if st.WatcherError != we {
		t.Errorf("重载后 WatcherError 应保留为 %q，实际 %q", we, st.WatcherError)
	}
	if st.Result != ReloadOK {
		t.Errorf("重载后 Result 应更新为默认预设 ok，实际 %q", st.Result)
	}
}

// TestConfigWatcher_PanicDoesNotKillLoop 覆盖要求 13：ReloadFunc panic 不打挂事件循环，
// 转成 WatcherError + error 日志，后续写入仍能触发。对应 §9 风险表第三行。
func TestConfigWatcher_PanicDoesNotKillLoop(t *testing.T) {
	f := newWatcherFixture(t, 100*time.Millisecond)
	f.panicMode.Store(true)

	f.mustWrite(t, "x: 1\n")
	f.waitFor(t, 1, 2*time.Second)

	we := f.waitForWatcherError(t, 2*time.Second)
	if !strings.Contains(we, "panicked") {
		t.Errorf("WatcherError 应记录 panic，实际 %q", we)
	}
	f.assertLoopRunning(t)
	if !strings.Contains(f.logBuf.String(), "config reload panicked") {
		t.Error("panic 应记一条 error 日志")
	}

	// 循环仍在跑：再写一次仍能触发，且 WatcherError 被保留。
	f.mustWrite(t, "x: 2\n")
	f.waitFor(t, 2, 2*time.Second)
	f.assertLoopRunning(t)
	if f.w.State().WatcherError == "" {
		t.Error("第二次重载后 WatcherError 仍应非空")
	}
}

// TestConfigWatcher_RejectsMissingPath 覆盖 §3 构造期前置判断：不存在、相对、空路径、onReload 为 nil 都返回错误。
func TestConfigWatcher_RejectsMissingPath(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(existing, []byte("a: 1\n"), 0644); err != nil {
		t.Fatalf("准备文件失败: %v", err)
	}
	stub := func() (ReloadState, error) { return ReloadState{}, nil }

	cases := []struct {
		name     string
		path     string
		onReload ReloadFunc
	}{
		{"不存在的绝对路径", filepath.Join(dir, "nope.yaml"), stub},
		{"相对路径", "relative/config.yaml", stub},
		{"空路径", "", stub},
		{"onReload 为 nil", existing, nil},
	}
	for _, c := range cases {
		w, err := NewConfigWatcher(c.path, 100*time.Millisecond, c.onReload, nil)
		if err == nil {
			t.Errorf("%s：应返回错误，却成功建出 watcher", c.name)
		}
		if w != nil {
			t.Errorf("%s：返回错误时 watcher 应为 nil", c.name)
		}
	}

	// 对照：合法绝对路径 + 非 nil onReload 能建成。
	if w, err := NewConfigWatcher(existing, 100*time.Millisecond, stub, nil); err != nil {
		t.Errorf("合法路径不应报错: %v", err)
	} else {
		_ = w.Close()
	}
}

// TestConfigWatcher_ConcurrentStateReads 覆盖要求 9：一个协程连写触发重载，
// 两个协程各读 State() 200 轮，-race 必须干净（一次 atomic.Pointer 换指针，读不到半成品）。
func TestConfigWatcher_ConcurrentStateReads(t *testing.T) {
	// 写入间隔（80ms）必须大于防抖窗口（50ms），否则窗口一直被重置、永不触发（防抖本来的形状）。
	f := newWatcherFixture(t, 50*time.Millisecond)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var writeErr atomic.Value

	wg.Add(1)
	go func() {
		defer wg.Done()
		// 非测试协程里不调 t.Fatal：写失败记进 writeErr，停止后在主协程检查。
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if err := os.WriteFile(f.path, []byte(fmt.Sprintf("n: %d\n", i)), 0644); err != nil {
				writeErr.Store(err)
				return
			}
			time.Sleep(80 * time.Millisecond)
		}
	}()

	reader := func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			st := f.w.State()
			// 每次读到的都应是完整份：Result 只可能是枚举值或空串，不会是半截内存。
			switch st.Result {
			case "", ReloadOK, ReloadUnchanged, ReloadRejected, ReloadFailed, ReloadDegraded:
			default:
				t.Errorf("读到非法 Result: %q", st.Result)
				return
			}
			time.Sleep(time.Millisecond)
		}
	}
	wg.Add(2)
	go reader()
	go reader()

	// 读协程跑满约 200ms，期间写协程每隔 80ms 触发一次重载，两者重叠——-race 必须干净。
	f.waitFor(t, 1, 2*time.Second)
	close(stop)
	wg.Wait()
	if e := writeErr.Load(); e != nil {
		t.Fatalf("写协程失败: %v", e)
	}
}

// TestConfigWatcher_CloseStopsTimersWithoutLoop 给"Close 自己停计时器"留一个不依赖事件循环的判红点。
//
// 为什么需要第二条：Close 与 Run 的每条退出路径共用同一个 stopTimers，所以
// TestConfigWatcher_CloseIdempotent 那个"跑过循环再 Close"的形状**分不出是谁清的**——
// 一个让 Close 忘掉停表的变异体会被 Run 的 defer 兜住、在那条用例上绿灯
// （本轮复测实测到的，见 §10.3 变异 3 与 §10.5 D-R0505）。
// 这里干脆不启动 Run：排期只能来自测试自己的一次 schedule，Close 之后 timers 必须为空。
func TestConfigWatcher_CloseStopsTimersWithoutLoop(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("server:\n  port: 8080\n"), 0644); err != nil {
		t.Fatalf("准备文件失败: %v", err)
	}
	var calls atomic.Int64
	w, err := NewConfigWatcher(path, 100*time.Millisecond, func() (ReloadState, error) {
		calls.Add(1)
		return ReloadState{Result: ReloadOK, LastAttemptAt: time.Now()}, nil
	}, nil)
	if err != nil {
		t.Fatalf("NewConfigWatcher 失败: %v", err)
	}
	// 没有事件循环在跑：Run 从未被调用，defer 的那份清理也不可能执行。
	w.schedule(path)
	w.mu.Lock()
	pending := len(w.timers)
	w.mu.Unlock()
	if pending != 1 {
		t.Fatalf("前置条件失败：schedule 后应恰好有 1 个在计时的防抖计时器，实际 %d 个", pending)
	}

	if err := w.Close(); err != nil {
		t.Fatalf("Close 应返回 nil（Run 没跑过就没有循环可等），实际 %v", err)
	}
	w.mu.Lock()
	n := len(w.timers)
	w.mu.Unlock()
	if n != 0 {
		t.Fatalf("Close 自己没停计时器：timers 残留 %d 个（此时事件循环从未启动，没人能替它清）", n)
	}

	// 停表之后到点也不该有重载起来：一个窗口 + 余量的静默观测，10ms 轮询不 sleep 盲断言。
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		if c := int(calls.Load()); c != 0 {
			t.Fatalf("Close 之后防抖计时器仍打进了 ReloadFunc：%d 次", c)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestConfigWatcher_CtxCancelStopsScheduling 覆盖要求 11 的 ctx 那一半：
// 取消 ctx 让 Run 返回后，未触发的防抖计时器必须已经停掉，onReload 不再被计时器打进来。
//
// 修复前只在 Close 里清 timers，而 closed 位点也只由 Close 置真：
// 写入排上 200ms 的窗口 → 立刻取消 ctx → Run 返回、loopDone 关闭，
// 到点后 trigger 看到的 closed 仍是假，于是重载在循环退出之后才起——正是要求 11 反着写的形状。
func TestConfigWatcher_CtxCancelStopsScheduling(t *testing.T) {
	// 窗口取 200ms：留出确定性的一截，让我们能在到期之前取消。
	f := newWatcherFixture(t, 200*time.Millisecond)
	f.mustWrite(t, "server:\n  port: 8080\n")

	// 前置条件：计时器确实进了表。少了这一步就可能"取消得比排期还早"，
	// 那-times 判红就只是空转（同 D-R0501 的教训）。
	f.waitForPendingTimer(t, 2*time.Second)

	f.cancel()
	f.waitLoopReturned(t, 2*time.Second)

	// "Run 返回"之后不允许任何新的重载起来：先记下当前次数当基线
	// （取消与到期之间那一瞬真跑了一次也算既有事实），再等两个窗口判它没有增加。
	base := int(f.calls.Load())
	f.expectStays(t, base, 500*time.Millisecond)

	// 结构化旁证一：ctx 退出路径自己也清了 timers（Run 的 defer）。
	f.w.mu.Lock()
	n := len(f.w.timers)
	f.w.mu.Unlock()
	if n != 0 {
		t.Fatalf("Run 因 ctx 取消返回后 timers 应为空，实际残留 %d 个", n)
	}

	// 结构化旁证二：同一条 defer 也关掉了 fsnotify 句柄，判据是"句柄已关"本身。
	// 不能用"再调一次 Close 返回 nil"来证——已关与未关的 Close 都返回 nil（v1.9.0 各后端先查 closed 位点），
	// 那条断言分不出变异体。v1.9.0 的 AddWith 在已关的句柄上返回 ErrClosed（backend_windows.go:110-112），
	// 所以往同一个目录再挂一次监听拿到错误才是真证据。
	if err := f.w.watcher.Add(f.dir); err == nil {
		t.Error("Run 因 ctx 取消返回后 fsnotify 句柄应已关闭：对同一目录调 Add 应返回错误，却成功挂上了新监听")
	}
	// Close 依赖这条契约：事件循环先关过句柄之后，Close 再关一次仍然返回 nil（Run 注释第 2 条）。
	if err := f.w.Close(); err != nil {
		t.Errorf("句柄已由 Run 关闭，Close 再关一次应幂等返回 nil，实际 %v", err)
	}
}

// TestConfigWatcher_ReloadCallsAreSerialized 覆盖 ReloadFunc 契约第 1 条：调用被 watcher 串行。
//
// 触发者是计时器协程而不是事件循环，两次窗口到期可以重叠，所以串行只能靠 reloadMu。
// 判据是"同时在跑的重载数峰值 == 1"：两个协程在同一起跑线（close(start)）上调 callReload，
// 每次调用体内停留 40ms，去掉那把锁的话第二次的 Add 必然落在第一次的停留区间里、峰值变 2。
func TestConfigWatcher_ReloadCallsAreSerialized(t *testing.T) {
	f := newWatcherFixture(t, 100*time.Millisecond)
	f.hold = 40 * time.Millisecond

	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			f.w.callReload()
		}()
	}
	close(start)
	wg.Wait()

	if n := int(f.calls.Load()); n != 2 {
		t.Fatalf("两次 callReload 都应被执行，实际执行 %d 次", n)
	}
	if m := int(f.maxInflight.Load()); m != 1 {
		t.Errorf("ReloadFunc 调用出现重叠：同时在跑的峰值 %d，期望 1（reloadMu 串行化失效）", m)
	}
}

// TestConfigWatcher_SecondRunDoesNotPanic 覆盖"一个 watcher 只跑一个事件循环"：
// 第二次 Run（以及 Close 之后再 Run）立即返回、不 panic、也不关掉第一个循环正在用的 loopDone。
//
// 选了"静默返回"而不是"panic 报错"：这个类型的调用形状是 `go w.Run(ctx)`，
// 调用方在协程里 panic 等于打挂进程，比多一个空转的协程入口糟得多。行为写进 Run 的注释。
func TestConfigWatcher_SecondRunDoesNotPanic(t *testing.T) {
	f := newWatcherFixture(t, 100*time.Millisecond)

	// 先证明第一个循环真的在跑：能触发重载就说明 started 是它置上的（CAS 只有一个赢家），
	// 后面的第二次调用因此必定走到"返回"那一支，而不是抢在第一个循环之前赢下 CAS。
	f.mustWrite(t, "x: 1\n")
	f.waitFor(t, 1, 2*time.Second)

	if err := runAndCheckImmediate(f.w, 2*time.Second, "第二个 Run"); err != nil {
		t.Error(err)
	}
	f.assertLoopRunning(t)

	// 第一个循环没被第二次调用搅掉：再写一次仍能触发。
	f.mustWrite(t, "x: 2\n")
	f.waitFor(t, 2, 2*time.Second)
	f.assertLoopRunning(t)

	if err := f.w.Close(); err != nil {
		t.Fatalf("Close 应返回 nil，实际 %v", err)
	}
	// Close 之后（loopDone 已关闭）再调 Run：同样立即返回，不能再 close(loopDone) 一次。
	if err := runAndCheckImmediate(f.w, 2*time.Second, "Close 之后的 Run"); err != nil {
		t.Error(err)
	}
}

// runAndCheckImmediate 在独立协程里调一次 w.Run，要求它在上界内返回；超时即返回错误描述。
// 用协程包一层是因为 Run 正常形状就是阻塞的——直接在测试协程里调会把"没返回"看成"通过"。
func runAndCheckImmediate(w *ConfigWatcher, timeout time.Duration, label string) error {
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Run(context.Background())
	}()
	select {
	case <-done:
		return nil
	case <-time.After(timeout):
		return fmt.Errorf("%s 调用应立即返回（同一 watcher 只跑一个事件循环），%s 内未返回", label, timeout)
	}
}

// TestConfigWatcher_ResolveDiskFileName 覆盖构造期的磁盘拼写解析（修复本体，卡 §10.2）。
//
// 用例在任何平台都跑到同一条折叠匹配代码：目录里的真实名字由测试自己写成小写，
// 入参写成大写拼写在大小写敏感的 Linux 上也必须走 EqualFold 那一支才找得回来。
func TestConfigWatcher_ResolveDiskFileName(t *testing.T) {
	dir := t.TempDir()
	disk := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(disk, []byte("a: 1\n"), 0644); err != nil {
		t.Fatalf("准备文件失败: %v", err)
	}

	cases := []struct{ in, want string }{
		{"config.yaml", "config.yaml"}, // 精确相等那一支
		{"CONFIG.yaml", "config.yaml"}, // 折叠匹配那一支
		{"Config.YAML", "config.yaml"},
		{"config.YAML", "config.yaml"},
	}
	for _, c := range cases {
		got, err := resolveDiskFileName(dir, c.in)
		if err != nil {
			t.Errorf("resolveDiskFileName(%q) 报错: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("resolveDiskFileName(%q) 应返回磁盘拼写 %q，实际 %q", c.in, c.want, got)
		}
	}

	// 目录里没有对应条目：返回错误而不是静默回退成入参拼写——回退等于把"永不触发"请回来。
	if _, err := resolveDiskFileName(dir, "other.yaml"); err == nil {
		t.Error("目录里没有对应条目时应返回错误")
	}
	// 目录列不出来：同样报错。
	if _, err := resolveDiskFileName(filepath.Join(dir, "no-such-dir"), "config.yaml"); err == nil {
		t.Error("目录列不出来时应返回错误")
	}

	// 精确相等优先于折叠匹配：只有大小写敏感的卷上造得出来（两个只差大小写的不同文件）。
	// 判据不能用"写 Config.yaml 是否报错"——大小写不敏感的卷上它会**成功**，
	// 只是覆盖已有的 config.yaml 并保留磁盘上那份拼写，目录里仍只有一条。
	sens := filepath.Join(dir, "Config.yaml")
	if err := os.WriteFile(sens, []byte("b: 2\n"), 0644); err != nil {
		t.Logf("写第二种大小写拼写失败，跳过『精确相等优先』这一支: %v", err)
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("列目录失败: %v", err)
	}
	if len(entries) != 2 {
		t.Logf("本机卷大小写不敏感（目录里仍只有 %d 条，Config.yaml 覆盖的就是 config.yaml），跳过『精确相等优先』这一支", len(entries))
		return
	}
	if got, err := resolveDiskFileName(dir, "Config.yaml"); err != nil || got != "Config.yaml" {
		t.Errorf("目录里有精确同名条目时应返回它本身，实际 %q (err=%v)", got, err)
	}
}

// TestConfigWatcher_MatchesOnDiskNameCasing 端到端验大小写错配的 -config 路径仍真的触发。
//
// 修的是这一类静默：用户在 Windows/macOS 上手打 -config C:\...\Config.yaml，
// os.Stat 成功、watcher 建得出来、State() 看着一切正常，但 fsnotify 上报的是磁盘拼写
// config.yaml，精确比对永不命中——监听器再也不会触发，而没有任何地方报错。
//
// 这条只在大小写不敏感的卷上构造得出来；大小写敏感的 Linux 上 os.Stat 就先失败了，
// 端到端无从构造，那一段代码路径由 TestConfigWatcher_ResolveDiskFileName 覆盖。
func TestConfigWatcher_MatchesOnDiskNameCasing(t *testing.T) {
	dir := t.TempDir()
	diskPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(diskPath, []byte("server:\n  port: 8080\n"), 0644); err != nil {
		t.Fatalf("准备文件失败: %v", err)
	}
	typed := filepath.Join(dir, "CONFIG.yaml")
	if _, err := os.Stat(typed); err != nil {
		t.Skipf("本机文件系统大小写敏感（os.Stat(%q) 失败）：错配拼写的端到端场景无从构造，折叠匹配由 TestConfigWatcher_ResolveDiskFileName 覆盖", typed)
	}
	abs, err := filepath.Abs(typed)
	if err != nil {
		t.Fatalf("求绝对路径失败: %v", err)
	}

	var calls atomic.Int64
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w, err := NewConfigWatcher(abs, 100*time.Millisecond, func() (ReloadState, error) {
		calls.Add(1)
		return ReloadState{Result: ReloadOK, LastAttemptAt: time.Now()}, nil
	}, nil)
	if err != nil {
		t.Fatalf("用大小写错配的拼写构造 watcher 失败: %v", err)
	}
	defer func() { _ = w.Close() }()

	if w.basename != "config.yaml" {
		t.Errorf("basename 应解析成磁盘拼写 config.yaml，实际 %q", w.basename)
	}

	go w.Run(ctx)
	// 真实编辑器的行为是写在磁盘拼写那份上。
	if err := os.WriteFile(diskPath, []byte("server:\n  port: 9090\n"), 0644); err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && calls.Load() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if n := int(calls.Load()); n == 0 {
		t.Fatal("大小写错配的 -config 路径下 watcher 一次都没触发：basename 没解析成磁盘拼写（静默失效）")
	}
}
