# TASK-S05　`artifact_index` 表、输出索引写入与列表端点

- 所属阶段：M2 产物
- 依赖任务：TASK-S01、S02；执行器侧 TASK-E06（产物存储）、E07（结果端点）已完成
- 涉及文件：新增 `executor/artifact_index.go`、`executor/artifact_index_test.go`、`store/sqlite/artifacts.go`、`api/handlers_artifacts.go`；改 `executor/artifact.go`、`executor/proc.go`、`executor/http.go`、`api/server.go`、`api/handlers_executors.go`、`cmd/server/main.go`
- 预计规模：中

## 1. 任务目标

把每次执行的输出文件属性（两侧字节数、是否截断、状态）登记进 `artifact_index` 表，让"产物在不在、有多大、第几次尝试"能查出来；并新增 `GET /api/v1/jobs/:id/artifacts` 把它透出。输出正文继续留在文件里。

## 2. 背景与当前问题

产物侧现在有三个只能靠"扫目录"或"试一次读"解决的判断：

1. **判活**：`PurgeOrphans` 靠 `live()` 回调判断任务是否还存在，而回调是 `cmd/server/main.go:267` 的 `liveJobIDs` → `store.LoadAll()` 全量拉取快照。`executor/artifact.go:558-560` 的注释记录了这个耦合造成的既有问题：`store.history_limit: -1` 的部署里，刚跑完的任务在下次启动会被判成孤儿、产物被删；因此该方法的处置口径是"只在启动跑一次，周期清理只按 TTL"。
2. **判有无**：`GET /jobs/:id/result` 只能在读文件失败之后把摘要标成 `purged`（`api/handlers_executors.go:183` 的 `markArtifactPurged`）。也就是说"产物是否已被清理"这件事是通过一次失败的读取得知的。
3. **列出各次尝试**：结果端点的 attempt 取值靠 `snapshot.Attempts` 猜（`api/handlers_executors.go:99` 的 `parseResultAttempt`）。而按 attempt 分文件的整条理由就是"第一次为什么失败往往正是排障要看的"（`executor/artifact.go:163-166`），偏偏现在没有办法一次看到全部尝试。

设计文档 D8 把这张表的定位写死为**加速器而不是账本**：库写失败时文件仍然是权威，最终判定仍以文件存在与否为准。这条定位决定了本卡所有失败处理都是"记日志、不改动执行结果"。

## 3. 要实现的功能

1. `executor/artifact_index.go`——消费方接口与记录结构定义在 `executor` 侧：

   ```go
   // IndexRecord 是一次执行的输出登记项。Kind 与 Profile 由执行器给出（ArtifactStore 不认识档位），
   // 字节数与截断由 ArtifactInfo 给出。
   type IndexRecord struct {
       JobID   string
       Attempt int
       Kind    string      // script|binary|http
       Profile string      // 档位名，不含 exec. 前缀
       Info    ArtifactInfo
   }
   // ArtifactIndexer 是产物索引的读写能力，由 store/sqlite.ArtifactIndex 实现。
   // 它是可选依赖：nil 表示不维护索引，行为与本卡之前完全一致。
   type ArtifactIndexer interface {
       Record(rec IndexRecord) error
       MarkPurged(jobID string, attempt int) error
       MarkAllPurged(jobID string) error
       DeleteByJob(jobID string) error
       List(jobID string) ([]IndexRecord, error)
       Exists(jobID string, attempt int) (bool, error)
   }
   ```

   `ArtifactStore` 加 `SetIndex(ArtifactIndexer)` 与只读访问器 `Index() ArtifactIndexer`（nil 安全，未设置时返回 nil）。

   **绝对路径不入库**：`ArtifactInfo.OutPath/ErrPath` 是绝对路径（它们要交给 `os` 打开），`store/sqlite` 的 `Record` 在写库前用产物根目录折成相对写法（列名 `out_rel`/`err_rel`），与 `Profile.ProgramDisplay()`"不把服务器目录结构透给前端"同一取向。折径发生在库实现一侧而不是 `executor` 侧，这样 `executor` 不需要知道列名，接口也不带路径风格参数。
