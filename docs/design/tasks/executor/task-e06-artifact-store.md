# TASK-E06　输出文件存储与清理

- 所属阶段：M1 结果通道
- 依赖任务：TASK-E01、E05
- 涉及文件：新增 `executor/artifact.go`、`executor/artifact_test.go`
- 预计规模：中

## 1. 任务目标

提供一块独立的文件存储，用来放每次执行的完整 stdout / stderr，并按保留天数和"任务是否还存在"两个条件清理。写入必须是边产生边写、超过上限就停止，不能把整段输出先在内存里攒着。

## 2. 背景与当前问题

设计文档 §2 的 D4 说明了原因：`core/store.go` 的 `flushLocked` 每次落盘都重写整个 `jobs.json`，完整输出不能进快照。同时 `api/history.go` 的事件缓冲是纯内存（每任务 100 条、全局 500 条），重启即清空，也不能当输出存储。

另外两个已经存在的问题必须在本任务处理：

- `core/scheduler.go` 的 `Cancel` 会删除存储记录，`core/store.go` 的 `trimTerminalLocked` 会按 `store.history_limit` / `history_ttl` 淘汰终态快照。这两处都不认识产物文件，如果不做清理，`data/exec/` 只会一直增长。
- 重试副本沿用同一个任务 ID（`core/job.go` 的 `CloneForRetry`），所以每次尝试必须按 attempt 分文件，否则第二次尝试会覆盖第一次的输出。

## 3. 要实现的功能

1. 目录布局：`<executors.output.dir>/<job_id>/a<attempt>.out`、`a<attempt>.err`、`a<attempt>.meta.json`。
   `job_id` 是 UUIDv7，只含 `[0-9a-f-]`，但**仍然要做一次校验**：拒绝含路径分隔符或 `..` 的 ID，防止把路径拼接当可信输入。
2. `type ArtifactStore struct{ ... }` 与构造函数：

   ```go
   func NewArtifactStore(opts ArtifactOptions, logger *slog.Logger) (*ArtifactStore, error)
   type ArtifactOptions struct {
       Dir       string
       MaxBytes  int           // 单条流上限，超限停止写入
       TTL       time.Duration // 0 表示不按时间清理
       Now       func() time.Time // 测试注入
   }
   ```

   构造时创建目录（权限 `0750`），失败返回错误。
3. 写入句柄：

   ```go
   func (a *ArtifactStore) Open(jobID string, attempt int) (*ArtifactWriter, error)
   type ArtifactWriter struct{ ... }
   func (w *ArtifactWriter) Stdout() io.Writer     // 带 maxBytes 限制的写入器
   func (w *ArtifactWriter) Stderr() io.Writer
   func (w *ArtifactWriter) Close() (ArtifactInfo, error)  // 返回落盘字节数、是否截断、文件路径
   func (w *ArtifactWriter) WriteMeta(meta any) error      // meta.json，原子写（tmp + rename）
   ```

   限制写入器的实现要求：内部计数，超限后丢弃后续字节但保持 `io.Writer` 语义（返回 `len(b), nil`，否则 `exec.Cmd` 的 `Stdout` 管道复制会因为 `ErrShortWrite` 报错），并置 `truncated`。**这条是必须写进注释的关键细节。**
4. 读取：

   ```go
   func (a *ArtifactStore) Read(jobID string, attempt int, stream string, maxBytes int64) (data []byte, truncated bool, err error)
   func (a *ArtifactStore) Exists(jobID string, attempt int) bool
   func (a *ArtifactStore) Tail(jobID string, attempt int, stream string, n int64) (data []byte, truncated bool, err error)  // 从文件末尾反向读 n 字节
   ```

   `stream` 只接受 `out|err` 两个字面值，其它返回错误（不要把它变成任意文件名读取）。
   `Tail` 用于结果面板默认只展示尾部；`Read` 支持 `maxBytes` 上限，超限从**头部**截断并置 `truncated`。
