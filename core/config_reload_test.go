package core

import (
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---- §5.1 守卫用例：三档分类表的完整性 ----

// TestEveryLeafKeyIsClassed 双向钉住分类表：
//   - 正向：Config 摊出来的每一个叶子路径都能被 classify 归档（漏归档 → 该键永远不会被重载，
//     而且没有任何征兆）；
//   - 反向：configClasses 里不许留着结构体摊不出来的旧路径（键改名后那条规则会永久静默），
//     permissionCommandFields 里不许写着档位根本没有的字段名。
//
// 样本必须同时含一条 script 与一条 http，否则 http 那一组字段（allowed_hosts、
// deny_private_ranges 之类）摊不出来，拒绝档的守卫等于没守。
func TestEveryLeafKeyIsClassed(t *testing.T) {
	sample := DefaultConfig()
	deny := false
	sample.Executors.Commands = []ExecutorCommand{
		{
			Name:       "cfg-script-one",
			Kind:       "script",
			Runtime:    "bash",
			Script:     "scripts/one.sh",
			Timeout:    5 * time.Minute,
			Args:       []ExecutorArg{{Name: "day", Required: true}},
			ArgsRender: []string{"--day={day}"},
		},
		{
			Name:            "cfg-http-one",
			Kind:            "http",
			Method:          "POST",
			URLTemplate:     "https://api.example.com/jobs",
			AllowedHosts:    []string{"api.example.com"},
			Headers:         map[string][]string{"Accept": {"application/json"}},
			Body:            "json",
			CaptureResponse: true,
			DenyPrivate:     &deny,
		},
	}

	leaves := flattenLeaves(sample)
	if len(leaves) < 60 {
		t.Fatalf("flattenLeaves gave %d leaves, the walker is probably missing a section", len(leaves))
	}

	// 正向：每个叶子都有档
	for path := range leaves {
		if _, ok := classify(path); !ok {
			t.Errorf("leaf key %q has no reload class; add it to configClasses", path)
		}
	}

	// 反向一：表里的路径必须真能被摊出来（executors.commands 与凭据前缀是合法例外）
	for path := range configClasses {
		if _, ok := leaves[path]; !ok && !isCommandOrCredentialPrefix(path) {
			t.Errorf("configClasses has %q but DefaultConfig does not produce it", path)
		}
	}

	// 反向二：拒绝档字段清单里的名字必须真出现在某个档位叶子里
	for field := range permissionCommandFields {
		if !hasCommandField(leaves, field) {
			t.Errorf("permissionCommandFields lists %q but no executors.commands.* leaf carries it", field)
		}
	}

	// 摊平必须真的把两条档位摊成按名字的子路径，否则上面两条反向检查都在空转
	require.Contains(t, leaves, "executors.commands.cfg-script-one.runtime")
	require.Contains(t, leaves, "executors.commands.cfg-http-one.deny_private_ranges")
	assert.NotContains(t, leaves, "executors.commands", "有条目时档位列表不作为一个容器叶子出现")

	// 带连字符的名字（风险表 §9 第二条）：档位名由 ValidateProfileName 限定为
	// [A-Za-z0-9_-]{1,64}，不含点，所以按名字摊路径不会被 Cut 拆错。
	require.NoError(t, ValidateProfileName("cfg-script-one"))
}

// isCommandOrCredentialPrefix 认两类合法条目：executors.commands（条目增删本身，
// 只在列表为空时以容器路径出现）与 rejectPrefixes 里那三条前缀（凭据的子路径按元素摊开，
// 精确路径本来就不在 configClasses 里）。放在测试文件里，不外溢。
func isCommandOrCredentialPrefix(path string) bool {
	if path == "executors.commands" {
		return true
	}
	for _, prefix := range rejectPrefixes {
		if path == prefix {
			return true
		}
	}
	return false
}

// hasCommandField 在摊出的 executors.commands.<name>.<field> 集合里找指定字段名。
// 它是"permissionCommandFields 里的字段名写错了"这类笔误的守卫。
// 这里刻意用字面量重新解析一遍路径，不复用实现里的拆分函数：实现写错时守卫不会跟着一起错。
// 放在测试文件里，不外溢。
func hasCommandField(leaves map[string]leafValue, field string) bool {
	const prefix = "executors.commands."
	for path := range leaves {
		if !strings.HasPrefix(path, prefix) {
			continue
		}
		if name, f, ok := strings.Cut(strings.TrimPrefix(path, prefix), "."); ok && name != "" && f == field {
			return true
		}
	}
	return false
}

// ---- §5.3 Diff 的分类 ----

// TestFlattenLeavesKinds 钉住摊平的三条口径：命名标量类型（time.Duration）是叶子而不是
// 待展开的结构体；列表与映射各自有 Kind，好让"空列表"与"没有这个键"比出不同结论；
// 指针（*bool、*ExecutorPositional）当一个叶子，nil 与"指向 false"是两次改动。
func TestFlattenLeavesKinds(t *testing.T) {
	leaves := flattenLeaves(DefaultConfig())

	for _, path := range []string{"reload.debounce", "store.history_ttl", "executors.output.ttl", "scheduler.shutdown_timeout"} {
		leaf, ok := leaves[path]
		require.True(t, ok, "命名标量类型 %q 没被摊出来", path)
		assert.Equal(t, leafScalar, leaf.Kind, "%q 是 time.Duration，Kind 必须是标量而不是结构体", path)
	}

	for _, path := range []string{"server.auth.users", "executors.commands", "executors.runtime_allow"} {
		leaf, ok := leaves[path]
		require.True(t, ok, "%q 没被摊出来", path)
		assert.Equal(t, leafSlice, leaf.Kind, "%q 是列表", path)
	}

	// 空档位列表以容器路径出现（有条目时的形状由下面的 commandLeaves 断言）
	require.Contains(t, leaves, "executors.commands")

	// 映射是叶子：Headers 整份比较，不会摊成 headers.<名字>（那是 executor 侧的事，
	// 分类只看"能不能执行什么"）
	withHeaders := DefaultConfig()
	withHeaders.Executors.Commands = []ExecutorCommand{diffHTTPCommand("cfg-http-one")}
	commandLeaves := flattenLeaves(withHeaders)
	assert.Equal(t, leafMap, commandLeaves["executors.commands.cfg-http-one.headers"].Kind)
	assert.Equal(t, leafSlice, commandLeaves["executors.commands.cfg-http-one.args"].Kind)
	assert.Equal(t, leafScalar, commandLeaves["executors.commands.cfg-http-one.deny_private_ranges"].Kind,
		"*bool 也当一个叶子：nil 与指向 false 必须比出不同结论")
	assert.NotContains(t, commandLeaves, "executors.commands.cfg-http-one.headers.Accept")
}

// TestFlattenLeavesNamelessCommand 钉住无名档位的兜底路径：core 不校验档位字段
// （组合规则在 executor.LoadProfiles），摊平不能因为名字为空就产出重复或残缺的路径。
func TestFlattenLeavesNamelessCommand(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Executors.Commands = []ExecutorCommand{
		{Kind: "script", Runtime: "bash", Script: "scripts/a.sh"},
		{Kind: "script", Runtime: "bash", Script: "scripts/b.sh"},
	}

	leaves := flattenLeaves(cfg)
	assert.Contains(t, leaves, "executors.commands.#0.script")
	assert.Contains(t, leaves, "executors.commands.#1.script")
	for _, path := range []string{"executors.commands.#0.script", "executors.commands.#1.script"} {
		class, ok := classify(path)
		assert.True(t, ok, path)
		assert.Equal(t, ClassReject, class, "无名档位的身份字段照样进拒绝档")
	}
}

func diffScriptCommand(name string) ExecutorCommand {
	return ExecutorCommand{
		Name:       name,
		Kind:       "script",
		Runtime:    "bash",
		Script:     "scripts/one.sh",
		Timeout:    5 * time.Minute,
		Args:       []ExecutorArg{{Name: "day", Required: true}},
		ArgsRender: []string{"--day={day}"},
	}
}

func diffHTTPCommand(name string) ExecutorCommand {
	deny := false
	return ExecutorCommand{
		Name:         name,
		Kind:         "http",
		Method:       "POST",
		URLTemplate:  "https://api.example.com/jobs",
		AllowedHosts: []string{"api.example.com"},
		Headers:      map[string][]string{"Accept": {"application/json"}},
		Body:         "json",
		DenyPrivate:  &deny,
	}
}

// TestDiffClassifiesChangedKeys 逐条走 §5.3 的分类表。
// 每行只声明"这几条路径必须落在这一档、且不许落在另外两档"：
// 档位增删一行会摊出几十个叶子，逐条比对期望值会把用例写成第二份实现。
func TestDiffClassifiesChangedKeys(t *testing.T) {
	base := DefaultConfig().Normalized()
	twoCommands := withCommands(base, diffScriptCommand("cfg-script-one"), diffHTTPCommand("cfg-http-one"))

	for _, tc := range []struct {
		name         string
		applied      Config
		candidate    Config
		hot          []string
		restart      []string
		reject       []string
		commands     []string
		wantNoChange bool
	}{
		{
			name:      "logging.level 是热更",
			applied:   base,
			candidate: withLoggingLevel(base, "debug"),
			hot:       []string{"logging.level"},
		},
		{
			name:      "scheduler.workers 是热更",
			applied:   base,
			candidate: withSchedulerWorkers(base, 32),
			hot:       []string{"scheduler.workers"},
		},
		{
			name:      "store.history_ttl 是热更",
			applied:   base,
			candidate: withStoreHistoryTTL(base, 24*time.Hour),
			hot:       []string{"store.history_ttl"},
		},
		{
			name:      "观测层的保留条数是热更",
			applied:   base,
			candidate: withEventRetentionCount(base, 1000),
			hot:       []string{"observability.events.retention_count"},
		},
		{
			name:      "reload.debounce 是热更",
			applied:   base,
			candidate: withReloadDebounce(base, 2*time.Second),
			hot:       []string{"reload.debounce"},
		},
		{
			name:      "reload.enabled 属于重启档：改它只能重启",
			applied:   base,
			candidate: withReloadEnabled(base, true),
			restart:   []string{"reload.enabled"},
		},
		{
			name:      "增一条 script、改另一条的 timeout：条目增删与档位取值都是热更",
			applied:   twoCommands,
			candidate: withAddedAndRetimedCommand(twoCommands),
			hot: []string{
				"executors.commands.cfg-script-one.timeout",
				"executors.commands.cfg-added-one.runtime",
				"executors.commands.cfg-added-one.script",
			},
			commands: []string{
				"executors.commands.cfg-script-one.timeout",
				"executors.commands.cfg-added-one.runtime",
			},
		},
		{
			name:      "从没有档位到有一条档位：算一次真实的增删",
			applied:   base,
			candidate: withCommands(base, diffScriptCommand("cfg-added-one")),
			hot:       []string{"executors.commands", "executors.commands.cfg-added-one.kind"},
			commands:  []string{"executors.commands", "executors.commands.cfg-added-one.kind"},
		},
		{
			name:      "减一条档位同样是热更（收缩能力不需要拒绝）",
			applied:   twoCommands,
			candidate: withCommands(twoCommands, diffScriptCommand("cfg-script-one")),
			hot:       []string{"executors.commands.cfg-http-one.url_template", "executors.commands.cfg-http-one.allowed_hosts"},
			commands:  []string{"executors.commands.cfg-http-one.url_template"},
		},
		{
			name:      "换某条档位的 script 路径是拒绝",
			applied:   twoCommands,
			candidate: withCommandScript(twoCommands, "cfg-script-one", "scripts/evil.sh"),
			reject:    []string{"executors.commands.cfg-script-one.script"},
		},
		{
			name:      "换某条档位的 runtime 是拒绝",
			applied:   twoCommands,
			candidate: withCommandRuntime(twoCommands, "cfg-script-one", "node"),
			reject:    []string{"executors.commands.cfg-script-one.runtime"},
		},
		{
			name:      "改 http 档位的 allowed_hosts 是拒绝",
			applied:   twoCommands,
			candidate: withCommandHosts(twoCommands, "cfg-http-one", "evil.example.com"),
			reject:    []string{"executors.commands.cfg-http-one.allowed_hosts"},
		},
		{
			name:      "把 http 档位的 deny_private_ranges 从 false 改成 true 是拒绝",
			applied:   twoCommands,
			candidate: withCommandDenyPrivate(twoCommands, "cfg-http-one", true),
			reject:    []string{"executors.commands.cfg-http-one.deny_private_ranges"},
		},
		{
			name:      "给档位加一个 env 键是拒绝（env 装的是固定注入的凭据材料）",
			applied:   twoCommands,
			candidate: withCommandEnv(twoCommands, "cfg-script-one", "REPORT_HOME", "/srv/report"),
			reject:    []string{"executors.commands.cfg-script-one.env"},
		},
		{
			name:      "server.port 是重启档",
			applied:   base,
			candidate: withServerPort(base, "9090"),
			restart:   []string{"server.port"},
		},
		{
			name:      "server.auth.token 是拒绝档",
			applied:   base,
			candidate: withAuthToken(base, "rotated"),
			reject:    []string{"server.auth.token"},
		},
		{
			name:      "server.auth.users 增一条是拒绝档",
			applied:   base,
			candidate: withAppendUser(base),
			reject:    []string{"server.auth.users"},
		},
		{
			name:      "server.auth.jwt.secret 是拒绝档",
			applied:   base,
			candidate: withJWTSecret(base, strings.Repeat("k", 40)),
			reject:    []string{"server.auth.jwt.secret"},
		},
		{
			name:      "executors.workspace 与 runtime_allow 是重启档而不是拒绝档（设计文档 §6.4）",
			applied:   base,
			candidate: withExecWorkspaceAndAllow(base),
			restart:   []string{"executors.workspace", "executors.runtime_allow"},
		},
		{
			name:         "什么都没改",
			applied:      base,
			candidate:    base,
			wantNoChange: true,
		},
		{
			name:         "默认值与归一化后的默认值之间没有差别",
			applied:      DefaultConfig(),
			candidate:    DefaultConfig().Normalized(),
			wantNoChange: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			change := Diff(tc.applied.Normalized(), tc.candidate.Normalized())

			if tc.wantNoChange {
				assert.False(t, change.HasChanges(), "两侧取值相同却报出了改动：%+v", change)
				assert.Empty(t, change.Hot)
				assert.Empty(t, change.Restart)
				assert.Empty(t, change.Reject)
				assert.Empty(t, change.Commands)
				return
			}

			hot := changedPaths(change.Hot)
			restart := changedPaths(change.Restart)
			reject := changedPaths(change.Reject)
			commands := changedPaths(change.Commands)

			assertPresent(t, "热更档", hot, tc.hot)
			assertAbsent(t, "重启档", restart, tc.hot)
			assertAbsent(t, "拒绝档", reject, tc.hot)

			assertPresent(t, "重启档", restart, tc.restart)
			assertAbsent(t, "热更档", hot, tc.restart)
			assertAbsent(t, "拒绝档", reject, tc.restart)

			assertPresent(t, "拒绝档", reject, tc.reject)
			assertAbsent(t, "热更档", hot, tc.reject)
			assertAbsent(t, "重启档", restart, tc.reject)

			for _, path := range tc.commands {
				assert.Contains(t, commands, path, "档位变更必须单列进 Commands")
			}
			// Commands 的定义是"Hot 里属于 executors.commands 的那批"，不许多出别的键
			for _, path := range commands {
				assert.Contains(t, hot, path, "Commands 必须是 Hot 的子集")
			}
			assert.True(t, change.HasChanges())
		})
	}
}

