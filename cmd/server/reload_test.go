package main

// 配置热重载链的单元测试（TASK-R06 §5.1）。
//
// 替身全假、不起进程：链只被直接调，落点记流水。判据一律取"流水 + 交回的结论 + applied"，
// 不取时钟、不取 goroutine 计数。真实落点是否接在对的下游上由
// main_integration_test.go 的 TestRun_WiredReloadChainReachesEveryTarget 判；
// 真 watcher + 真 setter + 真表的端到端由 config_reload_smoke_test.go 判。

import (
	"encoding/binary"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf16"

	"godelayq/core"
)

// ---- 配置文件文本夹具 ----

// fileSpec 是配置文件里"用例关心的那些取值"。字段一律是即将写进 YAML 的原文片段，
// 空串表示这一项不写（由 core 的默认值与 Normalized 补齐）。
//
// 为什么要两份文本而不是一份配置再加内存改法：被测对象就是那份文件（README 的共同口径），
// 而 applied 与 candidate 必须来自同一条 LoadConfig 路径——否则没写的键会被
// 当成"0 值 vs 默认值"的改动，Diff 报出一片与用例无关的键名。
type fileSpec struct {
	reloadEnabled  string
	debounce       string
	port           string
	level          string
	workers        string
	queueCapacity  string
	retryDelay     string
	historyLimit   string
	historyTTL     string
	commandTimeout string
	commandScript  string
	token          string
	obsEnabled     string
	execEnabled    string
	eventCount     string
	eventAge       string
	auditCount     string
	auditAge       string
}

// baseSpec 是几乎所有用例共用的起点。reload.enabled 打开（链只在启用的部署里存在）；
// 观测层总开关关着但四个 retention 键照写——单元用例的落点是替身，
// "总开关关着就不建写入器"那条装配判定在 §5.2 那组用例里。
func baseSpec() fileSpec {
	return fileSpec{
		reloadEnabled:  "true",
		debounce:       "500ms",
		port:           "18080",
		level:          "info",
		workers:        "4",
		queueCapacity:  "0",
		retryDelay:     "60s",
		historyLimit:   "100",
		historyTTL:     "24h",
		commandTimeout: "10s",
		commandScript:  "scripts/echo.mjs",
		obsEnabled:     "false",
		execEnabled:    "true",
		eventCount:     "1000",
		eventAge:       "2160h",
		auditCount:     "1000",
		auditAge:       "2160h",
	}
}

// specChange 是"在起点之上改哪几项"的写法，省掉每个用例重抄一遍 baseSpec。
func specChange(tune func(*fileSpec)) fileSpec {
	spec := baseSpec()
	tune(&spec)
	return spec
}

// render 把 spec 摊成一份配置文件文本。
//
// 路径全部落在调用方给的临时目录里：链本身不打开这些文件（它只读配置），
// 但写错路径的用例不该有机会碰到仓库的 data/。
// runtime_allow 与 runtime 用块式列表 + 裸量：Windows 路径里的反斜杠在 YAML 双引号串里
// 会被当转义符，裸量不会（对照 configs/config.example.yaml 的 [bash, sh] 那种写法）。
func (s fileSpec) render(dir, workspace, executable string) string {
	server := "server:\n  port: \"" + s.port + "\"\n"
	if s.token != "" {
		server += "  auth:\n    token: " + s.token + "\n"
	}
	return server + fmt.Sprintf(`reload:
  enabled: %s
  debounce: %s
logging:
  level: %s
  format: text
scheduler:
  workers: %s
  queue_capacity: %s
  max_retry_delay: %s
  shutdown_timeout: 50ms
store:
  type: json
  path: %s
  groups_path: %s
  flush_interval: 1h
  history_limit: %s
  history_ttl: %s
observability:
  enabled: %s
  path: %s
  events:
    enabled: true
    retention_count: %s
    retention_age: %s
  audit:
    enabled: true
    retention_count: %s
    retention_age: %s
executors:
  enabled: %s
  workspace: %s
  # 产物目录与档位文件都指到临时目录：默认值是 ./data/exec 与 ./data/exec-profiles.json，
  # 装配用例（§5.2）会真的建产物存储，绝不能让它写到仓库的 data/ 里。
  output:
    dir: %s
  profiles_path: %s
  runtime_allow:
    - %s
  commands:
    - name: echo
      kind: script
      runtime: %s
      script: %s
      timeout: %s
`, s.reloadEnabled, s.debounce, s.level, s.workers, s.queueCapacity, s.retryDelay,
		filepath.Join(dir, "jobs.json"), filepath.Join(dir, "groups.json"), s.historyLimit, s.historyTTL,
		s.obsEnabled, filepath.Join(dir, "observe.sqlite"), s.eventCount, s.eventAge, s.auditCount, s.auditAge,
		s.execEnabled, workspace, filepath.Join(dir, "exec"), filepath.Join(dir, "exec-profiles.json"),
		executable, executable, s.commandScript, s.commandTimeout)
}

// ---- 落点替身 ----

// fakeTargets 记录每个落点的调用流水（名字 + 取值），并可预设第几次调用失败。
//
// 为什么是"第几次"而不是"应用还是回滚"：链的应用与回滚走的是同一段代码（那是卡 §3.2
// "回滚不会漏项"的结构保证），替身分不出调用方是谁、也不该分出——
// 回滚失败因此只能表达成"这个落点第二次被调用"。
type fakeTargets struct {
	mu    sync.Mutex
	calls []string
	// inside / peak 记"同时在跑的落点调用数"与其峰值：一条链里的调用是顺序的，所以峰值 > 1
	// 只可能是两条链在交叉跑（§5.1 #11 的判据）。
	inside int
	peak   int
	failOn map[string]int
	// gateOn 非空时，那个落点每次进来先通告一次、再等放行：用它把链卡在中间（#11、#12）。
	arrived chan string
	proceed chan struct{}
	gateOn  string
	gate    sync.Once
}

func newFakeTargets() *fakeTargets {
	return &fakeTargets{
		failOn:  map[string]int{},
		arrived: make(chan string, 8),
		proceed: make(chan struct{}),
	}
}

func (f *fakeTargets) invoke(name, value string) error {
	f.enter(name)
	defer f.exit()
	if f.gateOn == name {
		f.arrived <- name
		<-f.proceed
	}

	f.mu.Lock()
	f.calls = append(f.calls, name+"="+value)
	call := 0
	for _, item := range f.calls {
		if strings.HasPrefix(item, name+"=") {
			call++
		}
	}
	f.mu.Unlock()

	if f.failOn[name] == call {
		return fmt.Errorf("落点 %s 的预设失败", name)
	}
	return nil
}

func (f *fakeTargets) enter(name string) {
	f.mu.Lock()
	f.inside++
	if f.inside > f.peak {
		f.peak = f.inside
	}
	f.mu.Unlock()
}

func (f *fakeTargets) exit() {
	f.mu.Lock()
	f.inside--
	f.mu.Unlock()
}

// release 打开闸门；幂等，所以失败路径上多调一次也不会 panic。
func (f *fakeTargets) release() {
	f.gate.Do(func() { close(f.proceed) })
}

// recorded 交回调用流水的副本（用例读它，不碰替身内部）。
func (f *fakeTargets) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// peakOverlap 交回"同时在跑的落点调用数"的峰值。
func (f *fakeTargets) peakOverlap() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.peak
}

// targets 交回八只落点全都在场的替身。
func (f *fakeTargets) targets() reloadTargets {
	return reloadTargets{
		setLevel:   func(level string) error { return f.invoke("setLevel", level) },
		setWorkers: func(n int) error { return f.invoke("setWorkers", fmt.Sprint(n)) },
		setRetry:   func(d time.Duration) error { return f.invoke("setRetry", d.String()) },
		setRetention: func(limit int, ttl time.Duration) error {
			return f.invoke("setRetention", fmt.Sprintf("%d/%s", limit, ttl))
		},
		setEventRetention: func(count int, age time.Duration) error {
			return f.invoke("setEventRetention", fmt.Sprintf("%d/%s", count, age))
		},
		setAuditRetention: func(count int, age time.Duration) error {
			return f.invoke("setAuditRetention", fmt.Sprintf("%d/%s", count, age))
		},
		setCommands: func(candidate core.Config) error {
			return f.invoke("setCommands", fmt.Sprint(len(candidate.Executors.Commands)))
		},
		setDebounce: func(d time.Duration) error { return f.invoke("setDebounce", d.String()) },
	}
}

