# TASK-R05　配置文件监听器：防抖、内容去重与状态

- 所属阶段：M1 落点
- 依赖任务：R01（`reload.*` 取值、`ReloadState` 类型）
- 涉及文件：`core/watch.go`（新增）、`core/watch_test.go`（新增）
- 预计规模：中

## 1. 任务目标

新增 `core.ConfigWatcher`：盯住启动时真正读到的那份 YAML，变化后在防抖窗口结束时调用一次回调，
并把这次尝试的结论记进 `ReloadState`。它**不认识任何业务键、不应用任何配置**——
"读回来、比一比、应用"归 R01 的 `Diff` 与 R06 的接线，本卡只管"什么时候该重载"和
"监听这件事本身有没有坏"。

## 2. 背景与当前问题

全仓现在的 `fsnotify` 用例只有一处：目录任务加载器（`core/load.go:499-569`）。
它的形状与本文需要的不一样，直接复用会踩三个坑：

| 加载器的做法 | 本文要的做法 | 为什么要分开 |
| --- | --- | --- |
| 按 `Pattern` 匹配文件名（`core/load.go:546`） | 只认**一个绝对路径** | 配置文件的名字是启动时定下的，模式匹配会连带盯上 `config.yaml.bak`、`config.yml~` 这类编辑备份 |
| 只处理 `Create` 与 `Write`（`:543-544`） | 不筛事件类型，任何落在目标文件上的事件都排一次防抖 | Windows 上编辑器存盘的事件形状不可预测（`Rename`、`Chmod`、多次 `Write`），筛事件会漏；"这一次到底有没有变"交给重载链（§3 的第 3 条），watcher 只管"该不该问一次" |
| 静默窗口 100ms（`loaderDebounceInterval`，`core/load.go:94`） | 窗口可配（`reload.debounce`，默认 500ms）且运行期可改 | 设计文档 §5.1 与待拍板 P3：配置改错的代价高于多等 400ms |
| 处理后把文件路径记进 `processedFiles`（`:271`、`:446`） | 与上一次**读到的内容**比，不与文件名比 | 任务文件是一次性输入，配置文件可以反复改回同一个内容 |

还有一个本卡独有的问题：**监听器自己坏了要能看见**。加载器把 watcher 错误直接记日志
（`core/load.go:559-564`）就完事；而配置文件监听器一挂，进程会无声停在旧配置上直到重启，
看起来一切正常。所以本卡把 watcher 侧故障记进 `ReloadState.WatcherError`
（设计文档 I3"沉默是 bug"）。

## 3. 要实现的功能

```go
// core/watch.go
package core

// ReloadFunc 是一次重载的执行体：读配置、比对、应用，成功返回 nil。
// 它由装配方提供（R06），watcher 只在防抖窗口结束时调它，并保存它返回的状态。
// 契约：实现方**必须是同步且自串行**的（watcher 的事件循环串行调用它，
// 但档位在线管理的写链可能同时在跑，所以串行要靠实现方自己的锁）。
type ReloadFunc func() (ReloadState, error)

// ConfigWatcher 盯一份配置文件，变化后按防抖窗口触发一次重载。
//
// 三条设计选择：
//  1. 监听**目录**而不是文件本身：编辑器改文件最常见的是"写临时文件 + rename 覆盖"，
//     直接盯文件会让 inode 换掉后收不到任何事件（Windows 上表现不同，盯目录同样是对的）。
//  2. 不筛事件类型：任何落在目标文件上的事件都排一次防抖，真不真要重载由内容决定。
//  3. 状态由本类型保存并暴露：重载结论由 ReloadFunc 交回，watcher 只负责存最新一份
//     与并发可读（一次 atomic 换指针，不加锁读）。
type ConfigWatcher struct {
        // 构造后不再变更
        path      string        // 目标文件的绝对路径
        dirname   string        // path 的目录，watcher 盯这里
        basename  string        // path 的文件名，用于比对事件
        onReload  ReloadFunc
        logger    *slog.Logger

        // debounce 可运行期改（reload.debounce 是热更档），所以是原子纳秒
        debounce atomic.Int64

        state   atomic.Pointer[ReloadState]
        watcher *fsnotify.Watcher
        mu      sync.Mutex // 保护 debounce 与"下一次重载"的计时状态
        timers  map[string]*time.Timer // 按目标文件路径合并短时间内的多次事件
        stopCh  chan struct{}
        wg      sync.WaitGroup
}

// NewConfigWatcher 建监听器。path 必须是启动时真正读到的那个文件的绝对路径
// （由调用方 filepath.Abs 之后再传）；不存在的路径返回错误——
// "盯着一个当前不存在的文件"意味着进程在用默认值，那种部署不该打开重载
// （设计文档 §5.2）。debounce<=0 用 DefaultReloadDebounce；onReload 为 nil 返回错误。
func NewConfigWatcher(path string, debounce time.Duration, onReload ReloadFunc, logger *slog.Logger) (*ConfigWatcher, error)

// Run 启动事件循环，直到 ctx 取消或 Close 被调用。它阻塞，调用方负责 go。
// ctx 与 stopCh 两个停止源都要支持：优雅关闭走 Close（同步，等事件循环与在途重载收尾），
// 而测试用 context.WithCancel 更方便。
func (w *ConfigWatcher) Run(ctx context.Context)

// Close 停止监听并等待事件循环退出；幂等。
// 在途的那次 ReloadFunc 调用不强杀：它可能正握着写链的锁，强杀只会把状态弄得更乱。
// Close 会先停掉未触发的防抖计时器，再等事件循环返回。
func (w *ConfigWatcher) Close() error

// SetDebounce 运行期改防抖窗口（reload.debounce 的热更落点）。
// 非正数回 DefaultReloadDebounce。已在计时的这一次不改窗口——
// 新窗口从下一个事件起生效，这条不值得为它做重新计时。
func (w *ConfigWatcher) SetDebounce(d time.Duration)

// Debounce 返回当前窗口，给读数与测试用。
func (w *ConfigWatcher) Debounce() time.Duration

// State 返回最近一次重载尝试的状态副本；从未尝试过时 Result 为空字符串。
func (w *ConfigWatcher) State() ReloadState

// MarkWatcherError 把监听器自身的故障记进状态并记一条 error 日志。
// 导出是为了让装配方（R06）在 ReloadFunc 之外也能写这一个字段——
// 例如事件循环退出（watcher 死亡）这种"重载没跑但监听已经没了"的情形。
func (w *ConfigWatcher) MarkWatcherError(msg string)
```

行为要求（每条都要有用例）：

1. **触发**：`os.WriteFile(path, ...)` 后在一个窗口内收到一次 `onReload` 调用。
2. **合并**：连续 5 次快速写入只调一次。
3. **内容不变也要调**：本卡**不**做内容哈希比较。理由写进注释——判"内容有没有变"需要
   知道上一次真正生效的那份配置，而那份权威在 `applied`（R06 的接线方）手里；
   watcher 只比较"事件"，不比较"配置"。等价内容导致的那次 `onReload` 会自己返回
   `ReloadUnchanged`，代价是一次读文件，不值得让两个类型共享 `applied` 这个概念。
4. **只认目标文件**：同目录里另一个文件（`other.yaml`）变化不触发；
   `config.yaml.bak` 也不触发（比对 `basename` 用精确相等，不用 `filepath.Match`）。
5. **rename 覆盖**：先写 `path.tmp` 再 `os.Rename(tmp, path)` 必须触发一次，
   并且触发后 watcher 仍能收到后续事件（盯目录天然满足，这条用例就是盯目录这个选择的证据）。
6. **文件被删**：`Remove(path)` 触发一次 `onReload`，由 ReloadFunc 决定怎么记
   （R06 把它记成失败，绝不退回默认值——设计文档 §8 的倒数第二条）。
7. **不筛事件类型**：`Chmod` 也触发（Windows 上可能收不到 `Chmod`，用例只断言"写与 rename 必触发"，
   `Chmod` 那条用 `t.Skip` 说明平台差异，别写成假断言）。
8. **Close 幂等**：连调两次返回 nil；`Run` 返回；关闭后 `WriteFile` 不再触发任何调用。
9. **状态可读**：`onReload` 返回的 `ReloadState` 被存下来（含 `Result`/`Error`/三份键清单/
   `LastAttemptAt`/`LastAppliedAt`）；另一个协程同时读 `State()` 不会拿到半成品
   （一次 `atomic.Pointer` 换指针）。
10. **WatcherError 不被覆盖**：`onReload` 交回的状态里 `WatcherError` 一律留空
    （它只由 watcher 写），watcher 保存新状态时把已有的 `WatcherError` 原样带过去。
    这是"读端点看到的最后一个故障"不被下一次成功重载抹掉的前提。
11. **不吞 goroutine**：`Run` 返回后不再有计时器回调进入 `onReload`。
12. **ReloadFunc 返回错误时**：仍然保存它交回的状态（错误可能只写在 `State.Error` 里），
    并额外记一条 error 日志（`error`、`path` 两个属性）。
13. **ReloadFunc panic**：不能打挂事件循环（§9 风险表第一条处置），转成 `WatcherError` + error 日志，
    循环继续。

防抖实现照 `core/load.go` 里 `scheduleLoad` 的形状（`pendingLoads map[string]*time.Timer`，
同路径的旧计时器 `Stop` 后重建），不另发明一套。

## 4. 实现步骤

1. 新增 `core/watch.go`：类型 → `New*` → `Run` → 事件处理（`handleEvent`）→ `schedule` →
   `trigger`（调 ReloadFunc 并存状态）→ `Close` → `SetDebounce`/`Debounce`/`State`/`MarkWatcherError`。
   小节顺序按依赖排，注释体例照 `core/load.go:499-569` 与 `core/executor_profile_store.go`。
2. 事件循环里对 `watcher.Events` 与 `watcher.Errors` 两个通道的 `!ok`（已关闭）分别处理：
   前者退出循环并 `MarkWatcherError("config watcher event channel closed")`，
   后者把错误记进状态与日志但**不退出循环**（fsnotify 的 error 通道关掉不等于 watcher 死亡）。
3. `go build ./... && go vet ./...`，再写 §5 的用例。
4. 只对新增文件 `gofmt -w`。

## 5. 测试要求

统一 helper：

```go
// newWatcherFixture 在 t.TempDir() 里造一个真实的 config.yaml，
// 建 watcher 并 go Run(ctx)；返回 fixture 与一个"收到一次重载"的通道。
// onReload 记录调用次数并把一个可预设的 ReloadState 交回。
func newWatcherFixture(t *testing.T, debounce time.Duration) *watcherFixture

// mustWrite 写目标文件；mustTouch 写同目录的无关文件。
// waitFor 在超时内等通道收到调用（10ms 轮询），超时即 t.Fatal 并打印已收到的次数。
func (f *watcherFixture) waitFor(t *testing.T, want int, timeout time.Duration)
```

用例：

1. `TestConfigWatcher_TriggersOnWrite`（要求 1）。
2. `TestConfigWatcher_CoalescesBurstWrites`（要求 2）：debounce 设 200ms，
   立刻连写 5 次，`waitFor(…, 1, 2s)` 且再等一个窗口后仍是 1。
