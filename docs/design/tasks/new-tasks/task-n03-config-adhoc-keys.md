# TASK-N03　配置：`executors.adhoc.*` 一节

- 所属阶段：M1 配置与档位
- 依赖任务：无（可与 N01、N02 并行）
- 涉及文件：`core/config.go`、`core/config_test.go`、`core/config_reload.go`、
  `core/config_reload_test.go`、`configs/config.example.yaml`、`configs/config.yaml`（本机、被 `.gitignore` 排除）
- 预计规模：中

## 1. 任务目标

交付 `executors.adhoc` 这一节配置：结构、默认值、`Validate` 判据、环境变量覆盖、
热重载归类、两份 yaml 与注释。本卡结束时这份配置能被读出来并被正确校验，
但**还没有任何执行侧行为**（N04、N05 用它）。

## 2. 背景与当前问题

需求③④要求"提交任务时给出脚本位置或 URL"，这必须是一个显式打开的能力，
而且四个维度都要可调（解释器、路径范围、扩展名要求、URL 主机与私网）。
现成的先例是同一节里的 `web_enabled`（`core/config.go:272-278`）与 `profiles_path`（`:280-284`）：
默认 false、打开要求 `enabled=true`、重启档、注释里写清"为什么默认关"。

三个必须一起改的地方，漏一个都会被现成守卫抓出来：

| 机制 | 位置 | 漏了会怎样 |
| --- | --- | --- |
| 两份配置键集合 | `core/config_test.go:423` `TestExampleConfigMatchesLocal` | 失败（本机无 `config.yaml` 时是跳过，记录要写清） |
| 叶子归类 | `core/config_reload.go:60-96` 的表；守卫 `core/config_reload_test.go:24` `TestEveryLeafKeyIsClassed` | 新键两边都不在 → 分类 `ok=false` → 用例红 |
| 导出字段覆盖 | `core/config_reload_test.go:101` `assertEveryExportedFieldCovered` | 新结构体的字段摊不出叶子 → 用例红 |

## 3. 要实现的功能

### 3.1 结构

```go
// AdhocConfig 是"提交任务时给出执行位置"那一组开关（设计文档 §5.7）。
type AdhocConfig struct {
    // Enabled 是否注册四条内置自由执行档位（exec.php/exec.python/exec.shell/exec.http），默认 false。
    Enabled bool `mapstructure:"enabled"`
    // ShellRuntime exec.shell 用的解释器名，默认 bash；必须在 executors.runtime_allow 内。
    ShellRuntime string `mapstructure:"shell_runtime"`
    // PathPrefixes 允许的任务级脚本路径前缀（相对 executors.workspace 或绝对）；空表示不限（§12 P1）。
    PathPrefixes []string `mapstructure:"path_prefixes"`
    // RequireExtension 是否要求脚本扩展名与解释器匹配，默认 true。
    RequireExtension bool `mapstructure:"require_extension"`
    // URLHosts exec.http 允许的主机（写法照 executors.commands 的 allowed_hosts）；空表示不限主机。
    URLHosts []string `mapstructure:"url_hosts"`
    // URLAllowPrivate 打开后不再拒绝回环/私网/链路本地地址，默认 false。
    URLAllowPrivate bool `mapstructure:"url_allow_private"`
    // HTTPTimeout exec.http 这条档位的单次执行超时，留空用 DefaultAdhocHTTPTimeout（30s）。
    HTTPTimeout time.Duration `mapstructure:"http_timeout"`
}
```

挂在 `ExecutorsConfig`（`core/config.go:220`）下：`Adhoc AdhocConfig \`mapstructure:"adhoc"\``，
位置放在 `WebEnabled`/`ProfilesPath` 之后、`Output` 之前，与 yaml 里的顺序一致。

`core/config.go` 的默认值常量区（`DefaultExecProfilesPath` 在 `:212-214`）加一条
`DefaultAdhocShellRuntime = "bash"` 与 `DefaultAdhocHTTPTimeout = 30 * time.Second`。

### 3.2 `RequireExtension` 的默认值陷阱

`false` 是零值，而本项默认要是 `true`。照现有配置里"0 表示使用默认值"的写法处理不行——
布尔没有第三种取值。做法：字段用 `*bool`（`mapstructure:"require_extension"`），
`Normalized` 里 `nil` → `true`。注释必须写明为什么是指针，否则下一个人会"顺手改成 bool"。
`Enabled` 与 `URLAllowPrivate` 默认都是 `false`，用普通 `bool`。

