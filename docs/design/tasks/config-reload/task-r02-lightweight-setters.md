# TASK-R02　六个轻量 setter：给每个热更键一个可调用入口

- 所属阶段：M1 落点
- 依赖任务：R01
- 涉及文件：`core/logging.go`、`core/logging_test.go`（新增或增补）、`core/scheduler.go`、
  `core/scheduler_hot_config_test.go`（新增）、`core/store.go`、`core/store_hot_test.go`（新增）、
  `core/scheduler_test.go`（断言改写与替身补方法）、`core/store_history_test.go`（断言改写）、
  `core/config_test.go`（断言改写）、`store/sqlite/events_test.go`（断言改写）、
  `cmd/server/main_integration_test.go`（替身补方法）、
  `store/sqlite/events.go`、`store/sqlite/audit.go`、`store/sqlite/hot_retention_test.go`（新增）
- 预计规模：中

## 1. 任务目标

给六个热更键各交付一个运行期可调用的入口：日志级别、重试延迟上限、存储终态留痕的条数与时长、
事件写入器的保留策略、台账写入器的保留策略。本卡结束时这些入口**还没有调用方**
（接线在 R06），但每个都能在自己所属包里被单元测试证明真的改变了行为。

## 2. 背景与当前问题

R01 交付了"哪些键可以热更"的判据，但判据本身不能生效——生效要靠被持有取值的对象肯改口。
当前这五处取值都在构造时定死：

| 取值 | 现在长在哪 | 为什么现在改不了 |
| --- | --- | --- |
| `logging.level` | `slog.HandlerOptions.Level` 是一个 `slog.Level` 常量（`core/logging.go:72`） | 换级别等于换 handler，已交出去的 `*slog.Logger` 全要重建 |
| `scheduler.max_retry_delay` | `Scheduler.retryPolicy` 字段（`core/scheduler.go:41`），构造时填（`:135`） | 字段是普通接口值，运行期写会与 worker 协程的读并发 |
| `store.history_limit`/`history_ttl` | `JSONFileStore.historyLimit`/`historyTTL`（`core/store.go:57-58`），trim 时读（`:161-189`） | 同上，写它要与调度协程的读同步 |
| 观测层两个保留策略 | `EventLog.retentionCount`/`retentionAge`（`store/sqlite/events.go:62-63`，prune 读 `:225-240`）、`AuditLog` 同形（`store/sqlite/audit.go:73-74`，`:213-225`） | 同上 |

这五处都不涉及通道、ticker 或连接，改成原子字段即可，是本系列里改动面最小的一批
（`scheduler.workers` 要真起停协程，单独放在 R03）。

## 3. 要实现的功能

### 3.1 `core/logging.go`：可变级别

```go
// NewLoggerWithLevelVar 与 NewLogger 走同一条解析与 handler 构造，额外把级别载体交回调用方。
// 需要运行期改级别的装配方（cmd/server 的重载链）用它；其余调用方继续用 NewLogger。
func NewLoggerWithLevelVar(level, format string, w io.Writer) (*slog.Logger, *slog.LevelVar, error)

// SetLogLevel 把级别载体改成 level（debug|info|warn|error，解析规则复用 parseLogLevel）。
// 解析失败时载体保持原值——重载路径上"一条写错的级别名"不该把正在跑的日志级别也带走。
func SetLogLevel(v *slog.LevelVar, level string) error
```

- `NewLogger` 改为内部调用 `NewLoggerWithLevelVar` 并丢弃载体，**签名与行为不变**
  （它被 `cmd/server/main.go:324`、示例与测试大量调用）。
- 两者共用同一段 handler 构造，不出现第二份 `text|json` 分岔。
- 载体随 logger 一起建成，因此 `slog.SetDefault(logger)`（`cmd/server/main.go:329`）
  之后改级别，经 `slog.Default()` 的包级调用（第三方库桥接、示例 handler）一起跟着变。
  这是预期行为，注释里要写明，别让人以为只有拿到载体的那一路会变。
- `logging.format` 不在本卡：换格式要换 handler 类型，属重启档（设计文档 §6.2）。

### 3.2 `core/scheduler.go`：重试上限可换

```go
// retryPolicy 字段改成 atomic.Pointer[RetryPolicy]，构造时 Store，读取处 Load。
// 改的原因：worker 协程在 executeJob 里读它（core/scheduler.go:1686 那条调用），
// 而重载链要在运行期写它——普通接口字段的读写并发会被 -race 抓住。

// SetRetryPolicy 运行期替换重试策略。传 nil 视为不改（保持现值）。
// 与 SetConcurrency 那批 setter 的分别：那些只在 Start 前有效、运行期 warn 并忽略，
// 本方法是专给重载链用的，运行期生效。
func (s *Scheduler) SetRetryPolicy(p RetryPolicy)

// RetryPolicyMaxDelay 返回当前策略的延迟上限，非 ExponentialBackoffRetry 时返回 0。
// 给 /admin/runtime 的重载读数与测试用：调用方不该为了看一个取值去断言接口类型。
func (s *Scheduler) RetryPolicyMaxDelay() time.Duration
```

`main.go` 的调用点不动：仍然在 `newScheduler` 之前用
`&core.ExponentialBackoffRetry{MaxDelay: cfg.Scheduler.MaxRetryDelay}`（`cmd/server/main.go:387-389`）
构造，运行期改由 R06 调 `SetRetryPolicy`。

### 3.3 `core/store.go`：留痕策略可改

```go
// SetHistoryRetention 运行期调整终态快照的保留条数与时长。
// 两项必须一起给：分开调会出现"新条数配旧时长"的中间态，而 history_limit=-1（不留痕）
// 与 history_ttl 的组合语义只在成对时说得清。取值口径与 StoreOptions 完全同一条：
// limit==0 用 DefaultHistoryLimit、limit<0 不留痕、ttl<=0 不按时间淘汰。
func (s *JSONFileStore) SetHistoryRetention(limit int, ttl time.Duration)
```

- `historyLimit` 改 `atomic.Int64`、`historyTTL` 改 `atomic.Int64`（存纳秒），
  `trimTerminalLocked`（`core/store.go:161-189`）与 `trimAfterWriteLocked`（`:133`）里
  所有读取点换成 `Load()`；构造时的补齐逻辑（`:84-93`）改成建好后 `Store()`。
- **`Store` 接口加这个方法**（待拍板 P1）。落点：`core/store.go:25-36` 的接口里，
  注释说明"未启用热重载的调用方不需要实现语义，但必须能编译"——不，这条不要写进注释：
  接口方法就是必须实现，替身一律给真实实现或一个记录调用的桩。
- 已知的接口实现者只有两个测试替身需要补方法：`core/scheduler_test.go` 的 `mockStore`
  （`LoadAll` 在 `:55-60`）、`cmd/server/main_integration_test.go` 的 `stubStore`
  （`LoadAll` 在 `:2045`）。`api/handlers_executors_test.go:76` 是内嵌 `core.Store` 的结构体，
  自动满足，不用改。执行本卡时用
  `go build ./... && go vet ./...` 的失败清单为准逐个补，不要只按本卡列的两处。
- `flush_interval` 不在本卡：它是 `flushLoop` 的 ticker（`core/store.go:204-222`），属重启档。

### 3.4 `store/sqlite`：两个写入器的保留策略

```go
// SetRetention 运行期调整保留条数与时长，下一个批量周期的淘汰用新值。
// 取值口径与 New* 里的补齐完全同一条：count<=0 用各自的默认值
// （DefaultObserveEventRetentionCount / DefaultObserveAuditRetentionCount）、
// age<0 按 0 处理、age==0 表示不按时间淘汰。不许在 setter 里另立一套。
func (e *EventLog) SetRetention(count int, age time.Duration)
func (a *AuditLog) SetRetention(count int, age time.Duration)
```

