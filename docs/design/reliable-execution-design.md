# godelayq 可靠执行设计：Job/Attempt/状态机/幂等提交/Outbox/Misfire

> 状态：方案定稿，2026-11-08。现状盘点基于 main（b8ab637）的代码走读，已核实。
> 引用约定与 `docs/architecture.md` 一致：按**符号**锚定代码（如 `core/scheduler_exec.go` 的 `executeJob`），不写行号。
> 实施拆卡见 `docs/design/tasks/reliable-execution/`；本文件回答"改成什么样、为什么这么改、边界在哪"。

---

## 1. 背景与目标

现有调度器是一条"尽力而为"的单进程延迟任务链路：提交即落快照、到期弹出执行、失败按退避重试、崩溃靠 Restore 重入队。这套链路对"发一个延迟请求"够用，但回答不了三类问题：

- **可解释**：这条任务到底跑过几次？第 3 次为什么失败、是不是超时？为什么停在 paused？
- **可恢复**：进程崩溃时正在跑的任务重启后去哪了？谁决定重跑还是停住？停机错过了三个 cron 窗口怎么算？
- **可审计**：每次状态变化有没有留痕？事件丢了（队满丢弃）任务状态还在吗？取消的任务去哪了（现在直接删除）？

目标是在**不推翻现有分层与接口惯例**的前提下，用八件事把链路升级为可靠执行：

| # | 能力 | 一句话口径 |
| --- | --- | --- |
| 1 | Job/Attempt 数据模型 | 每次尝试一行账本，状态与结论可查 |
| 2 | 幂等提交 | 同一请求重发不产生第二个任务 |
| 3 | 状态机 | 状态转移集中校验，非法转移不存在 |
| 4 | Retry 分类 | 失败类别（可重试/不可重试）落在持久记录上 |
| 5 | Recovery Policy | 崩溃恢复策略可配置，悬空尝试有结局 |
| 6 | Outbox 事件 | 状态变更与事件同事务，事件至少送达一次 |
| 7 | Cron 时区与 misfire | 按任务时区计算触发；错过的窗口有策略、有留痕 |
| 8 | 故障注入测试 | 崩溃点、存储故障、事件丢失、时钟偏移成套验证 |

## 2. 决策摘要

| # | 决策 | 理由与代价 |
| --- | --- | --- |
| D1 | **Attempt 账本放任务库**（`store/jobs` 新表 `job_attempts`）；JSON 文件模式降级：不落逐次 attempt，审计靠事件流，文档写明边界 | attempt 是执行账本不是观测数据，应与任务快照同库、可同事务；JSON 整文件重写的形态装不下逐行追加。代价：默认 JSON 部署没有 attempt 端点（返回明确的降级标志，见 §8.2） |
| D2 | **Outbox 放任务库**（`job_outbox`），与 jobs/job_attempts 的状态变更**同事务**提交；中继器异步投递给 EventBus；JSON 模式保持现状（直发，队满即丢） | "状态落了、事件没了"的窗口必须用同事务关掉。代价：库模式下事件到 WS/SSE 有一个中继周期的延迟；崩溃重放按 at-least-once 语义可能重复推送（读侧去重，见 D5） |
| D3 | **Cancel 落 cancelled 终态**：取消不再删除快照，取消后 `GET /jobs/:id` 返回 cancelled 记录（原来是 404）；终态受既有留痕淘汰策略管理 | 可审计要求"这条任务存在过且被取消"在存储里可查。行为变化见 §11.3 |
| D4 | **状态机集中**：转移表定义在 `core/state.go`，调度器所有状态写点收口到 `persistAndEmit`；状态变化必须经校验，非法转移拒绝并记日志 | 转移规则现在散在约 10 处手写，靠注释约定；收口后"为什么能从这里到那里"只看一张表 |
| D5 | **事件 at-least-once**：中继器重投未送达事件；`observe.job_events` 增加 `outbox_seq` 可空列 + 部分唯一索引，入库按冲突忽略去重 | at-least-once 必然带来重复投递的可能（发布成功、标记 dispatched 前崩溃），去重放读侧入库点，方言分支收口成一个函数（先例：`mgr/models.go` 的 `nameConflictClause`） |
| D6 | **接口在消费方**：core 新增 `AttemptLedger`、`EventOutbox` 两个可选能力接口，实现方隐式实现；调度器持有可 nil 的引用，nil 即降级 | 全仓红线（各包 `doc.go`）。JSON 存储不实现这两个接口，能力差异表达为"不注入 = 明确降级"，与既有装配取向一致 |
| D7 | **Misfire 默认 fire_once**（维持现状语义：错过只补跑一次），任务级 `misfire_policy` 可改 skip，全局键给默认值与宽限期 | fire_once 是既有行为的显式化；skip 适合"错过多窗口就别补了"的部署。策略差异必须有事件留痕（`job.misfired`） |
| D8 | **兼容性 additive**：`JobSnapshot` 新字段全部 `omitempty`；三张新表各占一个迁移版本（jobs v2/v3/v4、observe v2），不改任何既有版本语句；事件 metadata 只加键不删键 | 迁移冻结契约（[bun-store-design.md](bun-store-design.md) D1）；旧 jobs.json / 旧库文件解码必须零改动可用 |

