# TASK-W08　控制台的档位管理页

- 所属阶段：M3 界面
- 依赖任务：TASK-W06、TASK-W07
- 涉及文件：`web/src/views/ProfilesView.vue`（新增）、`web/src/components/executors/ProfileForm.vue`（新增）、`web/src/api/executor-profiles.ts`（新增）、`web/src/api/executors.ts`、`web/src/composables/usePermission.ts`、`web/src/views/SettingsView.vue`、`web/src/content/job-template.md`、路由与导航注册处
- 预计规模：大

## 1. 任务目标

在控制台里交付档位的建/改/删界面，并把"改档位要重启进程"这句已经过时的说明换掉。
本卡结束时，一个 ops 身份可以在界面上从零建出一条 python 档位并提交任务跑通，
低档位身份看不到入口、直接调 API 仍是 403。

## 2. 背景与当前问题

设置页现在是一段只读列表（`web/src/views/SettingsView.vue:100-117`），
文案明写"档位写在配置的 `executors.commands` 里，改完要重启进程；这一节只是把
'这台机器现在能执行什么'说清楚"。W05/W06 之后这句话不再成立。

前端已有的三个可照抄形态：

| 先例 | 抄什么 |
| --- | --- |
| `web/src/views/GroupsView.vue:52-66` | `permission.can(...)` 决定入口、`describeError` 统一回显 `message + details`、改动后 `invalidateQueries` 连带刷新任务列表与统计 |
| `web/src/components/groups/GroupForm.vue` | 表单校验（与后端同一条正则在前端先判一遍）、`dirty`/`canSave`、未注册实体的退化只读态 |
| `web/src/components/jobs/JobForm.vue:89-106` | 档位数据源就是 `GET /executors`，`profiles` 按 `enabled` 退化、`canSubmitExecutor` 只决定"名字在不在下拉里" |

任务表单不需要改数据源：它已经在读 `GET /executors`（`JobForm.vue:89-92`），
W07 之后新条目自然出现在下拉里。

## 3. 要实现的功能

### 3.1 落点与导航

新增独立视图 `ProfilesView.vue`（路由与导航项照 `GroupsView` 的注册方式），
布局沿用分组页的"左列表右编辑"。理由：档位表单比分组表单长得多
（`args` 是一张表、http 那一组字段另算），塞进设置页会把设置页变成第二个编辑页。

`SettingsView.vue` 的只读区块保留，改两件事：文案按新事实重写、
末尾加一个"去档位管理"的入口（低档位隐藏）。**不做第二套表单**。

> 页面落点是设计文档 §12 的 P3，写卡时取的即"独立视图"；如果评审要改成设置页内改造，
> 只影响本节与 §3.5，其余不变。

### 3.2 列表

每行显示：`key`（等宽）、`kind`、`runtime`/`script`（`path_display`）、
`available` 徽章 + `reason`（不可用时展开）、`timeout`、`max_parallel`、
`source`（`config` / `store` 两种徽章）、`editable` 时才显示编辑与删除按钮。
`degraded` 行要显式标"与配置同名，未生效"并把 `reason` 摊开——这条是本设计里最容易让人困惑的状态。

### 3.3 表单

| 字段 | 控件 | 规则 |
| --- | --- | --- |
| `name` | 文本 | 前端先判 `^[A-Za-z0-9_-]{1,64}$`（与后端同源，照 `GroupForm.vue` 的做法） |
| `kind` | 单选三档 | 新建后不可改（W06 §3.3 的 400 对应一条界面提示："改类型请删除后重建"） |
| `runtime` | **下拉** | 选项来自 `GET /executors` 顶层的 `runtime_allow`（W07 §3 第 4 条）；拿不到时退化成文本输入并原样显示后端 400 |
| `script` / `program` / `fixed_args` / `cwd` | 文本 / 多行 | 前端**不做**路径合法性判断（越界是本期的合法取值，判断权在后端与 `Probe`） |
| `args` | 可增删行的表 | 列：name / required / default / pattern / secret / allow_dash；`name` 判 `^[a-z0-9_]+$` |
| `args_render` | 多行 | 提示"只能引用上面声明过的参数名" |
| `env` | 键名列（**值不可回显**） | 后端不返回取值（`SettingsView.vue:61-64` 钉住的口径），编辑时留空表示"不改" |
| `env_allow` | 标签输入 | 大写键名 |
| `timeout` / `max_parallel` / `retry_on_exit` | 数字 / 文本 | `timeout` 上限取顶层 `max_timeout`，与任务表单同源（`JobForm.vue:95`） |
| http 那一组 | 折叠区 | 仅 `kind=http` 显示；`allowed_hosts`、`expect_status`、`capture_response` 按 W06 的 DTO 一一对应 |

