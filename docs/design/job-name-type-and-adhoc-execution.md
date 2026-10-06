# 任务名称自由填写 + 任务类型（档位）+ 自由指定执行位置设计

> 状态：**待实施**。任务拆分见 `docs/design/tasks/new-tasks/README.md`（TASK-N01…N08）。
> 需求来源：使用者 2026-10-06 提出，原文要点四条——
> ①"任务名称"现在等于"已注册的任务类型"，要改成可以任意填写；
> ②新建任务页要有"任务类型"下拉，列出 PHP、Python、脚本 shell/bash、HTTP、其它当前可执行的类型；
> ③类型下拉下面再加一个输入框，填 PHP/Python/脚本的具体位置，或 HTTP 的 URL；
> ④选定 C2 方案：允许按任务自由填路径/URL，主动放宽安全边界。
> 补充拍板三条：档位就是任务类型；名称与类型的判断"前端判、后端最后判"两道都要有；
> 任务名称只允许中文、字母、数字三种字符；列表页同时显示名称与类型。
> 引用约定：正文只写 `文件`、`文件:行号` 与既有文档的 `§`，不写"上面那段"这类无法定位的指代。
> 冲突处理：本文与 `executor-design.md`、`web-profile-design.md` 在执行侧语义（payload 校验框架、
> argv 直传不过 shell、超时上限、产物与恢复、档位查重）上保持一致，只有"可执行体身份能不能
> 在提交任务时决定"这一条被本文改写，逐条见 §8。

## 1. 背景与目标

现在的实际情况是：**核心层早就把"叫什么"和"跑什么"分成了两个字段，只有 HTTP 接口和控制台
一直用同一个值填两个字段。**

- `core.Job.Name`（`core/job.go:143`）与 `core.Job.Type`（`core/job.go:144`，注释写着
  "Handler 注册键；为空时回退 Name"）是两个字段。
- 查处理函数统一走 `Job.HandlerKey()`（`core/job.go:203-208`）与 `JobSnapshot.HandlerKey()`
  （`core/job.go:265-270`）：Type 优先，Type 为空才回退 Name。
- 这条规则已经贯穿入堆选池（`core/scheduler.go:430`）、执行前重绑（`core/scheduler.go:1590`）、
  恢复重绑（`core/scheduler.go:607`）、定时任务重排（`core/scheduler.go:1820`）、
  重试复制（`core/job.go:219`）、按类型批量置暂停（`core/scheduler.go:981` `PauseByHandlerKey`）、
  崩溃恢复守卫（`cmd/server/main.go:945`）。持久化快照也带 Type（`core/job.go:240`）。
- 唯一的断点在 API 层：`CreateJobRequest`（`api/dto.go:11-34`）只有 `Name` 一个字段，
  `createJobFromRequest`（`api/handlers.go:82`）拿 `req.Name` 查注册表，随后又把同一个值写进
  `job.Name`（`api/handlers.go:115`）。

所以①②两半需求的主体是"把已有字段接起来"，不需要动核心调度与存储的语义。

③④两半需求是另一回事：现在的可执行体身份全部在任务之外定死——脚本路径写进档位定义
（`executors.commands[].script` / `program`，`core/config.go:303-305`），HTTP 目标写进
`url_template`（`core/config.go:334`），而且明确禁止在编辑时更换（`web-profile-design.md:38` D7：
"这两项是'哪一个可执行体'的身份，换掉等于新建一条档位"）。本文按使用者的明确选择放宽这一条：
**新增一组内置的"自由执行档位"，让路径与 URL 在提交任务时给出**，同时保留其余所有守卫
（不过 shell、超时上限、产物存储、地址范围守卫、身份档位）。

本设计交付四件事：

1. 任务名称变成给人看的标签：可以中文、字母、数字，最长 64 个字符，其它字符拒绝。
2. 任务类型成为新建任务时的显式选择项，选项就是注册表里的键；档位即任务类型，不新造概念。
3. 新增"自由执行档位"（下称 adhoc）：PHP、Python、Shell、HTTP 四条内置类型，
   执行位置由任务自己给；整套能力由一个新配置开关控制，默认关闭。
4. 列表与详情同时显示名称与类型，并支持按类型筛选。

不在本设计范围内：脚本文件本身的上传与编辑、按类型分权、任务改名的能力、
目录任务加载器（`core/load.go`）的名称/类型拆分。这四项在 §10 逐条登记。

## 2. 决策摘要

标 ★ 的是使用者已经拍板的四条；其余是按"不新造执行通路、不复制校验规则"推出来的结果。