## 3. 现状盘点（已核实，2026-11-08）

按八点需求逐项给出"现在有什么、缺什么"。符号锚定，行号见 git。

### 3.1 Job/Attempt 模型

- `core/job.go` 的 `Job` 有 `Attempts int`（注释"总尝试次数（含重试）"）与 `Exec *ExecMeta`（只存**最近一次**执行结论）；`JobSnapshot` 是唯一持久形态。
- 逐次尝试的痕迹只存在于事件流：`executeJob` 发 `job.started`（metadata 带 attempt 序号）与 `job.completed/failed/retrying`。事件在观测库里受保留策略约束（默认 20 万条 / 720 小时，`core/config.go` 的 `DefaultObserveEventRetention*`），且写入是异步批量的——**队满丢弃有计数但没有补偿**（[sqlite-observability-design.md](sqlite-observability-design.md) D4 的取舍，对观测合理，对账本不够）。
- 既有缺陷：`Job.CloneForRetry` 未拷贝 `Attempts`，重试后计数清零，与字段注释矛盾；`handleSuccess` 的 cron 重排构造新 Job 时同样丢 `Attempts`。执行器任务崩溃后 restore，事件的 attempt 序号会从 0 重来。
- 缺口：没有 Attempt 实体；"第 N 次尝试的结论"不可查询。

### 3.2 幂等提交

- `api/handlers.go` 的 `createJobFromRequest` 不接收任何客户端 ID；`core/scheduler_job.go` 的 `Schedule` 在 `job.ID == ""` 时调 `generateID()`（UUIDv7）。
- 提交方超时重试 = 新建第二个任务；没有任何去重机制。

### 3.3 状态机

- `JobStatus` 六态（pending/running/success/failed/cancelled/paused，`core/job.go`），`IsTerminal` 与"paused 非终态"有明确定义。
- 但**转移规则散落**：`executeJob`（pending→running、→success/failed）、`handleInterrupted`（running→pending）、`handleFailure`/`markFailed`（→failed）、`pausePendingJob`（pending→paused）、`Resume`（paused→pending）、`Cancel`（→删除）、`Restore`（running→pending 或 →paused）。
- `Cancel`（`core/scheduler_job.go`）对堆内/执行中/暂停中的任务统一 `store.Delete`——cancelled 状态从不落盘，"任务存在过且被取消"无据可查。

### 3.4 Retry 分类

- 执行器侧已经很完整：`executor/exit.go` 的 `failureClass` 十类 + `classifyExit` 单点判定 `Retryable` + `ExitError.Permanent()` 供 core 读（TASK-E12 交付）。
- core 侧只有二分：`isPermanentFailure`（`core/retry.go`）。
- 缺口：分类结论（类别文本、retryable、退出码、超时标记）**不落在任何持久记录上**，只在 `job.failed` 事件的 metadata 里出现过（`"permanent"`/`"timeout"` 键）；普通 Go handler 没有分类通道（维持二分，本设计不强制）。

### 3.5 Recovery Policy

- `Restore`（`core/scheduler_runtime.go`）：非终态非 paused 快照复位 pending 重入队；`RestoreGuard` 钩子（TASK-E14）可改判。
- 现有守卫只有一种：`cmd/server` 的 `pauseRunningExecOnRestore` 硬编码"running 且 exec 类 → paused"；`executors.restore_policy=replay` 时不装守卫。
- 缺口：策略按类别硬编码、不可配置；崩溃瞬间 running 的 attempt 在账本上永远悬空（没有 finished_at，也没有 abandoned 之类结局）。

### 3.6 Outbox

- `core/event.go` 的 `EventBus` 是进程内 pub/sub，`Publish` 非阻塞、订阅缓冲满即丢。
- `observe.EventLog`（`store/observe/events.go`）订阅总线 → 有界队列 → 批量事务落 `job_events`；与任务快照写入（`store/jobs` 或 JSON 文件）**不同库不同事务**。
- 后果：状态写成功、事件可能丢（总线丢弃 + 批量丢弃两层）；反过来事件在状态前丢也一样。崩溃恢复时 `Restore` 不重发任何历史事件。

### 3.7 Cron 时区与 misfire

- `core/cron.go` 的 `CronParser.Next(expr, now)` 用 robfig 解析器，时区跟随传入时刻（服务器本地）；Job 没有时区字段。
- misfire：`Restore` 把 TriggerAt 已过期的任务直接入堆立即补跑；cron 任务在 `handleSuccess` 里从 `time.Now()` 算下一轮。停机错过 N 个窗口 = 补跑 1 次，其余窗口静默蒸发，无事件、无策略。

### 3.8 故障注入

- 已有零散覆盖：`scheduler_recovery_test.go`（恢复）、`scheduler_timeout_test.go`、`scheduler_concurrency_test.go`、`jobs_test.go` 双后端等。
- 缺口：没有成套的故障注入（崩溃点矩阵、store 写失败、事件队列溢出、时钟偏移、关停竞态）。

## 4. 范围

### 4.1 进入本设计

单进程语义内的全部八点；双存储后端（JSON 降级、sqlite/PG 全量）；既有配置热重载链、组合根装配、观测层批量写入等机制全部沿用。

