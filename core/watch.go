package core

// 本文件是配置热重载的"什么时候该问一次"部分（设计文档 §5、TASK-R05）。
//
// ConfigWatcher 不认识任何业务键、不读配置文件内容、不应用任何取值：
// 它只盯住启动时真正读到的那一份 YAML 所在的目录，在防抖窗口结束时调用装配方
// 提供的 ReloadFunc，并把这次尝试交回的结论原样存进 ReloadState。
// "读回来、比一比、应用"属于重载链（R06），本文件不涉及。
//
// 之所以不 import 任何业务侧、也不调用配置的读取与比对，是为了让这个类型天然与业务无关：
// 判据表在 config_reload.go，装配在 cmd/server，本文件只负责触发时机与监听器自身的健康。

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
)

// loopStopWait 是 Close 等待事件循环返回的上界。正常路径下循环会在毫秒级退出；
// 超过这个上界说明循环卡在某个不该卡的调用里，Close 把它作为错误报出而不是无声返回。
const loopStopWait = 5 * time.Second

// ReloadFunc 是一次重载的执行体：读配置、比对、应用，成功返回 nil。
// 它由装配方提供（R06），watcher 只在防抖窗口结束时调它，并保存它返回的状态。
//
// 契约（两条，都得守）：
//  1. **调用由 watcher 串行**：callReload 持 reloadMu 跑完 onReload 并保存状态，
//     所以任何时刻至多一个 ReloadFunc 在跑，后触发的那次不会先落盘、再被前一次的旧结果覆盖。
//     注意调用点是防抖计时器的协程（time.AfterFunc 的回调 → trigger → callReload），
//     不是事件循环本身——串行靠这把锁，不靠"都跑在循环上"（那样并不成立）。
//  2. **实现方要自己与进程内其它写入口互斥**：reloadMu 只排这一个 watcher 的调用顺序，
//     管不到档位在线管理的写链（R06 的写链锁归它自己），所以要同步且不自吞状态。
//
// 调用可能在 Run/Close 返回之后才结束，实现方不能假设调用点被它们收住：Close 按卡 §3 的要求
// 不强杀也不等待在途重载（它可能正握着写链的锁，等它只会把关停拖死），reloadMu 因此只排序、不 join。
// watcher 保证的是另一头：closed 位点让 Run/Close 返回之后不再**新起**一次调用。
type ReloadFunc func() (ReloadState, error)

// ConfigWatcher 盯一份配置文件，变化后按防抖窗口触发一次重载。
//
// 三条设计选择：
//  1. 监听**目录**而不是文件本身：编辑器改文件最常见的是"写临时文件 + rename 覆盖"，
//     直接盯文件会让 inode 换掉后收不到任何事件（Windows 上表现不同，盯目录同样是对的）。
//  2. 不筛事件类型：任何落在目标文件上的事件都排一次防抖，真不真要重载由内容决定。
//  3. 状态由本类型保存并暴露：重载结论由 ReloadFunc 交回，watcher 只负责存最新一份
//     与并发可读（一次 atomic 换指针，不加锁读）。
//
// 与 §3 草图的形状差异记在卡 §10.2：用 loopDone/started 两个通道位点做结构化的
// 关停证明（取代 wg，wg.Add 在 `go Run` 的协程里跑会与 Close 的 Wait 竞态），
// 另加 closed 位点让关停后的排期与触发都短路、reloadMu 让 ReloadFunc 的调用串行、
// basename 取磁盘上的真实拼写（见 resolveDiskFileName）。
type ConfigWatcher struct {
	// 构造后不再变更
	path     string // 目标文件的绝对路径
	dirname  string // path 的目录，watcher 盯这里
	basename string // 磁盘上的真实文件名（构造期解析），用于精确比对事件
	// onReload 由装配方提供，watcher 只在窗口结束时调它
	onReload ReloadFunc
	logger   *slog.Logger

	// debounce 可运行期改（reload.debounce 是热更档），所以是原子纳秒。
	debounce atomic.Int64

	// state 是"最新一份重载状态"，一次换指针读写；closed/started 是关停与启动位点。
	state   atomic.Pointer[ReloadState]
	closed  atomic.Bool
	started atomic.Bool

	watcher *fsnotify.Watcher

	// mu 保护 timers 的读写；timers 按目标文件路径合并短时间内的多次事件，
	// 形状照 core/load.go 的 pendingLoads（同路径旧计时器 Reset 或重建）。
	mu     sync.Mutex
	timers map[string]*time.Timer

	// reloadMu 串行化 ReloadFunc 的调用与紧随其后的状态保存。触发者是计时器协程，
	// 两个防抖窗口的到期可以重叠（前一次还在跑、后一次已到期并重新计时），没有这把锁
	// 就会出现"旧结果覆盖新结果"。它只排序调用、不 join 在途调用：Close 不持也不等这把锁，
	// 所以握锁中的 ReloadFunc 拖不死关停（卡 §3 Close 注释、要求 11）。
	reloadMu sync.Mutex

	// stopCh 是主动关停信号，loopDone 由事件循环返回时关闭，Close 有界等待它。
	stopCh   chan struct{}
	loopDone chan struct{}
	stopOnce sync.Once
}