- `retentionCount` 改 `atomic.Int64`、`retentionAge` 改 `atomic.Int64`（存纳秒），
  `prune` 里两个读取点（`store/sqlite/events.go:225-240`、`store/sqlite/audit.go:213-225`）换成 `Load()`。
- `flush_interval`、`queue_capacity`、`busy_timeout`、`synchronous` 都不在本卡（设计文档 §6.2）。

### 3.5 一张对照表（写进包注释或本卡的实现记录，供 R06 按图索骥）

| R01 的热更键 | 本卡入口 | 生效时机 |
| --- | --- | --- |
| `logging.level` | `core.SetLogLevel(v, level)` | 下一条日志起 |
| `scheduler.max_retry_delay` | `Scheduler.SetRetryPolicy` | 下一次失败重试的延迟计算 |
| `store.history_limit` / `store.history_ttl` | `JSONFileStore.SetHistoryRetention` | 下一次写入触发的 trim |
| `observability.events.retention_*` | `sqlite.EventLog.SetRetention` | 下一个批量周期的 prune |
| `observability.audit.retention_*` | `sqlite.AuditLog.SetRetention` | 下一个批量周期的 prune |

`scheduler.workers` 与 `executors.commands` 不在这张表里，它们分别是 R03 与 R04。

## 4. 实现步骤

1. `core/logging.go`：抽 `NewLoggerWithLevelVar`、加 `SetLogLevel`、`NewLogger` 改为转调。
2. `core/scheduler.go`：`retryPolicy` 换 `atomic.Pointer[RetryPolicy]`，改构造点与读取点，
   加 `SetRetryPolicy` 与 `RetryPolicyMaxDelay`。
3. `core/store.go`：两个原子字段 + `SetHistoryRetention` + 接口加方法；
   按 `go build ./...` 的失败清单给替身补方法。
4. `store/sqlite/events.go`、`store/sqlite/audit.go`：原子字段 + `SetRetention`。
5. 写 §5 的用例，逐包跑绿。
6. 全量 `go build ./... && go vet ./... && go test ./... -race -count=1`。
7. 只对新增文件 `gofmt -w`。

## 5. 测试要求

### 5.1 日志级别

```go
func TestSetLogLevel_ChangesOutput(t *testing.T) {
        var buf bytes.Buffer
        logger, levelVar, err := NewLoggerWithLevelVar("info", "text", &buf)
        if err != nil {
                t.Fatalf("NewLoggerWithLevelVar: %v", err)
        }

        logger.Debug("first")                       // info 级别下不该出现
        if strings.Contains(buf.String(), "first") {
                t.Fatal("debug record emitted at info level")
        }

        if err := SetLogLevel(levelVar, "debug"); err != nil {
                t.Fatalf("SetLogLevel: %v", err)
        }
        buf.Reset()
        logger.Debug("second")
        if !strings.Contains(buf.String(), "second") {
                t.Fatal("debug record still hidden after SetLogLevel")
        }

        // 解析失败保持原值：这一条守住"写错的级别名不会把日志关掉"
        if err := SetLogLevel(levelVar, "verbose"); err == nil {
                t.Fatal("SetLogLevel accepted an invalid level name")
        }
        buf.Reset()
        logger.Debug("third")
        if !strings.Contains(buf.String(), "third") {
                t.Fatal("level changed by a rejected SetLogLevel call")
        }
}

func TestNewLogger_DelegationWritesThrough(t *testing.T) {
        // 只断"转调之后 NewLogger 交出的 logger 仍按解析出的级别过滤"这一件没被测过的事；
        // 非法级别/非法格式/默认 info+text 三条由既有的 TestNewLogger_RejectsUnknownValues
        // 与 TestNewLogger_LevelAndFormat 守着，那两条必须未经修改即过（落地见 §10.2 第 18 条）
        var buf bytes.Buffer
        logger, err := NewLogger("info", "text", &buf)
        require.NoError(t, err)

        logger.Debug("hidden at info level")
        assert.NotContains(t, buf.String(), "hidden at info level")

        logger.Info("kept at info level")
        assert.Contains(t, buf.String(), "kept at info level")
}
```

另外把既有用例跑一遍确认没有断言被 `NewLogger` 的转调改动（`core/logging_test.go` 若不存在，
就在本卡新建，不要为了塞一条用例去改别的包的测试）。

### 5.2 重试策略

在 `core/scheduler_hot_config_test.go` 里：

```go
func TestSetRetryPolicy_SwapsMaxDelay(t *testing.T) {
        store := &mockStore{}
        scheduler := NewScheduler(store, &ExponentialBackoffRetry{MaxDelay: time.Minute}, nil)

        if got := scheduler.RetryPolicyMaxDelay(); got != time.Minute {
                t.Fatalf("initial max delay = %v, want 1m", got)
        }

        scheduler.SetRetryPolicy(&ExponentialBackoffRetry{MaxDelay: 5 * time.Second})
        if got := scheduler.RetryPolicyMaxDelay(); got != 5*time.Second {
                t.Fatalf("max delay after swap = %v, want 5s", got)
        }

        scheduler.SetRetryPolicy(nil) // nil 不改
        if got := scheduler.RetryPolicyMaxDelay(); got != 5*time.Second {
                t.Fatalf("max delay after nil call = %v, want 5s (nil must not clear it)", got)
        }
}
```

行为用例（不是只看读数）：`mockRetryPolicy` 定义在 `core/scheduler_test.go`
（`core/scheduler_recovery_test.go` 里只是使用它），建调度器时给 `delay: 10ms`，跑一次失败重试
确认按 10ms 排期；再 `SetRetryPolicy` 换成 `delay: 30ms` 的策略，跑第二次失败重试确认按 30ms 排期。
排期取值断言容忍 ±1 个 `time.Millisecond` 的抖动，不断言精确时刻
（这条容差是本用例自己留的，不是既有体例——仓内其它重试用例断的是退避算法的抖动区间，
见 `core/retry_test.go:25-30`，与本卡的"排期用的是哪一条策略"不是同一件事）。
基准要取"策略自己被调用的那一瞬间"而不是用例开头的一个 `time.Now()`——`go test ./...` 是各包
并行跑的，一次 CPU 抢占就能把 ±1ms 的窗口整个吃掉。

**既有断言因字段类型必须换读法的四处**（原卡面只列了 `core/scheduler_test.go:124` 一条，
落地时发现直接读这四个待原子化字段的地方一共四处，见 §10.2 第 1 条）：
`core/scheduler_test.go` 比较 `scheduler.retryPolicy` 的两条（同一性比较与 `== nil`）、
`core/store_history_test.go` 的 `TestJSONFileStore_ZeroHistoryLimitUsesDefault`、
`core/config_test.go` 的 `TestConfig_ThreadsIntoComponents`、
`store/sqlite/events_test.go` 的 `TestEventLog_DefaultsForNonPositiveOptions`。
换法一律是把 `field` 改成 `field.Load()`（时长再转回 `time.Duration`），
断言的语义与期望取值一条都不动；`retryPolicy` 那条比较的是 `*s.retryPolicy.Load()` 指向的接口值，
身份语义与改写前一致。这些改写在实现记录 §10.2 里记为有意偏离（不是回归）。

### 5.3 存储留痕

