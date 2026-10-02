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
4. `Validate()` 加两条：`debounce < 0` 一律拒绝；`0 < debounce < minReloadDebounce` 也一律拒绝
   （不挂在 `Enabled` 上，实现记录 §10.2 第 1 条记了这个方向）。文案照 `core/config.go:848-856`
   的既有体例（说清合法取值范围与实际读到的值）。注意方向与 `executors.*` 那组不同：
   `debounce` 在关闭状态下也没有"合法的过小写法"，因为它会被 `Normalized` 补成默认值，
   所以负数与过小一律拒。`debounce == 0` 是唯一合法的"用默认值"写法，必须放过。
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
//     因为"改了哪一条的哪个字段"必须能被分类；列表为空（含 nil）时容器路径本身
//     作为一个叶子出现，承载"条目增删"这件事；
//   - 其余切片、映射与标量（含 time.Duration 这类命名标量，Kind 是 Int64 不是 Struct）
//     都是叶子，值用 reflect.DeepEqual 比；
//   - 指针也当一个叶子（leafScalar）：`deny_private_ranges *bool` 与 `positional
//     *ExecutorPositional` 都不递归进指针背后。DeepEqual 顺着指针比内容，
//     所以 nil 与"指向 false"、指针背后的取值改动都能比出差别；positional 整组
//     本来就在热更档（§6.4），拆成 .max/.pattern 不带来任何判据差别。
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
// 这些字段变了 → 整次作废。
// 清单必须显式列字段，且与 hotCommandFields 合起来恰好覆盖 ExecutorCommand 摊出的每一个
// 字段名：将来给 ExecutorCommand 加字段时，新字段两边都不在 → classify 回 ok=false，
// 由 §5.1 的守卫用例逼着人显式回答"它落在哪一侧"，而不是静默落进热更档。
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

// hotCommandFields 是档位内允许热更的取值型字段（设计文档 §6.4 第一条），与
// permissionCommandFields 显式对偶。两份清单合起来恰好覆盖 ExecutorCommand 的每一个字段名，
// 命中的叶子归 ClassHot。kind 与 body 也归这一侧（它们不改变"能执行什么"的身份/目标/凭据边界）。
var hotCommandFields = map[string]bool{
        "name": true, "kind": true, "timeout": true, "max_parallel": true,
        "retry_on_exit": true, "args": true, "args_render": true, "positional": true,
        "body": true, "expect_status": true, "capture_response": true, "max_body_bytes": true,
}
```

分类判定函数（`Diff` 与守卫用例共用一份，避免两套规则）：

```go
// classify 返回一个叶子路径的档位。ok 为 false 表示既没有精确命中也没有前缀命中，
// 属于"新增键忘了归档"，调用方（守卫用例）据此报错。
func classify(path string) (class ConfigClass, ok bool)
```

判定顺序：精确路径 → `rejectPrefixes` 前缀 → `executors.commands.<name|.#i>.<field>`
形式的字段名，**三态**：命中 `permissionCommandFields` 则 `ClassReject`，命中 `hotCommandFields`
则 `ClassHot`，两边都不在则 `ok=false`（与"未知顶层键未归档"同一脸色，让守卫去红）。
最后一条要在 `classify` 里用 `strings.Cut` 拆出字段名。

三条落地口径（实现时补进卡，避免后来人按字面理解）：

- 前缀命中**包含"路径与前缀相等"**这一种。`server.auth.users` 不是按元素摊开的列表
  （只有 `executors.commands` 按元素摊），整份账号列表摊出来就是这一个叶子；
  风险表 §9 第四条写的 `server.auth.users[0].password_bcrypt` 那种子路径现在并不存在，
  但等值匹配 + 子路径匹配两种形状都在拒绝档，将来真按元素摊开也不会漏（§10.2 第 3 条）。
- 未归档时返回的档位取 `ClassRestart` 而不是零值：忽略 `ok` 的调用方最坏只是
  "改了没生效但进 ignored_keys"，不会把未归档的键变成免重启通道。
- **条目增删的判据不在 `classify` 里**（它只看路径形状），在 `Diff` 的 `classifyChange`
  里按"这条档位是否只在一侧出现"判，见 §3.4 第 4 条。

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

四条行为要求：

1. 无变化的键不进任何清单。
2. `Reject` 非空时，调用方整次作废；`Diff` 本身仍然把 `Hot` 填好，方便日志说清
   "本来会应用哪些"，但不代表它们被应用了。
