# TASK-N07　web 控制台：新建任务表单与列表改造

- 所属阶段：M3 界面
- 依赖任务：TASK-N02、TASK-N06
- 涉及文件：`web/src/components/jobs/JobForm.vue`、`web/src/api/types.ts`、`web/src/api/jobs.ts`、
  `web/src/api/keys.ts`、`web/src/api/executors.ts`、`web/src/views/JobsView.vue`、
  `web/src/views/JobDetailView.vue`、`web/src/components/jobs/JobTable.vue`、
  `web/src/components/jobs/JobFilterBar.vue`、`web/src/display.ts`、`web/src/content/job-template.md`
- 预计规模：大

## 1. 任务目标

把"任务名称（= 已注册的任务类型）"这个下拉换成三件事：
**名称自由填写** + **类型下拉（分三组）** + **adhoc 类型的位置输入框**；
列表与详情同时显示名称和类型，筛选条拆成"类型下拉 + 名称文本框"。

## 2. 背景与当前问题

`web/src/components/jobs/JobForm.vue` 现在只有一个名称字段，控件是下拉
（`:553-566`，标签文案就是使用者要改掉的那句"任务名称（= 已注册的任务类型）"），
选项由 `nameOptions`（`:431-449`）用 `props.jobTypes`（来自 `GET /job-types`）拼出来；
"这条任务是不是档位任务"按 `form.name` 的 `exec.` 前缀判（`:115-120`）；
提交体只带 `name`（`:511`）。N02 之后名称与类型是两件事，这三处必须一起改，
否则"档位参数表单"会在选了普通类型 + 中文名时错误地不出现，或者反过来。

现成可抄的两个形态：

| 先例 | 位置 | 抄什么 |
| --- | --- | --- |
| 档位数据源与退化 | `JobForm.vue:81-92`、`:94-106` | 已经读 `GET /executors`、按 `enabled` 退化、按 `required_role` 决定条目可见 |
| 后端给结论、前端只呈现 | `JobForm.vue:444`（`不可用：${spec.reason}`） | `location.label`/`hint` 沿用同一口径，前端不拼规则句子 |

## 3. 要实现的功能

### 3.1 三个字段（新建模式）

| 顺序 | 字段 | 控件 | 判据 |
| --- | --- | --- | --- |
| 1 | 任务名称 | `UiInput` | 必填；正则 `^[\p{Han}A-Za-z0-9]{1,64}$`（与 `core.ValidateJobName` 同源，N01）；下方一行固定说明"名称只是标签，跑什么由下面的任务类型决定"；服务端 400 原样回显到表单顶部（既有 `serverError` 通道，`JobForm.vue:47`） |
| 2 | 任务类型 | `UiSelect` | 必填；选项见 §3.2；沿用 `canSubmitExecutor`（`:106`）决定档位条目在不在下拉里 |
| 3 | 执行位置 | `UiInput` | **仅当选中的类型带 `location` 时出现**；标签用 `location.label`、占位与提示用 `location.hint`；必填；前端只做形态提示（URL 检查 scheme、路径检查非空），拒绝权在服务端 |

选中非 adhoc 档位时，位置那一栏改成**只读展示**：脚本/产物显示 `path_display`
（后端来源 `executor/profile.go:195`）、HTTP 显示 `url` 模板（`web/src/api/types.ts:121` 已有）。
选中普通任务时不出现位置栏，`payload` 仍是 JSON 编辑器（`PayloadEditor.vue`）。

### 3.2 类型下拉的分组

```
普通任务
  payment_check
  email_send
  …
执行器档位
  exec.nightly_report（Shell 脚本）
  exec.order_api（HTTP 请求）
  …
自由执行（位置由任务给出）
  exec.php（PHP 脚本）
  exec.python（Python 脚本）
  exec.shell（Shell 脚本）
  exec.http（HTTP 请求）
```

- 分组判据：`location` 存在 → 第三组；`kind`/`runtime` 存在 → 第二组；其余 → 第一组。
  **前端不再自己认 `exec.` 前缀**（现在的写法在 `:115-120`、`JobDetailView.vue:136`）。
- 不可用条目仍然可见但选不动，标签写 `不可用：${reason}`（照 `:444` 既有做法）。
- `canSubmitExecutor=false` 时第二、三组整体不出现（既有口径：入口隐藏只是体验，
  服务端 403 才是边界，见 `:100-105` 的注释与 `api/handlers_executors.go:565`）。

