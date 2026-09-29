# TASK-E02　档位定义与配置校验

- 所属阶段：M0 基础
- 依赖任务：TASK-E01
- 涉及文件：新增 `executor/profile.go`、`executor/profile_test.go`；改 `core/config.go`（替换 `Commands` 的临时类型）
- 预计规模：大（校验规则多，规则错误等于后面全部返工）

## 1. 任务目标

把"一条可执行任务"定义成配置里的一个档位（profile），并在程序启动时把这个档位的所有写法检查干净。检查不通过就直接启动失败，说明哪一条哪一项不合法。本任务不执行任何进程。

## 2. 背景与当前问题

设计口径是不提供自由命令行字符串：能执行什么完全由配置决定。这个口径只有在"配置里的每一项都被严格校验、并且没有任何绕过路径"时才成立。所以校验代码是安全边界的一部分，必须在本任务把规则写全，并配足够的测试。

E01 里 `ExecutorsConfig.Commands` 是 `[]map[string]any` 的临时形状，本任务替换成正式结构。

## 3. 要实现的功能

> ⚠️ 实现说明（2026-09-29）：下面 §3.1 的代码块是**字段清单**，实际落点与命名有调整——
> 配置结构体必须在 `core/config.go`（`executor` 要读 `core.Config`，结构体放本包就成了循环依赖），
> 名字分别是 `core.ExecutorCommand` / `core.ExecutorArg` / `core.ExecutorPositional`；
> `executor` 包里只放校验后的运行期产物 `Profile`（字段与本节同义，另加 `ScriptRel`/`ProgramRel`/`CwdRel`
> 这三个"相对 workspace 的展示写法"，绝对路径不外露）。详见第 10 节。

1. 正式结构（`executor/profile.go`）：

   ```go
   type Kind string          // script | binary | http
   type Arg struct {
       Name     string `mapstructure:"name"`
       Required bool   `mapstructure:"required"`
       Default  string `mapstructure:"default"`
       Pattern  string `mapstructure:"pattern"`   // 单条正则，空表示只允许字母数字与 . _ -
       Secret   bool   `mapstructure:"secret"`    // 响应里掩码，E16 使用
   }
   type Command struct {
       Name        string            `mapstructure:"name"`
       Kind        Kind              `mapstructure:"kind"`
       Runtime     string            `mapstructure:"runtime"`      // script 用
       Script      string            `mapstructure:"script"`       // script 用，相对 workspace
       Program     string            `mapstructure:"program"`      // binary 用，相对 workspace 或在 runtime_allow 内
       FixedArgs   []string          `mapstructure:"fixed_args"`   // binary 用，写死前缀
       Args        []Arg             `mapstructure:"args"`
       ArgsRender  []string          `mapstructure:"args_render"`  // 形如 --day={day}
       Positional  *PositionalSpec   `mapstructure:"positional"`   // {max, pattern}
       Cwd         string            `mapstructure:"cwd"`
       Env         map[string]string `mapstructure:"env"`
       EnvAllow    []string          `mapstructure:"env_allow"`    // payload 可注入的键名白名单
       Timeout     time.Duration     `mapstructure:"timeout"`
       MaxParallel int               `mapstructure:"max_parallel"`
       RetryOnExit []int             `mapstructure:"retry_on_exit"`

       // http 用
       Method       string   `mapstructure:"method"`
       URLTemplate  string   `mapstructure:"url_template"`
       AllowedHosts []string `mapstructure:"allowed_hosts"`
       Headers      map[string][]string `mapstructure:"headers"`
       HeaderAllow  []string `mapstructure:"header_allow"`
       Body         string   `mapstructure:"body"`          // json | raw | none
       ExpectStatus []int    `mapstructure:"expect_status"`
       CaptureResp  bool     `mapstructure:"capture_response"`
       MaxBodyBytes int      `mapstructure:"max_body_bytes"`
       MaxRedirects int      `mapstructure:"max_redirects"`
       DenyPrivate  *bool    `mapstructure:"deny_private_ranges"`
   }
   ```

