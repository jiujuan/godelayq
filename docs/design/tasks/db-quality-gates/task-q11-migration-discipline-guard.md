# Q11 G7 迁移纪律守卫

## 1. 任务目标

storeguard 增加三条迁移纪律规则（测试期红线）：版本递增不重复、
`UpPostgres` 中 AUTOINCREMENT 必须配 IDENTITY、version 1 语句文本 golden 逐字节冻结。

## 2. 背景与当前问题

Q04（CI 首跑）暴露过 PG 方言可移植性问题——SQLite 专属写法混进迁移。
迁移清单靠人眼维护：版本号写重、v1 语句被"顺手整理"，都是静默事故
（版本判定错乱 / 既有部署迁移重放）。

## 3. 要实现的功能

storeguard 表驱动三条规则，store 包 `package_guard_test.go` 接线：

| 规则 | 内容 | 白名单 |
| --- | --- | --- |
| 版本递增 | 代码内迁移清单严格递增、无重复版本号 | 无 |
| 方言可移植 | `UpPostgres` 语句出现 `AUTOINCREMENT` 时必须同句含 `IDENTITY`（Q04 教训的泛化） | 无 |
| v1 冻结 | version 1 的 Up/Down 语句文本 golden 逐字节比对（`testdata/` + `-update` 通道） | 无 |

- 实现层面与既有守卫同构：静态扫描（清单遍历/字符串断言），不引 linter、
  不做运行时校验；
- 规则注释写明"防的是哪类事故"，与 GuardNoSQL 等既有规则同口径。

## 4. 实现步骤

1. 三规则实现 + golden；2. store 包接线；3. 人为违规必红自证；4. README 规则表同步。

## 5. 测试要求

- 每条规则"人为违规必红"自证（临时改动清单/语句，确认红后还原，记录进本卡）；
- golden 两次生成零 diff；
- 既有迁移清单全部通过（零误报是验收底线）。

## 6. 完成标准(DoD)

三条规则进测试期红线；自证记录齐全；零误报。

## 7. 验收方式

```
go build ./... && go vet ./... && go test -count=1 ./...
go test -run TestPackageGuard -count=1 ./internal/store/...   # 规则接线确认
```

## 8. 不在本任务范围

运行时迁移校验、跨方言 schema 契约测试（G8/P2）、迁移框架更换。

## 9. 风险与回滚

字符串级规则对格式敏感（大小写/空白）——规则注释写明匹配口径；v1 golden 在
合法重构时需要显式 `-update` + diff 评审，误报面由白名单机制兜住。
回退：摘规则，无生产代码影响。

## 10. 实现记录

- 2026-11-09 完成。storeguard/migration.go：三条规则做成 **Check\*（返回问题清单）
  + Require\*（接线包装）** 两层——与卡面"临时改动清单确认红后还原"的自证方式
  偏差：自证改为**构造坏输入直接断言问题非空的永久用例**（storeguard/migration_test.go
  四个用例），比临时改文件再还原更可靠且留作长期回归；接线面不变（各包
  package_guard_test 调 Require\*）。
- 三规则与范围：版本从 1 起步、严格递增无重复；UpPostgres 出现 AUTOINCREMENT
  必须同句含 IDENTITY（大小写不敏感；**Up 通用语句集不查**——那里写 SQLite 专属
  语法是 PG 迁移时的响亮故障，不是静默事故）；v1 冻结面是 Up + 非空 UpPostgres
  （engine.Migration 没有 Down——迁移只进不退，回滚靠备份，卡面的"Up/Down"
  以实现为准）。golden 文本为确定性折行（节标题 + 每语句一行）。
- 接线：jobs/mgr/observe 三包 package_guard_test 各加 Test*MigrationDiscipline，
  复用各包 SQL 快照的 `-update` 标志（同一测试二进制不许重复注册 flag）；
  golden `testdata/migration_v1.golden` × 3 已生成，重写通道
  `go test ./internal/store/<pkg>/ -run TestPackageGuard -update`。
- 零误报验证：三包既有迁移清单全部通过（jobs v1–v4 / mgr / observe）；
  storeguard 自证用例含每条规则的"既有形态必须通过"配对用例。
- 自证：storeguard 4 用例绿（含重复/乱序/非正版本、裸 AUTOINCREMENT、v1 漂移、
  缺 golden 报错、空清单零误报）；全量 10 包 EXIT=0；本机 PG 矩阵 7 包 EXIT=0。
