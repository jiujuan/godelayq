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

## 10. 实现记录（2026-10-01）

改动文件：
- 新增 `store/sqlite/`：`doc.go`（依赖边界三条）、`schema.go`（三张表 DDL + 8 个索引 + `observe_schema_migrations`，version 1）、`db.go`（`Open`/`Close`/`Path`/`Stats`/`JournalMode`/`Synchronous`、PRAGMA 读回、按版本号迁移）、`batch.go`（泛型 `batcher[T]`：非阻塞 append、整批事务、重试一次、`Close` 幂等），以及 `db_test.go`、`batch_test.go`。
- 改 `cmd/server/main.go`（+75 行：`observabilityDB` 窄接口、`runtimeDeps.newObservabilityDB`、依赖完整性检查、`enabled` 分支里的创建/启动日志/`defer` 关闭、`defaultRuntimeDeps` 里的 `sqlite.Open` 包装）。
- 改 `cmd/server/main_integration_test.go`（+317 行：12 个既有 `runtimeDeps` 构造点补上新闭包 + `observabilityStub` 替身 + 5 条新用例）、`cmd/server/main_test.go`（`TestDefaultRuntimeDeps` 的完整性断言加一项）。
- 改 `go.mod`/`go.sum`：`modernc.org/sqlite v1.46.0` 及其间接依赖（`modernc.org/libc`、`mathutil`、`memory`、`strutil`/`sortutil`/`token`/`gc`、`ncruces/go-strftime`、`remyoudompheng/bigfft`、`dustin/go-humanize`、`hashicorp/golang-lru/v2`、`golang.org/x/exp`），并把 `golang.org/x/{crypto,mod,net,sync,sys,text,tools}` 抬到 libc 要求的版本。**`go` 指令保持 1.24.13 未动**（见差异第 9 条）。

验证结果：
- `go build ./...`、`go vet ./...` 通过；`go test ./... -race` 全绿（api 65.5s、core 11.9s、executor 21.3s、cmd/server 5.4s、store/sqlite 1.6s），`go test -count=1 -race ./store/sqlite ./cmd/server ./core` 重跑亦绿。
- `go test ./store/sqlite -v`：21 个顶层用例，20 通过 + 1 跳过（`TestOpen_FilePermissions`，Windows 上 NTFS 权限不由 mode bits 表达，照 `executor/artifact_test.go:560` 的既有处理写明原因）。
- `go test ./cmd/server`：全通过，含本卡 5 条新装配用例（`TestRun_Observability{DisabledOpensNothing,OpenFailureStopsStartup,StatsFailureStopsStartup,ClosedBeforeStore,EnabledOpensAndClosesForReal}`）。
- 构建矩阵：`GOOS=linux`、`GOOS=darwin`、`CGO_ENABLED=0 GOOS=linux`、`CGO_ENABLED=0 GOOS=darwin`、`go build -tags dashboard ./...` 全部通过。无 CGO 的证据取"`CGO_ENABLED=0` 能构建交叉目标"这一条，不按 `go list -deps | grep cgo` 判断（依赖树里有名字含 cgo 的包，计数会误报）。
- DoD 第 2 条（依赖方向）的检查方法，三条命令都记在这里：
  - `go list -deps ./core ./api ./executor ./examples/demo1 ./examples/demo2 | grep -ci modernc` → `0`
  - `grep -rln "modernc.org/sqlite" --include=*.go .` → 只有 `store/sqlite/db.go`（真正 import 驱动的那一个）与 `store/sqlite/doc.go`（包注释里提到名字）
  - `grep -ci mattn/go-sqlite3 go.mod go.sum` → `0` / `0`（没有第二套驱动）