// targetsWithout 交回缺了指定落点的替身，用来演"那一节没启用"与"装配缺入口"两种脸色。
func (f *fakeTargets) targetsWithout(names ...string) reloadTargets {
	targets := f.targets()
	for _, name := range names {
		switch name {
		case "setLevel":
			targets.setLevel = nil
		case "setWorkers":
			targets.setWorkers = nil
		case "setRetry":
			targets.setRetry = nil
		case "setRetention":
			targets.setRetention = nil
		case "setEventRetention":
			targets.setEventRetention = nil
		case "setAuditRetention":
			targets.setAuditRetention = nil
		case "setCommands":
			targets.setCommands = nil
		case "setDebounce":
			targets.setDebounce = nil
		}
	}
	return targets
}

// ---- 用例夹具 ----

type reloadFixture struct {
	t          *testing.T
	dir        string
	workspace  string
	executable string
	path       string
	targets    *fakeTargets
	logs       *strings.Builder
	logger     *slog.Logger
	chain      *reloader
	applied    core.Config
	logMu      sync.Mutex
}

// newReloadFixture 落两份配置：base 那份读回来当 applied，candidate 那份留在磁盘上让链去读。
func newReloadFixture(t *testing.T, base, candidate fileSpec) *reloadFixture {
	t.Helper()

	dir := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	workspace := filepath.Join(dir, "ws")

	path := filepath.Join(dir, "config.yaml")
	fx := &reloadFixture{
		t:          t,
		dir:        dir,
		workspace:  workspace,
		executable: executable,
		path:       path,
		targets:    newFakeTargets(),
	}
	fx.write(base)

	applied, err := core.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig(base): %v\n%s", err, base.render(dir, workspace, executable))
	}
	fx.applied = applied.Normalized()

	fx.write(candidate)
	var logs strings.Builder
	fx.logs = &logs
	fx.logger = slog.New(slog.NewTextHandler(&logs, nil))
	return fx
}

// write 把一份 spec 摊成文件覆盖到链要读的那个路径上。
func (fx *reloadFixture) write(spec fileSpec) {
	fx.t.Helper()
	if err := os.WriteFile(fx.path, []byte(spec.render(fx.dir, fx.workspace, fx.executable)), 0o600); err != nil {
		fx.t.Fatalf("write config: %v", err)
	}
}

// build 用当前替身组装链。单独一步是为了让用例先把 failOn / gateOn 摆好。
func (fx *reloadFixture) build() *reloader {
	fx.t.Helper()
	fx.chain = fx.buildFrom(fx.targets.targets())
	return fx.chain
}

// buildWithout 组装一条缺了指定落点的链。
func (fx *reloadFixture) buildWithout(names ...string) *reloader {
	fx.t.Helper()
	fx.chain = fx.buildFrom(fx.targets.targetsWithout(names...))
	return fx.chain
}

func (fx *reloadFixture) buildFrom(targets reloadTargets) *reloader {
	return &reloader{
		applied:        fx.applied,
		processEnabled: fx.applied.Reload.Enabled,
		cfgPath:        fx.path,
		targets:        targets,
		logger:         fx.logger,
	}
}

// reload 调一次链并交回结论。
func (fx *reloadFixture) reload() (core.ReloadState, error) {
	fx.t.Helper()
	if fx.chain == nil {
		fx.build()
	}
	return fx.chain.Reload()
}

// log 交回到目前为止的全部日志。
func (fx *reloadFixture) log() string {
	fx.logMu.Lock()
	defer fx.logMu.Unlock()
	return fx.logs.String()
}

// candidateOf 交回磁盘上那份配置的解析结果（applied 应当推进到的目标）。
func (fx *reloadFixture) candidateOf() core.Config {
	fx.t.Helper()
	candidate, err := core.LoadConfig(fx.path)
	if err != nil {
		fx.t.Fatalf("LoadConfig(candidate): %v", err)
	}
	return candidate.Normalized()
}

// ---- §5.1 的用例 ----

// TestReload_AppliesEveryHotKey 是顺序表的主判据：一次改动同时含六个热更落点时，
// 调用次序必须严格等于 §3.2 的 1,2,3,4,6,7（#5 那两个写入器这一条用例没改到）。
// 断的是**次序**而不是"都调过"——卡 §6 第二条点名要的形状。
func TestReload_AppliesEveryHotKey(t *testing.T) {
	fx := newReloadFixture(t, baseSpec(), specChange(func(s *fileSpec) {
		s.level = "debug"
		s.debounce = "200ms"
		s.retryDelay = "5s"
		s.historyLimit = "2"
		s.workers = "12"
		s.commandTimeout = "5s"
	}))
	chain := fx.build()

	state, err := fx.reload()
	if err != nil {
		t.Fatalf("Reload 返回错误：%v（state=%+v）", err, state)
	}

	assertExactCalls(t, fx, []string{
		"setLevel=debug",
		"setDebounce=200ms",
		"setRetry=5s",
		"setRetention=2/24h0m0s",
		"setWorkers=12",
		"setCommands=1",
	})

	if state.Result != core.ReloadOK {
		t.Fatalf("result = %q, want %q（error=%q）", state.Result, core.ReloadOK, state.Error)
	}
	for _, key := range []string{
		keyLoggingLevel, keyReloadDebounce, keySchedulerMaxRetryDelay,
		keyStoreHistoryLimit, keySchedulerWorkers,
	} {
		if !containsKey(state.AppliedKeys, key) {
			t.Fatalf("applied_keys = %v，少了 %q", state.AppliedKeys, key)
		}
	}
	if !containsPrefix(state.AppliedKeys, keyExecutorsCommands) {
		t.Fatalf("applied_keys = %v，档位那条改动没进来", state.AppliedKeys)
	}
	// debounce 的期望还包括"以新值调一次"，上面的流水已经断到；这里再断它进了 applied。
	if got := chain.applied.Reload.Debounce; got != 200*time.Millisecond {
		t.Fatalf("applied 里的窗口 = %v, want 200ms", got)
	}
	if state.LastAppliedAt.IsZero() {
		t.Fatal("真的应用了却不给 LastAppliedAt，读数会看着像从没应用过")
	}
	// WatchedPath 一律由 watcher 写（R05 要求 10 的字段归属），链交回的那一份留空是对的：
	// 这里断它为空，谁日后在链里填上它就会红——那不会出错，但会把字段归属搞成两处。
	if state.WatchedPath != "" {
		t.Fatalf("链不该写 watched_path（归属在 watcher），实际 = %q", state.WatchedPath)
	}
}

// TestReload_RollbacksInReverseOnFailure 是不变量 I2 的后半句：中途失败要按逆序把每一项
// 以**旧值**再调一次，而失败之后的那一步（档位）根本不该被碰过。
func TestReload_RollbacksInReverseOnFailure(t *testing.T) {
	fx := newReloadFixture(t, baseSpec(), specChange(func(s *fileSpec) {
		s.level = "debug"
		s.debounce = "200ms"
		s.retryDelay = "5s"
		s.historyLimit = "2"
		s.workers = "12"
		s.commandTimeout = "5s"
	}))
	fx.targets.failOn["setWorkers"] = 1
	chain := fx.build()

	state, err := fx.reload()
	if err == nil {
		t.Fatal("落点失败了却报无错，违反 I3")
	}
	if state.Result != core.ReloadFailed {
		t.Fatalf("result = %q, want %q", state.Result, core.ReloadFailed)
	}
	if !strings.Contains(state.Error, "落点 setWorkers 的预设失败") {
		t.Fatalf("error 里没有失败原因：%q", state.Error)
	}
	if !strings.Contains(state.Error, "scheduler.workers") {
		t.Fatalf("error 里认不出是哪一步：%q", state.Error)
	}

	assertExactCalls(t, fx, []string{
		"setLevel=debug", "setDebounce=200ms", "setRetry=5s", "setRetention=2/24h0m0s",
		"setWorkers=12",
		// 逆序回滚，且每一项给的是 applied 里的旧值
		"setRetention=100/24h0m0s", "setRetry=1m0s", "setDebounce=500ms", "setLevel=info",
	})
	if len(state.AppliedKeys) != 0 {
		t.Fatalf("失败那次的 applied_keys = %v，应当为空（那些键已被回滚）", state.AppliedKeys)
	}
	if !reflect.DeepEqual(chain.applied, fx.applied) {
		t.Fatal("applied 被失败的重载推前了，违反不变量 I1")
	}
	if !state.LastAppliedAt.IsZero() {
		t.Fatalf("没成功却给了 LastAppliedAt = %v", state.LastAppliedAt)
	}
}

