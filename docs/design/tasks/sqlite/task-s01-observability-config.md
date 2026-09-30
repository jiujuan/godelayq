# TASK-S01　`observability` 配置段与两份 YAML 模板

- 所属阶段：M0 基础
- 依赖任务：无
- 涉及文件：改 `core/config.go`、`core/config_test.go`、`configs/config.example.yaml`、本机 `configs/config.yaml`
- 预计规模：小

## 1. 任务目标

新增一个 `observability` 配置节，把 SQLite 观测层的开关、路径与保留策略读进来并校验，程序行为零变化（本卡不建表、不打开库）。

## 2. 背景与当前问题

设计文档 §1 列的三块数据（运行事件、产物索引、写操作审计）需要一处共同的开关与生命周期参数。

现有配置节不能借用：

- `store:` 一节描述的是任务快照的存放位置（`store.type` / `store.path`），观测库不改快照的存放方式，混进去会让人以为它和 `store.type` 有关（设计文档 §9.1）。
- `executors:` 一节描述执行能力，而运行事件表对不使用执行器的部署同样有用（普通任务也有 `job.*` 事件）。

另外要避开一个已有做法：`api/history.go:12-19` 的三档上限是常量，注释写着"未生效的选项不进配置"。本卡配置的每一项都有明确的读取方（S02 起逐个接手），因此不违反该惯例；但**本卡结束时确实还没有读取方**，所以要把"配置项在本卡校验、由 S02 消费"写进注释，避免后续误以为是遗留项而删掉。

## 3. 要实现的功能

1. 结构体（字段名与 YAML 键一一对应，`mapstructure` 标签照现有写法）：

   ```go
   // ObservabilityConfig 是 SQLite 观测层：运行事件、产物索引、写操作审计。
   // 默认关闭，关闭时下面所有取值都不生效，进程不创建任何文件。
   type ObservabilityConfig struct {
       Enabled       bool          `mapstructure:"enabled"`
       Path          string        `mapstructure:"path"`
       FlushInterval time.Duration `mapstructure:"flush_interval"`
       QueueCapacity int           `mapstructure:"queue_capacity"`
       BusyTimeout   time.Duration `mapstructure:"busy_timeout"`
       Synchronous   string        `mapstructure:"synchronous"` // normal|full
       Events    ObservabilityEventsConfig    `mapstructure:"events"`
       Artifacts ObservabilityArtifactsConfig `mapstructure:"artifacts"`
       Audit     ObservabilityAuditConfig     `mapstructure:"audit"`
   }

   type ObservabilityEventsConfig struct {
       Enabled        bool          `mapstructure:"enabled"`
       RetentionCount int           `mapstructure:"retention_count"`
       RetentionAge   time.Duration `mapstructure:"retention_age"`
   }
   type ObservabilityArtifactsConfig struct {
       Enabled bool `mapstructure:"enabled"`
   }
   type ObservabilityAuditConfig struct {
       Enabled        bool          `mapstructure:"enabled"`
       RetentionCount int           `mapstructure:"retention_count"`
       RetentionAge   time.Duration `mapstructure:"retention_age"`
   }
   ```

   `Config` 顶层加 `Observability ObservabilityConfig \`mapstructure:"observability"\``，位置放在 `Executors` 之后。
2. 默认值常量与默认值表（写法参照 `core/config.go:499` 附近的 `StoreConfig` 默认值）：

   | 键 | 默认值 | 说明 |
   | --- | --- | --- |
   | `enabled` | `false` | 总开关 |
   | `path` | `./data/observe.sqlite` | 与 `store.path`、`executors.output.dir` 同目录树但不同文件 |
   | `flush_interval` | `200ms` | 与 `DefaultFlushInterval`（`core/store.go:19`）同口径 |
   | `queue_capacity` | `4096` | 有界队列；满则丢弃并计数 |
   | `busy_timeout` | `2s` | |
   | `synchronous` | `normal` | |
   | `events.enabled` | `true` | 只在总开关打开时生效 |
   | `events.retention_count` | `200000` | |
   | `events.retention_age` | `720h` | 0 表示不按时间淘汰 |
   | `artifacts.enabled` | `true` | |
   | `audit.enabled` | `true` | |
   | `audit.retention_count` | `500000` | |
   | `audit.retention_age` | `2160h` | 90 天 |
