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

## 10. 实现记录（2026-09-30）

落地文件：新增 `api/executors_submission_test.go`（6 条用例，含 §5.1 的 15 组矩阵）与
`api/executors_secret_mask_test.go`（6 条用例）；改动 `api/handlers.go`
（`createJobFromRequest` 的第一个参数改成 `c *gin.Context`、`toJobResponse` 与 `execForResponse`
走掩码出口、`UpdateJob` 的两条守卫）、`api/handlers_executors.go`（`gateExecutorSubmission`、
`resultGuard`、`payloadForResponse`、`warnAuthDisabledOnce`、`submissionRejected`、
`RedactionNote`、`HasSecretArgs`、`RequiredRole`）、`api/dto.go`（`UpdateJobRequest.Name`）、
`api/security.go`（角色比较抽成 `require`，`logAccessRejection` 补 `have` 与 `required`）、
`api/server.go`（`executorAuthWarn` 字段与两处路由注释）、`executor/args.go`（`MaskPayload`、
`MaskSecretText` 与两个取值写法函数）、`executor/profile.go`（`HasSecretArgs`）、
`executor/registry.go`（`EffectiveTimeout`，登记表自己留一份归一化后的 `executors` 配置）、
`executor/proc.go` 与 `executor/http.go`（摘要生成处掩预览）、`executor/register.go`
（`Registration.Unavailable` 的说明改成"提交期就拒"）、`api/handlers_executors_test.go`
（E07 期那条"`required_role` 固定为 null"的断言改成真实取值 `"admin"`，并补
`has_secret_args` 两条断言）。

**没有新增配置键**：§3 用到的 `executors.required_role` 是 E01 定义、E02 校验的键，本卡把它接进判定。

### 与卡片的偏离与补充

1. **判定集中在 `gateExecutorSubmission` 一个函数，三个入口共用**（`POST /jobs`、
   `POST /jobs/batch`、`PUT /jobs/:id`）。顺序固定为：未启用鉴权的 warn → 身份档位 →
   这台机器能否跑 → 请求顶层 `timeout` → payload 校验 → 写入生效超时。
   理由：档位不够时不应该把探测原因与参数规则讲给没有权限的身份听。
2. **请求顶层的 `timeout` 也按档位上限拒**（§3.2 只写 payload 里的 `timeout`）：
   与配置里 `executors.max_timeout`"超过就拒"的姿态一致，两处一条规则。
   两条错误文案分别是 `invalid timeout` 与 `invalid executor payload`，前端能分清是哪个字段。
3. **生效超时写进 `job.Timeout` 并落盘**（§3.2 第 1 条的"覆盖"）。冒烟 B0 与 B7 的现场：
   不填超时任务详情是 `30s`（档位声明值），payload 填 `5s` 就是 `5s`。
4. **`Registry.EffectiveTimeout` 是本卡新增的**：§3.2 点名的这个函数此前并不存在，执行侧只有
   包内私有方法 `Profile.timeoutWithin`。新方法直接复用那个私有方法，入参是登记表里那份
   归一化配置，所以 §5.4 要求的"两处规则一致"落在同一个函数上，而不是两份实现抄同一条规则。
5. **探测不可用的档位提前到提交期拒**：E04 第 10 节与 E07 的代码注释原本都写着"这类档位照常
   可提交、执行期拒绝，留给 E16"。本卡按 §3.2 第 1 条改成提交期 400 并带探测原因
   （`script file "..." does not exist`），文案与"类型未注册"那条 400 分开。
6. **`UpdateJobRequest` 原本没有 `name` 字段**：请求体里多写的 `name` 会被静默忽略，也就是说
   §3.2 第 2 条要防的"改名"在旧代码里根本没有入口。本卡加了 `Name *string`，
   传回与任务相同的值按没传处理，不同值 400；这条判定对所有任务生效——普通任务改成
   `exec.` 前缀同样被拒（冒烟 D5），因为换名字就等于换执行体。
7. **PUT 的重校验走同一个 `gateExecutorSubmission`**，所以它同样包含档位判定与可用性判定
   （§3.2 只写"重跑同一套校验"）。operator 改不动执行器任务的 payload（冒烟 D1 拿 403）。