// TestReload_DegradedWhenRollbackFails 让 degraded 可达（卡 §6 第四条：
// 它不能只是注释里的一个字符串）：回滚链里的那一步自己也失败。
func TestReload_DegradedWhenRollbackFails(t *testing.T) {
	fx := newReloadFixture(t, baseSpec(), specChange(func(s *fileSpec) {
		s.level = "debug"
		s.retryDelay = "5s"
		s.workers = "12"
	}))
	fx.targets.failOn["setWorkers"] = 1
	fx.targets.failOn["setRetry"] = 2 // 第二次调用就是那次回滚
	chain := fx.build()

	state, err := fx.reload()
	if err == nil {
		t.Fatal("回滚失败却不报错")
	}
	if state.Result != core.ReloadDegraded {
		t.Fatalf("result = %q, want %q", state.Result, core.ReloadDegraded)
	}
	if !strings.Contains(state.Error, "落点 setWorkers 的预设失败") ||
		!strings.Contains(state.Error, "落点 setRetry 的预设失败") {
		t.Fatalf("degraded 的 error 要同时带原始失败与回滚失败：%q", state.Error)
	}
	logged := fx.log()
	if !strings.Contains(logged, "degraded") || !strings.Contains(logged, "rollback") {
		t.Fatalf("degraded 没有一条能对得上的 error 级记录：\n%s", logged)
	}
	if !reflect.DeepEqual(chain.applied, fx.applied) {
		t.Fatal("degraded 也不该推进 applied")
	}
	// 流水里能看到 setRetry 被调了两次（一次新值、一次旧值），只是旧值那次没落下去。
	assertExactCalls(t, fx, []string{
		"setLevel=debug", "setRetry=5s", "setWorkers=12", "setRetry=1m0s", "setLevel=info",
	})
}

// TestReload_RejectsCredentialChange 是 I2 最硬的那条证据：碰凭据时所有落点函数都没被调过。
func TestReload_RejectsCredentialChange(t *testing.T) {
	fx := newReloadFixture(t, baseSpec(), specChange(func(s *fileSpec) {
		s.token = "rotated-token"
		s.level = "debug" // 本来会生效的那一条，也要一起作废
		s.workers = "32"
	}))
	chain := fx.build()

	state, err := fx.reload()
	if err == nil {
		t.Fatal("拒绝档没返回错误，运维的告警不会触发")
	}
	if state.Result != core.ReloadRejected {
		t.Fatalf("result = %q, want %q", state.Result, core.ReloadRejected)
	}
	assertNoCalls(t, fx)
	if !reflect.DeepEqual(state.RejectedKeys, []string{"server.auth.token"}) {
		t.Fatalf("rejected_keys = %v", state.RejectedKeys)
	}
	if len(state.AppliedKeys) != 0 {
		t.Fatalf("被拒那次还有 applied_keys = %v", state.AppliedKeys)
	}
	if !reflect.DeepEqual(chain.applied, fx.applied) {
		t.Fatal("被拒那次推进了 applied")
	}
	// 拒绝的代价必须是"什么都没发生"，所以 error 文本要说清旧取值仍在生效。
	if !strings.Contains(state.Error, "一项都没有应用") {
		t.Fatalf("error 文案没交代什么都没动：%q", state.Error)
	}
	if strings.Contains(state.Error, "rotated-token") {
		t.Fatalf("error 里带出了凭据取值：%q", state.Error)
	}
}

// TestReload_RejectsPermissionFieldInProfile 是拒绝档的另一半：档位里的身份字段
// （script）改了要整次作废，而同一份改动里的取值型热更项也一条都不应用。
func TestReload_RejectsPermissionFieldInProfile(t *testing.T) {
	fx := newReloadFixture(t, baseSpec(), specChange(func(s *fileSpec) {
		s.commandScript = "scripts/other.mjs"
		s.level = "debug"
	}))
	chain := fx.build()

	state, err := fx.reload()
	if err == nil {
		t.Fatal("档位身份字段被当成热更接受了")
	}
	assertNoCalls(t, fx)
	if state.Result != core.ReloadRejected {
		t.Fatalf("result = %q, want %q", state.Result, core.ReloadRejected)
	}
	if !reflect.DeepEqual(state.RejectedKeys, []string{"executors.commands.echo.script"}) {
		t.Fatalf("rejected_keys = %v，归因要能到具体那一条档位的字段", state.RejectedKeys)
	}
	if !reflect.DeepEqual(chain.applied, fx.applied) {
		t.Fatal("拒绝档推进了 applied")
	}
}

// TestReload_BadFileKeepsApplied 守住设计文档 §8 的"坏文件 → 旧配置原样 + 修好自动恢复"。
// 这里用一份**合法 YAML 但含未知键**的文件（UnmarshalExact 会拒），
// 因为"语法错"与"解码被拒"在链上走的是同一个 LoadConfig 错误分支。
func TestReload_BadFileKeepsApplied(t *testing.T) {
	fx := newReloadFixture(t, baseSpec(), baseSpec())
	chain := fx.build()

	if err := os.WriteFile(fx.path,
		[]byte("logging:\n  level_not_a_key: debug\n"), 0o600); err != nil {
		t.Fatalf("write broken config: %v", err)
	}

	state, err := fx.reload()
	if err == nil {
		t.Fatal("读不进来却报成功")
	}
	if state.Result != core.ReloadRejected {
		t.Fatalf("result = %q, want %q", state.Result, core.ReloadRejected)
	}
	assertNoCalls(t, fx)
	if !strings.Contains(state.Error, "read config failed") &&
		!strings.Contains(state.Error, "parse config failed") {
		t.Fatalf("error 没带上配置层的原文（%q），运维看不出是哪一次读坏", state.Error)
	}
	if !strings.Contains(state.Error, "现网继续按当前生效的取值运行") {
		t.Fatalf("error 没说清现网怎么样：%q", state.Error)
	}
	if !reflect.DeepEqual(chain.applied, fx.applied) {
		t.Fatal("坏文件把 applied 改了——那等于整台机器静默换了一套值")
	}
	if strings.Contains(strings.ToLower(state.Error), "fallback") ||
		strings.Contains(state.Error, "默认配置") {
		t.Fatalf("读数里出现了回退默认值的暗示：%q", state.Error)
	}

	// 修好文件即自动恢复：下一次 Reload 不需要任何人干预就成功（§8 那条）。
	fx.write(specChange(func(s *fileSpec) { s.level = "debug" }))
	recovered, err := fx.reload()
	if err != nil {
		t.Fatalf("修好文件之后没有自动恢复：%v", err)
	}
	if recovered.Result != core.ReloadOK {
		t.Fatalf("result = %q, want %q", recovered.Result, core.ReloadOK)
	}
	assertExactCalls(t, fx, []string{"setLevel=debug"})
}