// TestDiffReportsOldAndNewValues 断言 ChangedKey 带着两侧原值：日志与 /admin/runtime
// 只输出路径（配置里可能有凭据），但重载链与测试需要能说出"从什么改成什么"。
func TestDiffReportsOldAndNewValues(t *testing.T) {
	change := Diff(DefaultConfig().Normalized(), withLoggingLevel(DefaultConfig(), "debug").Normalized())
	require.Len(t, change.Hot, 1)
	assert.Equal(t, "logging.level", change.Hot[0].Path)
	assert.Equal(t, "info", change.Hot[0].Old)
	assert.Equal(t, "debug", change.Hot[0].New)
}

// TestDiffHasRejections 是"要么全变要么没变"的判据：一次改动同时含热更键与拒绝键时，
// Reject 有内容、Hot 也有内容（Hot 填好是为了让日志说清"本来会应用哪些"，不代表被应用了），
// 调用方据 HasRejections() 整次作废。
func TestDiffHasRejections(t *testing.T) {
	applied := DefaultConfig().Normalized()
	candidate := withAuthToken(withLoggingLevel(applied, "debug"), "rotated")

	change := Diff(applied, candidate.Normalized())

	assert.True(t, change.HasChanges())
	assert.True(t, change.HasRejections(), "改了凭据必须让整次重载作废")
	assert.Equal(t, []string{"server.auth.token"}, changedPaths(change.Reject))
	assert.Equal(t, []string{"logging.level"}, changedPaths(change.Hot))
}

