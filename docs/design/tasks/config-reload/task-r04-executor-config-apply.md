# TASK-R04　配置侧档位的整表替换与处理函数重登记

- 所属阶段：M1 落点
- 依赖任务：R01（`executors.commands` 归档为热更，许可字段归档为拒绝）
- 涉及文件：`executor/registry.go`、`executor/applier.go`、`executor/registry_apply_config_test.go`（新增）、
  `executor/applier_config_test.go`（新增）、`executor/applier_test.go`（fixture 增补一个构造口）、
  `cmd/server/main.go`（只改 `Applier` 的构造条件一处）
- 预计规模：中

## 1. 任务目标

让 `executors.commands` 在运行期可换：新增 `executor.Registry.ApplyConfig`（换掉登记表的 config 那一批、
store 那一批原样保留）与 `executor.Applier.ApplyConfig`（读新列表 → 校验探测 → 整表替换 →
重登记全部 `exec.*` 处理函数并摘掉不再存在的键）。本卡结束时这条链还没有触发方（R05/R06）。

## 2. 背景与当前问题

`executor.Registry` 已经支持运行期整表替换（TASK-W03，`executor/registry.go:143-211` 的 `ApplyStore`），
但它的语义被刻意收窄成"只动 store 那一批，config 来源的条目一律保留"
（`:167-173`：先从当前表里跳过 `SourceStore` 的条目再合）。
配置侧那一批发动在 `NewRegistry`（`:99-141`），只在启动时跑一次，此后再没有入口——
`executor.Register` 也帮不上：它键冲突即返回错误（`executor/register.go:71-77`），且只被启动链调一次。

于是热更 `executors.commands` 缺三样：

1. 登记表侧一个覆盖 config 批次的整表替换入口（`ApplyConfig`）。
2. 处理函数侧一次"config + store 两批全部重登记"的同步——现有 `Applier.Apply`
   只重登记 store 那批，并且显式放过 config 的键位（`executor/applier.go:159-164` 的 `keepConfig`）。
3. 一份"当前 config 侧命令列表"的记账：`Applier.Apply` 用 `a.registry.executors`
   做撞名判定与建处理函数（`:124`、`:175`），而 `Registry.executors` 是构造期冻结的字段
   （`executor/registry.go:82`）。config 批次换掉之后若还读那份冻结值，
   页面档位的撞名判定就对着一张过期的名字表。

第 3 项决定了本卡的落点：**记账放在 `Applier`，不动 `Registry` 的冻结字段**。
理由是依赖方向与安全口径一致——`Registry.executors` 里的非 `Commands` 取值
（`workspace`/`runtime_allow`/`env_allow`/两个 timeout）全是重启档或拒绝档，
本系列没有任何合法路径去改它们；把它们做成可变的，等于给未来的误用留一个入口。

## 3. 要实现的功能

### 3.1 `Registry.ApplyConfig`

```go
// ApplyConfig 用给进来的这一批档位重建登记表的 config 部分。
//
// 与 ApplyStore 镜像对称（那一份只动 store 侧、保留 config 侧，本方法反之）：
// 不在这一批里的旧 config 条目消失，store 来源的条目一律保留。
// commands 是这一批对应的原始声明，只用于把 `Registry` 的展示面保持一致，
// 不参与判定——判据全在 profiles 里（那批已经过 LoadProfiles 的严格校验）。
//
// 三条规则：
//   - 同一批内注册键重复：拒，整表不动（与 ApplyStore 的 :182-185 同一条）。
//   - 新的 config 条目与现存 store 条目撞名：**config 赢**，那条 store 条目移入 degraded 展示面，
//     不留在生效表里。这是启动合并规则的同一方向（executor/merge_profiles.go:31-33、
//     待拍板 P1 取的答案），差别是这里发生在运行期，所以降级原因是
//     "配置里新加的这条把它顶掉了"，文案由本方法给。
//   - 执行器关闭（enabled=false）时返回错误，与 ApplyStore 的 :155-161 同一条理由。
func (r *Registry) ApplyConfig(profiles []*Profile, commands []core.ExecutorCommand) error
```

实现要点：与 `ApplyStore` 一样在 `writeMu` 内构造新 `snapshot` 并一次 `data.Store`
（`executor/registry.go:163-209` 的形状），复用 `newSnapshot`（`:419`）。
`commands` 参数若最终只用于一致性检查而没有任何读取方，就删掉它（YAGNI）——
本卡的判据是"实现完后 `grep` 得到至少一个读取点"，否则签名里不该留这个参数。
（写卡时预计没有读取方，因此**落地大概率是 `ApplyConfig(profiles []*Profile) error`**，
记进 §10.2。）

### 3.2 `Applier` 的 config 批次记账

```go
// Applier 结构体新增字段（受既有 a.writeMu 保护，与 store 读写同一把锁——
// 见 docs/design/config-reload-design.md §12 那条"两条链共用一把写锁"）：
//   configCommands []core.ExecutorCommand
//
// NewApplier 里初始化为 registry 冻结的那一份（a.registry.executors.Commands）。
// 只在 writeMu 内读写，两个入口（Apply 与 ApplyConfig）都在方法开头取锁。

// executorsNow 返回"当前该用的 executors 取值"：
// 非 Commands 的部分永远是启动期冻结的那一份，Commands 换成 configCommands。
// 这个方法必须在 writeMu 内调用。
func (a *Applier) executorsNow() core.ExecutorsConfig
```