// NewConfigWatcher 建监听器。path 必须是启动时真正读到的那个文件的绝对路径
// （由调用方 filepath.Abs 之后再传）；不存在的路径返回错误——
// "盯着一个当前不存在的文件"意味着进程在用默认值，那种部署不该打开重载
// （设计文档 §5.2）。debounce<=0 用 DefaultReloadDebounce；onReload 为 nil 返回错误。
func NewConfigWatcher(path string, debounce time.Duration, onReload ReloadFunc, logger *slog.Logger) (*ConfigWatcher, error) {
	if onReload == nil {
		return nil, fmt.Errorf("config watcher: onReload must not be nil")
	}
	if path == "" {
		return nil, fmt.Errorf("config watcher: path must not be empty")
	}
	// 构造只收绝对路径，把"相对/空"这类判断前置到构造期，运行期不再处理。
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("config watcher: path must be absolute, got %q", path)
	}
	info, err := os.Stat(path)
	if err != nil {
		// 不存在（或不可 stat）即失败：盯一个当前不存在的文件不是本类型该支持的情形。
		return nil, fmt.Errorf("config watcher: target file not readable: %w", err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("config watcher: path is a directory, want a file: %q", path)
	}

	if debounce <= 0 {
		debounce = DefaultReloadDebounce
	}

	// basename 用磁盘上的真实拼写而不是入参里的那一段：Windows/macOS 默认卷大小写不敏感，
	// 用户手打的 -config 路径可以写成 Config.yaml，os.Stat 成功但 fsnotify 上报的是磁盘拼写，
	// 精确比对就会永不命中（详见 resolveDiskFileName）。解析不出来是部署级问题，直接报错。
	dirname := filepath.Dir(path)
	basename, err := resolveDiskFileName(dirname, filepath.Base(path))
	if err != nil {
		return nil, err
	}

	w := &ConfigWatcher{
		path:     path,
		dirname:  dirname,
		basename: basename,
		onReload: onReload,
		logger:   resolveLogger(logger),
		timers:   make(map[string]*time.Timer),
		stopCh:   make(chan struct{}),
		loopDone: make(chan struct{}),
	}
	w.debounce.Store(int64(debounce))

	// 初始状态：WatchedPath 立即可读，Result 为空（从未尝试过重载，卡 §3 的 State 约定）。
	w.state.Store(&ReloadState{WatchedPath: w.path})

	// 盯目录而不是文件：见类型注释的第 1 条设计选择。
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("config watcher: create fsnotify watcher failed: %w", err)
	}
	if err := fsw.Add(w.dirname); err != nil {
		_ = fsw.Close()
		return nil, fmt.Errorf("config watcher: watch directory failed: %w", err)
	}
	w.watcher = fsw
	return w, nil
}