3. 切片从 `nil` 变成一条元素、从一条变成空切片：算改动（`leafValue.Kind` + `reflect.DeepEqual`
   能区分，`Kind` 相同再比 `Val`）。
4. 只在一侧出现的路径算改动，`Old`/`New` 的另一侧是 `nil`。其中**档位条目的增删**
   必须整体按 `ClassHot` 判：新增一条 script 档位会带来 `executors.commands.<新名>.runtime`
   与 `.script` 这类"只在一侧出现"的字段叶子，按 §3.3 的纯路径判据它们会命中拒绝档，
   于是 §5.3 表里"增一条 script → `Hot` + `Commands`"那行永远过不了。落地是在 `Diff` 内部
   多加一条判据（`classifyChange(path, oldEntries, newEntries)`）：路径所属的条目名
   不是两侧都有 → 这次改动是增删带来的 → `ClassHot` 并进 `Commands`；两侧都有该条目 →
   回到 §3.3 的字段判据。`classify` 本身不变，守卫用例仍按纯路径判。
   依据是设计文档 §6.4 把"条目的增删"明列在允许热更的一侧。

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
        // 两份清单必须恰好覆盖摊出的每一个档位字段名，多一个少一个都报错：
        //   少——ExecutorCommand 新增字段却没进任一清单（未归档，静默落热更档），本次新增的守卫；
        //   多——清单列着结构体没有的字段名（笔误/字段被删）。
        commandFields := commandFieldNames(leaves)
        for field := range commandFields {
                if !permissionCommandFields[field] && !hotCommandFields[field] {
                        t.Errorf("executors.commands field %q is on neither permissionCommandFields nor hotCommandFields; archive it explicitly", field)
                }
        }
        for field := range permissionCommandFields {
                if !commandFields[field] {
                        t.Errorf("permissionCommandFields lists %q but no executors.commands.* leaf carries it", field)
                }
        }
        for field := range hotCommandFields {
                if !commandFields[field] {
                        t.Errorf("hotCommandFields lists %q but no executors.commands.* leaf carries it", field)
                }
        }
}
```

`isCommandOrCredentialPrefix(path)` 认两类合法条目：`executors.commands`（条目增删本身）与
`rejectPrefixes` 里那三条前缀（凭据的子路径按元素摊开，精确路径本来就不在 `configClasses` 里）。
`commandFieldNames(leaves)` 收集摊出的 `executors.commands.<name>.<field>` 里出现过的字段名集合，
是"两份清单恰好覆盖档位字段"这条守卫的基础。两个辅助函数都放在测试文件里，不外溢。

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
| `executors.commands` 减一条（收缩能力） | `Hot` + `Commands` |
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

落地时另加六条同族用例（都守本卡已写明的判据，不是新行为）：
`TestDiffReportsOldAndNewValues`（`ChangedKey` 带两侧原值）、
`TestDiffListsKeysSorted`（四份清单按路径字典序）、
`TestClassifyUnknownPathIsNotSilent`（`classify` 的 `ok=false` 分支 + 十个代表路径的归档）、
`TestClassifyUnarchivedCommandFieldIsNotSilent`（档位新增未归档字段 → `classify` 回 `ok=false`，
证明"新字段必须显式归档"这条守卫存在）、
`TestFlattenLeavesKinds`（`time.Duration` 当叶子、列表/映射的 `Kind`、空列表以容器路径出现、
`*bool` 与 `map` 不递归）、`TestFlattenLeavesNamelessCommand`（无名档位的 `#<i>` 兜底路径）。

### 5.4 手工

临时目录里造一份含 `reload: {enabled: true, debounce: 10ms}` 的配置，
`go run ./cmd/server -config=...` 应**启动失败**并给出 `reload.debounce` 的错误文案。
这是"新键真的进了结构体"的最硬证据（本卡还没有监听器，所以除此之外不该有任何现象）。

## 6. 完成标准（DoD）

- [x] `reload.enabled` / `reload.debounce` 能读、能归一化、能被环境变量覆盖，
      `TestExampleConfigMatchesLocal` 通过（不是跳过）。
- [x] `configClasses` + `rejectPrefixes` + `permissionCommandFields` + `hotCommandFields` 覆盖
      `DefaultConfig()` 摊出的每一个叶子路径，`TestEveryLeafKeyIsClassed` 双向绿（缺归档、留旧路径、
      档位字段两份清单没恰好覆盖都报错）。