改 `Applier.Apply` 的两处读取：`:124` 的 `mergeStoreProfiles(a.registry.executors, records)` 与
`:175` 的 `executors := a.registry.executors` 都换成 `a.executorsNow()`。
这样页面档位与 config 档位的撞名判定用的是同一张最新的名字表。

`Applier.Validate`（`:98-104`）也换成 `a.executorsNow()`——它在 writeMu 之外被调用，
所以要么给它加锁、要么保持用冻结值。**本卡取"保持用冻结值"**：
`Validate` 只看非 `Commands` 的许可字段（路径模式、timeout 上限），
那些永不热更，用冻结值是正确的；`writeMu` 是写链的锁，不该被一次只读校验占住。
这个取舍写进方法注释。

### 3.3 `Applier.ApplyConfig`

```go
// ApplyConfig 把 executors.commands 的新列表换进登记表与调度器。
// candidate 是整份新配置（本方法只用它的 Executors.Commands），
// 返回值沿用 ApplyResult：Config/Stored/Degraded 是换完之后生效表的分项计数，
// Added/Removed 是这次处理函数的增减。
//
// 顺序固定，且失败点全部在"动手之前"：
//  1. executors.enabled 为 false → 返回错误（热重载链据此把本次记成 failed，见 §5.4）
//  2. 取 candidate.Executors.Commands，接到启动期冻结的那份非 Commands 取值上，
//     用这一份合成配置调 LoadProfiles（严格模式：任一条非法即返回错误，与启动同一条规则）
//  3. 逐条 Probe；探测失败照常入表并记 warn（与 NewRegistry 的 :126-137 同一口径）
//  4. a.writeMu 内：registry.ApplyConfig(entries) → 重登记全部 exec. 键 → 摘掉不再存在的键
//     然后 a.configCommands = 新列表
//
// 为什么第 2 步必须用冻结值而不是整份 candidate：workspace / runtime_allow / env_allow
// 都是重启档（设计文档 R8），如果让新配置里的它们参与校验，就会出现
// "新档位按新 workspace 建、既有档位仍按旧 workspace 跑"的分裂，
// 而那次分裂的表现是同一台机器上两条同名不同路径的档位都能通过校验。
func (a *Applier) ApplyConfig(candidate core.Config) (ApplyResult, error)
```

第 4 步的同步与 `Apply` 的差别（这三条是本次新增逻辑的核心，注释里逐条写）：

- **两批全部重登记**：`Apply` 只重登记 store 那批，`ApplyConfig` 对生效表里每个键都重建处理函数
  （config 那批的闭包按 `executor/register.go:87` 同一构造口 `Handler(...)` 建，
  两条路径不出现第二种处理函数）。
- **摘除范围放宽到全部 `exec.` 前缀键**：`Apply` 靠 `keepConfig` 放过 config 键位
  （`executor/applier.go:159-164`），`ApplyConfig` 不能放过——被删掉的正是 config 键位。
  判据统一成"`exec.` 前缀 + 不在新生效表里"，代码注册的普通任务键（不带前缀）仍然永不触碰
  （`core.ExecPrefix` 的用法照 `:166-170`）。
- **降级条目不进调度器**：被顶掉的 store 条目已经不在生效表里，重登记循环遍历的是
  `registry.Keys()`，天然不会给它建处理函数；它的键位由那条新 config 档位占着。

`Applier.Validate` 的宽松路径模式（D5）与 `ApplyConfig` 的严格模式之间不需要新的判据：
config 侧一直用 `PathInsideWorkspace`（由 `LoadProfiles` 决定），本卡不改。

### 3.4 与重启后的一致性

`ApplyConfig` 生效的东西与"改完 yaml 重启"必须完全等价。两条可验证的要求：

1. 重启时 `NewRegistry` 读同一份 `commands`，得到的生效表与本卡的第 3 步一致（用例：
   运行期 `ApplyConfig` 之后 `NewRegistry` 一张新表，两者的 `Keys()`/`SourceOf()` 逐项相等）。
2. 崩溃恢复守卫认得这批键（`cmd/server/main.go` 的 `installRestoreGuard` 按 `HandlerClass` 判断）：
   重登记用的是 `RegisterHandlerClass(key, ..., core.JobClassExec)`，类别与启动期同一条，
   本卡不需要为守卫做任何改动——但要有一条用例证明运行期新加的 config 键
   在 `classOf` 上回的是 `JobClassExec`。

## 4. 实现步骤

1. `executor/registry.go`：在 `ApplyStore` 之后加 `ApplyConfig`（同一把 `writeMu`、同一个
   `newSnapshot`），注释里写清与 `ApplyStore` 的镜像关系。
2. `executor/applier.go`：加 `configCommands` 字段与 `executorsNow()`、
   改 `Apply` 的两处读取、加 `ApplyConfig`。`Apply` 与 `ApplyConfig` 共享的"重登记 + 摘除"
   片段抽成一个内部函数（入参：要重登记的键集合、允许摘除的键判据），
   避免两份差集逻辑各写一遍——但**不要**把 `Apply` 改成调用 `ApplyConfig`
   （两者的范围不同，合并会让 store 路径意外摘掉 config 键位）。
3. `executor/applier_test.go`：给 `newApplierFixture` 加一个可选的"不预登记 config 键"开关
   或直接新增一个 `newConfigApplierFixture`，供本卡用例用；既有用例不改预期。
