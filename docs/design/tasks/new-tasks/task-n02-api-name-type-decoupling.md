# TASK-N02　api：名称与类型解耦、按类型筛选

- 所属阶段：M0 语义
- 依赖任务：TASK-N01
- 涉及文件：`api/dto.go`、`api/handlers.go`、`api/handlers_executors.go`、`api/api_test.go`、
  `api/handlers_executors_test.go`、`api/dto_test.go`、`docs/api.md`（本卡只改创建/更新两节，其余留给 N08）
- 预计规模：中

## 1. 任务目标

把"任务名称"与"任务类型"在 HTTP 接口上分成两件事：请求体新增 `type`（决定跑什么），
`name` 变回给人看的标签（必填、套 N01 的规则）。同时把接口里所有"按任务名找档位"的位置
换成按注册键找，并给 `GET /jobs` 加 `?type=` 筛选。

本卡结束时，用 `{"name":"每晚对账","type":"payment_check"}` 能建任务；
只给 `{"name":"payment_check"}` 的旧客户端行为与改动前逐字一致。

## 2. 背景与当前问题

`CreateJobRequest`（`api/dto.go:11-34`）只有一个 `Name`，`createJobFromRequest`
（`api/handlers.go:74-144`）用它做三件事：查注册表（`:82`）、当错误文案的键名（`:87`）、
当任务名（`:115`）。而核心层的 `Job.Type`（`core/job.go:144`）与 `HandlerKey()`
（`core/job.go:203-208`、快照那份 `:265-270`）从设计之初就是"Type 优先、回退 Name"，
并且下面这些位置已经在按这条规则做事：

| 已按 HandlerKey 的位置 | 作用 |
| --- | --- |
| `core/scheduler.go:430` | 入堆时按注册键选执行池 |
| `core/scheduler.go:607` | 恢复时按注册键重绑处理函数 |
| `core/scheduler.go:992` | `PauseByHandlerKey` 按注册键匹配（`core/scheduler.go:981`） |
| `core/scheduler.go:1590` | 执行前按注册键补绑，查不到判 failed |
| `core/scheduler.go:1820` | 定时任务重排时复制 Type |
| `api/handlers_executors.go:75` | 结果读取门槛已经用 `snapshot.HandlerKey()` |
| `cmd/server/main.go:945` | 崩溃恢复守卫按注册键判类别 |

所以本卡的主体是"补上 API 层这一个缺口"，而不是改造核心。

## 3. 要实现的功能

### 3.1 请求与响应

```go
// api/dto.go CreateJobRequest 新增：
// Type 决定这条任务跑什么——调度器注册表里的键（普通处理函数名或档位注册键 exec.<name>）。
// 为空时按旧写法：Name 兼作这个键，且 Name 不套标签规则（设计文档 §D4）。
Type string `json:"type,omitempty" example:"exec.php"`
```

- `JobResponse`（`api/dto.go:69`）加 `Type string \`json:"type,omitempty"\``。
  为空表示旧写法建的任务，前端显示时回落到 `Name`。
- `UpdateJobRequest`（`api/dto.go:37`）加 `Type *string`：**只用于发现误用**，
  与那份结构里 `Name *string` 的既有注释同一体例（`:41-44`）。
- `BatchItemError` 的示例文案（`api/dto.go:61-66`）里那条"job type 'nope' not registered"
  仍成立，不必改。

### 3.2 创建流程（`api/handlers.go:74-144`）

按顺序，一步都不能少：

1. 算出本次的**注册键**：`key := strings.TrimSpace(req.Type)`；`key == ""` 时 `key = req.Name`
   （D4 旧写法）。这一条判据在本卡只出现一次，抽成一个函数
   （建议 `func (req CreateJobRequest) handlerKey() string`），批量接口共用。
2. `key` 为空（两个字段都没给有效值）→ 400，文案 `job type is required`。
   注意 `binding:"required"` 现在只在 `Name` 上（`api/dto.go:13`），保持不动，
   空 `type` + 空 `name` 的组合走这里。
3. 带 `type` 时校验名称：`core.ValidateJobName(req.Name)` → 不合法 400，
   `Details` 用原错误文本（N01 的规则只在这里生效）。
4. 查注册表：`s.scheduler.LookupHandler(key)`（现在是 `:82` 的 `LookupHandler(req.Name)`）。
   失败文案保留既有句式，但把被查的键写清楚：
   `job type '%s' not registered`，参数是 `key`。
