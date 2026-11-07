package core

import (
	"reflect"
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
	// 样本用 diffScriptCommand/diffHTTPCommand 而不是内联字面量：内联那份和用例里的
	// 差异构造是同一份数据的两个真相源，早各自漂过（只有内联这侧设了 CaptureResponse）。
	sample := DefaultConfig()
	sample.Executors.Commands = []ExecutorCommand{
		diffScriptCommand("cfg-script-one"),
		diffHTTPCommand("cfg-http-one"),
	}

	leaves := flattenLeaves(sample)

	// 覆盖断言是类型驱动的：从 Config 的类型走一遍每个导出字段路径，要求它"自身是叶子，
	// 或其下有叶子"。它同时抓两种漏摊——整节没被摊开，以及摊不出导出字段的结构体
	// （time.Time 那一类，靠 flattenStruct 的容器叶子兜底才出现在 leaves 里）。
	// 原先的 len(leaves) < 60 是个没有出处的魔法数字，漏的是"少了哪一节"而不是"少了几个"。
	assertEveryExportedFieldCovered(t, reflect.TypeOf(sample), "", leaves)

	// 正向：每个叶子都有档
	for path := range leaves {
		if _, ok := classify(path); !ok {
			t.Errorf("leaf key %q has no reload class; add it to configClasses", path)
		}
	}

	// 反向一：表里的路径必须真能被摊出来（executors.commands 与凭据前缀是合法例外）
	for path := range configClasses {
		if _, ok := leaves[path]; !ok && !isCommandContainerPath(path) {
			t.Errorf("configClasses has %q but DefaultConfig does not produce it", path)
		}
	}

	// 反向二：摊出来的每条档位路径必须能反解回自己在 leaves 里的键（配对可逆）。
	// splitCommandLeaf 用两次 Cut 拆 name 与 field，前提是名字段不含点——由 commandEntryName
	// 兜底保证。这条守卫就是那个前提的可执行表述：档位名含点时
	// executors.commands.report.timeout.script 会被切成 name=report field=timeout，
	// 反解出的 executors.commands.report.timeout 不是叶子 → 这里红。
	// 含点样本本身的用例见 TestDiffDottedCommandNameKeepsAttribution。
	assertCommandLeavesPair(t, leaves)

	// 档位字段的两份清单必须**恰好**覆盖摊出来的每一个字段名——多一个少一个都报错：
	//   少（清单漏字段）：ExecutorCommand 新增了字段却没进 permission/hot 任一清单 → 未归档，
	//     本次要新增的就是这条守卫；
	//   多（清单写多了）：清单里列着结构体根本没有的字段名（笔误或字段被删）。
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
	// 两份清单还必须**不相交**：同一个字段名两边都写时 classify 的 switch 会静默偏向拒绝档
	//（先判 permissionCommandFields），写热更清单的那半等于没写，谁也不会发现。
	for field := range permissionCommandFields {
		if hotCommandFields[field] {
			t.Errorf("executors.commands field %q is on both permissionCommandFields and hotCommandFields; classify silently prefers the reject side", field)
		}
	}

	// 摊平必须真的把两条档位摊成按名字的子路径，否则上面几条反向检查都在空转
	require.Contains(t, leaves, "executors.commands.cfg-script-one.runtime")
	require.Contains(t, leaves, "executors.commands.cfg-http-one.deny_private_ranges")
	assert.NotContains(t, leaves, "executors.commands", "有条目时档位列表不作为一个容器叶子出现")
}