// TestReload_EmptyFileKeepsApplied 判的是 §3.1 第 1 步那句"绝不退回默认值"的另一半：
// 坏文件走"读不进来"那一条，而**空文件读得进来**——core.LoadConfig 交回的是一份全部取
// 默认值的配置，Diff 会把它当成一次正常改动应用到现网。
//
// 现场来自 TASK-R07 场景 16E 的真实进程：那份部署没配任何凭据（所以拒绝档挡不住），
// 把文件写成空的之后结论是 ok，workers 从 7 变成代码默认的 100、logging.level 从 debug
// 回 info、executors.commands 整表被清空。带凭据的部署此前只是碰巧被 server.auth.* 的
// 拒绝项挡住，那是巧合而不是判定，所以这里把它变成判定。
func TestReload_EmptyFileKeepsApplied(t *testing.T) {
	fx := newReloadFixture(t, baseSpec(), baseSpec())
	chain := fx.build()

	if err := os.WriteFile(fx.path, []byte(""), 0o600); err != nil {
		t.Fatalf("write empty config: %v", err)
	}

	state, err := fx.reload()
	if err == nil {
		t.Fatal("空文件交回了成功——现网已经被换成一份默认配置")
	}
	if state.Result != core.ReloadRejected {
		t.Fatalf("result = %q, want %q", state.Result, core.ReloadRejected)
	}
	assertNoCalls(t, fx)
	if !strings.Contains(state.Error, "没有表达任何取值") {
		t.Fatalf("error 没说是空文件这一种（%q），运维会以为是又一次读坏", state.Error)
	}
	if !strings.Contains(state.Error, "现网继续按当前生效的取值运行") {
		t.Fatalf("error 没说清现网怎么样：%q", state.Error)
	}
	// 日志那两行也要逐字核：只断 result 的话，把这条专门的 ERROR 行删掉、只留交回的 error 字段，
	// 全包还是绿的（R06 复核在同一条系列上栽过一次）。
	if logged := fx.log(); !strings.Contains(logged, "the config file carries no values") ||
		!strings.Contains(logged, "不等于默认配置") {
		t.Fatalf("拒绝日志没留下可读的结论：\n%s", logged)
	}
	if !reflect.DeepEqual(chain.applied, fx.applied) {
		t.Fatal("空文件把 applied 改了——那等于整台机器静默换了一套值")
	}

	// 与坏文件同一口径：文件恢复成有内容的样子，下一次重载不需要任何人干预。
	fx.write(specChange(func(s *fileSpec) { s.level = "debug" }))
	recovered, err := fx.reload()
	if err != nil {
		t.Fatalf("空文件之后没有自动恢复：%v", err)
	}
	if recovered.Result != core.ReloadOK {
		t.Fatalf("result = %q, want %q", recovered.Result, core.ReloadOK)
	}
	assertExactCalls(t, fx, []string{"setLevel=debug"})
}

// TestReload_CommentsOnlyFileIsSameConclusion 补的是空文件那一判据的几个文本形状：只有注释、
// 只有空白、只剩 YAML 的文档标记、标记后跟注释，以及带 BOM / UTF-16 写出来的同样内容。
// 它们在 LoadConfig 那侧与空文件同一种结果（一份全默认配置），所以必须落进同一条拒绝判据。
//
// 为什么带上 BOM/UTF-16 这几格：判据用的是 YAML 解析器，它自己认得 BOM 与带 BOM 的 UTF-16，
// 而 Windows PowerShell 5.1 的 `>` 与 Out-File 默认写的就是带 BOM 的 UTF-16LE——
// "用重定向把文件清成只剩注释"这一格必须也被判成没取值，而不是"文件里有字就算有内容"。
//
// 每一格都同时核 error 文案：只看 result=rejected 不够——第 1 步读不进来也回 rejected，
// 那说明这一格压根没进这条判据。
func TestReload_CommentsOnlyFileIsSameConclusion(t *testing.T) {
	utf16LE := func(s string) []byte {
		out := []byte{0xFF, 0xFE}
		for _, u := range utf16.Encode([]rune(s)) {
			out = binary.LittleEndian.AppendUint16(out, u)
		}
		return out
	}
	utf16BE := func(s string) []byte {
		out := []byte{0xFE, 0xFF}
		for _, u := range utf16.Encode([]rune(s)) {
			out = binary.BigEndian.AppendUint16(out, u)
		}
		return out
	}
	comments := "# 临时清空，等下填回来\n\n# 第二行注释\n"
	for _, tc := range []struct {
		name string
		raw  []byte
	}{
		{"只有注释", []byte(comments)},
		{"只有空白", []byte("\n  \n\n")},
		{"只剩文档分隔符", []byte("---\n...\n")},
		{"分隔符后跟注释", []byte("--- # 这一格还没填\n...\n")},
		{"UTF-8 BOM 加注释", append([]byte{0xEF, 0xBB, 0xBF}, []byte(comments)...)},
		{"UTF-16LE 加注释", utf16LE(comments)},
		{"UTF-16BE 加注释", utf16BE(comments)},
		{"只有 BOM", []byte{0xEF, 0xBB, 0xBF}},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			fx := newReloadFixture(t, baseSpec(), baseSpec())
			chain := fx.build()

			if err := os.WriteFile(fx.path, tc.raw, 0o600); err != nil {
				t.Fatalf("写配置文件失败：%v", err)
			}
			state, err := fx.reload()
			if err == nil || state.Result != core.ReloadRejected {
				t.Fatalf("交回 result=%q err=%v，应当是 rejected", state.Result, err)
			}
			if !strings.Contains(state.Error, "没有表达任何取值") {
				t.Fatalf("result 对了但原因是别的事（%q）——这一格没进空文件判据", state.Error)
			}
			assertNoCalls(t, fx)
			if !reflect.DeepEqual(chain.applied, fx.applied) {
				t.Fatal("把 applied 换了")
			}
		})
	}
}

// TestReload_ValuelessFileIsSameConclusion 是第三轮复核补的那一族：文件里有键名、结构也在，
// 但一个取值都没有——`logging:` 后面空着、`workers: null`、`logging: {}`、整份就是一个 null、
// 层层下去都是空键，以及"值写在第二份文档里"（viper 只读第一份）。
//
// 它们在 core.LoadConfig 那里同样是一份合法的全默认配置，所以和第 1 类落进同一条判据；
// 差别在于这一族靠逐行扫文本永远扫不出来（第三轮复核就是用这个理由把第一版判据判回去的）。
func TestReload_ValuelessFileIsSameConclusion(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  []byte
	}{
		{"只有键名", []byte("logging:\n  level:\n")},
		{"显式 null 叶子", []byte("scheduler:\n  workers: null\n")},
		{"空流映射", []byte("logging: {}\n")},
		{"顶层 null", []byte("null\n")},
		{"层层都是空键", []byte("a:\n  b:\n    c:\n")},
		{"值写在第二份文档", []byte("---\n# 这一格还没填\n---\nlogging:\n  level: debug\n")},
		{"带 BOM 的只有键名", append([]byte{0xEF, 0xBB, 0xBF}, []byte("scheduler:\n  workers:\n")...)},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			fx := newReloadFixture(t, baseSpec(), baseSpec())
			chain := fx.build()

			if err := os.WriteFile(fx.path, tc.raw, 0o600); err != nil {
				t.Fatalf("写配置文件失败：%v", err)
			}
			// 这一族的成立前提是"第 1 步真能读通它"：读不通的话这里拦下的是别人的脸色，
			// 判据本身就退化成"文件里有字就不许过"。
			if _, err := core.LoadConfig(fx.path); err != nil {
				t.Fatalf("LoadConfig 读不通这一格（%v），它压根走不到第 1 步之后", err)
			}
			state, err := fx.reload()
			if err == nil || state.Result != core.ReloadRejected {
				t.Fatalf("交回 result=%q err=%v，应当是 rejected", state.Result, err)
			}
			if !strings.Contains(state.Error, "没有表达任何取值") {
				t.Fatalf("result 对了但原因是别的事（%q）——这一格没进空文件判据", state.Error)
			}
			assertNoCalls(t, fx)
			if !reflect.DeepEqual(chain.applied, fx.applied) {
				t.Fatal("把 applied 换了")
			}
		})
	}
}

