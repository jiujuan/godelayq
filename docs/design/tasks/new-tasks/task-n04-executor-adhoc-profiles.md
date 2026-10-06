# TASK-N04　executor：构造并注册四条自由执行档位

- 所属阶段：M1 配置与档位
- 依赖任务：TASK-N03
- 涉及文件：`executor/adhoc.go`（新增）、`executor/adhoc_test.go`（新增）、`executor/profile.go`、
  `executor/register.go`、`executor/registry.go`、`executor/merge_profiles.go`、`cmd/server/main.go`、
  对应 `_test.go`
- 预计规模：大

## 1. 任务目标

按 N03 的配置构造出四条内置档位 `exec.php`、`exec.python`、`exec.shell`、`exec.http`，
注册进调度器，让它们出现在 `/job-types` 与 `/executors` 里（元数据字段在 N06 加）。
本卡结束时这四条键"存在且可查"，但还不能带位置执行（N05 负责 payload）。

## 2. 背景与当前问题

现在每条可执行档位的路径/URL 都在档位定义里写死：`executors.commands[].script` / `program`
（`core/config.go:303-305`）、`url_template`（`core/config.go:334`），并且编辑时禁止更换
（`web-profile-design.md:38` 的 D7）。需求③要的是"位置由任务给"，所以这四条档位**天生没有**
`script`/`url_template`，这与构造函数 `checkFieldsMatchKind`（`executor/profile.go:501`）
和 `buildProfile`（`:286`）的现有要求正面冲突。

必须解决的四件事：

| 问题 | 现位置 | 处理方向 |
| --- | --- | --- |
| 构造函数要求 script 档位必须有 `script`、http 档位必须有 `url_template` | `executor/profile.go:501` `checkFieldsMatchKind`、`:426` `fillHTTPProfile` | 加一个"内置 adhoc"的标记，让这两条要求对它绕开；**不改判据本身**，否则用户自建档位也能少写脚本路径 |
| 路径解析模式要允许任意本机路径 | `executor/profile.go:44-56`（`PathWithinWorkspace`/`PathAnywhere`）、`:945` `resolveProfilePath` | 内置档位按 `PathAnywhere` 构造（`resolveAnywhere`，`:959`），但此刻还没有具体路径可解析——所以这四项的**路径校验发生在提交期**（N05），本卡只定模式与解释器 |
| 键冲突策略 | `executor/register.go:71-77`（撞名直接启动失败）、`executor/merge_profiles.go:39`（同名降级） | §D15：内置条目让位 + 一行日志，不能因为运维打开 adhoc 开关就把用户已建好的 `exec.php` 档位搞得启动失败 |
| 探测对象 | `executor/probe.go:44` `Probe`（script 档位探脚本文件）、`:69` `probeScript`、`:80` `probeProgramName` | 内置档位没有具体文件可探，只探解释器：`probeScript` 这一支对它绕开（§10 风险 5） |

## 3. 要实现的功能

### 3.1 新文件 `executor/adhoc.go`

```go
// adhocSpec 是一条内置自由执行档位的静态定义。
type adhocSpec struct { ... }

// AdhocProfiles 按配置构造内置档位。返回的条目顺序固定（php、python、shell、http），
// 接口与启动日志的输出因此稳定。
func AdhocProfiles(cfg core.Config, mode PathMode) ([]*Profile, []string, error)
```

- 返回的第二个值是"因配置或环境不满足而没构造出来的键 + 原因"，交给调用方打日志。
- 构造仍然**合成一份 `core.ExecutorCommand` 再交给 `BuildProfile`**（`executor/profile.go:257`），
  不在别处复制字段检查（`web-profile-design.md` D3 的"一份规则"）。
- 四条定义：

