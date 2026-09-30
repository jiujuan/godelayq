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

- [x] 未注入事件库时两个端点的响应逐字段与改动前一致，有 §5 第 1 条用例守着。（`TestGetJobEvents_MemoryPathUnchanged`：除结构体外还断言响应文本里有原句、且不含 `persisted`；`TestWithEventLogNilKeepsMemoryPath` 补了"显式传 nil 与不注入等价"）
- [x] 注入后重启进程，详情页时间线仍有重启前的事件（手工步骤见 §7）。（§10.4：三轮启动，第二轮的响应与第一轮逐字节相同）
- [x] `Note` 与实际读取来源一致：不存在"读库却写着内存缓冲"或反之的组合。（两个常量各只在一处出现；`TestRun_NoServerReaderWhenEventsDisabled` 钉住"没装配时交出去的就是 nil"，所以分岔只有一个依据）
- [x] 排序契约不变（升序）、`count` 语义不变、空结果仍是 200 + 空列表。（`TestEventLog_EventsAscending`、`TestListRecentEvents_BothPaths` 的"库空结果"子例；`count` 仍是 `len(items)`，见 §10.1）
- [x] 库读失败返回 500，不静默退回内存。（`TestGetJobEvents_DBReadErrorIs500`：内存缓冲里先放一条真事件，断言 500 的响应体里不带它；全局端点同口径另有一条子例）
- [x] 前端三处注释不再声称"重启即清空"，且 `api` 包没有 import SQLite 驱动（`go list -deps ./api | grep sqlite` 为空）。（实际改了六处，见 §10.2 第 6 条；依赖边界见 §10.3）
- [x] `docs/api.md` 的"运行事件 API"一节同步（`Note` 两个取值、库路径 `limit` 上界 1000、微秒精度）；`web-console-design.md` §5.6 的"持久化审计不在本期范围"标注改为指向本卡。（另外补了时区表示、`count` 语义、500 口径，以及 `DELETE /admin/events` 只管内存缓冲那一条，见 §10.5 的 D-0401）

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

完成日期：2026-10-01。改 `store/sqlite/events.go`、`api/server.go`、`api/handlers_events.go`、`cmd/server/main.go` 与三个测试文件，另改前端六处注释与本卡涉及的三份文档。前端与接口的行为分岔只有一处依据：装没装配事件库。

### 10.1 落地的接口

```go
// store/sqlite/events.go
func (e *EventLog) Events(jobID string, limit int) ([]core.Event, error)  // 升序
func (e *EventLog) Recent(limit int) ([]core.Event, error)                // 升序

// api/server.go
type eventReader interface {
    Events(jobID string, limit int) ([]core.Event, error)
    Recent(limit int) ([]core.Event, error)
}
func WithEventLog(r eventReader) Option
```

读侧形状：`ORDER BY seq DESC LIMIT ?` 取回后整体反转成升序（要的是尾部 N 条，降序走 `seq` 索引只读 N 行）；`seq` 而不是 `ts_us` 做定序依据，同微秒连发的两条才分得出先后。行还原由 `scanEvent` 完成，`data` 原样进 `json.RawMessage`，NULL 的 `metadata` 还原成 nil map，`ts_us` 经 `time.UnixMicro` 还原。

端点侧：两条路径各一句 `Note`（常量并列放在 `api/handlers_events.go`），库路径的 `limit` 上界是新常量 `eventsQueryLimit = 1000`，`count` 仍是 `len(items)`，空结果仍是 200 + `[]`，库读失败是 500 + `ErrorResponse`。

### 10.2 与本卡写法的差异

