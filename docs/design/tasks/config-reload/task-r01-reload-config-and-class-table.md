# TASK-R01　reload 配置节、三档分类表与 Diff

- 所属阶段：M0 判据
- 依赖任务：无
- 涉及文件：`core/config.go`、`core/config_reload.go`（新增）、`core/config_reload_test.go`（新增）、
  `core/config_test.go`、`configs/config.example.yaml`、`configs/config.yaml`
- 预计规模：中

## 1. 任务目标

新增 `reload.enabled` 与 `reload.debounce` 两个配置键，并交付"每个叶子键属于热更 / 重启 / 拒绝
哪一档"的唯一分类表与 `Diff(applied, candidate)`。本卡结束时**没有任何行为变化**：
新键读得到、两份配置能比出差异，但还没有任何人调用它，也没有监听器。

## 2. 背景与当前问题

配置只在启动时读一次（`cmd/server/main.go:318` → `core.LoadConfig`，`core/config.go:717`），
之后每个子系统持一份启动快照。要改这个局面，第一步不是去动某个子系统，而是先把
"哪些取值允许运行期换、哪些不允许"写成一处、并由测试钉住。缺了这一步的后果是每个实现者
按自己的判断决定一项改不改：凭据可能被静默换掉，而"能执行什么"的边界可能变成免重启通道
（设计文档 §9）。

本卡交付的三样东西都是纯判据，没有副作用：

- `ReloadConfig`：两个新键。
- `configClasses`：叶子路径 → 档位，`ClassHot` / `ClassRestart` / `ClassReject`。
- `Diff`：两份 `Config` → 三份清单。

## 3. 要实现的功能

### 3.1 `reload` 一节

```go
// core/config.go
// DefaultReloadDebounce 是配置监听的事件合并窗口（设计文档 §5.1、待拍板 P3）。
const DefaultReloadDebounce = 500 * time.Millisecond

// minReloadDebounce 是可用取值下界：比它小等于每次存盘读好几遍配置文件。
const minReloadDebounce = 50 * time.Millisecond

// ReloadConfig 配置热重载的开关与节奏。整体默认关闭。
type ReloadConfig struct {
        // Enabled 总开关。false 时不建监听器，进程行为与本节不存在时一致。
        // 这一项自身属于重启档：运行期关掉监听只能重启。
        Enabled bool `mapstructure:"enabled"`
        // Debounce 事件合并窗口；0 表示 DefaultReloadDebounce。
        // 低于 minReloadDebounce 由 Validate 拒绝。这一项可以热更。
        Debounce time.Duration `mapstructure:"debounce"`
}
```

配套五处，缺一处就是静默失效：

1. `Config` 结构体加 `Reload ReloadConfig \`mapstructure:"reload"\``
   （`core/config.go:25-35`，跟在 `Observability` 后面）。
2. `DefaultConfig()` 补 `Reload: ReloadConfig{Enabled: false, Debounce: DefaultReloadDebounce}`
   （插入位置见 `core/config.go:630-711` 末尾，`Observability` 之后）。
3. `Normalized()` 补 `if c.Reload.Debounce == 0 { c.Reload.Debounce = defaults.Reload.Debounce }`
   （`core/config.go:907` 起那一段的末尾）。
4. `Validate()` 加两条：`enabled=true` 时 `debounce < minReloadDebounce` 拒绝；
   `debounce < 0` 一律拒绝。文案照 `core/config.go:848-856` 的既有体例
   （说清合法取值范围与实际读到的值）。注意方向与 `executors.*` 那组不同：
   `debounce` 在关闭状态下也没有"合法的 0 值写法"，因为它会被 `Normalized` 补成默认值，
   所以负数与过小一律拒，不必挂在 `Enabled` 上。
5. `LoadConfig` 的 `BindEnv` 列表加 `"reload.enabled"` 与 `"reload.debounce"`
   （`core/config.go:734-794`）→ `GODELAYQ_RELOAD_ENABLED`、`GODELAYQ_RELOAD_DEBOUNCE`。
   **漏在这份列表里不会报错，只会让键无法用环境变量覆盖**，§5.2 的用例守这条。

### 3.2 叶子摊平

