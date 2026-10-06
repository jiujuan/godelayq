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

落地：`web/src/api/types.ts`（`Job.Type`、`CreateJobRequest.Type`、`ListJobsQuery.Type`、
`ExecutorProfile.Adhoc` / `.Location`、新接口 `ExecutorProfileLocation`）、`web/src/api/jobs.ts`
（`listJobs` 的查询参数逐字段清单里加 `type`）、`web/src/display.ts`（`jobTypeOf`）、
`web/src/components/ui/UiSelect.vue`（`SelectOption.group` 与 `<optgroup>` 渲染路径）、
`web/src/components/jobs/JobForm.vue`（三字段与三分组、位置输入框、编辑模式只读化）、
`web/src/components/jobs/JobTable.vue`（"类型"列）、`web/src/components/jobs/JobFilterBar.vue`
（类型下拉 + 名称文本框）、`web/src/views/JobDetailView.vue`（类型行与位置行）、
`web/src/views/JobsView.vue`（筛选状态与地址栏回写各加一个 `type`）、
`web/src/content/job-template.md`（§1 拆成名称与类型两行、新增 §12.4 自由执行类型）。
后端零改动。

| # | 与卡片的偏离 | 原因 |
| --- | --- | --- |
| 1 | 类型下拉的三组**全部**用 `<optgroup>`，占位项"请选择任务类型"留在分组外平铺 | 卡面 §3.2 的图示把三组并列；原生 `select` 的占位项放进分组会让人以为它是一个可提交的类型。`UiSelect` 的分组是新加的可选能力，一项都不带 `group` 时渲染路径与改动前逐字相同（其余下拉不受影响） |
| 2 | 名称正则写成 `^[\p{Script=Han}A-Za-z0-9]{1,64}$`（卡面是 `\p{Han}`） | 本仓库的 `vue-tsc` 不认 `\p{Han}` 这个别名（`TS1529 Unknown Unicode property name`），`Script=Han` 是同一份 Unicode 数据的正式写法；判据仍与 `core.ValidateJobName` 同源，常量处注释指向 `core/job_name.go` |
| 3 | 位置输入框的 `placeholder` 由前端给举例（两种后缀各一个），`location.hint` 仍作输入框下方的说明 | 卡面 §3.1 让"占位与提示都用 `location.hint`"，但后端那句 hint 是整段范围说明（"只能选这些目录里的文件：…"），塞进占位会把两行文字挤成一行灰字，且范围一变整句都要重排。举例给两种后缀是因为四条内置档位共用一个占位，对 `exec.php` 说"就像 report.sh 那样填"会说错 |
| 4 | 没有新增 `profiles` prop：`JobForm` 沿用自己在 TASK-E18 起就有的 `GET /executors` 查询，`JobsView` 继续只传 `jobTypes` | 卡面 §3.5 让两种做法二选一并记录。档位表、`required_role`、不可用原因本来就在同一份响应里，再经宿主转一遍会让抽屉的入参变成两份真相 |
| 5 | 列表、详情、编辑三处的旧写法回退统一走 `jobTypeOf()`（`web/src/display.ts`） | 与后端 `Job.HandlerKey()` 同一条判据（`type` 空则用名称）。三处各写一份 `job.type \|\| job.name` 的话，将来加第三种回退就又要改三处 |
| 6 | 详情页多读一次 `GET /executors`（`staleTime` 5 分钟），只为拿位置行的标题 | 位置标题带扩展名清单（`脚本路径（.sh、.bash）`），后端已经拼好；前端再拼一遍就是复制一份规则（README 共同口径第 2 条）。缓存键 `queryKeys.executors` 本来就有，不会每次进详情都打一遍接口 |
| 7 | 筛选条的类型下拉候选仍是 `GET /job-types` 的全量，不给"不可用"标注、也不按角色收口 | 筛选是查询条件而不是提交入口：档位在这台机器不可用，历史上用它的任务照样筛得出来；按角色收口会让 operator 看不见已经存在的档位任务 |
| 8 | 编辑模式那条只读段落在旧写法任务下不再重复打印名称 | 实测（00:22）看到 `payment_check·payment_check（旧写法：名称就是类型）`：类型为空时"名称·类型"退化成同一词两遍。改为类型为空只给名称 + 旧写法说明 |
| 9 | 位置的前端检查是三条：必填、控制字符（`U+0000`–`U+001F`、`U+007F`）、URL 的 scheme | 卡面 §3.1 只要求"URL 检查 scheme、路径检查非空"。加控制字符一条是因为服务端判据里换行与回车本来就在 shell 元字符表内（`executor/adhoc.go:367` `firstPathSpecial`），这类值不可能来自真实路径，输入时就说明比提交后 400 省一轮。路径里的空格、相对 workspace 的写法、上跳再下来的写法一律放过，拒绝权仍在服务端 |
| 10 | 名称与位置的错误在字段为空时就显示（未加"碰过才说"） | 既有口径：改动前那个名称下拉的 `:error="nameMissing ? '必须选择一个任务类型' : null"` 同样是即时显示，本卡没有引入新行为，也没有顺手改掉它 |
| 11 | 新增的 §12.4 与既有那节"跑完之后看什么"撞了同一编号，收尾复核时改成 §12.5 | 写卡时只说了"新增一节讲自由执行类型"，没规定编号；`grep "^### 12\."` 才看出两个 12.4 并排。文中没有指向旧编号的交叉引用（`grep "12\.4\|12\.5"` 只剩两条标题） |