在 `core/store_hot_test.go` 里，全部走**公开写路径**（`Save`/`Update`）而不是直接调
`trimAfterWriteLocked`——那个方法的前置条件是调用方持锁，测试绕过 `Update` 去调它等于测了一个
生产中不存在的形状。helper 复用 `core/store_history_test.go` 里的
`newHistoryStore`（`:13-20`，只写 `t.TempDir()`，不碰仓库 `data/`）、
`terminalSnapshot`（`:22-24`）、`snapshotIDs`（`:26-32`）与 `mustLoadAll`：

```go
func TestSetHistoryRetention_LimitTakesEffectOnNextWrite(t *testing.T) {
        store := newHistoryStore(t, StoreOptions{HistoryLimit: 100})
        base := time.Now()

        // 先写进 5 条终态快照，100 条上限下全部留存
        for i := 0; i < 5; i++ {
                require.NoError(t, store.Update(terminalSnapshot(
                        string(rune('a'+i)), StatusSuccess, base.Add(time.Duration(i)*time.Minute))))
        }
        assert.Len(t, mustLoadAll(t, store), 5)

        // 把上限收到 2：本卡要求的生效时机是"下一次写入触发的 trim"，
        // 所以调完 setter 必须先断言旧的 5 条还在，再写一条才见结果。
        store.SetHistoryRetention(2, 0)
        assert.Len(t, mustLoadAll(t, store), 5, "retention must not trim without a write")

        require.NoError(t, store.Update(terminalSnapshot("f", StatusSuccess, base.Add(5*time.Minute))))
        assert.Len(t, mustLoadAll(t, store), 2, "the new limit must apply to the next trim")
}

func TestSetHistoryRetention_TTLExpiresOnNextWrite(t *testing.T) {
        store := newHistoryStore(t, StoreOptions{HistoryLimit: 100})
        old := time.Now().Add(-48 * time.Hour)

        for i := 0; i < 3; i++ {
                require.NoError(t, store.Update(terminalSnapshot(
                        string(rune('a'+i)), StatusSuccess, old.Add(time.Duration(i)*time.Minute))))
        }
        store.SetHistoryRetention(100, 24*time.Hour)
        require.NoError(t, store.Update(terminalSnapshot("fresh", StatusSuccess, time.Now())))

        // 三条 48h 前的旧记录被新 TTL 淘汰，只剩刚写的那条
        ids := snapshotIDs(mustLoadAll(t, store))
        assert.Len(t, ids, 1)
        assert.True(t, ids["fresh"])
}

func TestSetHistoryRetention_NegativeLimitKeepsNothing(t *testing.T) {
        store := newHistoryStore(t, StoreOptions{HistoryLimit: 100})
        store.SetHistoryRetention(-1, 0)
        require.NoError(t, store.Update(terminalSnapshot("a", StatusSuccess, time.Now())))

        assert.Empty(t, mustLoadAll(t, store), "limit -1 means write-then-delete")
}

func TestSetHistoryRetention_ConcurrentWithTrim(t *testing.T) {
        store := newHistoryStore(t, StoreOptions{HistoryLimit: 50})
        var wg sync.WaitGroup
        wg.Add(2)
        go func() {
                defer wg.Done()
                for i := 0; i < 20; i++ {
                        store.SetHistoryRetention(10+i, time.Duration(i)*time.Hour)
                }
        }()
        go func() {
                defer wg.Done()
                for i := 0; i < 200; i++ {
                        _ = store.Update(terminalSnapshot(strconv.Itoa(i), StatusSuccess, time.Now()))
                }
        }()
        wg.Wait()
        // 终态自洽：读回来的记录数不超过最后一次的目标条数，且没有 panic / race
        assert.Less(t, len(mustLoadAll(t, store)), 31)
}
```

`mustLoadAll` 若已存在于 `core/store_history_test.go` 就直接用；不存在则在本文件里
写一个 `require.NoError(t, err)` 包一层的小 helper，不要复制既有 helper 的名字。

### 5.4 观测层保留策略

`store/sqlite/hot_retention_test.go`，事件与台账各一条，复用包里既有的三个 helper：
`newTestEventLog(t, tune)`（`store/sqlite/events_test.go:21-48`，真库 + 真总线 + `t.Cleanup` 已挂好）、
`waitQueued(t, log, n)`（`:52-63`，等的是条件不是时长）与 `EventLog.Count()`（`store/sqlite/events.go:357`）；
台账侧用 `newTestAuditLog(t, tune)`（`store/sqlite/audit_test.go:20`）与 `AuditLog.Count()`（`:377`）。

```go
func TestEventLog_SetRetentionPrunesNextBatch(t *testing.T) {
        log, db, bus := newTestEventLog(t, func(opts *EventLogOptions) {
                opts.RetentionCount = 1000
        })

        // 六条带 job_id 的事件（总线缓冲是 100，这里六条不会丢）
        for i := 0; i < 6; i++ {
                bus.Publish(core.Event{Type: core.EventJobCompleted,
                        JobID: fmt.Sprintf("job-%d", i), JobName: "task"})
        }
        waitQueued(t, log, 6)
        require.NoError(t, log.Flush())

        count, err := log.Count()
        require.NoError(t, err)
        require.EqualValues(t, 6, count, "six events must be stored before the retention change")

        log.SetRetention(2, 0)
        bus.Publish(core.Event{Type: core.EventJobCompleted, JobID: "job-6", JobName: "task"})
        waitQueued(t, log, 1)
        require.NoError(t, log.Flush())

        count, err = log.Count()
        require.NoError(t, err)
        assert.EqualValues(t, 2, count, "the next batch's prune must use the new count")
        _ = db
}

func TestEventLog_SetRetentionZeroFallsBackToDefault(t *testing.T) {
        log, _, bus := newTestEventLog(t, func(opts *EventLogOptions) {
                opts.RetentionCount = 3
        })
        // 0 不是"不限量"，而是"回到 DefaultObserveEventRetentionCount"——
        // 这条口径与 NewEventLog 里的补齐（store/sqlite/events.go:93-101）必须一致，
        // 否则 setter 就成了第二套规则。
        log.SetRetention(0, -time.Hour)
        for i := 0; i < 5; i++ {
                bus.Publish(core.Event{Type: core.EventJobCompleted,
                        JobID: fmt.Sprintf("job-%d", i), JobName: "task"})
        }
        waitQueued(t, log, 5)
        require.NoError(t, log.Flush())

        count, err := log.Count()
        require.NoError(t, err)
        assert.EqualValues(t, 5, count, "count<=0 falls back to the default (200000), age<0 to 0")
}
```

台账那条同构成 `TestAuditLog_SetRetentionPrunesNextBatch`：

```go
func TestAuditLog_SetRetentionPrunesNextBatch(t *testing.T) {
        log, db := newTestAuditLog(t, func(opts *AuditLogOptions) {
                opts.RetentionCount = 1000
        })

        for i := 0; i < 6; i++ {
                require.NoError(t, log.Append(auditEntry(i)))
        }
        waitAuditQueued(t, log, 6)                 // 与 waitQueued 同形；台账侧若已有同名 helper 就用既有的
        require.NoError(t, log.Flush())

        count, err := log.Count()
        require.NoError(t, err)
        require.EqualValues(t, 6, count)

        log.SetRetention(2, 0)
        require.NoError(t, log.Append(auditEntry(6)))
        waitAuditQueued(t, log, 1)
        require.NoError(t, log.Flush())

        count, err = log.Count()
        require.NoError(t, err)
        assert.EqualValues(t, 2, count, "the next batch's prune must use the new count")
        _ = db
}
```

