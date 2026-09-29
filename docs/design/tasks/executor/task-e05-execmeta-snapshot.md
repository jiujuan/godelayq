# TASK-E05　执行结果结构与任务快照落盘

- 所属阶段：M1 结果通道
- 依赖任务：TASK-E01…E04
- 涉及文件：`core/job.go`、`core/job_test.go`、`core/job_store_test.go`、`core/store_test.go`、新增 `executor/result.go`
- 预计规模：中

## 1. 任务目标

定义"一次执行的结果"这个数据结构，并让它能随任务快照一起落盘、重启后还能读到。同时把 `Job` 到 `JobSnapshot` 的三处字段搬运补齐。本任务不执行进程，只提供结构和一个手工赋值的通路。

## 2. 背景与当前问题

`core/job.go` 的 `Handler` 签名是 `func(ctx, *Job) error`，返回值只有一个错误。全仓没有任何输出、退出码、HTTP 状态的概念（设计文档 §3 已核实）。执行器最需要的信息恰恰是"进程输出了什么、以什么状态退出"，所以现在必须先把这条数据通路建起来，否则 E09 写出来的执行逻辑结果无处安放。

放的位置有两个候选，本任务选第二个：

- 放进 `JobSnapshot` 的大字段：不行。`core/store.go` 的 `flushLocked` 是整文件重写，终态留痕默认 1000 条，每条带几十 KB 输出会让 `jobs.json` 膨胀到百 MB 级，每 200ms 重写一次。
- 只放摘要（退出码、状态码、时长、输出字节数、截断标记、尾部若干字节预览），完整输出走文件（E06）。

## 3. 要实现的功能

1. `core/job.go` 新增：

   ```go
   // ExecMeta 是一次执行的结论摘要，随任务快照落盘。
   // 完整输出不在这里：JSON 存储每次合并落盘都是整文件重写，见 docs/design/executor-design.md §6.4。
   type ExecMeta struct {
       Kind       string `json:"kind"`                 // script | binary | http
       Profile    string `json:"profile"`              // 档位名，不含 exec. 前缀
       ExitCode   int    `json:"exit_code,omitempty"`
       Signal     string `json:"signal,omitempty"`
       HTTPStatus int    `json:"http_status,omitempty"`
       DurationMs int64  `json:"duration_ms"`
       OutBytes   int64  `json:"out_bytes"`
       ErrBytes   int64  `json:"err_bytes"`
       Truncated  bool   `json:"truncated,omitempty"`
       Permanent  bool   `json:"permanent,omitempty"`   // 不重试的失败，E12 使用
       Preview    string `json:"preview,omitempty"`     // 尾部预览，长度上限由调用方保证
       Artifact   string `json:"artifact,omitempty"`    // available | purged | 空
   }
   ```

2. `Job` 增加运行期字段 `Exec *ExecMeta`（`json:"-"`，因为它不进 `Job` 的 JSON，持久化走快照）；`JobSnapshot` 增加 `Exec *ExecMeta`（`json:"exec,omitempty"`）。
3. 三处搬运，一处都不能漏（`Group` 字段的既有教训就是一个字段搬四处）：
   - `ToSnapshot`：复制指针即可，但要在注释里说明"写入之后不得再改这个对象"（`handleFailure` 与事件发布都会读它）。
   - `FromSnapshot`：还原。
   - `CloneForRetry`：**不带上一次的 `Exec`**，置为 `nil`。理由是重试副本代表一次新的执行，带旧结论会让 `GET /jobs/:id` 显示上一次的退出码。注释要写这句。
   - Cron 重排路径：如果 `handleSuccess` 里有重新排期构造新快照的代码，同样不带 `Exec`。实现时先确认 `core/scheduler.go` 的 `handleSuccess` 是否复用同一对象。
4. 指针语义：`Exec == nil` 表示"这次任务不是执行器任务，或还没执行完"。不要用空结构体表示"没有结果"，否则前端无法区分。
5. 兼容性：`JobSnapshot` 是 JSON 对象，老 `data/jobs.json` 没有 `exec` 键时解码为 `nil`。要有一条测试直接读一份不含 `exec` 的旧数据文件。
6. `executor/result.go` 提供构造器，让后面的卡片不必手写零散字段：

   ```go
   type Result struct{ Meta core.ExecMeta; Stdout, Stderr []byte }
   func (r *Result) SetPreview(limit int)      // 取尾部 limit 字节，按字符边界裁剪，不切坏 UTF-8
   func (r *Result) Truncate(maxBytes int)     // 置 Truncated
   ```

   真正的"边写边截断"由 E06 的写入器负责，本卡的 `Result` 只在内存里成型，方便单测。