DoD 核对：

1. **没有任何一处前端再按 `exec.` 前缀判档位**：`grep -rn "startsWith('exec" web/src` 零命中；
   分流判据是"选中类型的档位对象有没有 `location`"（`JobForm.vue` 的 `locationSpec`）。
2. 名称与类型分别落到 `name` 与 `type`，adhoc 的 `payload` 就是 `{"script":…}` / `{"url":…}`，
   证据是浏览器里 `POST /api/v1/jobs` 的请求体原文（见下面实测记录 ②）。
3. 列表同时显示名称与类型，筛选条给的是"类型下拉 + 名称文本框"，
   网络请求里确实是 `?type=` 与 `?name=`（下面 ⑤）。
4. 编辑模式名称、类型、位置均只读，且写明"名称与类型不可修改，要换就新建一条"（下面 ⑦）。
5. `grep -rn "已注册的任务类型" web/src` 零命中；`job-template.md` 的 §1 已按新事实重写。
6. 图标全部来自 lucide（本次未新增图标），样式只用既有令牌（未新增令牌，未改 `:root`）。

界面实测（内嵌形态：`-tags dashboard` 的 `godelayq-server.exe`，端口 8123，配置与数据目录在
`%TEMP%\n07smoke`，`executors.enabled: true` + `executors.adhoc.enabled: true`，
`required_role: admin`，档位 `hello_sh`（两条参数：`day` 必填、`token` secret）与 `absent_py`（脚本不存在））：