// TestReload_StepOneShapesKeepTheirOwnWording 管的是两种脸色不许串门：不是合法 YAML 的文件
// （Tab 缩进、`----` 开头、不带 BOM 的 UTF-16）与读不出来的文件（被删），走的都是第 1 步那条
// "重新读取配置文件失败"，不许被这条判据说成"没有表达任何取值"。
//
// 为什么要专门写这一条：判据对"读不懂"答 false 之后，第 1 步自然接住它，这条链的行为是对的；
// 但如果哪天有人把判据改成"解析失败也当空文件"，现网就会收到一条误导性质的拒绝原因。
func TestReload_StepOneShapesKeepTheirOwnWording(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  []byte
	}{
		{"Tab 缩进的空白", []byte("\n \n\t\n")},
		{"四个横杠开头不是标记", []byte("---- 这不是标记\nscheduler:\n  workers: 9\n")},
		{"不带 BOM 的 UTF-16 注释", func() []byte {
			out := []byte{}
			for _, u := range utf16.Encode([]rune("# 只有注释\n")) {
				out = binary.LittleEndian.AppendUint16(out, u)
			}
			return out
		}()},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			fx := newReloadFixture(t, baseSpec(), baseSpec())
			chain := fx.build()

			if err := os.WriteFile(fx.path, tc.raw, 0o600); err != nil {
				t.Fatalf("写配置文件失败：%v", err)
			}
			state, err := fx.reload()
			if err == nil || state.Result != core.ReloadRejected {
				t.Fatalf("交回 result=%q err=%v，应当是 rejected", state.Result, err)
			}
			if strings.Contains(state.Error, "没有表达任何取值") {
				t.Fatalf("这一格本来读不通，不该说成空文件：%q", state.Error)
			}
			if !strings.Contains(state.Error, "重新读取配置文件失败") {
				t.Fatalf("没走第 1 步那条统一口径：%q", state.Error)
			}
			assertNoCalls(t, fx)
			if !reflect.DeepEqual(chain.applied, fx.applied) {
				t.Fatal("把 applied 换了")
			}
		})
	}

	t.Run("文件被删", func(t *testing.T) {
		fx := newReloadFixture(t, baseSpec(), baseSpec())
		chain := fx.build()

		if err := os.Remove(fx.path); err != nil {
			t.Fatalf("删掉配置文件失败：%v", err)
		}
		state, err := fx.reload()
		if err == nil || state.Result != core.ReloadRejected {
			t.Fatalf("交回 result=%q err=%v，应当是 rejected", state.Result, err)
		}
		if strings.Contains(state.Error, "没有表达任何取值") {
			t.Fatalf("文件被删不该被说成空文件：%q", state.Error)
		}
		if !strings.Contains(state.Error, "重新读取配置文件失败") {
			t.Fatalf("没走第 1 步那条统一口径：%q", state.Error)
		}
		assertNoCalls(t, fx)
		if !reflect.DeepEqual(chain.applied, fx.applied) {
			t.Fatal("把 applied 换了")
		}
	})
}

// TestReload_LeadingBOMDoesNotLookEmpty 是上面几条的正向对照：带 BOM 但真有内容的文件不许被
// 这条判据拦下来。少了这一格，"凡是带 BOM 一律拒"这种过头实现也能全绿。
func TestReload_LeadingBOMDoesNotLookEmpty(t *testing.T) {
	fx := newReloadFixture(t, baseSpec(), baseSpec())
	fx.build()

	fx.write(specChange(func(s *fileSpec) { s.level = "debug" }))
	raw, err := os.ReadFile(fx.path)
	if err != nil {
		t.Fatalf("读回配置文件失败：%v", err)
	}
	if err := os.WriteFile(fx.path, append([]byte{0xEF, 0xBB, 0xBF}, raw...), 0o600); err != nil {
		t.Fatalf("写带 BOM 的配置文件失败：%v", err)
	}

	state, err := fx.reload()
	if err != nil {
		t.Fatalf("带 BOM 的正常配置被拒了：%v（error=%s）", err, state.Error)
	}
	if strings.Contains(state.Error, "没有表达任何取值") {
		t.Fatalf("带 BOM 的正常配置被判成空文件：%s", state.Error)
	}
	assertExactCalls(t, fx, []string{"setLevel=debug"})
}

// TestReload_FlowMappingWithMarkerKeepsApplied 管的是另一个方向的过头实现：
// `--- {logging: {level: debug}}` 这种"文档标记后面紧跟流式取值"的整行，
// 前一轮那版按前缀跳分隔符的实现会连内容一起跳掉，于是真配置被误判成空文件。
func TestReload_FlowMappingWithMarkerKeepsApplied(t *testing.T) {
	fx := newReloadFixture(t, baseSpec(), baseSpec())
	fx.build()

	if err := os.WriteFile(fx.path, []byte("--- {logging: {level: debug}}\n"), 0o600); err != nil {
		t.Fatalf("写流式配置失败：%v", err)
	}
	state, err := fx.reload()
	if err != nil {
		t.Fatalf("标记后面的真取值被拒了：%v（error=%s）", err, state.Error)
	}
	if strings.Contains(state.Error, "没有表达任何取值") {
		t.Fatalf("这份文件明明有取值：%s", state.Error)
	}
	// 这一份只写了 logging.level，没写的键按"整份文件是唯一真相"退回默认（D-R0703），
	// 所以流水不止一条；这里要判的是它被当成"有取值的一份配置"走完了整条链。
	if state.Result != core.ReloadOK {
		t.Fatalf("result = %q, want %q", state.Result, core.ReloadOK)
	}
	calls := fx.targets.recorded()
	if !strings.Contains(strings.Join(calls, ","), "setLevel=debug") {
		t.Fatalf("标记后面的取值没落到落点上：%v", calls)
	}
}

// TestConfigFileCarriesNoValuesShapes 直接量判据本身，不走整条链。
//
// 存在的理由有两个：
//   - 链上那几条用例只能证"被拒的那一次给了空文件文案"，证不到这一族的边界在哪里：
//     哪些形状算"没表达取值"、哪些算"表达了"（空序列是作者写的"这一族清空"，
//     非映射标量、真取值、坏 YAML 都不归这条判据管）。
//   - 判据的返回语义（读不出文件时交回错误）也要有一格点到点的用例，链上碰不到那一支。
func TestConfigFileCarriesNoValuesShapes(t *testing.T) {
	writeAndAsk := func(t *testing.T, raw []byte) bool {
		t.Helper()
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatalf("write %q: %v", path, err)
		}
		noValues, err := configFileCarriesNoValues(path)
		if err != nil {
			t.Fatalf("判据读不出自己写的文件：%v", err)
		}
		return noValues
	}
	utf16WithBOM := func(s string) []byte {
		out := []byte{0xFF, 0xFE}
		for _, u := range utf16.Encode([]rune(s)) {
			out = binary.LittleEndian.AppendUint16(out, u)
		}
		return out
	}

	for _, tc := range []struct {
		name string
		raw  []byte
		want bool
	}{
		{"空文件", []byte(""), true},
		{"只有换行与空格", []byte("\n  \n\n"), true},
		{"只有注释", []byte("# 一会儿填回来\n"), true},
		{"注释里带 #! 之类的前缀", []byte("#!/usr/bin/env 不是键\n"), true},
		{"只有文档标记", []byte("---\n...\n"), true},
		{"标记后跟注释", []byte("--- # 还没填\n"), true},
		{"UTF-8 BOM 独存", []byte{0xEF, 0xBB, 0xBF}, true},
		{"UTF-8 BOM 加注释", append([]byte{0xEF, 0xBB, 0xBF}, []byte("# 注释\n")...), true},
		{"UTF-16LE 加注释", utf16WithBOM("# 注释\n"), true},
		{"只有键名", []byte("logging:\n  level:\n"), true},
		{"显式 null 叶子", []byte("scheduler:\n  workers: null\n"), true},
		{"空流映射", []byte("logging: {}\n"), true},
		{"顶层 null", []byte("null\n"), true},
		{"层层都是空键", []byte("a:\n  b:\n    c:\n"), true},
		{"非字符串键的空值", []byte("1:\n2:\n"), true},
		{"值写在第二份文档", []byte("---\n# 占位\n---\nlogging:\n  level: debug\n"), true},

		{"一行真取值", []byte("logging:\n  level: debug\n"), false},
		{"流式映射跟在标记后面", []byte("--- {logging: {level: debug}}\n"), false},
		{"流式映射单独一行", []byte("{scheduler: {workers: 9}}\n"), false},
		{"带 BOM 的真取值", append([]byte{0xEF, 0xBB, 0xBF}, []byte("scheduler:\n  workers: 9\n")...), false},
		{"显式空序列是作者意图", []byte("executors:\n  commands: []\n"), false},
		{"空字符串也算写下来的取值", []byte("logging:\n  level: \"\"\n"), false},
		{"顶层是标量", []byte("hello\n"), false},
		{"四个横杠不是文档标记", []byte("---- 这不是标记\n"), false},
		{"三个点后面紧跟键", []byte("...logging: x\n"), false},
		{"Tab 缩进的空白读不懂不归这里判", []byte("\n \n\t\n"), false},
		{"不带 BOM 的 UTF-16 读不懂不归这里判", func() []byte {
			out := []byte{}
			for _, u := range utf16.Encode([]rune("# 只有注释\n")) {
				out = binary.LittleEndian.AppendUint16(out, u)
			}
			return out
		}(), false},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if got := writeAndAsk(t, tc.raw); got != tc.want {
				t.Fatalf("configFileCarriesNoValues = %v, want %v（原文 %q）", got, tc.want, string(tc.raw))
			}
		})
	}

	// 读不出文件那一支交回的是错误，不是 false：调用方据此走第 1 步那条统一口径，
	// 不许把"文件不见了"说成"文件是空的"。
	t.Run("文件不存在交回错误", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "no-such-config.yaml")
		noValues, err := configFileCarriesNoValues(path)
		if err == nil {
			t.Fatal("文件不存在却没交回错误")
		}
		if noValues {
			t.Fatal("读不出文件时不许判成空文件")
		}
	})
}