// assertEveryExportedFieldCovered 从 structType 的类型出发，逐个导出字段路径断言
// "自身是叶子，或其下有叶子"。递归只进结构体字段：切片与映射本身就是叶子
// （档位列表下面按名字摊出的子路径由"其下有叶子"这一支覆盖，元素字段名则由
// permission/hotCommandFields 的恰好覆盖断言守着，不在这里重复）。
func assertEveryExportedFieldCovered(t *testing.T, structType reflect.Type, prefix string, leaves map[string]leafValue) {
	t.Helper()
	for i := 0; i < structType.NumField(); i++ {
		field := structType.Field(i)
		if field.PkgPath != "" {
			continue // 非导出字段不是配置项，摊平也不会摊它
		}
		path := joinLeafPath(prefix, mapstructureKey(field))
		if _, isLeaf := leaves[path]; isLeaf {
			continue
		}
		if !hasLeafUnder(leaves, path) {
			t.Errorf("exported field path %q is neither a leaf nor has any leaf under it; 摊平漏了这一节", path)
			continue
		}
		if field.Type.Kind() == reflect.Struct {
			assertEveryExportedFieldCovered(t, field.Type, path, leaves)
		}
	}
}

// mapstructureKey 与实现里的 leafKey 同规则，但独立写一遍：实现把标签读错时
// 这条覆盖断言不会跟着一起瞎。
func mapstructureKey(field reflect.StructField) string {
	key, _, _ := strings.Cut(field.Tag.Get("mapstructure"), ",")
	if key == "" {
		return strings.ToLower(field.Name)
	}
	return key
}

func hasLeafUnder(leaves map[string]leafValue, prefix string) bool {
	for path := range leaves {
		if strings.HasPrefix(path, prefix+".") {
			return true
		}
	}
	return false
}

// TestClassifyUnarchivedCommandFieldIsNotSilent 证明"给 ExecutorCommand 新增字段时必须显式
// 归档"这条守卫真的存在：一个不在 permissionCommandFields 也不在 hotCommandFields 里的档位
// 字段路径，classify 必须回 ok=false（与"未知顶层键未归档"同一脸色），而不是悄悄落到热更档。
// 用直接调 classify 的写法，不依赖改结构体——将来加字段忘了归档时，守卫用例（
// TestEveryLeafKeyIsClassed）正是靠这个 ok=false 分支变红。
func TestClassifyUnarchivedCommandFieldIsNotSilent(t *testing.T) {
	class, ok := classify("executors.commands.cfg-script-one.__not_archived__")
	assert.False(t, ok, "未归档的档位字段必须报 miss，而不是默认落进热更档")
	assert.Equal(t, ClassRestart, class, "未归档时返回重启档：忽略 ok 的调用方最坏只是进 ignored_keys，不会把新字段变成免重启通道")
}

// isCommandContainerPath 只认一类合法条目：executors.commands（条目增删本身，
// 列表为空时以容器路径出现、有条目时摊成按名字的子路径，两种形状都不会同时在场）。
//
// 这里不再放行 rejectPrefixes 那三条凭据前缀：configClasses 里没有那三条精确键
// （凭据只在前缀表里），所以原来的循环是死分支，留着会让读者以为反向守卫对凭据开了口子。
// 放在测试文件里，不外溢。
func isCommandContainerPath(path string) bool {
	return path == "executors.commands"
}