5. 清理：
   - `func (a *ArtifactStore) PurgeExpired() (int, error)`：按目录 mtime 与 `TTL` 删除过期任务目录。
   - `func (a *ArtifactStore) PurgeOrphans(live func() (map[string]bool, error)) (int, error)`：`live` 返回当前存储里存在的任务 ID 集合；ID 不在集合里的目录删除。`live` 返回错误时本轮跳过（不能因为存储读一下失败就把产物全删）。
   - 后台清理入口 `func (a *ArtifactStore) Start(ctx context.Context, live func() (map[string]bool, error))`：启动时先跑一次 `PurgeOrphans` + `PurgeExpired`，之后每 24 小时只跑 `PurgeExpired`；`ctx` 结束即退出。goroutine 内的 panic 要恢复并记日志（清理任务不应导致进程异常退出）。
   - 同步删除入口 `Remove(jobID string) error`：本卡只提供方法，不挂到 `Cancel` 上。理由是 `core.Store` 不该知道文件系统产物（设计文档 §6.4），取消后的产物由"启动时的孤儿清理"回收。
6. 大小与数量下限保护：`ArtifactOptions.MaxBytes < 1024` 时归一化为 1024 并记 warn（配成 0 会让所有输出丢失，比配小更糟）。

## 4. 实现步骤

1. 先做目录布局与路径校验（一个私有 `pathFor(jobID, attempt, stream)`，所有读写都走它）。
2. 做限制写入器 + `Open/Close`，配单测。
3. 做 `Read/Tail/Exists`。
4. 做 `PurgeExpired/PurgeOrphans/Start`。
5. 最后把 `NewArtifactStore` 接进 `cmd/server/main.go` 的 `runtimeDeps`（新增闭包 `newArtifactStore`），关闭时 `Stop`/取消 ctx；此时还没有读它的端点，属正常。

## 5. 测试要求

全部用 `t.TempDir()`，时间用注入的 `Now`，不要 `sleep` 等 TTL。

1. `TestArtifactWriter_RespectsMaxBytes`：写 3×`MaxBytes`，断言文件实际大小 == `MaxBytes`、`ArtifactInfo.Truncated == true`、每次 `Write` 返回的 `n == len(p)`。
2. `TestArtifactWriter_PerAttemptFiles`：同一 jobID attempt 1/2/3 各写一份，断言三个文件独立存在且内容互不覆盖（这条守住 `CloneForRetry` 复用任务 ID 可能导致的覆盖问题）。
3. `TestPathFor_RejectsBadJobID`：`jobID` 取 `../escape`、`a/b`、绝对路径 → `Open` 返回错误且不创建文件。
4. `TestRead_And_Tail`：写 10KB，`Read(maxBytes=1KB)` 从头部截断且 `truncated=true`；`Tail(1KB)` 得到末尾内容且 `truncated=true`；`Tail` 大于文件长度时返回全文且 `truncated=false`。
5. `TestRead_UnknownStream`：`stream="meta"` 或非 `out|err` → 错误。
6. `TestPurgeExpired`：注入 `Now` 造两个目录（一个在 TTL 内、一个过期），断言只删过期那个，且返回删除数量。
7. `TestPurgeOrphans_LiveFuncError`：`live` 返回错误 → 不删除任何东西、不返回 panic。
8. `TestPurgeOrphans_RemovesMissingJobs`：造三个目录，`live` 只认其中一个 → 另两个被删。
9. `TestWriteMeta_Atomic`：`WriteMeta` 后目录里不残留 `.tmp` 文件；再次 `WriteMeta` 覆盖成功。
10. `TestStart_StopsWithContext`：`ctx` 取消后 goroutine 退出（用有缓冲的完成通道断言，不用 `sleep`）。
11. `TestMaxBytesNormalization`：`MaxBytes=0` → 实际生效 1024 且记了一条 warn。

