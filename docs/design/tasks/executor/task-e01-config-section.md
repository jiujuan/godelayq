# TASK-E01　新增 executors 配置段与两份 YAML 模板

- 所属阶段：M0 基础
- 依赖任务：无
- 涉及文件：`core/config.go`、`core/config_test.go`、`configs/config.example.yaml`、`configs/config.yaml`（本机文件，不入库）
- 预计规模：中（配置结构 + 校验 + 测试，无运行行为变化）

## 1. 任务目标

给程序加一节 `executors` 配置，把执行器的所有开关和参数收在这一节里。本任务只增加"能读、能校验、有默认值"，不注册任何执行器，不改任何现有行为。

## 2. 背景与当前问题

`core/config.go` 的 `Config` 现在只有 `server`、`scheduler`、`store`、`logging` 四节。执行器需要一批新开关（总开关、工作目录、允许的解释器、超时上限、输出大小与保存天数、档位列表）。

配置解码用的是 `LoadConfig` 里的 `v.UnmarshalExact`，也就是**写了没定义的键就直接报错**。所以"加配置段"必须先于"用配置段"落地，否则测试环境的 YAML 会启动失败。这也是本任务单独成卡的原因。

## 3. 要实现的功能

1. `Config` 增加 `Executors ExecutorsConfig` 字段，`mapstructure:"executors"`。
2. `ExecutorsConfig` 及其子结构的字段与注释（每个字段的含义、单位、0 值语义）：

   ```go
   type ExecutorsConfig struct {
       Enabled         bool              `mapstructure:"enabled"`
       RequiredRole    string            `mapstructure:"required_role"`
       Workspace       string            `mapstructure:"workspace"`
       RuntimeAllow    []string          `mapstructure:"runtime_allow"`
       EnvAllow        []string          `mapstructure:"env_allow"`
       Concurrency     int               `mapstructure:"concurrency"`
       QueueCapacity   int               `mapstructure:"queue_capacity"`
       DefaultTimeout  time.Duration     `mapstructure:"default_timeout"`
       MaxTimeout      time.Duration     `mapstructure:"max_timeout"`
       RestorePolicy   string            `mapstructure:"restore_policy"`
       LoaderAllow     bool              `mapstructure:"loader_allow"`
       Output          ExecutorOutput    `mapstructure:"output"`
       Commands        []map[string]any  `mapstructure:"commands"`  // 本任务只声明类型，E02 才定义正式结构
   }

   type ExecutorOutput struct {
       InlinePreview int           `mapstructure:"inline_preview"`
       MaxBytes      int           `mapstructure:"max_bytes"`
       Dir           string        `mapstructure:"dir"`
       TTL           time.Duration `mapstructure:"ttl"`
   }
   ```

   `Commands` 在 E02 会替换成正式的 `[]ExecutorCommand` 结构，本任务用宽松类型先跑通解码，避免一次改动过大。
3. `DefaultConfig()` 给出与 `../../executor-design.md` §6.7 一致的默认值：`enabled=false`、`required_role=admin`、`workspace=./exec-workspace`、`runtime_allow` 八项、`env_allow` 五项、`concurrency=4`、`queue_capacity=0`、`default_timeout=5m`、`max_timeout=30m`、`restore_policy=pause`、`loader_allow=false`、`output` 为 `2048 / 262144 / ./data/exec / 168h`。
4. `Validate()` 增加检查，错误信息格式照抄同文件既有体例（`字段名 must ...`，带 `got %v`）：
   - `required_role` 必须能被 `ParseRole` 解析，且不接受 `machine`；`viewer` 视为过于宽松并报错（能提交任务至少是 operator 档，见 §6.7 的说明）。
   - `workspace`、`output.dir` 非空。
   - `concurrency`、`queue_capacity`、`output.inline_preview`、`output.max_bytes` 不为负；`enabled=true` 时 `concurrency>=1`、`max_bytes>=1024`。
   - `default_timeout`、`max_timeout`、`output.ttl` 不为负；`enabled=true` 时 `default_timeout>0` 且 `default_timeout<=max_timeout`。
   - `restore_policy` 取 `pause|replay`；`loader_allow=true` 而 `enabled=false` 时直接报错（这是明显写错，不要静默接受）。
   - `runtime_allow`、`env_allow` 不含空串或带空格/控制字符的项；`env_allow` 里出现 `GODELAYQ_` 前缀项时报错（这类键会把服务端凭据传给子进程）。
   - `enabled=true` 且 `server.auth` 未启用（`AuthConfig.Enabled()` 为 false）时**不报错，但需要能被打断**：`Validate` 里保持不报错，把警告放到 E04 的启动日志，这样测试环境仍能开执行器做单测。