- [x] `Diff` 的表驱动用例逐条通过，含"同时含热更与拒绝"那一条。
- [x] `ReloadState` 与五个结论常量在 `core` 里定义，`api`/`cmd` 尚未引用也能编译通过
      （本卡不接线）。
- [x] 关掉 `reload` 与不写本节，`DefaultConfig()` 的其余取值一字不变
      （一条断言守住"默认关闭"）。
- [x] 本卡没有引入任何监听、没有改任何子系统、`cmd/server` 零改动：
      `go test ./... -race` 与改动前同样绿。
- [x] `go build ./...`、`go vet ./...` 绿；新增文件已 `gofmt -w`。

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

落地日期 2026-10-02，基线提交 `4abdadd`。本节里的行号是**落地后**的位置（卡正文的行号是动笔基线，
落地后整体下移，见 §10.2 第 8 条）。

### 10.1 落地的接口

`core/config.go`（§3.1 的类型与五处配套）：

| 符号 | 位置 |
| --- | --- |
| `Config.Reload ReloadConfig` | `core/config.go:36`（跟在 `Observability` 之后） |
| `DefaultReloadDebounce = 500 * time.Millisecond` | `core/config.go:184` |
| `minReloadDebounce = 50 * time.Millisecond` | `core/config.go:187` |
| `type ReloadConfig struct{ Enabled bool; Debounce time.Duration }` | `core/config.go:189-197` |
| `DefaultConfig()` 的 `Reload: ReloadConfig{Enabled: false, Debounce: DefaultReloadDebounce}` | `core/config.go:729-734` |
| `LoadConfig` 的 `BindEnv` 列表末尾 `"reload.enabled"`、`"reload.debounce"` | `core/config.go:821-822` |
| `Config.Validate()` 的两条 debounce 判据 | `core/config.go:916-922` |
| `Config.Normalized()` 的 `Debounce == 0 → 默认值` | `core/config.go:1063-1065` |

`core/config_reload.go`（新增，478 行；小节顺序按 §4.3 的依赖顺序排）：

| 符号 | 位置 |
| --- | --- |
| `const commandsPath = "executors.commands"` | `:20` |
| `type ConfigClass int` + `ClassHot`/`ClassRestart`/`ClassReject` | `:23-32` |
| `func (c ConfigClass) String() string`（hot/restart/reject，未知给 `ConfigClass(n)`） | `:35-46` |
| `var configClasses map[string]ConfigClass`（**50 条**：热更 11、重启 39） | `:54-97` |
| `var rejectPrefixes []string`（3 条凭据前缀） | `:103-107` |
| `var permissionCommandFields map[string]bool`（14 个身份/目标/凭据字段） | `:118-136` |
| `var hotCommandFields map[string]bool`（12 个允许热更的档位字段，D-R0104 补） | 本次新增，位置见改动后的 `core/config_reload.go` |
| `func classify(path string) (ConfigClass, bool)` | `:144-162` |
| `type leafKind int` + `leafScalar`/`leafSlice`/`leafMap` | `:166-172` |
| `type leafValue struct{ Kind; Path; Val }` + `func (leafValue) any() any` | `:175-187` |
| `func flattenLeaves(cfg Config) map[string]leafValue` | `:202-206` |
| 摊平内部：`flattenStruct`（`:209`）、`flattenCommands`（`:232`）、`isCommandList`（`:245`）、`commandEntryName`（`:251`）、`addLeaf`（`:263`）、`kindOfLeaf`（`:267`）、`leafKey`（`:280`）、`joinLeafPath`（`:288`） | — |
| `func splitCommandLeaf(path) (name, field string, ok bool)` | `:300-314` |
| `func isCommandPath(path string) bool` | `:317-319` |
| `type ChangedKey struct{ Path string; Old, New any }` | `:323-327` |
| `type ConfigChange struct{ Hot, Restart, Reject, Commands []ChangedKey }` | `:330-335` |
| `func (ConfigChange) HasChanges() bool` / `HasRejections() bool` | `:338-345` |
| `func Diff(applied, candidate Config) ConfigChange` | `:355-386` |
| `func classifyChange(path string, oldEntries, newEntries map[string]bool) ConfigClass` | `:390-402` |
| `func commandEntryNames` / `sortedLeafPaths` / `sameLeaf` | `:405` / `:416` / `:434` |
| `type ReloadResult string` + `ReloadOK`/`ReloadUnchanged`/`ReloadRejected`/`ReloadFailed`/`ReloadDegraded` | `:445-458` |
| `type ReloadState struct`（十个字段，JSON 标签照 §3.5） | `:467-478` |

