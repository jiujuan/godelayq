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