3. `TestConfigWatcher_IgnoresOtherFiles`（要求 4）：写 `other.yaml` 与 `config.yaml.bak`，
   等两个窗口，断言调用次数为 0。**反向验证**：把实现里的文件名比对临时改成前缀匹配，
   这条必须变红（在 §10.3 里记下这一步的实际输出）。
4. `TestConfigWatcher_TriggersOnRenameReplace`（要求 5）：写 tmp → rename → 断言触发一次；
   再直接 `WriteFile` 一次，断言共两次（盯目录在 rename 之后仍然有效）。
5. `TestConfigWatcher_TriggersOnRemove`（要求 6）。
6. `TestConfigWatcher_StoresStateFromReload`（要求 9、10）：
   预设 `ReloadState{Result: ReloadRejected, RejectedKeys: ["server.auth.token"]}` 且 error 非 nil，
   写文件 → 断言 `State().Result=="rejected"`、`RejectedKeys` 原样、`LastAttemptAt` 非零。
7. `TestConfigWatcher_SetDebounce`：改成 50ms 后，`Debounce()` 立刻回新值，
   且连写 3 次（间隔 20ms）在旧窗口 1s 下只应合并成一次、在新窗口 50ms 下应合并成 ≤2 次
   ——判据写成"新窗口下的调用次数 < 旧窗口下的调用次数"以免 flake；
   `SetDebounce(0)` 回 `DefaultReloadDebounce`、`SetDebounce(-1)` 同样回默认。
8. `TestConfigWatcher_CloseIdempotent`（要求 8）：`Close()` 两次 nil、`Run` 已返回
   （用 `wg` 或一个 done 通道断言，不要靠 sleep）、关闭后再写文件不触发。
9. `TestConfigWatcher_WatcherErrorVisible`（要求 10）：同包测试可以直接把 watcher 的
   `fsnotify.Watcher` 关掉（`w.watcher.Close()` 之后事件通道即关闭）来复现"事件通道已关闭"，
   断言 `State().WatcherError` 非空、日志里有对应 error 记录
   （用 `slog.New(slog.NewTextHandler(&buf, nil))` 捕获），并再写一次文件让状态被换新值，
   断言 `WatcherError` 仍在（要求 10 的后半句——它不该被下一次成功重载抹掉）。
   若本机两种做法都不稳定，允许改成只测 `MarkWatcherError` 这条路，
   并在 §10.6 未覆盖项里写明"事件通道关闭的真实故障路径未实测"。
10. `TestConfigWatcher_PanicDoesNotKillLoop`（要求 13）：`onReload` 里 `panic("boom")`，
    断言事件循环没死——`Run` 仍在跑（用 done 通道判"未返回"）、`State().WatcherError` 非空、
    再写一次文件时 `WatcherError` 被保留下来。这条对应 §9 风险表的第三行。
11. `TestConfigWatcher_RejectsMissingPath`：`NewConfigWatcher` 对一个不存在的文件返回错误；
    相对路径、空路径、`onReload` 为 nil 三种都返回错误（构造只收绝对路径，把判断前置到构造期）。
12. `TestConfigWatcher_ConcurrentStateReads`（要求 9）：一个协程连续写文件触发重载，
    两个协程各读 `State()` 200 轮，`-race` 必须干净。

## 6. 完成标准（DoD）

- [x] 十三条行为要求各有用例（§3 的编号 1…13）（要求 7 允许平台受限，但要留一条明确的 `t.Skip` 而不是删掉）。
      要求 3 在首版只写成口头论证、返工轮补了 `TriggersOnIdenticalContent`（§10.5 D-R0513）。
- [x] 本卡不调 `LoadConfig`、不 import `viper`、不认识任何业务键：
      `core/watch.go` 里 `grep -n "LoadConfig\|Diff\|Executors\|Logging"` 无命中
      （返工轮终态复跑，仍无命中）。
- [x] `Close` 之后没有任何 goroutine 残留：**按 §10.2 第 1 条换成结构化判据**（卡面原写的
      `runtime.NumGoroutine()` 差值 + `core/scheduler_exec_class_test.go:430` 的引用是反的，本机实测会 ±1 抖动）：
      `loopDone` 已关闭 + `timers` 为空 + 关闭后写文件不再触发 + 直接调 `trigger()` 也不新起，四条在
      `CloseIdempotent`/`CloseStopsTimersWithoutLoop`/`CtxCancelStopsScheduling` 三条用例里分工作证。
- [x] 未打开 `reload.enabled` 时本类型完全没有被构造的机会（本卡不改 `cmd/server`）：
      全仓 `grep -rn ConfigWatcher --include=*.go` 只命中定义、用例与 `core/config_reload.go` 的一句注释。
- [x] `go test ./core -race -count=5 -timeout 30m` 无 flake（返工轮终局读数 102.682s，见 §10.3 步骤 3）。
- [x] `go build ./...`、`go vet ./...`、`go test ./... -race -count=1` 全绿；新增文件已 `gofmt -w`（§10.3 步骤 1/4/5）。

## 7. 验收方式

```bash
go test ./core -run 'TestConfigWatcher' -v
go test ./core -race -count=5 -timeout 30m
go build ./... && go vet ./...
go test ./... -race -count=1
```

预期：第一条现在列出 **20 个 Test 函数**（19 PASS + 1 SKIP，SKIP 是要求 7 的 Chmod，原因写在 §10.4），
其中 §5 原列的 12 条之外多出的八条是返工轮补的（见 §10.1 的表与 §10.5 的缺陷行）。
真实输出与逐项读数记在 §10.3。

## 8. 不在本任务范围

- 不做 `SIGHUP`/`SIGUSR1` 触发（R2 已排除，且 Windows 上没有 SIGHUP）。
- 不做手动触发的 REST 端点（设计文档 §10 的 N5）。
- 不调 `LoadConfig`、不做 `Diff`、不应用任何 setter（R06）。
- 不做内容哈希缓存（§3 的第 3 条里已经说明为什么不由 watcher 判等价）。
- 不改目录任务加载器的防抖或事件筛选（待拍板 P3、设计文档 §10 的 N7）。
- 不处理"配置目录本身被删"这种部署级故障（超出可恢复范围，交给 ReloadFunc 报错）。

## 9. 风险与回滚

| 风险 | 说明 | 处置 |
| --- | --- | --- |
| 盯文件而不是盯目录 | rename 覆盖后静默失联，看起来"重载不工作" | §3 第 1 条 + §5.4 那条 rename 用例（触发后还要能收第二次事件） |
| 事件筛选过窄 | Windows 上某些编辑器不发 `Write`，漏触发 | 不筛类型，靠文件名 + 防抖；§5.7 守窗口可配 |
| ReloadFunc panic 打挂监听器 | 一次坏配置让监听器死亡，此后所有改动无声 | **站点纠正（见 §10.5 D-R0512）**：ReloadFunc 不在事件循环上跑，它由防抖计时器的协程调用（`time.AfterFunc` 的回调 → `trigger` → `callReload`），所以那一层 recover 必须挂在 `callReload` 内而不是事件循环里——挂在循环上捕获不到，未捕获的 panic 会**打挂整个进程**（§10.3 变异 5 的栈就是 `created by time.goFunc`）。处置：`callReload` 内 recover 转成 `WatcherError` + error 日志，用例断言循环仍在跑且 `WatcherError` 非空 |
| 两次重载并发跑 | 触发者是计时器协程，前一次还在跑时后一个窗口已到期；两条链交叉写配置、后发起的那次先返回反而被前一次的旧结果覆盖 | `callReload` 持 `reloadMu` 把"调用 + 保存"整段串行（§10.5 D-R0504）；用例 `ReloadCallsAreSerialized` 判"同时在跑的重载数峰值 == 1" |
| `Run` 的 ctx 退出路径不收口 | 只按卡面在 `Close` 里停计时器：ctx 取消后 `Run` 返回了，未触发的防抖计时器仍会打进 `ReloadFunc`，fsnotify 句柄也一直漏着——正是要求 11 反着写的形状，而测试台全部走 ctx 停 | `Run` 用 defer 做与 `Close` 同样的两件事（关句柄、停计时器），见 §10.5 D-R0506；用例 `CtxCancelStopsScheduling` |
| 防抖计时器"谁清的"分不出来 | `Close` 与 `Run` 共用同一个 `stopTimers` 后，"Close 忘停表"的变异会被 `Run` 的 defer 兜住、在 `CloseIdempotent` 上绿灯（§10.5 D-R0505） | 补一条不启动 `Run` 的用例 `CloseStopsTimersWithoutLoop`：排期只能来自测试自己的一次 `schedule`，`Close` 之后 `timers` 必须为空 |
| `basename` 用入参拼写 | Windows/macOS 默认卷大小写不敏感：`-config ...\Config.yaml` 能 `os.Stat` 成功、watcher 建得出来、`State()` 一切正常，但 fsnotify 上报磁盘拼写 `config.yaml`，精确比对永不命中——监听器再也不触发，且没有任何地方报错 | 构造期用 `resolveDiskFileName` 取目录里的真实拼写（§10.5 D-R0507）；用例 `ResolveDiskFileName`（任何平台都走折叠匹配那一路）+ `MatchesOnDiskNameCasing`（端到端） |
| 状态的两个写方互相抹字段 | `storeState`（重载结果）与 `MarkWatcherError`（监听器健康）都是"读快照—改—整份换指针"，被插进去一次就把对方那一半写回旧值；`reloadMu` 挡不住事件循环那一头（它不取那把锁）。 | 两边的读—改—写都进 `stateMu`（只锁几次赋值，绝不包住 `onReload`，所以慢重载拖不住关停）；顺序判据 M4/ME，并发那半边如实记在 §10.5 D-R0516/D-R0520 |
| `closed` 只由 `Close` 置真 | ctx 取消那条 `Run` 退出路径上 `trigger` 的短路永不生效，而契约注释写的是"Run/Close 返回之后不再新起一次调用"——那句话当时是假的。 | `Run` 的 defer 先置 `closed` 再关句柄、停表、关 `loopDone`；判红点是 `CtxCancelStopsScheduling` 末尾那次直接调 `trigger`（变异 MB） |
| 已到期的防抖计时器被裸 `Reset` 留在表里 | 表项还在、计时器已经跑过：`Reset` 返回假但代码不看它，于是这一路排期永远不会打进 `ReloadFunc`（监听器还在、事件也认得，就是不再重载），同时旧回调与新表项还能凑出第二次触发 + 一条停不住的孤儿。 | `Reset` 返回假就 `Stop` 掉旧的并 `arm` 一条新的；回调按**序号**认领（`debounceSlot.gen`），认不到就不触发也不删别人的表项；判红点 M8 之外新增 MC、MD 两条（§10.5 D-R0518） |
| 防抖窗口过短 | 每次存盘读好几遍配置、反复触发写链 | `reload.debounce` 的下界（50ms）在 R01 的 `Validate` 里拦，本卡不重复拦 |
| 计时器泄漏 | 每个路径一个 `time.Timer`，`Stop` 后重建才有界；关停不清 timers 会留待触发的闭包 | 抽成 `stopTimers`（照 `core/load.go` 的 `stopLoaders` 写法），`Close` 与 `Run` 的每条退出路径都调它；证据是 `CloseStopsTimersWithoutLoop` + `CtxCancelStopsScheduling` 两条用例判 `timers` 为空（§6 第三条的 goroutine 差值判据已被 §10.2 第 1 条换掉，它不是证据） |
| 用例靠 sleep 判定 | flake，尤其 debounce 50ms 在 CI 机器上 | 一律"10ms 轮询 + 上界断言"，判据写成"次数 <"或"次数 =="而不用时间等式 |