摊平的实际规模：`flattenLeaves(DefaultConfig())` 给出 **53** 个叶子；
§5.1 的样本（带一条 script + 一条 http）给出 **104** 个叶子（每条档位摊出 26 个字段叶子）。
`core/config.go` 与 `core/config_test.go` 的既有符号一个没改（`configKeys`、
`TestExampleConfigMatchesLocal` 原样），`cmd/server`、`api`、`executor`、`store/sqlite`、
`core/logging.go`、`core/scheduler.go`、`core/store.go` 零改动。

### 10.2 与本卡写法的差异

1. **§3.1 第 4 条前后矛盾，已就地改写卡面**。前半句写"`enabled=true` 时 `debounce < min` 拒绝"，
   后半句（"注意"段）与 §5.2 的第三条用例写"`enabled: false` + `debounce: 10ms` 也被拒"。
   落地按后者：负数一律拒、`0 < debounce < 50ms` 一律拒、`debounce == 0` 放过（表示用默认值，
   由 `Normalized` 补齐）。卡面那句"在关闭状态下也没有'合法的 0 值写法'"读起来像"0 也被拒"，
   与 §9 风险表第三行"0 合法（表示用默认）"打架，所以改成"`debounce == 0` 是唯一合法的
   '用默认值'写法，必须放过"。
2. **指针取值的摊法卡里没写**（§3.2 只给了 `leafScalar|leafSlice|leafMap` 三种 Kind 和"标量/切片/映射"）。
   落地：指针当一个 `leafScalar` 叶子，**不递归进指针背后**。理由是 `DeepEqual` 顺着指针比内容，
   所以 `nil` 与"指向 false"、指针背后的取值改动都比得出差别；`positional *ExecutorPositional`
   整组本来就在热更档（设计文档 §6.4），拆成 `.max`/`.pattern` 不带来任何判据差别，
   代价只是 R06 的日志会说"`...positional` 变了"而不是"`max` 变了"。卡面 §3.2 已补这条规则。
3. **§3.2 与 §9 风险表第四条的 `server.auth.users[0].password_bcrypt` 那种子路径现在不存在**。
   只有 `executors.commands` 按元素摊开，`server.auth.users` 整份账号列表摊出来是一个叶子
   `server.auth.users`。落地：前缀命中判据写成"等值或带点前缀"两种形状都算命中
   （`path == prefix` 或 `strings.HasPrefix(path, prefix+".")`），
   等值与子路径两种形状都落拒绝档；守卫用例的 `TestClassifyUnknownPathIsNotSilent` 里带一条
   `server.auth.users.#0.password_bcrypt → ClassReject` 的断言，将来真按元素摊开也不会漏。
   卡面 §3.3 已补这条口径。
4. **§3.4 少写了一条判据，落地加了 `classifyChange`**（重要）。按 §3.3 的 `classify` 顺序，
   新增一条 script 档位会摊出 `executors.commands.<新名>.runtime` 与 `.script`，
   这两个字段名命中 `permissionCommandFields` → 拒绝档，于是 §5.3 表里
   "增一条 script → `Hot` + `Commands`"那一行按纯路径判据永远过不了。落地在 `Diff` 内部加一层：
   路径所属的档位条目名不是两侧都出现 → 这次改动是条目增删带来的 → `ClassHot` 并进 `Commands`；
   两侧都有该条目才回到字段判据。依据是设计文档 §6.4 把"条目的增删"明列在允许热更一侧。
   `classify` 本身没动，守卫用例仍按纯路径判。卡面 §3.4 已从"三条行为要求"改成四条并写清这条。
5. **`classify` 未命中时返回的档位卡里没规定**（只规定 `ok=false`）。落地返回 `ClassRestart`
   而不是零值 `ClassHot`：忽略 `ok` 的调用方最坏是"改了没生效但进 `ignored_keys`"，
   不会把未归档的键变成免重启通道。`Diff` 侧同一条兜底写在 `classifyChange` 的注释里。
6. **`isCommandList` 按元素类型判定而不是按路径字符串写死**：`value.Kind() == Slice &&
   Elem() == reflect.TypeOf(ExecutorCommand{})`。卡 §3.2 说的是"`executors.commands` 的切片"，
   两种写法现在等价；按类型写是为了将来再加同类型列表时摊平与分类不会各写一遍。
7. **`Commands` 子集的收录判据含容器路径本身**：`path == "executors.commands" ||
   HasPrefix(path, "executors.commands.")`。空档位列表以容器路径出现（§3.2 落地补充），
   "从没有档位到有一条档位"这件事的改动键就是它，必须进 `Commands` 供 R04/R06 判定。