8. **PUT 的判定顺序是"身份 → 请求合法性 → 任务状态"**：处理器在 `UpdatePending` 之前先读一次
   快照（`snapshotOf`），依次给出 404（任务不存在）、400（改了名字）、403（执行器任务但档位不够），
   之后才轮到 `core` 的 409（不是 pending）。理由与 §3.3 第 2 条把 `resultGuard` 放在参数解析之前
   是同一条：状态码不该把"这条执行器任务跑完了没有"透露给本来就没权限的身份。
   第一轮冒烟不是这个顺序（D 组全部命中 409，改用 `delay: 30m` 的待执行任务才走到 403/400），
   复查时按上面这条改了实现，用例 `TestUpdateJob_ExecutorGuards/判档与改名排在状态检查之前`
   与冒烟 D7 是现在的现场。代价是 PUT 多读一次存储（PUT 是低频操作，且 `GetJob` 本来就是整份读）。
9. **掩码覆盖的 payload 字段是 `args`、`params`、`headers`、`env` 四个**（§3.3 第 1 条只列了
   args/headers/env）：`params` 是 http 档位的参数入口，漏掉它等于 http 档位的凭据不掩。
10. **`body` 字段不掩码，这是结论不是漏项**：请求体是提交者给的业务数据，档位没有"body 里
    哪个键是凭据"的声明能力，整段掩掉会让普通档位的详情页读不出自己提交过什么。
    要给 http 档位传凭据的正确写法是走请求头：把参数声明成 `secret`，再在 payload 的 `headers`
    里用同名键（要出现在档位的 `header_allow` 里）——`headers` 在 §3.3 的掩码字段集合内，
    因此它的值既不会出现在读取响应里，也会在预览被对端回显时按值掩掉（`MaskSecretText` 读的
    四个字段是 `args`/`params`/`headers`/`env`）。已把这条写进 E19 的接口文档清单。
11. **`headers` 与 `env` 的键按归一化名字与 secret 参数名比较**（小写、连字符折成下划线）：
    档位参数写 `api_key`、payload 头写 `API-KEY` 算同一个凭据。反过来档位参数叫 `token`、
    payload 头叫 `X-Token` 不算同一个——名字不同的两个键在配置里就是两件事，框架不做前缀猜测。
12. **掩码只发生在响应层**（§3.3 第 3 条）：存储与执行输入照旧原文，这条限制写进
    `payloadForResponse` 与 `MaskPayload` 的注释，并有 `TestJobResponses_MaskSecretArgs`
    最后一条用例专门检查"快照里的 payload 仍是原文"。`markArtifactPurged` 回写存储时用的
    也是原文摘要，不会把响应层的掩码写进磁盘。
13. **响应出口实际是六处，不是 §3.3 第 2 条列的四处**：`CreateJob`、`ListJobs`、`GetJob`、
    `UpdateJob`、`RetryJob`、`BatchCreateJobs` 都走 `toJobResponse`，掩码加在这一个函数里就
    覆盖六处。卡片列四处是因为它只点名了读接口。
14. **降级两条**（§5.6 要求覆盖）：payload 里某个字段不是对象 → 该字段的值整体换成 `"***"`；
    payload 整体不是对象 → 换成 `{"masked":"***"}`。理由是"解析失败就原样返回"等于把默认
    方向设成泄露。
15. **`/result` 的收严复用 `executors.required_role`，没有新配置键**（§8 明确不做按档位设档）；
    判档在查询参数解析之前，越权请求不会去碰文件系统，也不会从状态码里读出产物是否存在。
16. **`redaction_note` 只在档位声明了 secret 参数时出现**，文案是一个常量、一句英文说明，
    前端可以直接显示。它按 §9 第一条的要求写成"提醒"，不是"已防护"的声明。
17. **warn 去重用的是 `Server` 上的 `sync.Once`**（§3.1 第 3 条写的是包级 `sync.Once` 或按档位名
    去重）：包级变量会让同一进程里的多个 Server 实例互相压制提示，测试也没法各自验证；
    "每次进程启动提示一次"正是这条告警的原意。