// resolveDiskFileName 返回目录 dirname 里与 name 对应的那个**磁盘上的**文件名。
//
// 先要精确相等（大小写敏感的卷上入参本身就是磁盘拼写，直接返回，行为与从前一致），
// 找不到再按 strings.EqualFold 兜一次，返回条目的真实拼写。
//
// 它守的是哪一件事：Windows 与 macOS 默认卷大小写不敏感，用户在 -config 里手打的
// 路径完全可能写成 Config.yaml 或 CONFIG.yaml —— os.Stat 成功、watcher 建得出来，
// 但 fsnotify 上报的事件名字用的是磁盘拼写，handleEvent 的精确相等永不命中。
// 那时 State() 看着一切正常、监听器却再也不触发，正是本系列点名要消灭的"沉默"。
//
// 它不守哪些事，别误解：
//   - 不做大小写不敏感的内容/路径规范化，也不管 YAML 内部的键；
//   - 不管"文件被改名成另一种大小写拼写"——那是重新部署，本类型构造期一次性解析；
//   - 不处理同一目录里两个只差大小写的不同文件：那种卷本身不允许，
//     在大小写敏感的卷上精确相等那一路已经选定了 os.Stat 命中的那一份。
//
// 拆成独立函数也是为了让用例在任何平台都能验：用例造一个 config.yaml、传 CONFIG.yaml，
// 在大小写敏感的 Linux 上走的是"读目录后折叠匹配"这同一条代码路径。
func resolveDiskFileName(dirname, name string) (string, error) {
	entries, err := os.ReadDir(dirname)
	if err != nil {
		return "", fmt.Errorf("config watcher: read watch directory %q failed: %w", dirname, err)
	}
	folded := ""
	for _, entry := range entries {
		if entry.Name() == name {
			return name, nil
		}
		if folded == "" && strings.EqualFold(entry.Name(), name) {
			folded = entry.Name()
		}
	}
	if folded != "" {
		return folded, nil
	}
	return "", fmt.Errorf("config watcher: no entry in %q matches file name %q", dirname, name)
}

// Run 启动事件循环，直到 ctx 取消或 Close 被调用。它阻塞，调用方负责 go。
// ctx 与 stopCh 两个停止源都要支持：优雅关闭走 Close（同步，等事件循环返回；按卡 §3 它
// 不等在途重载），而测试用 context.WithCancel 更方便。
//
// 两条退出路径的收口一样（defer 顺序：先关 fsnotify 句柄、再停计时器、最后关 loopDone）：
//  1. stopTimers —— 卡要求 11 说的是"Run 返回后不再有计时器回调进入 onReload"，
//     ctx 取消同样是 Run 返回，不能只有 Close 那条路停表；
//  2. 关 fsnotify 句柄 —— 否则只用 ctx 停的调用方要把目录句柄一直漏到有人调 Close。
//     v1.9.0 的 fsnotify.Watcher.Close 各后端都先查 closed 位点、重复调用返回 nil
//     （Windows 后端 backend_windows.go:91，inotify/kqueue/fen 走 shared.close()），
//     所以这里关过之后 Close 再关一次是安全的，本文件就依赖这一点而不另设 once。
//
// 同一个 watcher 只跑一个事件循环：第二次 Run（包括 Close 之后再调）直接返回、什么都不做，
// 不 panic 也不抢第一个循环的位置。
func (w *ConfigWatcher) Run(ctx context.Context) {
	// started 由第一个进入的调用 CAS 置真；后进者原路返回。
	// 这也让 waitForLoop 的"从没跑过 Run 就无需等待"判断保持不变。
	if !w.started.CompareAndSwap(false, true) {
		return
	}
	// loopDone 关闭 = 事件循环确已返回，给 Close 的有界等待与测试的结构化证明用。
	defer close(w.loopDone)
	defer w.stopTimers()
	defer func() {
		if w.watcher != nil {
			if err := w.watcher.Close(); err != nil {
				w.logger.Error("failed to close config watcher on run exit", "error", err)
			}
		}
	}()

	events := w.watcher.Events
	errs := w.watcher.Errors

	for {
		select {
		case <-ctx.Done():
			return
		case <-w.stopCh:
			return
		case event, ok := <-events:
			if !ok {
				// 事件通道在 stopCh 与 ctx 都没触发的情况下关闭：监听器自身死了（卡 §4.2 前半句）。
				// 主动关停时 closed 已置真，这里不当故障处理。
				if w.closed.Load() {
					return
				}
				w.MarkWatcherError("config watcher event channel closed")
				return
			}
			w.handleEvent(event)
		case watchErr, ok := <-errs:
			if !ok {
				// error 通道关闭不等于 watcher 死亡（卡 §4.2 后半句）：摘掉这一路、继续监听 events。
				errs = nil
				continue
			}
			if watchErr != nil {
				// 记进状态与日志，但不退出循环。
				w.MarkWatcherError(fmt.Sprintf("config watcher error: %v", watchErr))
			}
		}
	}
}

