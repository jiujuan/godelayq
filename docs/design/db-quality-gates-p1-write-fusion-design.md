# godelayq 数据库质量门禁 P1 + 热路径写合并设计

> 状态：定稿（2026-11-09，§12 五项已拍板）。任务卡见 `tasks/db-quality-gates/`（Q05–Q11）。
> 关联：`db-quality-gates-design.md`（P0：CI/红线守卫/SQL 快照，已全绿）、
> `reliable-execution-design.md`（RE01 转移收口 / RE02 账本 / RE07 出箱）、
> `sqlite-observability-design.md`（观测层批量写先例）、`../../architecture.md`。

## 1. 背景与目标

P0 把"SQL 写得对"钉住了（CI + 守卫 + golden）；P1 补运行时的眼睛，并顺手解决
链路分析发现的一个新问题——**worker 热路径上的写放大**。

一次成功执行（库模式、非 cron）在 worker 协程上串行发生约 6 次独立事务：

| 步骤 | 写操作 | 收口 |
| --- | --- | --- |
| `transition(running)` | `UpdateWithEvents` | state.go |
| `beginAttempt` | 账本 upsert | scheduler_exec.go |
| `emit(job.completed)` | `AppendOutbox` | state.go |
| `finishAttempt` | 账本 upsert | scheduler_exec.go |
| `finishAttempt` 后 | `PruneAttempts` DELETE | store/jobs/attempts.go |
| `transition(success)` | `UpdateWithEvents`（终态带 trim） | state.go |

SQLite 单写者下这 6 次写全局串行；到期风暴时执行吞吐的上限是它们而不是 Handler。
P1 门禁（G4/G5）恰好提供观测这事的眼睛，顺序上先门禁拿基线、再合并看改善。

目标：

1. **G4 可观测读数**——DB 占用（`sql.DBStats`）、方言、schema 版本、事件丢弃计数、
   出箱积压进 `/admin/runtime`；
2. **G5 慢查询日志**——bun QueryHook 按阈值打 Warn，默认关；
3. **G6 语句超时**——PG `statement_timeout` 经 DSN，缺省 Warn 提示；
4. **W1–W3 写合并**——在不改 durability 语义的前提下把热路径写次数压下来；
5. **G7 迁移纪律守卫**——版本递增不重复、方言可移植、v1 冻结。

非目标（另立卡，不在本批）：单写协程 + 批量提交（W4，会改"先落盘再执行"语义）、
普通池队头阻塞改造、Restore 按状态过滤下推、事件丢弃的补投。

## 2. 现状与缺口

| 已有（P0 + RE 系列） | 缺口 |
| --- | --- |
| CI 三 job、storeguard 三红线、SQL golden 钉文本 | 运行时对 DB 占用/慢查询零可见 |
| RE07 出箱：事件不丢（中继器按 seq 去重） | EventBus 满即丢**静默无痕**（直发模式无兜底） |
| RE02 账本逐尝试记账 | begin/finish/prune 三次独立写，无合并通道 |
| `UpdateWithEvents` 单事务落快照+出箱行 | 成功路径上 completed 事件与终态快照分两个事务 |
| 引擎层 WAL/busy_timeout/synchronous 可配 | PG 侧无语句超时；配置缺失只静默取默认 |

## 3. G4 可观测读数（Q05）

- **读数来源**：`sql.DBStats`（MaxOpen/Open/InUse/Idle/WaitCount/WaitDuration）经
  engine 暴露；方言名与 schema 版本（migrations 表 max version）由 store 侧上报；
  出箱积压行数由 relay 提供（relayer 已知 pending 计数则直接复用）。
- **接线面**：扩 `observabilityDB` 消费方接口（cmd/server），`/admin/runtime` 增加
  `db` 段。读数为瞬时值，口径注释沿用 RuntimeStats 的写法。
- **事件丢弃计数**（本次链路分析新增）：EventBus.Publish 满即丢处加 atomic 计数器，
  按 `dropped_events_total` 进 G4 读数。丢弃本身是有意取舍（不拖慢调度），要补的是痕迹。
- 风险：低。纯读数，不改执行路径。

## 4. G5 慢查询日志（Q06）

- bun `QueryHook`：AfterQuery 阶段测耗时，超过阈值打 Warn（SQL 文本 + 耗时 + 方言）。
- 配置：`store.slow_query_threshold`，默认关（0）；开时下限守卫对齐
  `minObserveFlushInterval` 的思路（防"每条查询都进日志"）。
- **安全口径（已拍板）**：bun v1.3 内联参数渲染，`event.Query` 含数据值——任务
  payload 可能带敏感内容。日志只记语句前 N 字节（N 为常量，取向同
  eventPreviewLimit）+ 阈值开关默认关，两道闸都不依赖使用方读文档。
- golden 交互：QueryHook 只在运行期挂，不影响快照测试的捕获 hook（两个 hook 并存，
  测试侧断言 hook 序列不互相污染）。

## 5. G6 语句超时（Q07）

- PG：`statement_timeout` 经 DSN options 注入（`store.statement_timeout`，默认 0=不设，
  启动日志 Warn 提示未配置）；SQLite 侧 busy_timeout 已有，文档对齐口径即可。
- **迁移连接排除（已拍板）**：超时只对业务连接生效——迁移/对账连接单独拼 DSN
  （不带 statement_timeout），避免启动期长迁移被杀。
- 与 P1 的关系：给 W 系列的"写次数下降"提供兜底——合并后单事务变大，超时上限先行。