4. 新增两份测试文件（§5）。
5. `cmd/server/main.go`：把 `Applier` 的构造条件从 `if cfg.Executors.WebEnabled`（`:602`）改成
   `if cfg.Executors.WebEnabled || (cfg.Reload.Enabled && cfg.Executors.Enabled)`，
   而档位存储 `profileStore` 的打开条件**不变**（仍只看 `web_enabled`）——所以新条件里
   要处理"`web_enabled=false` 时 store 是 nil"这一支：`NewApplier` 目前要求 store 非 nil
   （`executor/applier.go:72-74`），本卡给它一个允许 nil store 的构造入口
   `NewConfigApplier(syncer, registry, artifacts, logger)`（不接 store，`Apply` 那条路径
   在 store 为 nil 时返回明确错误而不是 panic）。这条对应设计文档 §13 的 P2 与 README 的 P5。
6. 跑 §7 的命令，再跑全量。

## 5. 测试要求

### 5.1 `Registry.ApplyConfig`（`executor/registry_apply_config_test.go`）

- 基本替换：初始 config 有 `a`、`b`，`ApplyConfig([b', c'])` 后 `Keys()` 是 `exec.b'`、`exec.c`，
  `a` 消失；store 侧条目（fixture 先 `ApplyStore` 一条 `s`）仍在，来源仍是 `SourceStore`。
- 同批重复键 → 返回错误，且 `Keys()` 与调用前逐条相等（不留中间态）。
- 与现存 store 条目撞名：新 config 里加一条与 `s` 同名的档位 → `s` 从生效表移到 `Degraded()`，
  原因是"配置里新加的这条把它顶掉了"这类可读文案，`ListedProfile.Editable` 为 false。
- `enabled=false` 的登记表上调 `ApplyConfig` → 返回错误。
- 空批次：`ApplyConfig(nil)` 清空 config 那批，store 那批不受影响
  （这条是"删除配置里最后一条档位"的路径，别当成不可能）。

### 5.2 `Applier.ApplyConfig`（`executor/applier_config_test.go`）

fixture 用 `selfExecutable(t)` + `namedScript(...)`（`executor/applier_test.go:36-38` 的既有手法，
探测必然可用，不依赖本机装了什么解释器）。

- **新增一条 config 档位** → 替身调度器里出现 `exec.<新名>`，`classOf` 回 `JobClassExec`，
  `ApplyResult.Added` 含这个键，`Config` 计数 +1。
- **删掉一条 config 档位** → 替身里那个键被摘掉（`LookupHandler` 查不到），
  `ApplyResult.Removed` 含它；普通任务键（fixture 里预先注册一个不带 `exec.` 前缀的 `payment_check`）
  始终不被触碰——这条必须断言，它是"摘除只看前缀"的既有承诺。
- **改一条 config 档位的 `timeout`** → 键位不变、`Added`/`Removed` 都空，
  但替身里的闭包被重新登记过（用 `fakeRegistrar.writeLog()`（`executor/register_test.go:65`）
  断言出现过一次 `register:<键>`）。
- **新列表里有一条非法**（脚本路径越界）→ 返回错误，登记表与替身**一字未动**：
  逐条断言 `Keys()`、`HandlerNames()` 与调用前相等，`a.configCommands` 也保持旧值
  （再调一次 `Apply()` 时撞名判定仍按旧列表，用 §5.3 那条用例的形式验证）。
- **`enabled=false`** → 返回错误且不碰任何东西。
- **新增/重命名一条档位越出冻结许可 → 整次失败、一字不动**（这条是设计文档 §9"免重启新增档位"
  那句的验证面，兜底在 R04 手里）：fixture 冻结的 `workspace` 只允许 `scripts/` 下、
  `runtime_allow` 只含 `bash`。candidate 里既放宽顶层 `workspace`（改成能覆盖 `../elsewhere/`）、
  放宽 `runtime_allow`（加 `node`），又新增（或把一条既有档位改名后）一条档位，其 `script` 落在
  放宽后目录里但**越出冻结 workspace**、或其 `runtime` 用 `node`（**不在冻结 `runtime_allow`**）。
  判据：`ApplyConfig` 返回错误（走 §3.3 第 2 步的 `LoadProfiles`，用的是冻结的那份非 `Commands`
  取值而不是整份 candidate），且登记表 `Keys()`、替身 `HandlerNames()`、`a.configCommands`
  与调用前逐条相等——即使 YAML 里顶层许可字段已放宽，新档位也不能按新值建出来。
  这条守的是"新增条目可热更"被限定在既有许可范围内、不能借热更扩边界（§9、§12 新增风险行）。
- 连续两次 `ApplyConfig` 同一份列表 → 第二次 `Added`/`Removed` 全空、
  替身里每个键都只被重登记一次（可重入，与 `Applier.Apply` 的既有承诺同一条）。

### 5.3 两批共存时的判定（本卡最容易写错的地方，单独成用例）

fixture：config 有 `cfg_one`，store 有 `page_one`（不同名）。

1. `ApplyConfig` 加一条与 `page_one` 同名的 config 档位 → 生效表里 `page_one` 这个键归 config 来源，
   `page_one` 那条 store 条目进 `Degraded()`，替身里该键的处理函数是**新 config 那条**建的
   （判据：改变 store 那条的 timeout 不会影响生效表——用 `Registry.Lookup(key)` 拿到的
   `Profile` 的 timeout 等于 config 那条）。