| 键 | kind | 来自配置 | 附加定义 |
| --- | --- | --- | --- |
| `exec.php` | script | `Runtime=php` | 位置键 `script`，扩展名 `.php` |
| `exec.python` | script | `Runtime=python` | 位置键 `script`，扩展名 `.py` |
| `exec.shell` | script | `Runtime=executors.adhoc.shell_runtime` | 位置键 `script`，扩展名 `.sh`/`.bash` |
| `exec.http` | http | `Timeout=executors.adhoc.http_timeout` | 位置键 `url`，`Method`/`Body` 按 §12 P5（默认 `POST`+`json`），`DenyPrivate=!url_allow_private`，`AllowedHosts=adhoc.url_hosts` |

- 解释器不在 `runtime_allow`（`core/config.go:234`）内的条目**不构造**，落进第二个返回值。
  使用者把 `runtime_allow` 收成 `[bash]` 时，页面上就只剩 Shell 与 HTTP 两条，这是预期。
- `exec.http` 的 `AllowedHosts` 为空是合法取值（§D9 第②层放宽），
  而 `checkAllowedHosts`（`executor/profile.go:744-746`）现在写死"http 档位不许空"。
  绕开方式：给 `buildProfile` 那条判据加 adhoc 分支，**同时**给"空列表"补一条启动 warn 的条件
  （由 N06 打），并把这条分工写进注释。

### 3.2 `Profile` 上的标记

`executor/profile.go:102` 的 `Profile` 加一个字段：

```go
// Adhoc 为 true 表示这是内置的自由执行档位：执行位置来自任务的 payload，
// 因此档位定义里没有 script/url_template，探测只判解释器。
Adhoc bool
```

用它做判据的四处（本卡改前两处，后两处属 N05）：

| 用途 | 位置 | 本卡 |
| --- | --- | --- |
| 字段与 kind 的组合校验 | `executor/profile.go:501` | 改 |
| http 档位的 URL/主机校验 | `executor/profile.go:426` `fillHTTPProfile`、`:744` | 改（绕开"必须有模板"和"主机不许空"） |
| argv 组装 | `executor/args.go:531` `Render` | 留给 N05 |
| URL 组装 | `executor/http.go:247` `fillTemplate` | 留给 N05 |

`Probe`（`executor/probe.go:44`）加判据：`p.Adhoc` 时只走 `probeProgramName`（`:80`）那一支，
`Available` 只取决于解释器在不在；`Reason` 文案要说明"位置由任务给出，这里只判解释器"，
否则运维会在 `/executors` 里看到一条"可用"却不知道它能跑什么。

### 3.3 注册与合并

- 新函数 `executor.RegisterAdhoc(...)`（照 `Register` 的形状，`executor/register.go:52`），
  或给 `Register` 加一个输入参数：把内置条目并入同一批注册循环，共用
  `RegisterHandlerClass(key, Handler(...), core.JobClassExec)`（`executor/register.go:87`）。
  内置条目的执行主体与用户档位**完全同一条路径**（§口径 1）。
- 撞名策略（§D15）：内置条目与 `executors.commands` 或档位文件里同名键冲突时，
  内置那一条不注册、记一行 `Info`/`Warn`，并把冲突写进 `Registration` 的计数
  （`executor/register.go:25-36` 那个结构，加 `AdhocSkipped int`）。
  **启动照常成功。**
- 内置条目**不进** `Registry` 的存储侧快照：`executor.Applier`（`executor/applier.go`）
  在档位表热更新时只重建用户档位，不能把四条内置的冲掉，也不能重复注册。
  实现上给 `Registry` 加一个"内置那一份"的独立视图（如 `AdhocEnabled bool` +
  `AdhocKeys() []string`），读侧合并两份来源。
- 装配：`cmd/server/main.go:895` `registerHandlers` 里把内置条目传进去，
  配置关闭时传空列表——于是全链路零变化（§口径 4）。

### 3.4 键名与显示

- 键名固定四条，见 §12 P2：档位名字符集不含点（`core/executor_profile_store.go:22`），
  所以 `exec.adhoc.php` 这类写法不合法，必须避用。
- `/job-types` 的输出因此自然多出四项（`core/scheduler.HandlerNames()`，`core/scheduler.go:387`），
  本卡不需要改那个端点（`api/handlers.go:528`）。
- 界面标签（"PHP 脚本"这类）由前端按 `key`/`kind`/`runtime` 生成（N07），
  本卡不引入后端文案表。