回滚：本卡纯新增两个文件（`core/watch.go`、`core/watch_test.go`），没有任何调用方，
`git revert` 单提交即可，全仓行为零变化。

## 10. 实现记录（执行时补写）

### 10.1 落地的接口

`core/watch.go`（新增，纯新增无调用方）落地的符号（按符号名，不给行号）：

- 类型：`ReloadFunc`、`ConfigWatcher`，以及非导出的 `debounceSlot`（防抖表里的一项：一条计时器 + 身份序号）。
- 构造函数：`NewConfigWatcher(path, debounce, onReload, logger)`。
- 方法：`Run(ctx)`、`Close()`、`SetDebounce(d)`、`Debounce()`、`State()`、`MarkWatcherError(msg)`。
- 内部方法（非导出）：`handleEvent`、`schedule`、`arm`、`claim`、`stopTimers`、`trigger`、`callReload`、
  `storeState`、`waitForLoop`、`resolveDiskFileName`（包级函数，构造期解析磁盘拼写）。
  首版的 `forget` 在第三轮被 `claim` 取代（认领到才是自己的那次到期，见 D-R0518）。
- 结构体字段：`path`、`dirname`、`basename`（磁盘拼写，构造期解析）、`onReload`、`logger`、
  `debounce`(`atomic.Int64`)、`state`(`atomic.Pointer[ReloadState]`)、`closed`(`atomic.Bool`)、
  `started`(`atomic.Bool`)、`watcher`(`*fsnotify.Watcher`)、`mu`、`timers`(`map[string]*debounceSlot`)、`seq`(uint64)、
  `reloadMu`（串行化 ReloadFunc 调用与状态保存）、`stateMu`（串行化两个写方的读—改—换指针）、`stopCh`、`loopDone`、`stopOnce`。
  `resolveDiskFileName` 的返回值是 `(basename, listed, error)`：目录列不出来时回退成入参拼写并让调用方记 warn。
- 包级常量：`loopStopWait`（Close 等事件循环返回的上界，5s）。

`core/watch_test.go`（新增）落地 §5 的 12 个用例，第二轮补 7 条、第三轮补 4 条（都登记在 §10.5），
共 **24 个 Test 函数**（23 PASS + 一条 §3 要求 7 的 Chmod 用例本机 SKIP）。统一 helper：`watcherFixture`、`newWatcherFixture`、
`mustWrite`、`mustTouch`、`waitFor`、`expectStays`、`waitForWatcherError`、`waitForLog`、
`assertLoopRunning`、`waitForPendingTimer`、`waitLoopReturned`、`runAndCheckImmediate`、`syncBuf`。
fixture 的 `hold`/`inFlight`/`maxInflight` 三个字段只服务 `ReloadCallsAreSerialized` 一条用例。

第二轮（评审返工）新增的七条用例与它们各自守的东西：

| 用例 | 守的判据 |
| --- | --- |
| `TestConfigWatcher_CtxCancelStopsScheduling` | 要求 11 的 ctx 那一半：取消后 `timers` 为空、fsnotify 句柄已关（判据是对同目录再 `Add` 拿错误）、`Run` 返回之后调用次数不再增加 |
| `TestConfigWatcher_CloseStopsTimersWithoutLoop` | `Close` **自己**停表：整场不启动 `Run`，没有别人的 defer 替它兜底（§10.5 D-R0505） |
| `TestConfigWatcher_ReloadCallsAreSerialized` | ReloadFunc 契约第 1 条：同时在跑的重载数峰值 == 1 |
| `TestConfigWatcher_SecondRunDoesNotPanic` | 一个 watcher 只跑一个事件循环；第二次 `Run`（含 `Close` 之后）立即返回 |
| `TestConfigWatcher_ResolveDiskFileName` | 折叠匹配那一路在任何平台都被走到（目录里写 `config.yaml`、入参给 `CONFIG.yaml`），且找不到时报错而不是静默回退成入参拼写 |
| `TestConfigWatcher_MatchesOnDiskNameCasing` | 端到端：大小写错配的 `-config` 路径仍真触发（本机 Windows 构造得出来；大小写敏感的卷上 `t.Skipf` 说明由上一条覆盖） |
| `TestConfigWatcher_TriggersOnIdenticalContent` | 要求 3：两次同字节写入（间隔 150ms > 窗口 100ms）就是两次调用；谁日后加内容哈希缓存，这条判红 |

第三轮（第二次评审返工）新增的四条：

| 用例 | 守的判据 |
| --- | --- |
| `TestConfigWatcher_StateWritesDoNotLoseEachOther` | 两个写方并发时的合并完整性（要求 10 的并发那一半）。**这条只能当回归网**：本机把 `stateMu` 整个删掉跑 5000 轮 × 5 次都没撞到丢更新，判红点落在下面那条顺序用例上（D-R0520） |
| `TestConfigWatcher_MarkWatcherErrorKeepsLastReload` | `MarkWatcherError` 只许改 `WatcherError` 一个字段：上一次重载的 `Result`/`Error`/三份键清单/两个时间/`Enabled` 逐个还在（判红点是变异 ME） |
| `TestConfigWatcher_ReplacesExpiredDebounceTimer` | 表里那一项已到期时 `schedule` 必须换代：判据是"这次排期真的打进了一次重载"。留在表里的死表项会让这条路径**从此永不再重载**（变异 MC） |
| `TestConfigWatcher_OldFiredCallbackDoesNotStealTheNewSlot` | 旧到期回调不许抢新表项：既不再触发一次重载，也不许把新那一项从表里删掉（变异 MD 两条一起红） |

同轮还给 `CtxCancelStopsScheduling` 重排了断言次序并加了第五条（紧接循环返回查 `timers`、
直接调 `trigger` 验短路、且都排在 `Close` 之前），理由与实测到的遮蔽形状记在 §10.5 D-R0519。

另外两处判据强化（不是新用例）：`StoresStateFromReload` 改成**逐字段**断言交回状态被原样存下
（`Error`/`AppliedKeys`/`IgnoredKeys`/`LastAppliedAt`/`Enabled` 都要，见 §10.5 D-R0509），
`IgnoresOtherFiles` 末尾加一次**正向对照**（同一次运行里随后写目标文件必须正好触发一次，见 D-R0511）。

§5.3 反向验证证据（把 `handleEvent` 的 basename 精确相等改成前缀匹配后的实际输出）：

```
MUTANT 2: match basename by PREFIX instead of exact equality
target test: TestConfigWatcher_IgnoresOtherFiles
=== RUN   TestConfigWatcher_IgnoresOtherFiles
    watch_test.go:223: 期望调用次数不超过 0，但窗口内已达到 1 次
--- FAIL: TestConfigWatcher_IgnoresOtherFiles (0.11s)
FAIL	godelayq/core	0.283s
```

即：前缀匹配让同目录的 `config.yaml.bak`（前缀就是 `config.yaml`）被当成目标文件、触发了一次重载，
用例立刻判红——证明"用精确相等而不是 `filepath.Match`/前缀"这条判据是被用例守住的。
（这条输出取自首轮；同一变异在两轮返工后的新字节上重跑仍然判红，见 §10.3 的十九变异表。）

### 10.2 与本卡写法的差异

首轮落地有两处卡面前提是错的（下面第 1、2 条）；评审返工轮又发现一处（第 8 条：`ReloadFunc` 的契约注释本身写反了），
并把要求 11 的 ctx 那一半、磁盘大小写拼写、第二次 `Run`、状态逐字段透传、`State()` 的浅拷贝契约、
`trigger` 的关停短路这六组补成实现 + 判据（第 9–14 条）。

1. **§6 DoD 第三条（用 `runtime.NumGoroutine()` 差值证明 Close 后无协程残留）的引用是反的。**
   它援引 `core/scheduler_exec_class_test.go:430` 作"既有写法"，但那段注释（`TestExecClass_DisabledCreatesNoPool`
   之上）说的是相反的事：那张卡允许两种形状，仓库**恰恰因为**同包测试会留下在途协程、
   进程级计数在本机 `-race -count=5` 下 ±1 抖动（真失败过："应当多出 3 个，实际多出 2 个"）而**选了另一种**。
   R03 也踩过同一个坑。故本卡判"协程清理"用**结构化判据**，在 `TestConfigWatcher_CloseIdempotent` 里连做三件事：
   (a) 事件循环退出时 `defer close(w.loopDone)`，测试断言 `loopDone` 已关闭；
   (b) 断言 Close 后 `timers` map 为空；
   (c) 断言关闭后再写文件不产生任何新的 `onReload` 调用。
   判据不靠 sleep：Close 用 `waitForLoop` 有界等待 `loopDone`（上界 5s），测试用轮询/`select` 判退出。
   没有采用进程级 `NumGoroutine` 差值断言，正是为了避免上面这个已被实测证伪的抖动。
   返工轮又给它加了第 (d) 件——`Close` 返回后直接调 `w.trigger()`，仍不得新起一次调用（见下面第 14 条），
   并因第 9 条的改动把 (b) 的判红点分了一半给新用例 `CloseStopsTimersWithoutLoop`（D-R0505）。

2. **§5.7 的 `SetDebounce` 判据（"新窗口下的调用次数 < 旧窗口下的调用次数"）是两次不同时间运行的比较，
   Windows 上易 flake**，且 §9 风险表最后一行也警告不要用时间等式。改成的稳健形状：
   `Debounce()` 在 `SetDebounce(50ms)` 后**立刻**返回新值（确定性）；合并效果用**上界**断言——
   长窗口（1s）下 3 次间隔 20ms 的写入确定合并成"恰好 1 次"（跨度 40ms ≪ 1s，稳定）；
   短窗口（50ms）下 5 次间隔 10ms 的写入产生的调用增量落在 `[1,4]`（严格少于写入次数 5，且至少 1）。
   另加 `SetDebounce(0)`/`SetDebounce(-1)` 都回 `DefaultReloadDebounce` 的确定性断言。

其余实现级差异（都往"更守"的方向）：

3. 结构体形状相对 §3 草图把 `wg sync.WaitGroup` 换成 `loopDone`/`started` 两个位点：草图里
   `Run` 由调用方 `go` 起，若在 `Run` 协程内 `wg.Add(1)` 会与 `Close` 的 `wg.Wait` 竞态（Go 明确禁止的用法）。
   改用"循环退出即 close(loopDone)、Close 有界等它"，另用 `started` 区分"从没跑过 Run"（此时无需等待，直接返回）。