// TestDiffListsKeysSorted 断言四份清单都按路径字典序排好：调用方把这份列表直接打进日志，
// 无序的输出没法逐条比对。
func TestDiffListsKeysSorted(t *testing.T) {
	applied := DefaultConfig().Normalized()
	candidate := withLoggingLevel(withServerPort(withSchedulerWorkers(applied, 32), "9090"), "debug")

	change := Diff(applied, candidate.Normalized())
	lists := [][]ChangedKey{change.Hot, change.Restart, change.Reject, change.Commands}
	total := 0
	for _, list := range lists {
		paths := changedPaths(list)
		total += len(paths)
		require.True(t, sort.StringsAreSorted(paths), "清单未按路径字典序排列：%v", paths)
	}
	assert.Positive(t, total, "这条用例得有内容才能证明排序断言不是空转")
}

// TestClassifyUnknownPathIsNotSilent 守住 classify 的第二种返回：既无精确命中也无前缀命中
// 时 ok 为 false，调用方（守卫用例）才知道"新增键忘了归档"。
func TestClassifyUnknownPathIsNotSilent(t *testing.T) {
	_, ok := classify("brand.new.key")
	assert.False(t, ok, "未归档的路径必须报 miss，而不是悄悄落到某一档")

	for _, tc := range []struct {
		path  string
		class ConfigClass
	}{
		{"logging.level", ClassHot},
		{"server.port", ClassRestart},
		{"server.auth.token", ClassReject},
		{"server.auth.users.#0.password_bcrypt", ClassReject},
		{"executors.commands", ClassHot},
		{"executors.commands.a-b.timeout", ClassHot},
		{"executors.commands.a-b.script", ClassReject},
		{"executors.commands.#0.script", ClassReject},
		{"executors.commands.a-b.positional.max", ClassHot},
	} {
		class, ok := classify(tc.path)
		assert.True(t, ok, tc.path)
		assert.Equal(t, tc.class, class, tc.path)
	}

	assert.Equal(t, "hot", ClassHot.String())
	assert.Equal(t, "restart", ClassRestart.String())
	assert.Equal(t, "reject", ClassReject.String())
}

