# TASK-W07　`GET /executors` 标出来源、可编辑性与降级

- 所属阶段：M2 接线与端点
- 依赖任务：TASK-W03
- 涉及文件：`api/handlers_executors.go`、`api/executors_response_test.go`（新增或并入既有测试）、`docs/api.md`、`web/src/api/executors.ts`
- 预计规模：小–中

## 1. 任务目标

让唯一的档位读口 `GET /api/v1/executors` 回答四个问题：这条从哪来（配置还是页面）、
能不能改、现在能不能跑、跑的是本机哪个文件。本卡是"档位的真相分两处"（设计文档 §11 第三条风险）
的主要缓解手段。

## 2. 背景与当前问题

`ListExecutorsResponse` / `ExecutorProfileResponse`（`api/handlers_executors.go:341-381`）今天只回
`key`/`name`/`kind`/`runtime_ok`/`reason`/`timeout`/`max_parallel`/`args`/`env_allow`/`has_secret_args`/
`preferred_result_direction`，顶层回 `enabled`/`profiles`/`required_role`/`max_timeout`。
W05 之后表里同时存在两份来源的条目，且可能有降级条目（同名冲突）——
现在这份响应**说不出这三件事**，运维只能靠猜。

这个端点还是前端任务表单的数据源（`web/src/components/jobs/JobForm.vue:89`），
因此它的字段变化会同时影响设置页（W08）与任务表单。

## 3. 要实现的功能