5. `Normalized()` 把 0 值补成默认值（写法与 `Scheduler.Workers == 0` 那几段一致）。
6. `LoadConfig` 的 `BindEnv` 白名单追加标量键：`executors.enabled`、`executors.workspace`、`executors.required_role`、`executors.concurrency`、`executors.queue_capacity`、`executors.default_timeout`、`executors.max_timeout`、`executors.restore_policy`、`executors.loader_allow`、`executors.output.dir`、`executors.output.ttl`、`executors.output.max_bytes`、`executors.output.inline_preview`。
   `executors.commands` 与 `server.auth.users` 一样不绑定环境变量，在 `LoadConfig` 上方那段既有注释里补一句说明。
   `executors.runtime_allow`、`executors.env_allow` 用 viper 的逗号分隔列表覆盖，加进白名单。
7. `configs/config.example.yaml` 追加整段，注释风格照该文件既有体例（中文、说明单位、说明 0 值语义、标出用哪个环境变量覆盖）。`commands` 用注释掉的示例块，不要写 `commands: []`（照 `server.auth.users` 那段关于"空序列与块式序列会撞在一起"的既有提醒）。
8. 本机 `configs/config.yaml` 同步同一节，取值全部用默认（保持关闭）。

## 4. 实现步骤

1. 在 `core/config.go` 加结构体与字段，写默认值。
2. 加 `Validate` 分支和 `Normalized` 分支。
3. 加 `BindEnv` 键。
4. 改 `configs/config.example.yaml`，再同步本机 `configs/config.yaml`。
5. 写 `core/config_test.go` 的用例（见第 5 节）。
6. 跑全量验证。

## 5. 测试要求

`core/config_test.go` 新增（表驱动，风格照该文件既有的 `Validate` 用例）：

1. `TestExecutorsDefaults`：`DefaultConfig().Normalized()` 各字段等于第 3 节列出的默认值。
2. `TestExecutorsValidate_Rejects`：逐个构造非法值，断言错误信息里出现对应键名。至少覆盖：`required_role: machine`、`required_role: viewer`、`default_timeout > max_timeout`、`restore_policy: skip`、`loader_allow` 与 `enabled` 矛盾、`env_allow` 含 `GODELAYQ_SERVER_AUTH_TOKEN`、`concurrency: 0` 且 `enabled: true`。
3. `TestExecutorsValidate_AllowsDisabled`：`enabled: false` 时，`concurrency: 0`、`default_timeout: 0` 这些"没填"的写法不报错。
4. `TestLoadConfig_ExecutorsFromYAML`：写一份临时 YAML（含一个 `commands` 项）加载回来，断言字段值和时长单位解析正确。
5. `TestLoadConfig_ExecutorsEnvOverride`：`t.Setenv("GODELAYQ_EXECUTORS_CONCURRENCY","8")` 与 `GODELAYQ_EXECUTORS_RUNTIME_ALLOW=node,php` 生效（**不要用前导逗号**：`,node,php` 会被拆出一个空项，正好触发第 3.4 条的"空白项"拒绝，实测已确认）。
6. `TestLoadConfig_RejectsUnknownExecutorsKey`：写 `executors.allow_raw_command: true`（设计上不存在的键）必须报错——这条用来固定"不提供绕过白名单的命令入口"。
7. 既有的 `TestExampleConfigMatchesLocal` 必须继续通过（第 8 步同步两份文件就是为它）。

## 6. 完成标准（DoD）