2. `func LoadProfiles(cfg core.Config) ([]*Profile, error)`：把配置里的档位转成运行时可用的 `Profile`，失败时返回带 `executors.commands[i]` 前缀的错误。
3. `Profile` 是校验后的产物，字段可直接使用，不再需要二次检查：解析出的绝对路径 `ScriptPath`/`ProgramPath`/`CwdPath`、编译好的正则 `*regexp.Regexp`、最终的 `Timeout`、注册键 `HandlerKey()`（返回 `exec.<name>`）。
4. 校验规则，逐条要有对应测试：
   - `name`：非空、只含 `[A-Za-z0-9_-]`、长度 ≤ 64；加 `exec.` 前缀后不得与配置里另一个档位重名；`runtime`、`program` 等字段不得让注册键超出这个字符集。
   - `kind`：只能是 `script|binary|http`。
   - `runtime`：`script` 必填，且必须在 `executors.runtime_allow` 里；`binary`/`http` 不允许填。
   - 路径类字段（`script`、`program`、`cwd`）：不得是绝对路径、不得含 `..`、经 `filepath.Abs` 与 `EvalSymlinks` 后必须仍在 `workspace` 内（比较时用带结尾分隔符的前缀，避免 `./work` 与 `./workspace` 误判）。
   - `program` 额外允许两种取值：`runtime_allow` 里的解释器名（当作可执行文件查找），或 workspace 内的相对路径。
   - `args`：`name` 唯一且符合 `[a-z0-9_]+`；`pattern` 必须能编译；`default` 必须匹配自己的 `pattern`；`required=true` 且 `default` 非空时报错（语义矛盾）。
   - `args_render`：每项里的 `{x}` 占位符必须在 `args` 里声明过；不允许出现未闭合的 `{`；渲染项里除占位符外不含 shell 特殊字符（`\` `;` `|` `&` `` ` `` `$` `>` `<` 换行）——虽然不经 shell，但要求写死这一条，防止以后有人加 shell 通道时这些值直接变成命令。
   - `positional`：`max` 在 1..16；`pattern` 必须能编译，且默认值不允许以 `-` 开头。
   - `env`：键名符合 `[A-Z0-9_]+`；键名以 `GODELAYQ_` 开头、或值是控制字符时报错。
   - `timeout`：0 表示用全局 `default_timeout`；超过 `max_timeout` 报错。
   - `max_parallel`：≥1，0 表示 1。
   - `http`：`method` 是 HTTP 方法名大写；`url_template` 必须是 `https`（`http` 只在 host 是本机回环时允许，且必须写进 `allowed_hosts`）；`{x}` 占位符必须在 `params`（复用 `args` 字段）里声明；`allowed_hosts` 非空且每一项是主机名或 `host:port`，不含 `*`、不含路径、不含 `://`；`expect_status` 每项在 100..599；`max_redirects` 必须为 0（本设计要求不跟随重定向，写成非 0 就是配置错误）；`deny_private_ranges` 未显式设为 false 时默认 true，显式设 false 时要求 `allowed_hosts` 全部是具体主机名且记入启动警告。
   - `fixed_args`：不含控制字符；`binary` 档位同时存在 `fixed_args` 与 `positional` 时，`positional.max` 必须 ≥1。
5. `func (p *Profile) ValidateSubmission(providedKeys []string) error`：本任务只提供"payload 里出现的顶层键是否合法"的检查（允许 `args`/`env`/`params`/`body`/`headers`/`timeout` 这些按 kind 不同的集合），具体的值校验在 E08 做。这样 E16/E17 可以只依赖本任务就能拒绝明显越界的提交。
6. `core/config.go` 的 `ExecutorsConfig.Commands` 改成 `[]Command`（`mapstructure` 名不变），确认 E01 的测试仍然通过。

## 4. 实现步骤

1. 先写 `Profile`/`Command`/`Arg` 结构和 `LoadProfiles` 的骨架（返回未实现错误）。
2. 按第 3.4 节的规则逐条实现校验，每条规则一个私有函数，便于单测直接调用。
3. `workspace` 的路径检查用同一个函数处理 `script`/`program`/`cwd`，不要写三份。
4. 改 `core/config.go` 的 `Commands` 类型，跑 E01 的测试确认没破坏解码。
5. 补 `examples/` 或测试里的档位样例：`script`、`binary`、`http` 各一份完整 YAML，作为 E04/E18 的参照。

## 5. 测试要求

`executor/profile_test.go`，用临时目录当 workspace 并放真实文件（校验要求文件存在，因此测试要 `os.WriteFile` 造脚本）：