3. `Normalized()`：空值补默认（`Path`、`FlushInterval<=0`、`QueueCapacity<=0`、`BusyTimeout<=0`、`Synchronous==""`，以及三个子节的 `RetentionCount<=0`、`RetentionAge<0`）。
   **`QueueCapacity` 与 `Store` 那几个"0 表示用默认值"的 setter 不同，显式写 0 在 `Validate` 里直接拒绝**：队列容量为 0 等于"一条都不留"，那不是"用默认值"的合理表达（本卡把这条差异写进注释，避免后续按 `SetExecConcurrency` 的先例反过来改）。
4. `Validate()`：
   - `enabled == false` 时**跳过本节全部校验**（与 `executors` 关闭时"本节取值全部不生效"同口径）。
   - `path` 去空格后为空 → 报错。
   - `synchronous` 只接受 `normal` / `full`，其它报错并列出两个取值。
   - `flush_interval < 0` → 报错；`< 10ms` → 报错并提示"低于 10ms 的批量写入等价于每写一条一事务"。
   - `busy_timeout < 0` → 报错。
   - 三个子节的 `enabled` 在总开关关闭时无意义但不报错（保持"关闭即惰性"的读法）。
5. **环境变量绑定列表**：把上面 13 个键逐个加入 `core/config.go:554-595` 的 `for _, key := range []string{...}`，它们即可获得 `GODELAYQ_OBSERVABILITY_*` 形式的覆盖（例如 `GODELAYQ_OBSERVABILITY_ENABLED=true`）。
   这条列表的作用要说准，两处常见误解：
   - **它只做 `BindEnv`**，不在列表里的键不会让启动失败，只是环境变量对它无效。
   - **未知键报错不来自这里**，而来自 `v.UnmarshalExact(&cfg)`（`core/config.go:605`）：判据是 `Config` 结构体有没有对应字段。所以"YAML 里写 `observability.*` 能被接受"完全由 §3.1 的结构体决定。
   - 本节的键全部绑定，包括 `path`：`configs/config.example.yaml` 顶部承诺"每一项都可被环境变量覆盖"，不绑定就是破例；且路径不是凭据（对照故意不绑的 `server.auth.users` 与 `executors.commands`，理由见 `core/config.go:553` 的注释）。
6. 两份 YAML 模板同步：`configs/config.example.yaml` 新增本节，注释风格照 `executors:` 一节（每一项写清取值含义、默认值、以及"关掉时不生效"）；本机 `configs/config.yaml` 同样加上（该文件被 `.gitignore` 排除，但守卫测试会比对两份的键集合）。

## 4. 实现步骤

1. 先加结构体 + 默认值 + `Normalized` + `Validate`，配 `core/config_test.go` 用例。
2. 加 `BindEnv` 列表项（`core/config.go:554-595`），然后跑 `TestLoadConfig_EnvOverrides`（`core/config_test.go:187`）——它只覆盖 14 个键，本卡要把新键补进去（至少 `enabled`、`path`、`flush_interval` 与一个嵌套键如 `audit.retention_age`，用来证明点号路径的绑定对子节同样生效）。
3. 改 `config.example.yaml`，再改本机 `config.yaml`，跑 `TestExampleConfigMatchesLocal`——它比对的是**两份文件的叶子键集合**（`core/config_test.go:409-436`），所以两份只改其一必然失败；注意本机没有 `config.yaml` 时它会 `Skip`，不能把它当成本卡唯一的守卫。
4. 最后确认 `cmd/server/main.go` 一行未改（本卡不接线）。

## 5. 测试要求

在 `core/config_test.go` 里补，全部用内存结构体与临时文件，不启动服务。

