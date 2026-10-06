# TASK-N06　api：档位元数据、启动告警与门禁复核

- 所属阶段：M2 接口面
- 依赖任务：TASK-N02、TASK-N05
- 涉及文件：`api/handlers_executors.go`、`api/executors_response_test.go`、
  `api/profile_form_contract_test.go`、`api/handlers_executors_test.go`、`api/audit_test.go`、
  `executor/register.go`（告警）、`docs/api.md`（`/executors` 一节）
- 预计规模：中

## 1. 任务目标

让接口说清三件事：哪几条类型是自由执行档位、它的位置输入框该叫什么键、填什么形态；
把 §D1、§D3 两条边界在**后端**判死（前端也判，但服务端为准）；
并交付打开 adhoc 时的启动告警。

## 2. 背景与当前问题

`GET /api/v1/executors`（`api/handlers_executors.go:389` `ListExecutors`，条目由 `:420`
`toExecutorProfile` 组装）现在给的是档位的处境：`key`、`kind`、`runtime_ok`、`reason`、
`args`、`env_allow`、`source`、`editable` 等（前端形状见 `web/src/api/types.ts:91-131`）。
内置 adhoc 档位没有 `args`、没有 `script`/`url_template`，前端拿到这样一条空条目
无法判断"该给它一个位置输入框还是参数表单"。

同时，`gateExecutorSubmission`（`api/handlers_executors.go:596`）在 N02 之后按 `HandlerKey`
取档位，`ValidateSubmission`（`executor/args.go:96`）在 N05 之后负责位置校验，
本卡要把这条链在接口层验一遍：位置非法必须 400、身份不够必须 403、开关关时这四条键根本不存在。

## 3. 要实现的功能

### 3.1 响应新增两项

```go
// api/handlers_executors.go:308 的 ExecutorProfileResponse 结构
Adhoc    bool             `json:"adhoc"`              // 只有内置自由执行档位为 true
Location *ProfileLocation `json:"location,omitempty"` // 只有 adhoc 档位有
```

```go
// ProfileLocation 是"执行位置由任务给出"这一项的输入说明。
type ProfileLocation struct {
    Key      string `json:"key"`      // payload 顶层键：script | url
    Kind     string `json:"kind"`     // path | url
    Label    string `json:"label"`    // "脚本路径（.php）" / "请求 URL"
    Required bool   `json:"required"` // 内置四条恒为 true
    Hint     string `json:"hint"`     // 取值范围说明，见 §3.2
}
```

`Location` 的内容来自 `Profile.Adhoc`（`executor/profile.go:102` 一带）与
`core.ExecutorsConfig.Adhoc`，**不从 payload 或任务反推**。

### 3.2 `Hint` 的生成规则（三条，逐条可测）

| 情形 | Hint |
| --- | |
| 脚本类且 `path_prefixes` 非空 | 列出允许的前缀（相对 workspace 的按 workspace 展开后写绝对形式），并写明扩展名要求 |
| 脚本类且 `path_prefixes` 为空 | "任意本机路径"，并附一句"该部署未做路径范围限制" |
| http 且 `url_hosts` 非空 | 列出主机；并说明"回环与私网地址仍被拒绝"（除非 `url_allow_private=true`，那种情形改成"地址范围守卫已关闭"） |
| http 且 `url_hosts` 为空 | "任意主机"，仍附"回环与私网地址被拒绝"或"守卫已关闭"的那一句 |

`Hint` 是给人看的中文说明，前端直接显示、不再拼句子（口径与
`web/src/components/jobs/JobForm.vue:444` 的 `reason` 一致：后端给结论，前端只呈现）。

### 3.3 契约与兼容

- 普通档位：`adhoc` 为 `false`（或非零值默认省略，**取一种并在测试里钉住**），`location` 键不出现。
  前端因此可以用 `location` 存在与否作为唯一判据，不必自己认键名。
- 执行器关闭（`enabled=false`）时 `ListExecutors` 的整体形状不变
  （`api/handlers_executors.go:390-398` 那条默认响应）。
- `GET /api/v1/job-types` 形状不动（§D12），本卡只在 `docs/api.md` 里补一句
  "自由执行档位会出现在这里，位置由 payload 给"。

### 3.4 启动告警

在 `executor/register.go` 的注册流程里（`warnRelaxedAddressPolicy`，`:105` 旁边）加两条：

1. `adhoc.enabled=true` → `Warn`：一句话说清"任何够 `executors.required_role` 的身份
   都可以让这台机器执行它提交的路径 / 请求它提交的地址"，并带上当前的
   `path_prefixes`、`url_hosts`、`url_allow_private`、`required_role` 四个取值。
2. `url_allow_private=true` 或 `path_prefixes` 为空 → 各自的第二条 `Warn`，
   说明关掉了哪一道守卫。

日志文本不进接口响应；`/executors` 的 `Hint` 说的是范围，`Warn` 说的是风险，两条都要有。

### 3.5 门禁与身份复核（不改逻辑，只补证据）