## 4. 实现步骤

1. 先在 `executor/adhoc_test.go` 写出目标形状的红色用例：四条键、顺序、解释器过滤、撞名让位。
2. `Profile` 加 `Adhoc` 字段；改 `checkFieldsMatchKind`（`:501`）与 `fillHTTPProfile`（`:426`）的分支，
   跑一遍 `go test ./executor` 确认既有用例一条不改、全绿。
3. 写 `AdhocProfiles`：合成 `core.ExecutorCommand` → 调 `BuildProfile`（`:257`）→ 打 `Adhoc` 标记。
4. `Probe`（`executor/probe.go:44`）加 adhoc 分支。
5. 注册接线：`Register`/`RegisterAdhoc` + `Registration.AdhocSkipped`；
   `Registry` 加内置视图；`cmd/server/main.go:895` 装配传参。
6. `gofmt -w` 新改文件；`go test ./executor ./core`；`go build ./... && go vet ./...`。

## 5. 测试要求

| 用例 | 断言 |
| --- | --- |
| 默认关闭 | `adhoc.enabled=false` 时 `AdhocProfiles` 返回空；`/job-types` 不含 `exec.php`；既有执行器用例零改动全绿 |
| 打开构造 | 四条键、固定顺序、`Kind`/`Runtime` 与 §3.1 表一致；`Profile.Adhoc==true`；`script`/`url_template` 为空**不报错** |
| 解释器过滤 | `runtime_allow:[bash]` → 只有 `exec.shell` 与 `exec.http`，第二个返回值说明另外两条为什么没构造 |
| shell_runtime 生效 | `shell_runtime: pwsh` → `exec.shell` 的 `Runtime=pwsh` |
| http 空主机 | `url_hosts:[]` → 构造成功（内置专属绕开）；同时断言用户自建的 http 档位空主机仍然报错（防越界放宽） |
| 撞名让位 | 配置里有同名 `executors.commands` 条目 → 内置那条不注册、用户那条生效、启动成功、`AdhocSkipped=1` |
| 探测 | 内置 script 档位：解释器在 PATH 时 `Available=true`，`Reason` 提到"位置由任务给出"；不在 PATH 时 `Available=false` 且原因指向解释器而不是文件 |
| 执行池类别 | 四条都以 `core.JobClassExec` 注册（`core/scheduler.HandlerClass`，`core/scheduler.go:373`） |
| 跨包名规则 | `executor/profile_name_parity_test.go` 的体例：内置键名同时被 core 与 executor 两侧的规则接受 |
| 热更新不冲掉 | 调 `Applier.Apply` 换掉用户档位表后，四条内置键仍在注册表里（回归 §3.3 那条承诺） |

## 6. 完成标准（DoD）

1. 四条内置档位可由配置打开并注册成功，`Scheduler.HandlerNames()` 里出现它们。
2. `Profile.Adhoc` 只在 §3.2 前两处生效；`grep -n "\.Adhoc" executor/*.go` 的命中逐条有注释支撑。
3. 用户自建档位与 `executors.commands` 的行为一字未变：`executor/profile_test.go`、
   `executor/build_profile_modes_test.go`、`api/handlers_executor_profiles_test.go` 均未修改且全绿。
4. 撞名时启动成功且计数可见（`Registration.AdhocSkipped`），日志一行说清是哪条键让位。
5. 关闭时全仓零变化；`go test ./executor ./core ./api -race -timeout 30m` 绿。
6. 没有新增执行通路：内置条目的处理函数由现成的 `executor.Handler`（`executor/register.go:139`）
   构造（`grep` 证明内置注册处没有 `exec.Command`）。

## 7. 验收方式

```bash
go test ./executor -run "Adhoc" -v
go test ./executor ./core ./api -race -timeout 30m
go build ./... && go vet ./...
```

手工（可选，临时目录冒烟）：