1. `TestObservabilityDefaults`：零值配置 `Normalized()` 后 13 项取值等于默认值表。
2. `TestObservabilityValidateRejectsBadValues`：表驱动，逐项覆盖 `path` 空白、`synchronous: off`、`flush_interval: -1s`、`flush_interval: 1ms`、`busy_timeout: -1s`、`queue_capacity: 0`（总开关为 true 时）。断言每条错误信息里含被拒的键名，便于运维定位。
3. `TestObservabilityIgnoredWhenDisabled`：把上一条的每个非法取值放进 `enabled: false` 的配置，断言 `Validate()` 通过。
4. `TestObservabilityPartialOverride`：只配 `path` 与 `audit.retention_age`，断言其余取默认、已配项不被覆盖。
5. `TestExampleConfigMatchesLocal`（已有）：两份 YAML 的叶子键集合一致。**这是"两份模板不同步"的唯一守卫**，与结构体字段、`BindEnv` 列表都无关，别把它当成本卡的全部保障。
6. `TestLoadConfig_EnvOverrides`（已有，`core/config_test.go:187`）扩展：用 `t.Setenv` 覆盖 `GODELAYQ_OBSERVABILITY_ENABLED`、`..._PATH`、`..._FLUSH_INTERVAL`、`..._AUDIT_RETENTION_AGE`，断言四处取值都生效。**第四条是嵌套键的绑定证据**，漏一个点号路径就会失败。
7. `TestUnknownKeyRejected`（已有相似用例）：往临时配置写一个 `observability.bogus: 1`，断言加载报错。它证明的是 `UnmarshalExact` 按**结构体字段**拒绝拼错的键（`core/config.go:605`），与 `BindEnv` 列表无关——写这条用例时要把这个区别留在注释里，否则下一个人会以为它在守 §3.5 那条列表。

## 6. 完成标准（DoD）

- [ ] 默认配置（不含 `observability` 一节）下加载成功，`Normalized()` 得到 §3.2 的全部默认值。
- [ ] `enabled: false` 时本节任何非法值都不报错，且程序不因此创建任何文件（本卡不打开库，这条自然成立，但要用一条用例把它固定成约束）。
- [ ] 13 个键全部进入 `core/config.go:554-595` 的 `BindEnv` 列表，并有 §5 第 6 条的环境变量覆盖断言（含至少一个嵌套键）。
- [ ] 写一个结构体里没有的 `observability.bogus` 键会被拒（§5 第 7 条），且注释说清这条由 `UnmarshalExact` 保证、与 `BindEnv` 列表无关。
- [ ] 两份 YAML 模板同步，`TestExampleConfigMatchesLocal` 通过。
- [ ] `cmd/server/main.go`、`api/`、`executor/`、`core/store.go` 一行未改（DoD 第 5 条：本卡只加配置形状）。
- [ ] 每个配置项的注释里说明"由哪张卡片消费"（S02/S03/S05/S06），避免被当成未生效项删掉。

## 7. 验收方式

```bash
go build ./... && go vet ./...
go test ./core -run 'Observability|ExampleConfigMatchesLocal|UnknownKey' -v
go test ./... -race
```

手工：临时把 `configs/config.yaml` 的 `observability.enabled` 改成 `true`、`path` 写成空串，
启动应失败并报出 `observability.path`；再改回 `enabled: false` + 非法 `synchronous`，启动应正常。
两种情况都不应在 `data/` 下产生任何新文件。

## 8. 不在本任务范围

- 不打开 SQLite、不建表（S02）。
- 不加 `store/sqlite` 包（S02）。
- 不改 `core.Store` / `JSONFileStore` / `store.type` 的校验分支（设计文档 §4.3 明确本期不动快照后端）。
- 不加"每张表单独的写入参数"这类细分配置：现在只有队列与周期两个参数是三表共用的，分表配置属于未生效选项。
- 不改 `README.md`（文档统一在 S07 收口）。

## 9. 风险与回滚