1. **注入走 `runtimeDeps.newServer` 的新参数**（本卡 §3.1 只说"`cmd/server` 里 `enabled && events.enabled` 时注入，否则传 nil"）。实际把 `newServer` 的闭包签名扩成六参数，最后一个就是事件写入器；默认闭包在非 nil 时追加 `api.WithEventLog(events)`。理由：写入器在 `scheduler.Start()` 之前创建，服务在其后创建，两者之间只有这个闭包是通道，没有别的注入点。连带改了 20 处测试字面量的参数列表。
2. **`eventLogAPI` 扩了读取面**（S03 的记录里它是 `Close` + `Dropped` 两个方法）。现在是四个：关停面给 `run` 用，读取面原样交给 `api.WithEventLog`。不扩就得在 `run` 里做一次类型断言，而那是编译期就能确定的事，用断言换接口宽度不值得。`api` 侧仍然只有自己的 `eventReader`，`go list -deps ./api` 里没有驱动（§10.3）。
3. **`limit <= 0` 的兜底常量放在 store 侧**（本卡 §3.2 指向 §3.4 的上界）。实现是 `store/sqlite` 的 `eventsReadLimitFallback = 1000`，与 `api` 的 `eventsQueryLimit` 同值、两份独立，注释互指。不让 `store/sqlite` 反向依赖 `api` 的常量，也不为这一个数开配置项（本仓库惯例）。
4. **端点多了一个 `eventList` 归一**（本卡未列）。读取方返回 nil 切片时，响应会序列化成 `"items": null`，而 §5 第 5 条要求 `[]`。替身用例把这条暴露出来后，归一放在端点侧而不是要求每个读取方都返回非 nil——内存路径本来就返回非 nil（`api/history.go` 的 `tail` 总是新建切片），所以这个归一只作用在库路径。
5. **§5 第 3 条用例换了实现方式**（`TestGetJobEvents_DBPathLimit` → `TestEventLimitCapacityDiffersByPath`）。库路径收到的 `limit` 用替身记录（`?limit=500` 原样、`?limit=5000` 压到 1000）；内存路径的压制在只有两三条事件的响应里看不出来，所以那半边直接对 `parseEventLimit` 断言两个 capacity 的差值。断言的内容与卡片要求一致，只是可观察的位置不同。
6. **前端注释改了六处而不是三处**（本卡 §3.8 点名三个文件）。同一次改动会让另外三处也变成过时描述：`useJobEvents.ts:3`（同一文件的另一句，写着"后端内存缓冲"）、`useEventFeed.ts:4`（"两边都来自同一个内存环形缓冲"——装配库之后首屏读的是库）、`api/admin.ts:19`（"清空…各详情页时间线从当前时刻重新开始"——清空只动缓冲，时间线读库时不受影响）、`MonitorView.vue:69` 的一条注释。全是注释与文档字符串，前端逻辑一行未改，`npx vue-tsc --noEmit` 通过。
7. **§5 的九条用例全部实现**，另有补充：`store/sqlite` 的读侧从 3 条加到 7 条（多出的 `TestEventLog_EventsOrderWithinSameMicrosecond`、`TestEventLog_Recent`、`TestEventLog_EventsIsolateJobs`、`TestEventLog_EventsAfterClose` 各守一条读侧契约），`cmd/server` 加两条装配用例（§10.2 第 1 条那个新参数带来的），`api` 加 `TestWithEventLogNilKeepsMemoryPath`。

### 10.3 验证证据

```
go build ./...                                     通过
go vet ./...                                       通过
go test ./... -race -count=1                       全绿（api 76s / cmd/server / core / executor / store/sqlite）
go test ./api -run "Event|Events" -v               35 条 PASS
go test ./store/sqlite -run EventLog -v            22 条 PASS
go test ./cmd/server -run "Observability|EventLog|NoServerReader" -v   10 条 PASS
cd web && npx vue-tsc --noEmit                     通过（真实工作树，web/dist 在位）
cd web && npm run build                            通过（产物 8 个 chunk，built in 3.70s）
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build ./...      通过
GOOS=windows GOARCH=386 CGO_ENABLED=0 go build ./...      通过
go build -tags dashboard ./... && go vet -tags dashboard ./...  通过
go list -deps ./api | grep -i "sqlite|modernc"     空
grep -rln "modernc.org/sqlite" --include=*.go .    只有 store/sqlite/db.go、store/sqlite/doc.go
git diff -- core                                   空
```

前两个代码提交各用 `git archive <sha> | tar -x` 在新目录里重建中间态，跑 `go build ./...`、`go vet ./...` 与 `go test ./store/sqlite ./api ./cmd/server`，两轮都通过。格式检查用剥 CR 的副本配 `gofmt -s`，七个改动文件全部干净。

### 10.4 手工验收（本卡 §7）

临时目录里独立二进制 + `configs/config.example.yaml` 的副本，路径全部走 `GODELAYQ_*` 覆盖，仓库的 `configs/config.yaml` 与 `data/` 未写。端口 8141，`workers: 2`，`observability.flush_interval: 50ms`，鉴权用静态 token。

第一轮（`observability.enabled: true`）提交一条 `delay: 3s` 的 `payment_check`，跑完后：

```
GET /api/v1/jobs/01a0f3a8-…188a/events
{"job_id":"01a0f3a8-…188a","count":3,
 "items":[job.scheduled / job.started / job.completed 三条，升序],
 "note":"persisted event store; newest entry may lag by the write flush interval"}
```

