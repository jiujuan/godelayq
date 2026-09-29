# TASK-E07　结果查询端点与事件摘要

- 所属阶段：M1 结果通道
- 依赖任务：TASK-E05、E06
- 涉及文件：`api/server.go`、新增 `api/handlers_executors.go` 与测试、`core/scheduler.go`（事件 `Data` 追加摘要）
- 预计规模：中

## 1. 任务目标

让结果可被读到：一是 `GET /api/v1/jobs/:id/result` 读完整输出文件，二是任务完成/失败事件里带一份小的结论摘要，让实时页和事件时间线不查文件也能显示"退出码 1 / HTTP 500 / 耗时 3.2s"。

## 2. 背景与当前问题

`core/scheduler.go` 的 `executeJob` 现在只在失败时发一条 `Data` 为 `{"error": "..."}` 的事件，成功事件的 `Metadata` 只有 `duration_ms`。`api/history.go` 把这些事件收进内存缓冲（每任务 100 条、全局 500 条），`api/handlers_events.go` 与 WS/SSE 原样广播。

执行结果有两个读取需求，大小差两个数量级：列表和时间线要的是几十字节的结论，详情页要的是几 KB 到几百 KB 的输出。把两者混在一个响应里会让 `/jobs` 和 WS 广播被输出撑大，所以分两条路：摘要进事件、正文走专用端点。

## 3. 要实现的功能

1. `api/server.go` 增加 `WithArtifacts(*executor.ArtifactStore) Option` 与 `Server.artifacts` 字段；新增守卫 `requireArtifacts()`，未注入时端点返回 503，风格照 `api/handlers_groups.go` 的 `requireGroupStore()`。
2. `GET /api/v1/jobs/:id/result`（放在 `api/handlers_executors.go`）：

   | 查询参数 | 默认 | 说明 |
   | --- | --- | --- |
   | `attempt` | 最近一次已结束的尝试 | 从 `JobSnapshot.Attempts` 取值；`0`/省略表示默认，显式传值范围 `1..attempts`，超范围返回 400 |
   | `stream` | `out` | 只接受 `out` / `err` |
   | `from` | `tail` | `tail`（读尾部）或 `head`（读头部） |
   | `max_bytes` | `executors.output.inline_preview` 的 4 倍，硬上限 `executors.output.max_bytes` | 超限截断并在响应里标记 |

   响应：

   ```json
   {
     "job_id": "01...", "attempt": 2, "stream": "out",
     "found": true, "size_bytes": 183742, "returned_bytes": 8192,
     "truncated": true, "meta": { "...": "该次尝试的 ExecMeta" },
     "content": "...\n"
   }
   ```

   - 任务不存在 → 404；任务不是执行器任务（`Exec == nil` 且没有产物文件）→ 404，`message` 为 `no execution result for this job`。
   - 有快照摘要但文件已被清理 → 200，`found:false`、`content:""`，并把该次 `ExecMeta.Artifact` 显示为 `purged`（读到文件缺失时同时把快照里的 `available` 改成 `purged` 并 `store.Update` 一次，避免每次读取都碰到文件缺失）。
   - 响应头：`Cache-Control: no-store`；`Content-Type: application/json`（本任务不做纯文本流式输出，避免引入新的鉴权绕道，见第 8 节）。
   - 档位：`reader`（`core.RoleViewer`）。含 `secret_args` 的档位在 E16 里收严到 admin，本任务先把判档点留在实现注释里标明位置。
3. `core/scheduler.go` 的 `executeJob`：成功与失败事件的 `Data` 追加 `result` 字段，值就是 `job.Exec`（`nil` 时不追加，保持现有事件形状不变）。事件 `Metadata` 不加新键，避免影响 `api/history.go` 的既有断言。
   - 大小控制：写进 `Data` 的 `Preview` 必须已经过 E05 的 `SetPreview(limit)`，`limit` 来自 `executors.output.inline_preview`。为此 `core` 需要知道这个上限：给 `Scheduler` 加 `SetEventPreviewLimit(n int)`（默认取现有 `inline_preview` 默认值 2048），由 `cmd/server/main.go` 在装配时设置。事件发布处对 `Preview` 再截一次，双保险。
