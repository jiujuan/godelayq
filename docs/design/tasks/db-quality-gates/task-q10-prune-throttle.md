# Q10 W3 PruneAttempts 降频

## 1. 任务目标

`finishAttempt` 后的 `PruneAttempts` 改为 **seq 取模触发**（已拍板）：
`seq % attemptsKeepLast == 0` 时才执行，其余收尾不再跑这条 DELETE。

## 2. 背景与当前问题

每次尝试收尾都跑一次 prune 子查询 DELETE（`internal/store/jobs/attempts.go`
PruneAttempts，主键前缀索引代价为常数但非零）。cron 高频任务每轮 finish 都空跑
一次——表里几乎永远没有可删的行。热路径 4 → 3.25 写（取模点仍触发）。

## 3. 要实现的功能

- `finishAttempt`（scheduler_exec.go）内加取模判定：`job.Attempts %
  s.attemptsKeepLast == 0` 才调 `PruneAttempts`；
- 不加后台协程（与本项目取向一致）；行为可从账本行 seq 推导（可复现、可解释）；
- 不恰逢取模点的终态收尾不补裁：超窗幅度有界（≤ keepLast 行/任务），读侧
  `Attempts` 按 limit 取最近 N 条不受影响；
- 语义约束写注释：任意时刻单任务账本行数 ≤ keepLast + 未裁窗口；
- Job 删除路径（Delete 随任务删账本行）不变。

## 4. 实现步骤

1. 取模判定 + 注释；2. 单测（seq 序列下 prune 调用次数断言）；3. golden 重生成 +
   diff 评审。

## 5. 测试要求

- 序列断言：连续 N 次 finish，prune DELETE 恰在取模点出现（可用 SQL 快照或
  QueryHook 计数）；
- 窗口上界：keepLast=k 时任意时刻行数 ≤ 2k（含未裁窗口）；
- 取模点恰逢终态：最后一次 finish 在取模点上时仍正常裁剪；
- SQL 快照重生成 diff 逐行评审记录进本卡。

## 6. 完成标准(DoD)

cron 路径空跑 DELETE 消除（快照/计数佐证）；窗口上界有测试；golden diff 评审过。

## 7. 验收方式

```
go test -run TestSQLSnapshot -update
go build ./... && go vet ./... && go test -count=1 ./...
GODELAYQ_TEST_PG_DSN='...' go test -count=1 ./internal/store/... ./cmd/server/
```

## 8. 不在本任务范围

后台批量 prune（已拍板否决）、账本表全局对账、attemptsKeepLast 热更。

## 9. 风险与回滚

超窗中间态被误读为"账本泄漏"——读侧口径（Attempts limit）与注释共同兜住；
取模判定放错位置（如放 store 层）会把语义拆到两处——收口在 finishAttempt。
回退：revert 单提交。

## 10. 实现记录

- 2026-11-09 完成。规则落点：`finishAttempt`（core/scheduler_exec.go）——
  `job.Attempts % attemptsKeepLast == 0` 才调 `PruneAttempts`；除数恒 ≥1 的依据
  写在注释里（ledger 非 nil 时构造器已把 attemptsKeepLast 折成
  DefaultAttemptsKeepLast）。不加后台协程；函数注释写明上界（keepLast + 未裁
  窗口 ≤ 2×keepLast）与"终态不逢取模不补裁"的口径。Job 删除路径未动
  （store.Delete 的账本级联照旧）。
- 测试：core 位置断言（keepLast=3、seq 1..7 逐 seq 累计 prune 次数恰为
  0,0,1,1,1,2,2；seq=9 取模点恰逢终态照常裁剪）——mockLedger 记的是 keepLast
  不是 seq，位置用"逐 seq 的累计次数"表达；jobs 窗口上界（真实 store SQL，
  keepLast=3 跑 9 个 seq，任意时刻 ≤ 2×3，终态行数收回 k，双方言门控）。
- golden：**零 diff**（prune 的 DELETE 语句形状不变，仍由快照序列里显式的
  PruneAttempts 直调钉住；重生成后比对确认）。
- 自证：全量 9 包 EXIT=0；本机 PG 矩阵 6 包 EXIT=0。