7. `api/handlers.go` 的 `toJobResponse` 与 `api/dto.go` 的 `JobResponse` 增加 `exec` 字段透传（`GetJob`、`ListJobs`、创建响应都经 `Job.FromSnapshot` + `toJobResponse` 这一条路径，加一处即可全覆盖）。`Preview` 的裁剪在透传时再检查一次（防御式：不把超大字符串透给列表接口）。

## 4. 实现步骤

1. 加 `ExecMeta` 与两处字段，补三处搬运。
2. 先写兼容性测试（旧数据文件解码），再写搬运测试。
3. 加 `executor/result.go` 与预览裁剪工具。
4. 改 DTO 透传，跑 `api` 层测试。

## 5. 测试要求

1. `core/job_test.go`：
   - `TestJob_ExecSnapshotRoundTrip`：赋值 `Exec` → `ToSnapshot` → `FromSnapshot` → 字段逐项相等（含 `omitempty` 字段全部非零的一种、全部零值的一种）。
   - `TestCloneForRetry_DropsExec`：带 `Exec` 的任务生成重试副本，断言副本 `Exec == nil` 而 `RetryCount` 递增。
   - `TestExecMeta_OmitEmpty`：`json.Marshal` 一个只填必填字段的 `ExecMeta`，断言输出里没有 `signal`/`preview`/`truncated` 等键（保证 `jobs.json` 不被空字段撑大）。
2. `core/store_test.go` / `core/job_store_test.go`：
   - `TestJSONFileStore_LegacySnapshotWithoutExec`：手工写一份不含 `exec` 键的 `jobs.json`，`NewJSONFileStore` 加载后 `LoadAll` 正常，`Exec` 为 `nil`，随后 `Update` 再读回仍正常（证明读写不破坏老数据）。
   - `TestJSONFileStore_KeepsExecOutOfHotPathSize`：写入 500 条带 `Exec`（`Preview` 2KB）的快照，断言文件大小处于可预期范围（例如小于 5MB）。这条是 D4 决策的量化守卫，防止以后有人把 stdout 塞进快照。
3. `executor/result_test.go`：
   - `TestSetPreview_TailAtCharBoundary`：中文输出 + 超长输入，断言取尾部、不出现半个字符、长度不超上限。
   - `TestTruncate_SetsFlag`。
4. `api/dto_test.go`：`exec` 字段能从快照透传到响应；`Exec` 为 `nil` 时响应里不出现 `exec` 键。

## 6. 完成标准（DoD）

- [ ] 三处搬运全部到位，测试能证明漏改任意一处会失败。
- [ ] 老数据文件（无 `exec` 键）可直接读，无需迁移脚本。
- [ ] `CloneForRetry` 明确不带旧结论，且注释写清原因。
- [ ] `ExecMeta` 的序列化结果足够小：`Preview` 有上限、零值字段被省略，有用例断言。
- [ ] `go test ./... -race` 全绿，现有测试的断言没有被放宽。

## 7. 验收方式

```bash
go build ./... && go vet ./... && go test ./... -race
go test -run 'Exec|LegacySnapshot|CloneForRetry' ./core ./executor ./api -v
git show HEAD --stat        # 确认只改了 core/job.go、api/dto.go、executor/ 与测试
```

兼容性手工验证：把仓库里 `data/jobs.json` 复制一份，删掉某条记录里可能存在的 `exec` 键（当前数据文件里本来就没有），启动服务读一次任务列表，返回 200 且任务字段完整。

## 8. 不在本任务范围

- 不写文件（E06）。
- 不加 `/result` 端点（E07）。
- 不改 `JobStatus` 枚举：它有 int 落盘错位的既有约束（`core/job.go` 的常量块注释），新维度一律走 `ExecMeta`。
- 不改 `Handler` 的签名。

## 9. 风险与回滚

- 风险：在 `Job` 上加 `Exec` 字段意味着处理函数可以改任务对象。目前执行器任务由单个 worker 协程持有，安全；但如果有别的代码在任务执行期间读 `Job`（例如列表接口从堆里取），就会读到中间态。实现时确认读路径只走 `store.LoadAll` 的快照（`api/handlers.go` 的 `ListJobs` 已是这样），并在 `Exec` 字段的注释里写明"仅执行协程可写"。
- 风险：`Preview` 被 DTO 透传后，事件缓冲里若也带 `Preview`（E07 会做），单条事件会变大。要求事件里的预览字节数上限由 `executors.output.inline_preview` 控制，且默认值小（2KB）。
- 回滚：新增字段是加法，回滚只需 revert 本卡提交；已经写入 `exec` 键的 `jobs.json` 在旧代码下也能读（未知键在快照解码时被忽略，实现时验证这一点并记进测试注释）。