// TestReload_UnchangedDoesNotTouchExecutors 守住"每次存盘都重登记档位"这条退路：
// 内容等价时一个落点都不许动，结论是 unchanged。
func TestReload_UnchangedDoesNotTouchExecutors(t *testing.T) {
	fx := newReloadFixture(t, baseSpec(), baseSpec())
	chain := fx.build()

	state, err := fx.reload()
	if err != nil {
		t.Fatalf("无变化不该返回错误：%v", err)
	}
	if state.Result != core.ReloadUnchanged {
		t.Fatalf("result = %q, want %q", state.Result, core.ReloadUnchanged)
	}
	assertNoCalls(t, fx)
	if !reflect.DeepEqual(chain.applied, fx.applied) {
		t.Fatal("无变化那次不该动 applied")
	}
	if !state.LastAppliedAt.IsZero() {
		t.Fatalf("无变化却给了 LastAppliedAt = %v", state.LastAppliedAt)
	}
	// 文案要说清"被环境变量压住的键也长这样"（设计文档 §5.3、待拍板 P4）。
	if !strings.Contains(fx.log(), "环境变量") {
		t.Fatalf("unchanged 的日志没交代环境变量这一层：\n%s", fx.log())
	}
}

// TestReload_RestartKeysAreReportedNotApplied 是 I3 的正身：只改重启档也要说出口。
// 顺带钉住两条读数：reload.enabled 改了不生效（监听器不会因此停），
// 以及 applied 会带着那份没生效的重启取值往前走（设计 §4 的既定形状，见卡 §10.2）。
func TestReload_RestartKeysAreReportedNotApplied(t *testing.T) {
	fx := newReloadFixture(t, baseSpec(), specChange(func(s *fileSpec) {
		s.queueCapacity = "7"
		s.reloadEnabled = "false"
	}))
	chain := fx.build()

	state, err := fx.reload()
	if err != nil {
		t.Fatalf("只改重启档不该让整次失败：%v", err)
	}
	if state.Result != core.ReloadOK {
		t.Fatalf("result = %q, want %q（error=%q）", state.Result, core.ReloadOK, state.Error)
	}
	assertNoCalls(t, fx)
	want := []string{"reload.enabled", "scheduler.queue_capacity"}
	if !reflect.DeepEqual(state.IgnoredKeys, want) {
		t.Fatalf("ignored_keys = %v, want %v", state.IgnoredKeys, want)
	}
	if len(state.AppliedKeys) != 0 {
		t.Fatalf("applied_keys = %v, want 空", state.AppliedKeys)
	}
	if !state.LastAppliedAt.IsZero() {
		t.Fatalf("一个落点都没动却给了 LastAppliedAt = %v", state.LastAppliedAt)
	}
	// HasChanges 为真才没走 unchanged 分支——这条断言证明上面那个 ok 不是因为"被当成无变化"。
	if change := core.Diff(fx.applied, fx.candidateOf()); !change.HasChanges() {
		t.Fatal("夹具坏了：两侧取值其实没有差别，这条用例就成了空转")
	}
	if !strings.Contains(fx.log(), "reload.enabled is a restart-class key") ||
		!strings.Contains(fx.log(), "does not stop") {
		t.Fatalf("改了 reload.enabled 却没告诉人监听器不会因此停掉：\n%s", fx.log())
	}
	if chain.applied.Reload.Enabled {
		t.Fatal("applied 应当带上文件里那份新的 reload.enabled：链不等重启，但它要如实记录文件说了什么")
	}
	// 链交回的 Enabled 钉在建链时那一份上：上面那次成功把文件里的 false 带进了 applied，
	// 而监听器并没有因此停下来，读数若跟着 applied 走就会说"这台进程没开热重载"。
	second := state
	fx.write(specChange(func(s *fileSpec) {
		s.queueCapacity = "7"
		s.reloadEnabled = "false"
		s.level = "debug"
	}))
	state, err = fx.reload()
	if err != nil || state.Result != core.ReloadOK {
		t.Fatalf("第二次重载没成：%v（result=%q error=%q）", err, state.Result, state.Error)
	}
	if !state.Enabled || !second.Enabled {
		t.Fatalf("交回的 Enabled 跟着 applied 漂了：第一次 %v 第二次 %v", second.Enabled, state.Enabled)
	}
}

// TestReload_ObservabilityKeysSkippedWhenDisabled 是卡 §3.2 第 5 行的口径：
// 写入器没启用（对应函数为 nil）时那两条键改动跳过并记入 IgnoredKeys，而不是失败。
func TestReload_ObservabilityKeysSkippedWhenDisabled(t *testing.T) {
	fx := newReloadFixture(t, baseSpec(), specChange(func(s *fileSpec) {
		s.eventCount = "5"
		s.eventAge = "72h"
	}))
	chain := fx.buildWithout("setEventRetention", "setAuditRetention")

	state, err := fx.reload()
	if err != nil {
		t.Fatalf("观测层没启用不该让整次失败：%v", err)
	}
	if state.Result != core.ReloadOK {
		t.Fatalf("result = %q, want %q", state.Result, core.ReloadOK)
	}
	assertNoCalls(t, fx)
	for _, key := range []string{keyObservabilityEventCount, keyObservabilityEventAge} {
		if !containsKey(state.IgnoredKeys, key) {
			t.Fatalf("ignored_keys = %v，少了 %q", state.IgnoredKeys, key)
		}
		if containsKey(state.AppliedKeys, key) {
			t.Fatalf("applied_keys = %v，不该有被跳过的那两条", state.AppliedKeys)
		}
	}
	if !strings.Contains(fx.log(), "skipped") {
		t.Fatalf("跳过没留下可读的结论：\n%s", fx.log())
	}
	if !reflect.DeepEqual(chain.applied, fx.candidateOf()) {
		t.Fatal("跳过不等于没改：applied 要推进到 candidate，否则下一次存盘还会再报一遍同样的键")
	}
}

// TestReload_CommandsWithExecutorsDisabled 是卡 §3.1 第三条的特殊处理：
// 执行器没启用时档位改动记 warn 但结论仍为 ok，键留在 applied_keys。
func TestReload_CommandsWithExecutorsDisabled(t *testing.T) {
	fx := newReloadFixture(t, baseSpec(), specChange(func(s *fileSpec) {
		s.commandTimeout = "30s"
	}))
	fx.buildWithout("setCommands")

	state, err := fx.reload()
	if err != nil {
		t.Fatalf("执行器未启用不该让整次失败：%v", err)
	}
	if state.Result != core.ReloadOK {
		t.Fatalf("result = %q, want %q", state.Result, core.ReloadOK)
	}
	assertNoCalls(t, fx)
	if !containsPrefix(state.AppliedKeys, keyExecutorsCommands) {
		t.Fatalf("applied_keys = %v，档位那条改动要留在里面（配合下面的 warn 一起读）", state.AppliedKeys)
	}
	if !strings.Contains(fx.log(), "not in effect") {
		t.Fatalf("没有一条 warn 说明没生效：\n%s", fx.log())
	}
}

