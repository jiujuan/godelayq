# TASK-S04　事件端点改为读库（DB 优先、内存兜底）

- 所属阶段：M1 事件
- 依赖任务：TASK-S01、S02、S03
- 涉及文件：改 `api/server.go`、`api/handlers_events.go`、`api/handlers_events_test.go`、`cmd/server/main.go`；改前端三处注释 `web/src/api/events.ts`、`web/src/composables/useJobEvents.ts`、`web/src/views/JobDetailView.vue`
- 预计规模：小

## 1. 任务目标

`GET /api/v1/jobs/:id/events` 与 `GET /api/v1/events` 在装配了事件库时改为读库，未装配时读内存缓冲；响应里的 `Note` 跟着分岔，前端界面上"重启即清空"的说明自然变成"取自持久化事件库"。

## 2. 背景与当前问题

S03 把事件写进了库，但两个端点还只读 `api.EventHistory`（`api/handlers_events.go:42`、`:58`），因此用户看到的行为一字未变——本卡补上读的一侧。

三个已经存在的约束决定了实现形状：

1. `Note` 是一个写死的字符串常量 `"in-memory buffer, cleared on restart"`（`api/handlers_events.go:22`），注释说明"前端 UI 直接引用它，省得两处各写一句、日后不同步"。读库之后这句话不再成立。
2. 前端确实只引用、不比对：`useJobEvents.ts:63` 与 `useEventFeed.ts:55` 都是 `query.data.value?.note ?? ''` 直接渲染（`JobDetailView.vue:286`、`MonitorView.vue:110`），没有字符串相等判断。**所以改文案不会破坏前端逻辑，但会让三处注释变成假话**（`api/events.ts:1`"都是内存缓冲，重启即清空"、`useJobEvents.ts:62` 引用了旧句子、`JobDetailView.vue:5`"进程重启后从头计"），本卡要一起改。
3. `?limit=` 的上界当前受内存窗口约束：`parseEventLimit` 会把请求值压到 `capacity`（`api/handlers_events.go:24-34`），即每任务最多 100、全局最多 500。库里能留 20 万条，这个压制要松开，但**松开是响应能力的扩大**，必须写进 `docs/api.md`，不能被当成回归。

为什么可以"只看库、不与内存合并"（设计文档 D5、§8.1）：批量写入的可见性延迟上界就是一个 `flush_interval`（默认 200ms），而这两个端点的用途是详情页与 Dashboard 的首屏回灌（`api/handlers_events.go:37-39`），实时增量由 WebSocket 推送承担。200ms 换掉"跨来源去重与定序"的复杂度是划算的。

## 3. 要实现的功能

1. `api/server.go`：
   - 定义消费方接口，**不在 api 包 import 驱动**：

     ```go
     // eventReader 是事件时间线的持久化读取方，由 store/sqlite.EventLog 实现。
     // nil 表示这次部署没装配事件库，两个端点退回内存缓冲（即 S03 之前的行为）。
     type eventReader interface {
         Events(jobID string, limit int) ([]core.Event, error)
         Recent(limit int) ([]core.Event, error)
     }
     func WithEventLog(r eventReader) Option
     ```

     接口定义在 `api` 侧而不是 `store/sqlite` 侧，与 `Server.groups core.GroupStore`、`artifacts *executor.ArtifactStore` 的既有取向一致（依赖以能力形式声明，装配方负责给出实现）。
   - `Server` 加 `events eventReader` 字段；`cmd/server` 里 `enabled && events.enabled` 时注入，否则传 nil（与 `WithArtifacts(nil)` ⇒ 503 的既有表达方式同构）。
