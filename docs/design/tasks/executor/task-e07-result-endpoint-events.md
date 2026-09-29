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