4. `Close` 用 `stopOnce` 保证 `close(stopCh)` 幂等（草图只说"幂等"，没写怎么幂等），并在置 `closed` 之后
   照 `core/load.go` 的 `stopLoaders` 逐个 `timer.Stop()` 后从 map 删除。
5. `storeState` 把 `WatchedPath` 一律改写成监听路径（watcher 拥有它），并按要求 10 把已有 `WatcherError` 原样带过去；
   ReloadFunc 交回的 `Enabled` 等业务字段本卡不解释、原样保存（归 R06）。
6. §4.2 要求"error 通道关闭不退出循环"，但直接对已关闭通道 `continue` 会空转。实现把该 case 的通道局部变量置 `nil`
   来停用这一路、继续监听 events，语义不变（不退出、不 busy-loop）。
7. 事件循环退出（events 通道在 `closed` 仍为假时 `!ok`）与 ReloadFunc panic 都走 `MarkWatcherError` + error 日志；
   主动关停（`closed` 已真）时 events 通道关闭被识别为正常退出，不误记故障。

返工轮（评审后）新增的六条，其中第 8 条同样是卡面前提写错：

8. **§3 的 `ReloadFunc` 契约注释"watcher 的事件循环串行调用它"是错的。** 调用点是防抖计时器的协程
   （`time.AfterFunc` 的回调 → `trigger` → `callReload`），不是事件循环；两个窗口的到期可以重叠
   （前一次还在跑、后一次已重新计时并到期），所以"至多一个在跑"这件事 watcher 自己必须保证。
   落地：`callReload` 持 `reloadMu` 把"调用 + `storeState`"整段串行。契约因此改成两条
   （1 由 watcher 串行；2 实现方仍要自己与进程内其它写入口互斥，`reloadMu` 管不到档位在线管理的写链）。
9. **要求 11 的两条退出路径都要收口。** 卡 §3 只在 `Close` 的注释里写"先停掉未触发的防抖计时器"，
   但 `Run` 有 ctx 取消与 `stopCh` 两条返回路径，而测试台与"只用 ctx 停"的调用方都走前者。
   落地：停表逻辑抽成 `stopTimers`，`Close` 与 `Run` 的 defer 共用；`Run` 的 defer 同时关 fsnotify 句柄，
   否则只用 ctx 停的调用方会把目录句柄一直漏到有人调 `Close`。
10. **卡 §3 的两条注释自相矛盾，取了 `Close` 那条。** `Run` 的注释说优雅关闭"等事件循环与在途重载收尾"，
    `Close` 的注释说在途那次 `ReloadFunc` 调用"不强杀"。实现取后者，并且**也不等它**：
    它可能正握着写链的锁，等它只会把关停拖死。代价写进 `ReloadFunc` 契约第 1 条末尾——
    调用可能在 `Run`/`Close` 返回之后才结束，实现方不能假设调用点被它们收住；
    watcher 保证的是另一头（`closed` 位点让返回之后不再**新起**一次调用）。
11. **`basename` 用磁盘上的真实拼写，不用入参里那一段。** 卡 §3 的草图注释写的是"path 的文件名"。
    Windows/macOS 默认卷大小写不敏感，`-config ...\Config.yaml` 会 `os.Stat` 成功、watcher 建得出来，
    但 fsnotify 上报磁盘拼写，精确比对永不命中，于是"监听器再也不触发而没有任何地方报错"。
    构造期用 `resolveDiskFileName`（先精确相等、再 `strings.EqualFold` 兜一次）解析；
    解析不出来当作部署级问题直接返回构造错误，不静默回退成入参拼写。
12. **同一个 watcher 只跑一个事件循环**：`Run` 用 `started.CompareAndSwap` 认第一个进入者，后进者（含 `Close` 之后再调）
    立即返回。卡没写这条；草图里的 `wg` 形状下第二次 `Run` 会在退出时 `close` 已关的通道而 panic（§10.5 D-R0508）。
    选"静默返回"而不是"panic 报错"：这个类型的调用形状是 `go w.Run(ctx)`，在协程里 panic 等于打挂整个进程。
13. **`State()` 是浅拷贝**：`AppliedKeys`/`IgnoredKeys`/`RejectedKeys` 三份切片仍与 `ReloadFunc` 交回的那份共享底层数组。
    契约写进方法注释——交回之后不得再改那几份切片，读到的那一方（R06 渲染 `/admin/runtime`）也不许就地排序或改写。
14. **`Close` 之后由 `trigger` 的 `closed` 短路兜底**，而不是靠"计时器已停所以回调不会来"：
    `timer.Stop()` 对已到点的计时器无效，且"回调刚好在 `closed` 置真的前一瞬通过检查"这个残余竞态窗口在本卡消不掉。
    它保证的是"不再新起"，不是"绝不重叠"；这条写进 `trigger` 注释，用例
    `TestConfigWatcher_CloseIdempotent` 在 `Close` 返回后直接调 `trigger()` 验短路本身（缺陷登记见 §10.5 D-R0510）。

第三轮（第二次评审返工）新增的六条：

15. **防抖表换成 `debounceSlot`，`forget` 换成 `claim`。** 卡 §3 那句"照 `core/load.go` 的
    `pendingLoads`（同路径的旧计时器 `Stop` 后重建）"在实现里被写成了裸 `timer.Reset(d)` 并直接 `return`
    ——`core/load.go:195-197` 那份也是裸 `Reset`，所以照抄反而把它的缺口一起抄进来：`Reset` 返回假时
    那条计时器**已经跑过**，表项却还留着，此后再多的事件都只是原地给一条死计时器续期，这条路径永不再重载。
    落地改成：返回假就 `Stop` 掉旧的（`Reset` 会把它重新 arm 一次，不 Stop 就白跑一趟回调）并 `arm` 新的。
    身份为什么用序号不用 `*time.Timer` 指针：回调要认"我自己那一条"，而
    `t = time.AfterFunc(d, f)` 是**先建计时器、后把返回值赋给 t**，f 里读到的 `t` 与那次赋值之间
    按 Go 内存模型不构成 happens-before（窗口极窄但形式上是数据竞争，`-race` 也测不出来）。
    序号在建计时器之前算好、按值闭包进去，就没有这个窗口。这是我自己在落地第二版时发现并改掉的，
    登记为 D-R0526。
16. **新增 `stateMu`（结构体草图里没有第三把锁）。** `storeState` 与 `MarkWatcherError` 都是
    "读当前快照—改两个字段—整份换指针"，两头并发时后写的那份会带着自己读到的旧快照把对方那一半抹掉。
    实现取互斥锁而不是 `CompareAndSwap` 重试：CAS 只保证"不覆盖更新的指针"，而这里要的是**两边写的字段都在**——
    `storeState` 手里那份候选值来自更早的 `onReload`，即便 CAS 成功也会把 `WatcherError` 写成过时的值。
    锁只圈住几次赋值，绝不圈 `onReload`，所以慢重载不会挡住事件循环记故障、也不会拖住 `Close` 的 5s 上界。
17. **`closed` 现在由三条路置真**（`Close`、`Run` 因 ctx 取消返回、`Run` 因 `stopCh` 返回），
    defer 次序固定为"置 closed → 关句柄 → 停表 → 关 `loopDone`"。`ReloadFunc` 契约里那句
    "让 `Run`/`Close` 返回之后不再新起一次调用"到这一轮才真的成立；第二轮它只对 `Close` 成立。
18. **`resolveDiskFileName` 返回 `(basename, listed, error)`。** 第二轮它对"目录列不出来"直接报构造错误，
    等于把 0711 目录、Windows "可读不可列" ACL 这类以前完全能起 watcher 的部署判成部署级故障；
    现在回退成入参拼写 + 一条 warn（就是本卡之前的行为），只有"列得出、但没有匹配项"才是错误。
    同一轮把"只做大小写折叠、不做 Unicode 规范化"（macOS/APFS 的 NFD 名会落到"没有匹配项"那一支）
    写进函数注释，不猜规范化形式。
19. **`CtxCancelStopsScheduling` 的窗口从 200ms 改成 1s，且断言次序成了用例的一部分**：
    `timers` 必须**紧接循环返回**就查（拖到静默窗口之后，`claim` 会让到期回调自己把表项摘掉，
    于是"忘了停表"的变异变绿），直接调 `trigger` 必须排在 `Close` 之前（`Close` 自己会置 `closed`，
    那就分不出是谁置的）。这是 D-R0505 那条教训的第二形态，登记为 D-R0519。
20. **删掉两处永不成立的 `w.watcher != nil` 守卫**（`Run` 的 defer 与 `Close`）：构造函数在建不出
    fsnotify 句柄时直接返回错误，字段恒非空，留着反而暗示存在"没有句柄的 watcher"这种形态。

### 10.3 验证证据（含 §5.3 的反向验证输出）

统一工作目录 `D:\codeproject\mygo\godelayq`，本机 Windows。以下均为真实输出。

**步骤 1 — 只格式化新增两个文件**（仓库里既有文件带 CRLF，全局 `gofmt -l` 会误报，故只列这两份）：

```
gofmt -w core/watch.go core/watch_test.go
gofmt -l core/watch.go core/watch_test.go     # 无输出：两份文件已 gofmt 干净
```

返工轮改完再复核一次，`gofmt -l` 仍然无输出；本轮全部验证结束时的两份文件指纹（也是提交进去的那份）：

```
b8cd8405af4e2654b64f0bdd72068878f2e269f24f06aaf2873c6b5b5fb7f330  core/watch.go (571 行)
72d99800bf0e2a26a69e3ae06e6576860f57faeb07758a100e84a1d6cd2abd74  core/watch_test.go (1180 行)
```

第二轮那版的指纹是 `342463c8…` / `4a28a629…`；第三轮改了状态合并与防抖表之后是上面这两个。

**步骤 2 — `go test ./core -run 'TestConfigWatcher' -count=1 -v`**：24 个 Test 函数 = 23 PASS + 1 SKIP（`ok godelayq/core 7.823s`，下面这份就是同一次运行的逐条输出，不拼接不同轮次）。