| # | 决策 | 取舍与理由 |
| --- | --- | --- |
| D1 ★ | **档位就是任务类型**：类型下拉的选项直接取调度器注册表的键（代码注册的普通处理函数 + 全部档位），不引入"类型分类"这层新东西 | PHP/Python/Shell/HTTP 在现有模型里不是一个"类型"，而是档位的 `kind` 与 `runtime` 两个属性（`executor/profile.go:36-38`、`core/config.go:300-301`）。把它们再包一层分类，就会出现"下拉里的类型"和"注册表里的键"两套名字，而派发只认后者 |
| D2 ★ | **名称与执行体解耦落到 `core.Job.Type`**：新建任务时名称给人看，类型决定跑什么 | 这是启用一个已经存在、并且全链路都已支持的字段（见 §1 第一组行号）。替代方案是给 `Job` 再加一个 `DisplayName`，那要让上面七个读取点全部改一遍，还要处理老数据没有这个字段的问题 |
| D3 ★ | **名称规则：中文、字母、数字，长度 1..64 个字符，其它一律拒绝**（含空格、下划线、连字符、点、斜杠、标点） | 按使用者给的三条字符集直接落地。长度取 64 与档位名规则对齐（`core/executor_profile_store.go:22` `^[A-Za-z0-9_-]{1,64}$`），计数按字符而不是按字节，否则中文名三个字符就算超长。校验函数放 `core`，API 与前端各调一次同一条规则，**服务端为准**（使用者明确要求两道都有） |
| D4 | **两种写法并存，判据唯一：请求体有没有带 `type`** | 带 `type` → 新写法：`name` 是标签，套 D3 规则，查表用 `type`。不带 `type` → 旧写法原样保留：`name` 兼作注册键，**不套** D3 规则（`payment_check` 带下划线、`exec.demo-run` 带连字符，套上就会把既有调用方全部拒掉）。这样既满足新表单，也不破坏 `POST /jobs/batch`、目录加载器与已在跑的脚本 |
| D5 ★ | **C2 的落地形态：新增四条内置 adhoc 档位，而不是新增一条执行通路** | 现成一整套能力都挂在档位对象上：并发许可、超时合成与上限、产物落盘、失败分类与永久失败、重试、崩溃恢复、输出掩码、探测与"这台机器跑不跑得动"的说明（`executor/proc.go:65` `NewRunner`、`executor/http.go:62` `NewHTTPRunner`、`executor/register.go:87`）。新写一条通路要么把这整套复制一遍，要么全都缺 |
| D6 | **adhoc 档位键名：`exec.php`、`exec.python`、`exec.shell`、`exec.http`** | 名字要能直接当界面标签的锚点，也必须是合法档位名（`exec.shell` 的解释器由配置项指定，默认 `bash`）。注意不能用 `exec.adhoc.php` 这种带点的写法——点不在档位名字符集里（`core/executor_profile_store.go:22`） |
| D7 | **执行位置走 payload 顶层的 `script` / `url` 两个新键，不走 `args`/`params`** | 两条理由。①技术：HTTP 的整条 URL 放进 `params` 必然被拒，占位符值禁止 `:` `/`（`executor/args.go:406-434`，禁字符集 `urlParamForbidden`）。②语义：位置是"跑哪个可执行体"的身份，不是参数值；D7（`web-profile-design.md:38`）正是按这条区分身份与参数。放进 `args` 会让"参数"里混进一个决定执行体的键 |
| D8 | **路径解析复用 `PathAnywhere`，文件存在性在提交期判一次、执行前由探测再判一次** | `PathAnywhere` 与 `resolveAnywhere`（`executor/profile.go:55`、`:959`）在 TASK-W02 已经实现并只给 Web 侧档位用过，语义正好是"允许本机任意路径，只要求能算出绝对路径"。再判一次是因为提交与执行之间隔着一段时间，文件可能已经不在了；现成的 `fileCheckReason`（`executor/probe.go:115`）就是这一条 |
| D9 | **URL 仍保留三层守卫，放宽的只有"主机可以在提交时决定"** | ①scheme 只允许 http/https、URL 里不许带凭据（`executor/http.go:267` `checkTarget`）；②主机白名单（`hostAllowed`，`executor/profile.go:885`）由配置项给，空列表按"不限主机"处理，但这条放宽会让 `checkAllowedHosts`（`:744`）现存的"不允许为空"规则对 adhoc 档位绕开，需要显式分支而不是改坏既有档位；③地址范围守卫**不放宽**：拒回环、私网、链路本地（含云主机元数据地址 169.254.169.254）、组播与 100.64.0.0/10（`executor/http.go:744` `refusalReason`），只有配置显式打开 `url_allow_private` 才整条关闭 |
| D10 | **adhoc 的提交身份门槛复用 `executors.required_role`（默认 admin），不新增角色键** | 阶梯已经存在（`configs/config.example.yaml:80-83`）。再加一个键会出现"两个门槛取哪个"的问题。要放宽的是"执行位置从哪来"这一件事，不是"谁能提交" |
| D11 | **整套 adhoc 由 `executors.adhoc.enabled` 控制，默认 false；打开要求 `executors.enabled=true`** | 与 `executors.web_enabled` 同一条口径，那条"两个开关必须一起"的规则现在落在 `core/config.go:477`（`web-profile-design.md` 的落地位置段写的 `:459` 已经漂移）。关闭时四条内置档位一条都不注册，接口与页面看不出任何变化 |
| D12 | **`GET /api/v1/job-types` 形状不动，新增信息全部放 `GET /executors`** | job-types 的响应形状已被 `docs/api.md`、swagger 注释与既有用例钉住（`api/handlers.go:528-532`），而前端本来就同时读这两份（`web/src/components/jobs/JobForm.vue:81-92`）。要加的只有"这条档位是不是 adhoc、位置输入框该叫什么、填什么形态"三项，属于档位属性，放档位列表里最自然 |
| D13 | **事件与观测层不改表**：`Event.JobName` 继续记名称（标签），任务身份由 `JobID` 定位；执行体身份在接口响应与快照里给 | SQLite 事件表只有 `job_name` 一列（`store/sqlite/schema.go:35`），加列要走迁移而收益只有一条"看事件时不用回查任务"。台账侧记的已经是 `HandlerKey`（`api/audit.go:181`），不受影响 |
| D14 | **`PUT /jobs/:id` 本期不放开改名**，并把 `type` 纳入同一条"传了且不同就 400"的判定 | 改名要连带决定"改名后按名称筛选的历史任务怎么找"、"事件里的旧名字怎么办"，属于另一件事。已登记为 §10 的后续项。换类型的禁令必须扩展：现在只比 `name`（`api/handlers.go:334`），解耦后 `type` 才是执行体身份，只守 `name` 等于放开了一条换执行体的路 |
| D15 | **内置 adhoc 档位与用户自建档位撞名时，内置让位并记一行启动日志** | 撞名查重已有先例（`executor/register.go:72-77` 键被占用即启动失败；`executor/merge_profiles.go:39` 的降级条目）。这里是内置条目让位而不是拒绝启动，因为用户已经建好的档位不该因为运维打开了 adhoc 开关就失效 |