18. **执行器关闭时 `required_role` 返回 null**（§3.4 只说补字段）：那时没有任何档位可提交，
    "要什么档位"这个问题不成立；E07 那条"未装配登记表就是默认状态"的用例照旧通过。
19. **`has_secret_args` 只看档位声明，不看某一次 payload 有没有真的带值**（§3.4）：
    读侧门槛不该随任务内容变化，否则同一个任务会因身份不同显示成两种形状。
20. **`logAccessRejection` 补 `have` 与 `required`**（§3.4 第 2 条），两个调用点各带一句用途
    描述（`executor job submission` 与 `execution output of a profile with secret arguments`）。
21. **`submissionRejected` 这个错误类型是必要的**：`core.UpdatePending` 的 apply 回调只能返回
    `error`，而这里要返回带状态码与文案的响应，于是用这个类型把结论带出来，处理器侧用
    `errors.As` 认（`api/handlers.go` 的 `UpdateJob`）。
22. **测试令牌用 `authenticator.IssueSession` 直接签发，不走 `POST /auth/login`**：bcrypt 校验在
    `-race` 下每次登录要几百毫秒，15 组矩阵加掩码用例把 api 包从 100 秒拖到 134 秒。
    签发路径仍然使用配置里的真实账号名与档位，因为 `VerifyAccessToken` 要求令牌里的 `sub`
    能在账号表里对上同一个档位。
23. **`RequireRole` 未启用鉴权时一律放行这条既有行为没动**（§2 提到的"本机单用户"假设）：
    §3.1 第 3 条要求的是让它可见，不是把它改成拒绝。

### 冒烟跑出来的第二条读取路径：输出预览

§1 写的是"含敏感参数的任务在读取接口里不显示明文"，§3.3 第 2 条列了四处 payload 读取点与
`/result` 的收严。第一轮冒烟（C 组）撞出一条卡片没列的东西：**输出预览**。

现场是这样：http 档位的 `url_template` 是 `.../orders/{token}`，`token` 声明为 `secret`，
对端把收到的路径原样回显进响应体（`{"order": "pa55word-in-smoke", ...}`）。这份响应体的尾部
进了 `ExecMeta.Preview`，而预览会

- 随任务快照落进 `jobs.json`；
- 被完成与失败事件带走（`core/scheduler.go` 的事件摘要就是按 `inline_preview` 裁这一份），
  于是 `GET /jobs/:id/events`、`GET /events` 与 WS/SSE 实时推送都带着它；
- 从 `GET /jobs` 与 `GET /jobs/:id` 出去，而这两个端点的路由档位是 `reader`（viewer 及以上）。

结果就是 §3.3 第 2 条为 `/result` 设的那道收严被绕开：档位不够的身份读不到产物正文，
却能在任务详情的 `exec.preview` 里读到同一份内容的前 2048 字节。单元测试没抓到它，因为测试桩
handler 写出的预览本来就不含参数值——这条只有真实档位与真实对端才能暴露。

实现分两层，共用 `MaskSecretText` 这一个纯函数（`executor/args.go`）：

1. **执行侧**：`Runner.Handler` 与 `HTTPRunner.Handler` 那条"摘要落进 job.Exec"的 defer 里，
   先把预览中出现的 secret 取值换成 `***`。这一层管住所有下游——快照、事件、WS/SSE、接口，
   不需要每个读点各写一遍。放在 defer 而不是各分支：每条返回路径都要经过它，早退路径也不例外。
2. **响应层**：`execForResponse` 在把摘要透出去之前再掩一次（`api/handlers.go`）。
   这一层管的是那之前已经落盘的数据文件：旧快照里的预览带着明文，改代码不会自动改写磁盘。

替换规则四条，各有一条用例：原值与它的 URL 路径转义写法都换（http 档位把参数拼进地址，
对端回显的可能是转义后的那一版）；档位里 secret 参数的 `default` 也算凭据（payload 没给值时
执行用的就是它）；先长后短替换，否则短值先换会把长值切成碎片；payload 里的取值是数字或布尔时
按那段 JSON 文本比。掩码后的文本仍然可读（`orders/***`、`--token=***`），不是整段抹掉。