```go
// core/config_reload.go
// leafValue 是摊平出来的一个取值。Kind 用来让分类与比较对"空列表"和"没有这个键"
// 给出不同结论——executors.commands 从没有条目变成有一条，是一次真实的能力变化。
type leafValue struct {
        Kind leafKind // leafScalar | leafSlice | leafMap
        Path string
        Val  reflect.Value
}

// flattenLeaves 把 Config 结构体摊成叶子清单。规则：
//   - 结构体字段按 mapstructure 标签拼成点分路径，递归展开；
//   - executors.commands 的切片按元素展开成 executors.commands.<name>.<field>
//     （name 取该条的 Name 字段，空名的条目按索引 executors.commands.#<i>.<field>），
//     因为"改了哪一条的哪个字段"必须能被分类；
//   - 其余切片、映射与标量（含 time.Duration）都是叶子，值用 reflect.DeepEqual 比。
func flattenLeaves(cfg Config) map[string]leafValue
```

为什么不复用 `core/config_test.go:437-444` 的 `configKeys`：那份是从 YAML **文件**读键名，
只给路径不给值，且要求文件存在。本卡要对两份内存里的 `Config` 比取值，所以是 `core` 内部
新写的一份 `reflect` 实现；`configKeys` 保持原样不动（它是 `TestExampleConfigMatchesLocal`
的既有用法，改它等于同时改两条守卫的口径）。

### 3.3 分类表

```go
// ConfigClass 是一个叶子键在重载里的归属（设计文档 R1）。
type ConfigClass int

const (
        ClassHot     ConfigClass = iota // 重载时应用
        ClassRestart                    // 重载时接受但不应用，逐条记入 ignored_keys
        ClassReject                      // 变化即整次作废
)

// String 返回 hot|restart|reject，供日志与测试输出使用。
func (c ConfigClass) String() string

// configClasses 是三档分类的唯一权威表：叶子路径 → 档位。
// 表里有、结构体里没有的键（前缀规则命中）也要能分类，所以判定时先看精确路径，
// 再看下表里的前缀规则（凭据那一组是前缀）。
var configClasses = map[string]ConfigClass{
        // 热更：取值型参数，改动不扩大任何能力边界
        "logging.level":                            ClassHot,
        "scheduler.workers":                        ClassHot,
        "scheduler.max_retry_delay":                ClassHot,
        "store.history_limit":                      ClassHot,
        "store.history_ttl":                        ClassHot,
        "observability.events.retention_count":     ClassHot,
        "observability.events.retention_age":       ClassHot,
        "observability.audit.retention_count":      ClassHot,
        "observability.audit.retention_age":        ClassHot,
        "executors.commands":                       ClassHot, // 条目增删本身
        "reload.debounce":                          ClassHot,

        // 重启：绑着 Start 期建立的通道/ticker/连接，或属于重新装配
        "server.port":                ClassRestart,
        "scheduler.queue_capacity":   ClassRestart,
        "scheduler.shutdown_timeout": ClassRestart,
        "logging.format":             ClassRestart,
        "reload.enabled":             ClassRestart,
        // 其余全部落在重启档：用一张默认表反而看不清"哪些键想过关"，
        // 所以下面这些也逐条列出（值相同，作用是让读的人知道它们被认真归过类）。
        "store.type": ClassRestart, "store.path": ClassRestart,
        "store.flush_interval": ClassRestart, "store.groups_path": ClassRestart,
        "observability.enabled": ClassRestart, "observability.path": ClassRestart,
        "observability.flush_interval": ClassRestart, "observability.queue_capacity": ClassRestart,
        "observability.busy_timeout": ClassRestart, "observability.synchronous": ClassRestart,
        "observability.events.enabled": ClassRestart, "observability.artifacts.enabled": ClassRestart,
        "observability.audit.enabled": ClassRestart,
        "executors.enabled": ClassRestart, "executors.required_role": ClassRestart,
        "executors.workspace": ClassRestart, "executors.runtime_allow": ClassRestart,
        "executors.env_allow": ClassRestart, "executors.concurrency": ClassRestart,
        "executors.queue_capacity": ClassRestart, "executors.default_timeout": ClassRestart,
        "executors.max_timeout": ClassRestart, "executors.restore_policy": ClassRestart,
        "executors.loader_allow": ClassRestart, "executors.web_enabled": ClassRestart,
        "executors.profiles_path": ClassRestart,
        "executors.output.inline_preview": ClassRestart, "executors.output.max_bytes": ClassRestart,
        "executors.output.dir": ClassRestart, "executors.output.ttl": ClassRestart,
        "server.cors.allow_origins": ClassRestart, "server.cors.allow_credentials": ClassRestart,
        "server.auth.jwt.access_ttl": ClassRestart, "server.auth.jwt.refresh_ttl": ClassRestart,
}

// rejectPrefixes 是凭据那一组：整棵子树拒绝，包括按元素展开后的用户条目。
var rejectPrefixes = []string{
        "server.auth.token",
        "server.auth.users",
        "server.auth.jwt.secret",
}

// permissionCommandFields 是档位内的身份、目标与凭据字段（设计文档 §6.4、待拍板 P2）。
// 这些字段变了 → 整次作废；档位其余字段（timeout、max_parallel、retry_on_exit、
// args/args_render/positional、expect_status、capture_response、max_body_bytes）
// 以及条目增删 → 热更。
// 清单必须显式列字段：将来给 ExecutorCommand 加字段时，新字段默认落在
// "允许热更"一侧，由 §5.1 的守卫用例逼着人显式回答"它落在哪一侧"。
var permissionCommandFields = map[string]bool{
        // 跑哪个可执行体
        "runtime":    true,
        "script":     true,
        "program":    true,
        "fixed_args": true,
        "cwd":        true,
        // 以什么身份跑
        "env":       true, // 固定注入的凭据材料：控制台从不回显它的取值
        "env_allow": true,
        // 把请求发到哪里、怎么发
        "method":          true,
        "url_template":    true,
        "allowed_hosts":   true,
        "headers":         true,
        "header_allow":    true,
        "deny_private_ranges": true,
        "max_redirects":   true,
}
```

