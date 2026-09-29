# TASK-E16　提交权限、参数掩码与提交期校验接线

- 所属阶段：M5 收口
- 依赖任务：TASK-E04、E08、E13
- 涉及文件：`api/handlers.go`、`api/handlers_executors.go`、`api/security.go`（只读复用）、`api/dto.go`、`cmd/server/main.go`、`core/config.go`（如需补 `Validate`）
- 预计规模：中

## 1. 任务目标

把前面几张卡的纯逻辑接到请求路径上：谁能提交执行器任务、提交时参数是否合法、以及含敏感参数的任务在读取接口里不显示明文。

## 2. 背景与当前问题

现状三条都已核实：

1. `api/handlers.go` 的 `createJobFromRequest` 只做两件事：解析时间、用 `LookupHandler(req.Name)` 判断类型是否注册。参数内容完全不检查——非法参数会等到执行时才失败。
2. 路由档位写死：`jobs.POST("")` 挂的是 `operator`（`api/server.go` 的 `setupRoutes`）。也就是说今天任何 operator 或静态 token 都能提交任务。执行器任务如果沿用这条，等于把"能建任务"变成"能执行命令"。
3. `api/handlers.go` 的 `toJobResponse` 把 `job.Payload` 原样放进响应，`ListJobs`、`GetJob`、创建响应都走它，档位是 `reader`。含口令参数的 payload 会对所有能登录的人可见。

另外 `api/security.go` 的 `RequireRole` 与 `allowRole` 有一段重要行为：**未启用鉴权时一律放行**。这条既有的"本机单用户"假设在本卡要特别对待（见第 3.1 条）。

## 3. 要实现的功能

### 3.1 提交档位

1. 在 `createJobFromRequest` 里，`LookupHandler` 之后判断 `req.Name` 是否以 `exec.` 开头（或更准确：查登记表 `Registry.Lookup(key)` 命中）。命中则要求 `s.allowRole(c, role)`，`role` 来自 `executors.required_role`。
   - `required_role` 合法取值：`operator|admin|ops`（E01 的 `Validate` 已拒绝 `viewer` 与 `machine`）。默认 `admin`。
   - `machine`（静态 token）与 `operator` 同档，因此默认配置下脚本凭据不能提交执行器任务；只有把 `required_role` 显式降到 `operator` 才可以。这层含义要在配置注释里写明（E01 已加，本卡复查一次）。
2. 档位判定需要 `*gin.Context`，而 `createJobFromRequest` 现在不接 `c`。要求把 `c *gin.Context` 作为第一个参数传进去（`CreateJob` 与 `BatchCreateJobs` 两个调用点同时改）。批量接口是逐条独立的，因此"其中一条是执行器任务且档位不够"只让那一条失败（207 混合结果里 code 403），不能整批拒绝——这条要与既有的 `maxBatchCreateSize` 行为一致。
3. 未启用鉴权时：`allowRole` 返回 true，提交照旧通过。**但要在响应/日志里保持可见**：记一条 warn `executor job submitted while authentication is disabled`（每次一条，不要为每个任务重复输出——用包级 `sync.Once` 或按档位名去重）。这条是"默认关闭不是可选加固"这一告警的运行时补充。

### 3.2 提交期参数校验

1. `createJobFromRequest` 里对执行器任务调用 `executor.ValidateSubmission(profile, payload)`（E08）与 HTTP 档位的 URL 参数检查（E15）：
   - 失败 → 400，`message: "invalid executor payload"`，`details` 用 E08 的规范化错误串。
   - 档位不可用（E03 探测失败）→ 400，`details` 带探测原因，与"类型未注册"的 400 区分开（前者说明"能提交但跑不了"，运维需要看到区别）。
   - 超时：用 `EffectiveTimeout` 的结果覆盖 `job.Timeout` 再落盘（对应 D5："不填超时不代表无限"），这样任务详情显示的就是实际生效值。