## 6. 完成标准（DoD）

- [ ] 输出是流式落盘，内存里不会同时持有整段 stdout（`Exec` 里只有 2KB 预览，见 E05）。
- [ ] 超限后停止写入但仍满足 `io.Writer` 契约，注释说明了为什么必须返回 `len(b), nil`。
- [ ] 三个清理入口都有测试，且"存储读失败时不删产物"这条被单独覆盖。
- [ ] 文件权限不放开给其它用户（`0750` 目录、`0640` 文件），并在测试里用 `os.Stat` 断言权限位（Windows 上跳过并写明原因）。
- [ ] `data/exec/` 与 `store.path` 完全分离：改动本卡不影响 `jobs.json` 的任何行为。
- [ ] `cmd/server/main.go` 的装配带关闭路径（`run` 返回前停掉清理协程），优雅关闭测试不受影响。

## 7. 验收方式

```bash
go build ./... && go vet ./... && go test ./... -race
go test ./executor -run 'Artifact|Purge|Tail' -v
```

手工：临时在本机配置把 `executors.output.dir` 指到 `/tmp/gdq-exec`（Windows 用 `%TEMP%` 下目录），跑一次带清理的单元测试覆盖不到的真实场景：手造一个 8 天前的目录（`touch -d`/`(Get-Item).LastWriteTime`），启动服务，确认启动那一次扫描就把它删掉。

## 8. 不在本任务范围

- 不做执行进程（E09）。
- 不加 HTTP 端点（E07）。
- 不改 `core/store.go`（产物与快照的关联通过"孤儿清理"这一条松耦合完成，不引入反向依赖）。
- 不做压缩存储或输出转义（如后续需要，另立卡片）。

## 9. 风险与回滚

- 风险：`PurgeOrphans` 依赖 `store.LoadAll()`。`store.history_limit: -1`（不留痕）的部署里终态快照会被立刻删除，于是任务一跑完产物就成"孤儿"。必须在 `Start` 的第一次扫描前把这条写进注释和日志，并推荐做法：`executors.output.ttl` 与 `history_ttl` 一起配置。测试要覆盖 `history_limit:-1` 场景（`live` 集合里没有终态任务，产物被删是预期行为，因此更稳妥的默认是"孤儿清理只在启动时跑一次，周期清理只按 TTL"）。**采用这个更保守的方案**：`Start` 里周期任务只跑 `PurgeExpired`，`PurgeOrphans` 只在启动时跑一次。
- 风险：Windows 上没有权限位语义，断言要跳过，避免制造只在全平台通过的测试。
- 回滚：新增文件为主，`main.go` 的装配是一个独立提交点，可单独 revert。

## 10. 实现记录（2026-09-30）

改动文件：新增 `executor/artifact.go`、`executor/artifact_test.go`；
改 `cmd/server/main.go`（`runtimeDeps.newArtifactStore`、`run` 的创建与关闭、`liveJobIDs`）与
`cmd/server/main_test.go`、`cmd/server/main_integration_test.go`。`core/` 与 `api/` 一行未改（DoD 第 5 条）。

### 与卡片的偏离与补充

1. **`Start` 返回 `<-chan struct{}`，并且第一轮扫描是同步的**。卡片只写 `Start(ctx, live)`。
   返回通道是给关闭路径的 join 点：`run` 里 `cancel(); <-done` 才能保证清理协程先退、存储后关
   （否则协程可能在存储已关闭后再读一次任务集合）。同步跑第一轮有两个好处：
   调用方在开始服务之前就把产物清干净，测试也不用和协程调度抢顺序就能断言删除结果
   （实现过程中先写成"协程内跑第一轮"，`TestStart_StopsWithContext` 立刻出现断言比删除早的竞态，
   于是改成同步——这个竞态是真实存在的，不是为了让测试好写而做的取舍）。