### 4.2 明确不做（附理由）

| 不做 | 理由 |
| --- | --- |
| 多实例/分布式调度（选主、分片） | 单进程是既有边界；attempt 账本与 outbox 的表结构不排斥将来演进，但选主不进本设计 |
| 事件外推第三方（webhook/kafka） | Outbox 的消费方是本进程 EventBus；外推是新的部署面，等有需求另立项 |
| Attempt 无上限全量保存 | 与终态留痕同理，按任务保最近 N 条（`store.attempts_keep_last`）；全量归档是存储工程，不是调度语义 |
| 普通 Go handler 强制错误分类 | 既有 `PermanentError` 二分够用；分类通道只在执行器侧落地（§3.4） |
| 一次性任务的 misfire 策略 | 一次性任务补跑一次就是合理语义，策略只对 cron 有意义 |

## 5. 总体架构

分层不变（见 `docs/architecture.md` §1），改动集中在 core 的收口函数与 store/jobs 的新表：

```
                         ┌────────────────────────────────────────────┐
                         │ store/jobs（任务库，sqlite/PG）              │
   executeJob/handle*     │   jobs（快照，权威）          job_attempts   │
   ┌──────────────┐       │   job_outbox（出箱）         job_idempotency│
   │ core 状态机   │ persist│   ▲ 同一事务：快照+attempt+出箱             │
   │ (state.go)   ├──────►│   │                                        │
   └──────┬───────┘ AndEmit└───┼────────────────────────────────────────┘
          │                    │
          │ JSON 模式：Update+Publish 直发（现状，降级边界）        │
          │                    ▼
          │            outbox relay（中继器，cmd/server 装配）
          │                    │ 按 seq 批量取 pending → Publish 进 EventBus
          │                    ▼ → 标记 dispatched → TTL 淘汰
          │            EventBus ──► api 内存缓冲 ──► WS/SSE/GET /events
          │                    └──► observe.EventLog ──► job_events
          │                            （按 outbox_seq 去重）
          └── 幂等/读取：api.createJobFromRequest → 幂等查重 → Schedule
                       GET /jobs/:id/attempts → AttemptLedger
```

要点：

1. **core 的单一收口**：`persistAndEmit(job, events...)` 是所有"状态写 + 事件"的唯一通道。装了 outbox 时一次事务写快照与出箱行；否则退回 `store.Update` + 直接 `Publish`（与现状逐字节等价）。
2. **事件-only 通道**：`emit(ev)` 处理不带状态写的事件（`job.resumed`、`job.misfired` 等）。outbox 模式下它也走出箱（独立事务）——保证**同一任务的事件单调有序**（出箱 seq 全局递增，中继器按 seq 顺序投递），不会出现"resumed 比 scheduled 先到"的乱序。
3. **中继器是库模式下唯一的事件生产者**：EventBus 的订阅端（api 缓冲、observe.EventLog）完全不变。
4. **attempt 与快照同库同事务**：attempt 行的 begin/finish 与任务状态写可以合并进一次事务（begin 在 pending→running 的那次写里，finish 在收尾那次写里），杜绝"任务失败了但账本上还是 running"。

## 6. 数据模型

### 6.1 任务库新表（迁移按卡各占一版，均给双方言 DDL）

时间戳列口径与 jobs 表一致（UnixNano，PG 升 BIGINT）；命名避开 PG 保留字（`trigger` 是 PG 保留字，列名用 `trigger_source`；`error` 同理用 `error_text`）。

#### `job_attempts`（re02，迁移 v2）

```sql
CREATE TABLE IF NOT EXISTS job_attempts (
  job_id        TEXT NOT NULL,
  seq           INTEGER NOT NULL,          -- 1 起递增，= Job.Attempts 自增后的值
  status        TEXT NOT NULL,             -- running | success | failed | cancelled | abandoned
  trigger_source TEXT NOT NULL,            -- scheduled | retry | restore | misfire
  started_at    INTEGER NOT NULL,          -- UnixNano
  finished_at   INTEGER,                   -- NULL = 在途（崩溃遗留，由 Restore 落 abandoned）
  duration_ms   INTEGER NOT NULL DEFAULT 0,
  class         TEXT,                      -- 失败类别文本（§6.4），成功为 NULL
  retryable     INTEGER,                   -- 0/1；NULL = 未判定
  exit_code     INTEGER,
  signal        TEXT,
  http_status   INTEGER,
  error_text    TEXT,                      -- 脱敏后的错误文本（E08 口径）
  PRIMARY KEY (job_id, seq)
);
CREATE INDEX IF NOT EXISTS idx_attempts_job_started ON job_attempts(job_id, started_at);
CREATE INDEX IF NOT EXISTS idx_attempts_status ON job_attempts(status);
```

PG 侧：`seq INTEGER NOT NULL` 与 `started_at/finished_at/duration_ms` 升 BIGINT，其余逐字一致。attempt 状态五值是封闭词表，与 JobStatus 的关系：`abandoned` 只由 Restore 写（§7.5）；`cancelled` 只由取消收尾写。

#### `job_idempotency`（re03，迁移 v3）

