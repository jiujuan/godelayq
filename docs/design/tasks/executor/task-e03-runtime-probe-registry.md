# TASK-E03　解释器探测与执行器登记表

- 所属阶段：M0 基础
- 依赖任务：TASK-E02
- 涉及文件：新增 `executor/probe.go`、`executor/registry.go` 及各自测试
- 预计规模：小

## 1. 任务目标

启动时检查每个档位需要的可执行文件是否真的存在，并把"档位 + 可用性"整理成一份登记表，供 HTTP 层查询、供提交时给出明确错误。本任务仍然不执行任何进程。

## 2. 背景与当前问题

`../../executor-design.md` §3 记录了现状：注册表只有调度器一份，接口是 `core/scheduler.go` 的 `LookupHandler` / `HandlerNames`，`api/handlers.go` 的 `ListJobTypes` 直接把键名列表给前端。这条链路上没有"某个任务类型当前能不能跑"的概念。

如果档位声明了 `runtime: node` 而这台机器没装 node，最糟的处理方式是照常注册、任务在触发时失败、运维在日志里找原因。更好的方式是启动时就确定不可用，并且能明确拒绝提交。

## 3. 要实现的功能

1. `func Probe(p *Profile) ProbeResult`，返回：

   ```go
   type ProbeResult struct {
       Available bool
       Path      string   // 找到的可执行文件绝对路径
       Reason    string   // 不可用的原因，可直接显示给用户
       Version   string   // 可选：探测到的版本，取不到就留空
   }
   ```

   规则：
   - `script`：`runtime` 必须在 `executors.runtime_allow` 内（E02 已保证），用 `exec.LookPath` 找；`ScriptPath` 必须存在且是普通文件。
   - `binary`：`ProgramPath` 在 workspace 内时只检查存在性与是否可执行（Windows 检查 `.exe`/`.cmd`/`.bat` 后缀，或用 `exec.LookPath`）；是解释器名时同 `script`。
   - `http`：不需要外部程序，恒为可用；但要检查 TLS 端口可达性**不做**（本任务不做网络探测，避免启动被外部依赖卡住）。
   - 找不到时 `Reason` 写成 `runtime "node" not found in PATH`，与 Go 标准库的错误风格一致，方便对照。
   - `Version` 的获取是可选增强：只有当执行 `<program> --version` 明显安全（不触发副作用）时才采集；本任务默认**不采集**，把字段留着并在注释说明为什么不采集（探测期不执行任何外部程序）。
2. `type Registry struct{ ... }`：持有 `[]*Profile` 与探测结果，提供
   - `Profiles() []*Profile`（按 `HandlerKey` 字典序，接口输出要稳定）
   - `Lookup(handlerKey string) (*Profile, bool)`
   - `Available(handlerKey string) (reason string, ok bool)`
   - `Keys() []string`
3. `func NewRegistry(cfg core.Config, logger *slog.Logger) (*Registry, error)`：调用 E02 的 `LoadProfiles`，逐个 `Probe`，对不可用的档位记一条 warn 日志（含档位名与原因），但**不返回错误**——注册照常进行，让 HTTP 层能显示"已声明但不可用"，比静默消失更容易排查。
4. `executors.enabled == false` 时 `NewRegistry` 返回空登记表（非 nil），`Keys()` 为空，后续所有端点返回空列表。

## 4. 实现步骤

1. 先写 `ProbeResult` 与 `Probe`，把三种 kind 的判断分支分开。
2. 写 `Registry` 与其四个方法，内部用 `map[string]*entry` + 预排序的 `keys`。
3. `NewRegistry` 串起来：加载档位 → 逐个探测 → 记日志。
4. 日志字段照 `core/logging.go` 既有风格（结构化键值，不用 `fmt.Sprintf` 拼进消息）。

## 5. 测试要求

`executor/probe_test.go`、`executor/registry_test.go`：

