# TASK-E04　注册到调度器与启动装配

- 所属阶段：M0 基础
- 依赖任务：TASK-E01、E02、E03
- 涉及文件：`cmd/server/main.go`、`cmd/server/main_test.go`、`cmd/server/main_integration_test.go`、新增 `executor/handler_stub.go`
- 预计规模：中

## 1. 任务目标

把配置里的档位变成调度器里可用的处理函数：`executors.enabled: true` 时，`exec.<name>` 出现在 `GET /job-types` 里，提交能过校验，触发时执行一个"还没实现"的占位逻辑并明确返回未实现错误。目标是把装配链路先接通，让后面的卡片可以在真实进程里验证。

## 2. 背景与当前问题

现在唯一的注册点是 `cmd/server/main.go` 的 `registerHandlers`，被 `run` 调用，写死四个示例处理函数。`run` 的装配走 `runtimeDeps`（`defaultRuntimeDeps` 提供构造闭包），测试通过替换闭包来跑 `TestRun_RegistersHandlersStartsAndStops`。`cmd/server/main_test.go` 的 `TestRegisterHandlers_RegistersAllSupportedJobTypes` 断言"恰好注册四个"，本任务要改它。

另外，`api.NewServer` 的可选项走 `Option`（`WithGroupStore`、`WithConsole` 是现成先例），执行器登记表要按同样方式注入，才能做到"没配置就 503，不 panic"。

## 3. 要实现的功能

1. `executor/handler_stub.go`：`func (p *Profile) StubHandler() core.Handler`，返回一个只做两件事的处理函数：记一条 info 日志（含 `job_id`、`handler_key`），然后返回 `errNotImplemented`（`fmt.Errorf("executor %s: not implemented yet", p.HandlerKey())`）。E09 会把它换成真实实现，本卡只需要链路通。
2. `executor.Register(s schedulerRegistrar, reg *Registry, logger *slog.Logger) int`：
   - 遍历登记表，对每个档位调用 `s.RegisterHandler(p.HandlerKey(), p.StubHandler())`，返回注册数量。
   - 注册前检查键是否已被占用（用 `LookupHandler`），冲突时**返回错误**而不是覆盖：档位名与代码里注册的示例处理函数重名，必须让运维立刻知道。因此 `schedulerRegistrar` 接口需要 `RegisterHandler`、`LookupHandler` 两个方法，`*core.Scheduler` 都满足。
   - 注册不可用档位（探测失败）时照常注册，并在返回结构里区分计数：`registered` / `unavailable` 两个数字都进日志。
3. `cmd/server/main.go`：
   - `runtimeDeps` 加字段 `newExecutorRegistry func(core.Config, *slog.Logger) (*executor.Registry, error)`，`defaultRuntimeDeps` 里给实现（调 `executor.NewRegistry`）。
   - `run` 里在 `newScheduler` 之后、`newServer` 之前建登记表并注册；登记表随后传给 `newServer` 闭包（见第 4 点）。失败时返回错误、进程不启动。
   - `registerHandlers` 改签名为 `registerHandlers(server serverAPI, reg *executor.Registry, logger *slog.Logger) error`，示例处理函数部分保持不变。
   - `schedulerAPI` 接口需要补 `LookupHandler(string) (core.Handler, bool)`（`*core.Scheduler` 已有此方法）。注意 `main_integration_test.go` 里的 `spyScheduler` 要同步补方法。
4. `api.NewServer` 增加 `WithExecutorRegistry(reg *executor.Registry) Option`：本卡只加注入与 `Server.executors` 字段（未注入时为 nil），端点在 E07/E13 实现。加 `requireExecutorRegistry()` 守卫，风格照 `api/handlers_groups.go` 的 `requireGroupStore()`：未注入返回 503。
5. 启动横幅：`executors.enabled == true` 且 `cfg.Server.Auth.Enabled() == false` 时，记 **error** 级日志：`executors are enabled while server authentication is disabled`，并附一条提示（生产环境必须配置 `server.auth.token` 或 `server.auth.users`）。只记日志，不阻止启动（测试环境需要能开执行器）。
6. 注册数量为 0 且 `enabled == true` 时记 warn：开了开关但没声明档位，多半是 `commands` 写错或写在了错误的层级。

## 4. 实现步骤

1. 先写 `StubHandler` 与 `Register`，用假 registrar 做单测。
2. 改 `main.go` 的 `runtimeDeps` 与 `run`，保持既有的关闭顺序不动（`store.Close` 的 defer 位置不变）。
3. 改 `api/server.go`：新增 `Option` 与字段，不碰任何路由。
4. 更新两个测试文件的断言。
5. 跑全量。