2. 紧接着再 `ApplyConfig` 把那条撞名的 config 档位删掉 → 键位从生效表消失，
   `Degraded()` 空，替身里该键被摘除。
   **这条是设计文档 §12 那条风险的落点**：降级条目在 config 批次换掉之后不该"复活"成生效条目
   （复活需要一次 `Apply()` 重读文件才会发生，那是 store 路径的正常语义）。
   用例断言的是"本卡的行为"，注释里要写出这一点，别让人以为这是缺陷。
3. 上面之后调一次 `a.Apply()` → `page_one` 回到生效表（store 来源），
   证明两条链对同一张表的操作是收敛的。

### 5.4 与重启等价

§3.4 的第 1 条，写成可执行的断言：

```go
// TestApplier_ApplyConfigMatchesStartup：运行期换表与"改完 yaml 重启"必须给出同一张表。
func TestApplier_ApplyConfigMatchesStartup(t *testing.T) {
        f := newApplierFixture(t, "cfg_a")                 // 启动时 config 侧只有 cfg_a
        candidate := f.configWithCommands("cfg_a", "cfg_b") // 同一份非 Commands 取值 + 新列表
        _, err := f.applier.ApplyConfig(candidate)
        require.NoError(t, err)

        // 用同一份 candidate 走启动路径建一张全新的表
        fresh, err := NewRegistry(candidate, quietLogger())
        require.NoError(t, err)

        assert.Equal(t, fresh.Keys(), f.registry.Keys())
        for _, key := range fresh.Keys() {
                wantSource, _ := fresh.SourceOf(key)
                gotSource, _ := f.registry.SourceOf(key)
                assert.Equal(t, wantSource, gotSource, key)
                wantProfile, _ := fresh.Lookup(key)
                gotProfile, _ := f.registry.Lookup(key)
                assert.Equal(t, wantProfile.Timeout, gotProfile.Timeout, key)
        }
}
```

`f.configWithCommands(...)` 是本卡给 fixture 加的一个方法：复制 `f.exec` 里那份**冻结的**
非 `Commands` 取值、只换 `Commands`，再装回一个 `core.Config` 返回——
这正是 §3.3 第 2 步的那次合成，用例与实现共用一个构造点，避免出现"测试造出一份实现拿不到的配置"。
`Profile.Timeout` 是核对过的字段（`executor/profile.go:126`）。

### 5.5 并发

一个协程反复 `ApplyConfig` 两份不同的列表（来回切），另一个协程反复 `Apply()`，
第三个协程读 `registry.List()` 与 `syncer.HandlerNames()`。跑 20 轮，
`-race` 必须干净，且结束时最后一批 `Keys()` 里每个键在替身里都有处理函数、
且替身里没有多余的 `exec.` 键（**终态自洽断言**，不是过程断言）。

### 5.6 手工（接线之前先单包验一次）

`go test ./executor -race -count=5 -timeout 30m`。本卡不改 `api`，也不起进程冒烟。

## 6. 完成标准（DoD）

- [ ] `Registry.ApplyConfig` 存在，四条规则各有用例（整表替换、同批重复拒绝、撞名降级、关闭时拒绝）。
- [ ] `Applier.ApplyConfig` 存在，七条行为各有用例（§5.2 全部），
      失败点全部在动手之前（那条"非法即整次不动"的用例逐条断言表与替身都没变）。
- [ ] `Applier` 的 `configCommands` 记账生效：`Apply` 的撞名判定与处理函数构造
      用的是当前列表而不是启动期列表（§5.3 那条用例是它的唯一证据，别省）。
- [ ] `Applier.Apply` 的行为未变：`executor/applier_test.go` 的既有用例**未经修改**即通过
      （只允许 fixture 增加，不允许改断言）。
- [ ] 普通任务的处理函数在任何路径上都不被触碰（§5.2 第二条显式断言）。
- [ ] `executors.enabled=false` 时两个入口都拒绝，不产生半状态。
- [ ] `cmd/server` 只改了 `Applier` 的构造条件与一个新的构造入口调用，
      `web_enabled=false` 的部署仍然不打开档位文件（`go test ./cmd/server -race` 的既有用例
      若有一条断言"关闭时不碰 profiles_path"，它必须仍然绿）。
- [ ] `go build ./...`、`go vet ./...`、`go test ./... -race -count=1` 全绿；新增文件已 `gofmt -w`。

## 7. 验收方式

```bash
go test ./executor -run 'TestRegistry_ApplyConfig|TestApplier_ApplyConfig' -v
go test ./executor -run 'TestApplier' -v
go test ./executor -race -count=5 -timeout 30m
go test ./cmd/server -run 'WebEnabled|Profile' -v
go build ./... && go vet ./...
go test ./... -race -count=1
```

预期：第一条列出 §5.1–§5.4 的用例；第二条证明既有 store 路径未被改动（用例数只增不改）；
第四条覆盖 `main_integration_test.go` 里"关闭时不装配"那几条既有断言。

## 8. 不在本任务范围

- 不做 `executors.enabled`/`required_role`/`workspace`/`runtime_allow`/`env_allow`/
  两个 timeout/`output.*` 的运行期改动（重启档与拒绝档）。