提交前不做"能不能跑"的判断，保存成功后的响应（含 `available`/`reason`）直接回显到列表——
探测失败**不阻止保存**这条反直觉口径要在界面上说清（表单底部一行固定说明）。

### 3.4 删除

确认弹窗必须写清三件事（照 `GroupsView.vue` 的删除弹窗体例）：
默认会"把该类型的所有待执行与暂停中的任务置为暂停"；正在执行的那条**不会被中止**；
之后恢复它会因找不到处理函数而失败。`?jobs=block` 作为"先别删，让我看看还有没有任务"的选项。

### 3.5 权限与能力表

`web/src/composables/usePermission.ts` 的 `Capability` 与 `REQUIRED_ROLE`（8-40）加三项，档位都是 `ops`：

```ts
'executor.profile_create': 'ops',
'executor.profile_update': 'ops',
'executor.profile_delete': 'ops',
```

该文件第 4 行的注释就是在提醒"与 `setupRoutes` 同步"，本卡改它时必须一起核对
W06 的路由档位确实是 `core.RoleOps`。

### 3.6 文案同步

- `SettingsView.vue:100-104` 那段重写为"档位可以来自配置，也可以来自档位管理页；
  页面改的立即生效并活过重启，配置里的那部分仍然要重启"。
- `web/src/content/job-template.md:261-276` 那一节的三条前提里"档位已声明：改 `executors.commands`
  需要重启进程，页面上不能新增或编辑档位"要改成新事实（保留"改 yaml 仍需重启"这半句）。
  这份文件是 `?raw` 编译期内联进前端的（`web/src/views/TemplateView.vue:14`），改完要重新构建才见效。

## 4. 实现步骤

1. `web/src/api/executors.ts` 的类型补 W07 的四个字段与顶层 `web_enabled`/`runtime_allow`。
2. 新增 `web/src/api/executor-profiles.ts`：`createExecutorProfile` / `updateExecutorProfile` /
   `deleteExecutorProfile`（错误处理沿用 `web/src/api/client.ts` 的 `ApiError{status,code,details}`，不自己拼 fetch）。
3. `ProfileForm.vue`：先做 script/binary 两组字段，http 折叠区第二步补。
4. `ProfilesView.vue`：列表 + 左右布局 + 删除弹窗 + 权限位。
5. `usePermission.ts` 三个能力；`SettingsView.vue` 文案与入口。
6. `job-template.md` 文案。
7. `npx vue-tsc --noEmit` 与 `npm run build`，然后按 §5.3 在浏览器里实测。

## 5. 测试要求

1. 类型检查与构建：`cd web && npx vue-tsc --noEmit && npm run build`。
2. 后端侧的配套用例（W06 已覆盖 API，本卡补"前端契约"那一条）：
   把 `ProfileForm` 提交体的固定样本进一条 Go 用例，断言字段与 `ExecutorProfileRequest` 一一对应
   （防止前后端各写一份 DTO 而漂移）。
3. 浏览器实测（用内嵌形态 `go run -tags dashboard ./cmd/server` + 环境变量把 store 指到临时目录）：
   1. ops 登录 → 档位管理页 → 新建一条 `exec.ui_py`（python + `exec-workspace/scripts/py_hello.py`）→
      保存后列表出现该行且 `available` 为绿。
   2. 任务页下拉里立刻能选到 `exec.ui_py`（**不重启**）→ 提交 → 状态转 `success`。
   3. 编辑同一条，改 `timeout` → 保存 → 重新打开表单值仍在。
   4. 编辑同一条，试图改 `kind` → 控件不可用或后端 400 原文可见。
   5. 新建一条 `script` 指向不存在的路径 → 保存**成功**，列表显示不可用与 `reason`。
   6. 新建一条与 yaml 同名 → 冲突提示可见（409），表单不关闭。
   7. 删除带待执行任务的档位 → 弹窗三条说明齐全 → 删除后那条任务在列表里是 `paused`。
   8. 退出登录换 operator 账号 → 管理入口不可见；直接调 API 是 403（用 curl 复核，不能只看界面）。
   9. 设置页 `source` 徽章与"去管理"入口正确；`config` 行没有编辑按钮。
   10. 台账页（S08）的动作下拉里能选到三个 `executor.profile_*` 动作并查得到行。