// handleEvent 决定一个 fsnotify 事件是否落在目标文件上。
//
// 不筛事件类型（卡 §2 表第二行、要求 2）：Windows 上编辑器存盘的事件形状不可预测
// （Rename、Chmod、多次 Write），筛类型会漏触发；"这一次到底有没有变"交给 ReloadFunc，
// watcher 只判断"该不该问一次"。判据是文件名**精确相等**，不用 filepath.Match 也不用前缀：
// 配置文件的名字是启动时定下的，前缀或模式匹配会连带盯上 config.yaml.bak、config.yml~ 这类编辑备份。
//
// 比对的 w.basename 是构造期从目录里解析出的**磁盘拼写**（见 resolveDiskFileName）：
// fsnotify 上报的事件名字用的就是磁盘拼写，两边同一来源才不会因大小写错配而永不命中。
func (w *ConfigWatcher) handleEvent(event fsnotify.Event) {
	if filepath.Base(event.Name) != w.basename {
		return
	}
	w.schedule(w.path)
}

// schedule 在防抖窗口结束时触发一次重载；窗口内的重复事件只保留一次触发。
// 形状照 core/load.go 的 scheduleLoad（同路径计时器 Reset 后重建），不另发明一套。
func (w *ConfigWatcher) schedule(path string) {
	w.mu.Lock()
	defer w.mu.Unlock()

	// 关停后不再排期：Close 已把 closed 置真，之后的任何事件都不该再生出计时器。
	if w.closed.Load() {
		return
	}
	d := time.Duration(w.debounce.Load())
	if timer, ok := w.timers[path]; ok {
		// 新窗口从下一个事件起生效：已在计时的这一次不改窗口，不值得为它重新计时。
		timer.Reset(d)
		return
	}
	w.timers[path] = time.AfterFunc(d, func() {
		w.forget(path)
		w.trigger()
	})
}

// forget 在计时器触发后摘掉该路径的记录，让下一轮写入能重新排期。
func (w *ConfigWatcher) forget(path string) {
	w.mu.Lock()
	delete(w.timers, path)
	w.mu.Unlock()
}

// stopTimers 停掉所有在计时的防抖计时器并从 map 删除，照 core/load.go 的 stopLoaders 写法。
//
// Close 与 Run 的**每条**退出路径都要调它（Run 里是 defer）：卡要求 11 说的是"Run 返回后
// 不再有计时器回调进入 onReload"，而 Run 有 ctx 取消与 stopCh 两条返回路径，只在 Close 里清
// 就等于让 ctx 那条路留下能打进 ReloadFunc 的计时器。
func (w *ConfigWatcher) stopTimers() {
	w.mu.Lock()
	for path, timer := range w.timers {
		timer.Stop()
		delete(w.timers, path)
	}
	w.mu.Unlock()
}

