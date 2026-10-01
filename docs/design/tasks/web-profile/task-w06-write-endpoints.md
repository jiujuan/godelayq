# TASK-W06　三个写端点：页面增删改档位

- 所属阶段：M2 接线与端点
- 依赖任务：TASK-W03、TASK-W04、TASK-W05
- 涉及文件：`api/handlers_executor_profiles.go`（新增）、`api/handlers_executor_profiles_test.go`（新增）、`api/server.go`、`api/audit.go`、`docs/api.md`
- 预计规模：大

## 1. 任务目标

交付 `POST/PUT/DELETE /api/v1/executors/profiles[/:name]` 三个写端点：
校验 → 探测 → 落盘 → 热生效 → （删除时）把该类型未终态任务置 `paused`。
本卡结束时，不重启进程就能建出一条可用的任务类型。

## 2. 背景与当前问题

后端前置已经齐了：W02 给得到"单条校验 + 探测"，W03 给得到"整表热替换"，W04 给得到"摘除与按类型暂停"，
W05 给得到"两份来源与冲突规则"。缺的是把这条链挂到 HTTP 上，并且**接上三道门槛**：

1. 校验：`executor.BuildProfile(cmd, ec, executor.PathAnywhere)` + `executor.Probe`。
2. 鉴权：往执行面注入命令，比"删组"（`api/server.go:288` 的 admin）更危险，取 ops 起步
   （设计文档 D10；现成的 ops 写法见 `api/server.go:293`）。
3. 审计：`api/audit.go:97-118` 的 `auditActions` 必须加行，否则动作词落进 `other`
   （`api/audit.go:260-270` 那条"宁可记 other 也不静默丢行"的口径就是为这种情况准备的）。

形态上照抄分组端点：路由分组 + 未装配守卫（`api/server.go:280-289`、
`api/handlers_groups.go` 的 `requireGroupStore`），测试照 `api/handlers_groups_test.go:18` 的 helper 形状。

## 3. 要实现的功能

### 3.1 路由

```go
// 档位的在线管理。web_enabled=false 时这一组全部 503（设计文档 D2）。
profiles := api.Group("/executors/profiles", s.requireExecutorProfiles())
{
    profiles.POST("",     s.RequireRole(core.RoleOps), s.CreateExecutorProfile)
    profiles.PUT("/:name", s.RequireRole(core.RoleOps), s.UpdateExecutorProfile)
    profiles.DELETE("/:name", s.RequireRole(core.RoleOps), s.DeleteExecutorProfile)
}
```

只读端点 `GET /api/v1/executors`（`api/server.go:275`，路由档位 reader）**本卡不动**，扩展归 W07。

### 3.2 依赖注入（接口定义在消费方）

`api` 包内声明三个窄接口，由 `cmd/server/main.go` 把实现传进来（照 `WithGroupStore` 的 Option 形状）：

| 接口 | 方法 | 实现方 |
| --- | --- | --- |
| `profileStoreAPI` | `List`/`Get`/`Save`/`Delete` | `core.JSONFileExecutorProfileStore`（W01 落地的名字，比本卡原写的 `JSONFileProfileStore` 多一段 `Executor`） |
| `profileApplier` | `ApplyStore([]executor.StoreEntry) error` | `*executor.Registry`（W03） |
| `profileRegistrar` | `RegisterHandlerClass`/`UnregisterHandler`/`PauseByHandlerKey` | `*core.Scheduler`（后两个是 W04） |

未注入任一个 → `requireExecutorProfiles()` 回 503，文案与 `requireGroupStore` 同一体例
（"未装配"与"没打开"要在响应里可区分，但两者都拒绝，不返回半套结果）。

**W05 交接过来的两件事实（本卡必须自己补上）**：

1. W05 只把"读档位文件"接进了启动路径——`runtimeDeps.newExecutorProfiles` 交回的是
   `[]core.ExecutorProfileRecord`，**没有交回存储实例**，`api` 侧也还没有任何
   `WithExecutorProfileStore`/`WithExecutorProfileApplier`（W05 卡 §3.6 的那步执行时判定为
   "没有读取方就不先注入"，记在 W05 §10.2）。所以写端点要自己把存储实例建到 `run()` 里、
   并把 Option 一路传进 `newServer`。
2. 页面写入后的"重建整张 store 侧表"直接用 `executor.MergeStoreProfiles(cfg, records)`：
   它与启动路径共用同一个函数、同一份校验（I1），返回的 `[]StoreEntry` 已经带好
   `Source=store` 与撞名的 `Degraded` 标记，直接喂 `ApplyStore` 即可。
   撞名判定只看 config 侧声明过哪些名字，不看那一条合不合法（W05 的
   `TestMergeStoreProfiles_CollisionChecksNameOnly`），所以 `Degraded` 只能由这个函数给，
   不要在外面自己判（W03 也提醒过：没标 Degraded 的撞名会被 `ApplyStore` 当编程错误拒掉）。

### 3.3 每个写请求的固定五步（顺序即 DoD）

1. `executors.web_enabled` 判定 → 关闭时 503。**先判它再判依赖**：没打开的部署不该从状态码里
   泄露"这台装配了什么"。