分类判定函数（`Diff` 与守卫用例共用一份，避免两套规则）：

```go
// classify 返回一个叶子路径的档位。ok 为 false 表示既没有精确命中也没有前缀命中，
// 属于"新增键忘了归档"，调用方（守卫用例）据此报错。
func classify(path string) (class ConfigClass, ok bool)
```

判定顺序：精确路径 → `rejectPrefixes` 前缀 → `executors.commands.<name|.#i>.<field>`
形式的字段名（命中 `permissionCommandFields` 则 `ClassReject`，否则该路径归 `ClassHot`）。
最后一条要在 `classify` 里用 `strings.Cut` 拆出字段名，不要把档位字段单独存一份表。

### 3.4 Diff

```go
// ChangedKey 是一次改动的路径与两侧取值。Val 为 any（叶子原值），日志与
// /admin/runtime 只输出路径，不输出值——配置里可能有凭据。
type ChangedKey struct {
        Path string
        Old  any
        New  any
}

// ConfigChange 是 Diff 的结果，三份清单的键都按路径字典序排好。
type ConfigChange struct {
        Hot      []ChangedKey
        Restart  []ChangedKey
        Reject   []ChangedKey
        Commands []ChangedKey // Hot 里属于 executors.commands 的那批，单列供 R04/R06 判定
}

func (c ConfigChange) HasChanges() bool
func (c ConfigChange) HasRejections() bool

// Diff 比较两份配置，得出"该应用的、需要重启的、导致作废的"三份清单。
// 两份都必须是 Normalized() 之后的取值，否则 0 值与默认值的差别会被当成一次改动
// （签名不返回 error 但要断言：入参来自 Config.Normalized()，见 §9 风险表）。
func Diff(applied, candidate Config) ConfigChange
```

三条行为要求：

1. 无变化的键不进任何清单。
2. `Reject` 非空时，调用方整次作废；`Diff` 本身仍然把 `Hot` 填好，方便日志说清
   "本来会应用哪些"，但不代表它们被应用了。
3. 切片从 `nil` 变成一条元素、从一条变成空切片：算改动（`leafValue.Kind` + `reflect.DeepEqual`
   能区分，`Kind` 相同再比 `Val`）。

### 3.5 `ReloadState`