1. 每条 profile 新增：
   - `source`: `"config"` | `"store"`（读 W03 的 `SourceOf`）。
   - `editable`: bool，`web_enabled && source=="store" && !degraded`。
     **由后端算好**，不在前端重复一遍规则（前端只读这个布尔决定是否显示按钮，
     边界仍是服务端 403——`web-console-design.md:767` 那条口径）。
   - `degraded`: bool；为真时 `available=false`，`reason` 必须指名"与 `executors.commands` 的同名档位冲突，未注册"。
     **这批要从 `Registry.Degraded()` 取，不是从 `Profiles()`** —— W03 的落地形态是降级条目从不进注册表键，
     所以 `Profiles()`/`Lookup()`/`Available()` 都看不到它们。接口要把两批合进同一个 `profiles` 数组输出，
     降级那批的 `key` 与生效那条**相同**（同名冲突的本义），前端靠 `degraded` 区分；
     `available` 恒 false，`reason` 用 `DegradedProfile.Reason`（W03 已给默认文案）。
   - `path_display`: 脚本/产物的展示写法。workspace 内给相对写法（沿用 `ScriptRel`/`ProgramRel`），
     越界给绝对路径原样；两者都没有（http 档位）时该键**省略而不是空串**。
     **W02 §3.5 已核实**：判"有没有越界"不能看 `ScriptRel` 的形状（同盘越界它会给 `..\..\` 上跳形式，
     只有 Windows 跨盘符才兜底成文件名），只能用 `withinDirectory(workspace, 绝对路径)`；
     该函数在 `executor` 包内，所以展示逻辑要落在 `executor` 侧的一个只读方法上
     （建议 `(*Profile).PathDisplay() string`），api 层只透传，不自己拼路径。
     ⚠️ 这条会把本机目录结构暴露给 reader 档——设计文档 §7.1 第 3 条已登记为 S-3，
     本卡照现状实现，并在 §8 明确"遮蔽方案不在本卡"。
2. 顶层新增 `web_enabled`: bool（与既有的 `enabled` 并列；`enabled` 仍是 `executors.enabled`）。
   关闭时**仍然给出这个键并回 false**：它是"能不能改"的唯一判据，省略会让前端猜。
3. 既有字段一个不改、不删、不改语义。特别是：
   - `env` 的固定取值仍然不返回（`web/src/views/SettingsView.vue:61-64` 钉住"只列键名"）。
   - `required_role` 为 null 的条件不变（执行器关闭时不给，`api/handlers_executors.go:345-348`）。
   - `max_timeout` 只在 `enabled=true` 时给（同处注释）。
4. 顶层再加 `runtime_allow`: []string，取值就是 `executors.runtime_allow` 原样透出。
   W08 的"解释器"下拉靠它（不透明的文本框会让拼错的解释器走到保存之后才由 400 暴露）。
   `executors.enabled=false` 时**也给**：它是配置事实而不是运行状态，与 `required_role` 那条
   "关闭时不给"的规则不同，理由要在注释里写一句，免得后来人当成不一致顺手"修平"。
5. `ExecutorProfileRequest`（W06 的请求体）与响应结构共用的字段命名必须一致，
   避免"POST 用 `args_render`、GET 回 `argRender`"这种往返不齐。
   一条往返用例守（§5.4）。
6. `docs/api.md` 的档位端点一节补这四个字段与顶层 `web_enabled`/`runtime_allow`，
   并写明"探测结论只代表当前这台机器"这条既有口径（`web/src/views/SettingsView.vue:100-104` 里已经这么说了）。

## 4. 实现步骤

1. 先扩 `Registry` 的访问器（W03 已给 `SourceOf`/`Degraded`，缺 `path_display` 的取值口径时补一个只读方法，
   不要让 api 层去猜 `Profile` 的哪个字段是路径——`script`/`program`/`ProgramPath` 三种情况要收在一个函数里）。
2. 改 `toExecutorProfile`（`api/handlers_executors.go` 里那个转换函数）多收来源与降级两个入参。
3. `ListExecutors` 顶层补 `web_enabled`。
4. 前端类型 `web/src/api/executors.ts` 同步（本卡只加类型定义与一处设置页文案，
   完整的档位管理页归 W08）。
5. 补测试（§5）+ `docs/api.md`。

## 5. 测试要求

1. 三条来源各一例：config 档位 → `source=config`、`editable=false`；
   store 档位 → `source=store`、`editable=true`；冲突降级的 store 条目 → `degraded=true`、
   `available=false`、`reason` 含同名档位那句，且**该键没有被注册**（`LookupHandler` 查不到）。
2. `web_enabled=false` 时：`editable` 恒 false、`source` 仍有值、顶层 `web_enabled=false`，
   其余响应与本卡之前逐字段一致（一条对照用例）。
3. `path_display`：workspace 内档位给相对写法；越界档位给绝对写法；http 档位该键 `omitempty` 生效
   （断言 JSON 里不存在这个键，而不是值为空串）。
4. **往返一致**：拿 §5.1 里那条 store 档位的响应体，去掉 W07 新增的四个字段后作为 PUT 请求体发回去，
   必须 200 且档位内容不变。这条防止"GET 能看到的字段 POST 不接受"。
5. 既有响应字段的回归：`has_secret_args`、`preferred_result_direction`、`args[].pattern` 等
   全部由现有用例守住，**不得为了本卡改动它们的断言**。
6. 手工：起进程 → `curl .../executors | python -m json.tool` 里逐条核对三种来源；
   设置页（未接管理功能之前）能看到新字段不报错。

## 6. 完成标准（DoD）

- [ ] 四个新字段 + 顶层 `web_enabled` 按 §3 落地，既有字段零改动（`git diff` 里没有对既有断言的修改）。
- [ ] `editable` 的规则只在后端一处计算。
- [ ] 降级条目在响应里可见、可解释，且确实没注册 handler。
- [ ] `path_display` 三种情况（相对/绝对/省略）都有用例。
- [ ] §5.4 往返用例绿。
- [ ] `docs/api.md` 与 `web/src/api/executors.ts` 已同步，`npx vue-tsc --noEmit` 通过。
- [ ] `go build ./...`、`go vet ./...`、`go test ./... -race` 全绿。

## 7. 验收方式

```bash
go test ./api -run 'TestListExecutors' -v
cd web && npx vue-tsc --noEmit
```

手工：`curl -H "Authorization: Bearer <token>" http://127.0.0.1:<port>/api/v1/executors`，
预期三种来源各一条，`web_enabled` 在顶层，`env` 取值搜不到。

## 8. 不在本任务范围

- **不做绝对路径的遮蔽**（S-3）。本卡先把来源与路径如实说出来，遮蔽方案要等权限模型定稿；
  这是设计文档 §7.1 已登记的风险，不是本卡的遗漏。