| 点 | 位置 | 本卡要做 |
| --- | --- | --- |
| 提交身份 | `gateExecutorSubmissionRole`（`api/handlers_executors.go:565`） | 一条用例：低档位身份提交 `exec.php` → 403，且不因"名称是中文标签"而绕过（N02 的解耦证据） |
| 可用性 | `:609` `Available` | 一条用例：解释器不在 PATH 时提交 `exec.php` → 400 且文案是"这台机器跑不了"而不是"类型未注册" |
| payload | `:630` `ValidateSubmission` | 用例覆盖"位置缺失/非法 → 400 且任务没进堆" |
| 结果读取 | `resultGuard`（`api/handlers_executors.go:673`） | 一条用例证明 adhoc 任务的结果仍按 reader 起步（它没有 secret 参数） |
| 台账 | `api/audit.go:181` 一带 | 一条用例证明 adhoc 建任务落进既有动作词、执行器列记的是 `exec.php` 这类注册键；**不新增动作词**，但要用例证明漏配时会落 `other`（`api/audit.go:260-270` 的既有口径） |

## 4. 实现步骤

1. 先在 `api/executors_response_test.go` / `api/profile_form_contract_test.go` 写目标形状的用例：
   四条内置条目带 `adhoc` + `location`、普通条目不带。
2. `toExecutorProfile`（`api/handlers_executors.go:420`）里补两字段与 `Hint` 生成
   （`Hint` 需要的配置值从 `s.executors` 那条视图取，若视图缺项就给 `Registry` 补一个读方法，
   不要在 api 侧重新读一遍 yaml）。
3. `executor/register.go` 的两条 `Warn`。
4. §3.5 的五条证据用例。
5. `docs/api.md` 的 `/executors` 一节补 `adhoc`/`location` 与示例响应。
6. `gofmt -w`；`go test ./api ./executor`；`go build ./... && go vet ./...`。

## 5. 测试要求

| 用例 | 断言 |
| --- | --- |
| 四条内置条目的元数据 | `location.key` 分别是 `script`×3 与 `url`；`kind` 是 `path`×3 与 `url`；`required=true`；`adhoc=true` |
| 普通条目 | 响应里**没有** `location` 键；`adhoc` 取值按 §3.3 钉住 |
| 执行器关闭 | 整体形状与关闭前一致（`enabled:false`、`profiles:[]`） |
| Hint 四种组合 | 前缀有/无 × 主机有/无，各断言 Hint 的关键片段（不断言整句原文） |
| 403 | operator 身份（默认 `required_role=admin`）提交 `exec.php` → 403 |
| 400 可用性与 payload | 两条分别命中"这台机器跑不了"与"位置非法"，且堆里任务数不变 |
| 结果门槛 | reader 身份能读 adhoc 任务的输出（无 secret 参数因此不升档） |
| 台账 | 一条成功 + 一条被拒的 adhoc 提交各查到台账行，执行器列是注册键 |
| 告警 | `adhoc.enabled=true` 时启动日志里有那一条 `Warn` 且带上四个取值（用 `slog` 测试替身断言，而不是 grep 控制台） |
| 变异反向验证 | 把 §3.5 的"payload 400"用例打回 N02 前的写法（按 name 取档位）→ 必须红；逐条留原始输出 |

## 6. 完成标准（DoD）

1. 前端只靠 `location` 是否存在就能决定给不给位置输入框，不需要自己认 `exec.php` 这类键名。
2. `Hint` 的四条规则有用例，且 `path_prefixes`/`url_hosts` 改动后 Hint 跟着变（读的是生效配置）。
3. 两条启动 `Warn` 有测试证据；`url_allow_private=true` 时那一条单独出现。
4. §3.5 五条证据用例齐；台账未新增动作词且有用例说明该动作词落在既有那一行。
5. 执行器或 adhoc 关闭时接口形状零变化（既有用例未修改且全绿）。
6. `go test ./api ./executor -race -timeout 30m` 绿；`go build ./...`、`go vet ./...` 无输出。

## 7. 验收方式

```bash
go test ./api -run "Executors|ProfileForm|Adhoc" -v
go test ./api ./executor -race -timeout 30m
go build ./... && go vet ./...
```

手工（临时目录）：打开 `executors.enabled` + `executors.adhoc.enabled`，
`curl -H "X-Auth-Token: …" http://127.0.0.1:<端口>/api/v1/executors`，
预期四条 `adhoc:true` 条目各带 `location`，且启动日志里有一条 adhoc 的 `Warn`。

## 8. 不在本任务范围

- 不改前端（N07）。
- 不改校验规则本身（N05 已定；本卡只把它呈现出来）。
- 不做按类型细粒度授权（README S-5）。
- 不改 `GET /job-types` 形状（§D12）。
- 不加"绝对路径对 viewer 遮蔽"那类读侧收敛（web-profile 系列 S-3 是同一件事，仍登记不修）。

## 9. 风险与回滚

| 风险 | 说明 | 退路 |
| --- | --- | --- |
| `Hint` 在 api 侧复制了一份规则 | 与 `executor` 的判据分叉，界面说的和服务端拒的不是同一件事 | §4 第 2 步：只呈现 `Registry`/配置里的事实，范围判断留在 N05 |
| 前端改成自己猜键名 | 键名一换界面就坏 | DoD 第 1 条明确以 `location` 为唯一判据 |
| 告警文本泄露内部路径 | `path_prefixes` 是配置内容，不是凭据，但它会进日志 | 与既有 `warnRelaxedAddressPolicy` 同一条口径：只写范围不写文件内容；记录在 §10 |

回滚：新增字段是纯增量，去掉 `Location`/`Adhoc` 两字段与两条 `Warn` 即恢复原响应；
既有字段语义未变，前端可留兼容代码。

## 10. 实现记录（执行时补写）

| # | 与卡片的偏离 | 原因 |
| --- | --- | --- |
