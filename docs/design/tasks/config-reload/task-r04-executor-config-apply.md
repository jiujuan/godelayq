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
- [ ] `Applier.ApplyConfig` 存在，六条行为各有用例（§5.2 全部），
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

### 10.1 落地的接口（含 `ApplyConfig` 是否带 `commands` 参数）

### 10.2 与本卡写法的差异

### 10.3 验证证据

### 10.4 手工验收

### 10.5 缺陷

### 10.6 未覆盖项