4. 秒级现象（第 2 条的状态流转）用 REST 轮询取证；界面侧受隐藏页节流影响的项一律标"未观测"，
   不要靠推测写成"已验证"。

## 6. 完成标准（DoD）

- [ ] 新视图可建/改/删，任务表单无需重启就能看到新档位（§5.3 第 1-2 条实测通过）。
- [ ] 三个能力键进 `usePermission`，且与 W06 路由档位逐条核对过。
- [ ] `config` 来源的行没有任何编辑入口，`degraded` 行有明确状态说明。
- [ ] `env` 取值不回显（前后端都不出现），表单里"值不可回显"有说明文字。
- [ ] 探测失败不阻止保存这条口径在表单与列表都写明了。
- [ ] 删除弹窗三条说明齐全，且默认策略与 W06 一致。
- [ ] `SettingsView.vue` 与 `job-template.md` 的过时文案已替换。
- [ ] `npx vue-tsc --noEmit`、`npm run build`、`go build -tags dashboard ./cmd/server` 全绿；图标全部来自 lucide。

## 7. 验收方式

```bash
cd web && npx vue-tsc --noEmit && npm run build
cd .. && go build -tags dashboard ./cmd/server
```

§5.3 十条逐条记录，做不到的按"未观测"标注并写清为什么。

## 8. 不在本任务范围

- 不做脚本文件管理（上传/编辑/预览，S-4）。
- 不做"改前改后对比"或版本历史（S-2）。
- 不做绝对路径的界面遮蔽（S-3）：本期如实显示 `path_display`。
- 不改任务表单的字段渲染逻辑（它已按档位 `args` 工作，`JobForm.vue:89-114`）。
- 不做 `executors` 全局参数（`required_role`、`max_timeout`、`concurrency`）的界面编辑。

## 9. 风险与回滚

| 风险 | 说明 | 处置 |
| --- | --- | --- |
| 前后端 DTO 漂移 | 表单回填即失败，或提交的字段后端不认 | §5.2 那条 Go 用例 + W07 §5.4 的往返用例 |
| 把前端隐藏当权限 | 用户以为低档位"看不到就安全" | §3.5 + §5.3 第 8 条要求 curl 复核服务端 403 |
| `runtime_allow` 没实现，下拉退化成文本 | 解释器拼错要到保存后才看到 400 | 退化路径是设计内行为，但要确认 400 原文可见 |
| 隐藏页节流导致误判"没生效" | 记成缺陷或记成功都是错的 | §5.3 末条的"未观测"口径 |

回滚：纯前端 + 一处能力表，退回删视图与 API 文件、还原 `SettingsView.vue` 文案即可，
后端不受影响（W06/W07 的端点仍然可用）。

## 10. 实现记录（执行时补写）

### 10.1 落地的接口与文件

后端（本卡前置，收掉 W07 登记的 D-0702）：

- `api/handlers_executor_profiles.go`：`ExecutorProfileRecordResponse`（= `core.ExecutorProfileRecord`
  外加 `env_keys`）与 `GetExecutorProfile`；`UpdateExecutorProfile` 增加"`record.Env == nil` 就取
  `existing.Env`"这一条（handler 位置在第 316-322 行附近，`immutableFieldChange` 之后、
  `profileCommand` 之前）。
- `api/server.go`：`profiles.GET("/:name", s.RequireRole(core.RoleOps), s.GetExecutorProfile)`，
  分组注释从"三个写端点"改成"查增改删"。

前端：

- `web/src/api/types.ts`：`ExecutorProfileRecord` / `ExecutorProfileArgRecord` /
  `ExecutorProfilePositionalRecord` / `ExecutorProfileRecordResponse` / `ExecutorProfileDeleteResult`，
  另把 `AUDIT_ACTIONS` 从 21 项补到 24 项（三个 `executor.profile_*` 动作，设计文档 §6.7 归本卡）。