2. 写入点（两处，都是已有位置加一行）：
   - `executor/proc.go`：紧跟现有的 `result.Meta.Artifact = core.ArtifactAvailable`（`proc.go:171`）之后，用 `r.profile.Kind`、`r.profile.Name` 与 `writer.Close()` 已拿到的 `info`（`proc.go:167`）调 `Record`。
   - `executor/http.go`：同一位置（`http.go:184`、`:190`）。
   - **失败处理**：索引写失败只记 warn，不改执行结果、不让任务失败。理由就是 D8：文件已经落盘，索引缺一行只意味着"列表端点看不见它"，而 `GET /jobs/:id/result` 照常能读到正文。
3. 删除点（两处，跟随实际删除成功）：
   - `PurgeExpired`：每次 `os.RemoveAll(dir)` 成功之后调 `DeleteByJob`。
   - `PurgeOrphans`：同上。
   - 删除顺序必须是**先删目录、再删行**：反过来的话中途失败会留下"索引说没有、文件还在"的状态，而"文件还在却看不见"比"行残留"更难解释（残留行会被 §3.4 的 purged 标注纠正）。
   - `DeleteByJob` 失败记 warn，不影响本轮删除计数。
4. purged 标注：`api/handlers_executors.go` 的 `markArtifactPurged`（`:183`）在写快照之后补一次索引标注。它当前持有的是快照与一个 attempt，所以按 `MarkPurged(jobID, attempt)` 精确标注这一条，**不用 `MarkAllPurged`**：同一任务的其他尝试的产物可能还在，全标 purged 是假话。
   `MarkAllPurged` 仍然保留在接口里，供启动期的对账使用（§3.6）。
5. 新增读端点 `GET /api/v1/jobs/:id/artifacts`：

   ```json
   { "job_id": "...", "count": 2,
     "items": [ { "attempt": 1, "kind": "script", "profile": "hello",
                  "out_bytes": 42, "err_bytes": 512, "truncated": false,
                  "state": "available", "created_at": "2026-09-30T12:00:00Z" } ] }
   ```

   - 档位：`reader`（`api/server.go:202` 的既有 `reader` 组）。它是元信息，不含输出正文；正文端点另有更严的档位判定（含 secret 参数的档位会把读取门槛升到提交档，`api/handlers_executors.go:72-79`）。
   - 未注入索引 → **503**，照 `requireArtifacts()` 的既有做法（`api/handlers_executors.go:449`）：区分"没装配"与"没有记录"。守卫函数命名 `requireIndex()`，与 `requireArtifacts()` 并列放在同一文件里。
   - 任务没有任何索引行 → 200 + `"items": []`。
   - 排序按 `attempt` 升序（重试链的自然阅读顺序）。
   - 新文件 `api/handlers_artifacts.go` 放 handler 与响应体，路由注册在 `setupRoutes` 的 `jobs` 组里（`jobs.GET("/:id/artifacts", reader, s.requireIndex(), s.ListJobArtifacts)`）。
6. 启动期对账（可选实现，本卡按"做"处理）：`ArtifactStore.Start` 的第一轮扫描里，索引存在但目录已不存在的行按 `state='purged'` 标注，用 `MarkAllPurged`/逐 attempt 标注皆可，但只能标"这一行的文件确实不在"。
   理由：S05 之前写入的产物没有索引行，S05 之后清理的产物会留下索引行——只有启动对账能把两侧对齐一次。
   限制：对账**只标注、不删除**（文件被手工放回时行还在，状态被纠正回 available 靠 §3.4 的反向标注；本期不做反向纠正）。
7. 装配（`cmd/server/main.go`）：`enabled && observability.artifacts.enabled && executors.enabled` 三者都成立时才建索引并 `artifacts.SetIndex(...)`；任一不成立时传 nil。
   注释要写清这条三条件判断：关着执行器时根本没有产物可索引，此时建索引等于在库里留一张空表。
8. `core.ExecMeta.Artifact` 的取值语义不变（`available` / `purged` / 空）。索引是它的第二份记录，不替换它。

## 4. 实现步骤