## 6. W1 beginAttempt 并入 running 落盘（Q08）

- `UpdateWithEvents` 扩变体：一个事务内 upsert 快照 + 账本行 + 出箱行
  （`store/jobs/outbox.go` 的 upsertSnapshot/upsertAttempt/出箱 append 共用事务句柄）。
- 收口位置遵守 state.go 单通道原则：**transition 加变体**（Go 无可选参数，
  nil/bool 参数会让既有调用点读不清）——新变体 `transitionWithAttempt(job, to,
  attempt, events ...Event)`，executeJob 的 `transition(running)` + `beginAttempt`
  两次调用合成一次；transition 本体与其余调用点零改动。
- 直发模式（JSON 存储）无账本能力，路径保持逐字节不变；ledger nil 降级语义不变。
- 验证：golden 快照 diff 恰好体现"两条语句并入一个事务"（语句序列变化逐行评审）；
  faultinject 补"事务中途失败回滚"用例（对齐 `TestUpdateWithEvents_RollsBackOnMidTxFailure`）。

## 7. W2 completed 事件与终态快照同事务（Q09）

- 现状：成功路径 `emit(completed)`（独立 AppendOutbox 事务）→ `handleSuccess` →
  `transition(success)`（又一次 UpdateWithEvents）。出箱模式下事件本就有中继延迟，
  合并成一个事务反而更原子（事件与结论同生共死）。
- 改法：成功分支不提前 emit，completed 事件作为 `transition(success, completedEvent)`
  的随行事件进同一事务。**事件语义变化**：completed 事件从"Handler 返回即发"变为
  "终态落盘时随发"——出箱模式下两者间隔本就是一个中继周期，订阅端无感；
  duration_ms 在事件组装时已按 job.UpdatedAt 算定，不受 transition 改 UpdatedAt 影响。
- **范围（已拍板）**：仅成功路径合并。失败路径有重试分支（不落终态），事件与
  排期落盘的关系另议，本批不动——失败路径的 emit/transition 调用保持现状。
- 直发模式保持现状（事件先于终态落盘的既有顺序语义不动）。

## 8. W3 PruneAttempts 降频（Q10）

- 现状：每次 `finishAttempt` 后跑一次 DELETE 子查询（`store/jobs/attempts.go:132`），
  cron 高频任务每轮空跑一次。
- **触发方式（已拍板）**：按 seq 取模——`seq % attemptsKeepLast == 0` 时才 prune。
  不加后台协程（与本项目取向一致），行为可从账本行 seq 推导；不恰逢取模点的
  终态收尾不额外补裁，超窗幅度有界（≤ keepLast 行）且读侧 Attempts 按 limit 取
  最近 N 条不受影响。
- 语义约束：任意时刻账本行数 ≤ keepLast + 未裁窗口（临时超窗是可接受的中间态，
  读侧 Attempts 按 limit 取最近 N 条，不受影响）。

## 9. G7 迁移纪律守卫（Q11）

- 三条规则，storeguard 同构（测试期红线，非 CI 单独 job）：
  1. 版本清单严格递增不重复（对代码内迁移清单做静态校验）；
  2. `UpPostgres` 中出现 `AUTOINCREMENT` 的语句必须配 IDENTITY（Q04 教训）；
  3. version 1 的语句文本 golden 逐字节冻结（防"顺手整理"破坏既有部署的判定）。
- 白名单/例外机制沿用 storeguard 的表驱动写法。

## 10. 验证口径

- 全卡共同：`go build ./... && go vet ./... && go test ./...`；配
  `GODELAYQ_TEST_PG_DSN` 时 PG 侧真跑；守卫类改动做"人为违规必红"自证；
  golden 类改动做"两次生成零 diff"自证。
- W1/W2：SQL 快照 diff 逐行评审（预期语句序列变化之外零 diff）；faultinject
  事务回滚用例绿。
- W 系列前后对比：G4 读数（每执行写事务数）作为证据——门禁先行的意义。

## 11. 实施计划

| 卡 | 内容 | 完成态 |
| --- | --- | --- |
| Q00 | 本设计文档定稿 + 任务卡目录 | 方案可执行 |
| Q05 | G4 可观测读数（含事件丢弃计数） | /admin/runtime 可见 DB 占用与丢弃 |
| Q06 | G5 慢查询日志 | 阈值超限 Warn 可查，默认关 |
| Q07 | G6 语句超时 | PG DSN 可配、缺省 Warn |
| Q08 | W1 账本 begin 并入 running 落盘 | 热路径 6 写 → 5 写，golden diff 评审过 |
| Q09 | W2 completed 与终态同事务 | 成功路径再省 1 事务，出箱原子性增强 |
| Q10 | W3 prune 降频 | cron 任务消除空跑 DELETE |
| Q11 | G7 迁移纪律守卫 | 三条规则进测试期红线 |

依赖：Q05/Q06 先于 Q08–Q10（基线证据）；Q07 与 W 系列无强依赖可并行；
Q05–Q07 相互独立。每卡一个提交，Conventional Commits 中文。

## 12. 决议记录（2026-11-09 拍板）

1. W1 收口签名：**transition 加变体**（§6）。
2. W2 失败路径：**仅成功路径合并**，失败路径保持现状另议（§7）。
3. W3 触发方式：**seq 取模**，不加后台协程（§8）。
4. G5 SQL 掩码：**截断前 N 字节 + 默认关**（§4）。
5. G6 超时：**排除迁移连接**，只对业务连接生效（§5）。
