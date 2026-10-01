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

（待补：落地的接口 / 与本卡写法的差异 / 验证证据 / 手工验收 / 缺陷 / 未覆盖项）