4. `api/sse.go` 的 `publishedEventTypes` 不需要改（没有新增事件类型，只扩 `Data`）。要加一条测试证明新 `Data` 能通过 SSE 与 WS 的既有过滤逻辑。
5. 控制台读取封装：新增 `web/src/api/executors.ts`（`listExecutors` 与 `getJobResult` 本卡完成）与 `web/src/api/types.ts` 的 `ExecMeta` / `JobResultResponse` 类型。本卡只加 API 层与类型，页面渲染在 E18。
6. `GET /api/v1/executors`（本卡一并落地，因为 E16 的表单校验与 E18 的档位列表都依赖它）：
   - 档位：`reader`。
   - 响应：`{ "enabled": bool, "required_role": null, "profiles": [ { "key":"exec.<name>", "name", "kind", "runtime_ok": bool, "reason": "", "timeout": "10m", "max_parallel": 1, "args": [{"name","required","default","pattern","secret"}], "env_allow": [], "url": "(仅 http 档位，模板原文)" } ] }`
     `required_role` 由 E16 补上真实取值，本卡先返回 `null` 并在注释里写明去向（避免两张卡都以为对方做了）。
   - 不返回：`env` 的固定值、脚本绝对路径（只给相对 workspace 的路径）、档位间顺序不稳定的字段。
   - 登记表未注入（`enabled=false` 或未 With）时返回 `{"enabled":false,"profiles":[]}`，不是 503：这不是错误状态，是默认状态。
   - 排序：按 `key` 字典序（E03 的 `Registry.Keys()` 已保证）。

## 4. 实现步骤

1. 先改 `core/scheduler.go` 的事件 `Data`（最小改动，跑 `core` 测试看有没有断言事件 `Data` 精确形状的既有用例）。
2. 加 `SetEventPreviewLimit` 并在 `cmd/server/main.go` 装配。
3. 加 `WithArtifacts` Option 与守卫。
4. 写 `handlers_executors.go` 的 `GetJobResult`：先取快照（`store.LoadAll`，与 `GetJob` 同一取法）→ 判档 → 解析参数 → 读产物 → 处理 `purged` 回写。
5. 加 `GET /executors`（放在同一个新文件里），注册到 `setupRoutes` 的 `api` 组：`api.GET("/executors", reader, s.ListExecutors)`。
6. 注册路由到 `setupRoutes` 的 `jobs` 组：`jobs.GET("/:id/result", reader, s.GetJobResult)`。
7. 加前端类型与 API 封装。

## 5. 测试要求

1. `core/scheduler_timeout_test.go` 或新增 `core/scheduler_events_test.go`：
   - `TestEvent_CarriesExecResult`：处理函数里给 `job.Exec` 赋值 → 完成事件 `Data.result.kind` 正确、`preview` 被夹到上限。
   - `TestEvent_NoExecMeansUnchangedData`：`Exec == nil` 时事件 `Data` 与改动前逐字节一致（守住既有断言）。
   - `TestEvent_PreviewLimitRespected`：`SetEventPreviewLimit(8)` + 100 字节预览 → 事件里不超 8 字节。
2. `api/handlers_executors_test.go`（用真的临时 `ArtifactStore`，不要 mock 文件系统）：
   - `TestGetJobResult_TailAndHead`：造 20KB 输出，`from=tail&max_bytes=1024` 与 `from=head` 各断言内容与 `truncated`。
   - `TestGetJobResult_BadStream` → 400；`attempt` 超范围 → 400；任务不存在 → 404；非执行器任务 → 404。
   - `TestGetJobResult_PurgedArtifact`：摘要在、文件已删 → `found:false`，且再次读取时快照 `artifact` 已是 `purged`（验证回写）。
   - `TestGetJobResult_RequiresArtifactsOption`：未注入 → 503（照 `requireGroupStore` 的既有用例写法）。
   - `TestGetJobResult_NoStore`：`Cache-Control: no-store`。
   - `TestGetJobResult_AuthRequired`：启用鉴权时无凭据 401；viewer 凭据 200（本卡档位就是 reader，E16 再测收严）。
