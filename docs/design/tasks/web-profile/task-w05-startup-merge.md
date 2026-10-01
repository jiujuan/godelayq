# TASK-W05　启动合并：yaml ∪ 档位文件

- 所属阶段：M2 接线与端点
- 依赖任务：TASK-W01、TASK-W02、TASK-W03、TASK-W04
- 涉及文件：`cmd/server/main.go`、`executor/registry.go`（必要时补构造参数）、`cmd/server/main_test.go`、`cmd/server/main_integration_test.go`
- 预计规模：中

## 1. 任务目标

进程启动时把两份档位来源合成一张表：`executors.commands`（严格模式，来源 `config`）与
`executors.profiles_path` 指向的文件（宽松模式，来源 `store`），同名冲突按"config 赢、store 条目降级"处理，
并保证注册→恢复守卫→启动的顺序正确。本卡结束时页面建的档位**活过重启**（还没有写端点，只能手改文件）。

## 2. 背景与当前问题

今天只有一份来源：`executor.NewRegistry`（`executor/registry.go:44-85`）→ `LoadProfiles` → 逐条 `Probe`
→ 填表；随后 `executor.Register`（`executor/register.go:49-100`）把每条注册进调度器。
装配点在 `cmd/server/main.go`：`newExecutorRegistry`（118、163）→ `registerHandlers`（513、560-584）。

要接第二份来源，有三处顺序是硬约束：

1. **注册在 `Restore` 之前**。崩溃恢复守卫按 `HandlerClass` 查表决定改判
   （`cmd/server/main.go:613-624`，`installRestoreGuard` 在 595-603，必须早于 `Start`）。
   如果 store 档位在守卫装好之后才注册，那批任务恢复时查不到类别，就会跳过 `paused` 改判——
   这是本卡最容易做反、而且**不会有任何测试提醒**（除非专门写一条顺序断言）的一处。
2. **文件读失败不能悄悄跳过**。分组先例是读不到就不起服务（`main.go:233-239`），
   档位文件照同一条（设计文档 P2 取的答案），但文案要给自救路径。
3. **冲突不得让整个进程起不来**。`executor/register.go:69-74` 现在的行为是"键被占用即启动失败"，
   这条对 config 侧保留（防与代码注册的 `payment_check` 撞名），对 store 侧要改成降级
   （设计文档 §5.2、P1 取的答案）。

## 3. 要实现的功能

1. `executors.web_enabled=true` 时构造 `core.JSONFileProfileStore`（`main.go` 的分组 store 旁边，
   同一段错误处理形状）；`false` 时**不构造、不读文件、不创建目录**。
2. 合并逻辑放在 `executor` 包而不是 `main.go`（`main.go` 只做接线，规则要有测试覆盖）：

   ```go
   func MergeProfiles(cfg core.Config, stored []core.ExecutorProfileRecord) ([]MergedProfile, error)
   ```

   `MergedProfile{Profile *Profile; Probe ProbeResult; Source Source; Degraded bool}`。
   - config 侧走 `LoadProfiles`（严格，不变）。
   - store 侧逐条 `BuildProfile(cmd, ec, PathAnywhere)` + `Probe`。
     **单条失败不得连坐**：某条记录坏了（字段组合非法）时，跳过它并把原因返回给调用方打日志，
     其余继续——与"页面上的一次保存不该让队列停摆"同一条理由。
     这与 config 侧"任一条不过即启动失败"（`executor/profile.go:166-168`）**故意不同**，
     要在注释里写死这条差别。
   - 同名（注册键相同）：config 条目保留并注册，store 条目 `Degraded=true` 且不注册。
   - 返回顺序按注册键字典序。
3. `Registry` 的构造改为接受合并结果（W03 已有 `ApplyStore`；本卡用它的兄弟形状，
   保持"一次构造一张完整表"，不在 `main.go` 里循环填）。
   `NewRegistry` 的签名可以变（它是唯一构造点，测试替身在 `main_test.go:44、59`）。
4. `executor.Register`（`executor/register.go:49-100`）：
   - 跳过 `degraded` 条目（不注册 handler，因此提交期根本查不到这个键，行为等同"这个类型不存在"，
     与降级原因"配置里已有同名档位"一起出现在 `GET /executors` 里，W07 负责展示）。
   - 69-74 的查重保留，但只对"要注册的条目"生效。
   - 启动日志用 W03 新加的 `Registration.Degraded`。
5. 文件损坏（JSON 解析失败）→ **启动失败**，错误文案必须包含：文件路径、
   "修好或删掉该文件可退回只由 `executors.commands` 决定档位的形态"。
6. 注入 api：新增 `api.WithExecutorProfileStore(store)`（照 `api/server.go:89-91` 的形状）
   与 `api.WithExecutorProfileApplier(...)`（W06 需要的那个窄接口的实现方），
   本卡只做注入与"未注入时端点 503"的准备，端点本身属于 W06。

## 4. 实现步骤

1. 先在 `executor` 包写 `MergeProfiles` + 单测（§5.1-5.3），此时 `main.go` 未动，全仓行为零变化。
2. 改 `Registry` 构造与 `NewRegistry` 签名，跑绿 `./executor ./api ./cmd/server`。
3. `executor.Register` 加 degraded 跳过 + 日志。
4. `main.go` 接线：读 store（失败即退出）→ 合并 → 构造 Registry → `registerHandlers` →
   `installRestoreGuard` → `Start`，并在实现记录里贴出这段的实际调用顺序行号。