```
PASS  TestConfigWatcher_TriggersOnWrite              (0.11s)   要求 1
PASS  TestConfigWatcher_CoalescesBurstWrites         (0.47s)   要求 2
PASS  TestConfigWatcher_IgnoresOtherFiles            (0.82s)   要求 4 + 正向对照
PASS  TestConfigWatcher_TriggersOnRenameReplace      (0.62s)   要求 5
PASS  TestConfigWatcher_TriggersOnRemove             (0.11s)   要求 6
SKIP  TestConfigWatcher_TriggersOnChmod              (0.00s)   要求 7 平台受限
PASS  TestConfigWatcher_TriggersOnIdenticalContent   (0.37s)   要求 3
PASS  TestConfigWatcher_StoresStateFromReload        (0.13s)   要求 9、12（逐字段透传）
PASS  TestConfigWatcher_SetDebounce                  (2.00s)   §3/§5.7
PASS  TestConfigWatcher_CloseIdempotent              (0.62s)   要求 8、11 + trigger 短路
PASS  TestConfigWatcher_WatcherErrorVisible          (0.12s)   要求 10
PASS  TestConfigWatcher_MarkWatcherErrorKeepsLastReload (0.11s) 要求 10 的另一半（ME 的判红点）
PASS  TestConfigWatcher_PanicDoesNotKillLoop         (0.22s)   要求 13
PASS  TestConfigWatcher_RejectsMissingPath           (0.01s)   §3 构造期
PASS  TestConfigWatcher_ConcurrentStateReads         (0.63s)   要求 9
PASS  TestConfigWatcher_CloseStopsTimersWithoutLoop  (0.31s)   要求 11（Close 自己停表）
PASS  TestConfigWatcher_CtxCancelStopsScheduling     (0.72s)   要求 11（ctx 退出三重收口；次序是用例的一部分）
PASS  TestConfigWatcher_StateWritesDoNotLoseEachOther (0.01s)  要求 10 并发那一半（回归网，见 D-R0520）
PASS  TestConfigWatcher_ReplacesExpiredDebounceTimer (0.05s)   防抖表换代（MC 的判红点）
PASS  TestConfigWatcher_OldFiredCallbackDoesNotStealTheNewSlot (0.19s) 旧回调不抢新表项（MD 的判红点）
PASS  TestConfigWatcher_ReloadCallsAreSerialized     (0.09s)   ReloadFunc 契约第 1 条
PASS  TestConfigWatcher_SecondRunDoesNotPanic        (0.23s)   一个 watcher 一个循环
PASS  TestConfigWatcher_ResolveDiskFileName          (0.01s)   折叠匹配（任何平台）
PASS  TestConfigWatcher_MatchesOnDiskNameCasing      (0.12s)   错配拼写端到端
ok    godelayq/core   7.823s
```

要求 3（内容不变也要调）现在有自己的用例 `TriggersOnIdenticalContent`：两次同字节写入、间隔 150ms 大于窗口 100ms，
所以落在两个不同防抖窗口里，必须是两次调用。首轮它只是"实现不做内容比较"的口头证据（见 §10.5 D-R0513）。

**步骤 3 — `go test ./core -race -count=5 -timeout 30m`**：无 flake、无 data race。

```
ok  	godelayq/core	114.049s
```

（读数一路是 86.694s（首轮记录）→ 95.239s（返工前对同一份字节复测）→ 102.682s（第二轮返工后）→ 114.049s（第三轮）。
本卡的用例全在等真实时间窗口，包内 wall 随用例数线性走，本机 wall 又随负载浮动，所以这些数字只能当"本次真实输出"读、
不能当基线；卡 §6 要的判据是"ok 且无 data race"，每一次都成立。）

**步骤 4 — `go build ./... && go vet ./...`**：两条命令均无输出（BUILD_VET_CLEAN）。

**步骤 5 — `go test ./... -race -count=1 -timeout 30m`**：五个有测试的包全 ok。

```
ok  	godelayq/api          219.930s
ok  	godelayq/cmd/server   6.479s
ok  	godelayq/core         21.709s
ok  	godelayq/executor     25.119s
ok  	godelayq/store/sqlite 3.744s
（cmd/gensecret、cmd/hashpassword、examples/demo1、examples/demo2、web 无测试文件）
```

第三轮改完 `core` 之后同一条命令再跑一次（全仓重验，串行跑，不与变异并行）：

```
ok  	godelayq/api          224.516s
ok  	godelayq/cmd/server   6.509s
ok  	godelayq/core         22.240s
ok  	godelayq/executor     24.541s
ok  	godelayq/store/sqlite 3.969s
```

`api` 那个读数要如实记下它的浮动：首轮卡面记录 124.196s，返工前后我这边四次复测分别是 232.994s、128.744s、219.930s、本次 224.516s——同一条命令、同一台机器，wall 能差到 2 倍（该包大量 SQLite/HTTP 超时用例，跟负载强相关；本卡没改 `api`，但 `-race` 下整仓并发跑时它会跟别的包抢 CPU）。
所以这张表只能当"本次真实输出"读，不能当基线；判据是 `ok`（无 FAIL、无 race），五包全部成立。

另外记一条流程上的教训：**这组数字是重跑过的**。上一轮我把"变异反验证"与"后台全量验证"并行跑，两次读数（103.619s / api 128.744s）因此可能被变异的中间字节污染过编译窗口——结果本身是 `ok`，但既然无法证明那次跑读的是干净字节，就在终态字节上整轮重跑一遍（步骤 2–5 全部是重跑后的读数），并把并行这件事记为 D-R0515：全量验证期间不许同时改被验证包的源文件。

**变异反向验证（十九个变异体，只改 `core/watch.go`，每个跑完即从字节备份还原）**：

这张表分三代，且**最后一列全部是在终态字节（`b8cd8405…`）上跑出来的**：
1–6 是首轮那六条，第三轮又改了 `schedule`/`storeState`/`MarkWatcherError`/`Run` 四处，所以它们在终态字节上**整批重跑**，
判红用例与结论不变（只有 `watch_test.go` 的行号随补用例整体后移）；7–14 是第二轮返工补的；MA–ME 是第三轮返工补的。
下表是三次跑下来的合并结果，全部为真实输出。

| 编号 | 变异 | 判红的用例 | 报红信息 |
| --- | --- | --- | --- |
| 1 | 盯文件而不是盯目录（`fsw.Add(w.dirname)`→`Add(w.path)`） | TestConfigWatcher_TriggersOnRenameReplace | `等待 2 次重载调用超时（2s），已收到 1 次` |
| 2 | basename 前缀匹配而非精确相等 | TestConfigWatcher_IgnoresOtherFiles | `期望调用次数不超过 0，但窗口内已达到 1 次` |
| 3 | `Close` 里不调 `stopTimers` | TestConfigWatcher_CloseStopsTimersWithoutLoop | `Close 自己没停计时器：timers 残留 1 个（此时事件循环从未启动，没人能替它清）`。**同一次变异下 `CloseIdempotent` 反而 PASS**，见下面"变异 3 的两轮" |
| 4 | 用 ReloadFunc 交回的空值覆盖 WatcherError | TestConfigWatcher_WatcherErrorVisible | `重载后 WatcherError 应保留为 "config watcher event channel closed"，实际 ""` |
| 5 | 去掉 onReload 外的 recover | TestConfigWatcher_PanicDoesNotKillLoop | `panic: boom` 顺着计时器协程逃逸、整个测试二进制崩溃（栈见下） |
| 6 | 事件只筛 `Create\|Write` | TestConfigWatcher_TriggersOnRemove | `等待 1 次重载调用超时（2s），已收到 0 次` |
| 7 | 去掉 `callReload` 的 `reloadMu.Lock/Unlock` | TestConfigWatcher_ReloadCallsAreSerialized | `ReloadFunc 调用出现重叠：同时在跑的峰值 2，期望 1（reloadMu 串行化失效）` |
| 8 | 去掉 `Run` 的 `defer w.stopTimers()` | TestConfigWatcher_CtxCancelStopsScheduling | `期望调用次数不超过 0，但窗口内已达到 1 次`（`Run` 返回之后计时器照样打进 ReloadFunc） |
| 9 | 去掉 `Run` 的 `defer` 关句柄那一段 | TestConfigWatcher_CtxCancelStopsScheduling | `Run 因 ctx 取消返回后 fsnotify 句柄应已关闭：对同一目录调 Add 应返回错误，却成功挂上了新监听` |
| 10 | `storeState` 存之前把 `Error`/`AppliedKeys`/`IgnoredKeys`/`LastAppliedAt` 抹空 | TestConfigWatcher_StoresStateFromReload | 四条一起报：`Error 应原样保留，实际 ""`、`AppliedKeys 应原样保留为 [logging.level]，实际 []`、`IgnoredKeys 应原样保留为 [server.port]，实际 []`、`LastAppliedAt 应为 …，实际 0001-01-01 00:00:00 +0000 UTC` |
| 11 | 去掉第二次 `Run` 的 `started` CAS 守卫（改成无条件 `Store(true)`） | TestConfigWatcher_SecondRunDoesNotPanic | 先是 `第二个 Run 调用应立即返回（同一 watcher 只跑一个事件循环），2s 内未返回`，随后 `panic: close of closed channel`（栈里 `Run` 那一帧报在变异体的 `watch.go:236`，协程由 `runAndCheckImmediate`（`watch_test.go:837`）起） |
| 12 | `basename` 直接用 `filepath.Base(path)`、不走 `resolveDiskFileName` | TestConfigWatcher_MatchesOnDiskNameCasing | `basename 应解析成磁盘拼写 config.yaml，实际 "CONFIG.yaml"` + `大小写错配的 -config 路径下 watcher 一次都没触发：basename 没解析成磁盘拼写（静默失效）` |
| 13 | 去掉 `trigger` 的 `closed` 短路 | TestConfigWatcher_CloseIdempotent | `期望调用次数不超过 1，但窗口内已达到 2 次`（Close 之后直接调 `trigger()` 仍新起一次重载） |
| 14 | `handleEvent` 认出了目标文件却不排期（`w.schedule(w.path)`→`_ = w.path`），即"监听器整体失灵" | TestConfigWatcher_IgnoresOtherFiles | `等待 1 次重载调用超时（2s），已收到 0 次`——红在末尾那次**正向对照**上。少了对照，这条用例只有反向判断（"不该触发的没触发"），它在"什么都不触发"的实现下会绿灯（§10.5 D-R0511） |
| MA | 去掉 `stateMu` 那对加解锁（回到 D-R0516 的形态） | **没有判红点**：`StateWritesDoNotLoseEachOther` 在本机 5000 轮 × 5 次都没撞到一次丢更新，带 `-race` 也不撞（两个写方都是 atomic 操作，竞争检测器看不见逻辑丢更新）；这条如实登记为 D-R0520，`stateMu` 的必要性由顺序判据 M4/ME + 结构性论证支撑 |
| MB | 去掉 `Run` 的 defer 里那次 `closed.Store(true)` | TestConfigWatcher_CtxCancelStopsScheduling | `期望调用次数不超过 0，但窗口内已达到 1 次`——红在末尾那次**直接调 `trigger`**上；这条判据必须排在 `Close` 之前，否则 `Close` 自己置的位会把它遮住（D-R0519） |
| MC | `schedule` 回到裸 `Reset` 就 `return`（不看返回值） | TestConfigWatcher_ReplacesExpiredDebounceTimer | `等待 1 次重载调用超时（2s），已收到 0 次`——表里留的是那条已跑过的死计时器，此后这条路径的排期永远打不进 ReloadFunc |
| MD | 去掉 `claim` 的序号比对（改成无条件摘表项 + 一律触发） | TestConfigWatcher_OldFiredCallbackDoesNotStealTheNewSlot | 两条一起报：`旧的那条到期回调又触发了一次重载：调用次数从 0 变成 1（身份比对失效）`、`表里当前那条计时器被旧的回调摘掉了：新排期成了停不住的孤儿（身份比对失效）` |
| ME | `MarkWatcherError` 从空快照起步（不带上一次重载的字段） | TestConfigWatcher_MarkWatcherErrorKeepsLastReload | `Result 应保持上一次重载的 unchanged，实际 ""`、`Error 应保持原样，实际 ""`、三份键清单与两个时间字段各自报红 |

