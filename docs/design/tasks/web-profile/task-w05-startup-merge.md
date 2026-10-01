# TASK-W05　启动合并：yaml ∪ 档位文件

- 所属阶段：M2 接线与端点
- 依赖任务：TASK-W01、TASK-W02、TASK-W03、TASK-W04
- 涉及文件：`executor/merge_profiles.go`（新增）、`executor/merge_profiles_test.go`（新增）、
  `cmd/server/main.go`、`cmd/server/profile_merge_test.go`（新增）、
  `cmd/server/main_integration_test.go`（替身调度器加调用流水）
  ——落地清单与卡面差异见 §10.1、§10.2
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

- [x] `web_enabled=false`（默认）时：不构造 store、不读文件、不建文件、日志一行不多，行为与本卡之前一致。
- [x] `MergeProfiles`（落地名 `MergeStoreProfiles`，差别见 §10.2）在 `executor` 包内，`main.go` 里没有档位合并规则。
- [x] config 侧"任一条不过即启动失败"保持不变；store 侧"单条失败不连坐"有独立用例与注释。
- [x] 同名冲突：config 生效、store 降级不注册；进程能起来。
- [x] 文件损坏 → 启动失败，文案含路径与自救指引（用例断言两句都在）。
- [x] §5.5 顺序断言用例绿。
- [x] 手改文件加一条 python 档位 → 起进程后能提交并跑通该类型任务（不重启、无端点参与）。
- [x] `go build ./...`、`go vet ./...`、`go test ./... -race`、`go build -tags dashboard ./cmd/server` 全绿。

## 7. 验收方式