台账侧的行构造**直接复用 `store/sqlite/audit_test.go` 里既有的 `auditEntry(seq)`**（它是包内
共用的 `api.AuditEntry` 构造器，字段集是 actor、actor kind、role、action、method、route、
status、latency、verdict 那一组；卡面早先写的 `auditEntryFixture(i)` 与"method、path、result"
都不存在，见 §10.2 第 4 条），不要自造字段组合；
队列等待的 helper 若台账侧还没有，就照 `waitQueued`（`store/sqlite/events_test.go:52-63`）
的形状写一个，同样等条件而不是 sleep。
两条都要再断言一次 `SetRetention(0, -1)` 回到"用默认条数、不按时间淘汰"的口径
（count<=0 走默认、age<0 按 0），否则"setter 另立一套"这件事没人能发现。

### 5.5 手工

`go test ./core ./store/sqlite -race -count=3`。本卡没有调用入口，不该起进程冒烟——
冒烟统一留到 R06 的接线之后（R07 逐场景实测）。

## 6. 完成标准（DoD）

- [x] 六个入口全部存在且被 §5 的用例证明改变了行为，而不只是改了字段值：
      `SetLogLevel`、`SetRetryPolicy`、`SetHistoryRetention`、`EventLog.SetRetention`、
      `AuditLog.SetRetention`（`RetryPolicyMaxDelay` 是读数口，不单独算一条）。
- [x] `NewLogger` 的签名与既有行为不变；`NewLoggerWithLevelVar` 与它共用解析与 handler 构造
      （实现上只有一份 `text|json` 分岔）。
- [x] `Store` 接口新增 `SetHistoryRetention`，`go vet ./...` 绿——即所有实现者都补齐了。
      注意判据要取 `go vet ./...`（或 `go test` 的编译阶段）而不是 `go build ./...`：
      接口的新实现者全在 `_test.go` 里，`go build` 根本不编译它们，绿不说明补没补齐。
      补齐清单写进 §10.1。
- [x] 非法输入不改现值：`SetLogLevel("verbose")` 返回错误且级别保持；
      `SetRetryPolicy(nil)` 返回"不改"而不是清空。
- [x] 三处换成原子字段的读取点全部改完，`-race -count=3` 干净，
      且 §5.3 的并发用例覆盖"写策略 / 读策略"交错。
- [x] 未启用热重载的行为零变化：本卡不改任何构造函数的默认取值、不改任何既有日志文案，
      既有测试只有 §5.2 列出的那四处因字段类型而换读法（断言语义与期望取值不变），其余用例未经修改即通过。
- [x] `go build ./...`、`go vet ./...`、`go test ./... -race -count=1` 全绿；新增文件已 `gofmt -w`。

## 7. 验收方式

```bash
go test ./core -run 'TestSetLogLevel|TestNewLogger|TestSetRetryPolicy|TestRetryPolicyMaxDelay|TestSetHistoryRetention' -v
go test ./store/sqlite -run 'SetRetention' -v
go test ./core ./store/sqlite -race -count=3
go build ./... && go vet ./...
go test ./... -race -count=1
```

预期：全部 `ok`；第二条若显示 `no tests to run` 说明用例名前缀没对上，要改卡或改用例名，
不许留一条永远抓不到用例的验收命令。

## 8. 不在本任务范围

- 不起停任何 worker 协程（R03）。
- 不动 `executors.commands` 的加载与登记（R04）。
- 不建监听器、不碰 `cmd/server` 的重载链（R05、R06）。
- 不给 `SetEventPreviewLimit`、`SetConcurrency`、`SetQueueCapacity`、`SetExecConcurrency`
  加运行期能力：它们既有的"Start 之后调用则 warn 并忽略"口径原样保留（设计文档 §3 的盘点行）。
- 不热更 `logging.format`、`store.flush_interval`、`observability.flush_interval`、
  `observability.queue_capacity`（都属重启档）。

## 9. 风险与回滚

| 风险 | 说明 | 处置 |
| --- | --- | --- |
| 接口加方法打断构建 | `Store` 的实现者散落在三个包的测试里 | 以 `go build ./...` 的失败清单为准逐个补，不靠本卡列的名单 |
| 原子化后补齐口径漂移 | `limit==0 → DefaultHistoryLimit` 这类补齐原本只在构造时做一次，setter 里再写一遍容易漂 | §3.3/§3.4 明确要求"与 `StoreOptions`/`New*` 同一条口径"，并用 §5.4 的 `SetRetention(0,-1)` 用例反证 |
| 只改字段没改行为 | 换了原子字段但 trim/prune 的读取点漏改，测试仍绿（读到旧值） | §5 每条用例都断言"调用前后的行为差别"，不断言字段值本身 |
| 日志级别改坏现网可读性 | `SetLogLevel` 若解析失败就置默认值，会把正在跑的 debug 关掉 | §3.1 定死"解析失败保持原值"，§5.1 第三条断言守它 |
| 与档位在线管理系列的编号撞号 | 本系列编号 D-01xx…D-07xx 与该系列的重号问题（其 D-0902）已知 | 引用缺陷时一律带系列名"配置热重载系列" |

回滚：本卡六个入口都没有调用方，退回等于删掉新增方法与测试文件、把三处原子字段还原成普通字段、
把 `Store` 接口的这一行删掉并还原两个替身。既有行为不受影响，可以整卡 `git revert`。

## 10. 实现记录（执行时补写）

### 10.1 落地的接口与替身补齐清单

按符号名定位，不写行号（后续卡会继续改这些文件）。

`core/logging.go`：

- `NewLoggerWithLevelVar(level, format string, w io.Writer) (*slog.Logger, *slog.LevelVar, error)`
- `SetLogLevel(v *slog.LevelVar, level string) error`
- `NewLogger` 改为转调上面那条并丢弃载体：签名、三条既有行为（非法级别报错、非法格式报错、
  默认 info/text）都不变；`text|json` 分岔仍只有 `NewLoggerWithLevelVar` 里那一份。

`core/scheduler.go`：

- `Scheduler.retryPolicy` → `atomic.Pointer[RetryPolicy]`
- `NewScheduler`：结构体字面量之后一次性 `Store`
- `(*Scheduler).SetRetryPolicy(p RetryPolicy)`（`p == nil` 视为不改）
- `(*Scheduler).RetryPolicyMaxDelay() time.Duration`（非 `*ExponentialBackoffRetry` 返回 0）
- `(*Scheduler).handleFailure`：全仓**唯一**的生产读取点，改成 `Load()` 一次进局部变量

`core/store.go`：

- `Store` 接口新增 `SetHistoryRetention(limit int, ttl time.Duration)`
- `JSONFileStore.historyLimit` / `historyTTL` → `atomic.Int64`（时长存纳秒）
- `(*JSONFileStore).SetHistoryRetention`
- `historyRetentionValues(limit int, ttl time.Duration) (int64, int64)`：构造与 setter 共用的
  同一条补齐口径
- `(*JSONFileStore).trimTerminalLocked`：开头各 `Load()` 一次

`store/sqlite/events.go`：`EventLog.retentionCount` / `retentionAge` → `atomic.Int64`、
`(*EventLog).SetRetention`、`eventRetentionValues`、`(*EventLog).prune` 各 `Load()` 一次。
`store/sqlite/audit.go`：同形（`AuditLog.retentionCount` / `retentionAge`、
`(*AuditLog).SetRetention`、`auditRetentionValues`、`(*AuditLog).prune`）。

替身补齐清单（判据取 `go vet ./...`——接口的新实现者全在 `_test.go` 里，`go build` 不编译它们）：

- `core/scheduler_test.go` 的 `mockStore`：空实现
- `cmd/server/main_integration_test.go` 的 `stubStore`：空实现
- `api/handlers_executors_test.go` 那个内嵌 `core.Store` 的结构体自动满足，未改

新增测试文件：`core/scheduler_hot_config_test.go`、`core/store_hot_test.go`、
`store/sqlite/hot_retention_test.go`；增补用例的既有文件：`core/logging_test.go`。