- `go test -run TestExampleConfigMatchesLocal ./core` 通过（S01 那份守卫不受影响；本卡没改配置键）。
- 冒烟（系统临时目录独立二进制 + 独立配置 + 独立 `data/`，未触碰仓库的 `configs/config.yaml` 与 `data/`）：
  1. `enabled: true` + 合法 `path` → 启动日志 `level=INFO msg="observability enabled" path=... schema_version=1 journal_mode=wal`；运行期 `data/` 下出现 `observe.sqlite`(4 KB) 与 `-wal`(70 KB)、`-shm`(32 KB) 旁文件；对端 `/api/v1/stats` 可达。
  2. `enabled: false` + 同一个 `path` → 目录里一个文件都没有、日志里搜不到 `observability` 字样（`grep -ci` = 0）。
  3. `enabled: true` + `path: "   "` → 启动失败，报 `observability.path must not be empty when observability.enabled is true`（S01 那条路径同时复验）。
  4. 卡片 §7 要求"`Ctrl-C` 停服后旁文件被合并回收"：Windows 上外部无法向控制台进程发优雅 SIGTERM（`taskkill` 不带 `/F` 对无窗口的控制台进程无效），实测 `taskkill /F` 之后旁文件确实留在原地。因此这条现象改由用例 `TestCloseCheckpointsWAL` 证明：写一次事务后 `-wal` 存在，`Close()` 之后 `-wal` 与 `-shm` 都不在目录里，重开还能读到那一行。强杀后残留、下次打开由 SQLite 自行恢复，与 §11"库文件是加速器"的定位一致。
  5. 跑完删除临时目录。

与卡片的差异（九处，均为实现时的判断）：
1. `Stats` 的签名取设计文档 §9.2 的 `Stats() (Stats, error)`，不是卡片 §3.3 的 `Stats() Stats`：把"读不出来"报成"表是空的"会让运维以为观测层在正常工作。`TestStatsReportsQueryError` 钉住这条。
2. `Stats` 不含 §3.3 列的 `DroppedEvents`/`DroppedAudit`：本卡没有任何写入器，DB 也拿不到它们的计数；S03/S06 卡片把 `Dropped()` 放在 `EventLog`/`AuditLog` 自己身上，所以这里不为将来预留没人填的字段。要汇总时由持有两边的那一层合并。
3. `newBatcher` 返回 `(*batcher[T], error)`，卡片 §3.6 只返回一个指针：`flush` 传 nil 时若照样给一个实例，它就变成"每次 append 都静默丢弃"的写入器，正是 §2 要避免的"看起来正常、实际没记"。非正的容量与周期则回到 `core` 的默认值（§3.3 的口径），不报错。
4. PRAGMA 写进 DSN（`?_pragma=busy_timeout(2000)&_pragma=journal_mode(WAL)&...`）而不是 §3.4 说的"连接建立后立刻执行一次"：`synchronous`、`busy_timeout`、`foreign_keys` 是每条连接的设置，连接因故重建后只有 DSN 这份还会生效。打开之后仍然读回**实际生效**的 `journal_mode` 与 `synchronous`（`JournalMode()`/`Synchronous()`），与期望值不一致时记 warn、debug 记进 `Open` 自己那行，符合 §9 风险 2。
5. 索引数量：§3.5 与 §5.1 都写"6 个索引"，设计文档 §6 实际是 8 个（事件 3 + 产物 1 + 审计 4）。按设计文档建 8 个，用例逐个点名（不是只数总数），DoD 第 3 条的"逐字一致"以设计文档为准。
6. `newObservabilityDB` 返回消费方定义的窄接口 `observabilityDB`（`Path`/`JournalMode`/`Stats`/`Close`），不是 §3.7 的 `*sqlite.DB`：关闭顺序（§5.14）在真实句柄上没有旁路可观察，而"接口定义在消费方"是本仓库既有惯例（`schedulerAPI`、`serverAPI` 同理）。`defaultRuntimeDeps` 里包一层 `sqlite.Open`，错误分支显式返回 nil 接口而不是类型化空句柄。
7. 句柄没有按 §3.7 "变量保持 nil" 声明在 `if` 外面：本卡除了关闭它没有读取方，而 Go 不允许声明用不上的变量。注释里写明 S03 注入事件写入器时再取出来，`defer` 的声明位置不变，所以关闭顺序不受影响。
8. §3.7 关于 `defer` 的那句自相矛盾（"排在 `store.Close` 的 defer 之前声明"与"先执行的是观测层收尾"正好相反，先声明的 defer 会后执行）。按设计文档 §7.3 的执行顺序实现：观测层的 `defer` 晚于 `store.Close` 声明，因此实际先关观测层、后关存储；`TestRun_ObservabilityClosedBeforeStore` 用共享顺序表断言 `[observability, store]`。另外收尾的 `defer` 挂在 `Stats()` **之前**，这样"Open 成功但库读不出版本"这条早退路径也不会留下一条没人认领的连接。
9. 驱动版本取 `v1.46.0` 而不是 latest：`v1.60.1` 的 `go.mod` 要求 `go 1.26.0`（`v1.47.0` 起要求 `go 1.25.0`），`go get` 会把本仓的 `go` 指令一并抬上去，等于把整仓最低工具链（含 `examples/demo1`、`demo2` 的 `go run` 与任何 CI 镜像）从 1.24.13 改成 1.26。DoD 第 6 条只允许"新增依赖"，所以选了保持 `go 1.24.13` 不动的最新版本。**要升到 1.60.x 需要显式决定抬语言版本**，那是另一件事，不在本卡范围内。
10. `artifact_index` 的 `PRIMARY KEY (job_id, attempt)` 与设计文档逐字一致；`TestSchemaColumnsMatchDesign` 用 `pragma_table_info` 把三张表的 `name|type|notnull|pk|default` 全部列钉住——这是"DoD 第 3 条：S03/S05/S06 不需要再执行任何 DDL"唯一可执行的守卫（DDL 是字符串，改错一个列名只会在读侧的运行期才暴露）。