```sql
CREATE TABLE IF NOT EXISTS job_idempotency (
  key         TEXT PRIMARY KEY,            -- Idempotency-Key 原文（trim 后）
  job_id      TEXT NOT NULL,
  fingerprint TEXT NOT NULL,               -- SHA256 hex（§7.3）
  created_at  INTEGER NOT NULL             -- UnixNano
);
CREATE INDEX IF NOT EXISTS idx_idem_job ON job_idempotency(job_id);
CREATE INDEX IF NOT EXISTS idx_idem_created ON job_idempotency(created_at);
```

#### `job_outbox`（re07，迁移 v4）

```sql
CREATE TABLE IF NOT EXISTS job_outbox (
  seq           INTEGER PRIMARY KEY AUTOINCREMENT,      -- PG: BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY
  job_id        TEXT NOT NULL,
  type          TEXT NOT NULL,             -- core.EventType 原文
  payload_json  TEXT NOT NULL,             -- core.Event 序列化原文（含 OutboxSeq 回填前的形状）
  status        TEXT NOT NULL,             -- pending | dispatched
  created_at    INTEGER NOT NULL,          -- UnixNano
  dispatched_at INTEGER
);
CREATE INDEX IF NOT EXISTS idx_outbox_pending ON job_outbox(status, seq);
CREATE INDEX IF NOT EXISTS idx_outbox_job ON job_outbox(job_id, seq);
```

### 6.2 观测库增量（re07，observe 迁移 v2）

```sql
ALTER TABLE job_events ADD COLUMN outbox_seq INTEGER;   -- PG: BIGINT
CREATE UNIQUE INDEX IF NOT EXISTS idx_events_outbox
  ON job_events(outbox_seq) WHERE outbox_seq IS NOT NULL;  -- 双方言同形的部分唯一索引
```

旧行为库（outbox_seq 全 NULL）：NULL 互不冲突，唯一索引不生效，行为不变。

### 6.3 core 结构体增量（全部 omitempty，旧快照解码零影响）

```go
// core/job.go
type Job struct { /* 既有字段之外新增 */ }
// JobSnapshot 新增：
Timezone      string `json:"timezone,omitempty"`       // IANA 名；空 = 服务器本地
MisfirePolicy string `json:"misfire_policy,omitempty"` // fire_once | skip；空 = 继承全局默认
// Attempts 既有字段语义收严：全局单调，重试/cron 重排不再清零（修 §3.1 缺陷）

// core/attempt.go（新）
type AttemptStatus int    // running|success|failed|cancelled|abandoned（String/Parse 对称）
type AttemptTrigger int   // scheduled|retry|restore|misfire
type Attempt struct {     // 账本一行；与 job_attempts 列一一对应
    JobID string; Seq int; Status AttemptStatus; Trigger AttemptTrigger
    StartedAt time.Time; FinishedAt time.Time; DurationMs int64
    Class string; Retryable bool; RetryableSet bool
    ExitCode int; ExitCodeSet bool; Signal string; HTTPStatus int; HTTPStatusSet bool
    Error string
}

// core/outbox.go（新，消费方接口）
type EventOutbox interface {
    // UpdateWithEvents 在同一事务里落快照与出箱事件；实现方只允许是任务库。
    // 事件经中继器投递，本方法返回后调用方不得再自行 Publish（避免双发）。
    UpdateWithEvents(snapshot JobSnapshot, events []Event) error
    // AppendOutbox 只入箱不写快照（emit 通道用），独立事务。
    AppendOutbox(events []Event) error
}

// core/attempt.go（新，消费方接口）
type AttemptLedger interface {
    BeginAttempt(jobID string, rec Attempt) error        // upsert（seq 冲突视为重放，幂等）
    FinishAttempt(jobID string, seq int, rec Attempt) error
    AbandonAttempts(jobID string, uptoSeq int, at time.Time) error // Restore 收口悬空尝试
    Attempts(jobID string, limit int) ([]Attempt, error) // 读端点用，seq 降序取最近 limit 条
    PruneAttempts(keepLast int) error                    // 每任务保最近 keepLast 条，写入路径周期调用
}

// core/event.go
type Event struct { /* 既有字段之外新增 */ }
OutboxSeq int64 `json:"outbox_seq,omitempty"` // 中继器回填；读侧去重键
```

新事件类型：`EventJobMisfired EventType = "job.misfired"`（metadata：`missed_count`、`first_missed_at`、`policy`）。

### 6.4 失败类别词表（re04）

`executor/exit.go` 的 `failureClass` 增 `String()`，值即 `job_attempts.class` 与 `job.failed` metadata `"class"` 的取值：

| 类别 | 可重试 | 说明 |
| --- | --- | --- |
| `invalid_submission` | 否 | 参数越界、payload 结构错、命令行拼不出 |
| `profile_unavailable` | 否 | 程序找不到、档位类型不符、起不来 |
| `not_allowed` | 否 | 产物文件建不出、目录不可写、权限 |
| `timeout` | 是 | 执行超时（含等并发许可超时 `permit_wait`，单列） |
| `permit_wait` | 是 | 档位并发许可等满 |
| `exit_code` | 按 retry_on_exit | 进程退出码 |
| `http_status` | 5xx/408/429 | 响应状态码不在 expect_status |
| `address_refused` | 否 | 拨号层策略拒绝（回环/内网） |
| `transport` | 是 | DNS/TCP/TLS/断连 |
| `interrupted` | 不适用 | 取消/关停，走 handleInterrupted，不参与重试 |

