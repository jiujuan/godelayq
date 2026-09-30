# TASK-S02　`store/sqlite` 包与启动装配

- 所属阶段：M0 基础
- 依赖任务：TASK-S01
- 涉及文件：新增 `store/sqlite/doc.go`、`store/sqlite/db.go`、`store/sqlite/schema.go`、`store/sqlite/batch.go`、对应 `_test.go`；改 `cmd/server/main.go`、`cmd/server/main_test.go`、`cmd/server/main_integration_test.go`、`go.mod`、`go.sum`
- 预计规模：中

## 1. 任务目标

建起全仓唯一使用 SQLite 的包，能打开库文件、设置并发与持久化参数、建三张表和迁移版本表、随进程干净关闭；并在 `cmd/server` 里按 `observability.enabled` 装配与关闭。本卡结束时三张表都没有读写方。

## 2. 背景与当前问题

设计文档 §5 的依赖边界要有唯一的落点：`core` 现在不认识任何存储驱动（`core/store.go` 只用标准库 `os`/`encoding/json`），`examples/demo1`、`demo2` 直接构造 `core.Scheduler`，一旦 `core` import 了驱动，这两个示例和全部 `core` 测试都会被动继承一个 CGO/大体积依赖。

装配侧也有一个既有惯例要照抄：`cmd/server/main.go` 的 `runtimeDeps` 用闭包提供每个重依赖，并在 `run()` 开头做依赖完整性检查（`main.go:154-157`）——`newExecutorRegistry` 与 `newArtifactStore` 的注释都写着"缺了它会静默不生效，所以列入检查"。观测层同理：少装配一个闭包的后果是"三张表永远是空的"，看起来正常、实际没记，必须在启动期就报错。

关闭顺序是另一处已有先例：`main.go:221-227` 用 `defer` 的声明顺序保证产物清理协程先退、存储后关，注释明确写了原因（否则协程可能在存储已关闭后再读一次任务集合）。观测层的撤订阅与关闭要排在同一批 `defer` 里，理由一致（设计文档 §7.3）。

## 3. 要实现的功能

1. 模块依赖：`go get modernc.org/sqlite`，它是 `database/sql` 的驱动实现，注册名 `"sqlite"`。禁止引入 `mattn/go-sqlite3`（需要 C 工具链，见设计文档 D2）。
2. `store/sqlite/doc.go`：包注释写清三件事——本包是全仓唯一 import SQLite 驱动的包；接口定义在消费方（`api`、`executor`），本包只提供实现；`core` 不依赖本包。
3. `type DB struct` 与构造函数：

   ```go
   // Open 建好父目录、打开连接、设置 PRAGMA、建表并应用迁移。
   // 建不起库等于什么都记不住，因此任何一步失败都返回错误，由装配方决定是否终止启动。
   func Open(cfg core.ObservabilityConfig, logger *slog.Logger) (*DB, error)
   func (d *DB) Close() error          // 幂等；重复调用返回 nil
   func (d *DB) Path() string
   func (d *DB) Stats() Stats          // 三张表行数、迁移版本、各写入器的 dropped 累计
   type Stats struct { Events, Artifacts, AuditRows int64; DroppedEvents, DroppedAudit int64; SchemaVersion int }
   ```

   内部持 `*sql.DB`，显式 `SetMaxOpenConns(1)`（设计文档 §7.2：写入是批量单点、读是短查询，一个连接足够并消除 `SQLITE_BUSY` 的主要来源）。
4. PRAGMA（连接建立后立刻执行，取值来自 S01 的配置）：

   ```
   journal_mode = WAL
   synchronous  = normal | full        -- observability.synchronous
   busy_timeout = <BusyTimeout 的毫秒数>
   foreign_keys = ON
   ```

   `synchronous=normal` 的持久化含义要写进注释：WAL + NORMAL 下断电最多丢最后若干次已提交事务，与 `core/store.go:17-19` 记录的"崩溃最多丢失一个合并周期"是同一量级的保证；需要更强保证的部署改配 `full`。
5. `schema.go`：三张表的 DDL 原样落进本包（`job_events`、`artifact_index`、`write_audit`，见设计文档 §6），列注释保留；另外建 `observe_schema_migrations(version INTEGER, applied_at INTEGER)`。
   迁移方式是"按版本号顺序执行内置语句切片"，本卡只写 `version 1`（建全部三张表 + 6 个索引）。**表名与索引名与设计文档 §6 逐字一致，S03/S05/S06 不再建表。**
   迁移版本表带 `observe_` 前缀：将来若把任务快照也换成 SQLite（设计文档 §4.3），两张迁移表不能撞名。
