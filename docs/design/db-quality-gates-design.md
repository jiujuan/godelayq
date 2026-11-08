# godelayq 数据库质量门禁设计（P0：CI + 红线守卫 + SQL 快照）

> 状态：定稿（2026-11-08）。任务卡见 `tasks/db-quality-gates/`。
> 关联：`bun-store-design.md`（存储层双方言化的来源）、`../../architecture.md`。
> P1/P2（可观测、慢查询、语句超时、迁移纪律、schema 契约、基准、runbook）不在本批，留卡待议。

## 1. 背景与目标

存储层 bun 化（feat/store-bun）之后，质量风险画像变了：SQL 由 bun 生成（`Order(expr)`
引号陷阱已实际出现）、查询层双方言共用、bun 升级可能悄悄改变生成文本、业务包留有
`SQL()` 逃生口可被后人当捷径。现有测试矩阵很厚，但**只在有人记得跑的时候存在**——
仓库没有 CI。

目标：给数据库层立三道 P0 门禁，全部沿用现有工程文化（parser 守卫、golden 文件、
一卡一提交）：

1. **CI 流水线**——让既有测试矩阵自动跑：SQLite 快档（ubuntu+windows）+ PG 全矩阵
   （services postgres:17）。
2. **红线守卫扩展**——防 bun 化成果被回潮：`SQL()` 逃生口白名单、方言分支白名单、
   动态 SQL 拼接禁令。
3. **SQL 快照门禁**——把 bun 生成的 SQL 文本钉进 golden：升级与改动的 diff 逐行可见。

非目标：不引入 linter（守卫走 parser/golang ast，与既有 package_guard 同构）、
不做性能阈值门禁、不接覆盖率。

## 2. 现状与缺口

| 已有（B00–B07 交付） | 缺口 |
| --- | --- |
| 双方言测试矩阵（jobs/mgr/observe/engine/pgtest/cmd/server） | 无 CI，矩阵靠人肉触发 |
| package_guard：engine 唯一驱动入口、业务包不碰驱动 | `SQL()` 逃生口、方言分支、动态拼接无守卫 |
| 行为断言（往返/淘汰/过滤/对账/幂等） | 生成的 SQL 文本无任何钉住手段 |
| 配置校验、example 同步、env 分类守卫 | — |

## 3. G1 CI 流水线（Q01）

`.github/workflows/ci.yml`，两个 job：

- `sqlite-fast`：ubuntu-latest + windows-latest 矩阵，`go build/vet/test -count=1 ./...`；
  PG 用例因未配 DSN 自然跳过——快档不依赖任何服务。
- `postgres-matrix`：ubuntu-latest + `services: postgres:17`（健康检查 pg_isready），
  `GODELAYQ_TEST_PG_DSN` 从 service 连接串拼出，跑
  `go test -count=1 ./internal/store/... ./cmd/server/`——与本地 `scripts/test-pg.ps1|sh`
  同一口径，门禁只有一份。

go 版本经 `go-version-file: go.mod` 取自声明；两个 job 各 15 分钟超时；同 ref 并发取消。
首次真跑发生在推送/合并后，本卡以 YAML 语法检查 + 本地等价命令验证。

## 4. G2 红线守卫（Q02）

新增测试辅助包 `internal/store/storeguard`（与 pgtest 同类：只被 _test 引用），
AST 级三条规则，各业务包的 `package_guard_test.go` 调用：

| 规则 | 内容 | 白名单 |
| --- | --- | --- |
| `GuardNoSQL` | 生产文件不得出现 `.SQL()` 逃生口调用 | observe `db.go`（Stats 原生可移植语句） |
| `GuardNoDialectBranch` | 生产文件不得引用 `DialectPostgres`/`DialectSQLite` 标识符 | mgr `models.go`（nameConflictClause 唯一分支点） |
| `GuardNoDynamicSQL` | 构建器链（Where/WhereExpr/On/Set/Order/OrderExpr/ColumnExpr/GroupExpr/Having/Exec…）的参数不得直接是 `fmt.Sprintf(...)` 调用 | 无 |

规则只扫生产文件（非 `_test.go`），与既有守卫同一取向：违规在编译期之前被点名，
而不是等一次线上事故。已知局限写进注释：经变量中转的拼接拦不住，门禁抓的是
"直接把 Sprintf 塞进构建器"这一类高频捷径。

## 5. G3 SQL 快照门禁（Q03）

**捕获机制**：bun `QueryHook`——`BeforeQuery` 阶段 `event.Query` 已是带方言占位符的
完整 SQL 文本。测试给真实句柄挂一个记录 hook，**经由生产 API**（store.Save/Update/
LoadAll/ImportOnce、AuditLog.Append+Query、ArtifactIndex.Record…）触发全部查询，
把语句序列与 golden 文件比对。生产代码怎么拼 SQL，快照就是什么——零复制、零漂移。

**形态**：每包一个 `sql_snapshot_test.go`，`t.Run("sqlite"/"postgres")` 两份 golden
（`testdata/sql_snapshot_<dialect>.golden`）；PG 侧走 pgtest 门控，未配 DSN 跳过。
更新通道：`go test -run TestSQLSnapshot -update` 重写 golden。

**钉住什么**：占位符风格（`?` vs `$N`）、EXCLUDED 大小写、ORDER BY 表达式引号、
`lower(name)` 冲突目标、IDENTITY 列省略、prune 子查询文本。bun 升级或改动查询时
diff 逐行评审。

**确定性保障**：操作序列固定；golden 里只有语句文本、无参数值（时间戳等都是占位符
参数）；ImportOnce 等走 map 的路径用单条目入参；批量器经显式 Flush 同步落盘。

## 6. 验证口径

- 每卡：`go build ./... && go vet ./... && go test ./...` 全绿（无 DSN 时 PG 侧跳过）。
- Q02：守卫对现有代码零误报；人为制造一次违规（临时）确认能红，随后还原。
- Q03：golden 生成→重跑稳定（两次连续生成无 diff）；`-update` 后 diff 只含预期变更。
- 终验：本机 PG 全矩阵 + `GODELAYQ_TEST_PG_DSN` 注入下快照测试过。

## 7. 实施计划

| 卡 | 内容 | 完成态 |
| --- | --- | --- |
| Q00 | 本设计文档 + 任务卡 | 方案可执行 |
| Q01 | ci.yml：sqlite-fast ×2 OS + postgres-matrix | CI 就绪（首跑在推送后） |
| Q02 | storeguard 包 + 三包守卫接线 | 三条红线进测试期 |
| Q03 | 三包 SQL 快照 + golden | bun 生成文本被钉住 |