热更键 → 本卡入口 → 生效时机（§3.5 要求的那张对照表，R06 按这张表接线；
**本卡之后不得再新增第七个入口**，新键要么复用它、要么另开一张卡）：

| R01 的热更键 | 本卡入口 | 生效时机 |
| --- | --- | --- |
| `logging.level` | `core.SetLogLevel(v, level)` | 下一条日志起 |
| `scheduler.max_retry_delay` | `core.Scheduler.SetRetryPolicy` | 下一次失败重试的延迟计算 |
| `store.history_limit` / `store.history_ttl` | `core.JSONFileStore.SetHistoryRetention` | 下一次写入触发的 trim |
| `observability.events.retention_count` / `retention_age` | `sqlite.EventLog.SetRetention` | 下一个批量周期的 prune |
| `observability.audit.retention_count` / `retention_age` | `sqlite.AuditLog.SetRetention` | 下一个批量周期的 prune |

`scheduler.workers` 属 R03、`executors.commands` 属 R04，都不在这张表里。

### 10.2 与本卡写法的差异

1. **既有断言因字段类型换读法的地方是四处，不是卡面说的一处**。卡 §5.2 原先只列了
   `core/scheduler_test.go:124`，§6 DoD 也跟着写"只有那一条"；落地时 grep 直接读这四个待原子化
   字段的地方，一共四处：`core/scheduler_test.go` 比较 `scheduler.retryPolicy` 的两条
   （同一性比较与 `== nil`）、`core/store_history_test.go` 的
   `TestJSONFileStore_ZeroHistoryLimitUsesDefault`、`core/config_test.go` 的
   `TestConfig_ThreadsIntoComponents`、`store/sqlite/events_test.go` 的
   `TestEventLog_DefaultsForNonPositiveOptions`。换法一律 `field` → `field.Load()`
   （时长再转回 `time.Duration`），断言语义与期望取值一条没动；`retryPolicy` 那条比的是
   `*s.retryPolicy.Load()` 装着的接口值，身份语义与改写前一致。**卡 §5.2、§6 与"涉及文件"
   清单已就地改正**（补上三个被点名改写的既有测试文件）。
   唯一非取值变动是 `store/sqlite/events_test.go` 里那条测试失败消息的格式动词
   （`%v` → `%d`，因为读回来的是 `int64`），不是生产日志文案。
2. **§5.4 的台账行构造器卡面写的是不存在的东西**：`auditEntryFixture(i)` 全仓没有，
   包内既有的共用构造器叫 `auditEntry(seq)`（`store/sqlite/audit_test.go:49`），字段集是
   actor、actor kind、role、action、method、route、status、latency、verdict 那一组；
   卡面原写的"必填字段：actor、actor kind、method、path、result"里
   `path`/`result` 两个字段在 `api.AuditEntry`（`api/audit.go`）上根本不存在。
   落地复用 `auditEntry(seq)`，**卡面已就地改正**。
3. **§5.2 的行为用例没有复用 `mockRetryPolicy`**。卡一面要求"基准取策略自己被调用的那一瞬间"，
   一面要求复用 `mockRetryPolicy`——两者不能同时满足：它只回一个 `time.Now().Add(delay)`，
   不记录"自己被问的那一刻"，拿它就没法把排期差值算在同一次取时之上。落地在本测试文件里
   新增 `recordingRetryPolicy`（形状与 `mockRetryPolicy` 一致，多一个 `calledAt` 列表）。
   顺带：卡 §5.2 原写 `mockRetryPolicy` 定义在 `core/scheduler_recovery_test.go:162`，
   实际定义在 `core/scheduler_test.go:114`，那一行只是使用点——卡面已改正。
4. **行为用例直接调 `handleFailure`，不端到端跑 `Start` → dispatch**。本卡要证的只有
   "排期读的是哪一条策略"，端到端会引入 worker 时序与队列这两个与本卡判据无关的变量；
   仓内既有那类排期/失败归因用例也是直调这个方法（`core/scheduler_permanent_test.go:180`、
   `:247`、`:265`），本卡与它们同一体例。
5. **`quietLogger()` 的理由卡面写错了，注释也一度写错了**。卡 §5.2 说丢掉那条 warn 是为了
   不让 stderr 的耗时吃掉 ±1ms 窗口——**因果不成立**：`calledAt` 与副本 `TriggerAt` 都在
   那条 warn 之前算完，写不写日志都改变不了两者之差。真实的理由只有"并发那条要跑 200 轮，
   200 条 warn 会把 `-v` 的输出冲掉"。测试里的注释已按真实理由重写（不保留一个听着更"硬"的
   错误理由）。
6. **`SetLogLevel` 对 nil 载体返回错误而不是 panic**：卡没规定这一态。选返回错误是因为载体
   只可能来自 `NewLoggerWithLevelVar`，传 nil 只能是 R06 接线疏漏，而重载链要把疏漏收成一次
   普通失败（`ReloadFailed`），不该在写日志的半路把进程崩掉。新增
   `TestSetLogLevel_NilCarrierIsRejected` 一条。错误文案按 `core/logging.go` 既有体例
   不带包名前缀（该文件里 `parseLogLevel` 与 handler 构造的两条错误都不带）。
7. **三个 setter 的参数形状不同，卡 §3.3 那句自问自答按后半句落地**：`SetRetryPolicy` 的参数是
   接口值，"没取到值"有 nil 这一态，所以按 §3.2 要"视为不改"；`SetHistoryRetention` /
   `SetRetention` 的参数是 `int` + `Duration`，没有"没取到值"这一态，`0` 与负数一律按
   §3.3/§3.4 的补齐口径折算。因此 `Store` 接口那条注释只写取值口径，不写"未启用热重载的
   调用方不需要实现语义"这种话（卡 §3.3 自己也把这句否掉了）。
8. **补齐口径抽成三个共用函数**（`historyRetentionValues` / `eventRetentionValues` /
   `auditRetentionValues`）。卡只说"与构造时同一条口径"，没说形状；不抽出来就是构造与 setter
   各写一遍判断，正是要防的"setter 成了第二套规则"。
9. **两个保留值在淘汰入口各 `Load()` 一次留在局部变量**（卡只要求"换成 `Load()`"，没要求这条）。
   理由**不是**防撕裂：`trimTerminalLocked` 由调用方持 `s.mu`、`prune` 跑在单个批量落盘协程里，
   读侧本来就不会与自己的两次读交错。真实理由是"同一次淘汰的『按几条切』与『按多久切』必须
   来自同一代策略"。首版注释写的是"否则『是否超限』与『从哪里切』用了两个不同的上限"，
   而代码里条数只读了一次，那句是在解释一个不存在的读法——已改成上面这条真实理由。
10. **两个测试替身的"记录调用"字段被删掉**。首版给 `mockStore` / `stubStore` 各加了
    `retentionLimit` / `retentionTTL` / `retentionCalls` 三条字段，复核确认全仓没有读取点
    （`mockStore` 那条甚至从不自增），属于替 R06 预留的死状态，改成空实现并在注释里指向
    真正验证淘汰行为的地方。R06 若要用替身断"调到没调到"，在那张卡里按需加。
11. **并发用例的判据补强**。`TestSetRetryPolicy_ConcurrentWithFailureHandling` 首版的唯一判据是
    "-race 没报错"，一条都不断。补了尾部一段：并发写完之后设一条已知策略、再走一次失败重试，
    断排期等于它——证明 200 轮并发写之后读取点仍拿得到最近一次写入的策略。