**第三轮补的五条变异（MA–ME）里 MA 是唯一不红的一条**，这是本轮最重要的如实记录：
它不是"用例没写好"，而是那个缺陷在 Go 里造不出确定性交错——丢更新要求被插进"读快照"与"换指针"
那几条指令之间，而两边的读写都是 atomic 的，`-race` 也不会报。处理方式是不假称有判红点，
把能保证的两半边（M4 与 ME，都是顺序形状）当判据，把并发那半边登记为 D-R0520 并写进 §10.6。

**M8 与 MB 这两条要在同一份字节上各自判红**，靠的是 `CtxCancelStopsScheduling` 里三条旁证的次序
（紧接循环返回查 `timers` → `Add` 探针 → 直接调 `trigger`，全部排在 `Close` 之前）：
第二轮那版用例把 `timers` 检查放在静默窗口之后、把 `trigger` 检查放在 `Close` 之后，
于是加了 `claim` 之后 M8 变绿、加了 `closed` 之后 MB 变绿——同一类"修一处遮住另一处判据"的形状
第二次出现，登记为 D-R0519。

变异 5 的栈（返工轮重跑，行号是新字节上的）：
`created by time.goFunc` → `schedule.func1 watch.go:298` → `trigger watch.go:333` → `callReload watch.go:357/358`
→ `watcherFixture.reload watch_test.go:115`。**它证明 §9 第一行的站点纠正没错**：panic 发生在防抖计时器的协程上，
与事件循环无关，未捕获时崩溃的是整个测试二进制（生产进程同理），而不是"只死事件循环"。

变异 3 的两轮（这条是本轮最重要的判据缺口，登记为 §10.5 D-R0505）：
首版 `CloseIdempotent` 用"写文件后立刻 Close"制造在计时的计时器，六个变异第一轮跑下来 3 号**没红**——
Close 可能赶在 fsnotify 事件投递/消费之前，`timers` 表本就空，"timers 为空"成了空断言（D-R0501）。
把用例改成先 `schedule` 再 Close 之后 3 号判红了；但返工轮给 `Run` 也加了 `defer w.stopTimers()`（变异 8 的修复本体），
两条退出路径共用同一个 `stopTimers`，于是"跑过循环再 Close"那个形状**又分不出是谁清的了**：
本轮实测 3 号在 `CloseIdempotent` 上 PASS（上表括注的那条），必须靠新增的 `CloseStopsTimersWithoutLoop`
（整场不启动 `Run`，排期只能来自测试自己的一次 `schedule`）才判得红。这条不是把旧变异改成新用例那么轻——
它说明"两处共用一份清理"会让原有用例的判据失效，改实现时要连着复判旧变异。

**字节一致性**：十九个变异只动 `core/watch.go`，`core/watch_test.go` 全程未参与变异。
每个变异跑完都从 `C:\Users\xing\AppData\Local\Temp\r05mut3\watch.go.orig` 用二进制方式
（python `open(p,'rb')`/`open(p,'wb')`）还原，脚本在每个变异结束后断言 `sha256(工作文件) == sha256(备份)`，
十九个里除了没有判红点的 MA 之外全部判红，且每次都还原成功；全部跑完后的读数仍是：

```
b8cd8405af4e2654b64f0bdd72068878f2e269f24f06aaf2873c6b5b5fb7f330  core/watch.go
```

与变异前备份逐字节一致。历史上另外两版的字节已作废：`f5373d9336…` 是首轮、`342463c8…` 是第二轮返工后的版本。

### 10.4 手工验收

本卡纯新增两个文件、不起任何进程、也没有任何调用方：全仓 grep `NewConfigWatcher`/`ConfigWatcher`
只命中 `core/watch.go`（定义）、`core/watch_test.go`（测试）和 `core/config_reload.go` 的一句注释，
`cmd/server` 一行没改。所以 DoD"未打开 `reload.enabled` 时本类型完全没有被构造的机会"成立——
没有装配点，就没有构造入口，无需额外守卫。真正的"改一次存盘看进程是否按防抖重载一次"的手工冒烟
需要重载链与 `/admin/runtime`，那是 R06 的接线，端到端实测归 R07。

本卡用以下方式替代"起进程手工验收"（全部真实跑过）：

- 单元 + `-race`：23 PASS + 1 SKIP（§10.3 步骤 2，两轮返工后共 24 个 Test 函数）；`./core -race -count=5` 无 flake（步骤 3）；
  全仓 `-race -count=1` 五包 ok（步骤 5）。
- 十九个变异体反向证明每条判据都被用例守住（§10.3 变异表，其中 MA 是唯一不红的一条，如实登记为 D-R0520）：
  首轮那六个在第三轮的新字节上重跑仍判红，
  新写的八条判据（串行、ctx 退出停表、ctx 退出关句柄、逐字段透传、第二次 `Run`、磁盘拼写、`trigger` 短路、
  正向对照接住"监听器整体失灵"）各有自己的变异。
- 监听器自身故障可见：`WatcherErrorVisible` 用 `w.watcher.Close()` 真实关闭底层 fsnotify、复现"事件通道已关闭"，
  断言 `State().WatcherError` 非空且有 error 日志，再验下一次重载不抹掉它。
- 关停可见：`CloseIdempotent` 判"循环已返回 + timers 空 + 关闭后写文件不触发 + 直接调 `trigger()` 也不新起"，
  `CloseStopsTimersWithoutLoop` 判"`Close` 自己停表"，`CtxCancelStopsScheduling` 判"ctx 那条退出路径同样收口"
  （停表、关句柄、置 `closed` 三件事，且三条旁证都排在 `Close` 之前——次序本身是用例的一部分，见 D-R0519）。

SKIP 清单（平台受限，写明原因）：

- `TestConfigWatcher_TriggersOnChmod`：`runtime.GOOS=="windows"` 显式 `t.Skip`。
  Windows 没有 POSIX chmod，`os.Chmod` 改的是只读属性，fsnotify 是否投递、投成哪种 Op 都不可预测，
  本卡只断言"写与 rename 必触发"（要求 7），Chmod 触发不写成两边都过的假断言。
- `TestConfigWatcher_MatchesOnDiskNameCasing`：在大小写敏感的卷上 `t.Skipf`（那种卷上 `os.Stat("CONFIG.yaml")`
  就先失败，端到端的错配拼写根本构造不出来），折叠匹配的代码路径由 `ResolveDiskFileName` 那条覆盖。
- `TestConfigWatcher_ResolveDiskFileName` 的最后一支（"目录里同时存在 `config.yaml` 与 `Config.yaml` 时精确相等优先"）：
  本机 Windows 写第二种拼写会**成功但覆盖同一个文件**，目录里仍只有一条，用例 `t.Logf` 后 return。
  这一支只有在大小写敏感的卷上才构造得出来，本机未验（见 §10.6）。

### 10.5 缺陷