1. 先写 `executor/artifact_index.go` 的接口与 `SetIndex`/`Index`，配一个测试用的假实现（`map` 存行）。
2. 在假索引下把三个写入/标注点接上（proc、http、markArtifactPurged），配用例断言"调用发生 + 失败不改变执行结果"。
3. 写 `store/sqlite/artifacts.go` 的真实现（`Record` 用 `INSERT ... ON CONFLICT(job_id,attempt) DO UPDATE`，因为清理与重复读取会二次触达同一行）。
4. 做 `Purge*` 的删除跟随 + 启动对账。
5. 最后加读端点与 503 守卫，接 `cmd/server`。

## 5. 测试要求

`executor/artifact_index_test.go`（用假索引，不依赖 SQLite）：

1. `TestRunner_RecordsIndex`：script 档位跑一次，断言 `Record` 被调用一次，`Kind`/`Profile`/`OutBytes`/`Truncated` 与 `ExecMeta` 里的值一致（**同源断言**：索引与摘要不能各算一套）。
2. `TestRunner_IndexFailureDoesNotAffectResult`：假索引返回错误 → 任务照常成功、`Exec.Artifact` 仍是 `available`、产物文件存在、只多一条 warn。
3. `TestHTTPRunner_RecordsIndex`：http 档位同上（覆盖第二个写入点，别只测进程路径）。
4. `TestPurgeExpired_DeletesIndexRowsAfterDirectory`：过期目录被删后索引行也消失；删除目录失败时**不删行**。
5. `TestPurgeOrphans_DeletesIndexRows`：同上，孤儿路径。
6. `TestSetIndex_NilIsSafe`：不调 `SetIndex` 时所有产物路径正常，等价于本卡之前。

`store/sqlite/artifacts_test.go`（真库）：

7. `TestArtifactIndex_RecordAndList`：两条不同 attempt 记录，`List` 按 attempt 升序返回全部列。
8. `TestArtifactIndex_Upsert`：同一 `(job_id, attempt)` 记录两次（第二次字节数更大）→ 只有一行、取第二次的值。**这条守住重试与清理二次触达的情况。**
9. `TestArtifactIndex_MarkPurgedScopedToAttempt`：两条 attempt，标一条 → 另一条仍是 `available`（守住 §3.4 的"不用 MarkAllPurged"）。
10. `TestArtifactIndex_AttemptZeroAllowed`：`attempt=0` 能写入与读回（`ArtifactStore` 允许 0，见 TASK-E06 第 10 节第 3 条）。
11. `TestArtifactIndex_BadJobIDRejected`：与 `Open` 同一套 ID 校验，`../escape`、带分隔符、非 ASCII 全部拒绝。

`api/handlers_artifacts_test.go`：

12. `TestListJobArtifacts_NoIndex503`：未注入索引 → 503 且响应体是 `ErrorResponse`。
13. `TestListJobArtifacts_Empty200`：注入了但无记录 → 200 + `"items": []`。
14. `TestListJobArtifacts_RoleViewer`：viewer 可读；未认证 401（沿用 `reader` 组的既有测试体例）。
15. `TestMarkArtifactPurged_UpdatesIndex`：走一次 `GET /jobs/:id/result` 的读取失败路径，断言快照与索引两侧同时变成 purged。

## 6. 完成标准（DoD）