3. `TestListExecutors`（新增，同一测试文件）：
   - `enabled=true` + 三条档位（含一条探测失败）→ 200，`profiles` 按 `key` 字典序、三条齐全，失败那条 `runtime_ok:false` 且 `reason` 非空。
   - `enabled=false` → `{"enabled":false,"profiles":[]}`，不是 503、不是 401（未启用鉴权时）；启用鉴权时无凭据 → 401。
   - 断言响应里不含 `env` 固定值、不含脚本绝对路径（用带敏感字面值 `JWT-secret-should-not-appear` 的档位配置，`strings.Contains` 断言全文不含该串）。
4. `api/sse_test.go`：新增一条，断言带 `result` 的完成事件能通过 `event_types=job.completed` 订阅送达，且 `job_ids` 过滤仍生效。
5. 前端：`cd web && npx vue-tsc --noEmit` 通过（只加类型与封装，不接页面）。

## 6. 完成标准（DoD）

- [ ] `/result` 的所有失败分支都有明确状态码和可读 `message`，不出现 500 兜底。
- [ ] 摘要进事件，且 `Exec == nil` 的既有事件形状完全不变（有测试证明）。
- [ ] 事件里的预览字节数受配置控制，且代码里有第二次截断。
- [ ] 产物被清理后不会反复读空文件：回写 `purged` 生效。
- [ ] `WithArtifacts` 未注入时端点 503，服务其余部分不受影响（`go test ./api` 全绿，不需要为该 Option 改现有用例）。
- [ ] 手工能看到：任务失败后，`GET /events` 里该任务的 `job.failed` 事件带 `result.exit_code`。

## 7. 验收方式

```bash
go build ./... && go vet ./... && go test ./... -race
go test ./api -run 'GetJobResult|ExecResult' -v
go test ./core -run 'Event_' -v

# 手工（需要先完成 E09，否则只能测 404/503 分支）
curl -s "localhost:8080/api/v1/jobs/<id>/result?from=tail&max_bytes=2048" | head -c 400
curl -s "localhost:8080/api/v1/events?limit=5" | grep -o '"result":{[^}]*}'
```

## 8. 不在本任务范围

- 不做 `?format=text` 的纯文本输出（会把鉴权与内容类型变成两套逻辑，且没有页面需求）。
- 不做范围读取（`offset`/分页）：大文件用 `from=head|tail` 加 `max_bytes` 足够，真需要分段下载再立卡片。
- 不做权限收严与掩码（E16）；本卡的 `/executors` 与 `/result` 一律 `reader`。
- 不改 `api/history.go` 的缓冲容量或淘汰策略。

## 9. 风险与回滚

- 风险：在 `executeJob` 里改事件 `Data` 属于共享路径，`core` 与 `api` 两侧都有针对事件的断言。要求第 5.1 节第二条测试明确覆盖"无执行结果时事件不变"，否则回归面不可控。
- 风险：`/result` 读文件是在请求线程上做的，几 MB 文件 + `max_bytes` 上限足以限制，但如果 `executors.output.max_bytes` 被配成很大（例如 100MB），一次请求就要读很久。缓解：端点内再加一个代码常量硬上限（建议 8MB），超过时报 400 并说明。
- 风险：回写 `purged` 会在读路径上触发 `store.Update`。频率极低（每任务每尝试一次），可以接受，但要在注释里写明"这是读路径上唯一一处写"，避免后来者在同一函数里继续加写。
- 回滚：`api` 侧是新增文件 + 一条路由，`core` 侧是事件 `Data` 的追加，单独 revert 本卡提交即可。

