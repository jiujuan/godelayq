# TASK-R02　六个轻量 setter：给每个热更键一个可调用入口

- 所属阶段：M1 落点
- 依赖任务：R01
- 涉及文件：`core/logging.go`、`core/logging_test.go`（新增或增补）、`core/scheduler.go`、
  `core/scheduler_hot_config_test.go`（新增）、`core/store.go`、`core/store_hot_test.go`（新增）、
  `core/scheduler_test.go`（断言改写）、`cmd/server/main_integration_test.go`（替身补方法）、
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

func TestNewLogger_KeepsOldContract(t *testing.T) {
        // 既有调用方的三条预期：非法级别报错、非法格式报错、默认 info/text
        if _, err := NewLogger("nope", "text", io.Discard); err == nil {
                t.Error("NewLogger accepted an invalid level")
        }
        if _, err := NewLogger("info", "yaml", io.Discard); err == nil {
                t.Error("NewLogger accepted an invalid format")
        }
        var buf bytes.Buffer
        logger, err := NewLogger("", "", &buf)
        if err != nil {
                t.Fatalf("NewLogger defaults: %v", err)
        }
        logger.Info("hello")
        if !strings.Contains(buf.String(), "hello") {
                t.Error("default logger wrote nothing")
        }
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

行为用例（不是只看读数）：复用 `core/scheduler_recovery_test.go:162` 那个
`mockRetryPolicy{delay: ...}`，建调度器时给 `delay: 10ms`，跑一次失败重试确认按 10ms 排期；
再 `SetRetryPolicy(&mockRetryPolicy{delay: 30ms})`，跑第二次失败重试确认按 30ms 排期。
排期取值断言容忍 ±1 个 `time.Millisecond` 的抖动，不断言精确时刻（本仓既有重试用例的口径）。

**既有用例要改的一条**：`core/scheduler_test.go:124` 断言 `scheduler.retryPolicy == retryPolicy`
（同一性比较）。字段换成 `atomic.Pointer` 后这条必然编译失败，改成
`if got := scheduler.RetryPolicyMaxDelay(); got != ...` 或比较 `*s.retryPolicy.Load()` 指向的对象，
在实现记录 §10.2 里记为有意偏离（不是回归）。

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
                require.NoError(t, log.Append(auditEntryFixture(i)))
        }
        waitAuditQueued(t, log, 6)                 // 与 waitQueued 同形；台账侧若已有同名 helper 就用既有的
        require.NoError(t, log.Flush())

        count, err := log.Count()
        require.NoError(t, err)
        require.EqualValues(t, 6, count)

        log.SetRetention(2, 0)
        require.NoError(t, log.Append(auditEntryFixture(6)))
        waitAuditQueued(t, log, 1)
        require.NoError(t, log.Flush())

        count, err = log.Count()
        require.NoError(t, err)
        assert.EqualValues(t, 2, count, "the next batch's prune must use the new count")
        _ = db
}
```

`auditEntryFixture(i)` 照 `store/sqlite/audit_test.go` 里既有用例的 `api.AuditEntry` 构造写法抄
（必填字段：actor、actor kind、method、path、result 那一组），不要自造字段组合；
队列等待的 helper 若台账侧还没有，就照 `waitQueued`（`store/sqlite/events_test.go:52-63`）
的形状写一个，同样等条件而不是 sleep。
两条都要再断言一次 `SetRetention(0, -1)` 回到"用默认条数、不按时间淘汰"的口径
（count<=0 走默认、age<0 按 0），否则"setter 另立一套"这件事没人能发现。

### 5.5 手工

`go test ./core ./store/sqlite -race -count=3`。本卡没有调用入口，不该起进程冒烟——
冒烟统一留到 R06 的接线之后（R07 逐场景实测）。

## 6. 完成标准（DoD）

- [ ] 六个入口全部存在且被 §5 的用例证明改变了行为，而不只是改了字段值：
      `SetLogLevel`、`SetRetryPolicy`、`SetHistoryRetention`、`EventLog.SetRetention`、
      `AuditLog.SetRetention`（`RetryPolicyMaxDelay` 是读数口，不单独算一条）。
- [ ] `NewLogger` 的签名与既有行为不变；`NewLoggerWithLevelVar` 与它共用解析与 handler 构造
      （实现上只有一份 `text|json` 分岔）。
- [ ] `Store` 接口新增 `SetHistoryRetention`，`go build ./...` 绿——即所有实现者都补齐了；
      补齐清单写进 §10.1。
- [ ] 非法输入不改现值：`SetLogLevel("verbose")` 返回错误且级别保持；
      `SetRetryPolicy(nil)` 返回"不改"而不是清空。
- [ ] 三处换成原子字段的读取点全部改完，`-race -count=3` 干净，
      且 §5.3 的并发用例覆盖"写策略 / 读策略"交错。
- [ ] 未启用热重载的行为零变化：本卡不改任何构造函数的默认取值、不改任何既有日志文案，
      既有测试只有 §5.2 明说的那一条因字段类型而改写，其余用例未经修改即通过。
- [ ] `go build ./...`、`go vet ./...`、`go test ./... -race -count=1` 全绿；新增文件已 `gofmt -w`。

## 7. 验收方式

```bash
go test ./core -run 'TestSetLogLevel|TestNewLogger|TestSetRetryPolicy|TestSetHistoryRetention' -v
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

### 10.2 与本卡写法的差异（含 `core/scheduler_test.go:124` 那条既有断言的处置）

### 10.3 验证证据

### 10.4 手工验收

### 10.5 缺陷

### 10.6 未覆盖项