不掩的两处与理由：产物文件（`a<attempt>.out`）保持原文，读它的门槛已经升到提交档位并带
`redaction_note`；产物目录里的 `meta.json` 在那条 defer 之前写，因此也保持原文，它与 out/err
同属磁盘上的产物。§9 第一条的风险照旧成立——把值换个写法再打印（例如 base64）就掩不住，
这一层只覆盖"原样回显"这一种最常见形态，不能写成已防护。

### 复查补记（同日，两条待确认口径收口）

第 8 条与第 10 条登记的两条待确认按默认结论办了：

1. **PUT 的判定顺序改成"身份 → 请求合法性 → 任务状态"**。实现上是把提交档位的判定从
   `gateExecutorSubmission` 里拆出 `gateExecutorSubmissionRole`，让 `UpdateJob` 在
   `scheduler.UpdatePending` 之前先读一次快照（`snapshotOf`）并按 404 → 400（改名）→ 403（档位）
   的顺序判完，再轮到 `core` 的 409。理由与 `resultGuard` 放在参数解析之前同一条：
   状态码不该把"这条执行器任务跑完了没有"透露给本来没权限的身份。
   代价是 PUT 多读一次存储，PUT 是低频操作且 `GetJob` 本来就是整份读，可以接受。
   新现场（单元测试 `TestUpdateJob_ExecutorGuards/判档与改名排在状态检查之前`，冒烟 D7 七条）：

   | 请求 | 结果 |
   | --- | --- |
   | 已到 success 的 exec 任务 + operator 改 payload | 403 `insufficient role`（改之前是 409） |
   | 同一条任务换 admin | 409 `only pending jobs can be updated`（状态仍然最后判） |
   | 已到终态的普通任务 + 传不同 name | 400 `job name cannot be changed`（改之前是 409） |
   | 已到终态的普通任务 + 只改 payload | 409 |
   | 不存在的任务 | 404 |

2. **`body` 不掩码定为结论**，并把"要给 http 档位传凭据就走请求头"的写法写进第 10 条与
   E19 的接口文档清单：`secret` 参数 + `header_allow` 里的同名键，`headers` 在掩码字段集合内，
   值既不进读取响应，也会在预览被对端回显时按值掩掉。

顺带修了冒烟驱动自己的一处假通过：C4 那条"磁盘快照仍是原文"原本是 `SECRET_VALUE in 文件全文`，
而 JSON 存储把 `payload` 按 base64 写，明文本来就不会以裸字符串出现在文件里；
上一轮之所以通过，是文件里还留着改动之前那批任务的明文预览。现在 C4 解码 `payload` 再比
（结论：磁盘上确实是原文），另加 C4b 检查磁盘上的 `exec.preview` 已经是掩码
（执行侧写摘要时替换的结果）。清空 `data/` 之后整组重跑，45 条断言全部相符。

### 验证结果

Windows 本机（`10.0.26200`，go1.26.4 windows/amd64）：

| 命令 | 结果 |
| --- | --- |
| `go build ./... && go vet ./...` | 通过 |
| `go test ./... -race -count=1` | api 100.9s、cmd/server 5.3s、core 12.7s、executor 21.4s 全 ok |
| `go test ./executor ./api -race -count=1`（预览掩码落地后那一轮） | executor 21.9s、api 102.4s 全 ok |
| `go test ./api ./core ./cmd/server -race -count=1`（复查改完 PUT 判定顺序之后） | api 124.5s、core 12.5s、cmd/server 5.5s 全 ok |
| `go test ./... -race -count=1`（全仓，两次） | api 57.3s / 57.7s、executor 21.1s、core 11.8s、cmd/server 5.5s 全 ok |
| `go test ./api ./executor -race -count=2` | api 111.5s、executor 41.3s 全 ok |
| `GOOS=linux GOARCH=amd64`、`GOOS=darwin GOARCH=arm64` 的 `go build` | 通过 |
| `go build -tags dashboard ./...` | 通过 |
| §7 的 `go test ./api -run 'Executor \| MaskPayload \| Secret' -v` | 全 PASS（新增用例名含 `Executor`、`Mask`、`Secret`、`WarnOnce`） |

新增用例清单：

