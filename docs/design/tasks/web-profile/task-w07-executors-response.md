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
   - `path_display`: 脚本/产物的展示写法。workspace 内给相对写法（沿用 `ScriptRel`/`ProgramRel`），
     越界给绝对路径原样；两者都没有（http 档位）时该键**省略而不是空串**。
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

（待补：落地的接口 / 与本卡写法的差异 / 验证证据 / 手工验收 / 缺陷 / 未覆盖项）