2. `UpdateJob`（`PUT /jobs/:id`）：如果目标是执行器任务且请求带 `payload`，重跑同一套校验；不允许把非执行器任务改名为 `exec.` 前缀（返回 400）；反之也不允许把执行器任务改成普通 `name`（会造成"绕过档位判定"的误解）。
3. 校验逻辑放在 `api` 层还是 `executor` 层：判定与错误信息生成放 `executor`（纯函数，可测），`api` 只负责 HTTP 状态码映射。登记表通过 E04 的 `WithExecutorRegistry` 注入，未注入时（`enabled=false`）`exec.` 前缀本来就查不到处理器，走既有 400 分支。

### 3.3 敏感参数掩码

1. `Profile` 已支持 `args[].secret`（E02）。新增 `func (p *Profile) MaskPayload(payload []byte) []byte`：把命中的 `args.<name>`、`headers.<k>`、`env.<k>` 的值替换成 `"***"`；只处理声明为 secret 的键，其余原样。
2. 掩码的应用位置（逐个确认，别漏）：
   - `toJobResponse`：`ListJobs`、`GetJob`、`CreateJob`、`RetryJob` 的响应。
   - `GET /jobs/:id/events` 与 `GET /events`：事件 `Data` 目前只含错误与结果摘要，不含 payload，但 `job.failed` 的错误串里可能带上参数值（E08 的 `<redacted>` 规则已在源头处理），本卡补一条测试确认事件流里不出现明文。
   - `GET /jobs/:id/result`：响应里的 `meta` 不含参数；但产物文件内容可能包含被回显的参数（脚本自己打印）。要求：档位含 `secret` 参数时，`/result` 档位从 `reader` 收严到 `executors.required_role`，并在响应里带 `"redaction_note"` 字段说明"输出内容由脚本产生，可能包含敏感值"。这条就是 E07 里预留的那个判档点。
3. 掩码只在输出层，不改存储：`jobs.json` 里仍是明文（设计文档 §8 已声明不做加密）。要求 `docs/deployment.md` 增加一句文件权限建议（E19 落实），并在代码注释里写明，避免有人以为落盘也是掩码后的。

### 3.4 观测

1. `GET /executors` 的响应（E07 实现；本卡补 `required_role` 与 `has_secret_args` 两个字段）里对含 `secret` 参数的档位标注 `has_secret_args: true`，让前端可以据此显示"该档位参数不会回显"。
2. 被权限层拒掉的执行器提交，走 `logAccessRejection`（`api/security.go` 既有），确认日志里带 `required` 与 `have` 两个档位名——如果该函数当前只记路径，本卡补字段。

## 4. 实现步骤

1. 改 `createJobFromRequest` 签名并接入校验与档位判定（一次提交，`CreateJob` + `BatchCreateJobs` 同步）。
2. 改 `UpdateJob` 的重校验与前缀禁止。
3. 加 `MaskPayload` 与输出层的四处应用。
4. 加 `/result` 的收严判档（E07 预留位置）。
5. 补 warn 去重与测试。

## 5. 测试要求

`api/handlers_executors_test.go`、`api/handlers_test.go`、`api/auth_test.go` 风格（该目录已有完整的登录/档位测试脚手架，复用 `newFakeServer` 与真实 JWT 的既有辅助函数）。