- 不做管理页（W08）。
- 不改写端点（W06）。
- 不新增"某条档位的历史改动"这类只读端点（S-2 的版本历史，本期不做）。

## 9. 风险与回滚

| 风险 | 说明 | 处置 |
| --- | --- | --- |
| 前端把 `editable` 当安全边界 | 隐藏按钮不等于拒绝请求 | §3.1 要求服务端仍判 ops；W08 的能力表与用例守住 |
| `path_display` 泄目录结构 | reader 档可见 | §8 明确登记 S-3；本卡在 `docs/api.md` 里写清这个字段会对谁可见 |
| 新字段与 W06 请求体命名漂移 | 往返不齐，页面回填即失败 | §5.4 往返用例 |
| 既有字段被"顺手规范化" | 设置页与任务表单同时受影响，且不会有人报修 | DoD 第一条要求 `git diff` 里没有既有断言的改动 |

回滚：纯响应字段扩展，退回删字段即可；`Registry` 的访问器可以留着。

## 10. 实现记录（执行时补写）

完成日期：2026-10-01。

### 10.1 落地的接口与位置

| 位置 | 内容 |
| --- | --- |
| `executor/profile.go:195` | `(*Profile).PathDisplay() string`：workspace 内给相对写法、之外给绝对路径、没有文件可指给空串 |
| `executor/registry.go:342` | `ListedProfile{Profile, Probe, Source, Degraded, Reason, Editable}` |
| `executor/registry.go:364` | `(*Registry).List()`：一次读表，生效 + 降级合并成一份按注册键字典序的列表，同键时生效那条在前，`editable` 在这里算好 |
| `executor/registry.go:405` | `(*Registry).RuntimeAllow()`（Normalized 补齐之后的那份名单，返回副本） |
| `api/handlers_executors.go:345-355` | `source` / `editable` / `degraded` / `path_display` 四个字段 |
| `api/handlers_executors.go:374,379` | 顶层 `web_enabled` / `runtime_allow` |
| `api/handlers_executors.go:389,420` | `ListExecutors` 改读 `List()`；`toExecutorProfile(view executor.ListedProfile)` |
| `api/handlers_executor_profiles.go` | `respondProfile` 也改读 `List()` 的那一行（写响应与列表行同一个来源） |
| `web/src/api/types.ts` | `ExecutorProfile` 四个字段、`ExecutorListResponse` 两个字段，注释写明"响应不是请求体" |
| `web/src/views/SettingsView.vue` | 档位一览那段文案改成两份来源；列表 `:key` 带上 `degraded`（同一注册键现在会有两行） |

用例：`api/executors_response_test.go` 8 条、`executor/registry_view_test.go` 6 条。

### 10.2 与本卡写法的差异

1. **`toExecutorProfile` 换成收一个 `ListedProfile`**，不是卡 §4.2 说的"多收来源与降级两个入参"。
   按卡面写法 `ListExecutors` 要对每一行连调 `Profiles()` + `Available()` + `SourceOf()`，
   而登记表每个方法各做 `Load()` 一次——W06 之后运行期真的会整表替换，
   三次读就可能读到三张表（"这条的来源查不到、那条的探测来自上一张表"）。
   于是新增一次读表的 `Registry.List()`，`Profiles()` / `Degraded()` 原样保留。
2. **`editable` 的规则落在 `executor` 侧**（`List()` 里算），api 与前端都只是搬运，
   DoD 的"只在后端一处计算"由这个位置保证。顺带得到一条更强的判据：
   写端点的 201 响应与列表行是同一个结构体，可以直接比较（有用例守）。
3. **降级那条的 `reason` 用冲突说明、`runtime_ok` 恒 `false`**，即便它指向的脚本其实存在
   （`ListedProfile.Probe` 里那条"能跑"的结论同时存在，接口不说）。
   取舍理由：两条事实同时成立时，"为什么这条没生效"是读者更要紧的那一件。