5. 建任务：`Name: req.Name`、`Type: req.Type`（旧写法下 Type 保持空串，
   让 `HandlerKey()` 的回退规则去做事，不在两处各写一遍规则）。
6. 门禁：`gateExecutorSubmission` 内部改按 `job.HandlerKey()` 取档位（§3.4），
   位置仍在 `scheduler.Schedule` 之前（`api/handlers_executors.go:594` 注释钉的这条不许动）。
7. 绑处理函数用第 4 步查到的那个（`:136` 照旧）。

**分组校验（`:91`）与重试/超时解析（`:96-111`）不改。**

### 3.3 更新与查询

| 位置 | 改动 |
| --- | --- |
| `api/handlers.go:334` | 改名禁令扩展到类型：`req.Name` 不同 → 400（现有）；`req.Type` 非 nil 且与 `snapshot.HandlerKey()` 不同 → 400，文案说明"换类型等于换执行体，请新建任务"（§D14）。传了相同值按没传处理 |
| `api/handlers.go:149`、`:185` 一带 | 新增 `?type=`，比 `snap.HandlerKey()`（不是比 `snap.Type`——旧写法任务的类型在 `Name` 里）；`?name=` 继续精确比 `snap.Name`，语义不变 |
| `api/handlers.go:628` `toJobResponse` | 带上 `Type: job.Type`；:635 的 `payloadForResponse` 与 :650 的 `execForResponse` 两个入参从 `job.Name` 换成 `job.HandlerKey()`（函数签名里的形参名与注释同步） |
| `api/handlers.go:435` `RetryJob` | 确认它按 `HandlerKey` 重绑（现状已如此），本卡不加代码、加一条用例 |
| `api/handlers.go` 其余 | `grep -n "\.Name" api/handlers.go` 逐条过一遍，把"用来查档位/判类型"的换成 `HandlerKey()`，"用来显示/筛选标签"的保留 |

### 3.4 门禁与档位侧（`api/handlers_executors.go`）

| 位置 | 改动 |
| --- | --- |
| `:599` | `s.executorProfile(job.Name)` → `s.executorProfile(job.HandlerKey())` |
| `:603` | `name := job.Name` → `key := job.HandlerKey()`，下游三处（`:609` Available、`:633` 台账、`:643` 超时）跟着换 |
| `:662` `payloadForResponse` | 形参名改为 `handlerKey`，注释按"这里查的是档位不是任务"重写；调用点 §3.3 已换 |
| `:565` `gateExecutorSubmissionRole` | 不改逻辑，只确认文案里的 `profile.HandlerKey()` 仍是注册键（已经是） |
| `:75` GetJobResult | 不改（已按 `snapshot.HandlerKey()`） |

**这是本卡最容易漏的一处，也是设计文档 §10 风险 1 的位置**：只改创建不改门禁，
新写法建的 adhoc/档位任务会因为"标签查不到档位"而绕过 payload 校验直接被执行。
DoD 第 3 条专门为它存在。

### 3.5 台账与日志

- `api/audit.go:181` 一带记的已经是 `profile.HandlerKey()`，不改。
- `stashAuditJobID`（`api/handlers.go:65`）记的是任务 ID，不改。
- 事件里的 `JobName`（`core/scheduler.go:1580`、`:1606`）继续传 `job.Name`，
  这是标签语义（§D13），本卡不改。

## 4. 实现步骤

1. 先加两条**红色回归用例**（写在 `api/api_test.go`）：
   `TestCreateJobWithFreeNameAndType`、`TestCreateJobLegacyNameOnly`，
   再加一条门禁用例 `TestExecutorGateUsesHandlerKeyNotName`（`api/handlers_executors_test.go`）。
2. 按 §3.1 改 DTO；按 §3.2 改创建流程；跑第 1 步用例，前两条转绿、第三条仍红。
3. 按 §3.4 改门禁与两个响应 helper，第三条转绿。
4. 按 §3.3 改更新判定与 `?type=`，补对应用例。
5. 全仓 `go build ./... && go vet ./... && go test ./api ./core`。
6. 只对新改动的文件 `gofmt -w`。
7. `docs/api.md` 的创建/更新两节按新字段与小改（其余章节留给 N08）。

## 5. 测试要求

