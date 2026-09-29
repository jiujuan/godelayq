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
- - 不做注册到调度器（E04）。
- 不加 HTTP 端点（E07 一起做）。

## 9. 风险与回滚

- 风险：`exec.LookPath` 在 Windows 上会按 `PATHEXT` 匹配，测试里用 `os.Executable()` 当"存在的程序"是安全的，但不要用 `"node"` 这种依赖环境的值。
- 风险：把不可用档位保留在注册表里，会让 `GET /job-types` 出现当前跑不了的名字。这是有意的（前端据此显示不可用状态），E04 要在注册日志里说明，E13 的响应必须带 `runtime_ok`，否则运维会以为系统坏了。
- 回滚：新增文件，`git revert` 即可。