## 3. 现状基线盘点（本文所有改动点的坐标）

| 主题 | 位置 | 现状 |
| --- | --- | --- |
| 名称与类型两个字段 | `core/job.go:143-144` | Type 已存在，注释即"Handler 注册键；为空时回退 Name" |
| 查表键的规则 | `core/job.go:203-208`、`core/job.go:265-270` | Type 优先回退 Name，两份实现只差接收者类型 |
| 创建请求只有一个字段 | `api/dto.go:11-34` | 只有 `Name`，`binding:"required"` |
| 创建流程 | `api/handlers.go:74-144` | :82 按 `req.Name` 查表；:115 写 `Name`；:131 调 adhoc/档位提交期门禁 |
| 档位门禁 | `api/handlers_executors.go:596` `gateExecutorSubmission` | :599 按 `job.Name` 取档位；:605 身份档位；:609 可用性；:620 超时上限；:630 payload 校验 |
| 响应组装 | `api/handlers.go:628` `toJobResponse` | :635 `payloadForResponse(job.Name, …)`、:650 `execForResponse(job.Name, …)`（两个函数在 `api/handlers_executors.go:662`、`api/handlers.go:674`） |
| 更新流程 | `api/handlers.go:278` `UpdateJob` | :334 只比 `req.Name` 与快照 Name 是否相同 |
| 列表筛选 | `api/handlers.go:149`、`:185` | `?name=` 精确比 `snap.Name` |
| 类型列表端点 | `api/server.go:351`、`api/handlers.go:528-532` | 返回 `{"types": […]}`，内容 = `Scheduler.HandlerNames()`（`core/scheduler.go:387`） |
| 档位列表端点 | `api/server.go:303`、`api/handlers_executors.go:389` `ListExecutors` | 响应条目由 `toExecutorProfile`（`:420`）组装 |
| 执行方式只有三种 | `executor/profile.go:36-38` | `script` / `binary` / `http` |
| 解释器白名单 | `core/config.go:300-301`、`configs/config.example.yaml:88` | `runtime_allow: [bash, sh, cmd, pwsh, node, php, python, java]` |
| 路径解析两种模式 | `executor/profile.go:44-56`、`:945`、`:959`、`:993` | `PathWithinWorkspace`（`:51`）/ `PathAnywhere`（`:55`）；`resolveProfilePath` 按模式分流 |
| HTTP 目标校验 | `executor/profile.go:744`、`:817`、`:885` | `allowed_hosts` 不许为空、模板与主机白名单、执行期 `hostAllowed` |
| 地址范围守卫 | `executor/http.go:744` `refusalReason`、`:705` `allowedIP` | 回环/私网/链路本地/组播/100.64.0.0/10 一律拒 |
| payload 顶层键集合 | `executor/args.go:71-73`、`:96` `ValidateSubmission` | 只认 `args`/`env`/`params`/`headers`/`body`/`timeout` |
| argv 组装 | `executor/args.go:531` `Render` | script 档位的 argv 前两项是 `p.Runtime, p.ScriptPath`（`:541`） |
| URL 组装 | `executor/http.go:230` `renderURL`、`:251` `resolveTarget`、`:267` `checkTarget`、`:290` `renderRaw` | 模板来自档位 `URLTemplate` |
| 档位注册 | `executor/register.go:52` `Register`、`:139` `Handler`、`:153` `handlerFor` | 键冲突先全量检查再写入；按 kind 分流 |
| 内置处理函数 | `cmd/server/main.go:895-913` `registerHandlers` | 四个示例 + 全部档位 |
| 配置结构 | `core/config.go:220` `ExecutorsConfig`、`:294` `ExecutorCommand` | adhoc 新节要挂在 `ExecutorsConfig` 下面 |
| 热重载分类表 | `core/config_reload.go:60-96`（重启/热更表）、`:112-146`（身份字段）、`:154`（档位内热更字段） | 新键必须显式归类，否则 `TestEveryLeafKeyIsClassed` 与叶子覆盖用例（`core/config_reload_test.go:101`）会红 |
| 名称规则的既有先例 | `core/executor_profile_store.go:22`、`:240` `ValidateProfileName`；分组名 `core.ValidateGroupName`（`api/handlers.go:26-34` 调用） | 每个"会进 URL 与列表的字符串"都在 core 定一条规则，API 与前端各调一遍 |
| 前端表单 | `web/src/components/jobs/JobForm.vue:553-566`（下拉，标签文案"任务名称（= 已注册的任务类型）"）、`:115-120`（按 `exec.` 前缀判档位）、`:431-449`（选项构造）、`:411`、`:413`、`:511` | 名称只有一个下拉；提交体只带 `name` |
| 前端类型与接口封装 | `web/src/api/types.ts:292-303`（`CreateJobRequest`）、`:91-131`（`ExecutorProfile`）、`web/src/api/jobs.ts:32`、`web/src/api/executors.ts` | 名称/类型解耦要同时改这三处 |
| 列表与详情 | `web/src/views/JobsView.vue:315`、`:426`；`web/src/views/JobDetailView.vue:136`、`:160`；`web/src/components/jobs/JobTable.vue`、`JobFilterBar.vue:84` | 筛选下拉的候选值现在直接来自 jobTypes |

## 4. 目标形态（界面与接口）

### 4.1 新建任务表单

三个字段的顺序与联动：