普通 Go handler 的失败在 attempt 记 `class = "handler_error"`；超时与取消的识别沿用 `executeJob` 的 `errors.Is` 判定（记 `timeout` / `interrupted`）。

### 6.5 状态机转移表（re01）

```go
// core/state.go
var transitions = map[JobStatus][]JobStatus{
    StatusPending:   {StatusRunning, StatusPaused, StatusCancelled, StatusFailed},
    StatusRunning:   {StatusSuccess, StatusFailed, StatusPending, StatusPaused, StatusCancelled},
    StatusPaused:    {StatusPending, StatusCancelled},
    StatusSuccess:   {},   // 终态不可出
    StatusFailed:    {},
    StatusCancelled: {},
}
// canTransition(from, to)：from == to 恒真（原地更新不算转移，如 UpdatePending/SetGroup）
```

每条出边的现实来源（写卡与评审时逐一对照）：

| 出边 | 调用点 |
| --- | --- |
| pending→running | `executeJob` 开始执行 |
| pending→paused | `Pause`/`ForcePause`（堆内条目）、Restore 守卫改判 |
| pending→cancelled | `Cancel`（堆内条目、暂停中的任务） |
| pending→failed | `executeJob` 无 handler 分支（派发期失败，任务从未执行即终态） |
| running→success | `handleSuccess` |
| running→failed | `markFailed`（重试耗尽 / 永久失败） |
| running→pending | `handleInterrupted`（优雅关停）、`Restore` replay、cron 重排（同 ID 新一轮） |
| running→paused | `ForcePause` 收尾认领（`parkForcedPause`）、Restore 守卫 |
| running→cancelled | 取消执行中任务（`cancelRequested` 收尾认领，见 §7.2） |
| paused→pending | `Resume` |
| paused→cancelled | `Cancel`（暂停中的任务） |

实现口径注记（RE01 落地时确定）：cron 重排与 `Resume` 的目标态转移（running→pending、paused→pending）**不经 transition**——两者都以"同 ID 新对象 / 已按前置条件校验过的快照"走 `Schedule` 的 identity 写（新对象上无从取 from；`Resume` 的 paused 前置校验在调用点）。转移表对这两条边的保障是"测试钉住 + 调用点前置校验"，不是运行期校验。

## 7. 写入路径与并发

### 7.1 persistAndEmit：状态写 + 事件的单一收口

```go
// core（示意）
func (s *Scheduler) persistAndEmit(job *Job, events ...Event)
// 1. 若 job.Status 发生变化：canTransition 校验，非法则记 error 并拒绝本次状态写
//    （事件照发，状态保持原值——宁可账本少一行，不可账本说谎）
// 2. outbox 能力可用：outbox.UpdateWithEvents(job.ToSnapshot(), events)
//    否则：store.Update(job.ToSnapshot()) 后逐个 eventBus.Publish(events)
```

迁移期间所有既有状态写点（§3.3 列表）改走它；`handleInterrupted`/`parkForcedPause` 等"先读快照再改"的路径同样收口。`Schedule` 的首次落盘（`Save`）与 `job.scheduled` 事件也并入（outbox 模式 = Save+入箱同事务）。

### 7.2 Cancel 落终态（re01）

现有三条取消路径改为：

1. **堆内**：`heap.Remove` → 状态 cancelled → `persistAndEmit`（job.cancelled）→ 落终态快照（受留痕淘汰管理）。不再 `store.Delete`。
2. **暂停中**：快照改 cancelled → 同上。
3. **执行中**：登记 `cancelRequested[jobID]`（仿 `forcedPause` 集合）+ 取消 ctx → `handleInterrupted` 收尾时认领：转移 running→cancelled、FinishAttempt(cancelled)、发 job.cancelled。`parkForcedPause` 的优先级保持最高（强制暂停先于取消认领，与既有"暂停优先于一切"口径一致）。

调度器启动时 `cancelRequested` 清空（进程内意图，与 `suspended` 同理，Start 复位）。

### 7.3 幂等提交（re03）

- 请求头 `Idempotency-Key`（trim 后 1..128 字符）或请求体 `job_id`（可选，[A-Za-z0-9_-]{1,64}）。两者可同时给：`job_id` 决定任务 ID，`Idempotency-Key` 决定查重键（缺省时用 `job_id` 兼作键）。
- 指纹：对 `{name, type, payload, cron_expr, is_repeat, trigger_at(解析后), group, timeout, max_retries, retry_delay}` 的固定结构体做 `json.Marshal`（Go 结构体字段序确定）取 SHA256 hex。
- 语义（`api.createJobFromRequest` 在 `Schedule` 之前查）：