// TestReload_ConcurrentReloadsSerialized 判两层串行的第二层（链自己的 mu）：
// 两个 goroutine 直接调 Reload，临界区不许重叠，且第二次要能看到第一次的结果。
func TestReload_ConcurrentReloadsSerialized(t *testing.T) {
	fx := newReloadFixture(t, baseSpec(), specChange(func(s *fileSpec) {
		s.level = "debug"
		s.workers = "16"
	}))
	fx.targets.gateOn = "setLevel"
	chain := fx.build()

	firstDone := make(chan core.ReloadState, 1)
	go func() {
		state, _ := chain.Reload()
		firstDone <- state
	}()

	// 等到第一次确实进到 setLevel 里被闸门卡住，再起第二次：这样"第二次必须等着"才是判据。
	waitArrived(t, fx)
	defer fx.targets.release()

	secondDone := make(chan core.ReloadState, 1)
	go func() {
		state, _ := chain.Reload()
		secondDone <- state
	}()

	select {
	case <-secondDone:
		t.Fatal("第一次还在临界区里，第二次就调完了落点——链没自己串行")
	case <-time.After(150 * time.Millisecond):
		// 等到这里说明第二次确实卡在锁上，符合预期。
	}

	fx.targets.release()

	first := <-firstDone
	second := <-secondDone
	if first.Result != core.ReloadOK {
		t.Fatalf("第一次 result = %q, want %q", first.Result, core.ReloadOK)
	}
	// 第二次看到的是已经被推进的 applied：同一份文件再读一遍就是无变化。
	if second.Result != core.ReloadUnchanged {
		t.Fatalf("第二次 result = %q, want %q（applied 没被第一次推进？）", second.Result, core.ReloadUnchanged)
	}
	if peak := fx.targets.peakOverlap(); peak != 1 {
		t.Fatalf("落点调用峰值 = %d, want 1（两次重载的临界区重叠了）", peak)
	}
	// 落点只被跑了一遍：第二次不许把新值再应用一次（那是"重复登记"的形状）。
	assertExactCalls(t, fx, []string{"setLevel=debug", "setWorkers=16"})
}

// TestReload_StopWaitsInFlightChain 判 reloader.Stop 的两半：等在途那一串，
// 并让之后的调用立刻返回且不动任何落点。
//
// defer 次序按 R03/R05 的教训来：等到卡住之后立刻注册 release，而且 release 幂等
// （sync.Once），失败路径上不可能留下一个永远等不到的闸门。
func TestReload_StopWaitsInFlightChain(t *testing.T) {
	fx := newReloadFixture(t, baseSpec(), specChange(func(s *fileSpec) {
		s.level = "debug"
		s.workers = "24"
	}))
	fx.targets.gateOn = "setWorkers"
	chain := fx.build()

	reloadDone := make(chan struct{})
	go func() {
		defer close(reloadDone)
		_, _ = chain.Reload()
	}()
	waitArrived(t, fx)
	defer fx.targets.release()

	stopDone := make(chan struct{})
	go func() {
		defer close(stopDone)
		chain.Stop()
	}()

	select {
	case <-stopDone:
		fx.targets.release()
		t.Fatal("链还卡在 setWorkers 上，Stop 就返回了——那等于没等在途")
	case <-time.After(150 * time.Millisecond):
	}

	fx.targets.release()

	select {
	case <-stopDone:
	case <-time.After(10 * time.Second):
		t.Fatal("闸门已放开而 Stop 仍不返回")
	}
	<-reloadDone

	before := fx.targets.recorded()
	state, err := chain.Reload()
	if err != nil {
		t.Fatalf("Stop 之后 Reload 不该报错：%v", err)
	}
	if state.Result != "" || state.Enabled || state.WatchedPath != "" || state.Error != "" ||
		!state.LastAttemptAt.IsZero() || !state.LastAppliedAt.IsZero() ||
		state.AppliedKeys != nil || state.IgnoredKeys != nil || state.RejectedKeys != nil {
		t.Fatalf("Stop 之后交回了非空结论：%+v", state)
	}
	if after := fx.targets.recorded(); len(after) != len(before) {
		t.Fatalf("Stop 之后又跑了落点：%v", after[len(before):])
	}
}

// TestReload_AbsurdWorkerCountFailsAndRollsBack 收 R03 交接给本卡的 D3：
// ResizeWorkers 只有下限，链要拦住 scheduler.workers: 100000 这种写法，
// 而且要拦住 = 一次都不递给调度器 + 前面那几步照原样回滚。
func TestReload_AbsurdWorkerCountFailsAndRollsBack(t *testing.T) {
	fx := newReloadFixture(t, baseSpec(), specChange(func(s *fileSpec) {
		s.level = "debug"
		s.debounce = "200ms"
		s.workers = "100000"
	}))
	chain := fx.build()

	state, err := fx.reload()
	if err == nil {
		t.Fatal("荒谬的并发数被接受了")
	}
	if state.Result != core.ReloadFailed {
		t.Fatalf("result = %q, want %q", state.Result, core.ReloadFailed)
	}
	if !strings.Contains(state.Error, "上界") {
		t.Fatalf("error 没说是上界拦住的：%q", state.Error)
	}
	assertExactCalls(t, fx, []string{
		"setLevel=debug", "setDebounce=200ms", "setDebounce=500ms", "setLevel=info",
	})
	if !reflect.DeepEqual(chain.applied, fx.applied) {
		t.Fatal("失败那次推进了 applied")
	}
}

// TestReload_WorkerBoundBoundaryIsExact 钉住上界这个数本身：4096 要放行、4097 要拦。
// 只测"100000 被拦"的话，把常数写成 12 也照样过，而那种写法会把正常规模的扩缩一起挡掉。
func TestReload_WorkerBoundBoundaryIsExact(t *testing.T) {
	for _, tc := range []struct {
		workers string
		wantErr bool
	}{
		{"4095", false},
		{"4096", false},
		{"4097", true},
	} {
		t.Run("workers="+tc.workers, func(t *testing.T) {
			fx := newReloadFixture(t, baseSpec(), specChange(func(s *fileSpec) {
				s.workers = tc.workers
			}))
			fx.build()

			state, err := fx.reload()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("scheduler.workers=%s 越界却被接受了", tc.workers)
				}
				assertExactCalls(t, fx, nil)
				if state.Result != core.ReloadFailed {
					t.Fatalf("result = %q, want %q", state.Result, core.ReloadFailed)
				}
				return
			}
			if err != nil {
				t.Fatalf("scheduler.workers=%s 在上界之内却被拒：%v", tc.workers, err)
			}
			assertExactCalls(t, fx, []string{"setWorkers=" + tc.workers})
		})
	}
}

// TestReload_MissingEntryFailsLoudly 是"归档成热更、但装配没给入口"的保险丝：
// 那种装配错误必须让整次作废并且在动手之前就停住，而不是悄悄跳过、也不是先改一半再回滚。
func TestReload_MissingEntryFailsLoudly(t *testing.T) {
	fx := newReloadFixture(t, baseSpec(), specChange(func(s *fileSpec) {
		s.level = "debug"
		s.historyLimit = "3"
		s.queueCapacity = "9" // 同一次改动里的重启档：整次作废时也要把这份清单说出口
	}))
	chain := fx.buildWithout("setRetention")

	state, err := fx.reload()
	if err == nil {
		t.Fatal("缺入口被静默接受了")
	}
	if state.Result != core.ReloadFailed {
		t.Fatalf("result = %q, want %q（缺入口属装配错误，不该被说成拒绝档）",
			state.Result, core.ReloadFailed)
	}
	if !strings.Contains(state.Error, keyStoreHistoryLimit) {
		t.Fatalf("error 里没有那条键名，日志无法归因：%q", state.Error)
	}
	// 前置检查在应用之前：连排在最前面的日志级别都不许被动过。
	assertNoCalls(t, fx)
	if !containsKey(state.IgnoredKeys, "scheduler.queue_capacity") {
		t.Fatalf("ignored_keys = %v，作废那次也要把重启档的清单带上（I3）", state.IgnoredKeys)
	}
	if !reflect.DeepEqual(chain.applied, fx.applied) {
		t.Fatal("缺入口那次推进了 applied")
	}
}