- `web/src/api/executor-profiles.ts`：读定义 + 建 + 改 + 删四个封装，全部走 `client.ts` 的 `request`。
- `web/src/api/keys.ts`：`queryKeys.executorProfile(name)`。
- `web/src/composables/usePermission.ts`：三个 `executor.profile_*` 能力，档位都是 `ops`。
- `web/src/components/executors/ProfileForm.vue`（新）、`web/src/views/ProfilesView.vue`（新）。
- `web/src/router/index.ts`：`/profiles` 一条路由（`meta {title:'档位', icon: Boxes, minimumRole:'ops'}`），
  侧边栏由路由表派生，没有第二处导航要改。
- `web/src/views/SettingsView.vue`：文案换成两份来源的新事实 + 来源/降级徽章 + "去档位管理"入口（ops 才给）。
- `web/src/content/job-template.md` §12：三条前提里"档位已声明"那行改掉，"改 yaml 仍需重启"这半句保留。

### 10.2 与本卡写法的差异

1. **卡 §3.3 的字段表没有 `positional`**，而 PUT 是整条覆盖——不带它就会被抹掉。
   处理方式：表单把它只读回显（一行说明 + 原值），组装载荷时从载入的那份记录原样带回。
   代价是页面既不能给新档位声明位置参数，也不能改已有的规则；登记为 D-0801。
2. **`env` 的两种显式动作都给了控件**：卡只说"编辑时留空表示不改"。留空 = 整个键不发（后端解释成不改）；
   勾"清空这一组" = 发显式 `"env": {}`；填了新行 = **整组替换**（不是逐键合并）。
   替换这件事写在输入框下面的说明里：取值不回显，页面无从把旧值和新值合并成一份。
   后端没有"改单个键"的口，登记为 D-0802。
3. **http 那一组按 W06 的 DTO 全量给了 11 项**（卡只点名 `allowed_hosts` / `expect_status` /
   `capture_response` 三项），`headers` 用 `Name: v1, v2` 的行文本承载 `map[string][]string`，
   `deny_private_ranges` 做成三态下拉（默认/禁止/允许）以对应 Go 侧的 `*bool`。
4. **载荷按 `kind` 组装**，不属于当前档位类型的键整个不发。依据是 `executor/profile.go` 的
   `checkFieldsMatchKind`（`runtime`/`script` 只属 script，`program`/`fixed_args` 只属 binary，
   `cwd`/`args_render`/`env`/`env_allow`/`retry_on_exit` 只属两种进程档位，http 那一组只属 http）——
   新建时切过一次类型就会留下上一族的值，照直发必吃 400。卡面没写这一条。
5. **列表行是"整行一个按钮 + 行下方独立的编辑/删除按钮"**，不是卡 §3.2 暗示的行内按钮：
   `<button>` 不能嵌 `<button>`，浏览器会把内层那个提到外面去，Vue 的事件与样式都会跟着错位。
6. **`editable=false` 的行也能点选**，右侧退化成一段说明（配置来源 / 与配置同名两种文案各写各的）。
   卡 §3.2 只要求徽章与"editable 才显示按钮"；但说明态需要一个入口，否则"为什么这条改不动"
   只能靠猜。降级那条的文案把"配置那份生效、这份没进登记表"说在前面。
7. 定义读口的 `useQuery` 除 `editable` 之外还判 `canUpdate`：低档位不发这一趟（那个端点与写端点同档，
   发了只是收 403）。能力表没给"读定义"单列一项——界面只在要编辑时才读它。
8. **新建/修改成功后把写响应直接当选中行**（W07 §10 已确立"201/200 响应与列表行是同一个结构体"），
   不再回列表里找那一条；找不到竞态的余地，也省一次 `List()`。
9. 表单底部那行"探测失败不阻止保存"是固定说明，不随状态变化；保存成功后的
   `runtime_ok=false` 用 toast 直说"已保存，但这台机器现在跑不了：<reason>"，列表那一行同时变红。

### 10.3 验证证据

- `go test ./api -run 'TestExecutorProfile|TestProfileFormContract|TestExecutorProfiles' -count=1` 绿，
  25 条 `--- PASS`（本卡新增 11 条：定义读口 6 + 契约 5，其中契约首条含 3 个 kind 子用例）。
- 全仓 `go test ./... -count=1 -race` 绿：api 135.777s / executor 22.282s / core 12.302s /
  cmd/server 5.723s / store/sqlite 3.962s。