// ---- 构造两份配置的辅助函数（只在测试里用） ----

func withCommands(cfg Config, commands ...ExecutorCommand) Config {
	cfg.Executors.Commands = commands
	return cfg
}

func withLoggingLevel(cfg Config, level string) Config {
	cfg.Logging.Level = level
	return cfg
}

func withSchedulerWorkers(cfg Config, workers int) Config {
	cfg.Scheduler.Workers = workers
	return cfg
}

func withStoreHistoryTTL(cfg Config, ttl time.Duration) Config {
	cfg.Store.HistoryTTL = ttl
	return cfg
}

func withEventRetentionCount(cfg Config, count int) Config {
	cfg.Observability.Events.RetentionCount = count
	return cfg
}

func withReloadDebounce(cfg Config, debounce time.Duration) Config {
	cfg.Reload.Debounce = debounce
	return cfg
}

func withReloadEnabled(cfg Config, enabled bool) Config {
	cfg.Reload.Enabled = enabled
	return cfg
}

func withServerPort(cfg Config, port string) Config {
	cfg.Server.Port = port
	return cfg
}

func withAuthToken(cfg Config, token string) Config {
	cfg.Server.Auth.Token = token
	return cfg
}

func withJWTSecret(cfg Config, secret string) Config {
	cfg.Server.Auth.JWT.Secret = secret
	return cfg
}