2. **任务 ID 的校验比卡片更严**：只允许字母、数字与 `-`，长度 ≤128。
   因此 `..`、分隔符、绝对路径、空白、非 ASCII 以及**点号**全部被拒（UUIDv7 里没有点号）。
   写（`Open`）、读（`Read`/`Tail`/`Exists`）、删（`Remove`）四条入口共用同一个校验函数，
   `TestOpen_RejectsBadJobID` 断言被拒的输入"整棵树没有任何变化"。
3. **`attempt` 允许 0，拒绝负数**。卡片没写取值下界。把 0 判错并没有增加安全性（文件名 `a0.out`
   不会与任何真实尝试冲突，真实尝试从 1 起：`core` 在调用处理函数之前先自增 `Attempts`），
   却会让"按自己的习惯计数"的自行接入程序读不到自己的产物。负数会拼出 `a-1.out` 这种带减号的名字，仍然拒绝。
4. **新增卡片未列的出口**：`Dir()`、`MaxBytes()`、`TTL()` 三个访问器，以及哨兵错误 `ErrArtifactMissing`。
   访问器给 E07 的响应与启动日志用；`ErrArtifactMissing` 让接口能把"产物不存在（404）"
   与"读盘出错（500）"分开，而不是比 `os.ErrNotExist` 的原始文本。
5. **`Read` 的 `maxBytes<=0` 表示不限制，`Tail` 的 `n<=0` 返回空内容并报告截断**。卡片只写了正数用法，
   两个零值语义在这里固定下来并各有用例，避免 E07 再猜一次。
6. **名字不像任务 ID 的条目不参与清理**（记一条 warn）：`jobDirs` 只认校验得过的目录名。
   有人把别的目录放进产物根目录时，清理不会替别人做决定。用例 `TestJobDirs_IgnoresUnusableNames`。
7. **产物存储只在 `executors.enabled=true` 时创建**（卡片 §4.5 没提这个开关）。
   否则关着执行器的部署每次启动都会 `mkdir ./data/exec`，把"这次没启用"表现成"在写文件"。
   `newArtifactStore` 仍然列入依赖完整性检查：配置打开却没有装配闭包会直接报"依赖不完整"。
8. **闭包签名收 `core.Config`**（与 `newExecutorRegistry` 对称），`core.ExecutorOutputConfig` 到
   `ArtifactOptions` 的映射放在 `defaultRuntimeDeps` 里，和 `cfg.Store` → `StoreOptions` 的既有写法一致。
9. **`Open` 的失败路径**：建 stdout 成功、建 stderr 失败时会先关掉已经打开的句柄再返回错误，
   不留"开着句柄的半套产物"。
10. **卡片 §9 的保守方案已采用**：`PurgeOrphans` 只由 `sync.Once` 在启动那一轮执行，
    周期任务（每 24 小时）只跑 `PurgeExpired`；`live` 返回错误时整轮跳过并记 warn，不返回错误、不删文件。

### 验证结果

- 单元测试：`executor/artifact_test.go` 新增 21 个用例（含 §5 要求的 11 条，另补边界写入、`io.Copy` 契约、
  未知 stream、缺失文件的哨兵错误、TTL=0 不删、双份 Close 幂等、Remove 空操作等 10 条）；
  `cmd/server` 新增 3 条（产物存储构造失败不启动且存储已关、清理协程随 run 退出、`liveJobIDs` 包装），
  并把 `TestRun_WithIncompleteDependencies` 扩成"缺登记表"和"缺产物存储"两种缺失。
- 截断契约有专门用例：`TestArtifactWriter_RespectsMaxBytes` 写 3 倍上限，断言文件大小正好等于上限、
  `Truncated=true`、且**每一次** `Write` 都返回 `len(p)`；`TestArtifactWriter_SmallWritesPastLimit`
  覆盖"边界落在一次写入中间"以及"到顶之后继续写仍满额返回"。返回少字节会让 `io.Copy`
  报 `ErrShortWrite`、`os/exec` 因此中断子进程，这与"只截断输出、不打断执行"相反，注释里写明了。