| 编号 | 现象 | 判定 | 处置 |
| --- | --- | --- | --- |
| D-R0501 | 首版 `TestConfigWatcher_CloseIdempotent` 用"写文件后立刻 Close"制造在计时的计时器，但 Close 常赶在事件投递前，`timers` 本就空——"忘了停计时器"的变异 3 逃过判红（空断言）。 | 测试判据缺陷，变异发现（首轮） | **已修**：改为先直接调 `schedule` 确定性放入一个在计时的计时器、再 Close。R05。注意它在返工轮被 §10.5 D-R0505 再次削弱，那条是后续。 |
| D-R0504 | `ReloadFunc` 的调用没有被串行：触发者是防抖计时器的协程（`time.AfterFunc` → `trigger` → `callReload`），两个窗口的到期可以重叠，于是两条重载链并发跑、后发起的那次先返回反而被前一次的旧结果覆盖 `State()`；同时 `watch.go` 里的契约注释写的是"事件循环串行调用它"，与实现相反。评审 Important-1。 | 实现缺陷 + 注释错误 | **已修**：`callReload` 持 `reloadMu` 把"调用 + `storeState`"整段串行；契约注释改成两条（watcher 串行 / 实现方仍要自己与进程内其它写入口互斥）并写明"调用可能在 `Run`/`Close` 返回之后才结束"。用例 `ReloadCallsAreSerialized` 判峰值==1，变异 7 判红。R05 |
| D-R0505 | 修 D-R0506 之后新出现的判据缺口：`Close` 与 `Run` 共用同一个 `stopTimers`，"跑过循环再 Close"的 `CloseIdempotent` 分不出是谁清的表——"Close 忘掉停表"的变异 3 在这条用例上**变绿**（本轮实测 PASS），要靠 `Run` 的 defer 才判得红的变异 8 又只覆盖 ctx 那条路。 | 测试判据缺陷，变异发现（返工轮） | **已修**：新增 `CloseStopsTimersWithoutLoop`——整场不启动 `Run`，排期只能来自测试自己的一次 `schedule`，`Close` 之后 `timers` 必须为空。变异 3 在这条上判红（`残留 1 个`）、在 `CloseIdempotent` 上确认变绿，两条一起构成完整证据。教训写进 §10.3：改实现（两处共用一份清理）时要连着复判旧变异。R05 |
| D-R0506 | `Run` 因 ctx 取消返回时不收口：`stopTimers` 与关 fsnotify 句柄都只在 `Close` 里做，而 `closed` 位点也只由 `Close` 置真。于是"写文件排上窗口 → 取消 ctx → `Run` 返回 → 计时器到点仍打进 `ReloadFunc`"，正是要求 11 反着写的形状；句柄一路漏着直到有人调 `Close`。测试台全部用 ctx 停，这条路径首版一次都没被测过。评审 Important-2。 | 实现缺陷 | **已修**：停表抽成 `stopTimers`，`Run` 用 defer 做与 `Close` 同样的两件事（关句柄、停表）。用例 `CtxCancelStopsScheduling`，变异 8、9 分别判红。R05 |
| D-R0507 | `basename` 用的是入参路径里那一段文件名，而 fsnotify 上报的是磁盘上的拼写。Windows/macOS 默认卷大小写不敏感：`-config C:\...\Config.yaml` 会 `os.Stat` 成功、watcher 建得出来、`State()` 一切正常，但精确比对永不命中，监听器再也不触发且没有任何地方报错——本系列点名要消灭的"沉默"。评审 Minor。 | 实现缺陷（平台相关，Windows 上是主路径） | **已修**：构造期用 `resolveDiskFileName` 解析目录里的真实拼写（先精确相等、再 `strings.EqualFold`），解析不出来直接返回构造错误。用例两条：`ResolveDiskFileName`（任何平台都走折叠匹配那一路）+ `MatchesOnDiskNameCasing`（端到端，敏感卷上 `t.Skipf` 指回前一条）；变异 12 判红。R05 |
| D-R0508 | 同一个 watcher 上第二次调 `Run`（含 `Close` 之后再调）会走到 `defer close(w.loopDone)`，对已关闭的通道再关一次 → panic；而调用形状是 `go w.Run(ctx)`，协程里 panic 等于打挂进程。评审 Minor。 | 实现缺陷 | **已修**：`started.CompareAndSwap` 只认第一个进入者，后进者立即返回、什么都不做；行为写进 `Run` 注释。用例 `SecondRunDoesNotPanic`（先真触发一次证明第一个循环在跑），变异 11 判红（`2s 内未返回` + `panic: close of closed channel`）。R05 |
| D-R0509 | `StoresStateFromReload` 首版只断言 `Result`/`RejectedKeys`/`LastAttemptAt` 三项，卡要求 9 明写"含三份键清单"与 `Error`/`LastAppliedAt`；一个只把 `Result` 存对、其余抹空的实现能绿灯。评审 Minor。 | 测试判据缺陷 | **已修**：改成逐字段断言（`Error`/`AppliedKeys`/`IgnoredKeys`/`RejectedKeys`/`LastAppliedAt`/`Enabled`/`WatchedPath`/`WatcherError` 八项全覆盖）。变异 10 一次报出四条红。R05 |
| D-R0510 | `Close` 之后"计时器回调即使漏过停表也会被 `trigger` 的 `closed` 短路挡住"这条没有判红点：原 (b)(c) 只能证明"表已空 + 新事件不排期"，删掉 `trigger` 里那次 `closed` 检查能全绿。评审 Minor。 | 测试判据缺陷 | **已修**：`CloseIdempotent` 增加第 (d) 步——`Close` 返回后直接调 `w.trigger()`，仍不得新起一次调用。变异 13 判红。顺带把"短路保证的是不再新起、不是绝不重叠"这个残余竞态写进 `trigger` 注释。R05 |
| D-R0511 | `IgnoresOtherFiles` 只有反向判断（"不该触发的没触发"）：如果监听器整个失灵、`onReload` 一次都收不到，`expectStays(0)` 照样绿灯。沉默正是本系列要消灭的缺陷类。评审 Minor。 | 测试判据缺陷 | **已修**：同一次运行末尾加正向对照——写目标文件必须正好触发一次（`waitFor(1)` + `expectStays(1)`）。判红点是变异 14（`handleEvent` 收到目标事件却不排期，即"监听器整体失灵"）：`等待 1 次重载调用超时（2s），已收到 0 次`。R05 |
| D-R0512 | §9 风险表把 ReloadFunc panic 的站点写成"事件循环里 `defer recover()`"。实际调用点是防抖计时器的协程，recover 挂在循环上捕获不到，未捕获的 panic 崩溃的是整个进程（§10.3 变异 5 的栈：`created by time.goFunc` → `schedule.func1` → `trigger` → `callReload`）。 | 文档错误（会导致错误的修复位置） | **已修**：§9 那一行的处置改写为"`callReload` 内 recover"，并把站点与后果写进 `callReload` 注释与 §10.3。R05 |
| D-R0513 | 要求 3（"内容不变也要调"）首版没有用例，只在 §10.3 写成"实现不做内容比较，`TriggersOnWrite` 即证据"的口头论证——日后有人加内容哈希缓存，没有一条用例会判红。评审 Minor。 | 测试覆盖缺口 | **已修**：新增 `TriggersOnIdenticalContent`（两次同字节写入、间隔 150ms 大于窗口 100ms，必须两次调用）。R05 |
| D-R0514 | `State()` 返回的是**浅拷贝**：`AppliedKeys`/`IgnoredKeys`/`RejectedKeys` 仍与 `ReloadFunc` 交回的那份共享底层数组。 | 携带项，非本卡缺陷 | **登记不修**：契约已写进 `State()` 注释（交回后不得再改那几份切片；读到的一方要整理就先复制）。属主 R06：渲染 `/admin/runtime` 时不许就地排序或改写。R06 |
| D-R0515 | 本轮一度把"十四个变异体逐个改写 `core/watch.go`"与后台的 `./core -race -count=5` + 全量 `-race -count=1` **并行跑**，全量那一轮很可能在某个变异窗口里编译过（结果本身是 `ok`，但读到的字节无法证明是终态）。 | 验证流程缺陷，自己发现 | **已修**：在终态字节上把步骤 1–5 整轮重跑（`gofmt -l` 无输出、20 个 Test 函数 19 PASS + 1 SKIP 且 `ok 7.699s`、`go build ./... && go vet ./...` 无输出、`./core -race -count=5` 102.682s、全量五包 `ok`、结束时 `sha256` 与变异备份一致），§10.3 的读数全部换成这一轮；变异与全量验证从此串行。R05 |
| D-R0516 | 状态的两个写方互相抹字段（第三轮复核 I-A）：`storeState` 与 `MarkWatcherError` 都是不带同步的"读快照—改—整份换指针"。一次 fsnotify 错误被记进 `WatcherError` 之后，正在跑的那次重载会用它 `onReload` 之前读到的旧快照整份写回，把这条监听器故障**永久抹掉**（要求 10 的反面）；反过来的交错会把刚完成的 `Result`/`LastAttemptAt` 退回旧值。`reloadMu` 挡不住第二头，因为事件循环不取那把锁。 | 实现缺陷（丢更新） | **已修**：两个写方的读—改—写都进 `stateMu`（只锁几次赋值，不锁 `onReload`，理由见 §10.2 第 16 条）。确定性判据是 M4（`storeState` 覆盖 `WatcherError`）与 ME（`MarkWatcherError` 从空快照起步，`MarkWatcherErrorKeepsLastReload` 判红）；并发那一半见 D-R0520。R05 |
| D-R0520 | D-R0516 的修复本身**没有确定性判红点**：去掉 `stateMu` 那对加解锁（变异 MA）后，丢更新要求被插进"读快照—写快照"那几条指令之间；本机把轮次加到 5000 轮 × 5 次都没撞到一次（带 `-race` 也没撞到——两个写方都是 atomic 读写，竞争检测器看不见逻辑丢更新）。 | 测试判据缺口，变异发现 | **登记不修**：并发交错无法在 Go 里确定性地造出来，除非给实现加测试专用钩子（本卡取"不加钩子"）。做法是把能保证的部分降级成两条**顺序**判据（M4/ME，都判红）+ 一条并发回归网（`StateWritesDoNotLoseEachOther`，本机对 MA 不红、对终态实现恒绿），`stateMu` 的必要性按 §10.2 第 16 条结构性论证。R07 收口时在文档里如实写"读端点看到的最后一份"在并发下由这把锁保证、不由用例保证。R07 |
| D-R0517 | `closed` 只由 `Close` 置真（第三轮复核 I-B）：ctx 取消那条 `Run` 退出路径上 `trigger` 的短路永不生效，而 `ReloadFunc` 契约与 `Run` 注释都写着"`Run`/`Close` 返回之后不再新起一次调用"——那句话对 `Close` 成立、对 ctx 不成立；`trigger` 注释把残余窗口说成"几条指令宽"，在 ctx 那条路上其实是无限的（第二轮补的 `stopTimers` 只挡住"还没到点"的那些）。 | 实现缺陷 + 注释与实现不符 | **已修**：`Run` 的 defer 先置 `closed` 再关句柄、停表、关 `loopDone`（次序写进注释）。判红点是 `CtxCancelStopsScheduling` 末尾那次"直接调 `trigger`"（变异 MB），且它排在 `Close` 之前——否则 `Close` 自己置的位会把它遮住（D-R0519）。R05 |
| D-R0518 | 已到期的防抖计时器被裸 `Reset` 留在表里（第三轮复核 I-C，我复核 `Reset` 语义后成立）：`schedule` 不看 `Reset` 的返回值就 `return`，而表里那条已经跑过——于是这一路排期永远不会打进 `ReloadFunc`（此后每次写入只是给一条死计时器续期，监听器认得出事件却再也不重载）；同时旧回调与新表项还能凑出第二次触发，且旧回调会把新表项删成停不住的孤儿。卡 §3 明写的是"`Stop` 后重建"，照抄的 `core/load.go:195-197` 反而是裸 `Reset`。 | 实现缺陷（照抄了参考实现的同一缺口） | **已修**：表换成 `debounceSlot`，`Reset` 返回假就 `Stop` 旧 + `arm` 新，回调按序号认领（`claim`）。判红点 MC（`ReplacesExpiredDebounceTimer`）与 MD（`OldFiredCallbackDoesNotStealTheNewSlot`）。顺带把"两处共用一份清理会让既有用例判据失效"升级成"断言**次序**也会"，见 D-R0519。R05 |
| D-R0519 | 本轮第二次踩到"修好一处就把另一处的判红点遮住"：加了 `claim` 之后，到期回调自己会摘表项，于是 M8（`Run` 的 defer 忘停表）在原来的"静默窗口后再查 `timers`"的次序下**变绿**；而 MB（`Run` 退出没置 `closed`）在原来"先 `Close` 再直接调 `trigger`"的次序下也**变绿**（`Close` 替它置了位）。两条都是本轮实测到的。 | 测试判据缺陷，变异发现 | **已修**：`CtxCancelStopsScheduling` 把三条旁证全部挪到**紧接循环返回、且在 `Close` 之前**，窗口从 200ms 改成 1s（让"循环刚返回"与"计时器到点"分得开）；改完 M8、MB 各自判红。判据次序写进用例头注。R05 |
| D-R0521 | `resolveDiskFileName` 在第二轮里对"目录列不出来"直接返回构造错误（第三轮复核 Minor 1）：POSIX 上 0711 的目录、Windows 上"可读文件但不可列目录"的 ACL，都能 `os.Stat` 成功而 `os.ReadDir` 失败——这类以前完全能起 watcher 的部署会退化成"热重载起不来"。 | 实现缺陷（退化） | **已修**：改成返回 `(basename, listed, err)`，列不出来时回退成入参拼写并由 `NewConfigWatcher` 记一条 warn（就是本卡之前的行为）；只有"列得出但没有匹配项"才是错误。用例补 `ResolveDiskFileName` 那一支（用一个文件路径当目录，跨平台都能构造 `ReadDir` 失败）。R05 |
| D-R0522 | 大小写敏感卷上"接线"这一半没有判红点（第三轮复核 Minor 3）：那种卷里入参拼写必然等于磁盘拼写，变异 12（`basename` 不走 `resolveDiskFileName`）与终态实现**逐位等价**，没有任何用例能区分。 | 等价变异，如实登记 | **登记不修**：不写"只断私有字段"的假用例（照 R02 那条 N5 的先例）。错配拼写端到端由 `MatchesOnDiskNameCasing` 在大小写不敏感的卷上覆盖（本机 Windows 实跑通过），Linux/macOS 侧只有 `ResolveDiskFileName` 的折叠匹配那一路有判据。跨平台实测归 R07。R07 |
| D-R0523 | `ConcurrentStateReads` 没证明读与重载真重叠（第三轮复核 Minor 5）：两个读协程各跑 200 轮（约 200ms）在 `waitFor(1)` **之前**就起来了，完全可能在第一次触发之前读完，那时这条用例只剩"`-race` 没报错"。 | 测试判据缺陷 | **已修**：先 `waitFor(1)` 再起读协程，两轮 200 次读确定落在写入与重载仍在流动的区间里。R05 |
| D-R0524 | `waitForLoop` 读的 `started` 由 `go Run` 那个协程自己置真（第三轮复核 Minor 6）：`Close` 赶在它之前时会直接返回 nil，"Close 等到事件循环返回"那一瞬并不成立。 | 口径披露 | **已修（注释与卡面措辞）**：`waitForLoop` 注释写明这一瞬与真正的保证来源（`closed` 先置真 + 循环自己退出时收口），§6 第三条按这个口径改措辞。不改结构：`started` 只能由 `Run` 置，否则"从没跑过 Run 就无需等待"那条判断会反过来拖住测试。R05 |
| D-R0525 | 三处引用与死代码（第三轮复核 Minor 7）：`callReload` 注释写"§9 风险表第一行"实际是第三行；`waitForLoop` 注释引了一个不存在的编号 `D-R05`；`Run` 的 defer 与 `Close` 里的 `w.watcher != nil` 守卫永不成立（构造函数失败就不返回实例），却与 `Run` 里那次无条件解引用互相矛盾。另：§10.3 变异 5 的栈把 `callReload` 那两帧说成调用点。 | 注释/文档不准 | **已修**：三条引用改正、两处死守卫删掉（v1.9.0 各后端"重复 `Close` 返回 nil"这条依赖保留，它才是 `Run` 与 `Close` 都关句柄的前提）；变异 5 那一行改成"recover defer 所在帧"并给出真实调用点。R05 |
| D-R0526 | 第二版 `arm` 写成 `t := time.AfterFunc(d, func(){ ... t ... })`，回调读的那个 `t` 与赋值之间**不构成 happens-before**（`:=` 的变量在初始式里还没进作用域，改成先声明后赋值虽然能编译，但写入发生在计时器已交进运行时之后）。窗口极窄、`-race` 实测不到，但形式上是数据竞争。 | 实现缺陷，自己发现 | **已修**：改成先建 `debounceSlot` 并把序号**按值**闭包进回调（身份不来自共享变量），理由写进类型注释与 §10.2 第 15 条；MD 那条用例判的就是"身份比对失效"的后果。R05 |
| D-R0502 | `NewConfigWatcher` 立刻发布初值 `State()`：`Result==""`、`Enabled==false`、`LastAttemptAt/LastAppliedAt` 为零值 `time.Time`。配合 D-R0101（`ReloadState` 两个 `time.Time` 字段的 `,omitempty` 对 `time.Time` 无效），一旦 R06 把它 JSON 化，"从未尝试过"会序列化成 `0001-01-01T00:00:00Z` 而非省略。 | 携带项，本卡可见更早 | **登记不修**：R06 的具体动作（本轮评审要求写清）：(1) `/admin/runtime` 的 `reload` 对象不要直接 `json.Marshal(core.ReloadState)`，两个时间字段由 R06 自己格式化成指针或字符串，零值时省略；(2) 判"从未尝试过"一律用 `Result==""`，不要用时间零值；(3) R06 的用例里要有一条覆盖"进程起来但一次重载都没跑过"时 `reload` 对象的实际形状。属主 R06（D-R0101）。R06 |
| D-R0503 | 初值 `State().Enabled==false` 与"监听器正在运行"看似矛盾（读端可能误判热更被关）。 | 携带项 | **登记不修**：`Enabled` 由重载链（R06）在交回 `ReloadState` 时写入，watcher 按要求 10 只拥有 `WatcherError`/`WatchedPath`，不擅自填业务开关。R06 的具体动作：`/admin/runtime` 的 `reload.enabled` 读数取**进程侧配置**而不是 `State().Enabled`（否则从没重载过的进程会显示 `false`），并加一条用例钉住"启动后未触发重载时 `enabled` 仍为 true、`watched_path` 非空"。R06 |
| D-R0102 | "两份入参必须已 `Normalized()`"这条前提在 core 里无法自检——本卡的 `ReloadFunc` 不做读配置/比对，也就没有归一化入参可校验。 | 携带项 | **登记不修**：属主 R06（重载链在此调 `LoadConfig`/`Diff`，前提归它守）。R06 |