| 顺序 | 字段 | 控件 | 规则 |
| --- | --- | --- | --- |
| 1 | 任务名称 | 文本输入 | 必填；中文/字母/数字，1..64 个字符；输入框下方一行固定说明"名称只是标签，真正跑什么看下面的任务类型" |
| 2 | 任务类型 | 下拉 | 必填（新写法）；候选来自 `GET /job-types` + `GET /executors` 两份现成数据，分三组显示：普通任务（代码注册的四个示例）、执行器档位（按 kind 与 runtime 标出 PHP/Python/Shell/Binary/HTTP）、其它 |
| 3 | 执行位置 | 文本输入（仅 adhoc 类型显示） | 标签随类型变：PHP 显示"脚本路径（.php）"、Python 显示"脚本路径（.py）"、Shell 显示"脚本路径（.sh/.bash）"、HTTP 显示"请求 URL"；必填，规则见 §5.4/§5.5；**输入框的合法性只作提示，拒绝权在服务端** |

选中非 adhoc 档位时，第 3 项不出现参数框之外的新东西，位置对它是只读展示：
脚本/产物用 `Profile.PathDisplay()`（`executor/profile.go:195`）与 `ProgramDisplay()`（`:172`），
HTTP 用档位模板（`web/src/api/types.ts:121` 已有 `url` 字段）。
选中普通任务时保持现状，给 JSON 的 payload 编辑器（`web/src/components/jobs/PayloadEditor.vue`）。

### 4.2 提交体（新写法）

```json
{
  "name": "每晚对账脚本",
  "type": "exec.php",
  "delay": "10m",
  "payload": { "script": "D:/work/scripts/reconcile.php" }
}
```

HTTP 那条：

```json
{
  "name": "订单服务健康检查",
  "type": "exec.http",
  "payload": { "url": "https://api.example.com/healthz" }
}
```

普通任务不变，`payload` 仍是各自的业务 JSON：

```json
{ "name": "白天巡检", "type": "payment_check", "payload": { "order_id": "123" } }
```

### 4.3 读取侧

`JobResponse` 加 `type`（`api/dto.go:69`），`GET /jobs` 支持 `?type=`，
列表与详情的名称旁边多一列/一行类型。

## 5. 后端实现方案

### 5.1 名称规则（TASK-N01）

新增在 `core/job_name.go`：

```go
var jobNamePattern = regexp.MustCompile(`^[\p{Han}A-Za-z0-9]{1,64}$`)

// ValidateJobName 校验任务名称（给人看的标签）。
func ValidateJobName(name string) error
```

三条实现要点：

1. 用 `\p{Han}` 而不是硬编码 Unicode 区间，覆盖常用汉字且不必维护区间表。
2. 长度用 `utf8.RuneCountInString`，不用 `len()`。
3. 错误文本给出可执行的改正方向（列出被拒的那个字符与允许的三类），因为它会直接进
   `ErrorResponse.Details`（先例见 `core/executor_profile_store.go:240-245`）。

调用位置：`api/handlers.go` 的 `createJobFromRequest`，仅在请求带 `type` 时套这条规则（D4）。
名称必填这一点由 `binding:"required"` 已有规则保证，不重复实现。

### 5.2 名称/类型解耦（TASK-N02）

| 改动 | 位置 | 内容 |
| --- | --- | --- |
| 请求加字段 | `api/dto.go:11` | `Type string` 加进 `CreateJobRequest`，注释写明"决定执行体的注册键；为空时按旧写法把 name 当键" |
| 查表改键 | `api/handlers.go:82` | `LookupHandler(typeOrName(req))`，其中 `typeOrName` 就是 D4 那条判据；错误文案里带上被查的那个键 |
| 写任务 | `api/handlers.go:114-127` | `job.Type = req.Type`（旧写法保持空串，让 `HandlerKey()` 的回退规则去做事，不在两处各写一遍规则） |
| 档位门禁改键 | `api/handlers_executors.go:599` | `s.executorProfile(job.HandlerKey())`，函数入参从 `job.Name` 换成一整套都按注册键走 |
| 响应组装改键 | `api/handlers.go:635`、`:650` | 两个 helper 的 `name` 参数换成注册键；`toJobResponse` 带上 `Type` |
| 更新判定扩展 | `api/handlers.go:334` | `req.Name` 的"传了且不同→400"照旧（本期不支持改名，D14），新增对 `req.Type` 的同一条判定 |
| 列表筛选 | `api/handlers.go:149`、`:185` 之后 | 新增 `?type=`，比 `snap.HandlerKey()`；`?name=` 继续比 `snap.Name`，语义不变 |
| 前端与文档 | 见 §6、§7 | — |

**顺序要求**：解耦必须与"后端按 `HandlerKey` 判档位"同卡落地。只改 API 的查表而不改
`gateExecutorSubmission`，会出现"按标签查不到档位、于是 adhoc 任务的 payload 一条都没校验
就进了堆"，而那一刻任务已经会被执行（这条顺序在 `api/handlers_executors.go:594` 的注释里
已经钉过：门禁必须在 `scheduler.Schedule` 之前）。

### 5.3 adhoc 档位的构造（TASK-N04）

新文件 `executor/adhoc.go`：

```go
// AdhocProfile 是一条内置自由执行档位。
func BuildAdhocProfiles(cfg core.Config, ec core.ExecutorsConfig, mode PathMode) ([]*Profile, error)
```

四条内置条目的定义（字段名对齐 `core.ExecutorCommand`，`core/config.go:294-334`）：

| 键 | kind | 位置来源 | 附加规则 |
| --- | --- | --- | --- |
| `exec.php` | script | payload 顶层 `script` | `Runtime=php`；扩展名默认要求 `.php` |
| `exec.python` | script | payload 顶层 `script` | `Runtime=python`；扩展名默认要求 `.py` |
| `exec.shell` | script | payload 顶层 `script` | `Runtime=executors.adhoc.shell_runtime`（默认 `bash`）；扩展名默认要求 `.sh`/`.bash` |
| `exec.http` | http | payload 顶层 `url` | `Method=POST`（可配）、`Body=json`（默认允许带 JSON 请求体，见 §12 P5）、`DenyPrivate` 按配置 |

要点：