- [x] 三个开关任一关闭时不注入索引，`executor` 与 `api` 的产物路径行为与本卡之前逐字一致（§5 第 6 条守住 executor 侧；装配侧另有 `TestRun_ArtifactIndexOffByEachSwitch` 的三个子例，见 §10.2 第 7 条）。
- [x] 索引写失败绝不改变执行结果，也不让任务失败；这条有专门用例（§5 第 2 条 `TestRunner_IndexFailureDoesNotAffectResult`：任务成功、`Exec.Artifact` 仍是 `available`、文件读得到、只多一条 warn）。
- [x] 目录删除与索引行删除的先后顺序固定为"先删目录、再删行"，删除目录失败时行保留。（`TestPurgeExpired_DeletesIndexRowsAfterDirectory` 由替身回看删行那一刻目录已不在；`TestPurgeExpired_KeepsRowsWhenRemovalFails` 守反面；`TestConcurrent_ExecuteWhilePurging` 在并发下复查同一条顺序，见 §10.2 第 8 条）
- [x] `GET /jobs/:id/artifacts` 的四种情形都覆盖：未装配 503、无记录 200 空列表、正常 200、越权 401。（另加了"没挂产物存储也回 503"、"库读失败回 500"、"ID 不成目录名回 400"三条，见 §10.2 第 5、6 条）
- [x] 索引里的 `out_bytes` 与产物文件实际大小一致（用真实执行 + 一次 `os.Stat` 对照，不能只信单元测试里的假数据）。（`TestRunner_RecordsIndex` 里对 `OutPath` 做一次 `os.Stat`；`TestArtifactIndex_RealWriterBytesMatchDisk` 用真产物存储写文件 + 真库往返三方对照；手工验收第一轮 `a1.out` 34 B = 表里 34，见 §10.4）
- [x] `ArtifactInfo` 结构未新增字段（`Kind`/`Profile` 走 `IndexRecord`，不污染写进 `meta.json` 的那份）。（仍是 `JobID/Attempt/OutPath/ErrPath/OutBytes/ErrBytes/Truncated` 七个；`meta.json` 的实测内容见 §10.4）
- [x] `docs/api.md` 新增该端点；`docs/design/executor-design.md` §6.4 的"产物与快照分工"处补一句索引表的存在。（api.md 是新的小节"列出各次尝试的输出"，另在 `/result` 末尾加了指向它的交叉引用；executor-design.md 是在 §6.4 的要点列表里加一条，位置紧挨"清扫"那条，见 §10.5 的 D-0501/D-0502 两句文档口径）

## 7. 验收方式

```bash
go build ./... && go vet ./... && go test ./... -race
go test ./executor -run 'Index|Purge' -v
go test ./store/sqlite -run ArtifactIndex -v
go test ./api -run 'Artifacts|ArtifactPurged' -v
```

手工（临时目录配置，打开 `executors.enabled` 与 `observability.enabled`，跑完删除）：
用 `exec.hello` 档位提交一条会失败的脚本任务并配 `max_retries: 1`，等它跑完两次尝试；
`GET /jobs/<id>/artifacts` 应返回两行、`attempt` 分别是 1 和 2、`err_bytes` 非零；
用 `os.Stat` 对照 `data/exec/<id>/a1.out` 的实际大小与表里的 `out_bytes` 一致；
删掉 `a1.out` 文件后再 `GET /jobs/<id>/result?attempt=1`，确认响应 `found=false` 且下一次
`GET /jobs/<id>/artifacts` 里第 1 行的 `state` 已是 `purged`、第 2 行仍是 `available`。

## 8. 不在本任务范围

- 不把输出正文写进库（设计文档 §4.2）。
- 不改 `GET /jobs/:id/result` 的响应结构，也不改 `parseResultAttempt` 的默认选取逻辑（有了索引之后可以让它更聪明，那是另一张卡）。
- 不给索引加 `exit_code` / `http_status`：那两个属于执行结论，已经在 `core.ExecMeta` 里，重复存会造出两个真值来源（设计文档 §6.2）。
- 不做按档位/时间聚合的输出统计。
- 不改 `executors.output.ttl` 的语义，也不改 `PurgeOrphans` "只在启动跑一次"的既有处置。
- 不做"文件放回后把 purged 纠正回 available"。

## 9. 风险与回滚

- 风险：**存量产物没有索引行**。S05 之前已经落在 `data/exec/` 里的目录不会出现在列表中，而 `GET /jobs/:id/result` 仍然能读到它们（它直接读文件）。这是加速器定位的直接后果，必须在 §8 之外的用户文档里写一句"索引自本版本起，历史产物仍可读取但不在列表中"，否则会被当成数据丢失。
- 风险：两个 Runner 各有一处写入点，将来加第三种档位（例如 `container`）时容易漏掉。应对：把"新增 `Kind` 必须同时接索引"写进 `executor/artifact_index.go` 的接口注释，并在 §5 第 1、3 两条用例里让两种现有档位各自成为证据。
- 风险：`Record` 与 `DeleteByJob` 的调用时机分别在生产侧与清理协程里，两个不同 goroutine；`store/sqlite` 侧靠 `SetMaxOpenConns(1)` 串行化（S02 §3.4），本卡不需要再加锁，但 `-race` 用例要覆盖"执行中同时跑一轮 TTL 清理"。
- 回滚：`observability.artifacts.enabled: false` 即停止全部索引读写（表与既有行留着不影响任何东西）；接口是可选依赖，回滚代码不影响产物路径本身。

