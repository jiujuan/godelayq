# Q09 W2 completed 事件与终态快照同事务

## 1. 任务目标

非 cron 任务的成功路径：`emit(completed)`（独立 AppendOutbox 事务）与
`transition(success)`（UpdateWithEvents 事务）合成一次——completed 事件作为
终态转移的随行事件进同一事务。热路径写次数 5 → 4。

## 2. 背景与当前问题

出箱模式下事件本就有中继延迟（可见延迟上界 = 一个中继周期），
"完成结论"与"终态快照"分两个事务没有原子性收益，只有写放大。
已拍板：仅成功路径合并；失败路径有重试分支（不落终态），本批不动。

## 3. 要实现的功能

- `executeJob` 成功分支不再提前 emit completed，事件组装下沉到 `handleSuccess`：
  - **终态路径**（非 cron）：completed 事件作为 `transition(success, ev)` 的随行
    事件，与终态快照同一事务（出箱模式）；
  - **旁路路径保持独立 emit**：`parkForcedPause` 改判 paused、cron 重排分支——
    这两条没有终态转移，completed 事件照旧单独发（事件形状与顺序逐字节不变）；
- metadata.duration_ms 在事件组装时按 job.UpdatedAt 算定，不受 transition 改
  UpdatedAt 影响（现状已如此，注释钉住）；
- 直发（JSON）模式保持现状：事件先于终态落盘的既有顺序语义不动；
- 事件语义变化写进注释：出箱模式下 completed 从"Handler 返回即发"变为
  "终态落盘时随发"，订阅端可见延迟不变（中继周期兜着）。

## 4. 实现步骤

1. handleSuccess 事件组装重构；2. executeJob 成功分支接线；3. golden 重生成 +
   diff 评审；4. 事件顺序测试补断言。

## 5. 测试要求

- 出箱模式：completed 与终态快照同事务（faultinject 证"一起在或一起不在"）；
- 事件序列：started → completed → (scheduled|无) 顺序不变；outbox seq 单调；
- 旁路：强制暂停改判与 cron 重排路径的事件形状/顺序与改动前一致（既有测试零改动
  通过为证）；
- SQL 快照：两 golden 重生成，diff 逐行评审记录进本卡。

## 6. 完成标准(DoD)

非 cron 成功路径事件与终态同事务；旁路与直发模式零变化；golden diff 评审过。

## 7. 验收方式

```
go test -run TestSQLSnapshot -update
go build ./... && go vet ./... && go test -count=1 ./...
GODELAYQ_TEST_PG_DSN='...' go test -count=1 ./internal/store/... ./cmd/server/
```

## 8. 不在本任务范围

失败路径（failed 事件 + 终态/重试）的合并——另议；cron 重排路径的 completed
与 scheduled 合并（两个事实分属两轮，语义上本就分开，不做）。

## 9. 风险与回滚

旁路路径漏改会让 completed 事件丢失或重复——三条路径（终态/强制暂停/cron）各有
既有测试钉住形状，改动必须全绿；事件发布时点后移在出箱模式下无感（中继周期），
直发模式未动。回退：revert 单提交。

## 10. 实现记录

- 待实施。