4. 卡 §5.1 要"降级那个键 `LookupHandler` 查不到"：api 层看不到调度器的处理函数表
   （这里的 `core.Scheduler` 没注册任何 exec 处理函数），改成两条等价判据——
   `Registry.SourceOf(key)` 仍是 `config`、`Lookup(key)` 给出的仍是配置那条的 `url`。
   "没注册处理函数"本身由 W05/W06 的 `cmd/server` 用例守住（启动日志的 `degraded=1` 与不注册）。
5. **卡 §5.4 的"把响应体去掉四个新字段当 PUT 请求体发回去"落不了地**，因为它的前提不成立：
   响应体从来不是请求体的子集——`key` / `runtime_ok` / `reason` / `has_secret_args` /
   `preferred_result_direction` 在本卡之前就是只给展示用的键，而 `runtime` / `script` /
   `args_render` / `env` 这些**定义字段一个都不在响应里**；
   写端点（W06）又按 `DisallowUnknownFields` 把多余键一律拒掉，那种发回必定 400。
   本卡改成本条真正要防的三件事：新键不与请求体键名撞（反射比键名）、
   写响应与列表行逐字段相等、把列表行当请求体发回去必须 400（并留下文案说明为什么）。
   由此给 W08 留了一条 D-0702。
6. **既有断言动了一处**：`TestListExecutors_DefaultStateIsNotAnError` 的整对象 `JSONEq`。
   本卡 §3.2 与 §3.4 要求 `web_enabled` / `runtime_allow` 在关闭时也给出，
   整对象比较的期望值只能跟着长；断言用意（关闭不是错误、两种装配形态都返回 200）没变。
   两行期望值之间现在多出的唯一差别是 `runtime_allow`——没装配登记表就没有一份配置可读，给 `[]`。
7. 设计文档 §6.8 的 `editable` 公式少了 `!degraded`（与本卡 §3.1 不一致），已在文档里补齐。
8. `path_display` 的省略情形比卡面多一种：`program` 写成 `runtime_allow` 里的程序名时也没有路径可指。

### 10.3 验证证据

- `go test ./api -run 'TestExecutorsResponse|TestListExecutors' -count=1` 绿（本卡 8 条 + 既有 4 条）。
- `go test ./executor -run 'TestPathDisplay|TestRegistryList|TestRegistryRuntimeAllow' -count=1 -race` 绿。
- 全仓 `go test ./... -count=1 -race` 绿：api 213.310s / cmd/server 5.832s / core 12.557s / executor 24.298s / store/sqlite 3.494s。
- `go build ./...`、`go vet ./...`、`cd web && npx vue-tsc --noEmit`（exit 0）、`npm run build` 全过。
- `List()` 的"一次读表"有一条并发用例（20 轮 `List()` 与 `ApplyStore` 交错，`-race` 下无竞争报告）。

**变异反向验证四处**（各自变红后原样恢复并复跑）：

| 变异 | 变红的用例 |
| --- | --- |
| `Editable` 不再看 `web_enabled`（只看来源） | `TestRegistryList_EditableFollowsWebEnabled` + `TestExecutorsResponse_WebDisabledKeepsEverythingButEditable` |
| `PathDisplay` 直接信 `ScriptRel` 的形状（去掉 `withinDirectory`） | `TestPathDisplay_Kinds` 的两条越界用例 + `TestExecutorsResponse_PathDisplay`（越界那条给出 `../elsewhere/...`） |
| `List()` 不把降级条目合进来 | `TestExecutorsResponse_TwoSourcesAndDegradation` + `TestRegistryList_MergesSourcesAndOrders` |
| 降级那条改用探测结论而不是冲突说明 | `TestExecutorsResponse_TwoSourcesAndDegradation`（`runtime_ok` 变 true、`reason` 变空） |

### 10.4 真实进程冒烟（`%TEMP%\w07smoke`，跑完已删）

配置：执行器与在线管理都开，`workspace` 指向仓库 `exec-workspace`，`runtime_allow: [python, node]`，
配置侧两条（`py_hello`、`cfg_health`），档位文件手写三条（`store_report` 相对、`py_hello` 与配置撞名、
`store_outside` 指向临时目录里的 python 脚本）；store / 产物 / 档位文件全在临时目录，端口 18914，
账号 `w07_ops`(ops) 与 `w07_viewer`(viewer)。用 `-tags dashboard` 的内嵌形态起，顺带给界面走查用。