## 10. 实现记录（2026-09-30）

改动文件：新增 `api/handlers_executors.go`、`api/handlers_executors_test.go`、
`core/scheduler_events_test.go`、`web/src/api/executors.ts`；
改 `api/server.go`（`WithArtifacts`、`Server.artifacts`、两条路由）、`api/sse_test.go`、
`core/scheduler.go`（事件 `Data`、`SetEventPreviewLimit`）、`core/job.go`（`TrimExecPreview`
与产物状态常量）、`core/job_test.go`、`executor/result.go`（裁剪规则改为转调 core）、
`executor/artifact.go` 与其测试（新增 `Stat`）、`cmd/server/main.go` 与
`cmd/server/main_integration_test.go`、`web/src/api/types.ts`。

### 与卡片的偏离与补充

1. **`requireExecutorRegistry()` 没有做成 503 守卫**（E04 §3.4 要求、E04 §10 第 5 条与
   E05 §10 第 6 条登记"同批补"）。它与本卡 §3.6 以及 `docs/design/executor-design.md` §6.1
   直接冲突：那两处规定"没装配登记表"要回 `{"enabled":false,"profiles":[]}` 并明确写着
   "这不是错误状态，是默认状态"。按后写且更具体的口径实现，结果是——
   `Server.executors` 的两个读取方（`ListExecutors`、既有的 `execPreviewLimit`）都对 `nil` 做了
   降级；503 守卫只给了真正需要读文件的 `requireArtifacts()`（§3.1）。
   E04/E05 留下的那两条待办到此收口，后续卡片不要再等这个函数出现。
2. **新增 `ArtifactStore.Stat(jobID, attempt, stream)`**（卡片未列）。响应里的 `size_bytes` 要的是
   文件总量，而 `Read`/`Tail` 只给内容与"是否截断"，还原不出原始大小；用例 `TestStat`。
3. **新增 `core.ArtifactAvailable` / `core.ArtifactPurged` 两个常量**。设计文档 §6.4 与本卡写的是
   字面量 `available`/`purged`；填它的是 E09（执行侧），改它的是本卡（读侧），用一个常量而不是
   两处各拼一次字符串。
4. **`max_bytes` 分两层上限**：显式取值超过端点硬上限（代码常量 `8 MiB`）报 400 并在 details 里说明
   顶在哪里（§9 的缓解措施）；超过落盘上限 `executors.output.max_bytes` 时夹到该值而不是报 400——
   文件不可能比落盘上限更大，多要的部分并不存在，为一个读不到的请求回 400 只是在为难调用方。
   省略时的默认值是 `inline_preview × 4`，同样夹到落盘上限。
5. **预览裁剪规则挪到 `core.TrimExecPreview`**，`executor.TrimPreview` 变成一层转调（E05 §10 第 2 条
   把它放在 executor）。本卡要求"事件发布处再截一次"，而事件是 core 发的，core 不能反向依赖
   executor；留着两份实现迟早算出不同长度的预览，正是当初导出 `TrimPreview` 想避免的事。
   规则本身一字未改（尾部、字符边界、`limit<=0` 返回空串），用例同时留在两边。
6. **`SetEventPreviewLimit` 的取值规则**照同文件 `SetConcurrency`/`SetQueueCapacity` 的体例：
   非正数回退到 `DefaultExecInlinePreview`，`Start` 之后调用忽略并记一条 warn。卡片只写"默认取 2048"。
7. **产物缺失的回写只发生一次**，且写的是复制出来的摘要：`store.LoadAll()` 返回的快照与存储内部
   共用同一个 `*core.ExecMeta`，直接改它会和落盘协程的读法形成竞争（`-race` 下会报）。
   条件收紧为 `Exec != nil && Exec.Artifact != purged`，所以第二次读不再写。
   用例 `TestGetJobResult_PurgedArtifact` 用一层计数存储断言"两次请求只写一次"，
   并把注释写成"这是读路径上唯一一处写"（§9 要求）。
