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

- [ ] 三个开关任一关闭时不注入索引，`executor` 与 `api` 的产物路径行为与本卡之前逐字一致（§5 第 6 条守住 executor 侧）。
- [ ] 索引写失败绝不改变执行结果，也不让任务失败；这条有专门用例（§5 第 2 条）。
- [ ] 目录删除与索引行删除的先后顺序固定为"先删目录、再删行"，删除目录失败时行保留。
- [ ] `GET /jobs/:id/artifacts` 的四种情形都覆盖：未装配 503、无记录 200 空列表、正常 200、越权 401。
- [ ] 索引里的 `out_bytes` 与产物文件实际大小一致（用真实执行 + 一次 `os.Stat` 对照，不能只信单元测试里的假数据）。
- [ ] `ArtifactInfo` 结构未新增字段（`Kind`/`Profile` 走 `IndexRecord`，不污染写进 `meta.json` 的那份）。
- [ ] `docs/api.md` 新增该端点；`docs/design/executor-design.md` §6.4 的"产物与快照分工"处补一句索引表的存在。

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