- 不做处理函数闭包的"部分重建"：本卡一律整批重登记（一次闭包构造只是把档位与产物存储绑在一起，
  不碰任何进程，代价与它的正确性相比不用考虑——`executor/applier.go:172-174` 已说过同一句话）。
- 不动 `api` 的写端点与 `GET /executors`（档位在线管理系列已交付，本卡不改其语义）。
- 不做"页面档位与配置档位的写锁合并成一把全局锁"：共用 `Applier.writeMu` 就够（§5.5 证明它串行）。
- 不改崩溃恢复守卫（§3.4 说明它无需改动，用例只是守住这个结论）。

## 9. 风险与回滚

| 风险 | 说明 | 处置 |
| --- | --- | --- |
| 用整份新配置校验新档位 | 让重启档的 `workspace`/`runtime_allow` 参与校验，出现"新档位按新值、老档位按旧值"的分裂 | §3.3 第 2 步定死用冻结值合成；用例覆盖"新列表合法但新 `runtime_allow` 更宽"时仍按旧值判 |
| 只重登记 store 那批 | 改了 config 档位而闭包没换，表现是"接口显示新 timeout、执行按旧 timeout" | §5.2 第三条断言重登记发生；`ApplyConfig` 用抽出来的"两批全登记"片段而不是复制一份差集逻辑 |
| 摘除时误删普通任务 | 判据写成"不在新表里就摘"，代码注册的 `payment_check` 会被摘掉 | 摘除判据必须含 `core.ExecPrefix` 前缀检查（照 `applier.go:166-170`），用例显式断言 |
| 降级条目在新 config 批次下复活 | `ApplyConfig` 之后那条 store 条目既不在生效表也不在 `Degraded()`，页面看着"消失了" | §5.3 三条用例把预期行为钉住，并在注释里说明"要复活需要一次 store 路径的 Apply" |
| `Registry.executors` 被顺手改成可变 | 给未来的误用留入口，且 `-race` 会当场抓到 `List()` 的无锁读 | §3 第 3 项的落点说明；本卡 `Registry` 的字段集合零变化 |
| `ApplyConfig(nil)` 被当成 no-op | 删掉最后一条档位时不生效 | §5.1 最后一条用例 |

回滚：登记表与同步器的两个入口都是纯新增，`Applier.Apply` 只改了两处取值来源，
`cmd/server` 只改了构造条件。整卡 `git revert` 即可；回滚后页面档位在线管理（W 系列）行为不受影响。

## 10. 实现记录（执行时补写）

本节覆盖本卡的两半：前半已落地的 `Registry.ApplyConfig` 与 `executor/registry_apply_config_test.go`，
以及后半的 `Applier.ApplyConfig`、`configCommands` 记账、`NewConfigApplier`、`main.go` 构造条件。

### 10.1 落地的接口（含 `ApplyConfig` 是否带 `commands` 参数）

按符号名定位，不写行号（行号会随改动漂移）。

- `executor/registry.go`：`func (r *Registry) ApplyConfig(profiles []*Profile) error`（前半落地）。
  签名不带 `commands []core.ExecutorCommand` ——卡 §3.1 就预计大概率收不到读取方，最终确认没有读取点，
  按 YAGNI 删掉该参数。探测在 `ApplyConfig` 内部对每条档位现调 `Probe(profile)`，口径与 `NewRegistry` 相同。
- `executor/applier.go`：
  - `Applier` 新增字段 `configCommands []core.ExecutorCommand`（受既有 `a.writeMu` 保护）。
  - `func (a *Applier) executorsNow() core.ExecutorsConfig`：非 `Commands` 恒为冻结值、`Commands` 换成 `configCommands`，
    只能在 `writeMu` 内调用。
  - `func (a *Applier) syncHandlers(reRegister []string, live map[string]bool) (added, removed []string)`：
    `Apply` 与 `ApplyConfig` 共享的"重登记 + 摘除"片段，差集逻辑只有一份。
  - `func (a *Applier) ApplyConfig(candidate core.Config) (ApplyResult, error)`（后半主入口，§3.3 四步）。
  - `func NewConfigApplier(syncer HandlerSync, registry *Registry, artifacts *ArtifactStore, logger *slog.Logger) (*Applier, error)`：
    不接 store 的构造入口；`store` 为 nil 时 `Apply()` 返回明确错误而不是 panic。
  - `NewApplier` 保留要求 store 非 nil，并把 `configCommands` 初始化为 `registry.executors.Commands`。
  - `Validate` 未改代码路径，仍在 `writeMu` 之外读冻结的 `a.registry.executors`，取舍写进了方法注释。
- `executor/applier_test.go`（只增不改）：
  - `func newConfigApplierFixture(t, configNames ...string) *applierFixture`：在 `newApplierFixture` 之上多预登记一个
    不带前缀的普通任务键 `payment_check`。
  - `func (f *applierFixture) configWithCommands(t, names ...string) core.Config`：复制冻结的非 `Commands` 取值、
    只换 `Commands` 的合成配置构造点，实现与用例共用（§3.3 第 2 步的那份合成）。
- 测试文件：
  - `executor/registry_apply_config_test.go`（前半，7 个用例）。
  - `executor/applier_config_test.go`（后半，§5.2/§5.3/§5.4/§5.5 共 11 个用例，含 `NewConfigApplier` 的 nil-store 用例）。
  - 两个新增文件合起来 18 个 `func Test`。