1. `TestCreateJob_ExecutorRequiredRole`：表驱动，`required_role` 取 `operator|admin|ops` 三值 × 身份取 `viewer|operator|admin|ops|machine` 五值 → 断言 201/403 矩阵（15 组）。这张表是本卡的核心产出，建议直接写成 `api` 包里的常量表并让 E19 的文档引用它。
2. `TestBatchCreateJobs_MixedPermission`：批量里第二条是执行器任务且档位不够 → 207，第一条成功、第二条 403，其余不受影响。
3. `TestCreateJob_ExecutorValidation`：越界参数 → 400 且 `details` 含参数名；档位不可用 → 400 且 `details` 含探测原因；普通任务不受影响（回归）。
4. `TestCreateJob_TimeoutNormalized`：payload 不填 `timeout` → 落盘与响应里的 `timeout` 等于档位或全局默认值（非 0）；填超大值 → 按 E08 规则报错或夹取（两处规则必须一致，用同一常量）。
5. `TestUpdateJob_ExecutorGuards`：改 payload 时越界参数被拒；`name` 从 `payment_check` 改成 `exec.x` 被拒；反向也被拒。
6. `TestMaskPayload`：`secret` 参数在 `POST /jobs` 的 201 响应、`GET /jobs`、`GET /jobs/:id`、`RetryJob` 四处都是 `***`；非 secret 参数原样；payload 结构异常时掩码不能报错（掩码失败降级为"整个 args 字段掩码"，并在测试里覆盖这条降级）。
7. `TestGetJobResult_StrictRoleForSecretProfiles`：含 secret 参数的档位，operator 读 `/result` → 403（默认 `required_role: admin`），admin → 200 且响应含 `redaction_note`。
8. `TestEvents_NoSecretLeak`：执行失败的 `secret` 参数值不出现在 `GET /events` 的任何字段。
9. `TestAuthDisabled_WarnOnce`：无凭据提交执行器任务 → 201，且 warn 日志只出现一次（连提 5 条）。
10. `TestLogAccessRejection_RoleFields`：403 时日志含 `required`/`have`。

## 6. 完成标准（DoD）

- [ ] 第 5.1 的 15 组矩阵全部通过，且这张表进了文档（E19 引用）。
- [ ] `enabled=false` 时（默认）以上判定全部不触发，既有 `api` 测试零修改通过。
- [ ] 非法参数在提交时就被拒绝，不需要等到执行；错误信息能定位到具体参数。
- [ ] `secret` 参数在四处读取接口 + 事件流里都不出现明文，逐条有测试。
- [ ] 掩码不改动存储内容，这条限制写在代码注释与文档里（不是"实现了加密"）。
- [ ] `PUT /jobs/:id` 不能成为绕过提交档位的通道（改 name 被拒，改 payload 重校验）。
- [ ] 未启用鉴权时的 warn 生效且不会重复输出。

## 7. 验收方式

```bash
go build ./... && go vet ./... && go test ./... -race
go test ./api -run 'Executor|MaskPayload|Secret' -v
```

手工（需要 E09 已完成，能真跑进程）：配一条含 `secret` 参数的档位，用 operator 账号与 admin 账号分别提交并读取，确认 403/201 差异与掩码效果；再用带 `server.auth.token` 的静态凭据提交，确认被拒（默认 `required_role: admin`）。

## 8. 不在本任务范围

- 不做参数加密存储、不做凭据托管（设计文档 §8）。
- 不做按档位分别设档（本期只有全局 `required_role` + 档位内 `secret` 带来的 `/result` 收严）。
- 不做在线修改账号或档位。
- 不改 `POST /jobs/batch` 的 100 条上限与逐条独立语义。
- 不做 IP 级执行器限流（既有登录限流只覆盖登录端点）。

## 9. 风险与回滚

- 风险：脚本参数被脚本自己打印到 stdout，掩码就失效了。这是无法在框架层彻底解决的问题，只能靠 `/result` 的 `redaction_note` 与部署文档提醒。不要把这条写成"已防护"。
- 风险：`createJobFromRequest` 加参数会影响两个调用点与所有相关测试；`api` 层的批量测试用例较多，改签名容易漏。要求先改签名让编译报错逐个定位，再补测试。
- 风险：档位判定从"中间件"下移到处理器，是既有风格的延伸（`handlers_lifecycle.go` 的 `force-pause` 是同类先例），但会让"这个端点要什么权限"在路由表里看不全。要求在该路由行加注释指向本卡，避免后来者只读 `setupRoutes` 就下结论。
- 回滚：本卡是 `api` 层增量，回滚后参数校验退回执行期失败、掩码消失但功能仍可用；`Registry` 注入本身可保留。