| 场景 | 响应 |
| --- | --- |
| 键首次出现 | 201，登记 (key → job_id, fingerprint) |
| 键命中且指纹相同 | 200，返回既有任务 + 响应头 `Idempotency-Replayed: true`，**不创建** |
| 键命中但指纹不同 | 409 conflict，details 指明键已被不同内容占用 |
| `job_id` 与既有非终态任务撞且指纹不同 | 409 |
| `job_id` 撞且指纹相同 | 等价重放，200 |

- 幂等键淘汰：`store.idempotency_ttl`（默认 24h），与 attempt 淘汰共用写入路径的周期清理。
- 幂等存储的接口按消费方定义在 `api`（`IdempotencyStore`：`Lookup(key)` / `Put(key, jobID, fingerprint)` / `PruneExpired`），`store/jobs.Store` 隐式实现（server.go 的 Option 注入，与既有 eventReader 等窄接口同一手法）；RE07 事务化后登记并入 `UpdateWithEvents`。
- JSON 模式：api 层注入进程内 map 实现（`api` 的 Option），重启即失效——文档明确"JSON 模式的幂等只在进程生命周期内成立"。
- 顺带落地 `SnapshotReader`（`Get(jobID)`）窄接口：core 定义、JSON 与 jobs 两实现，幂等撞 `job_id` 判定用；`Scheduler.findSnapshot` 的 LoadAll 线性扫描同步收敛到它（既有注释里挂账的"等 Store 有 Get 之后一起收敛"）。

### 7.4 Retry 分类透传（re04）

- `ExitError` 增 `Class string`（§6.4 词表，由 `failureClass.String()` 填充）；`newFailure`/`newStatusFailure` 构造点集中赋值。
- `FinishAttempt` 的 rec 由 `executeJob` 收尾组装：执行器任务从 `ExitError` 取 class/retryable/exit_code/signal/http_status；普通 handler 记 `handler_error` + errors.Is 的 timeout/interrupted。
- `job.failed` 事件 metadata 增 `"class"`（additive；`"permanent"`/`"timeout"` 既有键不动）。

### 7.5 Recovery Policy（re05）

- 配置 `scheduler.recovery_policy: auto | replay | pause`（默认 auto）：
  - `auto` = 现状（default 池 replay、exec 池 pause——`pauseRunningExecOnRestore` 的行为成为 auto 的执行器分支）；
  - `replay` = 所有类别一律复位 pending；
  - `pause` = 崩溃前 running 的一律停 paused。
- `cmd/server` 的 `installRestoreGuard` 改读该配置；`RestoreGuard` 钩子机制本身不变（钩子是机制，策略是配置）。
- **悬空尝试收口**：`Restore` 处理崩溃前 running 的快照时（无论改判哪种），调 `AbandonAttempts(jobID, snap.Attempts, now)`——那一次尝试的 `finished_at` 落 now、status 落 `abandoned`。重跑会开出新 seq，账本永远连续。
- `holdRestored` 的落盘失败路径维持"记日志、下次启动再判"的既有口径。

### 7.6 Cron 时区与 misfire（re06）

- 时区：`CreateJobRequest.Timezone` → `Job.Timezone`（IANA 名，提交时 `time.LoadLocation` 校验，空 = 服务器本地，行为与现状一致）；`CronParser.Next(expr, now, loc)` 以 `now.In(loc)` 交给 robfig（其 schedule 按入参时刻的时区展开）。快照持久化 `timezone`，Restore/重排都按它算。
- misfire 判定点：`Restore` 与 `Schedule`（cron 分支）里，`TriggerAt < now - scheduler.misfire_grace_period` 且 `IsRepeat && CronExpr != ""` 时：
  - `fire_once`（默认）：立即入堆补跑一次（现状语义），事件 `job.misfired` 带 `missed_count`（用解析器从 TriggerAt 迭代 `Next` 到 now 统计，封顶 1000 防病态表达式）与 `first_missed_at`；attempt 的 trigger_source 记 `misfire`。
  - `skip`：不补跑，下一轮 = `Next(now)`，同事件留痕。
- 任务级 `misfire_policy` 字段（fire_once|skip，空 = 继承全局）；一次性任务不参与（§4.2）。
- `grace_period` 默认 60s：秒级停机重启不该刷 misfire 事件。

### 7.7 Outbox（re07）

- 写入：`UpdateWithEvents`（快照 upsert + 出箱批量 INSERT + 幂等键/attempt 若在同路径则一并，同一事务）；`AppendOutbox` 独立事务。出箱行 status=pending。
- 中继器（`store/jobs` 的 `OutboxRelay`，`cmd/server` 装配）：
  - 周期 `store.outbox_flush_interval`（默认 200ms，下限 10ms，与观测层同口径）批量取 `pending ORDER BY seq LIMIT n`（默认 256）；
  - 逐条回填 `OutboxSeq` 后 `eventBus.Publish`（EventBus 非阻塞，订阅满丢弃的现状不变——读侧已有 outbox_seq 可对账）；
  - 全批发布完标记 dispatched（同事务）；
  - 启动时先重放一轮残留 pending（崩溃恢复）；
  - 淘汰：`store.outbox_retention_age`（默认 72h）删 dispatched；pending 积压超过 `store.outbox_max_pending`（默认 100000）时记 error 并按最旧截断（防病态场景无界增长，丢弃计入日志）。