8. **卡正文的行号是动笔基线（`4abdadd`）的位置**，落地后 `core/config.go` 整体下移
   （`LoadConfig` 717→741、`Validate` 的调用点 811→840、`Normalized` 907→948、
   `configKeys` 437→438）。现行位置见 §10.1，卡面原文按 W04 的先例不逐处改行号。
9. **两份 YAML 多加一条注释**：卡 §4.2 要求的四条（默认关闭 / 关闭时一个监听器都不建 /
   `enabled` 改了要重启 / `debounce` 可以热更）都在，另加一条"监听器与重载链的接线在 R05/R06 落地，
   R07 收口之前打开本节不会有行为变化"。理由是"打开了却什么现象都没有"正是本系列要消灭的静默，
   现阶段读模板的人无从判断。**R07 要把这条改成本节的正式行为描述**（见 §10.6）。
10. **测试用例名与额外五条**：§5.3 的表驱动用例落地叫 `TestDiffClassifiesChangedKeys`
    （验收命令的 `TestDiff` 前缀命中）；另加 `TestDiffReportsOldAndNewValues`、
    `TestDiffListsKeysSorted`、`TestClassifyUnknownPathIsNotSilent`、`TestFlattenLeavesKinds`、
    `TestFlattenLeavesNamelessCommand` 五条，守的都是卡里已写明的判据（`ChangedKey` 原值、
    清单排序、`classify` 的 `ok`、命名标量当叶子 / 三种 `Kind` / 空列表的容器路径、空名兜底），
    没有引入新行为。§5.3 表里还补了一行"减一条档位 → `Hot` + `Commands`"。
11. **`Diff` 不实现"入参必须已 `Normalized`"的断言**：卡 §3.4 说"签名不返回 error 但要断言"。
    结构体里 0 值与"没写这一项"不可区分，core 内部无法自检，落地只在函数注释里写明前提，
    并把"调用方必须传 `Normalized()` 结果"这条留给 R06 的重载链（用例里靠
    `Diff(applied.Normalized(), candidate.Normalized())` 与"默认值 vs 归一化默认值无改动"那一行守）。
    登记为 D-R0102。
12. **卡 §3.3 承诺的"新字段必须显式归档"守卫在首版实现里不存在，本次补上**（登记为 D-R0104）。
    首版 `classify` 的档位字段分支是两态：命中 `permissionCommandFields` 归拒绝档，否则一律归热更档。
    后果是将来给 `ExecutorCommand` 加字段时，新字段会**静默落进热更档**、守卫正向放行，
    反向只遍历已列的 14 条拒绝字段，§5.1 里唯一的规模断言是 `len(leaves) < 60` 这个下限——
    "逼着人显式回答'它落在哪一侧'"这句话因此不成立。修法：新增显式的 `hotCommandFields`
    （设计 §6.4 第一条那十项，再加 `kind`、`body`——它们不改变身份/目标/凭据边界，与既有实现
    把它们当热更的方向一致，共 12 项），`classify` 的档位字段分支改三态
    （拒绝清单命中→`ClassReject`、热更清单命中→`ClassHot`、两边都不在→`ok=false`），
    `TestEveryLeafKeyIsClassed` 加一条 `permissionCommandFields ∪ hotCommandFields` **恰好等于**
    摊出的档位字段名集合的双向断言，另加 `TestClassifyUnarchivedCommandFieldIsNotSilent`
    用直接调 `classify("executors.commands.cfg-script-one.__not_archived__")` 证明这条守卫会红。
    卡面 §3.3、§5.1、§6 的文字与设计 §6.4 末段同步改为"新字段必须显式归档，否则守卫红"。

### 10.3 验证证据

先红后绿的证据（TDD）：