| # | 卡片 §5 的那一项 | 观测结果 |
| --- | --- | --- |
| ① | 中文名称 + `exec.php` + 不存在的路径 | 表单允许提交（"创建任务"未 disabled），`POST /api/v1/jobs` → **400**，抽屉不关，顶部 `role=alert` 是后端原文：`invalid executor payload：payload key "script": script file "nope_missing.php" does not exist`；位置输入框下方另有一句"后端拒了这条取值，原文见表单顶部的错误说明"（同一句拒绝按 `payload key "script"` 归位到这个字段，不重复贴原文） |
| ② | 改成存在的路径 | `POST` → **201**，请求体原文：`{"name":"界面实测脚本","type":"exec.php","payload":{"script":"C:/Users/xing/AppData/Local/Temp/n07smoke/ws/reconcile.php"},"delay":"10m"}`；列表表头 `名称 / 类型`，该行显示 `界面实测脚本` 与 `exec.php` |
| ③ | 切到 `payment_check` | 位置栏消失，JSON 编辑器（`textarea`，标签"payload 合法 JSON，字段由该任务类型的 Handler 定义"）出现 |
| ④ | 切到普通档位 | `exec.hello_sh`：位置只读展示 `print_path.sh` + "位置来自档位定义，任务只给参数。"，参数表单两栏（`day（必填） · 命令行参数` 是文本框、`token · 命令行参数` 是 `type=password`），未填 `day` 时"创建任务" disabled；填齐后 `POST` → **201**，请求体 `{"name":"档位参数实测","type":"exec.hello_sh","payload":{"args":{"day":"2026-10-07","token":"sekret123"}},"delay":"10m"}`，该任务到终态 success（服务端日志 `exit_code=0`）。`exec.absent_py` 在同一个下拉里可见但 `disabled`，标签是 `exec.absent_py（不可用：script file "not_deployed.py" does not exist）` |
| ⑤ | 按类型筛与按名称筛各一次 | 类型下拉选 `exec.php` → `GET /api/v1/jobs?type=exec.php&limit=50&offset=0`，表里只剩两条 exec.php；名称文本框输入"档位参数实测" → `GET /api/v1/jobs?name=%E6%A1%A3%E4%BD%8D…&limit=50&offset=0`，只剩一条；地址栏同步写成 `/jobs?type=exec.php` 与 `/jobs?name=…` |
| ⑥ | 详情显示位置 | `exec.php` 那条：`任务类型 exec.php`、`脚本路径（.php） C:/Users/…/reconcile.php`；`exec.shell` 那条的标题是 `脚本路径（.sh、.bash）`（同一份后端 label）；旧写法那条（请求体 `{"name":"payment_check"}`、不带 `type`）显示 `payment_check（旧写法：名称就是类型）` 且**没有**位置行 |
| ⑦ | 编辑模式名称与类型只读 | 名称、类型、位置三处都是文本段落，没有可编辑控件；`exec.shell` 那条段落是 `编辑分隔符检查·exec.shell（名称与类型不可修改，要换就新建一条）`，位置另起一行 `脚本路径（.sh、.bash）：C:/Users/…/print_path.sh`；档位任务的参数区仍是既有那段掩码说明 |
| 权限 | `operator` 身份打开同一页 | 退出 admin01、登录 oper01（`required_role: admin`）后，新建抽屉的类型下拉只剩占位项 + "普通任务"一组（`data_sync`、`email_send`、`payment_check`、`report_generate`），第二、三组整体不出现；选中 `payment_check` 之后下方给出说明"执行器档位与自由执行类型需要 admin 及以上才能提交，当前身份的下拉里没有它们。"（未选类型时这一句被同一位置的必填提示挡住——`UiSelect` 是 error 优先于 hint，既有实现） |
| 未观测 | toast、折叠动画等秒级现象 | 应用内标签页被隐藏时时钟节流，一律标**未观测**（既有实测口径）。`cancelled` 状态的行仍然不出现在列表里（取消即删记录），本卡未覆盖 |

构建与回归：

- `cd web && npx vue-tsc --noEmit` 退出码 0；`npm run build` 成功，
  上表那批实测跑在 `web/dist/index.html` 时间戳 `2026-10-07 00:25:45` 的产物上，
  随后 `go build -tags dashboard ./cmd/server` 产出的二进制时间戳 `2026-10-07 00:25:47`
  （内嵌的是 `web/dist`，顺序必须是先前端再 Go）。§12.4 编号撞车修掉之后又重了一次，
  最终产物是 `web/dist/index.html 00:39:26` 与二进制 `00:39:28`，
  任务模板页复核：`12.4 自由执行类型（打开 executors.adhoc 时）` 与 `12.5 跑完之后看什么` 各一处。
- 后端回归 `go test ./api ./core ./executor -race -timeout 30m` 全绿：
  `ok godelayq/api 212.311s`、`ok godelayq/core 21.107s`、`ok godelayq/executor 24.728s`（2026-10-07）。

环境说明（照实记）：`%TEMP%\n07smoke` 在 00:15–00:18 之间被系统回收掉 `config.yaml` 与 `ws/`
（`data/` 与二进制仍在），按原样重建后重跑了受影响的步骤；重建的 `config.yaml` 与两份脚本内容与回收前
一致（`hello_sh` 的两条参数是回收之前就已加进去的那份），因此 ④ 与 ⑤ 的两条 201 是重建之后重新取得的。