- 去重：中继器发布成功但标记前崩溃 → 重放重复投递；`observe.EventLog` 写入时带 `outbox_seq`，按 §6.2 的部分唯一索引冲突忽略。方言分支收口成一个函数（冲突子句带 `WHERE outbox_seq IS NOT NULL` 谓词），在 `observe/package_guard_test.go` 登记豁免（先例：`mgr/models.go` 的 `nameConflictClause`）。
- 关停顺序（组合根 defer 位次，在既有顺序的最前面加一步）：**outbox relay.Stop → 事件/审计写入器 → 观测库 → 管理库 → 任务库**。中继器必须先于 EventBus 的订阅者关闭，否则会有发布落在已关闭的订阅通道上。
- WS/SSE 延迟：事件可见延迟上界 = 一个中继周期（默认 200ms）；文档写明。

### 7.8 并发与锁纪律（不变量）

- `persistAndEmit` 不新增锁：状态机表是只读 map；outbox/ledger 调用发生在 worker 协程与 api 协程，各自持自己既有的锁序（堆锁 → 调度器锁的既有顺序不破坏——`dispatchable` 的注释仍然是铁律）。
- 中继器单协程，与调度器无共享可变状态（只经 EventBus 与任务库交互）。
- attempt 的 begin/finish 只由持有该 job 的 worker 协程调用（与 `Job.Exec` 的写侧纪律一致）；abandon 只由 Restore（启动期、Start 之前无并发执行）调用。

## 8. 读取路径

### 8.1 既有端点

- `GET /jobs/:id`：不变（cancelled 记录因 D3 变为可读，见 §11.3）。
- `GET /events`、WS/SSE：形状不变；`Event.OutboxSeq` 是新增可选字段，前端透传不解析。

### 8.2 新端点：`GET /api/v1/jobs/:id/attempts`

```json
{ "job_id": "...", "ledger": true,
  "items": [ { "seq": 3, "status": "failed", "trigger_source": "retry",
               "started_at": "...", "finished_at": "...", "duration_ms": 30120,
               "class": "timeout", "retryable": true,
               "exit_code": 0, "signal": "", "http_status": 0,
               "error_text": "" } ] }
```

- seq 降序，limit 默认 20（`?limit=` 可调，上界 100）。
- **降级语义**：JSON 模式返回 200 + `"ledger": false` + `"items": []`（可加 `detail` 说明），客户端按 `ledger` 标志决定是否展示账本页。不用 404/503——"后端没这能力"与"任务不存在"是两件事。
- 控制台任务详情页的时间线（既有）继续走事件流；attempt 账本是它的补充行视图（前端改造不在本系列卡内，接口先行）。

## 9. 配置面

| 键 | 类型/默认 | 热更 | 说明 |
| --- | --- | --- | --- |
| `store.attempts_keep_last` | int，20 | 重启生效 | 每任务保留最近 N 条 attempt；<=0 按默认 |
| `store.idempotency_ttl` | duration，24h | 重启生效 | 幂等键登记的保留时长 |
| `store.outbox_flush_interval` | duration，200ms（下限 10ms） | 重启生效 | 中继器批量周期，也是事件可见延迟上界 |
| `store.outbox_retention_age` | duration，72h | 重启生效 | dispatched 出箱行的保留时长 |
| `store.outbox_max_pending` | int，100000 | 重启生效 | pending 积压上限，超过按最旧截断并记 error |
| `scheduler.recovery_policy` | auto\|replay\|pause，auto | 重启生效 | 崩溃恢复策略（§7.5） |
| `scheduler.misfire_policy` | fire_once\|skip，fire_once | 重启生效 | misfire 全局默认；任务级字段可覆盖 |
| `scheduler.misfire_grace_period` | duration，60s | 重启生效 | TriggerAt 早于 now-该值 才判 misfire |

全部新键进 `core/config.go` 的 `Normalized()`/`Validate()` 与 `configs/config.example.yaml`（既有测试 `TestExampleConfigMatchesLocal` 把关）；热更分级全部取 ClassRestart——这些值影响的是执行语义而非读数，热更收益不抵口径混乱。

## 10. 后端改动方案（逐文件）