- `cmd/server/main.go`：唯一一处改动——`Applier` 的构造条件从 `if cfg.Executors.WebEnabled` 改成
  `if cfg.Executors.WebEnabled || (cfg.Reload.Enabled && cfg.Executors.Enabled)`，`web_enabled=false` 一支用
  `NewConfigApplier`（档位文件保持关闭）。`profileStore` 的打开条件未改（仍只看 `web_enabled`）。
- `cmd/server/profile_merge_test.go`（修复轮增补）：新增 `TestRun_ReloadWithoutWebBuildsConfigOnlyApplier`，
  给上面那支放宽后的分支补装配证据。为拿到调度器侧的注册结果，把既有的 `runWithCapturingServer`
  拆成"薄壳 + `runWithCapturingServerAndScheduler`"（后者多交回那份替身调度器），
  既有两条用例的调用方式与断言一字未改。

### 10.2 与本卡写法的差异

1. `ApplyConfig(profiles)` 不带 `commands`（§3.1 已预计，见上）。`Applier.ApplyConfig` 只为自己的 warn 日志探测一次，
   登记表内部会再探一次，两处同一个 `Probe`，结论不分叉——卡 §3.3 第 3 步"调用方与登记表重复探测"是有意为之，不是遗漏。
2. `Registry.ApplyConfig` 会随进来的那一批重算降级视图：一条 store 条目因 config 条目压制而降级时，
   只要压制它的那条还在这批里就仍降级；那条件从这批里消失时，降级行也一起消失——它不会"复活"回生效表
   （复活要一次 store 路径的 `Apply()` 重读文件）。这是 §5.3 case 2 的既定行为，`applier_config_test.go` 的注释里写清了，
   不是缺陷。
3. 共享片段 `syncHandlers` 把卡 §3.2/§4 里 `Apply` 的 `keepConfig` 变量折进了入参：`Apply` 的"config 键不摘也不重建"
   由两个入参表达——重建集合只给 `storeKeys`、`live` 集合含全部生效键（config 天然被放过）。摘除判据两条链一致
   （"有 `exec.` 前缀又不在生效表里"），语义与既有 `keepConfig` 等价。`Apply` 没有改成调用 `ApplyConfig`（范围不同）。
4. `ApplyResult` 的实际字段是 `Config / Stored / Degraded / Added / Removed / Warnings`，卡 §3.3 期望的形状吻合；
   多出的 `Warnings` 在 `ApplyConfig` 里恒为空——config 侧走 `LoadProfiles` 的严格连坐，一条非法即整次失败，
   没有 store 侧那种"单条跳过、其余进表"的逐条告警可记。
5. `configWithCommands` 需要创建脚本文件，因此方法签名带 `t`（卡 §5.4 示例里省略了）；这是测试辅助函数，不影响实现。
6. `configCommands` 存的只是 `candidate.Executors.Commands` 的**最外层切片副本**：调用方之后对自己那份列表
   做增、删、换顺序都动不到这份记账，但元素是值拷贝，每条命令里的 `FixedArgs` / `Args` / `ArgsRender` /
   `EnvAllow` / `RetryOnExit` 这些内层切片（以及 `Env` / `Headers` 映射）仍与调用方共享同一份底层数组，
   **原地改某一条的元素会连记账一起改**。今天唯一的 candidate 生产方（热重载链）每次都把配置文件整份重新解析、
   交来一批全新的命令对象，不会留着旧列表去原地改，所以这里不做深拷贝；
   `applier.go` 的注释按这条真实边界写，不再写成"存一份副本就隔离了"。
7. `Applier.ApplyConfig` 的方法注释原先把 `env_allow` 与 `workspace` / `runtime_allow` / 两个 timeout 并列，
   读起来像"四者都由第 2 步的 `LoadProfiles` 按冻结值把关"。核实结果：`executor/profile.go` 的校验路径
   从不读全局 `ExecutorsConfig.EnvAllow`（它只在执行时被 `executor/env.go` 的 `BuildEnv` 读；
   参与校验的是每条档位自己声明的 `cmd.EnvAllow`，检查点在 `buildProfile` 与 `args.go`）。
   注释已改成实际机制：`workspace` / `runtime_allow` / 两个 timeout 由 `LoadProfiles` 按冻结值拒绝，
   `env_allow` 不在那份校验里，而是随 `executorsNow()` 钉进每个处理函数闭包、在执行时生效——
   所以新 YAML 里放宽的 `env_allow` 到不了已经建好的档位，安全实质不变。
8. 同一条声明在 `executor/registry.go` 的 `ApplyConfig` 注释里**并不存在**（评审引用的那一处讲的是
   "快照只做一次 Load"的读侧语义），因此那一处一字未改。

### 10.3 验证证据

`gofmt -w` 逐个文件跑：`executor/applier.go`、`executor/applier_config_test.go`、`executor/applier_test.go`、
`cmd/server/main.go`，修复轮再加 `cmd/server/profile_merge_test.go` 与 `cmd/server/main.go` 的格式化复查
（`executor/registry.go` 本轮一字未改，见 §10.2 第 8 条）。repo 级 `gofmt -l` 因 CRLF 会误报，只判自己改的文件，
上述文件 `gofmt -l` 均无输出。

1. `go test ./executor -run 'TestRegistry_ApplyConfig|TestApplier' -v`：全绿。
   既有的 `TestApplier_*`（来自 `applier_test.go`）在断言一字未改的前提下通过，新增的 `TestApplier_ApplyConfig*`
   与 `TestApplier_NewConfigApplier*` 也全过。