## 5. 测试要求

1. `executor/register_test.go`：
   - `TestRegister_AllProfiles`：三个档位 → 注册三个 `exec.` 键，计数正确。
   - `TestRegister_KeyConflict`：预先把 `exec.dup` 注册成示例处理函数 → `Register` 返回错误且错误信息含冲突键名，且**不留下部分注册结果**（要么全成功要么明确报告冲突项；建议实现为"检测到冲突就停止并返回已注册数量+错误"，测试要把这个语义固定下来并写进注释）。
   - `TestRegister_EmptyRegistry`：空登记表 → 注册 0 个，不报错。
   - `TestStubHandler_ReturnsNotImplemented`：调用桩，断言错误信息含档位键名，且 ctx 已取消时不 panic。
2. `cmd/server/main_test.go`：
   - 改 `TestRegisterHandlers_RegistersAllSupportedJobTypes`：`reg` 传空登记表时仍恰好四个示例处理函数（证明执行器不干扰既有注册）。
   - 新增 `TestRegisterHandlers_WithProfiles`：登记表里两条档位 → 总共六个键，其中含 `exec.a`、`exec.b`。
   - 新增 `TestRegisterHandlers_ConflictFails`：档位名与 `payment_check` 冲突（用 `exec.payment_check` 与一个示例名重合的构造方式）→ 返回错误。
3. `cmd/server/main_integration_test.go`：
   - `TestRun_RegistersHandlersStartsAndStops` 继续通过（`spyScheduler` 补 `LookupHandler`）。
   - 新增 `TestRun_ExecutorRegistryError`：`newExecutorRegistry` 返回错误 → `run` 返回错误，且 `store.Close` 被调用（用现有 spy 断言）。
   - 新增 `TestRun_LogsErrorWhenAuthDisabled`：`enabled=true` 且无凭据 → 捕获日志含上面那句 error。
4. 手工：`enabled: true` 配一个档位，起服务后 `curl -s localhost:8080/api/v1/job-types` 能看到 `exec.<name>`。

## 6. 完成标准（DoD）

- [ ] `executors.enabled: false`（默认）时：注册表内容与改动前完全一致，`/job-types` 输出不变，全部既有测试不改断言就能通过（除了本卡显式要求改的那几个）。
- [ ] `enabled: true` 时档位进入注册表，提交任务能通过 `createJobFromRequest` 的 `LookupHandler` 检查，触发后返回明确的未实现错误（事件里能看到原因文本，不是 `no handler registered`）。
- [ ] 键冲突不会被静默覆盖。
- [ ] 未启用鉴权却开了执行器，会在启动日志里留下 error 级记录。
- [ ] `api.Server` 有了登记表字段但还没有端点读它；`requireExecutorRegistry` 有测试或至少被 E07 的测试覆盖到（写明去向，不留未使用代码：本卡先注入不使用是允许的，因为 E07 紧随其后）。

## 7. 验收方式

```bash
go build ./... && go vet ./... && go test ./... -race
go test ./cmd/server ./executor ./api -race -v
```

手工（临时打开开关）：

```bash
GODELAYQ_EXECUTORS_ENABLED=true ./godelayq-server -config=configs/config.yaml &
curl -s localhost:8080/api/v1/job-types | grep '"exec\.'
curl -s -X POST localhost:8080/api/v1/jobs -H 'Content-Type: application/json' \
  -d '{"name":"exec.<档位名>","delay":"2s","payload":{"args":{}}}'
```

预期：第一条命令能看到 `exec.` 键；第二条返回 201；约 2 秒后 `GET /api/v1/events?limit=10` 里能看到该任务的 failed 事件，原因是未实现。

## 8. 不在本任务范围

- 不执行真实进程（E09）。
- 不加分流（E13）。
- 不加 `/executors` 端点（E13）。
- 不做权限档位判断（E16）。

## 9. 风险与回滚

- 风险：`run` 的装配顺序改动会影响优雅关闭路径的既有测试（`api/shutdown_test.go`、`main_integration_test.go`）。要求只在指定位置插入，不改 `scheduler.Start()` 与 `server.Start()` 的相对顺序。
- 风险：桩处理函数容易在后续卡片里被忘记替换，导致"看起来接通了但永远失败"。E09 的 DoD 里必须写明删除 `StubHandler`。
- 回滚：本卡涉及 `main.go` 与 `api/server.go` 的结构改动，回滚用 `git revert` 单个提交；登记表是新增类型，不会被其它已发布代码引用。