// trigger 是计时器到点后的入口（计时器协程上跑）；关停后短路，保证 closed 置真之后
// 不再**新起**一次 ReloadFunc 调用。
//
// 这条短路有一个无法在本卡消掉的残余竞态：回调可能刚好在 closed 置真的前一瞬通过检查，
// 于是一次关停后开始的调用成为可能。窗口只有几条指令宽，且 Close 已先停掉 timers；
// 用例 TestConfigWatcher_CloseIdempotent 会在 Close 返回后直接调 trigger 验短路本身。
func (w *ConfigWatcher) trigger() {
	if w.closed.Load() {
		return
	}
	w.callReload()
}

// callReload 调用 ReloadFunc 并把结论存下来。
//
// 整个"调用 + 保存"在 reloadMu 里做完：调用者是防抖计时器的协程，不是事件循环，
// 两个窗口的到期时间可以重叠（前一次还在跑、后一次已经重新计时并到期）。不串行就会出现
// 两次调用并发跑、后发起的那次先返回、反而被前一次的旧结果覆盖 State()（ReloadFunc 契约第 1 条）。
// 这把锁只排序、不 join：Close 不等它，握锁中的重载因此拖不死关停。
//
// ReloadFunc panic 不能打挂进程（卡 §9 风险表第一行）：调用点是计时器协程，那上面没有别的
// recover，未捕获的 panic 会顺带整个进程崩溃（不是"只死事件循环"）。这里 recover 转成
// WatcherError + error 日志。panic 意味着 ReloadFunc 没有交回状态，只记 WatcherError、
// 不把半截状态当一次重载尝试存进去。
func (w *ConfigWatcher) callReload() {
	w.reloadMu.Lock()
	defer w.reloadMu.Unlock()

	var (
		state    ReloadState
		err      error
		panicked bool
	)
	func() {
		defer func() {
			if r := recover(); r != nil {
				panicked = true
				err = fmt.Errorf("config reload panicked: %v", r)
				w.logger.Error("config reload panicked", "panic", r, "path", w.path)
			}
		}()
		state, err = w.onReload()
	}()

	if panicked {
		w.MarkWatcherError(err.Error())
		return
	}
	w.storeState(state, err)
}

// storeState 保存最新一份重载状态，遵守两条归属（卡要求 10）：
//   - WatcherError 只由 watcher 写：无论 ReloadFunc 交回的这份里它是什么，一律用已有的值覆盖过去，
//     这样"读端点看到的最后一个故障"不会被下一次成功重载抹掉；
//   - WatchedPath 由 watcher 拥有：无论交回的这份里它是什么，都改写成监听的绝对路径。
//
// ReloadFunc 返回错误时仍然保存它交回的状态（错误可能只写在 State.Error 里），
// 并额外记一条 error 日志（error、path 两个属性，卡要求 12）。
func (w *ConfigWatcher) storeState(state ReloadState, err error) {
	if prev := w.state.Load(); prev != nil {
		state.WatcherError = prev.WatcherError
	} else {
		state.WatcherError = ""
	}
	state.WatchedPath = w.path
	w.state.Store(&state)

	if err != nil {
		w.logger.Error("config reload returned error", "error", err, "path", w.path)
	}
}