| 用例 | 覆盖的卡片条目 |
| --- | --- |
| `TestCreateJob_ExecutorRequiredRole` | §5.1，`required_role` 三值 × 身份五值 = 15 组 |
| `TestBatchCreateJobs_MixedPermission` | §5.2 |
| `TestCreateJob_ExecutorValidation` | §5.3（五条子用例：越界参数、payload 结构非法、档位不可用、普通任务回归、未知名字） |
| `TestCreateJob_TimeoutNormalized` | §5.4（四条子用例：不填、填小值、payload 超大值、顶层超大值） |
| `TestUpdateJob_ExecutorGuards` | §5.5（五条子用例：档位、重校验、两个方向的改名、相同值） |
| `TestCreateJob_MachineTokenDeniedUnderDefaultRole` | §3.1 第 1 条的"machine 等同 operator" |
| `TestMaskPayload`（`executor/args_test.go`） | §5.6（五条子用例，含两条降级） |
| `TestMaskSecretText`（`executor/args_test.go`） | 输出预览按值掩码的五种取值形态 |
| `TestRunner_SecretValueMaskedInPreview` | 真实进程把参数打印进 stdout 时摘要与产物各自的样子 |
| `TestHTTP_SecretValueMaskedInPreview` | 真实对端回显参数值时摘要与产物各自的样子 |
| `TestJobResponses_MaskSecretArgs` | §5.6 的四处读取响应 + "掩码不改动存储" |
| `TestJobResponses_MaskSecretArgsInPreview` | 上面"输出预览"一节的响应层 |
| `TestGetJobResult_StrictRoleForSecretProfiles` | §5.7（四条子用例，含"判档在参数解析之前"） |
| `TestEvents_NoSecretLeak` | §5.8 |
| `TestAuthDisabled_WarnOnce` | §5.9 |
| `TestLogAccessRejection_RoleFields` | §5.10 |

### 冒烟（Windows 真实服务端 + REST）

除说明另有写明的段落外，冒烟断言 45 条、不符 0 条。
配置在临时目录：`executors.required_role: admin`（最后一段改成 `operator` 再跑一遍）、
三个账号 `worker01`(operator)/`rooter01`(admin)/`keeper01`(ops) 加一个静态 token，
`runtime_allow: [bash]`，三条档位：`hook_secret`（http，`token` 是必填 secret 参数）、
`hook_open`（http，无 secret）、`nightly_missing`（script，脚本文件故意不部署）。
对端仍是 E15 那个本机 Python 服务，它把每个到达的请求记进自己的日志。

**A. 提交档位**

| 身份 | 结果 |
| --- | --- |
| 静态 token（machine） | 403 `insufficient role`，details 带 `requires role admin (executors.required_role)` |
| operator | 403 同上 |
| admin / ops | 201 |
| operator 提交普通任务 `payment_check` | 201（回归，普通任务不受影响） |

**B. 提交期校验与超时**

| 用例 | 结果 |
| --- | --- |
| 档位 `timeout: 30s`，payload 不填 | 任务详情 `timeout: "30s"` |
| payload `timeout: "5s"` | 任务详情 `timeout: "5s"` |
| 顶层 `timeout: "2h"` | 400 `invalid timeout`，`timeout 2h0m0s exceeds the 30s allowed by profile "hook_open"` |
| payload `timeout: "10m"` | 400 `invalid executor payload` |
| payload 写 `{"cmd":"rm -rf /"}` | 400，`payload key "cmd" is not accepted by profile "hook_open" (allowed keys: params, headers, body, timeout)` |
| 提交 `exec.nightly_missing` | 400 `executor profile is not available on this server`，details 是探测原因 |
| `exec.hook_secret` 不填 token | 400，`params.token: required by profile "hook_secret" but not provided` |
| `exec.hook_open` 的摘要 | `{"kind":"http","profile":"hook_open","http_status":200,...,"artifact":"available"}` |

**C. secret 参数掩码与结果端点**