## 10. 实现记录（执行时补写）

### 10.1 落点

| 能力 | 位置 |
| --- | --- |
| `IndexRecord` / `ArtifactIndexer` / `IndexReconciler` / `SetIndex` / `Index` | `executor/artifact_index.go`（接口在 44 行，对账接口在 68 行，访问器 75/78） |
| 写侧三个内部助手 | 同文件：`recordIndex`（90）、`deleteIndexRows`（111）、`reconcileIndex`（124）、`dirExists`（146） |
| 写入点 | `executor/proc.go:174`（用 `r.profile.Kind`/`Name` + `writer.Close()` 那份 `info`）、`executor/http.go:193`（`KindHTTP`） |
| 删除点 | `executor/artifact.go:554`（`PurgeExpired`）、`:595`（`PurgeOrphans`），都在 `os.RemoveAll` 成功之后，ID 取 `filepath.Base(dir)` |
| 启动对账 | `executor/artifact.go:675`，在 `runStartup` 的孤儿清理 + TTL 清理之后 |
| ID 校验的复用 | `executor.CheckArtifactJobID`（`executor/artifact.go:131`），库实现六处读写全走它，不另写规则 |
| 真库实现 | `store/sqlite/artifacts.go`：`NewArtifactIndex`（43）、`insertArtifact` 的 upsert（63）、`Record`（79）、`relative`（113）、`markPurged`（132）、`List`（193）、`ReconcileMissing`（286） |
| 读端点 | `api/handlers_artifacts.go`：`requireIndex`（45）、`ListJobArtifacts`（67）、`markArtifactIndexed`（107）；路由在 `api/server.go:237` 的 `reader` 组 |
| purged 标注 | `api/handlers_executors.go:187` 的 `markArtifactPurged` 两条路径各调一次 `markArtifactIndexed`（早退分支 `:192`、写快照之后 `:208`） |
| 装配 | `cmd/server/main.go`：字段 110、默认闭包 164、依赖完整性检查 253、构造与 `SetIndex` 389/393、启动日志 403 |

### 10.2 偏离了卡片的地方

1. **`IndexRecord` 多了四个只读字段**（`State`/`CreatedAt`/`OutRel`/`ErrRel`，卡片 §3.1 只有五个字段）。
   理由：`List` 要还原本行的状态与时间，而接口定义在 `executor` 侧——不另造一个"读回来的结构"就让实现无处放这些列。
   字段注释写明"写入时被忽略"，`Record` 也确实不读它们（状态恒为 `available`、时间取实现自己的时间源）。
2. **对账拆成可选接口 `IndexReconciler`**（卡片 §3.6 只说"启动期对账"，没规定形状）。
   对账需要"目录在不在"的判断，只有产物存储懂目录布局；把它塞进必需方法会逼每个实现（包括测试替身）都带一个用不上的 `ReconcileMissing`。
   `reconcileIndex` 用类型断言取它，实现没有这个能力时什么都不做也不报错。
3. **绝对路径折成相对写法的落点在库实现里**（卡片 §3.1 已经这么定），但 `relative()` 多做了一件事：
   落在产物根目录**之外**的路径直接报错，而不是折成 `../` 开头。折径失败意味着这条登记项本身就不可信。
4. **purged 标注落在新函数 `markArtifactIndexed`**，由 `markArtifactPurged` 调（卡片 §3.4 说"在写快照之后补一次"）。
   比卡片多标一处：快照**已经**是 `purged` 的早退分支也标一次——快照与索引可能来自不同部署（索引是这次重启才挂上的），
   那一支不标就留下"快照说 purged、索引说 available"。