2. `store/sqlite/events.go` 补两个读方法（接口要求的那两个签名）：

   ```go
   func (e *EventLog) Events(jobID string, limit int) ([]core.Event, error)  // 升序
   func (e *EventLog) Recent(limit int) ([]core.Event, error)                // 升序
   ```

   SQL：`SELECT ... ORDER BY seq DESC LIMIT ?`，取回后整体反转成升序（**排序契约不变**：现有端点按时间升序返回，前端时间线直接铺列）。
   `limit <= 0` 表示取 §3.4 的上界；查询出错时返回错误，由端点决定呈现（见下条）。
   行 → `core.Event` 的还原：`ts_us` → `time.UnixMicro(ts)`，`status` → `core.JobStatus(status)`，`data` 列原样进 `json.RawMessage`，`metadata` 反序列化成 map（NULL → nil）。
3. 两个端点各加一个分支（`api/handlers_events.go`）：

   ```
   if s.events != nil {  读库；库读失败 → 500 + ErrorResponse（与 ListJobs 的存储读失败同一口径）
   } else            {  读内存；错误不可能发生（现状路径） }
   ```

   **不做"读库失败自动退回内存"**：两条路径给出的数据范围不同（库有历史、内存只有本进程），静默退回会让人拿着一份不完整的列表做判断。报错更安全。
4. `Note` 分岔成两个常量：

   | 路径 | `Note` |
   | --- | --- |
   | 库 | `persisted event store; newest entry may lag by the write flush interval` |
   | 内存 | `in-memory buffer, cleared on restart`（原句不动） |

   两个常量并列放在 `api/handlers_events.go`，注释里写清"分岔依据是装没装配事件库，不是请求参数"。
5. `?limit=` 上界分岔：新增常量 `eventsQueryLimit = 1000`，库路径用它作为 `parseEventLimit` 的 `capacity`；内存路径继续用 `historyPerJobLimit` / `historyGlobalLimit`。不新增配置项（本仓库惯例：未生效的选项不进配置，这里 1000 是一次响应能给的合理上限，不是策略旋钮）。
6. `Count` 字段仍是 `len(items)`，不改成"库里总行数"：它的既有含义是"本次返回多少条"，改了会让前端计数出错。
7. 空结果仍是 `200` + 空列表，不返回 404（`web-console-design.md` §5.6 定下的口径：详情页时间线可能确实还没有事件）。任务 ID 不存在也走这条路，本卡不引入存在性检查（它需要读 `store`，与本卡范围无关）。
8. 前端三处注释按 §2.2 改掉，改成引用"由后端 `note` 说明"而不是复述某一句具体文本，避免下次文案再变时又留下假话。前端代码逻辑一行不改。

## 4. 实现步骤

1. 先给 `store/sqlite` 加两个读方法与行还原，配单测（含"同微秒按 `seq` 定序""降序查询反转成升序"两条）。
2. 再加 `api` 的接口与 Option，两个端点加分支与 `Note` 常量。
3. 补 `api/handlers_events_test.go` 的用例：库路径、内存路径、库读失败三条各一份。
4. 最后改前端三处注释，跑 `vue-tsc --noEmit`。

## 5. 测试要求

1. `TestGetJobEvents_MemoryPathUnchanged`：不注入事件库，断言响应与 S03 之前的字节级形状一致（含 `Note` 原句、`count`、升序）。**这条守住"默认关闭不改变任何行为"。**
2. `TestGetJobEvents_DBPath`：注入替身（api 测试里用假的 `eventReader`，不依赖 SQLite），断言 `items` 来自替身、`Note` 是库的句子、`job_id`/`count` 正确。
3. `TestGetJobEvents_DBPathLimit`：替身记录收到的 `limit`，断言 `?limit=500` 传到库路径时是 500、而内存路径同参数会被压到 100。
4. `TestGetJobEvents_DBReadErrorIs500`：替身返回错误 → HTTP 500 + `ErrorResponse`，且响应里**不**带内存路径的数据。
5. `TestListRecentEvents_BothPaths`：全局端点的两条路径各一份，含空结果 → `200` + `"items": []`（不是 `null`）。
6. `TestEventLog_EventsRoundTrip`（`store/sqlite`）：S03 写入的一批事件经 `Events()` 读回后，与 `core.Event` 原值在 `type/job_id/job_name/status/data/metadata` 上相等，`timestamp` 相差小于 1 微秒。
7. `TestEventLog_EventsAscending`：库里有 10 条同任务事件，`limit=3` 返回的是最后 3 条且升序。
8. `TestEventLog_NilMetadata`：写入时 `Metadata` 为 nil，读回是 nil 而不是空 map（`omitempty` 的 JSON 形状因此不变）。
9. 前端：`cd web && npx vue-tsc --noEmit` 通过（本卡只改注释，这一条是防手滑）。