- `go build ./...`、`go vet ./...`（exit 0）、`go build -tags dashboard ./cmd/server`、
  `cd web && npx vue-tsc --noEmit`（exit 0）、`npm run build` 全过。图标只有新增的 `Boxes` 一枚（lucide）。
- 契约用例（卡 §5.2）用**三份样本**而不是一份：`buildPayload` 按 kind 组装，一份载荷里同时出现
  `script` 与 `url_template` 是不合法的写法。两个方向都断言——
  样本键必须是 DTO 认识的键（`DisallowUnknownFields` 解码，与 `bindProfileRequest` 同一条判据），
  DTO 的键（反射 `core.ExecutorProfileRecord` 的 json 标签）必须都出现在样本里，
  服务端专属的 `created_at`/`updated_at` 单列一份白名单。

**变异反向验证三处**（各自变红后原样恢复并复跑）：

| 变异 | 变红的用例 |
| --- | --- |
| 样本里把 `allow_dash` 写成 `allowDash` | 契约首条 binary 子用例 + `ProcessSampleCarriesTheValues`（`json: unknown field "allowDash"`） |
| 样本里删掉 `cwd` 一行 | `TestProfileFormContract_DTOFieldsAllAppearInFormSamples`（`Should be empty, but was [cwd]`） |
| 定义读口不做 `record.Env = nil` | `TestExecutorProfileRecord_ReturnsTheDefinition`（响应里出现 env 取值 canary） |

env 的两条语义另有真进程证据：不带 `env` 的 PUT 之后文件里仍是
`{"LANG_PACK":"zh","REPORT_HOME":"/srv/report"}`，`"env": {}` 的 PUT 之后这个键整个消失。

### 10.4 真实进程与浏览器实测（`%TEMP%\w08-smoke`，跑完已删）

`-tags dashboard` 内嵌形态，端口 8123，store / 档位文件 / 产物 / 观测库全在临时目录；
`executors.enabled` 与 `web_enabled` 都开，`runtime_allow: [python, node]`，
配置侧一条 `cfg_py`，账号 `ops1`(ops) / `operator1`(operator) / `admin01`(admin) + 一份静态 machine token。

REST 侧 16 步全绿（浏览器步骤的取数层，先跑一遍确认后端在场）：建 `exec.ui_py` 201 且 `runtime_ok:true`、
`GET /executors` 两行、不重启提交并跑到 `success`（输出正文第二行 `runtime=python 3.13.2`）、
读定义 `env_keys:["LANG_PACK","REPORT_HOME"]` 且响应体搜不到取值、改 `timeout` 为 90s 之后
文件里 env 仍在、换 `kind` 400 原文点名 `delete it and create a new one`、指向缺失脚本的档位 201 +
`runtime_ok:false`、与 yaml 同名 409、默认删除钉住 1 条待执行（那条在 `GET /jobs/:id` 上是 `paused`）、
`?jobs=block` 在无任务档位上 200、operator 与静态 machine 四个方法全 403、
`GET /admin/audit?action=executor.profile_create` 三行且行内搜不到 env 取值。

浏览器 10 步（卡 §5.3）逐条：

1. ops 登录 → 侧边栏"档位"入口在 → 表单填 `ui_py`/python/`scripts/py_hello.py`/一个必填参数 `day` +
   `args_render` + `timeout 2m` → 保存 → 列表出现 `exec.ui_py`，徽章是"可用 + 在线档位"，
   `path_display` 给 `scripts/py_hello.py`。✅
2. 任务页新建抽屉的任务类型下拉里 `exec.ui_py（执行器档位）`立刻可选（不重启）→ 提交 →
   随后 `GET /jobs/:id` 是 `success`。秒级状态流转按 §5.4 走 REST 取证。✅
3. 编辑同一条把 `timeout` 改成 90s → 保存 → 表单仍是 90s，列表那行写 `1m30s`。✅
4. 编辑态下 `kind` 三个单选全部 `disabled`，旁边一行"类型、脚本与程序路径三条改不了：
   要换内核请删除这条档位再建一条新的"；后端 400 原文由第 4 步的 REST 取证。✅
5. 新建一条指向 `scripts/not_there.py` → **201 保存成功**，列表那行"不可用 + 原因
   `script file "scripts/not_there.py" does not exist`"。✅
6. 新建一条与 yaml 同名的 `cfg_py` → 顶部红条给 409 全文，表单不关、草稿仍在
   （`档位名` 输入框仍是 `cfg_py`）。✅