1. `TestProbe_ScriptMissingRuntime`：`runtime` 设成一个几乎不可能存在的名字（例如 `godelayq-test-runtime-xyz`），断言 `Available=false` 且 `Reason` 含该名字。测试不要依赖机器上装了 node 或 php。
2. `TestProbe_ScriptMissingFile`：runtime 用当前测试进程自身可执行文件（`os.Executable()`，保证存在）加一个不存在的脚本路径 → 断言原因是文件缺失而不是 runtime 缺失。这条能验证"两个失败分支不混淆"。
3. `TestProbe_BinaryInWorkspace`：workspace 放一个自建文件并设可执行位（Windows 用 `.bat`/`.cmd` 名字或跳过并说明）→ `Available=true`、`Path` 等于绝对路径。
4. `TestProbe_HTTPAlwaysAvailable`：http 档位 → `Available=true`，且没有任何子进程被启动（可用 `Probe` 不返回 error 且测试内不产生进程间接证明）。
5. `TestRegistry_LookupAndSort`：造三个档位，断言 `Keys()` 与 `Profiles()` 按 `exec.` 键字典序，重复调用顺序一致。
6. `TestRegistry_EmptyWhenDisabled`：`enabled=false` → 登记表非 nil、`Keys()` 空、`Lookup` 全部返回 false。
7. `TestNewRegistry_LogsUnavailable`：用 `slog.New(slog.NewTextHandler(&bytes.Buffer{}))` 捕获，断言不可用档位产生一条 warn 且包含档位名。

## 6. 完成标准（DoD）

- [ ] 探测阶段不启动任何外部程序（只查 PATH 与文件系统）。
- [ ] 档位不可用时不会从登记表和注册表里消失，而是带原因地保留；错误原因能在 HTTP 响应里原样读到。
- [ ] `Registry` 的四个方法全部有测试，输出顺序稳定。
- [ ] `enabled=false` 与"启用但没有档位"两种情况都能得到空登记表，不产生 nil 指针分支。
- [ ] `executor` 包不 import `gin`，只依赖 `core` 与标准库。

## 7. 验收方式

```bash
go test ./executor -race -v
```

手工确认：在本机 `configs/config.yaml` 加一个 `runtime: godelayq-no-such` 的档位并 `enabled: true`，启动后日志里能看到对应的 warn，且进程正常启动不退出。

## 8. 不在本任务范围

- 不做版本探测（字段留空，理由写进注释）。
- 不做网络连通性检查（E15 的执行期错误处理负责）。
- 不做注册到调度器（E04）。
- 不加 HTTP 端点（E07 一起做）。

## 9. 风险与回滚

- 风险：`exec.LookPath` 在 Windows 上会按 `PATHEXT` 匹配，测试里用 `os.Executable()` 当"存在的程序"是安全的，但不要用 `"node"` 这种依赖环境的值。
- 风险：把不可用档位保留在注册表里，会让 `GET /job-types` 出现当前跑不了的名字。这是有意的（前端据此显示不可用状态），E04 要在注册日志里说明，E13 的响应必须带 `runtime_ok`，否则运维会以为系统坏了。
- 回滚：新增文件，`git revert` 即可。

## 10. 实现记录（2026-09-29）

改动文件：新增 `executor/probe.go`、`executor/probe_test.go`、`executor/registry.go`、`executor/registry_test.go`；
本卡片修正了 §8 的一处笔误（多打的一个 `-`）。

### 与卡片的偏离与补充

1. **`exec.LookPath` 收进包级变量 `lookPath`**。测试需要一个"一定存在且可执行"的程序来验证正反两条分支，
   用当前测试程序自身的路径（`os.Executable()`，绝对路径也走 LookPath，语义一致）最可靠；
   留成变量也让后面任何要模拟 PATH 的测试不用改生产代码。