2. 解码：请求体结构 `ExecutorProfileRequest`，字段与 §5.1 的 json 键一致；
   未知键拒绝（与配置侧 `UnmarshalExact` 同口径）。PUT 时体内 `name` 缺失则取 `:name`，
   两者不一致 → 400。
3. `BuildProfile(..., PathAnywhere)` → 非法字段组合 400，`Details` 用 `err.Error()` 原文。
   **原文不进台账**（`api/handlers_executors.go:581-590` 已写明理由：可能带取值；
   这里同理，路径本身也不进台账列）。
4. `Probe` → 不可用**不拒绝保存**，条目带着 `available=false` 落盘并热生效（设计文档 §5.3 那条反直觉口径）。
   这一步必须跑，且结论要出现在响应里。
5. 不变量 → 落盘 → 热生效 → 响应：
   - POST：任一侧同名（`Registry.Lookup` 或 store `Get`）→ 409。
   - PUT：键不存在 → 404；改 `kind`/`script`/`program` → 400（D7，错误文案要点名"删了重建"）。
     PUT 前先用 `Registry.Lookup(key)` 确认它是 `SourceStore`，改 config 档位 → 409 并说明它来自配置。
   - DELETE：`?jobs=pause|block`，白名单外取值 → 400（照删组的策略白名单）；
     默认 `pause`：先 `PauseByHandlerKey` 再摘 handler 再删记录；`block` 时若还有未终态任务 → 409。
     **`PauseByHandlerKey` 的返回值不含正在执行的那条**（W04 落地口径，用例见其 §10.4 第三行变异），
     所以 `block` 的判定要自己按存储数未终态条数，不能把这个返回值当"还有几个没跑完"；
     响应里给出的条数也要说明它是"本次新钉住的待执行任务数"。
   - **落盘先于生效（I2）**：`Save`/`Delete` 失败 → 500 且注册表与调度器**一行不动**；
     生效失败（`ApplyStore` 返回错误）→ 500 并回滚文件（把记录写回原值，或删掉刚写入的那条）。
     回滚失败要记 Error 日志并在响应里说"文件与内存不一致，重启可对齐"。

### 3.4 响应

复用 `ExecutorProfileResponse`（`api/handlers_executors.go:341-354`）加 W07 的三个新字段，
不新开 DTO。**`env` 的固定取值不回显**（只列键名，`web/src/views/SettingsView.vue:61-64` 钉住的同一条口径）。

### 3.5 审计

`api/audit.go:97-118` 加三行：`executor.profile_create` / `executor.profile_update` / `executor.profile_delete`。
本期不加台账列。同时把三个动作词补进 `AuditActions()` 供查询端点校验（`api/audit.go:129-132` 读的就是这张表，
加完行它自动跟上——要有用例证明这一点，见 §5.4）。

## 4. 实现步骤

1. 先写 `requireExecutorProfiles()` + 三条空路由 + 角色门禁用例（§5.1），让 503 与 403 先立起来。
2. 写 POST：五步顺序 + 冲突 + 审计行；跑通"POST → 立即提交 `exec.<name>` 任务并跑成"这一条集成用例。
3. 写 PUT（含 D7 禁改字段）与 DELETE（含 `?jobs=pause|block` 与 `PauseByHandlerKey`）。
4. 补 §5 全部用例，接 `cmd/server/main.go` 的注入（W05 已备好 Option）。
5. `docs/api.md` 加三节（端点、请求体、状态码），并把"改档位要重启"那句改掉（与 W07/W09 分工：
   本卡只改 API 文档侧，控制台文档归 W08、README 归 W09）。
6. 全量 build/vet/test -race。

## 5. 测试要求

1. **门禁**：照 `api/handlers_groups_test.go:233` 的写法，一条用例覆盖
   viewer 403、operator 403、admin 403、**ops 通过**、`machine`（静态 token）403；
   另一条覆盖"未注入 store/applier"与"`web_enabled=false`"两种 503，且响应文案可区分。
2. **POST 生效链**：临时目录 + `t.TempDir()` 的 store 路径，POST 一条 python 档位
   （script 指向仓库里已入库的 `exec-workspace/scripts/py_hello.py`）→ 200 →
   `POST /jobs {"name":"exec.<新建档位>"}` → 任务跑到 success → `GET /jobs/:id/result` 读到输出。
   **全程不重启**。这条是本系列的命门，不许只用单元测试代替。
3. **I2 落盘先于生效**：把 store 路径指向一个不可写的目录（Windows 上造不出权限失败时，
   用测试替身让 `Save` 返回错误），断言：响应 500、`Registry.Lookup` 查不到该键、
   调度器 `LookupHandler` 也查不到。反向用例：`ApplyStore` 失败时文件回到原值。
4. **审计**：三个动作各产生一行台账，`action` 列逐条断言；
   另加一条"临时注释掉映射表其中一行 → 那一行动作变成 `other` 而不是消失"（`api/audit.go:266-269` 的既有口径）。
