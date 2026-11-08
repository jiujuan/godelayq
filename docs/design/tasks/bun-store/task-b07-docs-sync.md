# B07 文档同步与终验

## 1. 任务目标

README、architecture.md 与实现一致;PG 运维说明(启停、DSN、sslmode、连接池)落文档;
全量终验。

## 2. 背景与当前问题

README 明言"store/sqlite 是唯一引入驱动的包"、architecture.md 描述 sqlite 基座——改名
与双方言后均过期。

## 3. 要实现的功能

- README 存储章节:engine 包、双方言、PG 配置示例(凭据占位)、test-pg 脚本用法。
- architecture.md:store 层描述更新(engine/jobs/mgr/observe),接口表不变。
- `docs/design/bun-store-design.md` 回填实现差异(如 PG 冲突目标的最终形态)。
- 后续项记录:sqlite→PG 数据搬迁工具。

## 4. 实现步骤

1. 逐文件核对改述;2. 终验(下节);3. 本卡即收口提交。

## 5. 测试要求

`go build ./... && go vet ./... && go test -count=1 ./...`(含 PG 矩阵,凭据经 env)。

## 6. 完成标准(DoD)

文档与实现零出入,全量测试绿。

## 7. 验收方式

```
go test -count=1 ./...
grep -rn "store/sqlite" README.md docs/architecture.md   # 应无残留(历史设计文档除外)
```

## 8. 不在本任务范围

新功能、性能调优。

## 9. 风险与回滚

无;纯文档。

## 10. 实现记录

(执行时填写)