1. 构造仍然走 `executor.BuildProfile`（`executor/profile.go:257`），不在别处复制一套字段检查。
   实现方式是先合成一份 `core.ExecutorCommand`，再交给既有构造函数——这样"页面上建一条档位"、
   "配置里写一条档位"、"内置 adhoc" 三种来源共用同一套规则（`web-profile-design.md` D3 的口径）。
2. 解释器必须仍在 `runtime_allow` 内（`core/config.go:234`）：不在就不注册这一条并记一行日志，
   而不是让启动失败。使用者把 `runtime_allow` 收成只有 `bash` 时，页面上就只有 Shell 与 HTTP 两条。
3. 位置字段在档位定义里是空的，因此要放宽 `checkFieldsMatchKind`（`executor/profile.go:501`）
   对 script 档位"必须有 script"、http 档位"必须有 url_template"的要求：新增一个
   `Profile.Adhoc bool`，让这两条判据在它上面走另一支。**不改判据本身**，
   否则用户自建档位可以少写脚本路径。
4. 注册路径：`executor.Register`（`executor/register.go:52`）之后追加一批内置条目，
   撞名按 D15 让位；`executor.Applier`（`executor/applier.go`）在档位表变更时不能把这四条冲掉，
   所以内置条目不进 `Registry` 的存储侧快照，改由一个独立的注册函数在启动与热生效两处调用。

### 5.4 脚本路径的提交期校验（TASK-N05）

在 `executor/args.go` `ValidateSubmission`（`:96`）的 `switch p.Kind` 之前插入 adhoc 分支，
新函数放 `executor/adhoc.go`：

```go
// checkAdhocScript 判"这条任务要跑哪个文件"，返回解析后的绝对路径。
func checkAdhocScript(p *Profile, raw string) (string, error)
```

判据依次是：非空 → 无控制字符（复用 `containsControl`，`executor/profile.go:1138`）→
无 shell 元字符（新函数 `firstPathSpecial`，`executor/adhoc.go:367`：与 `firstShellSpecial`
（`executor/profile.go:1204`）同一份字符集，只放开反斜杠，因为 Windows 的绝对路径必带它）→
能算出绝对路径（复用 `resolveAnywhere`，`:959`）→
命中 `executors.adhoc.path_prefixes` 前缀之一（空列表=不限）→ 扩展名在允许集合内
（`executors.adhoc.require_extension` 打开时）→ 文件存在且是普通文件（复用 `fileCheckReason`，
`executor/probe.go:115`）。

`Submission`（`executor/args.go:29`）加一个字段 `Script string` 承接结果；两个入口判据都是
`takeAdhocLocation`（`executor/adhoc.go:201`，在 `ValidateSubmission` 的 `executor/args.go:148` 调用）；
需要的三样取值（workspace、允许的目录、扩展名）由 N04 构造时写进 `Profile` 字段，函数不再收配置；
`submissionFieldNames`（`:71-73`）加 `"script"` 与 `"url"` 两个键，但只有 adhoc 档位接受：
非 adhoc 档位带这两个键仍然报"payload 的键不被这条档位接受"，错误文案里列出允许键（`:109`）。

`Render`（`executor/args.go:531`）在 `KindScript` 一支里：`p.Adhoc` 时用
`[]string{p.Runtime, sub.Script}` 替换 `[]string{p.Runtime, p.ScriptPath}`（现位置 `:541`）。
argv 仍然是数组直传、不经过 shell，这一条不变（`:529` 注释里写明的 D3）。

### 5.5 HTTP 目标的提交期校验（TASK-N05）

`Submission` 加 `URL string`。校验函数同文件：

```go
// checkAdhocURL 判"这条任务要打到哪里"，返回清洗后的地址写法（string）。
func checkAdhocURL(p *Profile, raw string) (string, error)
```

返回字符串而不是 `*url.URL`：执行侧 `resolveTarget` 会在 `checkTarget` 里再解析一次并判
scheme、凭据与名单（`executor/http.go:251`、`:267`），提交侧拿着结构体没有消费者。

判据：无空白与控制字符 → `url.Parse` → scheme 只允许 http/https → 不许带 `user:pass`
→ 主机非空 → 命中 `executors.adhoc.url_hosts`（空=不限）→ 记录解析后的规范化地址。
**地址范围守卫不在这里做**，因为 DNS 解析结果要在真正发请求前判才有意义，
现成的 `allowedIP`/`refusalReason`（`executor/http.go:705`、`:744`）已经负责这件事，继续用它。

`HTTPRunner` 的地址链拆成 `resolveTarget`（`executor/http.go:251`）与 `checkTarget`（`:267`）：
前者按 `p.Adhoc` 分流——内置档位直接把 `sub.URL` 送进 `checkTarget`，普通档位先 `renderRaw`
（`executor/http.go:290`）再送进同一条判断；原 `fillTemplate` 因此没有保留（只剩两行的副本会把
"真正要访问的地址怎么判"复制成两条链）。`checkTarget` 里的四道判据（scheme、主机非空、凭据、
`hostAllowed` 名单）两条来源都跑，名单判据写成"有名单才判"：空名单只可能来自 `url_hosts` 留空，
而 `executors.commands` 的 http 档位仍然必须有非空名单（`checkAllowedHosts`，`executor/profile.go:812`）。
打码路径（`renderURL`，`executor/http.go:230` 的 `recorded`）对 adhoc 走同一份地址：
内置档位不声明 secret 参数，产物文件里记的就是那条地址本身。

### 5.6 接口面与元数据（TASK-N06）

`ExecutorProfileResponse`（`api/handlers_executors.go:420` `toExecutorProfile` 组装的那个结构）加三项：

```go
Adhoc    bool                `json:"adhoc"`
Location *ProfileLocation    `json:"location,omitempty"` // 只有 adhoc 档位有
```