- `go build ./...`、`go vet ./...`、`go test ./... -race -count=1` 全绿；
  linux、darwin 交叉编译与 `-tags dashboard` 构建通过；改动文件按 LF 副本 `gofmt -l` 无输出。
- `TestArtifactStore_Permissions` 在 Windows 上跳过（NTFS 权限不由 mode bits 表达，`os.Chmod` 只影响只读位），
  Unix 分支断言目录 `0750`、两个流文件与 meta 文件都是 `0640`。
  本机 WSL 有 Linux 但没有 Go，这一条在本机没跑过真实 Unix 分支，需要在 Linux 或 CI 上补一次（已登记）。
- 真实进程冒烟（二进制、配置、`jobs.json`、产物根目录全在系统临时目录，跑完已删除；
  仓库 `configs/config.yaml` 与 `data/` 未被写入）：产物根目录预置五种条目——
  存活任务且新鲜的目录、存活任务但目录时间 8 天前的目录、不在 `jobs.json` 里的"幽灵"目录、
  名字带点号的别人的目录（时间也 8 天前）、根目录下的散文件；配置 `ttl: 168h`。
  启动日志依次是
  `WARN artifact entry is not a job id, leaving it alone name=someone.elses.data`、
  `INFO artifact orphan directories purged count=1`、同一句 warn、
  `INFO artifact expired directories purged count=1 ttl=168h0m0s`。
  结果：幽灵目录（孤儿）与 8 天前的存活目录（过期）被删；存活且新鲜的目录连同 `a1.out`/`a1.meta.json` 保留；
  `someone.elses.data/keep.txt` 与 `loose.txt` 未被触碰。
  `POST /api/v1/jobs` 触发一次整文件重写后 `jobs.json` 三条记录照常、`GET /jobs` 返回 200 且字段完整
  （DoD 第 5 条：本卡不影响 jobs.json 的任何行为）。
  全程只有一条 ERROR——E04 的"开了执行器却没配鉴权"横幅，属预期。
  优雅关闭时"取消 ctx 并等待清理协程退出"这条路径由单测覆盖：冒烟用 `taskkill /F` 结束进程，走不到该分支。

- **`io.Copy` 契约单独验过**：`TestArtifactWriter_IOCopyContract` 把限制写入器交给 `io.Copy`，
  源是 4 倍上限的流，断言 `io.Copy` 返回"完整源长度 + nil 错误"、文件正好等于上限——
  这正是 E09 挂 `exec.Cmd.Stdout` 的前提。至于"边跑进程边落盘"要等 E09 端到端确认，此处不声称已验证。

### 留给后续卡片的接口形状

- E07：`Read(jobID, attempt, "out", maxBytes)` / `Tail(...)` 取内容，`Exists` 判有无，
  `ErrArtifactMissing` 对应 404；`Artifact.meta.json` 目前没人读，E07 若要展示执行侧结论再约定内容结构。
- E09：`Open(job.ID, job.Attempts)` → 把 `Stdout()/Stderr()` 直接挂给 `exec.Cmd` →
  `Close()` 拿 `ArtifactInfo` 填 `core.ExecMeta` 的 `OutBytes/ErrBytes/Truncated`，`WriteMeta` 记执行侧结论。
- E14：崩溃恢复把 running 的执行器任务置为 paused 时，产物目录已经存在（`Open` 在尝试开始时建），
  `Meta.Artifact` 应写 `available`，`Exists` 用来判断该不该给"查看输出"的入口。
- 配置侧：`executors.output.ttl` 要与 `store.history_ttl` 配对设置——
  留痕先被清掉的话，下次启动的孤儿清理就会把对应产物一起删（这在 §9 与 PurgeOrphans 注释里都写着）。