## 6. 完成标准（DoD）

- [ ] 未注入事件库时两个端点的响应逐字段与改动前一致，有 §5 第 1 条用例守着。
- [ ] 注入后重启进程，详情页时间线仍有重启前的事件（手工步骤见 §7）。
- [ ] `Note` 与实际读取来源一致：不存在"读库却写着内存缓冲"或反之的组合。
- [ ] 排序契约不变（升序）、`count` 语义不变、空结果仍是 200 + 空列表。
- [ ] 库读失败返回 500，不静默退回内存。
- [ ] 前端三处注释不再声称"重启即清空"，且 `api` 包没有 import SQLite 驱动（`go list -deps ./api | grep sqlite` 为空）。
- [ ] `docs/api.md` 的"运行事件 API"一节同步（`Note` 两个取值、库路径 `limit` 上界 1000、微秒精度）；`web-console-design.md` §5.6 的"持久化审计不在本期范围"标注改为指向本卡。

## 7. 验收方式

```bash
go build ./... && go vet ./... && go test ./... -race
go test ./api -run 'Events|EventLog' -v
go test ./store/sqlite -run EventLog -v
cd web && npx vue-tsc --noEmit
```

手工（临时目录配置，跑完删除）：`observability.enabled: true`，`workers: 2`；
提交一条任务、等它跑完，`GET /jobs/<id>/events` 记下返回条数与 `note`；
`Ctrl-C` 停服再启动，同一请求应返回**同样多**的事件、`note` 换成持久化那句；
再把 `observability.enabled` 改回 `false` 启动，同一请求返回条数变少（只剩本进程窗口内的事件）、
`note` 回到原句。三步都不需要清库，只靠开关切换。

## 8. 不在本任务范围

- 不新增事件端点、不给 `/jobs/:id/events` 加分页游标（`before_seq` 之类）：本期 1000 条上界够用。
- 不做事件统计/聚合接口。
- 不改 `EventHistory` 的三档常量、不删内存缓冲：它是未启用库时的读路径，也是 §5 第 1 条用例的对照实现。
- 不改 WS/SSE 的任何行为：实时通道继续读总线，与库无关。
- 不做前端页面改动（只改注释文本）。审计页在 S08。

## 9. 风险与回滚

- 风险：`ts_us` 只到微秒，`time.Time` 的纳秒部分在库路径上丢失，因此"读回来的 `timestamp` 与写入时不完全相等"。这是设计后果，不是缺陷，但**必须写进 `docs/api.md`**，否则前端若拿时间戳做精确比对会出现看起来随机的差异。§5 第 6 条用例用"相差小于 1 微秒"的断言把这个精度界限固定下来。
- 风险：`limit` 上界从 100 放宽到 1000，最坏情况一次响应的体积放大约十倍。1000 条 × 每条含 2KB 预览的执行结果事件，理论上能到 MB 级。因此 `docs/api.md` 要写明：任务时间线的常规用法不需要超过 100，1000 是给排障导出用的。若实测体积不可接受，收紧为 200 是安全的（不改库结构）。
- 风险：库路径与内存路径给出的 `items` 在同一瞬间可能不同（最新一批还没落库）。`Note` 的第二句就是为这个说的；不要把"WS 已经推过来的事件在 REST 里查不到"当 bug 修。
- 回滚：把 `cmd/server` 的注入去掉（传 nil）即回到内存路径，接口行为一字不变；代码层回滚是 revert 本卡单个提交。

## 10. 实现记录（执行时补写）