```bash
GODELAYQ_EXECUTORS_ENABLED=true GODELAYQ_EXECUTORS_ADHOC_ENABLED=true \
GODELAYQ_SERVER_AUTH_TOKEN=<临时 token> go run ./cmd/server -config <临时配置>
curl -s -H "X-Auth-Token: <临时 token>" http://127.0.0.1:<端口>/api/v1/job-types
```

预期：`types` 里出现 `exec.http`、`exec.php`、`exec.python`、`exec.shell`（按字典序）。
注意本机没有 PHP/Python 时，`exec.php`/`exec.python` 仍会注册但 `runtime_ok=false`（§3.2 探测语义）。

## 8. 不在本任务范围

- 不校验 payload 里的路径与 URL、不改 argv/URL 组装（N05）。
- 不给 `GET /executors` 加 `adhoc`/`location` 字段（N06）。
- 不改前端（N07）。
- 不做"列出可选脚本"的预检接口（README S-4）。
- 不加按类型的细粒度授权（README S-5）。

## 9. 风险与回滚

| 风险 | 说明 | 退路 |
| --- | --- | --- |
| 放宽判据误伤用户档位 | "http 档位主机可以为空"串到普通档位上，等于放开所有配置的白名单 | §5 的"http 空主机"用例有负例对照；DoD 第 3 条要求既有用例零改动 |
| 内置条目被热更新冲掉 | 用户改档位后四条键消失，任务在 `executeJob` 里判 failed（`core/scheduler.go:1594-1611`） | §3.3 的独立视图 + §5"热更新不冲掉"用例 |
| 探测说"可用"但没人知道它跑什么 | 运维误判能力边界 | §3.2 要求 `Reason` 文案写明；N06 的 `location` 字段补齐界面侧说明 |
| `exec.php` 与用户已建档位撞名 | 静默让位会让人以为开关没生效 | 日志一行 + `AdhocSkipped` 计数 + N06 的 `/executors` 状态 |

回滚：`cmd/server/main.go` 的装配处停止传入内置条目即恢复原行为（其余代码在默认关闭下不走）。

## 10. 实现记录（执行时补写）

落地：`executor/adhoc.go`（`adhocSpec`/`adhocSpecs`/`AdhocSkip`/`AdhocProfiles`/`adhocCommand`）、
`executor/profile.go`（`Profile` 四个 adhoc 字段、`buildProfile` 的 adhoc 分支、
`checkFieldsMatchKind` 与 `fillHTTPProfile` 的放宽、`checkAllowedHosts` 拆出 `checkHostEntries`）、
`executor/probe.go`（`probeScript` 的 adhoc 分支）、`executor/registry.go`
（`SourceAdhoc`、`NewRegistry` 合入内置四条、`AdhocSkipped()`、`ApplyStore` 的让位分支）、
`executor/register.go`（`Registration.AdhocSkipped` 与那一行启动日志）、新测试 `executor/adhoc_test.go`。