func withAppendUser(cfg Config) Config {
	cfg.Server.Auth.Users = append(cfg.Server.Auth.Users, UserConfig{
		Name:           "cfg-added-user",
		PasswordBcrypt: "$2a$10$abc",
		Role:           "admin",
	})
	return cfg
}

func withExecWorkspaceAndAllow(cfg Config) Config {
	cfg.Executors.Workspace = "./other-workspace"
	cfg.Executors.RuntimeAllow = []string{"bash", "node"}
	return cfg
}

// withAddedAndRetimedCommand 同时做两件事：改一条已有档位的 timeout、增一条新档位。
// 增删与已有档位的取值改动都必须留在热更档，只有"改了已有档位的身份字段"才进拒绝档。
func withAddedAndRetimedCommand(cfg Config) Config {
	added := diffScriptCommand("cfg-added-one")
	changed := cfg.Executors.Commands[0]
	changed.Timeout = 10 * time.Minute
	cfg.Executors.Commands = []ExecutorCommand{changed, cfg.Executors.Commands[1], added}
	return cfg
}

// mapCommand 按名字改一条档位：先复制切片，避免改到 applied 那一侧的共享取值。
func mapCommand(cfg Config, name string, edit func(ExecutorCommand) ExecutorCommand) Config {
	commands := make([]ExecutorCommand, 0, len(cfg.Executors.Commands))
	for _, command := range cfg.Executors.Commands {
		if command.Name == name {
			command = edit(command)
		}
		commands = append(commands, command)
	}
	cfg.Executors.Commands = commands
	return cfg
}