// commandFieldNames 收集摊出的 executors.commands.<name>.<field> 集合里出现过的所有字段名。
// 它是"两份清单恰好覆盖档位字段"这条守卫的基础：结构体新增/删除字段都会在这里显形。
// 这里刻意用字面量重新解析一遍路径，不复用实现里的拆分函数：实现写错时守卫不会跟着一起错。
// 放在测试文件里，不外溢。
func commandFieldNames(leaves map[string]leafValue) map[string]bool {
	const prefix = "executors.commands."
	fields := make(map[string]bool)
	for path := range leaves {
		if !strings.HasPrefix(path, prefix) {
			continue
		}
		if name, rest, ok := strings.Cut(strings.TrimPrefix(path, prefix), "."); ok && name != "" {
			field, _, _ := strings.Cut(rest, ".")
			if field != "" {
				fields[field] = true
			}
		}
	}
	return fields
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

	// 空档位列表以容器路径出现。它同时是"这一节被摊出来了"的证据：同一段循环里
	// executors.commands 已经 require 过存在且 Kind 是列表，这里不再重复 Contains 一遍。

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

// TestFlattenLeavesOpaqueStructBecomesContainerLeaf 钉住 flattenStruct 的容器叶子兜底：
// 一个摊不出任何导出字段的结构体（time.Time 的 wall/ext/loc 全未导出，不透明封装类型与
// 空占位节同理）本身必须成为一个叶子，否则这个配置键既不进 leaves 也不进 Diff，
// 正反向两条守卫都看不见它——改了它既不热更也不提示重启，是纯粹的静默失效。
//
// 现存的 Config 里还没有这种形状（实测：加了兜底之后 flattenLeaves(DefaultConfig())
// 的叶子集合与加之前逐条相同，53 条一个不多），所以只能用探针类型直接喂 flattenStruct：
// 它正是 flattenLeaves 的第一步。将来 Config 真出现这种字段时，摊出的容器路径会多出来，
// 由 TestEveryLeafKeyIsClassed 的正向断言逼着人去 configClasses 归档。
func TestFlattenLeavesOpaqueStructBecomesContainerLeaf(t *testing.T) {
	probe := func(value r01ProbeSection) map[string]leafValue {
		leaves := make(map[string]leafValue)
		flattenStruct(leaves, "section", reflect.ValueOf(value))
		return leaves
	}

	leaves := probe(r01ProbeSection{Level: "info", Stamped: r01Opaque{hidden: 1}, Blank: r01Empty{}})
	require.Contains(t, leaves, "section.stamped", "摊不出导出字段的结构体必须整体算一个叶子")
	require.Contains(t, leaves, "section.blank", "空占位节同理")
	require.Contains(t, leaves, "section.level", "能摊出来的字段照旧是它自己的叶子")

	// 兜底出来的叶子不是死形状：它背后的取值一改就得报出差异，否则归了档也没意义
	before := probe(r01ProbeSection{Stamped: r01Opaque{hidden: 1}})
	after := probe(r01ProbeSection{Stamped: r01Opaque{hidden: 2}})
	assert.False(t, sameLeaf(before["section.stamped"], after["section.stamped"]),
		"未导出字段变了也要报出差异：DeepEqual 比的是整个结构体")
}

// r01ProbeSection 是上面那条用例的探针类型；r01Opaque 只有一个未导出字段，
// 像 time.Time 那样摊不出任何叶子，r01Empty 一个字段都没有，是"空占位节"的形状。
type r01ProbeSection struct {
	Level   string    `mapstructure:"level"`
	Stamped r01Opaque `mapstructure:"stamped"`
	Blank   r01Empty  `mapstructure:"blank"`
}

type r01Opaque struct{ hidden int }

type r01Empty struct{}

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
		Name:            name,
		Kind:            "http",
		Method:          "POST",
		URLTemplate:     "https://api.example.com/jobs",
		AllowedHosts:    []string{"api.example.com"},
		Headers:         map[string][]string{"Accept": {"application/json"}},
		Body:            "json",
		CaptureResponse: true,
		DenyPrivate:     &deny,
	}
}