```go
type ProfileLocation struct {
    Key      string `json:"key"`   // payload 里的顶层键：script | url
    Kind     string `json:"kind"`  // path | url，前端据此决定输入框类型与提示
    Label    string `json:"label"` // "脚本路径（.php）" / "请求 URL"
    Required bool   `json:"required"`
    Hint     string `json:"hint"`  // 允许的取值范围，例如"必须落在 D:/work/scripts 之内"
}
```

其余接口动作：

| 项 | 位置 | 内容 |
| --- | --- | --- |
| 启动告警 | `executor/register.go`（`warnRelaxedAddressPolicy`，`:105` 现成的一条同类告警旁边） | adhoc 打开时记一行 warn，说清"任何够档位的身份都能让这台机器执行它收到的路径" |
| 身份门禁 | `api/handlers_executors.go:565` `gateExecutorSubmissionRole` | 复用，不改 |
| 结果读取门槛 | `api/handlers_executors.go:673` `resultGuard` | 复用，不改：它已经在用 `snapshot.HandlerKey()` 取档位（`:75`），解耦后自然按类型判定 |
| 台账 | `api/audit.go:181` 一带 | 已记 `HandlerKey`，adhoc 任务的类型就是 `exec.php` 这些键，无需新增动作词（建任务的动作词已在 `:109` 附近） |

### 5.7 配置与热重载（TASK-N03）

`ExecutorsConfig`（`core/config.go:220`）下新增一节：

```go
type AdhocConfig struct {
    Enabled         bool          `mapstructure:"enabled"`           // 默认 false
    ShellRuntime    string        `mapstructure:"shell_runtime"`     // 默认 bash
    PathPrefixes    []string      `mapstructure:"path_prefixes"`     // 空=不限
    RequireExtension  bool        `mapstructure:"require_extension"` // 默认 true
    URLHosts        []string      `mapstructure:"url_hosts"`         // 空=不限主机
    URLAllowPrivate bool          `mapstructure:"url_allow_private"` // 默认 false
    HTTPTimeout     time.Duration `mapstructure:"http_timeout"`      // 内置 http 档位的档位级超时
}
```

配套动作（缺任何一项都会被现成守卫用例抓出来）：

1. `Validate`：`enabled=true` 必须同时 `executors.enabled=true`（照 `core/config.go:477` 那条）；
   `shell_runtime` 必须在 `runtime_allow` 内；`url_hosts` 复用 `checkAllowedHosts` 的主机写法判据。
2. `Normalized`：给默认值（false / bash / 空 / true / 空 / false / 30s）。
3. 环境变量覆盖：`GODELAYQ_EXECUTORS_ADHOC_ENABLED`，与 `GODELAYQ_EXECUTORS_ENABLED`
   同一套写法（`configs/config.example.yaml:78`）。
4. 热重载归类：`core/config_reload.go` 的表里，`executors.adhoc.*` 整节按 `ClassRestart`
   （它绑定注册表内容与执行体的身份，与 `executors.enabled` 同类）。
   叶子覆盖用例（`core/config_reload_test.go:101` `assertEveryExportedFieldCovered`）要求
   `AdhocConfig` 的每个导出字段都摊得出叶子并都被归类。
5. 两份配置同步：`configs/config.example.yaml`（入库）与 `configs/config.yaml`（含凭据、被
   `.gitignore` 排除），守卫是 `TestExampleConfigMatchesLocal`。每个键都要带注释，
   注释体例照同节既有写法（`configs/config.example.yaml:74-114`）。

## 6. 前端实现方案（TASK-N07）

| 文件 | 改动 |
| --- | --- |
| `web/src/api/types.ts:292` | `CreateJobRequest` 加 `type?: string`；`Job` 加 `type?: string`；`ExecutorProfile`（`:91`）加 `adhoc?: boolean` 与 `location?: {…}` |
| `web/src/api/jobs.ts` | `listJobs` 的查询参数白名单加 `type`（该函数逐字段列参数，新增键必须在这里露头，见文件头注释） |
| `web/src/components/jobs/JobForm.vue` | ①`form` 加 `jobType` 与 `location`（`:60` 附近）；②`:553-566` 的名称下拉换成 `UiInput` 并加规则说明；③新增类型 `UiSelect`，选项由 `:431-449` 的 `nameOptions` 拆出来的 `typeOptions`，分三组；④`:115-120` 的档位判定从 `form.name` 换成 `form.jobType`；⑤`:511` 提交体带 `type`，adhoc 类型时把位置写进 `payload.script` / `payload.url`；⑥校验：名称按 D3 规则在前端先判一遍（照 `web/src/components/groups/GroupForm.vue` 与后端同源正则的做法），位置的判据只做提示性检查，服务端 400 原样回显 |
| `web/src/components/jobs/JobTable.vue` | 名称列旁边加"任务类型"列（等宽字体显示 `exec.php` 这类键） |
| `web/src/components/jobs/JobFilterBar.vue:84` | 现在的"名称"下拉候选来自 jobTypes，语义已经不对：改成"类型下拉 + 名称文本框"两个筛子 |
| `web/src/views/JobsView.vue:315`、`:426` | 传给抽屉的 props 由 `jobTypes` 换成"类型候选"（含元数据） |
| `web/src/views/JobDetailView.vue:136`、`:160` | 标题与档位判定改用 `job.type`；详情页多一行"任务类型"，adhoc 任务把位置显示出来（来自 payload） |
| `web/src/content/job-template.md`、`web/src/content/*.md` | 模板文档里"任务名称=已注册的任务类型"这类说法按新事实重写 |

内嵌形态的构建顺序（改 `.vue` 必须两步，否则页看不到变化）：
先 `npm run build` 产出 `web/dist`，再 `go build -tags dashboard`。

## 7. 数据与兼容