1. `TestLoadProfiles_KindsAndDefaults`：三种 kind 各一条合法配置，断言 `HandlerKey()`、绝对路径、超时取值、`deny_private_ranges` 默认 true。
2. `TestLoadProfiles_PathEscape`：`script: ../etc/x.sh`、`script: /abs/path`、cwd 越界、以及符号链接指向 workspace 外的情况（Windows 上创建符号链接需要权限，用 `t.Skip` 并在非 Windows 分支覆盖）→ 全部报错，错误信息里含该档位名。
3. `TestLoadProfiles_UnknownRuntime`：`runtime: ruby` 不在 `runtime_allow` → 报错。
4. `TestLoadProfiles_ArgRules`：重名参数、非法正则、default 不匹配 pattern、required 且有 default、`args_render` 引用未声明键、渲染项含 `$`/`;`/换行 → 各自报错。
5. `TestLoadProfiles_HttpRules`：`max_redirects: 2`、`allowed_hosts: ["*"]`、`allowed_hosts: ["evil.com/#x"]`、`url_template` 用 `http://` 且非回环、`expect_status: [999]` → 报错。
6. `TestLoadProfiles_NameCharset`：`name: "a b"`、`name: ""`、超长 name、两个档位渲染成同一个 `exec.` 键 → 报错。
7. `TestLoadProfiles_DuplicateDetection`：档位名与既有 Handler 注册键冲突的检查留给 E04（本任务只查档位之间不冲突），测试名与注释要写清楚这条边界。
8. `TestValidateSubmission`：`script` 档位提交 `{"cmd":"ls"}` → 拒绝；提交 `{"args":{"day":"yesterday"}}` → 通过；`http` 档位提交 `{"url":"..."}` → 拒绝（URL 不能由 payload 给）。

## 6. 完成标准（DoD）

- [ ] 第 3.4 节每条规则都有实现和对应测试，不存在"文档写了但没测"的规则。
- [ ] 路径检查覆盖 `..`、绝对路径、符号链接三种逃逸写法，并且比较逻辑用带结尾分隔符的前缀。
- [ ] `Profile` 是"校验后即用"的产物：后续任务不再需要重新判断路径或正则是否合法。
- [ ] `core/config.go` 的 `Commands` 换成正式类型，E01 全部测试仍绿。
- [ ] `executor` 包不 import `core` 之外的执行逻辑，也不 import 任何 gin/web 包（照 `core` 不依赖 web 框架的既有约定）。

## 7. 验收方式

```bash
go build ./... && go vet ./... && go test ./executor ./core -race -v
```

手工确认：故意在本机 `configs/config.yaml` 里写一条 `script: ../../evil.sh` 的档位，跑 `go run ./cmd/server`，启动失败并指出该档位（注意此时 `enabled` 仍是 false，如果校验只在 enabled 时执行，请显式打开 `enabled: true` 再验；确认后把这条行为写进 DoD 的第 6 条备注）。

## 8. 不在本任务范围

- 不做可执行文件是否存在于 PATH 的探测（E03）。
- 不注册 Handler（E04）。
- 不校验 payload 里的参数值（E08）。
- 不实现任何执行逻辑。

## 9. 风险与回滚

- 风险：`args_render` 里"不含 shell 特殊字符"这条会让人以为是 shell 通道的一部分，容易在实现时被删。它防的是以后有人加 shell 通道，删除前要先改设计文档。
- 风险：Windows 上 `EvalSymlinks` 与符号链接创建受限，测试若整段跳过会留下空白。要求非 Windows 分支必须覆盖符号链接场景，并在 CI 或本地 Linux/WSL 上跑一次。
- 回滚：`executor` 包是新增，`git revert` 本卡提交即可；`core/config.go` 的类型替换需要跟一次小改动一起回滚。

## 10. 实现记录（2026-09-29）

改动文件：新增 `executor/profile.go`、`executor/profile_test.go`；`core/config.go`（`Commands` 换成
`[]ExecutorCommand` 并新增三个配置结构体）；`core/config_test.go`（E01 的两条断言随之更新）；
`configs/config.example.yaml` 与本机 `configs/config.yaml` 的 `commands` 注释（示例补齐 `secret`/`cwd`/
`env`/`env_allow`/`retry_on_exit`，并把"档位内部键名拼错发现不了"改成"会被解码拒绝"）。

### 与卡片的偏离（7 处，均在实现时判断，理由如下）

1. **配置结构体落在 `core`，不在 `executor`**（卡片 §3.1）。`executor.LoadProfiles` 的参数是 `core.Config`，
   结构体放本包会让 `core → executor → core` 成环，而 E12 的 DoD 明确要求 `core` 不依赖 `executor`。
   顺带与仓库既有体例一致（`UserConfig`、`ExecutorOutputConfig` 都在 `core/config.go`）。
2. **不检查脚本/产物文件是否存在**（卡片 §5 假设"校验要求文件存在"）。存在性决定的是"这台机器现在能不能跑"，
   属于 E03 探测；配置先写、脚本后部署是常见顺序，让这种写法直接拒绝启动会让服务起不来。
   用 `TestLoadProfiles_MissingFileIsNotAProfileError` 钉住这条边界。
3. **`allowed_hosts` 允许 `*.` 前缀通配**（卡片 §3.4 写"不含 `*`"）。E15 §5.3 要求 `*.example` 可用，
   两者冲突；取"只允许 `*.` 前缀，裸 `*` 与其它位置的 `*` 一律拒绝"。通配的后缀比较保留前导点，
   `*.internal` 接受 `a.internal`、拒绝 `a.internal.evil.com`（有测试）。