第二轮（评审返工）合计登记 12 条缺陷，全部已修：**实现侧四条** D-R0504（重载未串行 + 契约注释与实现相反）、
D-R0506（ctx 退出路径不停表、漏句柄）、D-R0507（大小写拼写导致静默失效）、D-R0508（第二次 `Run` panic）；
**测试判据侧六条** D-R0501（空断言）、D-R0505（共用停表后判红点搬家）、D-R0509（透传只断三项）、
D-R0510（`trigger` 短路无判红点）、D-R0511（缺正向对照）、D-R0513（要求 3 无用例）；
**文档侧一条** D-R0512（§9 把 panic 站点写成事件循环）；**验证流程一条** D-R0515（变异与全量验证并行跑，读数可能取自被污染的编译窗口，已整轮重跑）。
第三轮（第二次返工）另登记 11 条：实现侧 D-R0516（丢更新）、D-R0517（ctx 路径不置 `closed`）、D-R0518（已到期计时器留在表里）、
D-R0521（目录不可列出时硬失败）、D-R0526（回调读共享变量的 happens-before 缺口）全部**已修**；
判据侧 D-R0519（断言次序遮住变异）、D-R0523（读与重载没重叠）全部**已修**；
口径与文档 D-R0524、D-R0525 **已修**；两条**登记不修**并写明归属——D-R0520（并发丢更新无确定性判红点，归 R07 文档口径）、
D-R0522（大小写敏感卷上的等价变异，归 R07 跨平台实测）。返工后 `go build`/`go vet`/`./core -race -count=5`/全仓 `-race -count=1` 全绿，
十四个变异体全部判红（其中变异 3 在 `CloseIdempotent` 上是**故意**的绿，由 `CloseStopsTimersWithoutLoop` 接管判红）。

### 10.6 未覆盖项（含平台受限的 SKIP）

诚实列出真没测到的：

- **操作系统驱动的 watcher 真死路径**：`Run` 里 `events` 通道 `!ok` 且 `closed==false` → `MarkWatcherError` 这条，
  只用测试手工 `w.watcher.Close()` 复现（等价于事件通道被关闭）。因"被监听的目录本身被删/卷卸载/
  fsnotify 内部 watch 句柄失败"这类真实故障把通道关掉的路径，本机 Windows 没能稳定复现，未实测。
- **error 通道 `!ok` 但不退出循环**那条分支（§4.2 后半句）：fsnotify 的 `Close()` 会同时关 events 与 errors，
  而事件循环几乎总是先从 events `!ok` 或 `stopCh` 退出，`errs` 被置 nil 后继续这一路没被独立断言。
- **Chmod 触发**：平台受限 SKIP（见 §10.4），未在本机证实。
- **内容等价性（`ReloadUnchanged` vs `ReloadOK`）**：本卡按设计不做内容比较，watcher 无条件调 `ReloadFunc`，
  由 ReloadFunc 自己返 `ReloadUnchanged`；这条区分不在本卡覆盖范围，也未测。
- **在途 ReloadFunc 不被强杀**：Close 注释承诺不强杀握锁中的重载；只有结构性推理（Close 不等计时器回调、
  `closed` 位点只挡新起调用），没有并发压测去证"关停在途重载被完整跑完"。
- **内容等价性（`ReloadUnchanged` vs `ReloadOK`）**：本卡按设计不做内容比较，watcher 无条件调 `ReloadFunc`，
  由 ReloadFunc 自己返 `ReloadUnchanged`；这条区分不在本卡覆盖范围，也未测。
- **`stateMu` 的并发必要性**：变异 MA（把那对加解锁整个删掉）在本机 5000 轮 × 5 次都没撞到丢更新，
  带 `-race` 也不撞（两个写方都是 atomic 操作，竞争检测器看不见逻辑丢更新）。所以 D-R0516 的修复
  只有顺序判据（M4、ME）是硬的，并发那一半靠结构论证 + 回归网（D-R0520）。
- **`claim` 在关停路径上的独立价值**：Close 会先 `stopTimers` 再置 `closed`，到期回调既认领不到表项也进不了
  `trigger`——两道保险叠在一起，所以「关停后挤进来的那次回调被序号挡掉」这一半没有单独的用例；
  序号判据落在换代那一半（MD）。要单独判它只能给实现加测试专用钩子，本卡不加。
- **真实「目录不可列出」的部署**：0711 目录（POSIX）与「可读文件但不可列目录」的 ACL（Windows）都没在
  本机复现出来；用例是用「把一个文件路径当目录传进去」构造同一条 `ReadDir` 失败分支的，走的是同一支
  代码，但不是那种部署本身。
- **macOS/APFS 的 NFD 文件名**：`resolveDiskFileName` 只做大小写折叠，NFC 打字与 NFD 存盘对不上时会落到
  「列得出但没有匹配项」那一支、返回构造错误（R06 记一条 error 后照常提供服务）。本机 Windows，未实测。
- **大小写敏感卷上的接线**：见 D-R0522，那种卷上变异 12 与终态实现逐位等价，没有用例能区分。
- **具体真实编辑器的存盘语义**：`TriggersOnRenameReplace` 真跑了 tmp→`os.Rename` 覆盖并确认触发一次、
  之后普通写入仍能触发（盯目录有效）。没有针对某个真实 Windows 编辑器（VSCode/记事本）的存盘事件形状做实测；
  若它走 copy-overwrite 而非 rename，普通 `Write` 事件仍由同一目录监听覆盖（`TriggersOnWrite`）。
- **`resolveDiskFileName` 的"精确相等优先"分支**：需要同一目录里两个只差大小写的真实条目，
  只有大小写敏感的卷造得出来；本机（Windows，大小写不敏感）跑不到，用例 `t.Logf` 后 return（见 §10.4）。
  折叠匹配那一路反而是每条入参都走的，已覆盖。
- **重叠防抖窗口的真实形状**：`ReloadCallsAreSerialized` 判的是"两个调用同时进 `callReload` 时峰值==1"，
  由测试起两个协程在同一起跑线调用制造；"两个防抖窗口自然到期重叠"需要 `ReloadFunc` 比窗口还慢，
  那种真实形状本卡没构造（窗口 100ms、fixture 的重载是微秒级返回）。串行性本身由 `reloadMu` 保证，与触发者是谁无关。
- **跨实例的调用顺序**：`reloadMu` 是每个 `ConfigWatcher` 实例一把，只排同一个实例的调用顺序。
  两个 watcher 盯同一份配置会并发跑两条重载链——本卡不管，也不该管：装配方（R06）全进程只建一个。
- **"句柄已关"只到 API 层**：`CtxCancelStopsScheduling` 的判据是对已关的句柄再 `Add` 拿到错误
  （v1.9.0 `readDirChangesW.AddWith` 在 `isClosed()` 时返回 `ErrClosed`，`backend_windows.go:110-112`）。
  这证明不了操作系统层面的目录句柄/缓冲区已释放，也没做进程级资源核对。
- **`State()` 交回的切片被交回方继续改**会怎样：这条是浅拷贝契约（D-R0514），本卡只在注释里立约，
  没有用例去证明违约会怎样（Go 里也测不出来——共享底层数组的写入没有钩子）。R06 的读侧要自己守。