### 3.3 `Validate` 判据（逐条要有用例）

在 `ExecutorsConfig` 的校验函数（`core/config.go:450` 一带）里追加：

1. `adhoc.enabled=true` 且 `enabled=false` → 报错。文案照 `web_enabled` 那一条的同款句式
   （`core/config.go:477` 的既有实现），理由同一句：没有执行能力却允许自由指定位置，只能是配置写错。
2. `adhoc.shell_runtime` 非空但不在 `runtime_allow` 内 → 报错（列出允许集合）。
   `runtime_allow` 为空按内置默认列表判（`core/config.go:234` 的注释）。
3. `adhoc.url_hosts` 的每一项按 `checkAllowedHosts`（`executor/profile.go:744`）的主机写法判。
   **注意依赖方向**：`core` 不许 import `executor`，所以本项在 `core` 侧只做基础形态检查
   （非空、不含空格、不含 `://`、不含 `/ ? # @`），完整判据由 N04 在 `executor` 侧构造档位时执行。
   这条分工要在代码注释里写明，不要为了"一次判完"而在 core 复制一份主机名正则。
4. `adhoc.path_prefixes` 的每一项非空、不含控制字符；相对路径按 `executors.workspace` 解析
   （真正的越界语义在 N05 用，本卡只管"写得对不对"）。
5. `adhoc.http_timeout` 为 0 合法（走默认），负数报错；超过 `executors.max_timeout` 报错，
   与档位的 `effectiveTimeout`（`executor/profile.go:907`）同一条口径。
6. `adhoc.url_allow_private=true` 而 `adhoc.url_hosts` 为空 → **合法但必须记一行启动 warn**
   （本卡只把它登记成 N04/N06 要打的日志条件，warn 由 N06 交付；`core` 侧不做日志）。

### 3.4 `Normalized`

给全部空值补默认：`shell_runtime="bash"`、`require_extension=true`、
`http_timeout=30s`、其余保持空。幂等（`Normalized()` 调两次结果一致），照既有归一化测试体例。

### 3.5 环境变量覆盖

`GODELAYQ_EXECUTORS_ADHOC_ENABLED=true` 一条即可（与 `GODELAYQ_EXECUTORS_WEB_ENABLED`
同一体例，`core/config.go:277`）。加键时同步那份键名清单（`core/config.go:785-800` 一带，
`executors.web_enabled` 在 `:794`；绑定动作在 `:824`，键名由点分路径自动换成 `GODELAYQ_*`）
——**字符串列表漏项不会报错，只会让该项无法用环境变量覆盖**（既有坑），
因此必须加一条测试证明"环境变量能把这一项打开"。
其余六项（`shell_runtime`、`path_prefixes`、`require_extension`、`url_hosts`、
`url_allow_private`、`http_timeout`）**不绑**：`path_prefixes` 与 `url_hosts` 是列表，
绑定会把"覆盖"变成"替换"，与 `executors.commands` 不绑的同一条理由一致（`:794` 上方的注释）；
其余四项在临时目录里改 yaml 即可，不值得为它们增加误用面。

### 3.6 热重载归类

`core/config_reload.go` 的表里把 `executors.adhoc.enabled`、`executors.adhoc.shell_runtime`、
`executors.adhoc.path_prefixes`、`executors.adhoc.require_extension`、`executors.adhoc.url_hosts`、
`executors.adhoc.url_allow_private`、`executors.adhoc.http_timeout` 全部标 `ClassRestart`，
理由与 `executors.enabled` 同一条：它们决定注册表里有没有这四条键、以及执行体身份边界。
注释里写一句"整节都是重启档"，避免下一个人以为 `http_timeout` 可以热更。

### 3.7 两份 yaml

在 `configs/config.example.yaml` 的 `executors:` 节里、`profiles_path`（`:132` 一带）之后插入
`adhoc:` 子节，注释体例照 `web_enabled` 那七行（`:115-124`）：默认值、为什么默认关、
打开的前置条件、重启档说明、设计文档指针、环境变量写法。

**必须同时改本机 `configs/config.yaml`**（含凭据、被 `.gitignore` 排除），
两份的键集合必须一致。注释里必须包含 §8 的运维须知四条
（`path_prefixes` 收紧、`require_extension` 保持 true、`url_hosts` 配起来、`required_role` 不降到 admin 以下）。