6. `batch.go`：三张表共用的批量写入骨架，本卡就要写完（S03/S06 各自只有一个写入器，重复实现三遍必然分岔）：

   ```go
   type batcher[T any] struct { ... }   // 或等价非泛型实现，见下方说明
   func newBatcher[T any](capacity int, interval time.Duration, flush func([]T) error, onError func(error)) *batcher[T]
   func (b *batcher[T]) append(item T) bool   // true=入队成功；false=队满丢弃并计数
   func (b *batcher[T]) Flush() error         // 立即落盘；空批次不产生事务
   func (b *batcher[T]) Close() error         // 停止 ticker → 最后一次 Flush → 不重复执行
   func (b *batcher[T]) Dropped() int64
   ```

   三条硬要求：
   - `append` 不阻塞（用 `select` + `default`），队满即丢并累计计数——**绝不回压调用方**，理由见设计文档 D4。
   - 一次事务提交整批；失败时整批重新入队一次，二次失败则丢弃并记 error，不无限重试（磁盘满会让队列变成内存增长点）。
   - `Close` 幂等：`sync.Once` + 完成通道，`Flush` 在 `Close` 之后返回错误而不是 panic。
   
   Go 版本是 1.24（`go.mod`），泛型可用。若实现中发现泛型让调用点更难读，退回 `[]any` + 各表自己的转换函数也可以，但**要把这个选择记进第 10 节**，不要留成两套并存。
7. 装配（`cmd/server/main.go`）：
   - `runtimeDeps` 加字段 `newObservabilityDB func(core.Config, *slog.Logger) (*sqlite.DB, error)`，默认实现为 `sqlite.Open` 的包装（映射放在 `defaultRuntimeDeps` 里，与 `cfg.Store` → `StoreOptions` 的既有写法对称）。
   - 加入 `run()` 开头的依赖完整性检查。
   - `run()` 里在 `newServer` 之前创建；`enabled == false` 时不调用闭包、变量保持 nil。
   - `defer` 关闭顺序：观测层关闭排在 `store.Close` 的 `defer` 之前声明（因此实际后执行的是 store，先执行的是观测层收尾——与本卡 §2 说的先撤订阅读者再关存储一致）。
   - 启动日志一行：`observability enabled path=... schema_version=1`；关闭时不需要日志（`store.Close` 已经收尾）。
8. `newServer` 闭包签名暂不扩（本卡没有要注入的读者/写者）。S03 起再扩，避免一次改动传三个 nil 参数。

## 4. 实现步骤

1. 先写 `schema.go`：把设计文档 §6 的 DDL 原文粘进来，确认列名、索引名一致。
2. 写 `db.go` 的 `Open`/`Close`/`Stats` + 迁移，配单测（用 `t.TempDir()`）。
3. 写 `batch.go`，配单测（含队满丢弃、失败重试一次、`Close` 幂等三条）。
4. 最后改 `cmd/server`：加闭包、加检查、加 `defer`，改两个测试文件里的 `runtimeDeps` 构造点。
5. 交叉编译验证：`GOOS=linux GOARCH=amd64 go build ./...`、`GOOS=darwin`，确认没有 CGO 引入（`go list -deps ./store/sqlite | grep -c cgo` 之类，只要构建通过即可）。

## 5. 测试要求