5. **对不成目录名的任务 ID 回 400**（卡片没提）。真库的 `List` 会拒绝这种 ID（它必须是合法目录名），
   原样透出就是 500——把客户端写错报成服务器坏了。`ListJobArtifacts` 进门先走一次 `executor.CheckArtifactJobID`，
   用 `respondBadParam` 与 `stream`/`attempt` 那几条同形。
6. **端点不查任务是否存在**，所以对一个不存在的 ID 是 200 空列表（卡片 §3.5 只规定"没有任何索引行 → 200 空列表"）。
   查一次任务存储要引入 `store` 读取，与这张表无关，写在 api.md 里免得被读成"这个任务没有产物"。
7. **没有新增 `newServer` 参数**：索引挂在 `ArtifactStore` 上（`SetIndex`），api 侧走既有的 `WithArtifacts` 那条注入通道，
   `runtimeDeps.newServer` 仍是 S04 之后的六个参数。S06 若要注审计写入器，仍不必动这个签名。
8. **`artifacts.Start` 从执行器装配处挪到观测层那段之后**（`cmd/server/main.go`，连带它的收尾 defer 一起挪）。
   卡片 §3.6 要求对账在"Start 的第一轮扫描里"，而第一轮在 `Start` 内同步跑，起早了那份索引还不在。
   关停顺序跟着变：先停清理协程 → 撤事件订阅 → 关观测库 → 关任务存储。
9. **测试用例比卡片 §5 列的 15 条多**：`executor/artifact_index_test.go` 13 条、`store/sqlite/artifacts_test.go` 14 条、
   `api/handlers_artifacts_test.go` 8 条、`cmd/server/main_integration_test.go` 新增 4 条（共 39 条）。
   多出来的都是本卡自己踩到的分支：删行失败只记日志、对账失败只记日志、实现不支持对账、nil 存储的访问器、
   相对路径折径越界、`attempt=0`、`List` 不返回 nil、分隔符折成正斜杠、四个开关组合的装配、并发下的删除顺序。

### 10.3 验证证据

```
go build ./...                → 通过
go vet ./...                  → 通过
go test ./... -race           → 全绿（api 62.3s / cmd/server 5.6s / core cached / executor 21.7s / store/sqlite 3.0s）
go test ./executor -run 'Index|Purge' -v          → 13 条 PASS + 1 条 SKIP（skip 的那条见 §10.6）
go test ./store/sqlite -run ArtifactIndex -v      → 14 条 PASS
go test ./api -run 'Artifacts|ArtifactPurged' -v  → 8 条 PASS
go test ./cmd/server -run TestRun_ArtifactIndex   → 4 条 PASS（其中 OffByEachSwitch 三个子例）
go list -deps ./api | grep -i sqlite              → 空
go list -deps ./executor | grep -i sqlite         → 空
gofmt -l -s（剥 CRLF 的临时副本，13 个文件）        → 无输出
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ./cmd/server   → 通过
CGO_ENABLED=0 GOOS=windows GOARCH=386 go build ./cmd/server   → 通过
go build -tags dashboard ./...                   → 通过
```

依赖边界那条在本卡多了一层含义：`store/sqlite` 现在 import `executor`（接口定义在消费方，实现方去引它），
方向是 `store/sqlite → executor`，所以"executor 不依赖观测库"这条仍然成立，反过来不成立。

### 10.4 手工验收（本卡 §7）

临时目录里独立二进制 + 自己写的 412 B 配置（`configs/config.example.yaml` 的键全部走默认，只写必要项），
路径全在临时目录内，仓库的 `configs/config.yaml` 与 `data/` 未写。端口 8142，静态 token 作 `machine`，
`executors.required_role` 降到 `operator`（否则 machine 提交不了执行任务），档位 `exec.fail3` 是
`kind: script` + `runtime: node` + `retry_on_exit: [3]`，脚本写 34 B stdout、41 B stderr 后 `exit 3`。

**第一轮**（`observability.enabled: true`）提交一条 `max_retries: 1` 的 `exec.fail3`，启动日志即证据：