4. **卡片 §3.5 的 `ValidateSubmission` 改名为 `ValidatePayloadKeys`**。E08 §3.2 有一个同名但更宽的函数
   （解析 payload 并校验值），两名并存会误导后来者；E02 这版只做顶层键名白名单，正是卡片描述的行为。
5. **`positional` 的"值不允许以 `-` 开头"没有在本卡实现**。那是取值判断，需要真实的值；
   本卡只保证 `max` 在 1..16、`pattern` 能编译并预置默认安全集。执行处由 E08 做。
6. **绝对路径判定加了一条跨平台规则：以路径分隔符开头的值一律拒绝**。Go 在 Windows 上
   `filepath.IsAbs("/tmp/x")` 返回 false（Windows 的绝对路径要盘符或 UNC），
   只靠 `IsAbs` 会让 Unix 上算绝对的写法在 Windows 被当成 workspace 内相对路径。
   补测 `/tmp/evil.sh`、`\tmp\evil.bat`、`\\server\share\evil.bat` 三种写法，
   加上只在 Windows 跑的盘符用例。
7. **新增两条卡片没写的校验**（一致性补强，不是扩大范围）：
   `method` 必须已经是大写（避免 `post`/`POST` 两种写法指向同一档位）；
   非 GET/DELETE 的方法必须显式写 `body`，否则"忘了发体"和"故意不发体"在配置里长得一样。

### 符号链接这条分支的覆盖方式

Windows 上创建符号链接需要管理员权限或开发者模式，本机的真实链接用例
`TestLoadProfiles_SymlinkEscape` 被跳过。为了让判定本身在任何平台都被执行过，
`profile.go` 加了两个包级变量 `evalSymlinks` / `statPath` 作为唯一注入点
（默认就是 `filepath.EvalSymlinks` / `os.Lstat`），`TestLoadProfiles_SymlinkEscapeViaResolver`
用一次"解析结果指向 workspace 之外"的文件系统状态断言拒绝，并断言指向 workspace 内时照常接受。
真实链接用例保留，供 Linux/macOS 或有权限的 Windows 环境跑。

### 卡片 §5 用例的对应实现

| 卡片要求 | 实现处 |
| --- | --- |
| §5.1 三种 kind 与默认值 | `TestLoadProfiles_KindsAndDefaults`（另含 PATH 程序名分支 `TestLoadProfiles_ProgramNameInRuntimeAllow`） |
| §5.2 路径越界与符号链接 | `TestLoadProfiles_PathEscape` + 上面两个符号链接用例 |
| §5.3 未列入白名单的解释器 | `TestLoadProfiles_UnknownRuntime`（含首尾空格的解释器名） |
| §5.4 参数规则 | `TestLoadProfiles_ArgRules` |
| §5.5 HTTP 规则 | `TestLoadProfiles_HttpRules`（16 个子用例） |
| §5.6 档位名 | `TestLoadProfiles_NameCharset` |
| §5.7 重名边界 | `TestLoadProfiles_DuplicateDetectionBoundary`：本卡只查档位之间重名，与代码注册键的冲突留给 E04 |
| §5.8 提交键白名单 | `TestProfile_ValidatePayloadKeys` |
| 未列在卡片但补的 | `TestLoadProfiles_KindFieldMismatch`（kind 专属字段互斥 + 必填项）、`TestLoadProfiles_TimeoutRules`、`TestLoadProfiles_EnvAndFixedArgRules`、`TestLoadProfiles_EmptyCommandsIsLegal`、`TestProfileErrorPrefix`、`TestProfile_ArgLookup`、`TestLoadProfiles_FromConfigFile` |

### 验证结果

- `go build ./...`、`go vet ./...`、`go test ./... -race` 全绿（executor 1.5s / core 7.7s / api / cmd/server）。
- `GOOS=linux`、`GOOS=darwin`、`GOOS=windows` 三个平台 `go build ./...` 通过；`GOOS=linux go vet ./executor` 通过。
- 格式化：`gofmt -w` 后无差异。
- 端到端可执行版的手工验证：`TestLoadProfiles_FromConfigFile` 用一份真实 YAML 走完
  `core.LoadConfig` → `LoadProfiles`，断言 `script: ../../evil.sh` 被拒且错误里带
  `executors.commands[0] "deploy_hook"` 与该路径。**注意**：服务端进程本身要到 E04 才会调用这条校验，
  卡片 §7 那句"跑 `go run ./cmd/server` 启动失败"此刻还验不出来，本次以该测试替代。