5. **不可改字段**：PUT 改 `kind`、改 `script` 各一条 400 用例，断言错误文案含"删除后重建"。
6. **删除语义**：造 3 条该类型 pending + 1 条 running（慢 handler）→ DELETE（默认）→
   3 条变 `paused`、running 那条仍是 running 且没被取消（对齐 W04 §5.2）；
   `?jobs=block` 时同样场景回 409 且什么都没动；`?jobs=whatever` → 400。
7. **冲突**：POST 与 `executors.commands` 同名 → 409 且注册表不动；POST 与已有 store 档位同名 → 409。
8. **探测不可用仍可保存**：script 指向不存在的路径 → 200 + `available=false` + `reason` 指名脚本；
   紧接着提交该类型任务 → 400（`api/handlers_executors.go:560-569` 那条既有拒绝）。
9. `env` 不回显：档位带 `env: {TOKEN: secret-value}` 时，POST 响应与 `GET /executors` 都搜不到
   `secret-value` 字面量（canary 断言，照 S06 的 canary 写法）。
10. 手工：见 §7。

## 6. 完成标准（DoD）

- [ ] 三条路由按 §3.1 挂载，`web_enabled=false` 或未注入依赖时 503，档位不足时 403。
- [ ] 五步顺序与 §3.3 一致，且顺序本身有用例支撑（特别是 I2 那条）。
- [ ] §5.2 的"不重启建出可用任务类型"集成用例绿。
- [ ] 删除默认 `pause`、`block` 白名单、D7 禁改字段三条都既有实现也有用例。
- [ ] 三个审计动作词进映射表，`AuditActions()` 列表包含它们，漏配落 `other` 有反证用例。
- [ ] `env` 与路径原文都不进台账；响应不出现 canary。
- [ ] `docs/api.md` 三节写完，"改档位要重启"那句已替换。
- [ ] `go build ./...`、`go vet ./...`、`go test ./... -race` 全绿。

## 7. 验收方式

```bash
go test ./api -run 'TestExecutorProfile' -race -v
go test ./api -run 'TestAudit' -race -v
go build ./... && go vet ./...
```

手工（临时目录冒烟，端口挑 IPv4/IPv6 都空的）：

1. ops 档登录取 JWT → `POST /api/v1/executors/profiles` 建一条 `exec.smoke_py`
   （python + 已入库脚本）→ 200，`available=true`。
2. 立刻 `POST /api/v1/jobs` 提交 `exec.smoke_py`，`delay: 2s` → 轮询到 `success`，
   `GET /jobs/:id/result` 看到脚本输出。**没有重启过进程。**
3. `PUT .../exec.smoke_py` 把 `script` 换成另一个路径 → 400。
4. `DELETE .../exec.smoke_py` 前先提交一条 `delay: 30m` 的同类型任务 → 删除后它是 `paused`；
   `POST /jobs/:id/resume` → 任务最终判失败（找不到处理函数），与 `cmd/server/main.go:610-612` 一致。
5. `GET /api/v1/admin/audit?action=executor.profile_create` 查到第 1 步那一行，
   整行里搜不到 `env` 取值与请求体原文。
6. operator 档重复第 1 步 → 403。

秒级现象（第 2 步的 success）要轮询 REST 取证；界面侧观测受隐藏页节流影响，见项目记忆。

## 8. 不在本任务范围

- 不改 `GET /executors` 的响应字段（W07）。
- 不做前端（W08）。
- 不做细粒度授权、不做"改前改后值"的专门台账列（S-2）。
- 不做路径白名单校验（S-1）；本期 `PathAnywhere` 就是设计文档 D5 拍板的口子。
- 不做页面上管理脚本文件（S-4）。
- 不改 `executors` 全局参数的在线可改性（§8 设计文档：不做）。

## 9. 风险与回滚

| 风险 | 说明 | 处置 |
| --- | --- | --- |
| 生效先于落盘 | 进程崩溃后页面上建过的档位消失，运维无从判断 | §3.5 顺序 + §5.3 用例；DoD 单列 |
| 删除时误杀 running | 变成隐式 force-pause，越过了 admin 档的既有边界 | §5.6 断言 running 未动 |
| ops 档写端点被当成"能提交任务" | 提交门槛是另一件事（`executors.required_role`，全局一份）；两条判定互不替代 | §5.1 门禁用例只判写端点；文案里点名两条门槛 |
| 错误原文带出的路径进了台账 | 台账给 ops 看，本身不泄露给 reader，但破坏了"表里没有值"的口径 | §5.9 的 canary 断言覆盖响应；台账断言只查列 |
| 一次改动的注册表与调度器不一致（`ApplyStore` 成、`Register` 败） | 提交期查得到档位但执行时没有 handler | §3.5 的回滚与"重启可对齐"文案；集成用例 §5.2 覆盖正常路径 |

回滚：三条路由摘掉即回到 W05 之后的形态（只能手改文件 + 重启）。窄接口与 Option 是纯新增，可保留。

## 10. 实现记录（执行时补写）

（待补：落地的接口 / 与本卡写法的差异 / 验证证据 / 手工验收 / 缺陷 / 未覆盖项）