func withCommandScript(cfg Config, name, script string) Config {
	return mapCommand(cfg, name, func(c ExecutorCommand) ExecutorCommand {
		c.Script = script
		return c
	})
}

func withCommandRuntime(cfg Config, name, runtime string) Config {
	return mapCommand(cfg, name, func(c ExecutorCommand) ExecutorCommand {
		c.Runtime = runtime
		return c
	})
}

func withCommandHosts(cfg Config, name, host string) Config {
	return mapCommand(cfg, name, func(c ExecutorCommand) ExecutorCommand {
		c.AllowedHosts = []string{host}
		return c
	})
}

func withCommandDenyPrivate(cfg Config, name string, deny bool) Config {
	return mapCommand(cfg, name, func(c ExecutorCommand) ExecutorCommand {
		// 指向新的布尔而不是复用同一个指针：两侧必须是不同的取值
		c.DenyPrivate = &deny
		return c
	})
}

func withCommandEnv(cfg Config, name, key, value string) Config {
	return mapCommand(cfg, name, func(c ExecutorCommand) ExecutorCommand {
		env := make(map[string]string, len(c.Env)+1)
		for k, v := range c.Env {
			env[k] = v
		}
		env[key] = value
		c.Env = env
		return c
	})
}

// changedPaths 取一份清单里的路径，排序后返回：这样断言写成 []string{...} 的字面量即可。
func changedPaths(list []ChangedKey) []string {
	paths := make([]string, 0, len(list))
	for _, key := range list {
		paths = append(paths, key.Path)
	}
	sort.Strings(paths)
	return paths
}

func assertPresent(t *testing.T, label string, list, want []string) {
	t.Helper()
	for _, path := range want {
		assert.Contains(t, list, path, "%s 缺 %q", label, path)
	}
}

func assertAbsent(t *testing.T, label string, list, unwanted []string) {
	t.Helper()
	for _, path := range unwanted {
		assert.NotContains(t, list, path, "%q 不该出现在%s", path, label)
	}
}