| # | 与卡面的偏离 | 原因 |
| --- | --- | --- |
| 1 | 构造入口是 `AdhocProfiles(cfg core.Config)`，不是卡面 §3.1 的 `BuildAdhocProfiles(cfg, ec, mode)` | 与 `LoadProfiles(cfg core.Config)`（`executor/profile.go:217`）同形：整节配置一次归一化、workspace 与白名单都在内部取。多收两个参数只会让调用方有机会传进一份没归一化的 `ec` |
| 2 | 第二返回值是 `[]AdhocSkip`（键 + 原因），不是 `[]string` | 原因要给启动日志与 `GET /executors` 用（N06），一句 `exec.php skipped` 没有可操作方向 |
| 3 | 让位在**两处**生效，而不只是注册时：`NewRegistry` 里配置侧占键则内置不登记；`ApplyStore` 里页面建的同名档位直接顶掉内置那条 | 卡面 §3.1/§3.3 只写了"内置条目让位"，没写运行期新建的同名档位怎么办。方向必须一致，否则"启动时内置赢、页面上用户赢"。两处的可见性不同：前者进 `AdhocSkipped()` 与启动日志，后者表现在 `List()` 里那一行的 `source` 从 `adhoc` 变成 `store` |
| 4 | 运行期顶掉内置那条时，`AdhocSkipped` 计数不追加 | 那个视图在 `NewRegistry` 里一次写好、之后无锁只读（`Registry` 的读方法靠"只做一次 Load"保住自洽，`executor/registry.go:67-73`）。为它加锁不值：让位本身在展示面上看得见 |
| 5 | 内置 http 档位的 `CaptureResponse` 取 false | 卡面未定这一项。默认不采集对端响应正文，与"这一节整体是放宽、这一项不在放宽清单里"一致；要看响应用 `executors.commands` 建一条普通 http 档位 |
| 6 | 内置档位可用时探测的 `Reason` 也带一句"路径来自任务的 payload" | 既有档位可用时 `Reason` 是空的，所以这一句是新出现的。它只在接口的 `reason` 字段里，界面侧现在只在不可用时显示 `reason`（`web/src/components/jobs/JobForm.vue:444`），不构成回归；N06 的 `location` 字段才是界面侧的正解 |
| 7 | `buildProfile` 多收一个 `adhoc bool` 参数，而不是新增一个入口函数 | 放宽点只有三处（`script` 必填、`url_template` 必填、`allowed_hosts` 可为空），全部落在既有函数体内；再开一个入口等于把 1100 行的构造函数复制一遍 |
| 8 | adhoc 分支保留 `binary` 的必填检查（`program is required`） | 内置四条没有 binary。若一并跳过，将来加一条内置 binary 档位时会静默放过一个空 program |
| 9 | `checkAllowedHosts` 拆成"空列表要求" + `checkHostEntries`（每一项的写法） | 卡面 §3.1 第 4 条要求绕开空列表要求。拆函数比在内置档位那侧复制一份主机正则更守得住"一份规则"；用例 `TestAdhocProfiles_BuiltinHostsEntriesStillChecked` 钉的就是这一点 |

DoD 核对：

1. 四条键可由配置打开并注册成功：`TestNewRegistry_RegistersAdhocAndYieldsOnClash`
   断言 `registry.Keys()` 含 `exec.php`/`exec.python`/`exec.shell`/`exec.http`。
2. `Profile.Adhoc` 的分支只在 §3.2 前两处（构造与探测）生效；
   `grep -n "\.Adhoc" executor/*.go`（非测试）的命中处都有注释说明用途。
3. 用户档位与 `executors.commands` 行为未变：`executor/profile_test.go`、
   `executor/build_profile_modes_test.go`、`executor/probe_test.go`、
   `executor/registry_test.go`、`api/handlers_executor_profiles_test.go` 一条未改，
   整包 `go test ./executor ./api ./core ./cmd/server` 全绿（executor 26.5s、api 24.3s、
   core 20.2s、cmd/server 6.6s）；对照用例是 `TestAdhocProfiles_EmptyHostsAreAllowedForBuiltinOnly`
   （内置空主机通过、用户档位空主机仍然报 `allowed_hosts must not be empty`）。
4. 撞名让位有日志与计数：`Registration.AdhocSkipped` 进那一行 `executor handlers registered`，
   用例 `TestRegister_BuiltInProfilesUseExecPoolAndCountSkips` 断言计数为 1。
5. 关闭时零变化：`TestAdhocProfiles_DisabledByDefault`；上面那一整轮既有用例未改动即为证据。
6. 没有新增执行通路：`executor/adhoc.go` 里不出现 `exec.Command`，
   注册仍走 `executor/register.go` 的那一条 `RegisterHandlerClass(key, Handler(profile, …), JobClassExec)`；
   用例断言四条的 `classOf` 都是 `core.JobClassExec`。

本卡新增用例清单（`executor/adhoc_test.go`，15 个顶层/子用例全部通过）：
构造与顺序、解释器过滤、`shell_runtime` 生效、空主机只对内置放过、主机写法仍判、
扩展名要求可关、私网开关传到档位、探测只判解释器、登记表合入与撞名让位、
热整表替换不冲掉内置、页面同名档位顶掉内置、执行池类别与跳过计数、档位名跨包合法。