2. `go test ./cmd/server -run 'WebEnabled|Profile|Reload' -v`：14 条全过，其中
   `TestRun_WebDisabledNeverTouchesTheProfilesPath` 与 `TestRun_ProfileDependenciesAreAbsentWhenDisabled`
   证明"关闭时不装配 / 不碰 profiles_path"仍在，`TestRun_ReloadWithoutWebBuildsConfigOnlyApplier`
   是修复轮新加的分支证据（web_enabled=false + reload.enabled=true + executors.enabled=true：
   档位存储闭包一次都没被调用、服务拿到的 `profileStoreAPI` 是 `store=nil` 且 `applier≠nil`、
   `exec.alpha` 照旧注册）。注意卡 §7 那条 `WebEnabled|Profile` 过滤器选不中这条新用例，
   要带 `Reload`；带 `Reload` 的过滤器还会顺带跑上 R01/R06 的三条冒烟，它们也全过。
3. `go test ./executor -race -count=5 -timeout 30m`：修复轮复跑 `ok 108.1s`（wall `1m52s`），`-race` 干净
   （首落地那次是 `ok 105.4s` / wall `1m48s`；交付前第三次复核复跑 `ok 109.929s`，三次都是同一份终态字节）。
4. `go build ./... && go vet ./...`：无输出（成功）。
5. `go test ./... -race -count=1 -timeout 30m`：五个包 `ok`，修复轮复跑
   （api 148.6s / cmd/server 6.4s / core 13.3s / executor 22.8s / store/sqlite 3.9s，wall `2m34s`；
   首落地那次 api 99.1s / cmd/server 6.4s / core 13.5s / executor 22.4s / store/sqlite 3.9s，wall `1m44s`；
   交付前第三次复核复跑 api 111.0s / cmd/server 6.4s / core 13.7s / executor 22.9s / store/sqlite 4.2s）。

变异反向验证（7 个变异，逐个"破坏 → 跑对应用例 → 确认报红 → 从字节副本还原"）：

| 编号 | 变异 | 判红的用例 | 报红信息 |
| --- | --- | --- | --- |
| M1 | 第 2 步用整份 candidate 校验（丢掉冻结的非 `Commands` 取值） | `TestApplier_ApplyConfig_ValidatesAgainstFrozenPermissions` | `An error is expected but got nil.` |
| M2 | `ApplyConfig` 不重建 config 闭包（`syncHandlers` 的重登记集合传 `nil`） | `TestApplier_ApplyConfig_AddsNewProfile` | `[]string(nil) does not contain "exec.cfg_b"` |
| M3 | 摘除丢掉 `core.ExecPrefix` 判据（普通任务键也被摘） | `TestApplier_ApplyConfig_RemovesDroppedProfileAndLeavesOrdinaryKeys` | `Should be true`（`payment_check` 不在了） |
| M4 | 失败批次也把 `configCommands` 提前推进 | `TestApplier_ApplyConfig_IllegalEntryLeavesEverythingAlone` | `Not equal`（记账长度变了） |
| M5 | `NewConfigApplier` 出来的 `Apply()` 走 nil 解引用而不是报错 | `TestApplier_NewConfigApplierStorePathErrorsNotPanics` | `panic: ... nil pointer dereference` |
| M6 | `Apply` 读启动期冻结列表而不是 `configCommands` | `TestApplier_ApplyConfig_CoexistsWithStoreSide` | `Received unexpected error`（`ApplyStore` 硬撞名报错） |
| M7 | 修复轮：把 `cmd/server/main.go` 的构造条件退回 `if cfg.Executors.WebEnabled {`（放宽的那一支 reload 分支消失） | `TestRun_ReloadWithoutWebBuildsConfigOnlyApplier` | `profile_merge_test.go:329: Expected value not to be nil.` + `Messages: reload 打开的部署要拿到一份配置侧热更依赖`（`got` 整个是 nil，跑不到后两条断言） |

字节同一性：每个变异用 `open(p,'rb')`/`open(p,'wb')` 从原始字节还原；M1–M6 那六个跑完后
`sha256sum executor/applier.go` 每次都回到当时那份基线
`af480b70ebb452687ecea08d603d804bdc7bff6ab293920301c07199fed4a357`（§10.2 第 6 条说的外层切片副本当时已在里面，
M1–M6 是对那份交付文件跑的）。修复轮把 `applier.go` 的两处注释（§10.2 第 6、7 条）改掉之后，
该文件的新基线是 `0d1de289be4b251cc8f5702b3a2fd6f75f303310fbb4139bb622fd5bb1818cac`。
**这份新基线之上又重跑过 M1**（承重的那条：第 2 步改用整份 candidate 校验）：
`TestApplier_ApplyConfig_ValidatesAgainstFrozenPermissions` 仍在
`executor/applier_config_test.go:179` 报红（`An error is expected but got nil.`），
还原后 sha256 仍是 `0d1de289…18cac`、逐字节相同。M2~M6 未在新基线上重跑——
修复轮只动了两处注释与该文件的文本，没有触及它们的判据路径。
M7 动的是 `cmd/server/main.go`：改前 `f09ef2b74ecbe27811364ff165aabe76527849421dcca94b88a214d056655a84`，
变异态 `a04bc3cddca9842608718efa101dc44ba28ad170be83921f5f1c35da89c20e11`，
从字节副本还原后回到 `f09ef2b74ecb…55a84`（与改前逐字节相同，37961 字节）。
文本 round-trip 会因 CRLF 破坏该仓文件，所以全程只用二进制 I/O。