强制结束进程、第二轮同样配置再起：同一请求的响应体与第一轮 **逐字节相同**（`diff` 无输出），三条事件、顺序、`metadata` 内容都不变。这就是"重启后详情页时间线仍有历史"在本卡能取到的最强证据——注意是在进程被强制结束时验的，不是干净关闭。

第三轮把 `GODELAYQ_OBSERVABILITY_ENABLED` 改成 `false`（库文件原地不动）：同一请求回 `{"count":0,"items":[],"note":"in-memory buffer, cleared on restart"}`，启动日志里没有 `observability enabled` 那一行。同一次请求里 `?limit=500` 也回内存那句，说明分岔只看装配、不看参数。

顺带取到两条本卡 §9 第一条风险的实测证据：库里读回的 `timestamp` 是 `2026-10-01T02:51:09.902971+08:00`，而任务创建响应里同一条事件的时间是 `…9029713+08:00`——纳秒部分被截断，且偏移是本地时区。已写进 `docs/api.md`。

### 10.5 缺陷处置

| 编号 | 严重度 | 事实 | 处置 |
| --- | --- | --- | --- |
| D-0401 | 中 | `DELETE /api/v1/admin/events` 只清内存缓冲（`api/handlers_admin.go:63` 的 `ClearEventHistory` 调 `s.history.Clear()`）。装配事件库后详情页时间线读的是库，这个端点不再影响运维在界面上看到的"清一下时间线"；`AdminView.vue:223` 的说明文字因此过时 | **已文档化 + 功能登记不修**（口径写进 `docs/api.md` 的"运行事件 API"：清空只动缓冲，库里的事件由 `retention_count`/`retention_age` 淘汰）。要给库加清空能力需要新方法与权限设计，本卡 §8 未含 |
| D-0402 | 中 | 前端去重键是 `job_id|timestamp|type`（`useEventFeed.ts:16` 的 `eventKey`，本卡改过该文件头部注释所以行号有漂移）。库里读回的时间戳精确到微秒且是本地时区表示，与 WS 直接推的那份文本不严格相等，所以"同一条事件既被 WS 推过、又出现在首屏"时归并不去重，界面上会短暂重复一行 | **登记不修**（本卡 §8 明确不改前端逻辑；触发条件是刷新与推送抢同一瞬间，而实时增量本来就由 WS 负责）。要修的形态是把归并键换成事件自带的单调量，但库里没有把它透出到 `core.Event`，属于后续卡 |
| D-0403 | 低 | 三处用户可见文案仍写"后端缓冲/内存缓冲是时间线数据源"：`MonitorView.vue:110` 副标题、`AdminView.vue:223` 段落、`web/src/content/job-template.md:253`（模板页表格）。本卡只改了注释 | **登记待点**（§8 写着"不做前端页面改动（只改注释文本）"，所以没动）。改动都是一行措辞，等用户点头再改，或者并进 S08 |

### 10.6 未覆盖与已知边界

- **优雅停服这一步在本机做不到**：Windows 下无法从外部给控制台进程发 SIGTERM。三轮手工验收的"停"都是强制结束，因此"重启后仍有历史"是在最硬的停法下验的，而"干净关闭会检查点 WAL"仍只有 S02 的 `TestCloseCheckpointsWAL` 覆盖。
- **恢复阶段的 `job.scheduled` 是否进库仍未手工验证**（S03 的 §10.6 留待本卡补，本卡没有补上）：三轮冒烟用的任务都是新提交后当场跑完，`jobs.json` 里没有崩溃时正在执行的任务，所以时间线头几条不是恢复产生的。要验它需要制造一条 `running` 状态的任务再强制结束。登记给后续（与本卡名义范围无关，且需要真任务执行到一半）。
- **`?limit=` 上界 1000 的实际响应体积没有测**：§9 第二条风险给的是推算（1000 条 × 2KB 预览）。冒烟只有三条事件，压不出体积。真要收紧到 200 不需要改库结构。
- **本卡 §2 的两处行号有漂移**（写卡时的代码与执行时差 3 行）：内存读取实际在 `api/handlers_events.go:45`（`Events`）与 `:59`（`Recent`），卡上写的是 `:42`、`:58`。引用的内容与结论不受影响，只是照行号跳转会偏。§2 提到的 `api/history.go:12-19`、`api/events.ts:1`、`useJobEvents.ts:62-63`、`JobDetailView.vue:286` 逐条核实过，准确。
- **前端没有做浏览器实测**：本卡未改前端逻辑，界面文字仍由后端 `note` 决定，`npx vue-tsc --noEmit` 与 `npm run build` 都通过。替身用例断言的是常量值，不是"界面显示出来的那句话"；要截图取证应与 D-0403 的三处文案一并处理（S07 或 S08）。