// TestReload_NoConfigPathIsNotSilence 判 cfgPath 为空那一条：链不该报"无事发生"。
func TestReload_NoConfigPathIsNotSilence(t *testing.T) {
	fx := newReloadFixture(t, baseSpec(), baseSpec())
	chain := fx.build()
	chain.cfgPath = ""

	state, err := fx.reload()
	if err == nil {
		t.Fatal("没有可盯的文件却报成功")
	}
	assertNoCalls(t, fx)
	if state.Result != core.ReloadRejected {
		t.Fatalf("result = %q, want %q", state.Result, core.ReloadRejected)
	}
	if !strings.Contains(state.Error, "配置文件路径") {
		t.Fatalf("error 说得不明不白：%q", state.Error)
	}
}

// ---- §5.1 附加：分派表自身的守卫 ----

// hotLeafPathsFromConfig 用反射把 Config 的每个标量叶子都改一次取值，交给 core.Diff 分类，
// 收回"会被判成热更"的那些路径。
//
// 为什么用反射而不是抄一份清单：抄的清单会与 configClasses 各自漂（R01 的
// TestEveryLeafKeyIsClassed 守的是 core 内部的表与结构体同步，守不到 cmd/server 的分派表）。
// 反射这份只走标量：切片与映射（档位列表、账号表、白名单）由前缀认领那一条单独判。
func hotLeafPathsFromConfig(t *testing.T) []string {
	t.Helper()

	base := core.DefaultConfig()
	mutated := core.DefaultConfig()
	bumpScalars(reflect.ValueOf(&mutated).Elem())

	change := core.Diff(base.Normalized(), mutated.Normalized())
	var hot []string
	for _, key := range change.Hot {
		hot = append(hot, key.Path)
	}
	if len(hot) == 0 {
		t.Fatal("反射改值之后 Diff 一条热更都没报出来，这条守卫已经空转")
	}
	return hot
}

// bumpScalars 给每个可寻址的标量字段换一个肯定不同的取值。
func bumpScalars(value reflect.Value) {
	for i := 0; i < value.NumField(); i++ {
		field := value.Type().Field(i)
		if field.PkgPath != "" || !value.Field(i).CanSet() {
			continue
		}
		target := value.Field(i)
		switch target.Kind() {
		case reflect.Struct:
			bumpScalars(target)
		case reflect.String:
			target.SetString("zz-reload-fuse")
		case reflect.Bool:
			target.SetBool(!target.Bool())
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			target.SetInt(target.Int() + 7)
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			target.SetUint(target.Uint() + 7)
		case reflect.Float32, reflect.Float64:
			target.SetFloat(target.Float() + 0.5)
		default:
			// 切片、映射、指针一律跳过：它们要么落在 executors.commands 的前缀认领里，
			// 要么（server.auth.users）本来就不该被链分派成单条键。
		}
	}
}

// TestReloadEveryHotKeyHasDispatchEntry 是三件事合起来的一条守卫
// （设计文档 §8 的 TestHotKeysAreEffective 在 cmd/server 侧的落地形状）：
//
//  1. 反射摊出的每一条热更键都被**恰好一步**认领；
//  2. 链的常量清单与反射摊出的集合逐条相等——两边谁先漂都会红；
//  3. 有一条热更键没有分派分支时，applyChange 一条都不应用。
func TestReloadEveryHotKeyHasDispatchEntry(t *testing.T) {
	fx := newReloadFixture(t, baseSpec(), baseSpec())
	chain := fx.build()

	// 常量清单：链认领的每一条热更键。档位那一节是前缀认领（摊成 <name>.<field>），
	// 单列在下面第二组断言里。
	scalarConstants := []string{
		keyLoggingLevel, keySchedulerWorkers, keySchedulerMaxRetryDelay,
		keyStoreHistoryLimit, keyStoreHistoryTTL,
		keyObservabilityEventCount, keyObservabilityEventAge,
		keyObservabilityAuditCount, keyObservabilityAuditAge,
		keyReloadDebounce,
	}

	hot := hotLeafPathsFromConfig(t)
	// 加上档位形状的三条：容器路径（列表为空时出现）、以及两种子路径形态。
	probe := append(append([]string{}, hot...),
		keyExecutorsCommands, "executors.commands.echo.timeout", "executors.commands.newname.script")

	plan, unmatched := chain.buildPlan(probe)
	if len(unmatched) > 0 {
		t.Fatalf("这些热更键在链里没有分派分支：%v", unmatched)
	}

	// 每条键恰好被一步认领：被两步抢着认领也算错（会重复应用）。
	claimed := map[string]int{}
	for _, step := range plan {
		for _, key := range step.matched {
			claimed[key]++
		}
	}
	for _, key := range probe {
		if claimed[key] != 1 {
			t.Fatalf("热更键 %q 被 %d 步认领，期望恰好 1", key, claimed[key])
		}
	}

	// 两份清单互检：反射摊出的热更集合里不该有档位路径（bumpScalars 跳过切片），
	// 于是它应当与常量清单逐条相等。
	if containsPrefix(hot, keyExecutorsCommands) {
		t.Fatalf("反射摊出的热更集合里出现了档位路径，这份对照要按标量清单来：%v", hot)
	}
	if extra := exceptKeysBySet(scalarConstants, hot); len(extra) > 0 {
		t.Fatalf("链认领了这些键，可 core 的 Diff 没把它们判成热更：%v", extra)
	}
	if missing := exceptKeysBySet(hot, scalarConstants); len(missing) > 0 {
		t.Fatalf("core 判成热更的键不在链的常量清单里：%v，去 reload.go 补一条分支与常量", missing)
	}

	// 第三条：假想一条没人认领的热更键（模拟"core 加了键而链没跟上"的中间态）。
	// 顺带钉住两条前置检查的读数对称：作废那次交回的 ignored_keys 仍是重启档那份清单
	// （分派表漏项与装配缺入口是同一种脸色，清单不该一边有一边没有）。
	outcome, failure, rollbacks := chain.applyChange(
		core.ConfigChange{
			Hot:     []core.ChangedKey{{Path: "brand.new.hot.key"}},
			Restart: []core.ChangedKey{{Path: "server.port"}},
		},
		fx.candidateOf())
	if failure == nil {
		t.Fatal("没人认领的热更键被静默接受了")
	}
	if rollbacks != nil {
		t.Fatalf("还没开始应用就产生了回滚：%v", rollbacks)
	}
	if !containsKey(outcome.ignoredKeys, "server.port") {
		t.Fatalf("ignored_keys = %v，缺分派那一次也该带上重启档", outcome.ignoredKeys)
	}
	assertNoCalls(t, fx)
}

// ---- 断言与工具 ----

func assertExactCalls(t *testing.T, fx *reloadFixture, want []string) {
	t.Helper()
	got := fx.targets.recorded()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("落点调用流水\n got = %v\nwant = %v", got, want)
	}
}

func assertNoCalls(t *testing.T, fx *reloadFixture) {
	t.Helper()
	if got := fx.targets.recorded(); len(got) != 0 {
		t.Fatalf("不该有任何落点被调用，实际流水 = %v", got)
	}
}

func containsKey(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

func containsPrefix(list []string, prefix string) bool {
	for _, item := range list {
		if item == prefix || strings.HasPrefix(item, prefix+".") {
			return true
		}
	}
	return false
}

// exceptKeysBySet 返回 a 中不在 b 里的元素。
func exceptKeysBySet(a, b []string) []string {
	set := map[string]bool{}
	for _, item := range b {
		set[item] = true
	}
	var out []string
	for _, item := range a {
		if set[item] {
			continue
		}
		out = append(out, item)
	}
	return out
}

// waitArrived 等到被卡住的那次调用真的进到落点里。
// 上界 20s：宁可让这一条用例失败，也不要把整个包拖到测试超时（R03/R05 各踩过一次）。
func waitArrived(t *testing.T, fx *reloadFixture) {
	t.Helper()
	select {
	case name := <-fx.targets.arrived:
		if name != fx.targets.gateOn {
			fx.targets.release()
			t.Fatalf("闸门设在 %q 却等到 %q", fx.targets.gateOn, name)
		}
	case <-time.After(20 * time.Second):
		fx.targets.release()
		t.Fatalf("没等到 %q 被调用，后面的顺序判据都是空的", fx.targets.gateOn)
	}
}