// Close 停止监听并等待事件循环退出；幂等。
// 在途的那次 ReloadFunc 调用不强杀也不等待：它可能正握着写链的锁，强杀只会把状态弄得更乱，
// 而等它则会让一个卡住的重载把关停拖死——reloadMu 因此只排序调用、不参与关停。
// Close 会先停掉未触发的防抖计时器，再等事件循环返回（卡 §3 Close 注释、要求 11）。
// 事件循环自己的退出也做同样的两件事（Run 的 defer），所以 ctx 取消这一路不留计时器、不漏句柄。
func (w *ConfigWatcher) Close() error {
	w.stopOnce.Do(func() {
		// closed 先置真：事件循环据此把"事件通道关闭"当主动关停而非故障，
		// schedule/trigger 据此短路，timer.Stop 之后到点也不再调 ReloadFunc。
		w.closed.Store(true)
		close(w.stopCh)
		if w.watcher != nil {
			// 事件循环可能已经先关过（ctx 取消那条 defer）：v1.9.0 各后端重复 Close 返回 nil。
			if err := w.watcher.Close(); err != nil {
				w.logger.Error("failed to close config watcher", "error", err)
			}
		}
		// 清计时器：Stop 后从 map 删掉，Close 后 timers 为空。
		w.stopTimers()
	})
	return w.waitForLoop()
}

// waitForLoop 有界等待事件循环返回。Run 从未启动时直接返回——没有循环可等。
// 用 loopDone 通道而不是 runtime.NumGoroutine 差值判关停：同包其它测试会留下在途协程，
// 进程级计数在本机 -race -count=5 下会 ±1 抖动（卡 §10.2 第 1 条、D-R05 的取舍）。
func (w *ConfigWatcher) waitForLoop() error {
	if !w.started.Load() {
		return nil
	}
	timer := time.NewTimer(loopStopWait)
	defer timer.Stop()
	select {
	case <-w.loopDone:
		return nil
	case <-timer.C:
		return fmt.Errorf("config watcher event loop did not stop within %s", loopStopWait)
	}
}

// SetDebounce 运行期改防抖窗口（reload.debounce 的热更落点）。
// 非正数回 DefaultReloadDebounce。已在计时的这一次不改窗口——
// 新窗口从下一个事件起生效（schedule 每次读取当前窗口，见 schedule 注释），
// 这条不值得为它做重新计时。下界 minReloadDebounce 由 R01 的 Validate 拦，本卡不重复拦。
func (w *ConfigWatcher) SetDebounce(d time.Duration) {
	if d <= 0 {
		d = DefaultReloadDebounce
	}
	w.debounce.Store(int64(d))
}

// Debounce 返回当前窗口，给读数与测试用。
func (w *ConfigWatcher) Debounce() time.Duration {
	return time.Duration(w.debounce.Load())
}

// State 返回最近一次重载尝试的状态副本；从未尝试过时 Result 为空字符串。
// 一次 atomic.Pointer 换指针，另一个协程并发读不会拿到半成品（卡要求 9）。
//
// 副本是**浅拷贝**：AppliedKeys/IgnoredKeys/RejectedKeys 三个 []string 仍与 ReloadFunc
// 交回的那份共享底层数组。契约因此是：ReloadFunc 交回之后不得再改动它返回的那几份切片，
// 而读到 State() 的一方（R06 渲染 /admin/runtime 时）也不许就地排序或改写它们——要整理就复制一份。
func (w *ConfigWatcher) State() ReloadState {
	if p := w.state.Load(); p != nil {
		return *p
	}
	return ReloadState{}
}

// MarkWatcherError 把监听器自身的故障记进状态并记一条 error 日志。
// 导出是为了让装配方（R06）在 ReloadFunc 之外也能写这一个字段——
// 例如事件循环退出（watcher 死亡）这种"重载没跑但监听已经没了"的情形。
//
// 只改 WatcherError 一个字段：其余状态（含上一次重载的 Result）原样保留，
// 这样监听器故障不会顺手抹掉最近一次重载的结论。
func (w *ConfigWatcher) MarkWatcherError(msg string) {
	w.logger.Error("config watcher error", "error", msg, "path", w.path)

	cur := w.state.Load()
	var next ReloadState
	if cur != nil {
		next = *cur
	} else {
		next.WatchedPath = w.path
	}
	next.WatcherError = msg
	w.state.Store(&next)
}