8. **`api` 的 500 只留给真读不出来的情况**：`ErrArtifactMissing` 之外的存储错误（权限、
   任务 ID 的写法不能当目录名）走 500，原文进日志与 details、不进 message。
   DoD 的"不出现 500 兜底"约束的是列出的四个分支（400/403/404/503），这一条不是兜底。
9. **参数写法收严**：`stream`/`from` 严格小写（`stream=OUT` 是 400，不是"大小写不敏感地当成 out"）；
   `attempt=0` 与省略同义，读最近一次已结束的尝试；`attempt` 超范围、非数字都 400。
   卡片只写了"0/省略表示默认"，这里把每条写法都固定在用例里。
10. **`meta` 字段没有 `omitempty`**：非执行器任务却存在产物文件时（摘要丢了、手工删过 jobs.json 的
    记录）回 `"meta":null`，让读侧区分"没有摘要"与"摘要内容恰好是空"。
11. **SSE 用例里的过滤参数是 `job_types`**：卡片 §5.4 写的 `job_ids` 在 `api/sse.go` 里并不存在，
    SSE 按事件的 `JobName` 过滤（`job_types`）。用例按 `event_types=job.completed&job_types=exec.demo`
    写，`publishedEventTypes` 一行未改（§3.4）。
12. **前端顺带补了 `Job.exec`**：E05 已经把 `exec` 放进 `JobResponse`，但 `web/src/api/types.ts`
    的 `Job` 当时没列在本卡文件清单里。本卡加 `ExecMeta` 类型时一并接上，页面渲染仍留给 E18。
13. **装配多传一个参数**：`runtimeDeps.newServer` 的闭包签名加 `*executor.ArtifactStore`（卡片未写）。
    产物存储建在 `if cfg.Executors.Enabled` 里面，注入接口需要把变量声明提到 if 之外，
    于是 run 的这段改成"先声明、开关打开时才创建"；关闭时传下去的是 `nil`，接口回 503。
    `spyScheduler` 随之补 `SetEventPreviewLimit`。

### 验证结果

- 单元测试新增：`core/scheduler_events_test.go` 5 个用例（成功事件带 result、失败事件 error+result、
  `Exec==nil` 时 data 逐字节不变、`SetEventPreviewLimit(8)` 把 100 字节预览夹到 8、默认上限与非正数回退），
  `core/job_test.go` 的 `TestTrimExecPreview`（规则搬家后的直接覆盖）；
  `api/handlers_executors_test.go` 11 个用例（`from=tail|head`、正好读满不算截断、空的 stderr、
  默认 `max_bytes` 跟随 `inline_preview`、八种 400 写法、404 两种、`purged` 降级与只写一次、
  未注入存储 503、`no-store` 头、鉴权 401/viewer 200、档位列表排序与不可用原因、
  `enabled=false` 与未注入都是 `{"enabled":false,"profiles":[]}`、`required_role` 为 null、
  响应不含 `env` 固定值与 workspace 目录名）；`api/sse_test.go` 1 个；`executor/artifact_test.go` 1 个（`TestStat`）。
- 事件形状的回归检查按要求做了两次：`TestEvent_NoExecMeansUnchangedData` 断言失败事件的 data 仍是
  `{"error":"boom"}`、成功事件仍是"没有 data 字段"；`go test ./api` 与 `./core` 全绿且没为
  `WithArtifacts` 改动任何既有用例（DoD 第 5 条）。
- `go build ./...`、`go vet ./...`、`go test ./... -race -count=1` 全绿；linux、darwin 交叉编译与
  `-tags dashboard` 构建通过；改动文件按 LF 副本 `gofmt -l` 无输出（`core/job.go`、
  `cmd/server/main_integration_test.go` 本身是 CRLF，检查时先转 LF 副本）。