7. 删除带待执行任务的档位 → 弹窗三条说明齐全（钉住待执行 / 不中止正在执行 / 之后恢复会失败）+
   两个策略单选；先选 `block` 提交 → 红条给 409 `1 pending, 0 running and 1 paused job(s)`、
   档位仍在；再按默认 `pause` 提交 → 列表那行消失，那条任务在 `GET /jobs/:id` 上是 `paused`。✅
   （toast 文案在隐藏标签页里观测不到，按既有口径不计入证据。）
8. 退出登录换 `operator1` → 侧边栏六个入口，"档位"不在；直接访问 `/profiles` 被守卫弹回概览；
   `curl` 四个方法复核都是 403（见上面的 REST 段）。✅
9. 设置页：ops 看到"去档位管理"链接、每行"来自配置 / 在线档位"徽章；operator 看不到那个链接；
   设置页两种行都没有编辑按钮（`querySelectorAll` 数过：0 个）。✅
10. 台账页动作下拉 25 项里含 `executor.profile_create/update/delete` 三项；选 `executor.profile_delete`
    → `共 5` 行，表格里能读到 `ops1/ops` 的 `ok 200`、`conflict 409` 与 `operator1/operator` 的 `denied 403`。✅

另外用一份手写的撞名档位文件（`cfg_py` + `seeded_report`）重启进程，验证了两种说明态：
`exec.cfg_py` 两行并存（配置那行"来自配置"、文件那行"与配置同名，未生效"+ 撞名原因摊开），
点降级那行右侧给的是"配置那一份生效、这一份没有进入登记表"的只读文案而不是表单；
`exec.seeded_report` 的编辑表单里 env 只显示键名 `REPORT_HOME`，整页搜不到文件里的取值。

⚠️ 环境事故一条：第一轮冒烟跑到一半，`%TEMP%\w08-smoke` 里的 `config.yaml`、`server.log` 与
`data/*.json` 被本机某个清理器回收（`server.exe` 与 sqlite 文件仍在）。仓库工作树未受影响
（`git status` 复核过），处置是照原配置重建同一目录并把 REST 16 步与浏览器 10 步全部重跑一遍。

### 10.5 缺陷与处置

| 编号 | 内容 | 处置 |
| --- | --- | --- |
| D-0801 | 页面不能声明也不能修改位置参数规则（`positional`）：卡 §3.3 的字段表没给它控件，本卡按"只读回显 + 原样带回"处理，以免一次改超时的保存把它抹掉 | 登记不修。要开这个口得先补卡面字段表；改档位文件后重启仍是唯一路径，界面上那一行说明已写明 |
| D-0802 | 固定环境变量没有"改单个键"或"删单个键"的语义，只有整组替换与整组清空：读口不回填取值，后端也就无法把"我只改 REPORT_HOME"与"我给了新的一组"区分开 | 登记不修，属既有口径（`env` 取值不外露是 §6.8 与本卡 DoD 都钉住的）。界面把"新增即替换整组"写在输入框下面 |
| D-0803 | 在编辑态填了草稿、未保存就点"新建档位"或另一条档位，草稿被静默丢弃（表单按 `:key` 重建） | 登记不修（低）。分组页 `GroupForm` 是同一形态，不是本页新增长出的问题；要改得引入"未保存"确认，属另一卡的判定 |

### 10.6 未覆盖项

- `runtime_allow` 为空时"解释器"退化成自由文本输入这条路径没实测：这份部署的名单始终非空，
  退化分支只有代码在场。卡 §9 风险表点名的正是这条，400 原文可见性由写端点用例保证。
- 能力表三项在本页恒为 true（路由 `minimumRole` 已经是 ops，低档位根本进不来），
  所以 `canCreate/canUpdate/canDelete` 的置灰分支与 `writeHint` 提示未实测，属防御性代码。
- `binary` 与 `http` 两种档位的表单只做过类型检查与契约用例：浏览器里走的是 `script` 那条。
  http 那一组的 `headers` 行文本解析（`Name: v1, v2`）没有页面侧实测。
- 越界绝对路径的表单实测没有做（S-3 现状是原样显示，W07 已在 REST 侧核过）。
- 前端没有单元用例框架（Vitest 未装），表单校验与载荷组装的判据全部走 Go 侧契约用例 + 浏览器实测。
- Linux / macOS 未实跑。