- [ ] `Config` 多出一节，`DefaultConfig`/`Validate`/`Normalized`/`LoadConfig` 四处都覆盖到，没有"字段声明了但没人校验"的情况。
- [ ] 两份 YAML 的键集合一致，`TestExampleConfigMatchesLocal` 通过。
- [ ] 默认配置下 `enabled=false`，本任务对所有现有测试零影响：不改任何现有断言就能跑绿。
- [ ] 每个新字段都有注释，说明含义、单位和 0 值语义；注释风格与同文件一致。
- [ ] 没有引入执行器行为：`api`、`core` 的执行路径没有任何改动。

## 7. 验收方式

```bash
go build ./... && go vet ./... && go test ./... -race
go test -run 'TestExecutors|TestExampleConfigMatchesLocal|TestLoadConfig' ./core -v
```

预期：全部 `ok`；`TestLoadConfig_RejectsUnknownExecutorsKey` 里那个未知键确实报错（说明"没有绕过白名单的入口"是可以验证的，不只是文档承诺）。

## 8. 不在本任务范围

- 不定义 `ExecutorCommand` 的正式结构（E02）。
- 不做探测、不注册 Handler（E03、E04）。
- 不加 HTTP 端点。
- 不改 `scheduler.*` 或 `store.*` 的既有键。

## 9. 风险与回滚

- 风险：`Commands []map[string]any` 是临时形状，E02 会换掉。因此 E02 的卡片里必须重跑本卡的第 5 节测试，不能认为配置测试已经做完。
- 风险：`BindEnv` 用了 viper 的逗号分隔列表，值里含逗号的解释器名会被拆开。当前解释器名不含逗号，可接受；如未来要支持，改用显式分隔符并记进设计文档。
- 回滚：本任务只加字段与两份 YAML 的一节，`git revert` 单提交即可，无数据迁移。

## 10. 实现记录（2026-09-29）

改动文件：`core/config.go`（+284 行：`ExecutorsConfig`/`ExecutorOutputConfig`、`DefaultConfig`、`Validate`、`Normalized`、`LoadConfig` 的 `BindEnv` 列表）、`core/config_test.go`（+5 个测试函数）、`configs/config.example.yaml`（+68 行）、本机 `configs/config.yaml`（同步同一节，不入库）。

验证结果：
- `go build ./...`、`go vet ./...` 通过；`go test ./... -race` 全绿（api 46.6s、core 8.6s、cmd/server 5.7s）。
- 新增测试：`TestExecutorsDefaults`、`TestExecutorsValidate_Rejects`（22 个子用例）、`TestExecutorsValidate_AllowsUnsetWhenDisabled`、`TestLoadConfig_ExecutorsFromYAML`、`TestLoadConfig_ExecutorsEnvOverrides`、`TestLoadConfig_RejectsUnknownExecutorsKeys`，全部通过。
- `TestExampleConfigMatchesLocal` 通过（本机 `configs/config.yaml` 存在且键集合一致，非跳过）。
- 既有测试与断言零修改。
- 冒烟（真实二进制，临时端口）：① `-config=configs/config.yaml` 正常启动；② 直接用 `-config=configs/config.example.yaml` 正常启动（验证新人 `cp` 之后的模板可用）；③ `GODELAYQ_EXECUTORS_ENABLED=true GODELAYQ_EXECUTORS_CONCURRENCY=0` 启动失败，报 `executors.concurrency must be at least 1 when executors.enabled is true, got 0 (omit the key to use 4)`；④ `GODELAYQ_EXECUTORS_ENABLED=true` 单独打开、其余用默认值，正常启动。

与卡片的差异（两处，均为实现时的判断）：
1. `output.ttl` 不参与 `Normalized` 的"0 补默认值"：0 表示"不按时间清理"，是有效取值而非未配置，与既有 `store.history_ttl` 的口径一致。已用测试 `TestExecutorsValidate_AllowsUnsetWhenDisabled` 钉住。
2. 环境变量列表覆盖的例子改为不含前导逗号（见第 5 节修订）。

留给 E02 的两件事：`ExecutorsConfig.Commands` 目前是 `[]map[string]any`，换成正式结构后要重跑本卡全部测试；`TestLoadConfig_RejectsUnknownExecutorsKeys` 里那条特征化子用例（档位内部键名拼错此时发现不了）在 E02 会失败，届时连同注释一起改掉。