### 3.3 提交体

```ts
const body: CreateJobRequest = { name: form.name.trim(), type: form.jobType, payload, group }
```

- `payload` 组装：adhoc 脚本类 → `{ [location.key]: form.location.trim() }`（可叠 `timeout`）；
  adhoc http → `{ url, …headers/body/params 按 N05 的归属 }`；非 adhoc 档位 → 现有
  `buildExecutorPayload`（`JobForm.vue` 里已在用的那个函数）；普通任务 → 现有 JSON 解析结果。
- 提交前校验：`canSubmit`（`:413`）与 `nameMissing`（`:411`）改成
  "名称合法 + 类型已选 + 位置（需要时）已填 + 触发方式合法 + 参数区无错"。

### 3.4 编辑模式

- 编辑抽屉里名称与类型都是只读（后端本期不支持改名与换类型，§D14、`api/handlers.go:334`），
  控件换成只读展示 + 一行原因说明，避免"填了不生效"的假表单
  （这份文件头部注释已经写明"编辑模式字段集比新建小一圈"的同一条理由）。
- adhoc 任务在编辑模式下位置只读：值来自 `job.payload.script` / `.url`。
- 档位参数表单在编辑模式仍然不给（`JobForm.vue:111-113` 的既有理由：回填的是掩码值）。

### 3.5 列表、详情与筛选

| 页面 | 位置 | 改动 |
| --- | --- | --- |
| 任务列表 | `web/src/components/jobs/JobTable.vue` | 名称列旁加"任务类型"列（等宽字体显示 `exec.php`）；列宽与折叠按既有列的样式令牌走，不新增令牌 |
| 筛选条 | `JobFilterBar.vue:84` | 现在这个"名称"下拉的候选值来自 `jobTypes`，解耦后语义不对：改为"类型下拉（候选同上）+ 名称文本框"两个筛子，分别落 `?type=` 与 `?name=` |
| 详情 | `JobDetailView.vue:136`、`:160` | 标题旁一行显示类型；adhoc 任务把位置显示出来（取 payload）；档位判定改用 `job.type` |
| 宿主页 | `JobsView.vue:315`、`:426` | 传给抽屉的 props 由 `jobTypes: string[]` 换成带元数据的类型候选（或保留 `jobTypes` 再加一个 `profiles`，取一种并在 §10 记录） |

### 3.6 类型与接口封装

- `web/src/api/types.ts:292` `CreateJobRequest` 加 `type?: string`；`Job` 加 `type?: string`；
  `ListJobsQuery` 加 `type?: string`；`ExecutorProfile`（`:91`）加 `adhoc?: boolean` 与
  `location?: { key: string; kind: string; label: string; required: boolean; hint: string }`。
- `web/src/api/jobs.ts` 的 `listJobs` 逐字段列查询参数（文件头注释写明这条纪律），
  新参数在这里露头才算生效。
- 缓存键（`web/src/api/keys.ts`）若因响应形状变化需要重取，一并核对。

### 3.7 文案同步

`web/src/content/job-template.md` 里"任务名称=已注册的任务类型"、
"改 `executors.commands` 需要重启"一类的说明按新事实重写：名称是标签、类型决定执行、
自由执行类型的位置在提交时给出。这份文档是使用者直接读的，不能与界面不一致。

## 4. 实现步骤

1. 类型与 API 封装（§3.6）先改，`vue-tsc` 会指出所有需要跟进的使用点——按报错清单改，不自己找。
2. `JobForm.vue`：`form` 加 `jobType`、`location` 两项（`:60` 附近）；
   `profile` computed（`:115-120`）的判据换成 `form.jobType`；
   `nameOptions`（`:431-449`）拆成 `typeOptions`（三分组）与只读的位置展示计算；
   模板里名称下拉 → `UiInput`，新增类型 `UiSelect` 与位置 `UiInput`（`:553-566` 一带）；
   `canSubmit`/`nameMissing`/`submit`（`:411`/`:413`/`:511`）同步。
3. 编辑模式只读化（§3.4）。
4. 列表、筛选、详情三处（§3.5）。
5. 文案（§3.7）。
6. `cd web && npx vue-tsc --noEmit && npm run build`。
7. 内嵌形态实测：`go build -tags dashboard ./cmd/server`（`web/dist` 被 `.gitignore` 排除，
   必须先 build 前端再 build Go）。