### 10.4 手工验收

本卡不改 `api`，也不起进程冒烟（触发链在 R05/R06）。改而在单包层面手工验了一次 §5.6：
`go test ./executor -race -count=5 -timeout 30m`（见 §10.3 第 3 条），并针对 `main.go` 的构造条件放宽跑了
`go test ./cmd/server -run 'WebEnabled|Profile|Reload' -v`（14 条全过，含修复轮新加的
`TestRun_ReloadWithoutWebBuildsConfigOnlyApplier`）确认关闭态不装配、打开态不碰档位文件、
reload 态只建不接 store 的同步器。

### 10.5 缺陷

| # | 说明 | 处置 | 归属 |
| --- | --- | --- | --- |
| D-R0401 | 卡 §5.2 长条目建议"放宽 `workspace` 后新档位越出冻结 workspace"作为兜底判据，但 `LoadProfiles` 走 `PathWithinWorkspace` 严格模式，无条件拒绝绝对路径与 `..`（`resolveInside` 前两层），换 `workspace` 取值并不会改变某条相对写法的合法性。真正能随顶层取值放宽而改变合法性、从而证明"用的是冻结值"的是 `runtime_allow`。用例据此把判据落在 `runtime`（`missingProgram` 不在冻结白名单、放宽后在里面），并同时放宽 `workspace` 只为证明它压根没参与校验 | 已在用例注释写明，非代码缺陷，不修 | 本卡（文档收口留 R07） |
| D-R0402 | D-R0103（来自 R01）：无名/重名档位在 `core.Diff` 里塌成 `#<i>`。R04 的答复是"整表替换按 `LoadProfiles` 的结论判"。核实：`LoadProfiles` 会拒绝空名（`name must not be empty`）与重名（`duplicate profile name`），所以 `ApplyConfig` 收到的档位永远是有名字且唯一的，`Registry.ApplyConfig` 自身的同批 `HandlerKey` 去重只是兜底。`#<i>` 这种形状是 `Diff` 侧的展示塌缩，不会作为 `*Profile` 进到 `ApplyConfig`，因此 `ApplyConfig` 的去重天然覆盖不到它、也无需覆盖 | 登记不修：口径正确，R06 接线时 `Diff` 的 `#<i>` 只作日志/拒绝档判据，生效判定以本卡的 `LoadProfiles` 为准 | R06 |
| D-R0403 | `NewConfigApplier` 的 nil-store 部署里 `Apply()` 恒返回错误。这是 P2 拍板的结果（web 关闭不打开档位文件），但未来若 reload 需要顺带重放 store 侧，会撞上这条错误 | 登记不修：那种部署本就没有 store 条目；要重放需先打开 `web_enabled` | R06 |
| D-R0404 | 设计文档 `docs/design/config-reload-design.md` §12 那张风险表（以及 §9 的同一句）把免重启新增档位的兜底写成"`ApplyConfig` 加载新批次固定用冻结的顶层 `workspace`/`runtime_allow`/`env_allow` 做 `LoadProfiles` 越界拒绝"。核实：`LoadProfiles` 只按 `workspace`/`runtime_allow`/两个 timeout 与档位自己的 `env_allow` 校验，**全局 `executors.env_allow` 从不进那次校验**（唯一读取点是执行时的 `executor/env.go` 的 `BuildEnv`），兜住它的实际机制是"冻结值随 `executorsNow()` 钉进处理函数闭包"。代码侧注释已按实际机制改写（§10.2 第 7 条） | 登记不修：R04 不改设计文档，文档同步留 R07 收口 | R07 |

### 10.6 未覆盖项

- 没有触发方：`ApplyConfig` 在本卡没有调用者（R05 监听、R06 重载链才接线）。本卡只在单包层验证这条链自身正确，
  没验"文件变了自动走到 ApplyConfig"——那是 R05/R06 的范围。
- 进程级冒烟未做（§10.4）：`cmd/server` 这一侧覆盖的是**装配可达性**，而且是修复轮才真正补上的——
  `TestRun_ReloadWithoutWebBuildsConfigOnlyApplier` 走的就是放宽后那一支
  （`web_enabled=false && reload.enabled=true && enabled=true`），断言 `NewConfigApplier` 被选中、
  档位文件的构造闭包一次都没被调用、服务拿到 `store=nil` 且 `applier≠nil`、`exec.alpha` 照旧注册。
  仍未覆盖的是"起一个真进程、改一次配置文件、看它自动重载"——那要 R05 的监听器与 R06 的重载链接线，
  本卡的 `ApplyConfig` 到现在也没有生产调用方（见上一条）。
- 探测"不可用"分支在 `ApplyConfig` 里只走 warn 日志、没有断言：`selfExecutable` 恒可用，构造不出稳定的不可用档位
  而不引入本机依赖；`Registry.ApplyConfig` 的探测口径已有 `registry_apply_config_test.go` 覆盖（同批重复、空批等）。
- `ApplyResult.Warnings` 在 `ApplyConfig` 恒空（§10.2 第 4 条），因此没有针对它的用例——它设计上就没有可填的内容。
