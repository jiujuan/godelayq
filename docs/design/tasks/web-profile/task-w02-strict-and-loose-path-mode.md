# TASK-W02　构造入口的严格与宽松两种路径模式

- 所属阶段：M0 基础
- 依赖任务：TASK-W01
- 涉及文件：`executor/profile.go`、`executor/probe.go`（只读确认，预期不改）、`executor/profile_test.go`、`executor/build_single_test.go`（新增）
- 预计规模：中

## 1. 任务目标

把档位定义的校验入口拆成可复用的形状：`executor` 包能只对**内存里的一条**档位定义执行
与启动期同一套校验，并且允许调用方选择"路径必须落在 workspace 内"（现状）或
"路径可以是本机任意位置"（页面建的档位）。本卡结束时 `config.yaml` 侧行为一字不变。

## 2. 背景与当前问题

校验今天长在一整节配置里：`LoadProfiles(cfg core.Config)`（`executor/profile.go:169-202`）
吃下整个配置，取 `Executors.Commands`、workspace（177）、`runtime_allow`（182-185），
逐条 `buildProfile`（205）。页面要保存一条档位时，需要判的就是这一条，但：

1. 没有单条入口，调用方只能拿整份 `core.Config` 拼一份"只含这条"的配置——
   这条路能走，但它同时会跑一遍重名检查与整节归一化，错误信息会指向 `executors.commands[0]`
   这种与调用方无关的位置。
2. `resolveInside`（867-888）无条件拒绝绝对路径、以分隔符开头的写法与 `..`。
   评审已决定页面建的档位**允许**指向 workspace 之外（设计文档 D5 ★），
   而 yaml 侧必须继续拒绝。所以这里需要一个显式的模式开关，而不是把检查删掉。

## 3. 要实现的功能

1. 导出模式类型：

   ```go
   type PathMode int
   const (
       PathWithinWorkspace PathMode = iota // 现状：解析后必须落在 executors.workspace 内
       PathAnywhere                        // 页面档位：允许本机任意绝对路径（设计文档 D5）
   )
   ```

2. 导出单条入口：

   ```go
   func BuildProfile(cmd core.ExecutorCommand, ec core.ExecutorsConfig, mode PathMode) (*Profile, error)
   ```

   内部完成 `Normalized()` 取值、workspace 解析、`runtime_allow` 集合构造，再调 `buildProfile`。
   它与 `LoadProfiles` **共用同一条构造路径**，不复制任何校验分支（I1）。
3. `buildProfile` 多收一个 `mode` 参数；`LoadProfiles` 恒传 `PathWithinWorkspace`。
   模式只影响三处路径字段的解析：`script`、`program`（workspace 内那种写法）、`cwd`。
   其余规则（名字、kind 组合、参数名与 pattern、`args_render` 占位符、secret、
   超时上下限、http 那组字段）两种模式**完全相同**。