| 用例 | 结果 |
| --- | --- |
| admin 提交 `{"params":{"token":"pa55word-in-smoke"}}` 的 201 响应 | `"payload":{"params":{"token":"***"}}` |
| `GET /jobs?limit=50` 与 `GET /jobs/:id` | 全文没有明文；payload 是 `***`，`exec.preview` 是 `{"order": "***", "auth_seen": false}` |
| `data/jobs.json` | 仍含明文（掩码不作用于存储） |
| operator 读 `GET /jobs/:id/result` | 403，details `profile "hook_secret" declares secret arguments; reading its execution output requires role admin` |
| admin 读同一端点 | 200，`content` 是 `{"order": "pa55word-in-smoke", ...}`，并带 `redaction_note` |
| ops 读同一端点 | 200（档位高于所需） |
| `GET /jobs/:id/events` 与 `GET /events` | 全文没有明文 |
| 无 secret 档位的 `/result` | operator 读到 200 正文，响应里没有 `redaction_note`（收严只针对声明了 secret 的档位） |

**D. PUT 的守卫**（目标是 `delay: 30m` 的待执行任务）

| 用例 | 结果 |
| --- | --- |
| operator 改 payload | 403 `insufficient role` |
| admin 把 payload 改成 `{"cmd":"bad"}` | 400 `invalid executor payload` |
| admin 把 name 改成 `payment_check` | 400 `job name cannot be changed` |
| admin 把 name 传回同一个值 | 200，响应里的 payload 仍是掩码 |
| 普通任务改成 `exec.hook_open` | 400 `job name cannot be changed` |
| 普通任务改 payload | 200 |

**E. 观测**：`GET /executors` 的 `required_role` 是 `"admin"`；`hook_secret` 的
`has_secret_args` 为 `true`，`hook_open` 为 `false`。

**F. 批量**：operator 提交 `[payment_check, exec.hook_open]` → 207，`succeeded:1`、`failed:1`，
`errors[0].index=1`、`code=403`；两条普通任务 → 207 `succeeded:2`。

**G. 换成 `required_role: operator` 重启**：operator 与静态 token 都能提交（201）；
`/executors` 报 `operator`；含 secret 档位的 `/result` 此时对 operator 返回 200 并带
`redaction_note`——这条说明收严用的就是同一个配置值，把提交档位降下来就等于把输出也降下来。

**H. 未启用鉴权**（第三份配置：无 token、无账号、`required_role: admin`、端口 18078）：
连提 5 条执行器任务全部 201，`GET /executors` 正常，日志里
`msg="executor job submitted while authentication is disabled"` 只有一行
（启动时另有 E04 那条 error 级 `executors are enabled while server authentication is disabled`）。

**被拒的请求在日志里长什么样**（A 组与 C 组现场）：

```
level=WARN msg="access denied" who=machine have=machine need="executor job submission" required=admin
level=WARN msg="access denied" who=worker01 have=operator need="execution output of a profile with secret arguments" required=admin
```

### 未验证与遗留

- **`viewer` 身份在冒烟里没有账号**（配置只建了 operator/admin/ops 三个账号）：
  15 组矩阵在单元测试里覆盖了 viewer，结论是它在路由层就被挡住（建任务本身要 operator）。
- （复查后收口两条）PUT 的判定顺序按第 8 条改成"身份 → 请求合法性 → 状态"；
  `body` 不掩码按第 10 条定下来，并给出"凭据走请求头"的写法。两条都已进代码与用例。
- **旧数据文件里已落盘的明文**：payload 与旧任务的预览都在磁盘上；响应层现在会掩，
  但文件本身仍是原文（§3.3 第 3 条的口径，清理靠换数据文件或重跑任务）。
- **WS/SSE 实时推送没有单独观测**：事件文本与 `GET /events` 取的是同一份摘要，
  执行侧掩码之后两者一起变干净；浏览器侧的观测限制与前面卡片同一条。
- `docs/api.md` 的档位矩阵表、`/executors` 两个新字段与 `/result` 的 `redaction_note`
  归 E19 写；已把结论补进该卡 §3.1 与 §3.2。
- E15 登记的两条既有缺陷照旧：重试的产物被后一次尝试覆盖、档位超时在事件里记成 `timeout=0s`。
- Linux/macOS 的真实运行与那一侧的 `-race`（WSL2 缺 gcc），与其他卡片同一条遗留。