```
$ go test ./core -run 'TestEveryLeafKeyIsClassed|TestDiff|TestLoadConfig_Reload|TestValidate_Reload|TestExampleConfigMatchesLocal' -v
# godelayq/core [godelayq/core.test]
core\config_reload_test.go:49:12: undefined: flattenLeaves
core\config_reload_test.go:56:15: undefined: classify
core\config_reload_test.go:62:20: undefined: configClasses
core\config_reload_test.go:69:21: undefined: permissionCommandFields
core\config_reload_test.go:92:25: undefined: rejectPrefixes
core\config_reload_test.go:104:40: undefined: leafValue
core\config_reload_test.go:293:14: undefined: Diff
core\config_reload_test.go:537:26: undefined: ChangedKey
FAIL	godelayq/core [build failed]

# 只加 ReloadConfig 类型与 Config.Reload 字段、其余四处配套未做时（§5.2 三条用例各自按预期红）：
--- FAIL: TestLoadConfig_ReloadDefaults (0.00s)
    config_test.go:822: Not equal: expected: 500ms  actual: 0s        # DefaultConfig 没补 Debounce
    config_test.go:838: Not equal: expected: 500ms  actual: 0s        # Normalized 没补
--- FAIL: TestLoadConfig_ReloadEnvOverrides (0.00s)
    config_test.go:866: Should be true  Messages: GODELAYQ_RELOAD_ENABLED 必须真能覆盖   # BindEnv 漏项
    config_test.go:867: Not equal: expected: 2s  actual: 500ms
--- FAIL: TestValidate_Reload (0.00s)
    config_test.go:876: An error is expected but got nil.                                 # Validate 没两条
```

验收命令 1（`-v`，30 条 PASS/子用例全过、0 条 FAIL）：

```
--- PASS: TestEveryLeafKeyIsClassed (0.00s)
--- PASS: TestDiffClassifiesChangedKeys (0.00s)          # 21 个子用例逐条 PASS
--- PASS: TestDiffReportsOldAndNewValues (0.00s)
--- PASS: TestDiffHasRejections (0.00s)
--- PASS: TestDiffListsKeysSorted (0.00s)
--- PASS: TestExampleConfigMatchesLocal (0.00s)          # 是 PASS 不是 SKIP
--- PASS: TestLoadConfig_ReloadDefaults (0.00s)
--- PASS: TestLoadConfig_ReloadEnvOverrides (0.00s)
--- PASS: TestValidate_Reload (0.01s)
PASS
ok  	godelayq/core	0.205s
```

守卫用例的反向验证（三处变异，改完即恢复）：

```
# 变异一：从 configClasses 删掉 "store.groups_path": ClassRestart（正向：漏归档）
--- FAIL: TestEveryLeafKeyIsClassed (0.00s)
    config_reload_test.go:57: leaf key "store.groups_path" has no reload class; add it to configClasses

# 变异二：表里留一条结构体摊不出来的旧路径 + 把 permissionCommandFields 的 script 写成 scrript（反向）
--- FAIL: TestEveryLeafKeyIsClassed (0.00s)
    config_reload_test.go:64: configClasses has "logging.formatt" but DefaultConfig does not produce it
    config_reload_test.go:71: permissionCommandFields lists "scrript" but no executors.commands.* leaf carries it

# 恢复后：ok  godelayq/core  0.197s
```

验收命令 2：`go build ./... && go vet ./...` 无输出（通过）。

验收命令 3（全量，`api` 包慢，实跑约 4.5 分钟）：

```
ok  	godelayq/api	225.298s
?   	godelayq/cmd/gensecret	[no test files]
?   	godelayq/cmd/hashpassword	[no test files]
ok  	godelayq/cmd/server	5.826s
ok  	godelayq/core	12.733s
?   	godelayq/examples/demo1	[no test files]
?   	godelayq/examples/demo2	[no test files]
ok  	godelayq/executor	24.679s
ok  	godelayq/store/sqlite	3.635s
?   	godelayq/web	[no test files]
```

（同一份代码重复跑过两轮全量，第二轮即上面这份；`go build ./...` 与 `go vet ./...` 在那两轮里
都无输出。）

`core` 包单跑（不带 `-race`，`-v`）：`346` 条 `--- PASS`、`0` 条 FAIL——本卡新增 11 条顶层用例
（`core` 里 8 条 + `core/config_test.go` 里 3 条），既有用例没有改预期、没有删。
新增文件已显式 `gofmt -w`（不按 `gofmt -l` 判断，本仓 CRLF 会全量误报）；
落地后核对过四个 Go 文件与两份 YAML 的行尾仍是全 CRLF（1068/1068、906/906、478/478、617/617），
没有引入混和行尾。

**D-R0104 复核补记**（本次修复的实测，2026-10-02，基线提交 `41691f9`）：

先红后绿：新增 `TestClassifyUnarchivedCommandFieldIsNotSilent` 断言
`classify("executors.commands.cfg-script-one.__not_archived__")` 回 `ok=false`，
在两态实现下先 FAIL（实测 `actual: 0`（ClassHot）、`Should be false`），加 `hotCommandFields`
+ 三态 `classify` 后 PASS。