- 真实进程冒烟一（临时目录里的独立驱动：core 调度器 + api.Server + 真产物文件，跑完已删除，
  仓库 `configs/` 与 `data/` 未被写入）。两个 handler 冒充执行器任务（写产物文件并填 `job.Exec`）、
  一个普通任务做对照，`SetEventPreviewLimit(16)`：
  `GET /events` 里 `job.completed` 的 `data.result` 带 `exit_code=1`、`out_bytes=200`、
  `preview` 只有 16 字节（执行侧写的是 200 字节，证明第二次裁剪生效），`job.failed` 同时带
  `error` 与 `result`，普通任务的 `job.completed` 的 `data` 仍是 `null`；
  `GET /jobs/:id` 的 `exec.preview` 仍是完整 200 字节（证明事件裁剪没改到快照引用的对象）。
  `GET /jobs/:id/result?from=tail&max_bytes=120` → 200、`size_bytes=200`、`returned_bytes=120`、
  `truncated=true`、`Cache-Control: no-store`；`from=head` 给开头 120 字节；省略参数给全 200 字节且
  `truncated=false`；`stream=err` → `found=true`、`size_bytes=0`；`attempt=2`（只跑过一次）→ 400；
  `stream=raw`、`max_bytes=90000000` → 400 并说明允许的写法；普通任务 → 404
  `no execution result for this job`；不存在的任务 → 404。
  随后手工删掉那个任务的产物目录再读：200、`found=false`、`content=""`、`meta.artifact="purged"`，
  第二次读仍是 purged，且 `GET /jobs/:id` 的快照里 `exec.artifact` 已是 `purged`（回写落到了存储）。
  `GET /executors`（驱动里没注入登记表）→ 200 `{"enabled":false,...}`，不是 503。
- 真实进程冒烟二（`go build ./cmd/server` 出来的二进制 + 系统临时目录里的一份最小配置，
  `executors.enabled: false`）：`GET /executors` → 200 `{"enabled":false,"profiles":[],"required_role":null}`；
  `GET /jobs/whatever/result` → 503，message `execution output storage is not configured`、
  details 指到 `api.WithArtifacts`；进程 cwd 与临时目录都没有被建出 `data/exec`（E06 的开关口径没被破坏）。
- 优雅关闭与清理协程仍由 E06 的用例覆盖；本卡的冒烟用 `taskkill /F` 结束进程，走不到那条路径。
- 提交按关注点切成六个（core 事件与裁剪规则、`ArtifactStore.Stat`、api 两个端点、cmd/server 装配、
  web 的 API 层与类型、文档），每个提交单独 `go build ./...` + `go vet ./...` + 相关包测试验过可编译。
  这与 `README.md`"每张卡片一个提交"的写法不同，沿用的是 E04/E05/E06 三张卡已经采用的做法：
  本卡同时改 core 共享路径与 api 新增文件，切开后任一块出问题可以单独回退（§9 的回滚口径）。

### 未验证

- `job.failed` 事件带 `result` 的"真执行器任务"路径要等 E09 的 runner：冒烟里用手写 handler 冒充，
  结论字段与产物文件都是真的，但"退出码来自真进程"这一条此刻不可能验证。
- 含 `secret` 参数档位的读侧收严（E16）。本卡两个端点都是 `reader`，判档点写在
  `GetJobResult` 的注释里（取到快照之后）。

### 留给后续卡片的接口形状

- E09：把 `ArtifactInfo` 填进 `core.ExecMeta` 时，`Artifact` 写 `core.ArtifactAvailable`；
  `WriteMeta` 的内容结构仍没人读。
- E16：`GET /executors` 的 `required_role` 目前是 null，那一卡把 `Registry.RequiredRole()` 接进
  提交路径时一起给出真实取值；含 `secret` 参数的档位要把 `/result` 的档位从 `reader` 收到
  `executors.required_role`，并在响应里补 `redaction_note`。
- E18：`web/src/api/executors.ts` 的 `listExecutors()` / `getJobResult(id, query)` 与
  `types.ts` 的 `ExecMeta`/`JobResultResponse`/`ExecutorListResponse` 已可用，页面直接调。