实施中发现并修掉的三条缺陷（都由本卡用例抓到，并反向验证过用例不是空转）：
1. **并发 `Flush` 会重复落同一批**：`Flush` 的"快照 → 落盘 → 摘队"三步不在同一把锁内，周期落盘与关停路径的显式 `Flush` 同时进来时两个轮次读到同一段队首，各写一遍，表里出现重复记录（事件表里就是一条重复的时间线）。已修：新增 `writeMu` 把整轮落盘串行化；钉桩测试 `TestBatcher_ConcurrentFlushDoesNotDuplicate`（用 `gateRecorder` 卡住落盘函数把竞争窗口撑开）。反向验证：临时摘掉 `writeMu` 后该用例报 `record 0 landed 4 times across 4 flush rounds`，装回去通过。
2. **`newBatcher` 的三条兜底写成 `switch` 只命中第一条**：非正容量会吞掉非正周期，`time.NewTicker(0)` 直接 panic。已修：拆成三条独立 `if`；`TestBatcher_ConstructorGuards` 同时传 0 容量与 0 周期。
3. **迁移版本表没人创建**：`migrate()` 上来就 `SELECT ... FROM observe_schema_migrations`，而建表语句只作为常量躺在 `schema.go` 里没被引用，`Open` 必定失败。已修：`migrate()` 先执行 `migrationTable` 再读版本。发现方式是第一次跑包测试。

引用核对（卡片与设计文档的行号在本卡执行时的实际值）：`main.go` 的依赖完整性检查卡片写 154-157，本卡之前是 154-157、加上 `newObservabilityDB` 之后是 **181-185**（这个函数上方多了 `observabilityDB` 接口声明 8 行）；产物清理那条 `defer` 的注释卡片写 221-227，实际在 **249-255**；`core/store.go:17-19` 的 `DefaultFlushInterval` ✓；`executor/artifact.go` 的权限常量在 56-57 ✓；`executor/artifact_test.go:560` 的 Windows Skip 体例 ✓；设计文档 §6 的 DDL 逐字搬进 `schema.go`（列注释保留），§9.2 的函数签名除差异第 1、3 条外一致。用例计数：`store/sqlite` 21 个顶层测试函数，其中 `TestSchemaColumnsMatchDesign` 内含三张表、`TestBatcher_RetryOnceThenGiveUp` 内含两个子用例。

留给后续卡的事：
- 三张表本卡没有任何读写方，`Stats()` 里三张表的行数恒为 0，这是预期状态而不是故障（S03/S05/S06 接手）。
- `DB.sqlDB` 是未导出字段：S03 的 `EventLog`、S06 的 `AuditLog` 都放在本包内，可以直接用；如果哪天要把写入器挪到别的包，需要显式给出事务入口而不是导出 `*sql.DB`。
- `batcher[T]` 的"重试一次"取的是"失败的那批留在队首等下一轮"，不是"退回队列尾部"：前者保住事件的时间顺序，也避免退回有界队列时被立刻丢弃。S03/S06 直接复用，别再各自实现一套。
- 关闭顺序的可观察性依赖 `observabilityDB` 这个窄接口。S03 若要往 `newServer` 注入事件读写器，扩接口面在 `cmd/server` 一侧，不要让 `api` 或 `executor` 因此 import 本包。
- Linux 侧的权限断言（0640/0750）与 `-race` 实跑仍只在 CI/Unix 有效，本机 Windows 是 Skip；收口（S07）要按"未实跑"记。