放在本卡（`core/config_reload.go`），因为产出它的三方（watcher、`cmd/server` 的重载链、
`api` 的读端点）都在 `core` 之下或同层，结构体必须落在依赖方向的下层（设计文档 §5.4）。
本卡只定义类型与 `String()` 结论名，不写入、不读取。

```go
// ReloadResult 是最近一次重载尝试的结论。
type ReloadResult string

const (
        ReloadOK        ReloadResult = "ok"
        ReloadUnchanged ReloadResult = "unchanged"
        ReloadRejected  ReloadResult = "rejected"
        ReloadFailed    ReloadResult = "failed"
        ReloadDegraded  ReloadResult = "degraded"
)

type ReloadState struct {
        Enabled       bool         `json:"enabled"`
        WatchedPath   string       `json:"watched_path,omitempty"`
        LastAttemptAt time.Time    `json:"last_attempt_at,omitempty"`
        LastAppliedAt time.Time    `json:"last_applied_at,omitempty"`
        Result        ReloadResult `json:"result"`
        Error         string       `json:"error,omitempty"`
        AppliedKeys   []string     `json:"applied_keys,omitempty"`
        IgnoredKeys   []string     `json:"ignored_keys,omitempty"`
        RejectedKeys  []string     `json:"rejected_keys,omitempty"`
        WatcherError  string       `json:"watcher_error,omitempty"`
}
```

## 4. 实现步骤

1. `core/config.go`：加 `ReloadConfig` 类型与两个常量（放在 `LoggingConfig`
   （`core/config.go:174-179`）之后，与其它小节同位置体例）、`Config.Reload` 字段、
   `DefaultConfig`/`Normalized`/`Validate`/`BindEnv` 四处配套。注释里写清默认值、
   能否用环境变量覆盖、以及"这一项自身改了不生效"。
2. `configs/config.example.yaml` 与 `configs/config.yaml`：加 `reload:` 一节，
   文案照设计文档 §5.1（含"默认关闭""关闭时一个监听器都不建""`enabled` 改了要重启"
   "`debounce` 可以热更"四条）。**两份都要改**。
3. 新增 `core/config_reload.go`：`ConfigClass` → `configClasses` → `rejectPrefixes` →
   `permissionCommandFields` → `classify` → 摊平 → `Diff` → `ReloadState`。
   小节顺序按上面这个依赖顺序排，方便后来人对着读。
4. 新增 `core/config_reload_test.go`：§5.1、§5.3、§5.4 的用例。
5. `core/config_test.go`：加 `TestLoadConfig_ReloadEnvOverrides` 与 `Validate` 的两条新断言（§5.2）。
6. 跑 §7 的三条命令，再跑全量 `go build ./... && go vet ./... && go test ./... -race`。

## 5. 测试要求

### 5.1 守卫用例（本卡最重要的一条）

```go
// TestEveryLeafKeyIsClassed 断言 Config 摊平出来的每一个叶子路径都能被 classify 归档。
// 样本必须含一条 script 与一条 http，否则 http 那一组字段摊不出来，守卫等于没守。
func TestEveryLeafKeyIsClassed(t *testing.T) {
        sample := DefaultConfig()
        deny := false
        sample.Executors.Commands = []ExecutorCommand{
                {
                        Name: "cfg-script-one", Kind: "script",
                        Runtime: "bash", Script: "scripts/one.sh",
                        Timeout: 5 * time.Minute,
                        Args:    []ExecutorArg{{Name: "day", Required: true}},
                        ArgsRender: []string{"--day={day}"},
                },
                {
                        Name: "cfg-http-one", Kind: "http",
                        Method: "POST", URLTemplate: "https://api.example.com/jobs",
                        AllowedHosts: []string{"api.example.com"},
                        Headers:      map[string][]string{"Accept": {"application/json"}},
                        Body:         "json", CaptureResponse: true,
                        DenyPrivate: &deny,
                },
        }

        leaves := flattenLeaves(sample)
        if len(leaves) < 60 {
                t.Fatalf("flattenLeaves gave %d leaves, the walker is probably missing a section", len(leaves))
        }
        for path := range leaves {
                if _, ok := classify(path); !ok {
                        t.Errorf("leaf key %q has no reload class; add it to configClasses", path)
                }
        }

        // 反向：表里有、结构体摊不出来的路径说明键名改了而表里留着旧条目（那种键会永久静默）
        for path := range configClasses {
                if _, ok := leaves[path]; !ok && !isCommandOrCredentialPrefix(path) {
                        t.Errorf("configClasses has %q but DefaultConfig does not produce it", path)
                }
        }
        for path := range permissionCommandFields {
                if !hasCommandField(leaves, path) {
                        t.Errorf("permissionCommandFields lists %q but no executors.commands.* leaf carries it", path)
                }
        }
}
```