## 4. 实现步骤

1. `core/config.go`：结构体 + 默认常量 + 字段挂载 + `Validate` + `Normalized` + `BindEnv`。
2. `core/config_test.go`：默认值、六条 `Validate` 判据、`Normalized` 幂等、
   环境变量覆盖、`TestExampleConfigMatchesLocal` 自然覆盖两份同步。
3. `core/config_reload.go` 加归类；`core/config_reload_test.go` 若需要样本补一条新键被归类的断言。
4. `configs/config.example.yaml` 与 `configs/config.yaml` 同步。
5. `gofmt -w` 新增/改动的 go 文件；`go test ./core`；`go build ./... && go vet ./...`。

## 5. 测试要求

| 用例 | 断言 |
| --- | --- |
| 默认值 | 不给这一节时 `Enabled=false`、`ShellRuntime=bash`、`RequireExtension=true`、`HTTPTimeout=30s`、三个列表为空 |
| 打开要求 enabled | `adhoc.enabled=true` + `enabled=false` → `Validate` 报错 |
| shell_runtime 白名单 | `runtime_allow: [bash]` 时 `shell_runtime: pwsh` → 报错且错误文本列出允许集合 |
| url_hosts 形态 | 含空格 / 含 `://` / 含路径的写法各自报错；`*.example.com` 通过 |
| path_prefixes 形态 | 空串项、带控制字符项报错；`./scripts` 与 `D:/work/scripts` 通过 |
| http_timeout | 负数报错；超过 `max_timeout` 报错；0 走默认 |
| 指针默认 | yaml 不写 `require_extension` → 归一化后 true；显式 `false` → false（两条都要） |
| 环境变量 | `GODELAYQ_EXECUTORS_ADHOC_ENABLED=true` 能打开本项（`t.Setenv`） |
| 归类守卫 | `TestEveryLeafKeyIsClassed` 与 `assertEveryExportedFieldCovered` 绿 |
| 两份同步 | `TestExampleConfigMatchesLocal` 绿（或本机无 `config.yaml` 时记录为跳过） |

## 6. 完成标准（DoD）

1. `executors.adhoc.*` 七个键在 `core/config.go`、`configs/config.example.yaml`、
   `configs/config.yaml` 三处都出现，注释齐全。
2. §3.3 六条 `Validate` 判据逐条有用例；§3.4 归一化幂等；§3.5 环境变量覆盖有测试证据。
3. 两个热重载守卫用例绿，且七个新键都在 `ClassRestart` 表里（`grep` 自查）。
4. `TestExampleConfigMatchesLocal` 绿；若本机没有 `config.yaml` 而该测试是跳过，必须在 §10 写明。
5. 关闭状态（默认）下全仓行为零变化：`go test ./... -race -timeout 30m` 绿，
   且 `api`、`executor` 两侧既有用例一条都没被改动。

## 7. 验收方式

```bash
go test ./core -run "TestValidate|TestNormalized|TestConfigReload|TestExampleConfig" -v
go build ./... && go vet ./...
grep -n "adhoc" configs/config.example.yaml configs/config.yaml core/config.go core/config_reload.go | head -30
```

预期：测试全绿；`grep` 在四处都命中，且 `core/config_reload.go` 里七个键逐条出现。

## 8. 不在本任务范围

- 不注册任何档位、不改 `executor` 包（N04）。
- 不校验"路径是否真的存在"（N05）。
- 不打启动 warn（N06）。
- 不改热重载机制本身。

## 9. 风险与回滚

| 风险 | 说明 | 退路 |
| --- | --- | --- |
| `require_extension` 改成普通 `bool` | 默认值翻成 false，扩展名要求静默失效 | §3.2 + 一条用例（不写该项时必须为 true） |
| core 侧复制主机名/路径正则 | 与 `executor` 那一份分叉，表现是"配置过了、构造档位失败" | §3.3 第 3 条把分工写进注释；N04 的构造用一份 |
| yaml 只改 example 忘改本机 | 本机起不来或键集合守卫红 | DoD 第 4 条 |
| 环境变量列表漏项 | 覆盖不生效且不报错 | §3.5 的用例专门针对这条 |

回滚：删掉这一节配置与两份 yaml 段落即可；因为没有任何行为接入，回滚不影响其它卡。

## 10. 实现记录（执行时补写）

| # | 与卡片的偏离 | 原因 |
| --- | --- | --- |