| 场景 | 行为 |
| --- | --- |
| 老任务快照（Type 为空） | `HandlerKey()` 回退 Name，一切照旧（`core/job.go:265-270`） |
| 只带 `name` 的旧客户端 | 走 D4 的旧写法：`name` 兼作注册键，不套名称规则 |
| `POST /jobs/batch` | 逐条走同一个 `createJobFromRequest`，自动获得新语义；单条失败仍按原始下标回报 |
| 目录任务加载器 | 本期不改：`core/load.go:25` 的 `name` 仍同时是查找键，名称规则不套在它上面。登记在 §10 |
| `PauseByHandlerKey`（删档位时钉任务） | 已按 `HandlerKey()` 匹配（`core/scheduler.go:992`），解耦后覆盖新写法建的任务 |
| 崩溃恢复守卫 | 已按 `snap.HandlerKey()` 判类别（`cmd/server/main.go:945`），无需改 |
| 观测层 SQLite | 不改表（D13）：事件里的 `job_name` 从此是标签，看执行体身份要按 `job_id` 回查任务 |
| `docs/api.md`、swagger 注释 | `CreateJobRequest`/`JobResponse`/`ListJobs` 查询参数三处要更新（TASK-N08） |

## 8. 安全边界：这次动了哪一条，没动哪一条

**动了的（使用者 2026-10-06 明确选择放宽）：**

| 原口径 | 位置 | 现在的口径 |
| --- | --- | --- |
| 可执行体身份不能在提交任务时决定 | `web-profile-design.md:38` D7、`config-reload-design.md:304` | adhoc 类型的任务在提交时给路径/URL；其余档位身份仍不可变（内置条目的身份字段由配置定死：解释器、方法、超时） |
| 脚本必须落在 `executors.workspace` 之内 | `executor/profile.go:993` `resolveInside` | adhoc 走 `PathAnywhere`（`:55`），默认允许本机任意路径；可用 `executors.adhoc.path_prefixes` 收紧 |
| HTTP 档位必须有非空 `allowed_hosts` | `executor/profile.go:744-746` | adhoc 的 `url_hosts` 允许为空=不限主机；`executors.commands` 里的普通档位这条规则不变 |

**没动的（本文全部复用现成实现）：**

1. 不经过 shell：argv 数组直传，无 `sh -c` / `cmd /c`（`executor/args.go:529` 注释钉住的 D3；
   Windows/Unix 各自的 `proc_windows.go` / `proc_unix.go` 不使用外壳）。
2. 解释器白名单：`runtime_allow`（`core/config.go:234`）仍然约束内置条目能注册哪几条。
3. 环境变量：`env_allow` 与 `GODELAYQ_` 前缀强制排除不变（`configs/config.example.yaml:89-93`）；
   adhoc 档位不声明 `env_allow`，因此 payload 无法注入任何环境变量。
4. 超时：执行器任务没有"不限制"这一档，`default_timeout`/`max_timeout` 上限照旧
   （`executor/args.go:487` `EffectiveTimeout`）。
5. 地址范围守卫：SSRF 那一层不放宽（D9 第③层，`executor/http.go:744`），
   只有显式 `url_allow_private: true` 才整条关闭，且关闭时启动记 warn（与既有
   `deny_private_ranges=false` 的处理同一条，`:742-743` 注释）。
6. 重定向一律拒绝（`executor/http.go:110` `refuseRedirect`）。
7. 产物落盘 workspace 之内、输出裁剪与 secret 掩码机制照旧。
8. 提交身份门槛：`executors.required_role`，默认 admin（`configs/config.example.yaml:80-83`）。
9. 打开 `executors.enabled` 必须有鉴权这一条既有告警不变（`configs/config.example.yaml:76-79`）。

**运维须知（要写进配置注释与文档，不能只留在这里）：**
`executors.adhoc.enabled=true` 意味着"任何够 `required_role` 的身份可以让这台机器执行它发来的
任意路径"。生产环境应当：把 `path_prefixes` 收紧到明确的脚本目录、保持 `require_extension=true`、
给 HTTP 配 `url_hosts`、并把 `required_role` 保持在 admin 或以上。

## 9. 测试与验证口径

| 层次 | 内容 |
| --- | --- |
| 单元 | `core` 名称规则（中文/字母/数字通过；空格、下划线、标点、超长、空串拒绝；按字符计数） |
| 单元 | `api` 双写法：带 `type` 时 name 是标签且不参与查表；不带 `type` 时行为与改动前逐字一致（回归用例照 `api/api_test.go:56` `TestCreateJob` 与 `:84` `TestCreateJobUnknownType` 补） |
| 单元 | `api` 门禁按 `HandlerKey` 判档位：名称不是注册键的 adhoc 任务必须走到 payload 校验（这条是 §5.2 的顺序要求的守门用例） |
| 单元 | `executor` adhoc：路径的六条判据逐条（含 Windows 反斜杠、盘符、`..`、目录当文件、扩展名不符）；URL 的六条判据逐条 |
| 单元 | `executor` Render：adhoc 与非 adhoc 两条 argv 前缀互不影响；普通档位的 argv 用例一条都不改（防回归） |
| 契约 | `api/profile_form_contract_test.go`、`api/executors_response_test.go`：新增字段与既有字段的形状；`adhoc`/`location` 只在 adhoc 条目出现 |
| 配置 | `core/config_reload_test.go` 的叶子覆盖与归类用例；`TestExampleConfigMatchesLocal`（两份配置同步）；`TestValidate` 系列 |
| 全仓 | `go build ./...`、`go vet ./...`、`go test ./...`，再加 `go test -race -timeout 30m ./...`（`-count=5` 必须带 `-timeout 30m`） |
| 界面 | 浏览器实测按 `docs/design/tasks/new-tasks/README.md` §共同验证口径：用 `-tags dashboard` 内嵌形态跑，四类各建一条任务并跑到终态 |
| 端到端 | 真实进程冒烟：PHP/Python/Shell/HTTP 各一条，含"路径不存在→400"、"私网地址→执行期拒绝且不外连"、"名称带空格→400"、"只带 name 的旧写法仍能建任务" |