## 5. 测试要求

前端没有单元测试框架的先例（本仓库界面靠类型检查 + 浏览器实测），因此：

| 项 | 要求 |
| --- | --- |
| 类型检查 | `npx vue-tsc --noEmit` 无错 |
| 构建 | `npm run build` 成功；产物 `web/dist` 比源码新（记录里给时间戳） |
| 界面实测（必做） | ①新建：中文名称 + `exec.php` + 一个不存在的路径 → 表单允许提交、服务端 400 原文回显在顶部；②改成存在的路径 → 建成功，列表里名称列显示中文、类型列显示 `exec.php`；③切到 `payment_check` → 位置栏消失、JSON 编辑器出现；④切到普通档位 → 参数表单出现、位置只读；⑤筛选：按类型筛与按名称筛各一次；⑥详情：adhoc 任务显示位置；⑦编辑模式：名称与类型只读 |
| 权限实测 | 用 `operator` 身份打开同一页，第二、三组类型不出现，且下拉里只剩普通任务 |
| 观测限制 | 秒级现象（toast、折叠动画）在应用内标签页会被节流，一律标"未观测"而不是"正常"（既有实测口径） |
| 后端回归 | `go test ./api ./core ./executor -race -timeout 30m`（前端改动不该动它们，但表单落的是同一批端点） |

## 6. 完成标准（DoD）

1. 新建任务页三个字段按 §3.1 出现与消失，且**没有任何一处前端代码再按 `exec.` 前缀判档位**
   （`grep -rn "startsWith('exec" web/src` 只剩注释或零命中）。
2. 名称与类型分别落到请求体的 `name` 与 `type`；`payload` 对 adhoc 类型是
   `{"script":…}` 或 `{"url":…}`（与 N05 的键名一致，用请求体的实际 JSON 证明而不是看代码）。
3. 列表页同时显示名称与类型，筛选条给的是"类型下拉 + 名称文本框"，
   后端查询参数确实是 `?type=` 与 `?name=`（网络请求面板证据）。
4. 编辑模式不出现"填了不生效"的控件（名称、类型、位置均只读，且写明原因）。
5. `job-template.md` 已无旧说法（`grep` 复核）。
6. 图标全部来自 lucide，样式只用既有令牌。

## 7. 验收方式

```bash
cd web && npx vue-tsc --noEmit && npm run build
cd .. && go build -tags dashboard ./cmd/server
grep -rn "startsWith('exec" web/src
grep -rn "已注册的任务类型" web/src
```

预期：类型检查与构建无错；第一条 `grep` 零命中；第二条只在历史说明里出现或零命中。
浏览器实测按 §5 的七条逐项做，每项记录请求体与响应状态码。

## 8. 不在本任务范围

- 不做脚本文件浏览/选择器（README S-4）。
- 不做任务改名（README S-1）。
- 不动档位管理页 `ProfilesView.vue` 与 `ProfileForm.vue`（那四条内置档位不在档位文件里，
  页面上不可编辑，`editable` 后端已经是 false）。
- 不改样式令牌。
- 不做"类型下拉里显示每条档位的说明文字"这种增强（本期只给分组与不可用原因）。

## 9. 风险与回滚

| 风险 | 说明 | 退路 |
| --- | --- | --- |
| 前端复制一份名称正则并与后端漂移 | 界面放过、服务端拒，用户看到的是"提交才报错" | 正则同源写在一处常量并加注释指向 `core/job_name.go`；服务端 400 必须原样回显（既有 `serverError` 通道） |
| 改了 `.vue` 但没重 build 前端就验证 | 内嵌的是 `web/dist`，页看不到变化会误判成后端问题 | DoD 第 1 条之前先跑 §7 的两条 build |
| 编辑模式回填空 `type` | 旧任务（`type` 为空、名称即键）在编辑时把类型显示成空 | 回落显示 `job.name` 并标注"（旧写法：名称即类型）" |
| 分组判据依赖 `location`，N06 未落地时下拉只剩第一组 | 界面看起来"能力丢了" | 本卡依赖 N06，顺序不许颠倒（§依赖任务） |

回滚：界面改动是纯前端 + 请求体多一个可选字段，回退 `web/src` 即可；
后端 N02 的兼容双写法保证旧请求体仍能建任务。

## 10. 实现记录（执行时补写）

| # | 与卡片的偏离 | 原因 |
| --- | --- | --- |