1. 启动日志 `msg="executor handlers registered" total=4 registered=4 unavailable=0 degraded=1`
   ——撞名那一条按预期只进展示面。
2. `GET /executors`（ops）逐条核对五种状态：`cfg_health` 是 `config` + `editable:false` 且
   **没有 `path_display` 这个键**；`store_report` 是 `store` + `editable:true` +
   `path_display:"scripts/py_hello.py"`；`store_outside` 的 `path_display` 是绝对路径
   `C:\Users\...\Temp\w07smoke\outside\outside_report.py` 且 `runtime_ok:true`；
   `exec.py_hello` 出现两行——配置那行 `runtime_ok:true`，文件那行 `degraded:true` +
   `runtime_ok:false` + `reason:"profile \"py_hello\" is already declared in executors.commands, the stored one is not registered"`。
   顶层 `web_enabled:true`、`runtime_allow:["python","node"]`、`required_role:"admin"`。
3. 档位文件里 `store_report` 写了 `env: {W07_FIXED: canary-w07-value}`，整个响应正文搜不到
   `canary-w07-value`（文件里那条仍在，值不外露）。
4. `viewer` 档读同一个端点：行数、顺序、每行的 `source/degraded/editable/path_display` 与 ops 看到的完全相同
   （越界的绝对路径对 reader 也可见——这正是 §8 登记的 S-3 现状，本卡把它如实记在文档里）。
5. 往返一致：`POST` 一条 `smoke_new` → 201 响应体与随后 `GET /executors` 里那一行**逐字段相等**。
6. 重启一次（换 embedded 前端产物）：`total=5 ... degraded=1`，页面上 `exec.py_hello` 两行并存。
7. 界面：`/settings` 的"执行器档位"一节渲染 6 行（含撞名的两行），文案已换成两份来源的说法，
   控制台无任何消息。⚠️ 产物是 `vite build` 的生产构建，Vue 的重复键警告本来就被剥掉，
   所以"无消息"不构成 `:key` 修复的证据；结构性证据是同一注册键的两行都独立渲染出来了。

### 10.5 缺陷与处置

| 编号 | 内容 | 处置 |
| --- | --- | --- |
| D-0701 | 设计文档 §6.8 的 `editable` 公式缺 `!degraded`，与本卡 §3.1 不一致 | 已修（文档补齐，并写明这条判断只在 `Registry.List` 有一份） |
| D-0702 | `GET /executors` 说不出档位的**定义**字段（`runtime` / `script` / `program` / `fixed_args` / `args_render` / `cwd` / `env` 的键名 / `retry_on_exit` / http 细则），因此这份响应不能当编辑表单的回填来源 | 登记，归 W08：要么页面自己留一份记录，要么补一个"单条档位详情"读端点。本卡不扩字段——§3.3 要求既有形状零改动，而新增定义字段会把 D5 的路径与配置内容透得更开 |
| D-0703 | 前端产物是生产构建，Vue 的开发期警告被剥掉，`list_console_messages` 为空不能证明 `:key` 唯一性 | 记为验证口径限制（不修）；真正的判据是用例与页面上两行并存 |

### 10.6 未覆盖项

- 没有登记表（`executors` 为 nil）时 `runtime_allow` 给 `[]`：只有单元用例，未冒烟（那种装配形态只出现在测试里）。
- binary 档位的 `path_display` 只有单元用例：配置侧写不出"workspace 之外的产物路径"（严格模式会拒），
  只有页面那侧能建出这种档位，端到端要等 W08 的管理页。
- Linux / macOS 未实跑：`PathDisplay` 的越界判定依赖 `withinDirectory` 的平台分支
  （Windows 不区分大小写），跨盘符与符号链接两种写法都没在真机上验过。
- 绝对路径的遮蔽（S-3）不在本卡，§8 已明确；本卡只把暴露面写进 `docs/api.md` 与冒烟证据。
- 前端只改了类型定义与设置页两处文案 + 一处 `:key`；管理页归 W08。