```
msg="observability enabled" path=./data/observe.sqlite schema_version=1 journal_mode=wal events_writer=true artifact_index=true
```

跑完（`GET /jobs/<id>` 是 `failed`、`retry_count 1`）之后：

```
GET /api/v1/jobs/01a0f3e9-…2012/artifacts
{"job_id":"01a0f3e9-…2012","count":1,"items":[{"attempt":1,"kind":"script","profile":"fail3",
 "out_bytes":34,"err_bytes":41,"truncated":false,"state":"available","created_at":"2026-10-01T04:03:14+08:00"}]}
```

与磁盘三方对照：`a1.out` 实际 34 B、`a1.err` 41 B，`a1.meta.json` 里的 `out_bytes`/`err_bytes` 也是 34/41，
即"索引 = 摘要 = 文件"。响应里没有任何路径列（`out_rel`/`err_rel` 不在体外）。

卡片 §7 说这里"应返回两行、attempt 分别是 1 和 2"——**当前构建不可能**：`CloneForRetry` 不搬 `Attempts`
（`docs/design/executor-design.md` §11 第 7 条、§6.4 那条 ⚠️，`docs/api.md` 也已按现状写明），
两次执行都写 `a1.*`。实测到的因此是 upsert 路径：`GET .../events` 有 7 条事件（第二次 `job.started` 在
`04:04:16`），同一时刻 `created_at` 从 `04:03:14` 变成 `04:04:16`、行仍是一条、`count` 仍是 1。
这是本卡 §3.3 那条 upsert 设计的真实取证，只是卡片把它写成了"两行"（见 §10.5 的 D-0501）。

**第二轮**：手工删掉第一个任务的整个产物目录后重启，索引侧的启动对账立刻给出结论：

```
msg="observability enabled" ... artifact_index=true
msg="artifact index rows marked as purged" count=1 reason="the output directory is gone"
```

`GET /jobs/<被删目录的任务>/artifacts` 的 `state` 已是 `purged`，而另一个任务的行仍 `available`。
这一步**没有读过一次 `/result`**——对账自己把两侧对齐了，正是 §3.6 的用途。

**第三轮**：对第二个任务只删 `a1.out`（留着 `a1.err`），走一次读取失败路径：

```
GET /jobs/<id>/result?attempt=1            → found=false, meta.artifact=purged
GET /jobs/<id>/result?attempt=1&stream=err → found=true,  size_bytes=41（正文照样给）
GET /jobs/<id>/artifacts                   → state=purged
```

一行盖两路输出，所以单独删一路会把整行标成 purged，而另一路仍读得到（见 §10.5 的 D-0502）。

**第四轮**（TTL 与回滚）：`GODELAYQ_EXECUTORS_OUTPUT_TTL=1s` 重启 → 启动扫描把剩下的目录删掉
（`msg="artifact expired directories purged" count=1 ttl=1s`），`GET /jobs/<id>/artifacts` 回
`{"count":0,"items":[]}`——先删目录、再删行，列表变短而不是变成一堆 `purged`。
再 `GODELAYQ_OBSERVABILITY_ARTIFACTS_ENABLED=false` 重启：启动日志 `artifact_index=false`，
新提交的 `exec.fail3` 仍正常写出 `a1.out`/`a1.err`/`a1.meta.json`、`GET /jobs/<id>/result` 回 `found=true`，
而 `GET /jobs/<id>/artifacts` 回 503 原文：

```
{"code":503,"message":"artifact index is not configured","details":"start the server with executors.enabled and observability.artifacts.enabled to enable /api/v1/jobs/:id/artifacts"}
```

顺带取到 400 那条（§10.2 第 5 条）的原文：

```
GET /api/v1/jobs/..escape/artifacts
{"code":400,"message":"invalid id","details":"got \"..escape\": artifact: job id \"..escape\" contains an unusable character '.'"} [400]
```

进程一律 `taskkill //F` 结束（Windows 上外部发不出优雅 SIGTERM，README 第 4 条口径）。

### 10.5 缺陷处置