| 用例 | 断言 |
| --- | --- |
| 新写法建普通任务 | `name:"每晚对账"` + `type:"payment_check"` → 201；响应 `Name`/`Type` 各自正确；堆里 1 条；执行时绑上的是 `payment_check` 的处理函数 |
| 新写法名称非法 | `name:"a b"` 或含 `_` 或空 → 400，`Details` 来自 `ValidateJobName` |
| 类型未注册 | `type:"nope"` → 400，`Details` 含 `nope`（照 `api/api_test.go:84` `TestCreateJobUnknownType` 体例） |
| 旧写法完全不变 | 只带 `name:"payment_check"` → 201、`Type` 为空、`HandlerKey()` 等于 name；照 `api/api_test.go:56` `TestCreateJob` 断言，且**该用例一字不改地跑绿** |
| 旧写法的名称规则不套 | `name:"payment_check"`（含下划线）带空 type → 201，证明 D4 落地 |
| 门禁按注册键 | 一条档位任务写成 `name:"我的脚本任务"`、`type:"exec.<profile>"`，payload 非法 → 400（不是 201）；payload 合法 → 201 |
| 门禁负例 | 若把 §3.4 的改动打回 `job.Name`，上面这条必须红（变异验证，逐条留原始输出） |
| `?type=` 筛选 | 旧写法与新写法各建若干条，`?type=payment_check` 命中两种写法（因为比的都是 `HandlerKey()`）；`?name=` 只命中标签 |
| PUT 换类型 | `type` 传不同值 → 400；传相同值 → 按没传处理（200） |
| 批量 | 新写法与旧写法混在一个批次里，逐条结论按原始下标回报（照 `api/handlers.go:536` `BatchCreateJobs` 的 207 体例） |
| 响应字段 | `GET /jobs/:id` 与列表都带 `type`；adhoc 相关字段本卡不涉及（N06） |

## 6. 完成标准（DoD）

1. `grep -n "req.Name" api/handlers.go` 的每条剩余命中都是"标签用途"（显示/校验/筛选），
   没有一处用于查注册表或取档位。
2. `api/handlers_executors.go` 里取档位、探测、超时、台账四处都用 `job.HandlerKey()`
   或局部 `key` 变量，且 `grep -n "job.Name" api/handlers_executors.go` 只剩显示用途。
3. 三条新用例（新写法、旧写法、门禁）全绿，其中门禁那条做过变异反向验证并留原始输出。
4. `api/api_test.go:56` 与 `:84` 两条既有用例未修改且跑绿（兼容证据）。
5. 只带 `type` 不带 `name` 时不会静默建出空标签任务（必填判据落在一处，有用例）。
6. `go test -race -timeout 30m ./api ./core` 绿；`go build ./...`、`go vet ./...` 无输出。

## 7. 验收方式

```bash
go test ./api -run "TestCreateJob|TestExecutorGate|TestUpdateJob|TestListJobs" -v
go test ./api ./core -race -timeout 30m
go build ./... && go vet ./...
```

预期：命名到的用例全部 PASS，既有的 `TestCreateJob`/`TestCreateJobUnknownType` 未被改动。

再手工看一份响应（可选，用 `go test -run xxx -v` 打印或直接 curl）：
新写法任务的响应里 `name` 是中文标签、`type` 是注册键、`exec` 键只在档位任务出现。

## 8. 不在本任务范围

- 不加 adhoc 档位、不动 `executor` 包（N03-N05）。
- 不给 `GET /executors` 加元数据（N06）。
- 不改前端（N07）。
- 不放开任务改名（§D14、README S-1）。
- 不动 `core/load.go` 的加载器语义（README S-2）。
- 不加观测层按类型查询（README S-3）。

## 9. 风险与回滚

| 风险 | 说明 | 退路 |
| --- | --- | --- |
| 门禁漏改 | adhoc/档位任务的 payload 绕过校验被执行一次 | DoD 第 2、3 条 + 变异验证；发现即回到 §3.4 逐处补 |
| `Type` 写成 trim 后的值 | 快照里出现"显示与查表不一致"的两个名字 | 存原文，只在算键时 trim（与 `core/job.go:19-20` 注释的口径一致） |
| `?type=` 与 `?name=` 混淆 | 使用者按类型筛却拿到标签匹配 | 用例断言"两种写法都被 `?type=` 命中" |
| 老客户端把响应整体 PUT 回来 | 带上 `type` 与旧值相同 → 按没传处理；不同 → 400 | 既有 `Name` 同一条先例（`api/dto.go:41-44`），文案照它写 |

回滚：本卡不动核心与存储，退回方式是还原 `api/` 三个文件；
旧写法路径全程未变，因此回滚不影响已建任务。

## 10. 实现记录（执行时补写）

| # | 与卡片的偏离 | 原因 |
| --- | --- | --- |
