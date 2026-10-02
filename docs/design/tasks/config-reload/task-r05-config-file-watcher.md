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

- [ ] 十三条行为要求各有用例（§3 的编号 1…13）（要求 7 允许平台受限，但要留一条明确的 `t.Skip` 而不是删掉）。
- [ ] 本卡不调 `LoadConfig`、不 import `viper`、不认识任何业务键：
      `core/watch.go` 里 `grep -n "LoadConfig\|Diff\|Executors\|Logging"` 无命中
      （`ReloadState`/`ReloadFunc` 自身除外）。
- [ ] `Close` 之后没有任何 goroutine 残留：用 `runtime.NumGoroutine()` 在 `New` 前、
      `Close` 后取差值断言（允许 ±2，照 `core/scheduler_exec_class_test.go:430` 的既有写法）。
- [ ] 未打开 `reload.enabled` 时本类型完全没有被构造的机会（本卡不改 `cmd/server`）。
- [ ] `go test ./core -race -count=5 -timeout 30m` 无 flake
      （本卡的用例全是等时间窗口的，最容易在这里露出 flake）。
- [ ] `go build ./...`、`go vet ./...`、`go test ./... -race -count=1` 全绿；新增文件已 `gofmt -w`。

## 7. 验收方式

```bash
go test ./core -run 'TestConfigWatcher' -v
go test ./core -race -count=5 -timeout 30m
go build ./... && go vet ./...
go test ./... -race -count=1
```

预期：第一条列出 §5 的 11 条用例（含可能的一条 `SKIP`，SKIP 原因要写在实现记录里）。

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
| ReloadFunc panic 打挂事件循环 | 一次坏配置让监听器死亡，此后所有改动无声 | 事件循环里 `defer recover()` 转成 `WatcherError` 与 error 日志（实现时补进 §3，用例：ReloadFunc 里 panic，断言循环仍在跑且 `WatcherError` 非空） |
| 防抖窗口过短 | 每次存盘读好几遍配置、反复触发写链 | `reload.debounce` 的下界（50ms）在 R01 的 `Validate` 里拦，本卡不重复拦 |
| 计时器泄漏 | 每个路径一个 `time.Timer`，`Stop` 后重建才有界；Close 不清 timers 会留待触发的闭包 | Close 里 `for _, t := range timers { t.Stop() }`（照 `core/load.go` 的 `stopLoaders` 写法），§5.8 的 goroutine 差值用例是它的证据 |
| 用例靠 sleep 判定 | flake，尤其 debounce 50ms 在 CI 机器上 | 一律"10ms 轮询 + 上界断言"，判据写成"次数 <"或"次数 =="而不用时间等式 |

回滚：本卡纯新增两个文件（`core/watch.go`、`core/watch_test.go`），没有任何调用方，
`git revert` 单提交即可，全仓行为零变化。

## 10. 实现记录（执行时补写）

### 10.1 落地的接口

### 10.2 与本卡写法的差异

### 10.3 验证证据（含 §5.3 的反向验证输出）

### 10.4 手工验收

### 10.5 缺陷

### 10.6 未覆盖项（含平台受限的 SKIP）