`isCommandOrCredentialPrefix(path)` 认两类合法条目：`executors.commands`（条目增删本身）与
`rejectPrefixes` 里那三条前缀（凭据的子路径按元素摊开，精确路径本来就不在 `configClasses` 里）。
`hasCommandField(leaves, field)` 在摊出的 `executors.commands.<name>.<field>` 集合里找这个字段名
——它是"清单里的字段名写错了"这类笔误的守卫。
两个辅助函数都放在测试文件里，不外溢。

同一条用例还要反向查表：`configClasses` 里的键若在 `DefaultConfig()` 摊出的路径集合里找不到，
也要报错（防止改了键名而表里留着旧路径——那种键会永久静默）。
样本必须包含 http 档位，否则 `deny_private_ranges` 一类的字段摊不出来，守卫等于没守。

### 5.2 配置读取

- `TestLoadConfig_ReloadDefaults`：不写 `reload:` 时 `Enabled=false`、
  `Debounce=DefaultReloadDebounce`。
- `TestLoadConfig_ReloadEnvOverrides`：`t.Setenv("GODELAYQ_RELOAD_ENABLED","true")` +
  `t.Setenv("GODELAYQ_RELOAD_DEBOUNCE","2s")`，断言两个值真被覆盖
  （对照既有 `GODELAYQ_EXECUTORS_RUNTIME_ALLOW` 那条的写法）。
- `TestValidate_Reload`：`debounce: 10ms`（enabled=true）被拒、`debounce: -1s` 被拒、
  `enabled: false` + `debounce: 10ms` **也被拒**（§3.1 第 4 条的方向），
  三条都用错误文案里的关键字断言。

### 5.3 Diff 分类

一张表驱动用例，每行给 `applied` 与 `candidate` 的差异与期望落在哪一份清单：

| 改的键 | 期望 |
| --- | --- |
| `logging.level` info→debug | `Hot` |
| `scheduler.workers` 100→32 | `Hot` |
| `store.history_ttl` 0→24h | `Hot` |
| `observability.events.retention_count` | `Hot` |
| `reload.debounce` | `Hot` |
| `executors.commands` 增一条 script、改另一条的 `timeout` | `Hot` + `Commands` |
| `executors.commands` 某条的 `script` 换路径 | `Reject` |
| `executors.commands` 某条的 `runtime` 换解释器 | `Reject` |
| `executors.commands` 某条 http 的 `allowed_hosts` | `Reject` |
| `executors.commands` 某条 http 的 `deny_private_ranges` false→true | `Reject` |
| `executors.commands` 某条的 `env` 加一个键 | `Reject` |
| `server.port` | `Restart`，且 `HasChanges()` 为真 |
| `server.auth.token` | `Reject` |
| `server.auth.users` 增一条 | `Reject` |
| `server.auth.jwt.secret` | `Reject` |
| `executors.workspace`、`executors.runtime_allow` | `Restart`（不是 `Reject`，见设计文档 §6.4） |
| 什么都没改 | 三份清单全空、`HasChanges()` 为假 |

最后再加一条 `HasRejections()` 的用例：一次改动同时含 `logging.level` 与
`server.auth.token` 时，`Reject` 有内容、`Hot` 也有内容，调用方据此整次作废。

### 5.4 手工

临时目录里造一份含 `reload: {enabled: true, debounce: 10ms}` 的配置，
`go run ./cmd/server -config=...` 应**启动失败**并给出 `reload.debounce` 的错误文案。
这是"新键真的进了结构体"的最硬证据（本卡还没有监听器，所以除此之外不该有任何现象）。

## 6. 完成标准（DoD）