4. `PathAnywhere` 下的路径解析（落地函数叫 `resolveAnywhere`，与 `resolveInside` 并列而不是替代，
   由 `resolveProfilePath(mode, ...)` 分流）：
   - 拒绝空值与纯空白（`cwd` 例外：留空表示 workspace 本身，与现状同一条）。
   - **相对写法仍以 workspace 为基准**（实施时定下的口径，卡原文只说"做 `filepath.Abs`"）：
     模式只放宽"能在哪儿"，不重新定义"相对谁"，否则同一句 `scripts/x.py` 在两份来源下指向两处。
   - 根写法（`/` 或 `\` 开头）按根解析，Windows 上由 `filepath.Abs` 补当前盘符；
     已带盘符/UNC 的直接清洗。判定"是不是根写法"用的是与 `resolveInside` 同一套写法检查，
     因为 Windows 的 `filepath.IsAbs("/tmp/x")` 返回 false。
   - 符号链接**不**作为越界判定依据（没有根可判）。
   - `binary` 档位里 `program` 精确命中 `runtime_allow` 时仍按 PATH 程序名处理，
     这条判定与模式无关（现状在 `executor/profile.go:320-333`）。
5. 宽松模式的产出要能被下游直接消费，不改下游：
   - `Probe` 只看 `Profile` 里的绝对路径与相对写法（`executor/probe.go:69-78、92-111`），
     不需要知道模式。本卡用 `TestProbeConsumesLooseProfileWithoutChanges` 证明了这点，
     `executor/probe.go` 一行未改（`git diff` 里没有这个文件）。
   - `ScriptRel`/`ProgramRel` 的事实与卡的原文不同，**W07 必须按这一条读**：
     `relativeTo`（`executor/profile.go:948-954`）只在"算不出相对路径"时兜底成文件名 ——
     也就是 Windows 跨盘符那一种；同盘越界时它老实给出 `..\..\srv\report\main.py` 的上跳形式。
     所以 **`ScriptRel` 不能用来判"在不在 workspace 内"**，W07 的 `path_display`
     要自己按 `withinDirectory(workspace, 绝对路径)` 判。用例对两种形状都放行，换机器不闪。
6. 错误信息里**不带**"这条档位来自哪种模式"（多出来的信息只会让人误以为存在两套规则）。
   但**位置前缀按来源分**：配置侧继续给 `executors.commands[i] "name": ...`（运维要靠下标定位到那一行），
   单条校验传 `singleProfileIndex = -1`，`profileError` 因此新增一支输出 `profile "name": ...`。
   卡原文 §3.6 写的是"措辞保持与现状一致"，而 §2 又抱怨过"错误信息会指向 `executors.commands[0]`
   这种与调用方无关的位置"——这里按后者落实：**前缀随来源不同，前缀之后的原因文本两边逐字相同**，
   `TestBuildProfileSameRulesAsLoadProfiles` 比的就是剥掉前缀之后的那段。

## 4. 实现步骤

1. 读一遍 `buildProfile` 里所有路径相关分支（`script`/`program`/`cwd` 三处调用 `resolveInside`），
   确认改动面就是这三处 + 函数签名。
2. `buildProfile` 加 `mode` 参数，`LoadProfiles` 传严格模式；把 `resolveInside`/`resolveAny` 的分流
   收在一个小函数里（`resolveProfilePath(mode, workspace, value, field, index, name)`），
   避免三个调用点各写一遍 `if mode ==`。
3. 新增 `BuildProfile(cmd, ec, mode)`；它自己**不做**重名检查（那是调用方的事，W06 会查两份来源）。
4. 补测试（§5）。
5. `go build ./... && go vet ./... && go test ./executor ./core -race`，再跑全量。

## 5. 测试要求

1. **回归**：`executor` 包现有用例（尤其覆盖绝对路径、`..`、以分隔符开头、Windows 盘符那几条）
   必须原样绿，一条都不许改预期。这是"yaml 侧行为不变"的落点。
2. `BuildProfile` 严格模式：与 `LoadProfiles` 对同一条 `ExecutorCommand` 的结果逐字段相等
   （含错误文本），证明单条入口与整表入口没有两套规则。
3. `PathAnywhere`：
   - 绝对路径（Linux 形态 `/tmp/x.py`、Windows 形态 `D:\\srv\\x.py`）通过，`ScriptPath` 是绝对路径。
   - 空 `script` 仍然拒绝；纯空白仍拒绝。
   - 含 `..` 的相对写法通过（`../outside/run.sh`），且 `ScriptRel`/`ScriptPath` 自洽。
   - `cwd` 留空时仍解析到 workspace 本身。
   - `binary` + `program` 命中 `runtime_allow` 时仍按 PATH 程序名走，不因宽松模式变成路径。
4. 模式不影响非路径规则：同一条 `kind: script` 缺 `runtime` 的档位，在两种模式下都失败且原因一致。
5. `Probe` 消费宽松模式产出的用例：一个越界但真实存在的临时文件（`t.TempDir()`）→ `Available`；
   一个不存在的绝对路径 → 不可用且 `reason` 指名脚本。断言 `Probe` 源码一行未改。
6. 手工（临时目录）：造一份 `web_enabled: false` 的配置 + 一条绝对路径的 yaml 档位，
   确认进程**启动失败**并指出该档位——yaml 侧永远严格。

## 6. 完成标准（DoD）

- [x] `BuildProfile(cmd, ec, mode)` 可用，且与 `LoadProfiles` 共用同一条 `buildProfile` 路径。
      → `TestBuildProfileSameRulesAsLoadProfiles`：12 条档位定义（4 条合法 + 8 条必拒）在两入口下
      逐字段比 Profile、逐字比剥掉前缀后的原因。
- [x] `LoadProfiles` 恒用严格模式，现有全部用例未改预期即通过。
      → `TestLoadProfilesAlwaysStrictEvenWithLooseDefaults`；`git diff --stat` 显示
      `executor/profile_test.go`、`register_test.go`、`args_test.go`、`probe_test.go` 零改动。
- [x] `PathAnywhere` 只放开 `script`/`program`/`cwd` 三处，其余规则两模式一致。
      → `TestPathModeOnlyAffectsPathFields`（缺 runtime、解释器白名单、字段配套、档位名四条在
      两种模式下都同样被拒）与 `TestPathAnywhereDoesNotTurnProgramNameIntoPath`。
- [x] 越界档位的相对写法有确定口径 —— **卡原文 §3.5 的前提不成立**，已按事实改写（见 §10.2 第 3 条）：
      `ScriptRel` 可能是 `..\..\` 上跳形式而不是文件名，判"在不在 workspace 内"只能用 `withinDirectory`。
- [x] `Probe` 与 `Render`/`ValidateSubmission` 一行未改即可消费宽松模式产出。
      → `git diff` 里没有 `executor/probe.go`、`executor/args.go`；
      `TestProbeConsumesLooseProfileWithoutChanges`、`TestRenderUsesAbsolutePathInBothModes`。
- [x] `go build ./...`、`go vet ./...`、`go test ./... -race` 全绿。

## 7. 验收方式

```bash
go test ./executor -run 'TestBuildProfile|TestLoadProfiles|TestProbe' -v
go build ./... && go vet ./...
```

预期：`TestLoadProfiles*` 全绿且 `git diff` 显示这些用例的断言**没有被改动**
（`git diff --stat executor/profile_test.go` 只应有新增，不应有既有用例的行改动）。

实测（2026-10-01）：

```
$ go test ./executor -run 'TestBuildProfile|TestPathMode|TestPathAnywhere|TestProbeConsumes|TestRenderUses|TestLoadProfilesAlwaysStrict' -v
  --- PASS: TestBuildProfileSameRulesAsLoadProfiles (0.03s)      ← 12 个子例
  --- PASS: TestLoadProfilesAlwaysStrictEvenWithLooseDefaults (0.00s)
  --- PASS: TestBuildProfilePathAnywhereAcceptsRootedAndClimbingPaths (0.00s)  ← 4 个子例
  --- PASS: TestPathModeOnlyAffectsPathFields / TestPathAnywhereDoesNotTurnProgramNameIntoPath
  --- PASS: TestProbeConsumesLooseProfileWithoutChanges / TestRenderUsesAbsolutePathInBothModes