- 风险：`BindEnv` 列表漏项**不会让任何东西失败**——配置照常读、值照常生效，只有环境变量悄悄无效。这是本卡最容易漏的一处，而且它和"未知键报错"看着像同一件事（都在 `core/config.go` 同一段里）。应对：§5 第 6 条用例是这条的唯一守卫，四个键各一次断言，其中嵌套键那一次专门用来暴露点号路径写错。
- 风险：只改一份 YAML。`TestExampleConfigMatchesLocal` 能抓住，但它在本机没有 `configs/config.yaml` 时是 `Skip`（`core/config_test.go:413-415`），在 CI 上等于没有守卫。应对：本卡要求两份都改，并在第 10 节记录守卫测试的实际结果（通过还是跳过），不允许把"跳过"记成"通过"。
- 风险：`viper` 会把 map 键折成小写（`executor/profile.go:270` 的注释记过这个坑）。本节的键全是小写下划线，不受影响；但如果后续有人加 `mapstructure:",remain"` 或嵌套 map，要重新检查。
- 回滚：本卡是纯新增（结构体 + 默认值 + 校验 + 模板段落），单个提交可直接 revert，不影响任何运行路径。

## 10. 实现记录（2026-10-01）

改动文件：`core/config.go`（+206 行：`ObservabilityConfig` 与三个子节、9 个默认值常量、`DefaultConfig`、`ObservabilityConfig.Validate`、`Config.Validate` 的调用、`Normalized` 的 9 条补齐、`LoadConfig` 的 `BindEnv` 列表 +13 键）、`core/config_test.go`（+5 个测试函数与 `TestLoadConfig_EnvOverrides` 的 4 条新断言，共 +163 行）、`configs/config.example.yaml`（+51 行）、本机 `configs/config.yaml`（同一节，不入库）。

`cmd/server/main.go`、`api/`、`executor/`、`core/store.go` 一行未改（`git diff --stat` 只有上述三个入库文件）。

验证结果：
- `go build ./...`、`go vet ./...` 通过；`go test ./... -race` 全绿（api 51.8s、core 11.6s、executor 21.7s、cmd/server 5.5s）。
- `go build -tags dashboard ./...`、`GOOS=linux GOARCH=amd64 go build ./...`、`GOOS=darwin GOARCH=amd64 go build ./...` 通过。
- 格式检查：`gofmt -l core/config.go core/config_test.go` 报出 `core/config_test.go`，按 README 的口径剥掉行尾再比（`gofmt -d` 对 `tr -d '\r'` 后的副本）两个文件都无真实格式差异，未执行 `gofmt -w` 以免把 CRLF 改成 LF 造成整文件 diff。
- 新增测试全部通过：`TestObservabilityDefaults`、`TestObservabilityValidateRejectsBadValues`（7 个子用例 + 1 条错误信息内容断言）、`TestObservabilityIgnoredWhenDisabled`、`TestObservabilityPartialOverride`、`TestLoadConfig_RejectsUnknownObservabilityKeys`。
- `TestExampleConfigMatchesLocal` **通过（不是跳过）**：本机 `configs/config.yaml` 存在，两份的叶子键集合一致。
- `TestLoadConfig_EnvOverrides` 扩展后通过，四条观测层断言分别是 `enabled`、`path`、`flush_interval` 与嵌套键 `audit.retention_age`；同一条用例顺带断言未覆盖的 `events.retention_age` 保持默认值。
- 既有测试与断言零修改（除 `TestLoadConfig_EnvOverrides` 按卡片要求扩展）。
- 冒烟（系统临时目录 `%LOCALAPPDATA%\Temp\s01smoke` 里的独立二进制 + 三份独立配置 + 独立 `data/`，未触碰仓库的 `configs/config.yaml` 与 `data/`）：
  1. `enabled: true` + `path: "   "` → 启动失败，输出 `load config failed: observability.path must not be empty when observability.enabled is true`。
  2. `enabled: false` + `path: "  "` + `synchronous: off` + `flush_interval: 1ms` + `busy_timeout: -1s` + `queue_capacity: 0` 同时写 → 正常启动到路由注册完成。
  3. `enabled: true` + `path: ./data/observe.sqlite`（合法值）→ 正常启动，说明本卡没有打开库的路径；三种情况的 `data/` 目录里 `*.sqlite*` 文件数为 0（jobs.json 也未写出，进程被 timeout 终止）。跑完删除临时目录。