`gofmt -l` 在本仓库因 CRLF 会全量误报，判断格式问题时以实际 diff 为准（既有口径，见
`docs/design/tasks/` 各系列的验证记录）。

## 10. 风险与缺陷登记

| # | 风险 | 处置 |
| --- | --- | --- |
| 1 | 解耦只改 API 不改门禁判据 → adhoc 任务的 payload 未经校验就入队并被真的执行 | 已登记，TASK-N02 硬性要求同一卡内改 `gateExecutorSubmission`，并由 §9 第三条用例守住 |
| 2 | `exec.php` 等四个键与用户自建档位撞名 | 按 D15：内置让位 + 启动日志说明；TASK-N04 用例覆盖 |
| 3 | 名称规则套到旧客户端上会把既有调用全部拒掉 | 按 D4：只在带 `type` 时套规则；TASK-N02 的回归用例覆盖 |
| 4 | 只带 `name` 时名称规则为空转（可以传 `payment_check` 这类带下划线的标签） | 登记不修。这是 D4 兼容选择的直接代价，收紧到"一律套规则"要另做客户端迁移，属范围变更而不是缺陷 |
| 5 | adhoc 路径来自任务，`Probe` 的"这台机器跑不跑得动"结论对内置条目失去意义 | 已登记：内置 adhoc 条目的探测只判解释器（`executor/probe.go:80` `probeProgramName`），不判文件；文件存在性在提交期与执行前各判一次（D8） |
| 6 | SSRF：自由填 URL 可以打内部服务 | 部分缓解：D9 的三层守卫保留、`url_hosts` 可配、`url_allow_private` 默认关。残余风险由 `required_role` 承担，写进 §8 运维须知 |
| 7 | 任意路径执行 = 本机的任意命令执行面（例如 `C:/Windows/System32/…`） | 主动放宽的选择，不再试图在这里补防：靠 `path_prefixes` + `require_extension` + `runtime_allow`（只经解释器，不经外壳）收窄；§8 的注释与启动 warn 是必须交付的部分 |
| 8 | payload 顶层新增 `script`/`url` 两个键，普通档位收到它们时报错文案要精确，否则会误以为是"新写法不被支持" | 已登记：TASK-N05 要求错误文本列出该档位实际接受的键（现成 `submissionKeys`，`executor/args.go:197`） |
| 9 | 事件表 `job_name` 从此是标签，同名的不同类型任务在事件列表里看起来一样 | 登记不修（D13）：详情页按 `job_id` 回查，观测层不复制任务身份 |
| 10 | `PUT /jobs/:id` 不能改名，界面上"编辑"表单的名称框会被误解成可改 | 已登记：TASK-N07 在编辑模式下把名称渲染成只读并写明原因；能力本身列入后续卡 |
| 11 | 目录加载器仍以 `name` 为查找键，与新写法语义不一致 | 登记为后续项：要么给任务文件加 `type`，要么在文档里写明"加载器只支持旧写法"。TASK-N08 先把现状说清楚 |
| 12 | 热重载分类漏登记会让"改了 adhoc 配置静默不生效" | 由现成守卫用例强制（`core/config_reload_test.go:101`、`TestEveryLeafKeyIsClassed`）；TASK-N03 的 DoD 直接写这两条 |

## 11. 任务拆分

| 卡 | 主题 | 依赖 |
| --- | --- | --- |
| TASK-N01 | core：任务名称规则 | — |
| TASK-N02 | api：名称/类型解耦与按类型筛选 | N01 |
| TASK-N03 | 配置：`executors.adhoc.*` 一节 + 两份 yaml + 热重载归类 | — |
| TASK-N04 | executor：adhoc 档位构造与注册 | N03 |
| TASK-N05 | executor：提交期路径/URL 校验与执行侧渲染 | N04 |
| TASK-N06 | api：档位元数据、启动告警、门禁复用 | N02、N05 |
| TASK-N07 | web：新建任务表单三处改动 + 列表/详情/筛选 | N02、N06 |
| TASK-N08 | 文档收口与全仓验证、端到端实测 | N01…N07 |

卡的落点：`docs/design/tasks/new-tasks/`，顺序与验证口径见该目录 `README.md`。

## 12. 待拍板（写卡时按推荐值落的）

| # | 事项 | 推荐值 | 影响面 |
| --- | --- | --- | --- |
| P1 | adhoc 的路径范围默认 | 默认**不限**（`path_prefixes: []`），注释里给收紧示例 | 使用者明确要求放宽，默认不限最贴合"可以输入具体位置"这句原话；代价是新部署开箱即风险，靠注释与启动 warn 提示 |
| P2 | 内置键名 | `exec.php` / `exec.python` / `exec.shell` / `exec.http` | 界面标签直接照它写；若使用者想要 `exec.adhoc.*`，档位名字符集不含点（`core/executor_profile_store.go:22`），需要改成 `exec_adhoc_php` 这类写法 |
| P3 | 是否再加 Node/PowerShell/cmd/Java 四条内置 | 本期只做 PHP/Python/Shell/HTTP 四条，其余按 `runtime_allow` 由用户自建普通档位 | 使用者原话是"PHP、Python、脚本shell/bash、http、其它等等现在可以执行的类型"，"其它"由现有档位列表承担 |
| P4 | 名称长度上限 | 64 个字符 | 与档位名规则对齐；上限只影响标签能写多长 |
| P5 | HTTP adhoc 是否允许带请求体 | 允许，`Body=json`，且仍受 `executors.commands` 那套 body 形态校验 | 使用者只提到"URL 地址"，但没体与健康检查/回调类需求不符；若只要 GET 可改成 `none` |