$ git status --short -- executor
  M executor/profile.go
  ?? executor/build_profile_modes_test.go        ← 既有测试文件一个都没动
```

## 8. 不在本任务范围

- 不做路径白名单（`executors.path_roots`）——设计文档 S-1，本期明确不做。
- 不动 `runtime_allow`、`env_allow`、超时上限这些全局参数。
- 不做 `Registry` 可变（W03）、不做端点（W06）。
- 不给页面提供任何"脚本内容"能力：本卡放开的是**路径**，内联源码仍然拒绝（D2 原口径）。

## 9. 风险与回滚

| 风险 | 说明 | 处置 |
| --- | --- | --- |
| 复制出一份"页面专用校验" | 违反 I1，将来"保存成功、重启失败" | §3.2 要求共用 `buildProfile`；§5.2 用逐字段相等用例钉住 |
| 把严格模式的拒绝分支误删 | yaml 侧越界防护消失，且不会有任何测试提醒（如果没有 §5.1） | §5.1 的回归用例是这条的哨兵，执行时不得跳过 |
| `PathMode` 泄漏进 `Profile` | 下游开始按模式分叉行为 | `Profile` 不加模式字段；只在构造期用一次 |
| 越界档位的相对写法算出 `..\..\x` | 展示与日志可读性差、可能泄露目录结构 | §3.5 要求兜底成文件名，W07 只用这个字段展示 |

回滚：本卡只改 `executor/profile.go` 的函数签名与一处新函数。退回即恢复 `resolveInside` 单路径 +
删 `BuildProfile`/`PathMode`；`LoadProfiles` 的调用方（`NewRegistry`）不受影响。

## 10. 实现记录（2026-10-01）

### 10.1 落地的接口

全部改动在 `executor/profile.go` 一个文件里（+ 一个新测试文件），`probe.go`、`args.go`、`register.go` 一行未动：

- `PathMode`（`PathWithinWorkspace` 零值 / `PathAnywhere`）紧接 `Kind` 常量块之后。
- `BuildProfile(cmd core.ExecutorCommand, ec core.ExecutorsConfig, mode PathMode) (*Profile, error)`
  + `singleProfileIndex = -1` + 抽出的 `runtimeSet(allow []string) map[string]bool`
  （`LoadProfiles` 原来内联的那三行改用它，两边共用同一份白名单构造）。
- `buildProfile` 多收 `mode`；三处路径字段（`cwd` / `script` / `program`）改调
  `resolveProfilePath(mode, workspace, value, field, index, name)`。
- `resolveProfilePath` 分流 + 新函数 `resolveAnywhere`，位置紧挨 `resolveInside` 之前。
- `LoadProfiles` 恒传 `PathWithinWorkspace`。
- `profileError` 新增 `index < 0` 一支（`profile "name": …`）；
  `checkProfileName` 改走 `profileError`，**文本逐字不变**（两条既有断言仍然精确匹配）。

测试：`executor/build_profile_modes_test.go`，7 个顶层用例 / 16 个子例。

### 10.2 与本卡写法的差异

1. 函数名 `resolveAnywhere`（卡里写的是 `resolveAny`）。
2. `BuildProfile` 自己执行 `core.Config{Executors: ec}.Normalized().Executors`，
   所以调用方可以直接传 `cfg.Executors` 那一节而不用担心 workspace / runtime_allow 留空。
   卡 §3.2 只说"内部完成归一化"，这里把做法钉成"复用 `Config.Normalized`，不另抄一份默认表"。
3. **卡 §3.5 的一条前提是错的**：`relativeTo`（`executor/profile.go:948-954`）只在
   `filepath.Rel` 失败时兜底成文件名，而它只在 Windows 跨盘符时失败；
   同盘越界路径它会老实给出 `..\..\srv\report\main.py` 这种上跳形式。
   于是"越界时 `ScriptRel` 是文件名而不是 `../..`"这条断言写不出来，
   §3.5 已就地改成事实描述，并把结论交给 W07：`path_display` 判"在不在 workspace 内"
   只能用 `withinDirectory(workspace, 绝对路径)`，不能看 `ScriptRel` 的形状。登记为 D-0201。
4. 错误前缀按来源分（§3.6 反过来落实，理由见该节改写）。
5. 宽松模式的"根写法"判定与 `resolveInside` 用同一套写法检查（`/`、`\` 前缀 + `filepath.IsAbs`），
   因为 Windows 上 `filepath.IsAbs("/srv/x.py")` 是 false；卡原文没提这一条。
   实际效果：`/srv/report/main.py` 在 Windows 上解析成"当前盘符 `:\srv\report\main.py`"，
   在 Linux 上就是 `/srv/report/main.py`。用例只断言"是绝对路径且不在 workspace 内"，两平台都过。
6. 相对写法以 workspace 为基准（§3.4 改写）：`PathAnywhere` 只放宽"能在哪儿"。
   `TestBuildProfilePathAnywhereAcceptsRootedAndClimbingPaths` 里的
   "相对写法仍以 workspace 为基准"子例专门钉这一点。

### 10.3 验证证据

见 §7 的实测块，另外两条：

```
$ go build ./... && go vet ./...            → VET-OK
$ go test ./... -race -count=1              → ok godelayq/api 194.9s
                                             ok godelayq/cmd/server 5.6s
                                             ok godelayq/core 12.5s
                                             ok godelayq/executor 22.6s
                                             ok godelayq/store/sqlite 3.7s
                                             ?   examples/demo1、demo2、cmd/gensecret、cmd/hashpassword、web [no test files]