5. §5.5 那条顺序断言用例（最容易漏的一步）。
6. 全量 `go build ./... && go vet ./... && go test ./... -race`。

## 5. 测试要求

1. `MergeProfiles`：
   - 只有 config → 与 `LoadProfiles` 结果一致，全部 `Source=config`、无降级。
   - 只有 store → 全部 `Source=store`，越界绝对路径的档位合并成功（依赖 W02 的宽松模式）。
   - 同名 → config 生效、store 条目 `Degraded`，返回条目数 2。
   - store 里一条字段组合非法（`kind: script` 缺 `runtime`）→ 该条被跳过并出现在返回的告警列表里，
     其余正常；config 侧同样非法 → 整个 `MergeProfiles` 返回错误（两侧口径差别必须分别有断言）。
   - `web_enabled=false` → 不读 store（传空切片即可断言"没有任何 store 条目进表"）。
2. 启动：`main_test.go` 的既有形状（44、59 的替身）加一例"store 有两条、yaml 有一条同名"，
   断言注册进调度器的只有 config 那条 + store 的另一条。
3. 文件损坏：`t.TempDir()` 里写一份坏 JSON，断言进程构造返回错误且错误串里同时出现路径与
   "删掉该文件"那句自救指引。
4. `New...` 对不存在文件返回空集合而不是错误（照 `core/group_store.go:110-114`），
   断言此时 `exec-profiles.json` **没有被创建**（只有页面写过之后才存在）。
5. **顺序断言**：在 `main_integration_test.go` 里造一条"store 档位的 `exec.<name>` 任务快照状态是 running"，
   跑一次完整启动流程，断言它被守卫钉成 `paused`。
   这条用例等价于"注册发生在 `installRestoreGuard` 与 `Restore` 之前"的证明；
   如果做不到（守卫只在 `enabled=true` 且 `restore_policy=pause` 时装，见 `main.go:595-601`），
   就在实现记录里写明用了哪两个配置取值。
6. 手工（临时目录，按 README 的冒烟口径）：
   - 手改 `exec-profiles.json` 加一条 python 档位（指向 `exec-workspace/scripts/py_hello.py`）→
     起进程 → `GET /executors` 里 `source=store`、`available=true`。
   - 同一条改成一个不存在的脚本 → `available=false` 且 `reason` 指名脚本，**进程照常起来**。
   - 再加一条与 `executors.commands` 同名 → 该条降级、config 那条照常在，进程照常起来。

## 6. 完成标准（DoD）

- [ ] `web_enabled=false`（默认）时：不构造 store、不读文件、不建文件、日志一行不多，行为与本卡之前一致。
- [ ] `MergeProfiles` 在 `executor` 包内，`main.go` 里没有档位合并规则。
- [ ] config 侧"任一条不过即启动失败"保持不变；store 侧"单条失败不连坐"有独立用例与注释。
- [ ] 同名冲突：config 生效、store 降级不注册；进程能起来。
- [ ] 文件损坏 → 启动失败，文案含路径与自救指引（用例断言两句都在）。
- [ ] §5.5 顺序断言用例绿。
- [ ] 手改文件加一条 python 档位 → 起进程后能提交并跑通该类型任务（不重启、无端点参与）。
- [ ] `go build ./...`、`go vet ./...`、`go test ./... -race`、`go build -tags dashboard ./cmd/server` 全绿。

## 7. 验收方式

```bash
go test ./executor -run 'TestMergeProfiles' -race -v
go test ./cmd/server -run 'TestMain|TestRegisterHandlers|Restore' -race -v
go build -tags dashboard ./cmd/server && echo BUILD-OK
```

手工三例（§5.6）逐条贴输出。

## 8. 不在本任务范围

- 不做写端点（W06）、不改 `GET /executors` 的响应字段（W07）。
- 不做路径白名单（S-1）。
- 不做"页面写入后立刻生效"的那条运行期路径——本卡只保证**重启后**读得到，
  运行期生效由 W06 调 `ApplyStore` 完成。
- 不改 `executors` 一节的任何全局参数语义。

## 9. 风险与回滚

| 风险 | 说明 | 处置 |
| --- | --- | --- |
| 注册晚于守卫/恢复 | store 档位的崩溃任务不被钉成 paused，重启后直接重排（虽然会因找不到 handler 判失败，但绕过了"等人确认"） | §5.5 顺序断言 + §4.4 要求贴出实际调用行号 |
| 降级条目也被注册 | 同名时 yaml 档位被页面那条的执行体顶掉，违反 D4 | `Register` 的跳过逻辑 + §5.2 那条用例 |
| store 侧错误连坐导致启动失败 | 一条手改坏的记录让整个队列停摆 | §3.2 明确不连坐，config 侧才连坐，两侧各有用例 |
| 冲突处理与 P1 的评审答案不符 | 实现时"顺手"改成拒启动 | DoD 单列一条；若要改动方向必须先回到设计文档 §12 重新拍板 |

回滚：`main.go` 的接线是本卡唯一的外部可见改动，把那段退回（不构造 store、`NewRegistry` 回原签名）
即可完全撤销；`MergeProfiles` 作为纯新函数可以留着不被调用。

## 10. 实现记录（执行时补写）

（待补：落地的接口 / 与本卡写法的差异 / 验证证据 / 手工验收 / 缺陷 / 未覆盖项）