| 编号 | 严重度 | 事实 | 处置 |
| --- | --- | --- | --- |
| D-0501 | 中 | 本卡 §7 的手工步骤前提不成立：`max_retries: 1` 的任务在列表里**不会**出现 attempt 1 与 2 两行。根因是 `CloneForRetry` 不搬 `Attempts`，重试仍写 `a1.*`（§11 第 7 条已登记、本卡 §8 明确不在范围内） | **已按现状记录**：本节写明实测拿到的是同一行被覆盖（`created_at` 移动、`count` 仍是 1），`docs/api.md` 的新小节与 `docs/design/executor-design.md` §6.4 新增那条各写了一句"重试过的任务在列表里仍是一行"。根因留给 E15 那条，修好之后本节与 api.md 的措辞要一起回改 |
| D-0502 | 低 | 一行覆盖一次尝试的**两路**输出，所以手工删掉 `a1.out` 会让整行变 `purged`，而 `a1.err` 还在、`/result?stream=err` 仍回 `found=true`（第三轮实测） | **已文档化 + 功能登记不修**：`docs/api.md` 的 `state` 段落明写"删掉某一路文件会让整行变成 purged"。要按流分开就得改表主键（`(job_id, attempt, stream)`），那是 S07 之后的话题，本卡 §3.1 的表结构来自 S01 的迁移 |
| D-0503 | 低 | `MarkAllPurged` 在真实现里对账走的是同类型的内部 `markPurged(jobID, nil)`，接口上这个公开方法当前**没有生产调用点**（只有 `TestArtifactIndex_MarkAllPurged` 与 `TestListJobArtifacts_*` 的替身路径覆盖它） | **登记不修**：卡片 §3.1 点名要它，删掉会让接口比承诺的窄，而"标注整个任务"仍是清理之外的合理入口（比如将来手工清库）。留着并在 §3.1 的注释里指明用途 |
| D-0504 | 低 | 列表端点不查任务是否存在：一个不存在的 ID 与一个"有任务但没有产物"的 ID 回答一样（200 空列表），排障时容易读成后者 | **已文档化**（`docs/api.md` 的新小节最后一段）。要区分就得在端点里读任务存储，卡片 §3.5 未含；等真有人把它当"这个任务不存在"的判据再改 |
| D-0505 | 低 | 启动对账只标注、不删除，反向（文件被手工放回 → 该纠正回 `available`）本期不做，所以放回文件后列表会说"已清理" | **卡片即如此**（§3.6 的限制、§8 明确排除），非缺陷；`docs/api.md` 的 `state` 两条来历一段已把"purged 从哪来"写清 |

### 10.6 未覆盖与已知边界

- `TestPurgeExpired_KeepsRowsWhenRemovalFails` 在本机（Windows）**跳过**：`os.RemoveAll` 无论如何都成功，
  造不出"目录删不掉"的现场。"先删目录、再删行"的失败分支因此只在 Linux 上真跑得通，
  本轮没有 Linux 环境（与执行器系列同一限制）。正向顺序由另两条用例与第四轮手工取证。
- 没有覆盖"存量产物 + 新索引混在同一个 `data/exec/` 里"的升级现场（§9 第一条风险）。
  手工环境是新建的临时目录，造不出"升级前就有产物"；api.md 里那句"索引从启用它的那个版本开始记"是文档侧的兜底。
- 索引与 `parseResultAttempt` 的默认选取没有打通（§8 排除），所以 `/result` 仍靠 `snapshot.Attempts` 猜 attempt，
  列表端点已经知道有哪些尝试——两份信息现在并存，合并是另一张卡。
- 并发用例（`TestConcurrent_ExecuteWhilePurging`）覆盖的是"执行侧写 + 清理侧删"两个 goroutine，
  装配里清理只有一个 goroutine，因此没有"两轮清理并发"的用例。`store/sqlite` 侧仍靠 `SetMaxOpenConns(1)` 串行化。
- 前端不消费这个端点（卡片未要求，控制台审计页在 S08），`web/` 与 `vue-tsc` 本卡未动。
- 观测库被外部改坏 / 表被删的情况只测到"读失败回 500"，没有测"写失败之后文件仍是权威"的真库版本
  （executor 侧用的是替身索引）。