| 包/文件 | 改动 | 卡 |
| --- | --- | --- |
| `core/state.go`（新） | 转移表 + `canTransition` | re01 |
| `core/scheduler_exec.go` / `scheduler_job.go` / `scheduler_runtime.go` | 状态写点收口 `persistAndEmit`；Cancel 改落终态（`cancelRequested` 集合）；Restore 调 AbandonAttempts | re01/re02/re05 |
| `core/attempt.go`（新） | Attempt/AttemptStatus/AttemptTrigger + `AttemptLedger` 接口 | re02 |
| `core/outbox.go`（新） | `EventOutbox` 接口 | re07 |
| `core/job.go` | JobSnapshot 增 Timezone/MisfirePolicy；CloneForRetry 与 cron 重排保留 Attempts | re02/re06 |
| `core/cron.go` | `Next(expr, now, loc)`；misfire 窗口计数助手 | re06 |
| `core/event.go` | `OutboxSeq` 字段、`EventJobMisfired` | re06/re07 |
| `core/store.go` | `SnapshotReader` 窄接口（Get） | re03 |
| `core/config.go` | §9 全部新键 + Validate/Normalized + 热更分级表 | re02/re03/re05/re06/re07 |
| `store/jobs/schema.go` | 迁移 v2（attempts）/ v3（idempotency）/ v4（outbox），双方言 | re02/re03/re07 |
| `store/jobs/db.go` | attemptRow/idemRow/outboxRow 模型与方法；`UpdateWithEvents`/`AppendOutbox`；`OutboxRelay`（新文件 `outbox.go`） | re02/re03/re07 |
| `store/jobs/jobs_test.go` 及新测试 | 双后端行为断言；SQL 快照追加新语句序列后 `-update` 重生成 golden | re02/re03/re07 |
| `store/observe/schema.go` / `events.go` / `models.go` | 迁移 v2 加列 + 部分唯一索引；写入去重（方言分支收口 + 守卫豁免登记） | re07 |
| `api/dto.go` / `handlers.go` | `job_id`/`timezone`/`misfire_policy` 请求字段；`Idempotency-Key` 头；幂等查重；`GET /jobs/:id/attempts`；404→cancelled 适配 | re01/re03/re06 |
| `executor/exit.go` | `failureClass.String()`；`ExitError.Class` | re04 |
| `cmd/server/main.go` / `reload.go` | 装配 ledger/outbox/幂等存储/中继器；关停 defer 链加 relay.Stop；`installRestoreGuard` 读新配置 | re02/re03/re05/re07 |
| 测试（新） | `core/state_machine_test.go`、`core/attempts_test.go`、`core/misfire_test.go`、`store/jobs/outbox_test.go`、`api/idempotency_test.go`、故障注入套件 | 各卡 / re08 |

## 11. 兼容性与回滚

### 11.1 存储迁移

- 三张新表各占一个版本（jobs v2/v3/v4）、observe 加列占 v2：`engine.Migrate` 每版本一事务、失败不落版本号、下次启动重跑；既有版本语句一个字符不改（冻结契约）。
- 旧库升级零数据迁移：新表从空开始，attempt 从新代码的第一次执行开始积累。

### 11.2 快照与事件兼容

- `JobSnapshot` 新字段 `omitempty`：旧 jobs.json / 旧 snapshot_json 解码不受影响；新快照被旧版本代码读到时忽略未知键（Go json 默认），回滚安全。
- 事件：`outbox_seq`、`"class"` metadata 全部 additive；`api/history.go` 与前端透传不解析（与 TASK-E12 加 `"permanent"` 同一手法）。

### 11.3 行为变化（需要写进 api.md 的变更）

1. **Cancel 后 GET 不再 404**：返回 cancelled 快照；列表接口会多出 cancelled 记录（受 `store.history_limit`/TTL 淘汰）。依赖 404 判断"任务已取消"的调用方需改判读 `status` 字段。
2. **库模式下事件经中继**：WS/SSE 可见延迟上界 ≈ `store.outbox_flush_interval`；崩溃恢复后可能收到少量重复事件（含 outbox_seq 可识别）。
3. **重试后 attempt 序号不再清零**：`job.started` 的 metadata attempt 值跨重试/cron 单调递增（修 §3.1 缺陷的必然结果）。

### 11.4 回滚

- 代码回滚：新表遗留无害（旧代码不读）；outbox 里的 pending 行不会被投递，状态一致性不受影响。
- JSON 模式与库模式可随时互切：JSON 是权威格式不变；切回 JSON 后 attempt/幂等/outbox 能力消失（§4.1 边界），任务本体无损。

## 12. 验收清单

1. `go build ./... && go vet ./... && go test ./...` 全绿；`scripts/test-pg.sh`（本机 DSN）与 CI `postgres-matrix` 同口径全绿。
2. SQL 快照：三张新表 + 去重插入的语句进 golden（两方言），连跑两次 `-update` 无 diff。
3. 状态机：转移矩阵穷举测试通过；非法转移在日志可见且状态未被改写。
4. 幂等：同键同指纹重放 200、异指纹 409；批量接口逐条独立。
5. Attempt：执行器任务一次失败重试 = 账本两行（scheduled+retry），崩溃恢复 = 悬空行 abandoned + 新行。
6. Outbox：杀进程后重启，崩溃前未投递事件被重放且 `job_events` 无重复行（outbox_seq 去重生效）。
7. Misfire：停机跨 N 窗口，fire_once 补跑 1 次并留 `job.misfired`（missed_count 正确）；skip 直接跳到下一窗口；纽约时区 DST 切换日触发时刻正确。
8. Cancel：取消后 GET 得 cancelled；Restore 不复活终态。
9. JSON 模式：全功能回归通过，attempt 端点返回 `ledger:false`，事件路径与现状逐字节一致。

## 13. 实施计划

拆卡与依赖见 `docs/design/tasks/reliable-execution/README.md`。顺序：re01（状态机地基）→ re02（账本）→ re03 幂等 ∥ re04 分类 ∥ re05 恢复 ∥ re06 cron，re07（outbox）只需 re01 可提前并行，re08（故障注入 + 文档）收口。每卡一个 PR，沿用 N 卡提交与全量回归的惯例。
