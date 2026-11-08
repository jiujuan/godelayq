# Q08 W1 账本 begin 并入 running 落盘

## 1. 任务目标

`executeJob` 的 `transition(running)` + `beginAttempt` 两次独立事务合成一次：
出箱模式下快照、账本行、出箱行同事务落盘。热路径写次数 6 → 5。

## 2. 背景与当前问题

成功路径在 worker 协程上串行 6 次独立事务（设计文档 §1 表格），SQLite 单写者下
全局串行，到期风暴时执行吞吐上限在写而不在 Handler。begin 行与 running 快照
语义上同生同灭（"这次尝试开始跑了"），没有理由分两个事务。

## 3. 要实现的功能

- store：`UpdateWithEvents` 增加带账本行的变体（快照 upsert + attempt upsert +
  出箱 append 共用同一事务句柄，`store/jobs/outbox.go`）；
- core：state.go 新增收口变体 **`transitionWithAttempt(job, to, attempt,
  events ...Event)`**（已拍板：加变体，Go 无可选参数，nil/bool 参数读不清）；
  `transition` 本体与其余调用点零改动；
- `executeJob`：running 落盘与 beginAttempt 合成一次调用；ledger nil（JSON 部署）
  降级路径不变，直发模式行为逐字节不变；
- 失败语义：事务中途失败整体回滚（快照、账本行、出箱行一起消失），错误处理对齐
  既有 `UpdateWithEvents` 的日志与返回口径。

## 4. 实现步骤

1. store 变体 + 单测；2. transitionWithAttempt 收口；3. executeJob 接线；
4. golden 重生成 + diff 评审；5. faultinject 用例。

## 5. 测试要求

- SQL 快照：sqlite/postgres 两 golden 重生成，diff 恰好体现"两条语句并入一个
  事务"，逐行评审记录进本卡；
- faultinject：事务中途失败回滚用例（对齐 `TestUpdateWithEvents_RollsBackOnMidTxFailure`
  的形状，扩账本行）；
- 降级：JSON 部署既有测试零改动通过；
- Q05 读数佐证：每执行写事务次数下降（有 /admin/runtime 前后对比记录）。

## 6. 完成标准(DoD)

合并落地、两方言 golden diff 评审过、faultinject 绿、直发模式零变化。

## 7. 验收方式

```
go test -run TestSQLSnapshot -update   # 重生成后 diff 评审
go build ./... && go vet ./... && go test -count=1 ./...
GODELAYQ_TEST_PG_DSN='...' go test -count=1 ./internal/store/... ./cmd/server/
```

## 8. 不在本任务范围

finishAttempt/prune 的合并（Q09/Q10）、失败路径合并、单写协程批量提交（W4）。

## 9. 风险与回滚

单事务变大——Q07 的语句超时先行兜底；快照与账本行同事务后，账本行丢失面与
快照一致（原来是账本单点失败不影响快照，新语义是同生共死）——这恰是合并的目的，
注释写明。回退：revert 单提交，golden 随之回滚。

## 10. 实现记录

- 待实施。