2. **脚本不要求执行位、产物要求**（卡片只写"检查存在性与是否可执行"）。
   脚本是被解释器读取的（`node app.mjs` 不需要 app.mjs 可执行），要求执行位会误伤一批合法配置；
   产物文件是要直接执行的，所以 Unix 看执行位、Windows 看扩展名集合 `.exe/.com/.bat/.cmd`。
   两侧各有用例（`TestProbe_ScriptIsAcceptedWithoutExecuteBit`、`TestProbe_BinaryNotExecutable`）。
3. **不可用原因里只写相对 workspace 的路径**。卡片只要求原因能原样显示，
   而 E16 要求不把服务器目录结构透给接口；在这里就不带绝对路径，比在 HTTP 层再遮蔽可靠
   （`TestProbe_ReasonNeverLeaksWorkspacePath` 直接断言原因里不含 workspace 绝对路径）。
4. **`Registry` 比卡片多四个访问器**：`Enabled()`、`RequiredRole()`、`LoaderAllowed()`、`ProbeOf(key)`。
   前三条分别被 E07、E16、E17 引用，`ProbeOf` 是 E07 输出 `runtime_ok` + 原因所需的；
   放在登记表的归属卡里实现，避免三张卡各自回来补方法。
5. **`enabled=false` 时不加载也不探测**（卡片只说返回空登记表）。
   这意味着关闭状态下配置里的档位错误不会暴露，与"enabled=false 时本节取值全部不生效"一致；
   `TestNewRegistry_EmptyWhenDisabled` 里放了一条越界路径的档位来钉住这个行为。
6. **卡片 §7 的手工验证此刻做不到**：服务端进程要到 E04 才会调用 `NewRegistry`，
   所以"启动后日志里看到 warn"这条只能以两种方式替代——
   `TestNewRegistry_FromConfigFile`（真实 YAML → `core.LoadConfig` → `NewRegistry`，混合可用与不可用档位），
   以及一次临时冒烟程序（见下）。E04 完成后应当回到这条手工验证再走一遍。

### 验证结果

- 单元测试：`go test ./executor -race -count=1` 通过；探测 13 个用例、登记表 8 个用例，全部不依赖目标机器装了
  node/php（用测试程序自身路径与一个必然不存在的名字 `godelayq-test-runtime-xyz`）。
- `go build ./...`、`go vet ./...`、`go test ./... -race` 全绿；
  `GOOS=linux/darwin/windows` 三平台 `go build ./...` 通过，`GOOS=linux` 与 `GOOS=windows` 下 `go vet ./executor` 通过。
- 真实进程冒烟（临时程序 + 本机真实 PATH，跑完已删除）：六条档位一次加载，
  `node` 与 `bash` 在本机确实存在 → 判可用；`godelayq-no-such-runtime` →
  `runtime "godelayq-no-such-runtime" not found in PATH`；缺失脚本 → `script file "scripts/absent.sh" does not exist`；
  `bin/tool.exe` 可用、`bin/absent.exe` 报文件缺失；http 档位可用且 `Path` 为空；
  warn 恰好 3 行、每行含 `profile`/`handler_key`/`kind`/`reason`；`Keys()` 按注册键字典序输出。
  另一份把脚本写成 `../../escape.sh` 的配置 → `NewRegistry` 返回
  `executors.commands[0] "report_present": script "../../escape.sh" must not climb out of executors.workspace`。
- 服务端行为未变的确认：`GODELAYQ_EXECUTORS_ENABLED=true` 启动服务端二进制，
  正常 listening、无 error 级日志（此时校验与探测都还没接进装配，属 E04）。

### 留给 E04 的接口形状

`NewRegistry(cfg, logger)` 返回 `(*Registry, error)`；`logger` 传 nil 时用 `slog.Default()`
（照 `core/load.go` 的 `LoaderOptions.Logger` 体例）；档位配置非法时返回 `nil, err`，
装配方据此终止启动。"开了开关但一个档位都没有"的 warn 留给 E04 记，本卡不重复输出。