```bash
go test ./executor -run 'TestMergeStoreProfiles' -race -v
go test ./cmd/server -run 'TestRun_(Merges|StoredProfiles|InvalidStored|CorruptProfile|RealProfileClosure|WebDisabled|MissingProfile)' -race -v
go build ./... && go vet ./...
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
（落地后这句的具体形状：`NewRegistry` 签名本来就没动，退回只需删掉 `run()` 里
`web_enabled` 那两段与依赖字段，`MergeStoreProfiles` 与它的单测留在包里不接。）

## 10. 实现记录（2026-10-01）

### 10.1 落地的接口与现行位置

`executor/merge_profiles.go`（新增，76 行）：

- `ProfileWarning{Name, Reason}` — `:13-18`。
- `MergeStoreProfiles(cfg core.Config, stored []core.ExecutorProfileRecord) ([]StoreEntry, []ProfileWarning)`
  — `:39-76`。config 侧只取"声明了哪些名字"用于撞名判定（`:42-51`），
  store 侧逐条 `record.Command()` → `BuildProfile(cmd, executors, PathAnywhere)` → `Probe`，
  任一步失败只跳过那一条并进 warnings；撞名标 `Degraded`（`:71`）。

`cmd/server/main.go`：

- 依赖字段 `newExecutorProfiles func(core.Config) ([]core.ExecutorProfileRecord, error)` — `:119-126`，
  默认实现 `:172-188`（打开存储 → `List()`，两处失败都追加自救指引）。
- 依赖完整性检查：`web_enabled` 打开却缺这个闭包 → `runtime dependencies are incomplete`（`:342`）。
- 启动接线 `:383-415`：读文件（仅 `web_enabled`）→ `newExecutorRegistry` → `MergeStoreProfiles`
  → 逐条 warnings 记 `logger.Warn("stored executor profile not registered", profile, reason)`
  → `executors.ApplyStore(entries)`（失败即启动失败）→ `registerHandlers`（`:570`）
  → `installRestoreGuard`（`:574`）→ `scheduler.Start()`（`:576`）。

测试：新增 `cmd/server/profile_merge_test.go`（8 例）、`executor/merge_profiles_test.go`（10 例）；
`cmd/server/main_integration_test.go` 的替身调度器加了调用流水 `calls` 与 `callIndex`。

### 10.2 与本卡写法的差异

1. **合并函数改名并缩窄范围**：`MergeProfiles(cfg, stored) ([]MergedProfile, error)`
   → `MergeStoreProfiles(cfg, stored) ([]StoreEntry, []ProfileWarning)`。三条理由：
   - `MergedProfile{Profile, Probe, Source, Degraded}` 与 W03 已落地的 `StoreEntry` 字段逐字相同，
     再造一个同形结构会让构造入口与运行期入口各吃一种类型。
   - config 侧在 `NewRegistry` 里已经加载并探测过；合并函数再跑一遍 `LoadProfiles`
     等于把 **config 侧的连坐规则搬进 store 侧的路径**——那条不对称正是本卡要守住的东西，
     顺带还白做一遍探测。
   - store 侧不连坐 ⇒ 这个函数没有"整体失败"这种结果，`error` 返回值是空的。
   ⚠️ 因此卡 §5.1 的"config 侧同样非法 → 整个 `MergeProfiles` 返回错误"落到两条用例上：
   `TestMergeStoreProfiles_DoesNotRevalidateConfigSide`（合并对 config 的非法既不看也不报错，
   同一份配置交给 `NewRegistry` 则报错）与 `_CollisionChecksNameOnly`（撞名只看键位占用，
   不看 config 那条合不合法）。两侧口径的差别仍然分别有断言，只是断在两个函数上。
2. **`NewRegistry` 的签名没动**（卡 §3.3 说可以变）：合并结果经 `ApplyStore` 进表。
   仍然是"一次调用一张完整表"（`ApplyStore` 内部整表替换 + `writeMu`），
   而签名不变让 `api`/`cmd` 那 10 处调用点零改动；更实际的理由是 W06 的运行期写入本来就要调
   `ApplyStore`，两条路径共用同一个入口就共用同一套拒绝规则（没标 `Degraded` 的撞名会被拒掉）。
3. **卡 §3.6 的 api 注入本卡没做**（`WithExecutorProfileStore` / `WithExecutorProfileApplier`）：
   本卡没有任何读取方，提前注入只多一个没人看的字段与一条没法断言的 503 分支。
   后果与交接已写进设计文档 §6.5 与 W06 §3.2 —— **W06 要自己把活的存储实例建到 `run()` 里**，
   本卡的闭包只交回记录，不交回实例，写盘（`Save`/`Delete`）拿不到手。
4. **依赖形状是"读记录"而不是"构造 store"**（同 3）。默认闭包用完即弃，
   这一点与分组 store 的装配不同（`newServer` 里那个要留给端点用）。
5. **顺序断言换了实现**：替身调度器原有的 `guardSetBeforeStart` 只能说"钩子早于 Start"，
   说不出 handler 是不是在钩子之前登记的。落地给 `spyScheduler` 加了调用流水
   （`register:<键>` / `restore_guard` / `start`）与 `callIndex`，
   用例断 `register:exec.beta < restore_guard < start`，并额外喂守卫一条 store 档位的
   running 快照，确认它真的认得这条来源。
6. **卡 §5.1 的"`web_enabled=false` → 传空切片断言没有 store 条目进表"落地成更强的形式**：
   用例直接用生产闭包 + 一个不存在的嵌套目录，断言跑完之后那个目录仍然不存在
   （`TestRun_WebDisabledNeverTouchesTheProfilesPath`）——"没调用"比"调用了但返回空"更接近 DoD。
7. 新增文件、新增测试文件名按仓库体例：`executor/merge_profiles.go`、`executor/merge_profiles_test.go`、
   `cmd/server/profile_merge_test.go`（卡头只列了 `main_test.go`/`main_integration_test.go`）。

### 10.3 验证证据

```
$ go test ./executor -run 'TestMergeStoreProfiles' -race -v   → 10/10 PASS，ok 1.4s
$ go test ./cmd/server -race                                  → ok 5.7s（含本卡 8 例）
$ go build ./... && go vet ./...                              → 通过
$ go build -tags dashboard ./cmd/server                       → 通过（产物 server.exe 已删）
$ go test ./... -race -count=1  → ok godelayq/api 217.0s
                                  ok godelayq/cmd/server 5.9s
                                  ok godelayq/core 12.8s
                                  ok godelayq/executor 25.0s
                                  ok godelayq/store/sqlite 4.0s