1. `TestOpen_CreatesFileAndSchema`：临时目录下不存在的 `observe.sqlite`，`Open` 后文件存在、`Stats().SchemaVersion == 1`、三张表用 `sqlite_master` 查询都能取到、6 个索引都在。
2. `TestOpen_ParentDirCreated`：`path` 指向两级不存在的目录，`Open` 成功而不是报错。
3. `TestOpen_InvalidPathRejected`：`path` 是已存在目录的路径 → 返回错误且不 panic。
4. `TestOpen_WALModeEnabled`：`PRAGMA journal_mode` 查询结果是 `wal`。
5. `TestOpen_SynchronousFull`：配置 `synchronous: full` 时 PRAGMA 生效（与默认 `normal` 对照）。
6. `TestIdempotentMigrations`：同一文件 `Open` → `Close` → 再 `Open`，不报错、`schema_version` 仍是 1、`observe_schema_migrations` 只有一行。
7. `TestClose_Idempotent`：连续 `Close()` 三次返回 nil。
8. `TestBatcher_DropsWhenFull`：容量 2、投 10 条，断言 `append` 返回 false 的次数是 8、`Dropped()==8`、**且调用方没有阻塞**（用带超时的 goroutine 断言，不用 sleep 等结果）。
9. `TestBatcher_FlushesInOneTransaction`：断言 `flush` 回调每批收到的是完整切片、且调用次数 ≤ 批数（空批不产生事务）。
10. `TestBatcher_RetryOnceThenGiveUp`：`flush` 首次返回错误、二次成功 → 数据最终落盘；两次都失败 → 数据丢弃、记 error、`Dropped()` 增加。
11. `TestBatcher_CloseFlushesRemaining`：`Close` 后剩余条目已交给 `flush`；`Close` 之后再 `append` 返回 false、`Flush` 返回错误不 panic。
12. `TestRun_ObservabilityDisabledOpensNothing`（`cmd/server`）：`enabled: false` 时 `newObservabilityDB` 闭包**从未被调用**（用置位的标志断言），且目标路径下没有文件产生。
13. `TestRun_ObservabilityOpenFailureStopsStartup`：闭包返回错误 → `run` 返回错误、`store.Close` 仍被调用（对照既有的 `TestRun_WithIncompleteDependencies` 体例）。
14. `TestRun_ObservabilityClosedBeforeStore`：在闭包里记录关闭顺序，断言观测层的 `Close` 早于 `store.Close`。

## 6. 完成标准（DoD）

- [ ] 默认配置（`observability.enabled: false`）下：闭包不被调用、不产生任何文件、`go test ./... -race` 与改动前一样全绿。
- [ ] `core`、`api`、`executor` 三个包里没有任何文件 import SQLite 驱动（用 `go list -deps` 或 grep 断言，并把检查方法写进第 10 节）。
- [ ] 表名、列名、索引名与设计文档 §6 逐字一致，S03/S05/S06 不需要再执行任何 DDL。
- [ ] 批量写入器满足"不阻塞调用方""失败重试一次""`Close` 幂等"三条，且三条都有独立用例。
- [ ] 库文件与父目录权限：目录 `0750`、文件 `0640`，与产物文件同一取向（Windows 上跳过权限断言并写明原因，照 `executor/artifact_test.go` 的既有处理）。
- [ ] `go.mod` 只新增 `modernc.org/sqlite` 及其间接依赖，没有引入第二套 SQLite 驱动。

## 7. 验收方式

```bash
go build ./... && go vet ./... && go test ./... -race
go test ./store/sqlite -v
go test -run TestExampleConfigMatchesLocal ./core
GOOS=linux go build ./... && GOOS=darwin go build ./...
go build -tags dashboard ./...
```

手工：临时在本机配置把 `observability.enabled: true`、`path` 指到系统临时目录，启动后确认
日志出现 `observability enabled`、目录下出现 `.sqlite` 与 `-wal`/`-shm` 旁文件、
`Ctrl-C` 停服后旁文件被合并回收（WAL checkpoint）；再改回 `false` 启动一次，确认不产生新文件。

## 8. 不在本任务范围

- 不写任何业务表的读写方法（S03/S05/S06）。
- 不订阅 EventBus（S03）。
- 不改 `core/store.go`、不改 `store.type` 的校验分支、不做 JSON → SQLite 的数据迁移（设计文档 §4.3）。
- 不加 SQL 层的连接池参数配置：`SetMaxOpenConns(1)` 本卡写死，需要放开时另议。
- 不做库文件的定期 `VACUUM`（备份口径写进 S07 的文档，交给运维）。
- 不引入 ORM。三张表都是手写 SQL，字段与 `core` 结构体一一对应，加 ORM 只会多一层需要维护的映射。

## 9. 风险与回滚

- 风险：`modernc.org/sqlite` 依赖树较大、二进制增加约 8~10MB。这是 D2 选择的代价，记进 `docs/deployment.md`（S07）。
- 风险：WAL 模式在部分网络文件系统上不可用（`journal_mode` 设置会静默退回 `delete` 或报错）。本卡不特殊处理，但 `Open` 要把实际生效的 `journal_mode` 记进启动日志的 debug 级，便于现场判断。
- 风险：`-race` 下 `database/sql` 与 ticker 的组合容易出偶发调度问题。所有用例用注入的时钟或显式 `Flush()`，不允许用 sleep 等结果（与 `executor/artifact_test.go` 的 `TestStart_StopsWithContext` 同一处理）。
- 回滚：本卡改动分三个可独立 revert 的提交——新增 `store/sqlite/`、`cmd/server` 装配、`go.mod`。回滚即全部删除，既有行为无残留。

## 10. 实现记录（执行时补写）