与卡片的差异（四处，均为实现时的判断）：
1. `synchronous` 的 `Validate` 额外放过空白/留空（`case "", "normal", "full"`），照 `executors.restore_policy`（`core/config.go:377-381`）的既有写法。卡片 §3.4 只写"只接受 normal/full，其它报错"，但 §3.3 又要求 `Normalized` 把 `Synchronous==""` 补成默认值——留空若直接报错，那条补齐规则就没有生效路径。非法取值（`off`）仍按卡片报错并列出两个取值。
2. §5.1 的"零值配置 `Normalized()` 后 13 项取值等于默认值表"对两个 `retention_age` 不成立：0 是"不按时间淘汰"的有意取值，`Normalized` 只把负值补回默认，这与 `store.history_ttl`、`executors.output.ttl` 同一读法（后者在 `TestExecutorsValidate_AllowsUnsetWhenDisabled` 里就有同方向的断言）。测试因此拆成两段：13 项默认值用 `DefaultConfig()` 断言，`Config{}.Normalized()` 只断言可补齐的 7 项，两个 `retention_age` 显式断言保持 0，并把理由写在注释里。`TestObservabilityPartialOverride` 另加一条 `retention_age: 0s` 的写法必须归一化后仍为 0。
3. §7 的手工验收原文是"临时把 `configs/config.yaml` 的 `observability.enabled` 改成 true"，与 README"全卡共同验证口径"最后一条（冒烟一律在系统临时目录造独立二进制+配置+数据目录，不得写入仓库的 `configs/config.yaml` 与 `data/`）冲突，取 README 的口径。三种情况覆盖的是同两条断言：非法值报错、关闭时惰性、不建文件。
4. `queue_capacity` 的拒绝条件写成 `<= 0` 而不是只拒 0：负值与 0 都是"一条都不留"，报错信息同一条更容易看懂。卡片 §3.3 只点名 0，§5.2 的表驱动用例同时含 `queue_capacity: 0`，本实现另加了一条 `-1` 的子用例。

留给后续卡的事：
- 本卡所有取值都没有读取方，`ObservabilityConfig` 只在 `core` 里被校验与归一化。S02 的 `store/sqlite.Open(cfg core.ObservabilityConfig, ...)` 接手 `enabled`/`path`/`flush_interval`/`queue_capacity`/`busy_timeout`/`synchronous` 六项（卡片 §3 的 PRAGMA 与 batcher 参数），S03 接手 `events.*`，S05 接手 `artifacts.enabled`，S06 接手 `audit.*`。字段注释与常量块开头都写了卡片编号，删项前先对照这里。
- S02 若要给 `path` 建父目录，请注意本卡不校验路径合法性（只拦空白），相对路径按进程工作目录解析，与 `store.path` 同一行为。
- `events.enabled`/`artifacts.enabled`/`audit.enabled` 三项在总开关关闭时不参与校验（卡片 §3.4 明确保持"关闭即惰性"），S03/S05/S06 若在子节上再加取值约束，需要同时决定是否把它们挪出"跳过"的范围，否则非法值在关闭态仍然无人拦。

引用核对（卡片与设计文档的行号在本卡执行时的实际值）：`core/config.go:499` 的 `StoreConfig` 默认值 ✓（块从 498 起）；`UnmarshalExact` 在 `core/config.go:605` ✓；`BindEnv` 列表卡片写 554-595，实际是 555-593（`for` 语句从 555 起、列表末项 592），本卡的 13 个键追加在 `executors.output.ttl` 之后；"故意不绑 `server.auth.users`"的注释在 551-554 ✓；`core/store.go:19` 的 `DefaultFlushInterval` ✓；`api/history.go:12-19` 的三档常量 ✓；`core/config_test.go:187`/`409-436`/`413-415` ✓；`executor/profile.go:270` 的 viper 折小写注释 ✓（本节键全为小写下划线，不受影响）。