```

`api/` 与 `core/` 零改动即通过：`NewRegistry`/`ApplyStore` 的读侧与构造侧都没碰签名，
`Register` 也仍不需要"跳过 degraded"的分支（降级条目从不进 `Keys()`，卡 §3.4 的预测成立）。

### 10.4 反向验证（三处变异）

| 变异 | 预期变红 | 实测 |
| --- | --- | --- |
| `installRestoreGuard` 挪到 `registerHandlers` 之前 | `_StoredProfilesAreRegisteredBeforeTheRestoreGuard` | `FAIL`："1" is not less than "0"（档位注册必须早于崩溃恢复守卫） |
| `Degraded:` 恒为 `false` | `_NameCollisionMarksStoreSideDegraded`、`_ResultFeedsApplyStore`、`_CollisionChecksNameOnly`、`TestRun_MergesStoredProfilesIntoTheScheduler` | 四条全 `FAIL`；cmd/server 那条是 `ApplyStore` 报"键已被 config 占用"→ 启动失败，正是 §9 风险表第二条的形态 |
| store 侧首条非法改成直接返回（连坐） | `_InvalidRecordIsSkippedNotFatal`、`TestRun_InvalidStoredRecordIsSkippedAndLogged` | 两条 `FAIL`（survivor 没能进表） |

三处都恢复原状后重跑 `./executor ./cmd/server -race` 为 `ok`（缓存命中，说明与变异前的文件字节一致）。

### 10.5 手工验收（卡 §5.6，临时目录 `%TEMP%\w05smoke`，跑完删除）

一份临时 config：`executors.enabled=true`、`web_enabled=true`、`required_role=operator`、
workspace/store/产物/档位文件全部指向临时目录，`executors.commands` 只声明一条 `smoke_cfg`（node）；
手写的 `exec-profiles.json` 三条：`smoke_py`（python，脚本真实存在）、
`smoke_missing`（python，脚本不存在）、`smoke_cfg`（python，与配置同名）。

1. **手改文件加一条 python 档位 → 起进程 → 三条都在**：
   启动日志 `executor handlers registered total=3 registered=3 unavailable=1 degraded=1`；
   `GET /executors` 给 `exec.smoke_cfg`（runtime_ok=true）、`exec.smoke_missing`
   （`runtime_ok=false`，`reason = script file "scripts/nope.py" does not exist`）、
   `exec.smoke_py`（true）——进程照常起来，降级那条没有出现在列表里（W07 才展示它）。
2. **提交并跑通**：`exec.smoke_py` → `success`，preview 第一行 `hello from godelayq executor`、
   第二行 `runtime=python 3.13.2`、`cwd=<临时 workspace>`；
   `exec.smoke_cfg` → `success`，preview 是 node 那句单行输出 `hello from godelayq executor --day=today`
   ——**跑的是配置里那条定义**，降级判定端到端成立；
   `exec.smoke_missing` → 提交期 400 `executor profile is not available on this server`。
3. **文件损坏 → 挡住启动**：把档位文件改成 `{ this is broken`，进程退出码 1，
   唯一一条 error 级日志同时含文件路径、`parse executor profile file ... failed: invalid character 't'`
   与 `fix that file or delete it to fall back to profiles declared only in executors.commands`。
4. **默认关闭**：同一份坏文件原地留着，`GODELAYQ_EXECUTORS_WEB_ENABLED=false` 重启
   （顺带证明 W01 的 `BindEnv` 覆盖有效）：`total=1 registered=1 degraded=0`，
   只有配置侧那条注册，`profiles_path` 指向的 `never/created/` 目录**没有被创建**。

观测口径：以上都是真实进程 + curl；`source`/`degraded` 两个字段此刻还没进响应（W07），
所以"降级"在手工侧只能靠"`smoke_cfg` 跑出来的是 node 输出"这条间接证据，接口侧的直接证据是 §10.3 的用例。

### 10.6 缺陷

| 编号 | 说明 | 处置 |
| --- | --- | --- |
| D-0501 | store 侧探测失败的条目没有逐条 warn：config 侧在 `NewRegistry` 里对每条不可用档位记一条 `executor profile unavailable`（`executor/registry.go:130-135`），而合并路径经 `ApplyStore` 进表时不再探测告警，启动日志只有汇总的 `unavailable=N` | **登记不修**。信息没丢（计数在启动日志、原因在 `GET /executors` 的 `reason`，W07 还要给它 `source`/`degraded`），而在 `ApplyStore` 里补 warn 会让"整表替换"这个纯数据结构动作带上副作用；真要做到每条可见，归 W06 的写响应与 W07 的展示面 |
| D-0101 | 档位存储 `List` 只拷结构不拷嵌套切片 | 沿用 W01 结论，登记不修 |

### 10.7 未覆盖项

- 没有"页面写入后立即生效"的运行期用例（`ApplyStore` 在 `main.go` 里只在启动路径被调一次）：
  那是 W06 的端点用例，本卡 §8 明确不做。
- 同一份档位文件里两条记录占同一个注册键的情形没能构造出来——W01 的存储在 `load` 阶段就按
  忽略大小写的名字查重（`duplicate executor profile: "x" appears more than once in "path"`，
  `core/executor_profile_store.go:320`），
  所以 `ApplyStore` 那条"同批重复即拒"在启动路径上是走不到的分支，只由 W03 的直接调用用例覆盖。
- `web_enabled=true` 且 `enabled=false` 的组合只在配置校验（W01）与
  `TestRun_StoredProfilesCannotBeAppliedWhileExecutorsAreDisabled` 两侧钉住，
  真实 `LoadConfig` 走不到（`Validate` 先拒），因此没测"绕过校验直接从代码构造配置"以外的路径。
- Windows 之外的平台未实跑：交叉构建与目标环境行为归 W09。