```

同一条命令在改动前后各跑一遍（改前那遍是 W01 收口时跑的），两遍全绿，没有 flake。
api 包从 94.6s 涨到 194.9s 是机器负载而不是本卡改动 —— 本卡没有动 `api` 包。

### 10.4 手工验收

`%TEMP%/gdq_w02_smoke/config.yaml`：`enabled: true` + `web_enabled: true` +
一条 `executors.commands` 档位写成绝对路径 `D:/srv/report/main.py`（越界写法），起真实进程：

```
time=... level=ERROR msg="server exited with error"
error="executors.commands[0] \"outside_abs\": script \"D:/srv/report/main.py\" must be relative to executors.workspace"
```

结论：**yaml 侧的越界拒绝一字未松**，且错误仍带 `executors.commands[0]` 位置前缀
（这正是 §10.2 第 4 条"前缀按来源分"要保住的东西）。冒烟目录跑完已删。

宽松模式的现场走查做不到：本卡还没有任何调用方会传 `PathAnywhere`（W05 的合并、W06 的端点才是调用方），
所以越界档位"能存、能探、能跑"的端到端结论留到 W05/W06，见 §10.6。

### 10.5 缺陷

| 编号 | 内容 | 处置 |
| --- | --- | --- |
| D-0201 | 卡 §3.5 关于 `relativeTo` 兜底的前提不成立，照它写 W07 会得到一个错的判据 | **已修**：本卡 §3.5 与 §6 第 4 条就地改成事实描述，并把"用 `withinDirectory` 判"的结论写给 W07 |

### 10.6 未覆盖项

- 越界档位在真实进程里被探测与被执行的现场（依赖 W05 的合并与 W06 的端点）。
- Linux/macOS 未实跑：本卡只在 Windows 跑绿。`/srv/report/main.py` 在两平台的解析差异
  已用"只断言绝对 + 不在 workspace 内"的写法规避，但绝对路径形态本身没在两平台上各验一次。
- 宽松模式**完全不判符号链接**（没有根可判）：这是设计选择（§3.4），
  但意味着 `path_roots` 白名单（S-1）落地时必须自己处理链接指向，W09 要把这条写进 `docs/deployment.md` 的警告里。

