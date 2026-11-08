# TASK-RE08　故障注入测试与文档同步

- 所属阶段：可靠执行（收口卡）
- 依赖任务：RE01–RE07 全部
- 涉及文件：新增 `core/faultinject_test.go`、`store/jobs/fault_test.go`、`cmd/server/faultinject_integration_test.go`；改 `.github/workflows/ci.yml`（如需）、`docs/architecture.md`、`docs/api.md`、`docs/deployment.md`、`docs/design/reliable-execution-design.md`（回填实现差异）
- 预计规模：中

## 1. 任务目标

用成套故障注入把八点能力的可靠性主张变成可回归的证据；三份使用者文档与实现对齐。

## 2. 背景与当前问题

RE01–RE07 各卡自带单测，但跨组件的故障行为（写失败时的状态一致性、崩溃点的账本/事件完整性、关停竞态）没有统一覆盖；`docs/architecture.md` 的主链路/恢复/事件三节描述的还是改造前的世界。故障注入不引外部依赖（不进 failpoint 类库），用测试替身。

## 3. 要实现的功能

### 3.1 故障注入替身（core 内测试文件）

- `flakyStore`：包装 `core.Store`，按"第 N 次调用某方法"注入失败（`failOn map[string]int`），透传其余调用——注入点：Save/Update/Delete/LoadAll。
- `flakyLedger` / `flakyOutbox`：同形包装 `AttemptLedger`/`EventOutbox`。
- 假时钟：misfire/grace 判定的 `now` 注入（若实现未留注入点，本卡补最小接口，不改行为）。

### 3.2 故障用例矩阵（全部进 CI 两档）

| 类别 | 用例 | 断言 |
| --- | --- | --- |
| 崩溃点 | 快照停 running / pending，× 策略 replay/pause/auto，× 类别 default/exec | Restore 矩阵结果正确；abandoned 收口发生（RE05） |
| 存储故障 | `persistAndEmit` 时 store.Update 失败；handleFailure 时失败；BeginAttempt 失败 | 任务不丢（堆/存储口径一致）、错误日志可见、调度循环不死 |
| 事件丢失 | EventLog 队列塞满触发丢弃 | `Dropped` 计数正确；outbox 模式下 `job_events` 仍完整（去重 + 补投） |
| Outbox | `UpdateWithEvents` 中途失败；relay 重放；重复重放 | 事务回滚；顺序 = seq；`job_events` 无重复行（RE07 用例的集成版） |
| 时钟 | misfire grace 边界；时钟回拨后 TriggerAt 判定 | 事件不误刷；回拨不产生负时长忙等 |
| 关停 | 优雅关停 vs 中途 cancel 请求在途；relay.Stop 与写入器 Close 的顺序 | 任务落 pending/cancelled 口径正确；无"发布到已关闭通道"panic（竞态窗口至少跑一次 -count=10） |
| 幂等 | 登记失败后的重发；指纹碰撞 | 降级行为符合 RE03 注释声明 |
| 幂等键 TTL | 过期键重发 | 允许新建（预期），淘汰发生 |

### 3.3 文档同步

- `docs/architecture.md`：§6.2 主链路（persistAndEmit 收口、attempt 挂点）、§6.5 崩溃恢复（策略配置化 + abandoned）、§6.7 观测（outbox 中继与去重）、§1 分层图与模块清单（`store/jobs` 职责行加四表）、§7 设计要点补"状态机与出箱"两条。
- `docs/api.md`：`Idempotency-Key`/`job_id`/`timezone`/`misfire_policy` 请求参数；`GET /jobs/:id/attempts`；**Cancel 后 404 → cancelled 的行为变更**单独立一条"变更记录"。
- `docs/deployment.md`：§9 全部新配置键 + 调参建议（grace、outbox 周期）；JSON 模式能力边界一节。
- 设计文档 §13 追加"实现差异"小节（各卡实现记录的汇总链接）。

## 4. 实现步骤

1. 替身与注入点（若 core 缺时钟注入点，最小补充）。
2. 用例矩阵逐行落地（每行一个具名测试，失败输出指明注入点）。
3. CI 确认：sqlite-fast 档全跑；postgres-matrix 档 PG 用例真跑（不是跳过）。
4. 文档四处同步 + 设计文档回填。

## 5. 测试要求

本卡自身就是测试卡：矩阵表每行都有具名测试函数；文档零漂移靠引用核对（不改行为）。

## 6. 完成标准（DoD）

1. 矩阵全绿（sqlite 本机 + PG 本机/CI）；`go test -count=10` 下关停竞态用例稳定。
2. 三份文档与实现一致；`docs/api.md` 的行为变更条目存在。
3. 设计文档回填"实现差异"。

## 7. 验收方式

CI 两档全绿；本地跑一次完整验收剧本（设计文档 §12 第 9 条的 JSON 模式回归 + §12 全清单抽查）。

## 8. 不在本任务范围

混沌工程级注入（网络分区、磁盘满的真实环境）、控制台前端改造、性能基准套件（压力数据只在各卡实现记录里随手留）。

## 9. 风险与回滚

- **风险：故障用例对实现细节过拟合**（重构即碎）。缓解：替身注入在接口边界（core.Store/AttemptLedger/EventOutbox），不摸私有状态；断言只写对外可观察行为（状态、日志键、计数）。
- **风险：-race 本机跑不了（TSan 环境问题），竞态用例本地验证不充分**。缓解：竞态用例 `-count=10` 重复跑；-race 交给 CI。
- 回滚：纯测试与文档，revert 无副作用。

## 10. 实现记录

（待实现后回填。）