// TestDiffClassifiesChangedKeys 逐条走 §5.3 的分类表。
// 每行只声明"这几条路径必须落在这一档、且不许落在另外两档"：
// 档位增删一行会摊出几十个叶子，逐条比对期望值会把用例写成第二份实现。
func TestDiffClassifiesChangedKeys(t *testing.T) {
	base := DefaultConfig().Normalized()
	twoCommands := withCommands(base, diffScriptCommand("cfg-script-one"), diffHTTPCommand("cfg-http-one"))

	for _, tc := range []struct {
		name           string
		applied        Config
		candidate      Config
		hot            []string
		restart        []string
		reject         []string
		commands       []string
		wantNoCommands bool // 这行的改动不涉及任何档位，Commands 必须是空的
		wantNoChange   bool
	}{
		{
			name:           "logging.level 是热更",
			applied:        base,
			candidate:      withLoggingLevel(base, "debug"),
			hot:            []string{"logging.level"},
			wantNoCommands: true,
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
			// D7 同向：kind 是"这个任务类型到底执行什么"的身份字段，不是可调参数。
			// 同一行里改 timeout 作对照——它是取值型参数，必须仍落热更档。
			name:      "换某条既有档位的 kind 是拒绝，同时改它的 timeout 仍是热更",
			applied:   twoCommands,
			candidate: withCommandKindAndTimeout(twoCommands, "cfg-script-one", "binary", 10*time.Minute),
			reject:    []string{"executors.commands.cfg-script-one.kind"},
			hot:       []string{"executors.commands.cfg-script-one.timeout"},
			commands:  []string{"executors.commands.cfg-script-one.timeout"},
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
			name:           "server.port 是重启档",
			applied:        base,
			candidate:      withServerPort(base, "9090"),
			restart:        []string{"server.port"},
			wantNoCommands: true,
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
				assert.True(t, isCommandsKey(path), "Commands 里出现了非档位键 %q", path)
			}
			// Hot 里的档位键一条都不许漏出 Commands
			for _, path := range hot {
				if isCommandsKey(path) {
					assert.Contains(t, commands, path, "热更的档位键必须同时进 Commands")
				}
			}
			if tc.wantNoCommands {
				assert.Empty(t, commands, "这行的改动不涉及任何档位，Commands 必须是空的")
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
//
// 这里刻意不走 changedPaths：那个 helper 内部自己 sort 过，用它断言"清单有序"是恒真断言
// ——把实现的 sort.Strings 换成长度降序，用例照样全绿（这条的变异验证见卡 §10.3）。
// 取路径要按清单里的原始顺序取，并且每份清单至少两条键：一条键的"有序"也是空转。
func TestDiffListsKeysSorted(t *testing.T) {
	applied := withCommands(DefaultConfig().Normalized(),
		diffScriptCommand("cfg-script-one"), diffHTTPCommand("cfg-http-one"))

	candidate := applied
	candidate = withLoggingLevel(candidate, "debug")              // Hot
	candidate = withSchedulerWorkers(candidate, 32)               // Hot
	candidate = withServerPort(candidate, "9090")                 // Restart
	candidate = withReloadEnabled(candidate, true)                // Restart
	candidate = withAuthToken(candidate, "rotated")               // Reject
	candidate = withJWTSecret(candidate, strings.Repeat("k", 40)) // Reject
	candidate = withAddedAndRetimedCommand(candidate)             // Hot + Commands

	change := Diff(applied, candidate.Normalized())

	for _, tc := range []struct {
		label string
		list  []ChangedKey
	}{
		{"Hot", change.Hot},
		{"Restart", change.Restart},
		{"Reject", change.Reject},
		{"Commands", change.Commands},
	} {
		paths := pathsInListOrder(tc.list)
		require.GreaterOrEqual(t, len(paths), 2, "%s 只有 %d 条键，排序断言在这份清单上是空转", tc.label, len(paths))
		require.True(t, sort.StringsAreSorted(paths), "%s 未按路径字典序排列：%v", tc.label, paths)
	}
}

// pathsInListOrder 按切片里的实际顺序取路径，一个都不重排。
func pathsInListOrder(list []ChangedKey) []string {
	paths := make([]string, 0, len(list))
	for _, key := range list {
		paths = append(paths, key.Path)
	}
	return paths
}

// TestDiffDottedCommandNameKeepsAttribution 守住 I-3：档位名里含点时路径归属不能错。
//
// 清洗前的形状是 executors.commands.report.timeout.script，被 splitCommandLeaf 的两次
// Cut 切成 name=report、field=timeout，于是这条新档位的 26 个字段全按"字段 timeout
// 在热更清单"归类，归因署成 report 的 timeout 字段——分类结论碰巧还是热更，署名是错的，
// 而 Diff 的输出要直接进日志与 applied_keys，署错名的改动没法逐条核对。
func TestDiffDottedCommandNameKeepsAttribution(t *testing.T) {
	applied := withCommands(DefaultConfig().Normalized(), diffScriptCommand("report"))

	// 名为 report.timeout 的新档位：与已有档位 report 同前缀，script 也不同
	// 名为 report.timeout 的新档位，script 与已有档位不同：合法的名字 report 一条没改
	dotted := diffScriptCommand("report.timeout")
	dotted.Script = "scripts/other.sh"
	candidate := withCommands(applied, diffScriptCommand("report"), dotted)
	change := Diff(applied, candidate.Normalized())

	// 新增的条目按"条目增删"归热更，不进拒绝档
	assert.False(t, change.HasRejections(), "新增档位不该被切成已有档位 report 的字段改动")
	hot := changedPaths(change.Hot)
	assert.Contains(t, hot, "executors.commands.#1.script",
		"含点的名字必须整体兜底成 #<i>，路径署在新条目自己名下")
	for _, path := range hot {
		assert.NotContains(t, path, "executors.commands.report.timeout",
			"含点的档位名不许出现被切成 report 的 timeout 字段的路径：%q", path)
	}
	// 已有档位 report 自己一条都没改
	for _, path := range hot {
		assert.NotEqual(t, "executors.commands.report.script", path,
			"report 自己没被改过，不许出现在热更清单里")
	}

	// 配对可逆：摊出的每条档位路径都能反解回自己在 leaves 里的键（守卫的另一半见
	// TestEveryLeafKeyIsClassed，这里用同一份含点样本直接跑一遍）
	assertCommandLeavesPair(t, flattenLeaves(candidate.Normalized()))
}

// assertCommandLeavesPair 断言摊出的每条档位路径与 splitCommandLeaf 的解析结果可逆：
// commandsPath + "." + name + "." + field 必须仍是 leaves 的键。
func assertCommandLeavesPair(t *testing.T, leaves map[string]leafValue) {
	t.Helper()
	for path := range leaves {
		name, field, ok := splitCommandLeaf(path)
		if !ok {
			continue
		}
		paired := commandsPath + "." + name + "." + field
		if _, found := leaves[paired]; !found {
			t.Errorf("command leaf %q does not pair back: %q is not a leaf; 档位名里含点会把路径切错", path, paired)
		}
	}
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
		{"executors.commands.a-b.kind", ClassReject},
		{"executors.commands.a-b.script", ClassReject},
		{"executors.commands.#0.script", ClassReject},
		{"executors.commands.a-b.positional.max", ClassHot},
	} {
		class, ok := classify(tc.path)
		assert.True(t, ok, tc.path)
		assert.Equal(t, tc.class, class, tc.path)
	}
}

// TestConfigClassString 是档位名的可读输出：三个结论名给日志与 /admin/runtime 用，
// 表外的整数值给"分类器与表失去同步"时的现场留一个能读的形状（ConfigClass(7) 而不是 panic）。
func TestConfigClassString(t *testing.T) {
	for _, tc := range []struct {
		class ConfigClass
		want  string
	}{
		{ClassHot, "hot"},
		{ClassRestart, "restart"},
		{ClassReject, "reject"},
		{ConfigClass(7), "ConfigClass(7)"},
	} {
		assert.Equal(t, tc.want, tc.class.String())
	}
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

// withCommandKindAndTimeout 在一条既有档位上同时改身份字段（kind）与取值字段（timeout）：
// 一行用例就能同时钉住"kind 进拒绝档、timeout 留热更档"两个方向。
func withCommandKindAndTimeout(cfg Config, name, kind string, timeout time.Duration) Config {
	return mapCommand(cfg, name, func(c ExecutorCommand) ExecutorCommand {
		c.Kind = kind
		c.Timeout = timeout
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

// isCommandsKey 认档位相关的两条形状：容器路径 executors.commands 本身（条目增删，
// 列表由空变非空或反向时出现）与按名字/索引摊出的子路径 executors.commands.<name>.<field>。
// Commands 的并集断言与子集断言共用它，避免两处各写一遍前缀规则。
func isCommandsKey(path string) bool {
	return path == commandsPath || strings.HasPrefix(path, commandsPath+".")
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