守卫用例的变异反向验证（三处，验完即恢复，两个 Go 文件行尾仍是全 CRLF 507/507、646/646）：

```
# 变异一：从 hotCommandFields 删掉 "timeout"（模拟新增字段没归档）→ 正向守卫红
--- FAIL: TestEveryLeafKeyIsClassed (0.00s)
    config_reload_test.go:57: leaf key "executors.commands.cfg-script-one.timeout" has no reload class; add it to configClasses
    config_reload_test.go:75: executors.commands field "timeout" is on neither permissionCommandFields nor hotCommandFields; archive it explicitly
    config_reload_test.go:57: leaf key "executors.commands.cfg-http-one.timeout" has no reload class; add it to configClasses
FAIL	godelayq/core

# 变异二：往 hotCommandFields 塞一个 "not_a_real_field"（清单里写着不存在的字段名）→ 反向守卫红
--- FAIL: TestEveryLeafKeyIsClassed (0.00s)
    config_reload_test.go:85: hotCommandFields lists "not_a_real_field" but no executors.commands.* leaf carries it
FAIL	godelayq/core

# 恢复后：ok  godelayq/core
```

验收命令 1（`-v`）：`TestEveryLeafKeyIsClassed`、`TestClassifyUnarchivedCommandFieldIsNotSilent`、
`TestDiffClassifiesChangedKeys`（21 子用例）、`TestDiffReportsOldAndNewValues`、`TestDiffHasRejections`、
`TestDiffListsKeysSorted`、`TestClassifyUnknownPathIsNotSilent`、`TestExampleConfigMatchesLocal`、
`TestLoadConfig_Reload*`（2）、`TestValidate_Reload` 全部 PASS、0 FAIL。

验收命令 2：`go build ./... && go vet ./...` 无输出（通过）。

`-race -count=5`：`ok  godelayq/core  57.690s`。

全量 `go test ./... -race -count=1`：`api` 221.5s、`cmd/server` 5.9s、`core` 13.1s、
`executor` 24.5s、`store/sqlite` 3.7s 全 `ok`，其余 `[no test files]`。

### 10.4 手工验收

§5.4 的启动失败证据（`%TEMP%` 下的 `r01-smoke-*` 目录，跑完已删）：

```
$ printf '...reload:\n  enabled: true\n  debounce: 10ms\n' > $SMOKEDIR/config.yaml
$ go run ./cmd/server -config=$SMOKEDIR/config.yaml
2026-10-02 17:43:45 load config failed: reload.debounce 10ms is too small, a merge window
  below 50ms means re-reading the config file several times per save (omit the key to use 500ms)
exit status 1
```

冒烟目录里除 `config.yaml` 外没有新建任何文件（`LoadConfig` 在 `cmd/server/main.go:318` 是第一步，
失败即 `log.Fatalf`，存储与日志器都还没建）——"本卡除此之外不该有任何现象"得到证明。

反向（合法取值必须能正常启动，且没有任何重载相关现象）：把同一份冒烟配置改成
`reload: {enabled: true, debounce: 500ms}`，`server.exe -config=...`（直接跑构建产物，
不用 `go run` 以免留下子进程）起来并打印既有的路由表与
`msg="http api server listening" addr=[::]:18099`，日志里没有任何 reload 相关内容
（没有监听器可建），随后按 PID `taskkill` 掉，只留下仓库原有的那个开发进程。

正向读取的补充证据在单元测试里：`TestLoadConfig_ReloadEnvOverrides` 用
`GODELAYQ_RELOAD_ENABLED=true` + `GODELAYQ_RELOAD_DEBOUNCE=2s` 真覆盖到结构体，
`TestLoadConfig_ReloadDefaults` 断言"文件里不写本节"与"写 `enabled: false`"两种形态
解出来的整份 `Config` 与 `DefaultConfig()` 完全相等。

### 10.5 缺陷