- [ ] `reload.enabled` / `reload.debounce` 能读、能归一化、能被环境变量覆盖，
      `TestExampleConfigMatchesLocal` 通过（不是跳过）。
- [ ] `configClasses` + `rejectPrefixes` + `permissionCommandFields` 覆盖 `DefaultConfig()`
      摊出的每一个叶子路径，`TestEveryLeafKeyIsClassed` 双向绿（缺归档、留旧路径都报错）。
- [ ] `Diff` 的表驱动用例逐条通过，含"同时含热更与拒绝"那一条。
- [ ] `ReloadState` 与五个结论常量在 `core` 里定义，`api`/`cmd` 尚未引用也能编译通过
      （本卡不接线）。
- [ ] 关掉 `reload` 与不写本节，`DefaultConfig()` 的其余取值一字不变
      （一条断言守住"默认关闭"）。
- [ ] 本卡没有引入任何监听、没有改任何子系统、`cmd/server` 零改动：
      `go test ./... -race` 与改动前同样绿。
- [ ] `go build ./...`、`go vet ./...` 绿；新增文件已 `gofmt -w`。

## 7. 验收方式

```bash
go test ./core -run 'TestEveryLeafKeyIsClassed|TestDiff|TestLoadConfig_Reload|TestValidate_Reload|TestExampleConfigMatchesLocal' -v
go build ./... && go vet ./...
go test ./... -race -count=1
```

预期：全部 `ok`；`TestExampleConfigMatchesLocal` 显示 `--- PASS`（若显示 `SKIP`
要在实现记录里写明本机缺 `config.yaml`）。

## 8. 不在本任务范围

- 不建监听器、不 import `fsnotify`（R05）。
- 不动 `core/logging.go`、`core/scheduler.go`、`core/store.go`、`store/sqlite/*`、`executor/*`（R02–R04）。
- 不碰 `cmd/server`、不碰 `api`（R06）。
- 不改前端。
- 不把 `configKeys`（`core/config_test.go:437`）改成通用工具：它服务的是"比较两份 YAML 的键名"，
  与本卡的"比较两份内存配置的取值"是两件事。

## 9. 风险与回滚

| 风险 | 说明 | 处置 |
| --- | --- | --- |
| 摊平漏掉某类字段 | 未摊出来的键等于没有分类，将来永远不会被重载 | §5.1 的反向查表 + `TestEveryLeafKeyIsClassed` 用带 http 档位的样本 |
| 档位字段名当普通路径处理 | `executors.commands.foo.runtime` 里 `foo` 含点会打乱 `strings.Cut` 的判定 | 档位名由 `core.ValidateProfileName` 限定为 `[A-Za-z0-9_-]{1,64}`（`core/executor_profile_store.go`），不含点；用例里加一条含连字符的名字 |
| `Validate` 与 `Normalized` 对 `debounce` 的口径打架 | `Normalized` 把 0 补成 500ms，`Validate` 拒的是"显式写 10ms"；顺序是 `LoadConfig` 里 Validate 在前（`core/config.go:811`），所以 0 会先被 Validate 放过再由 Normalized 补齐 | §3.1 第 4 条明确写"0 合法（表示用默认）"，用例覆盖 0 与 10ms 两种 |
| 把凭据误归档成 `ClassRestart` | 改了凭据静默不生效，正是设计文档要消灭的行为 | 凭据走前缀表而不是精确路径，`server.auth.users[0].password_bcrypt` 一类摊出的子路径也被拒；用例覆盖 |
| 依赖方向红线 | 借 `executor.LoadProfiles` 做投影最省事，但 `core` 不许 import `executor` | 待拍板 P2 已定：只用 `core` 侧字段清单，R04 在 executor 侧再挡一层 |

回滚：本卡是纯新增（两个键 + 一个新文件 + 两处 YAML 行），退回只需删新文件、
还原 `core/config.go` 的字段/默认值/`Normalized`/`Validate`/`BindEnv` 与两份 YAML，
没有任何调用点会断。

## 10. 实现记录（执行时补写）

### 10.1 落地的接口

### 10.2 与本卡写法的差异

### 10.3 验证证据

### 10.4 手工验收

### 10.5 缺陷

### 10.6 未覆盖项