12. **`RetryPolicyMaxDelay` 的读数形状**：卡 §3.2 只说"非 `ExponentialBackoffRetry` 返回 0"，
    落地还要处理"字段从没 `Store` 过"（零值 `Scheduler`）这一态，返回 0 而不是解引用空指针；
    这条没有独立用例（见 §10.6）。

**质量复核轮（第 13~22 条）**：上一轮 3 条 Important + 9 条 Minor 的处置，逐条落地如下。

13. **三条"分不开两种实现"的用例改成断存进去的取值**（复核的 I-3）。
    `TestSetHistoryRetention_ZeroLimitFallsBackToDefault`、
    `TestEventLog_SetRetentionZeroFallsBackToDefault`、
    `TestAuditLog_SetRetentionZeroFallsBackToDefault` 原先只断"5 条都留着"，
    而"回落到默认条数"与"当成不限量"两种实现下这个观察都成立，判据空转。
    现在各自再断 `historyLimit.Load()` / `retentionCount.Load()` / `retentionAge.Load()` 等于
    回落后的那个数——读写私有字段在同包既有用例里已有先例（`core/store_history_test.go` 的
    `TestJSONFileStore_ZeroHistoryLimitUsesDefault`、`store/sqlite/events_test.go` 的
    `TestEventLog_DefaultsForNonPositiveOptions），而"不许读私有字段"这条只适用于
    卡 §5.3 说的"别绕过 `Update` 去直调 `trimAfterWriteLocked`"，不适用于读数。
    **顺带把 §10.3 里那条等价变异 N5 变成了真判据**：负时长不再折 0 的变异现在会判红。
14. **两条并发用例的终态判据改成"自己再收一次"**（复核的 I-1）。
    `TestSetHistoryRetention_ConcurrentWithTrim` 原先断"记录数 < 31"：31 这个上界没有出处
    （setter 写的是 10..29），而且 setter 协程若被推迟到写入循环收尾，末次 trim 用的仍是
    构造时的 50 条——判据落在两个协程谁被调度到上面，不在被测语义上。
    现在 `wg.Wait()` 之后自己 `SetHistoryRetention(3, 0)` 再写一条，断剩下的条数 ≤ 3；
    事件侧那条同形用例本来就是这种写法，两处口径一致了。
    同一处还改了 `_ = store.Update(...)`：写失败会让终态判据 trivially 通过，现在改成
    `if err := ...; err != nil { t.Errorf(...); return }`。
15. **`TestSetRetryPolicy_ConcurrentWithFailureHandling` 的判据写实**（复核的 I-2）。
    尾段"设一条已知策略再跑一次失败重试"并不能证明跨协程可见性——写与读都在主协程，
    程序序天然可见。改法两段：
    其一，每轮读数收进切片，`wg.Wait()` 之后断每个读数都落在"构造时那条 1s"或
    "某一轮写进去的 1..200ms"之内（读到 0、负数、从没写过的值就说明读写没走原子语义）；
    其二，尾段只断"最后一次写入读得出来"。跨协程可见性本身的判据如实写成 `-race`，
    注释里说明为什么不能断"一定看到过 ≥2 个不同值"（读数落在哪一代取决于调度）。
16. **"只 Load 一次"这条注释主张补上了判据**（复核的 M-5 引申）。
    `assertScheduledDelay` 增加 `wantCalls` 参数，断一次失败只把策略问一次。
    加这条是因为 `handleFailure` 里"发事件用的时刻"与"真正排下来的时刻"必须来自同一次
    `Load()`，而分两次 `Load()` 在只断读数的用例下完全看不出来——变异 N8 现在能判红。
17. **±1ms 容差的出处卡面写错了，卡面与注释同步改正**（复核的 M-5）。
    卡 §5.2 原写"不断言精确时刻（本仓既有重试用例的口径）"，实跑核过：仓内其它重试用例
    断的是退避算法的抖动区间（`core/retry_test.go:25-30`），与本卡的"排期用的是哪一条策略"
    不是同一件事，全仓此前没有 ±1ms 这条体例。现在注释与卡面都写明这条容差是本用例自己留的。
18. **`TestNewLogger_KeepsOldContract` 换成 `TestNewLogger_DelegationWritesThrough`**（复核的 M-6）。
    前者的三条断是既有的 `TestNewLogger_RejectsUnknownValues`（非法级别/非法格式，输入字面相同）
    与 `TestNewLogger_LevelAndFormat`（默认 info+text）的子集，属于把既有覆盖抄一遍。
    现在只断一件没被测过的事：`NewLogger` 丢掉载体之后交出去的那条 logger 仍按解析出的级别过滤
    （转调若把 handler 的级别写成常量，这里会红）。非法级别/格式与默认值那三条由既有用例守，
    那两条未经修改即过，这本身就是 §5.1 想要的"转调没改行为"的证据。
    同时把 §5.1 给的两条新用例从 `if/t.Fatal` 改成同文件在用的 testify 写法（断言一条没减）。
19. **`waitAuditQueued` 与既有 `waitQueued` 两份同构保留**（复核的 M-7）。
    `batcher[T]` 是泛型，合成一份要把既有 `waitQueued` 的签名改掉，超出"本卡不改既有测试"的边界，
    而卡 §5.4 明写"台账侧若还没有就照 `waitQueued` 的形状写一个"。
    同一处能改的先改了：台账侧的落盘从 `require.NoError(t, log.Flush())` 换成包内既有的
    `mustFlush(t, log)`，注释里"收到 3 条"的错字改成"收成 3 条"。
20. **`core/scheduler_test.go` 的改写处不再一个表达式里 `Load()` 两次**（复核的 M-9）。
    首版写成 `if scheduler.retryPolicy.Load() == nil || *scheduler.retryPolicy.Load() != retryPolicy`，
    恰好是本卡在生产侧立起来的"读一次进局部变量"那条写法的反例，改成 `if loaded := ...; loaded == nil || *loaded != ...`。
21. **三个新测试文件的构词不一致保留**（复核的 M-10）：`scheduler_hot_config` / `store_hot` /
    `hot_retention` 三种形状都是卡"涉及文件"里点名的名字，本卡不自作改名。
22. **变异 N1/N6 首版是"编译失败"型，换成可编译的变异重跑**：删掉 `if err != nil { return err }`
    会让 `err` 没人用、删掉 nil 守卫会让 `errors` 导入没人用，两种都停在构建阶段而到不了断言。
    N1b 改成"解析失败也 `v.Set`，但仍然返回错误"，N6b 改成"守卫不再拦 nil"，
    两条都真跑到断言/panic 上判红（见 §10.3）。
    判定为**不改**的两条：`eventRetentionValues` 与 `auditRetentionValues` 两份同构不合并
    （默认常量不同、选项类型不同，各 8 行紧邻各自的构造函数，重复比抽象便宜）；
    `TestSetHistoryRetention_NegativeLimitKeepsNothing` 与既有 `store_history_test.go:72`
    看着重复但留（同机制、不同入口，本卡要证的正是 setter 这条路）。

### 10.3 验证证据

卡 §7 的验收命令，终态实跑结果（终态 = §10.2 第 13~22 条改动之后）：

1. `go test ./core -count=1 -v -run 'TestSetLogLevel|TestNewLogger|TestSetRetryPolicy|TestRetryPolicyMaxDelay|TestSetHistoryRetention'`：
   **15 条 `--- PASS`、0 条 FAIL**——其中 12 条是本卡新增（`TestSetLogLevel_ChangesOutput`、
   `TestSetLogLevel_NilCarrierIsRejected`、`TestNewLogger_DelegationWritesThrough`、
   `TestSetRetryPolicy_SwapsMaxDelay`、`TestRetryPolicyMaxDelay_NonBackoffPolicyReadsZero`、
   `TestSetRetryPolicy_ChangesScheduledDelay`、`TestSetRetryPolicy_ConcurrentWithFailureHandling`、
   `TestSetHistoryRetention_LimitTakesEffectOnNextWrite`、`_TTLExpiresOnNextWrite`、
   `_NegativeLimitKeepsNothing`、`_ZeroLimitFallsBackToDefault`、`_ConcurrentWithTrim`），
   另外 3 条是名单里的既有用例
   （`TestNewLogger_LevelAndFormat`/`_LevelFiltering`/`_RejectsUnknownValues`）**未经修改即过**。
   按 §10.2 第 1 条换了读法的那两条 `TestNewScheduler`/`TestNewScheduler_DefaultRetryPolicy`
   不在这个 `-run` 名单里，另行跑过：`go test ./core -count=1 -v -run 'NewScheduler'`
   两条 `--- PASS`、`ok godelayq/core`。
2. `go test ./store/sqlite -count=1 -v -run 'SetRetention'`：**5 条 `--- PASS`、0 条 FAIL**
   （事件三条 + 台账两条）。
3. `go test ./core ./store/sqlite -race -count=3 -timeout 30m`：
   `ok godelayq/core 34.865s`、`ok godelayq/store/sqlite 12.951s`。
   两条并发用例另按复核意见压过一遍：`go test ./core -count=50 -run 'TestSetHistoryRetention_ConcurrentWithTrim|TestSetRetryPolicy'`
   与 `go test ./store/sqlite -count=30 -run 'SetRetentionConcurrent'` 都 `ok`。
4. `go build ./... && go vet ./...`：无输出（`Store` 接口的实现者全在 `_test.go`，
   补齐判据取这条而不是 `go build`）。
5. `go test ./... -race -count=1 -timeout 30m`：`api` 100.043s、`cmd/server` 5.699s、
   `core` 12.397s、`executor` 22.318s、`store/sqlite` 4.020s，五包全 `ok`，其余 `[no test files]`。

变异反验证：把每处新语义改回旧写法，跑对应用例判红，再从字节副本恢复。八条全部判红。

| 编号 | 变异 | 期望判红 | 实际 |
| --- | --- | --- | --- |
| N1 | `SetLogLevel` 解析失败时也把级别写进载体（仍返回错误） | `TestSetLogLevel_ChangesOutput` | 红，`"" does not contain "third"` |
| N2 | `SetRetryPolicy` 只判 nil、不 `Store`（空操作） | `TestSetRetryPolicy_SwapsMaxDelay` + `_ChangesScheduledDelay` | 两条都红 |
| N3 | `SetHistoryRetention` 只写条数、不写时长 | `TestSetHistoryRetention_TTLExpiresOnNextWrite` | 红，`map[a b c fresh] should have 1 item(s), but has 4` |
| N4 | `eventRetentionValues` 去掉 `count<=0` 的补齐 | `TestEventLog_SetRetentionZeroFallsBackToDefault` | 红，两条一起报：存进去的条数不等于默认值、5 条没留住 |
| N5 | `auditRetentionValues` 不再把负时长折成 0 | `TestAuditLog_SetRetentionZeroFallsBackToDefault` | 红，`age<0 必须折成 0，即不按时间淘汰` |
| N6 | `SetLogLevel` 的 nil 守卫不再拦 nil | `TestSetLogLevel_NilCarrierIsRejected` | 红，`panic: invalid memory address or nil pointer dereference` |
| N7 | `historyRetentionValues` 去掉 `limit == 0` 的回退 | `TestSetHistoryRetention_ZeroLimitFallsBackToDefault` | 红，两条一起报：存进去的不是 `DefaultHistoryLimit`、`"[]" should have 5 item(s), but has 0` |
| N8 | `handleFailure` 分两次 `Load()`（问两次策略才算一次排期） | `TestSetRetryPolicy_ChangesScheduledDelay` | 红，`retry policy consulted 2 times, want 1` |

三点要记在这里：

- **N1 与 N6 的首版是"编译失败"型变异**（删掉 `if err != nil { return err }` 会让 `err` 没人用，
  删掉 nil 守卫会让 `errors` 导入没人用），停在构建阶段根本跑不到断言。上表记的是改成可编译形态
  （N1b/N6b）之后的结果，见 §10.2 第 22 条。
- **N5 首轮曾是等价变异**：`prune` 的时间淘汰判据是 `retentionAge <= 0`，负数与 0 在这条判据下
  同义，所以只断行数的用例分不开。改成断"存进去的取值"之后（§10.2 第 13 条）它才有判据，
  上表记的是那之后的结果。
- **N8 是本轮新增的判据类型**：`handleFailure` 里"只 `Load()` 一次"是注释主张，
  单看排期读数分不出问了一次还是两次，靠 `assertScheduledDelay` 的 `wantCalls` 才钉住（§10.2 第 16 条）。

八条变异跑完后 `core/logging.go`、`core/scheduler.go`、`core/store.go`、
`store/sqlite/events.go`、`store/sqlite/audit.go` 与变异前的字节副本逐字节相同。

新增与改动的 Go 文件都过了显式 `gofmt -w`；`gofmt -l` 在本仓因 CRLF 会对既有文件全量误报
（`core/cron.go` 等六个既有文件在列），本卡新加的三个测试文件不在列。

### 10.4 手工验收

本卡不起进程冒烟（卡 §5.5）：六个入口一个都没有调用方，接线在 R06，端到端场景由 R07 逐条实测。
手工只核了文件面与文案面两条，**没有跑 `go run ./cmd/server`**——本仓工作树的
`data/jobs.json` 与 `data/groups.json` 是别人正在改的未提交文件，起进程就会去写它们；
真要起进程得照系列体例在 `%TEMP%` 里造一份隔离配置，那一步连同"改文件触发一次重载"一起
留给 R06/R07（那两张卡才有调用方可以观测）。

- `git status --short` 逐个核：`cmd/server` 侧只动了测试替身所在的 `main_integration_test.go`，
  `main.go`、`api/`、`executor/` 零改动——"默认关闭时行为零变化"在文件面上成立。
- 启动日志文案没有新增或改写：`go diff` 里 `core/` 与 `store/sqlite/` 的生产改动没有一处
  新增/修改 `logger.Info`/`Warn`/`Error` 的字面量（唯一的文案变动在测试失败消息里，见 §10.2 第 1 条），
  而 `NewLogger` 的三条既有行为由 `core/logging_test.go` 里**未经修改**的
  `TestNewLogger_LevelAndFormat`/`_LevelFiltering`/`_RejectsUnknownValues` 三条用例继续守着。

**补做的冒烟（2026-10-03，用户要求"检测核心功能是否正常使用"）**

上面那句"本卡不起进程冒烟"是卡面口径，实测另补了两层：一层是把 R01 的判据与 R02 的六个入口
串起来的测试内冒烟，一层是真起进程的冒烟。

1. **测试内冒烟：`cmd/server/config_reload_smoke_test.go`（新增文件，7 条用例全绿）**。
   放在 `cmd/server` 是因为只有这一层能同时 import `core`、`store/sqlite` 与 `api`
   （`core` 不许反向依赖）。文件里自带一个 miniature 重载器 `applyCandidate`：
   读配置 → `core.Diff` → 有拒绝项就整次作废 → 逐键调 §10.1 那张表上的入口 → 成功后才推 `applied`。
   **这张表在这里第一次被当成接口使用**，签名对不上就编译不过；R06 接线之后
   `applyCandidate` 应被生产重载链替换，场景清单留下。
   七条用例与判据：
   - `TestSmokeHotReloadAppliesAllSixEntries`：一次改全部六个热更键（级别、重试上限、留痕条数、
     事件保留条数、台账保留条数），逐条断行为——debug 记录写出来了、`RetryPolicyMaxDelay` 变 5s、
     留痕在下一次写入后从 4 条剪到 2 条、`job_events` 与 `write_audit` 在下一个批量周期各剪到 2 行。
   - `TestSmokeRejectedKeyAbortsWholeReload`：混进 `server.auth.token` 时整次作废，
     本来会生效的级别与留痕上限都没变，`applied` 也没往前推（不变量 I1+I2）。
   - `TestSmokeRestartKeysAreReportedNotApplied`：只改重启档时一条都不应用，
     但两条键名都出现在 `ignored_keys` 里（不变量 I3：静默就是缺陷）。
   - `TestSmokeUnchangedConfigIsNotAReload`：同一份配置比自身 → `unchanged`，一个键都不报。
   - `TestSmokeHotKeyWithoutEntryFailsLoud`：表里没有入口的热更键（`scheduler.workers`，属 R03）
     返回带键名的错误，而不是"改了文件、什么都没发生"。
   - `TestSmokeBrokenConfigKeepsRunning`：坏 YAML 被 `UnmarshalExact` 拒掉之后，
     运行面继续按旧配置工作（info 出、debug 挡、存储照常留痕）。
   - `TestSmokeEnvOverrideReachesTheSameEntries`：`GODELAYQ_LOGGING_LEVEL` 与
     `GODELAYQ_SCHEDULER_MAX_RETRY_DELAY` 覆盖进来后走同一条应用路径并落到运行面。
   四条针对冒烟具本身的变异（把入口调用作废、去掉拒绝拦截、失败也推 `applied`、
   把重启档当热更应用）全部判红，恢复后文件与备份逐字节相同——即这七条不是"只跑一遍不判对错"。
   `go test ./cmd/server -count=3 -race -run TestSmoke` 干净（2.991s）。
2. **进程冒烟**：`go build -o %TEMP%/r02smoke/server.exe ./cmd/server`，
   在 `%TEMP%/r02smoke` 里放一份最小配置（`reload.enabled: false`），存储与观测层路径全部用
   `GODELAYQ_STORE_PATH`/`GODELAYQ_STORE_GROUPS_PATH`/`GODELAYQ_OBSERVABILITY_PATH`
   指到该目录，仓库的 `data/` 一次都没被写。观察到的事实：
   `GET /api/v1/health` 200 `{"status":"healthy"}`；`POST /api/v1/jobs`（`payment_check`，
   `delay: 10m`，带重试参数）201 回 id，`GET /api/v1/jobs` 列出这一条；
   `data/jobs.json` 与 `data/observe.sqlite`（含 -wal）在该目录里落出来；
   启动日志一条 `observability enabled`，**整份日志里 `reload` 出现 0 次**——
   即 R01+R02 落地之后，未接线状态下确实没有任何重载行为。
   停止用的是 `taskkill /F`（强杀），所以优雅关闭不在这层冒烟的观察范围内，
   那部分由 `cmd/server` 既有的 `TestRun_*` 用例覆盖。

加入这个文件之后的全量复跑：`go build ./... && go vet ./...` 无输出；
`go test ./... -race -count=1 -timeout 30m` 为 `api` 191.721s、`cmd/server` 6.369s、
`core` 12.937s、`executor` 23.484s、`store/sqlite` 4.210s，五包全 `ok`。

### 10.5 缺陷

| 编号 | 内容 | 处置 |
| --- | --- | --- |
| **D-R0201** | `SetHistoryRetention` 与两个 `SetRetention` 都是**两次独立 `Store`**（条数一次、时长一次），两者之间存在"新条数配旧时长"的瞬时窗口。卡 §3.3 要求"两项必须一起给"，落地做到的是一次调用收两个值、同一次淘汰只读一代策略，**没做到**"两条原子值一次性换" | **登记不修**：读侧已在淘汰入口各读一次留局部变量（§10.2 第 9 条），交错最坏是让某一次 trim 用了一套没人配置过的组合，不改变任何既有语义，也不会有 -race 报告。真要收口就得把两项装进一个 `atomic.Pointer[retentionPair]` 整体换，代价是给三处各加一个只有两个整数的结构体。归 **R06**：接线时若发现日志/读数把这种中间态暴露给用户（例如 `/admin/runtime` 把条数与时长分两次读出来展示），再改成整体换 |
| **D-R0202** | `RetryPolicyMaxDelay` 只认 `*ExponentialBackoffRetry`，自定义策略（第三方装配方、测试替身）一律读 0。R06 若直接把它当"当前重试上限"显示，会在自定义策略下显示成一个像是"没限制"的 0 | **登记不修**：卡 §3.2 就是这个口径（"非 `ExponentialBackoffRetry` 时返回 0"），本卡不改成返回 `(time.Duration, bool)` 以免在没接线的情况下先定下读数形状。归 **R06**：`/admin/runtime` 的字段注释与前端文案要写清"仅 backoff 策略有值，0 表示当前策略不是内置的那种" |
| **D-R0203** | 设计文档 §6.1 把 `store.flush_interval` 列为热更键，与代码现状冲突：`core/config_reload.go` 的 `configClasses` 把它记成 `ClassRestart`，`core/store.go` 的 flush 循环那三行注释也明确"间隔在构造时定下，改它要换 ticker 或加锁"。R02 的复核过程实跑核过这条 | **归 R07 的文档同步**（本卡按代码现状与本卡 §3.3 落地，`flush_interval` 不碰）。R07 要把设计 §6.1 那一行改到重启档，并在 §10.2 的差异记录里指明这条设计文档改动的理由；不要反过来把 ticker 重建塞进本系列 |

### 10.6 未覆盖项

- **六个入口零调用方**：本卡证明的是"每个入口都能改变它掌管的行为"，
  没证明"配置文件的改动会走到这些入口"。后半句是 R06。
- **`Scheduler` 零值时 `RetryPolicyMaxDelay` 返回 0** 这条分支没有用例（§10.2 第 12 条）：
  仓内没有"零值 `Scheduler` 被人拿去读数"的构造路径，造一条只能靠手搭 `&Scheduler{}`，
  与本卡要证的语义无关。
- **SQLite 侧"按新时长淘汰旧行"这条热路径没有用例**：本卡把 `retentionAge` 换成原子值，
  但时间淘汰的判据（`prune` 里 `retentionAge <= 0` 与 `cutoff`）与 S03 落地时相同、没改，
  本卡的并发用例又把 `age` 恒置 0，所以"运行期把时长改成 24h 之后旧行在下一次 prune 消失"
  这件事只有读数侧证据（存进去的值确实变了），没有行数侧证据。
  归 R07 的端到端场景（那时才有配置文件这一条完整路径）。
  注：首版曾把"负时长折成 0"记成等价变异，现已被 §10.2 第 13 条的读数断言变成真判据（§10.3 的 N5）。
- **`slog.SetDefault` 之后改级别会影响经 `slog.Default()` 的那一路**（卡 §3.1 要求写进注释，
  注释已写在 `NewLoggerWithLevelVar` 上）：没有用例，因为断它要改全局默认日志器，
  会和同包并行跑的用例互相污染。
- **`Store` 接口新增方法是对本仓之外的破坏性变更**（任何 `core.Store` 的实现者都要补方法）：
  系列拍板 P1 已接受这一条，本卡只保证仓内实现者全部补齐（`go vet ./...` 绿）。
- **平台**：本机 Windows 实跑。Linux/macOS 未实跑；本卡只引入 `sync/atomic`，
  没有文件监听与 syscall，交叉构建与实跑归 R07。