| 编号 | 内容 | 处置 |
| --- | --- | --- |
| **D-R0101** | `ReloadState` 的 `LastAttemptAt`/`LastAppliedAt` 是 `time.Time` 却带 `json:",omitempty"`，而 `omitempty` 对结构体无效：`result=rejected` 这类"从没成功应用过"的状态会把 `last_applied_at` 序列化成 `"0001-01-01T00:00:00Z"`，读端点给出的是一个假时间 | **登记不修**：卡 §3.5 与设计文档 §5.4 都按这个形状写，本卡没有任何读取方（改字段类型没有验证面，也没有调用点能证明它更对）。归 **R06**：接 `/admin/runtime` 时改成 `*time.Time` 或在 API 侧格式化，并在那张卡的用例里断言"没应用过就没有这个键" |
| **D-R0102** | `Diff` 的"入参必须已 `Normalized()`"前提无法在 `core` 内部自检（结构体里 0 值与"没写这一项"不可区分），落地只有函数注释与测试约定 | **登记不修**：无法实现断言，硬做只能是 `panic` 级别的新口径。归 **R06**：重载链里 `candidate` 必须由 `LoadConfig(...).Normalized()` 单点产出，`applied` 必须是上一次换入的归一化结果；R06 要有一条用例证明"未归一化的 candidate 会被 Diff 报出假改动"这一方向不再可能发生 |
| **D-R0103** | 无名档位靠 `#<i>` 索引兜底，重名档位则后者覆盖前者（`addLeaf` 同路径写入）：这两种形状的"按名字配对"不成立，改了名字会让整条档位的字段全部报成改动 | **登记不修**：`core.Validate` 不校验档位字段是既有口径（组合规则在 `executor.LoadProfiles`，任一条不过即启动失败），所以 `applied` 里永远是有名字且唯一的档位；无名/重名只可能出现在"executor 侧还没拒但 R06 已经比过一次"的中间态。归 **R04**：档位整表替换时按 `LoadProfiles` 的结论判，`Diff` 的这份形状只作日志与拒绝档判据 |
| **D-R0104** | 卡 §3.3 与设计 §6.4 承诺的"给 `ExecutorCommand` 加字段时新字段必须显式归档"守卫在首版实现里不存在：`classify` 的档位字段分支是两态（命中拒绝清单→拒绝，否则一律热更），新字段会静默落进热更档、守卫正向放行、反向只遍历已列的 14 条拒绝字段 | **本卡已修**：新增显式 `hotCommandFields` 清单（设计 §6.4 第一条十项 + `kind`/`body`，共 12 项），`classify` 改三态（两边都不在→`ok=false`），`TestEveryLeafKeyIsClassed` 加"两份清单恰好覆盖档位字段"的双向断言，另加 `TestClassifyUnarchivedCommandFieldIsNotSilent` 直接证明未归档字段会让 `classify` 回 `ok=false`。见 §10.2 第 12 条 |

### 10.6 未覆盖项

- **没有任何调用方**：`Diff`/`classify`/`flattenLeaves`/`ReloadState` 除测试外零引用，
  端到端"改文件触发一次重载"要等 R05（监听器）+ R06（重载链）。本卡的"行为零变化"是靠
  全量 `-race` 与冒烟日志里"没有任何 reload 现象"两条证明的，不是靠"应该有变化"证明的。
- **平台**：本机 Windows 实跑。Linux/macOS 未实跑（本卡没有文件监听、没有 syscall，
  理论上是纯内存计算，但按系列口径仍归 R07 的交叉构建与场景实测收口）。
- **两份 YAML 的注释文本没有守卫**：`TestExampleConfigMatchesLocal` 只比键名集合，
  不比取值也不比注释。两份 `reload:` 一节的键与注释是逐字人工核对的（本机那份的实际取值
  `enabled: false` / `debounce: 500ms` 与模板一致）。
- **`reload:` 一节的注释里那句"接线在 R05/R06 落地，R07 收口之前打开本节不会有行为变化"是暂时口径**，
  R07 必须改写成本节的正式行为描述（否则等监听器真上线后，这句话会把使用者劝退）。
- **摊平形状没覆盖的写法**：档位字段值为 nil 指针 / nil 映射 / 空切片时，`Diff` 仍会给出这些叶子
  （本卡样本里 `positional`、`env`、`headers` 都有 nil 形状出现过），但"档位重名""档位名为空"
  两种畸形输入只单测了摊出的路径形状（`TestFlattenLeavesNamelessCommand`），
  没测它们在 `Diff` 配对时的表现（见 D-R0103）。
- **`ConfigClass.String()` 的未知分支**没写用例（`fmt.Sprintf("ConfigClass(%d)")` 只在传了
  表外整数时出现，卡 §3.3 只要求三个结论名可读）。
- **`Reject` 非空时 `Hot` 仍然填好**这条只由 `TestDiffHasRejections` 覆盖了一对一的组合
  （`logging.level` + `server.auth.token`）；多热更键 + 多拒绝键的混合场景留给 R06 的重载链用例。
