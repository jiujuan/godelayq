package core

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 这一组是 TASK-N03 的用例：executors.adhoc 一节的默认值、校验判据、归一化与环境变量覆盖。
// 执行侧行为（注册哪四条、路径怎么判）在 executor 包，见 TASK-N04、TASK-N05。

const adhocEnabledBase = "executors:\n  enabled: true\n  adhoc:\n    enabled: true\n"

func TestAdhocDefaults(t *testing.T) {
	cfg, err := LoadConfig(writeConfigFile(t, "server:\n  port: \"8080\"\n"))
	require.NoError(t, err)

	adhoc := cfg.Executors.Adhoc
	assert.False(t, adhoc.Enabled, "整节默认关闭：打开它等于让任务提交时带来的路径决定执行什么")
	assert.Equal(t, DefaultAdhocShellRuntime, adhoc.ShellRuntime)
	assert.Empty(t, adhoc.PathPrefixes)
	assert.Empty(t, adhoc.URLHosts)
	assert.False(t, adhoc.URLAllowPrivate)
	assert.Equal(t, DefaultAdhocHTTPTimeout, adhoc.HTTPTimeout)
	require.NotNil(t, adhoc.RequireExtension, "没写这一项时默认值是 true，指针必须已经落好")
	assert.True(t, *adhoc.RequireExtension)
	assert.True(t, adhoc.WantExtensionCheck())
}

func TestAdhocRequireExtensionKeepsExplicitFalse(t *testing.T) {
	// 这一项的默认取值是 true，而 false 恰好是零值：普通布尔分不出"没写"与"写了 false"。
	// 上面那条用例证明"没写"落到 true，这条证明"显式写 false"不会被补回 true。
	cfg, err := LoadConfig(writeConfigFile(t, adhocEnabledBase+"    require_extension: false\n"))
	require.NoError(t, err)
	require.NotNil(t, cfg.Executors.Adhoc.RequireExtension)
	assert.False(t, *cfg.Executors.Adhoc.RequireExtension)
	assert.False(t, cfg.Executors.Adhoc.WantExtensionCheck())

	normalized := cfg.Normalized()
	assert.False(t, *normalized.Executors.Adhoc.RequireExtension,
		"归一化只补 nil，显式的 false 必须留住——那是知道风险后主动关掉这道要求的写法")
}

func TestAdhocWantExtensionCheckOnZeroValue(t *testing.T) {
	// 手工构造的 Config（测试替身与内嵌调用会这么用）没经过 DefaultConfig，指针是 nil；
	// WantExtensionCheck 必须给出与默认值一致的答案，而不是让每个调用处各自判空。
	var adhoc AdhocConfig
	assert.Nil(t, adhoc.RequireExtension)
	assert.True(t, adhoc.WantExtensionCheck())
	assert.Equal(t, DefaultAdhocRequireExtension, adhoc.WantExtensionCheck())
}

func TestAdhocNormalizedIsIdempotent(t *testing.T) {
	cfg, err := LoadConfig(writeConfigFile(t, "executors:\n  adhoc:\n    shell_runtime: \"\"\n    http_timeout: 0s\n"))
	require.NoError(t, err)

	once := cfg.Normalized()
	assert.Equal(t, DefaultAdhocShellRuntime, once.Executors.Adhoc.ShellRuntime)
	assert.Equal(t, DefaultAdhocHTTPTimeout, once.Executors.Adhoc.HTTPTimeout)

	twice := once.Normalized()
	assert.Equal(t, *once.Executors.Adhoc.RequireExtension, *twice.Executors.Adhoc.RequireExtension)
	assert.Equal(t, once.Executors.Adhoc, twice.Executors.Adhoc,
		"归一化跑两次必须得到同一个答案，否则执行侧两次拿到的默认值会不同")

	// 默认值里的那个指针不能被就地改写：每次取的都该是新指针。
	first := defaultAdhocRequireExtension()
	*first = false
	second := defaultAdhocRequireExtension()
	assert.True(t, *second, "默认指针每次新建一份，改一份不会串到另一份")
}

func TestAdhocValidate_Rejects(t *testing.T) {
	for _, tc := range []struct {
		name string
		yaml string
		want string
	}{
		{
			"adhoc enabled without executors",
			"executors:\n  enabled: false\n  adhoc:\n    enabled: true\n",
			"executors.adhoc.enabled requires executors.enabled to be true",
		},
		{
			"shell runtime outside allow list",
			"executors:\n  enabled: true\n  runtime_allow: [bash]\n  adhoc:\n    enabled: true\n    shell_runtime: pwsh\n",
			`executors.adhoc.shell_runtime "pwsh" is not in executors.runtime_allow`,
		},
		{
			"shell runtime is whitespace",
			"executors:\n  enabled: true\n  adhoc:\n    shell_runtime: \"  \"\n",
			"executors.adhoc.shell_runtime must not be whitespace",
		},
		{
			"empty path prefix entry",
			adhocEnabledBase + "    path_prefixes: [./scripts, \"\"]\n",
			"executors.adhoc.path_prefixes must not contain an empty entry",
		},
		{
			"padded path prefix entry",
			adhocEnabledBase + "    path_prefixes: [\" ./scripts \"]\n",
			"executors.adhoc.path_prefixes entries must not have surrounding spaces",
		},
		{
			"url host written as a full URL",
			adhocEnabledBase + "    url_hosts: [\"https://api.example.com\"]\n",
			"executors.adhoc.url_hosts[0] \"https://api.example.com\" must be a host, not a URL",
		},
		{
			"url host carries a path",
			adhocEnabledBase + "    url_hosts: [api.example.com/hooks]\n",
			"must not contain a path, query or credentials",
		},
		{
			"negative http timeout",
			adhocEnabledBase + "    http_timeout: -1m\n",
			"executors.adhoc.http_timeout must not be negative",
		},
		{
			"http timeout above the ceiling",
			adhocEnabledBase + "    http_timeout: 45m\n",
			"executors.adhoc.http_timeout 45m0s must not exceed executors.max_timeout",
		},
		{
			"shape is checked even when adhoc is off",
			"executors:\n  enabled: true\n  adhoc:\n    enabled: false\n    url_hosts: [\"http://x.example\"]\n",
			"must be a host, not a URL",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadConfig(writeConfigFile(t, tc.yaml))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestAdhocValidate_Allows(t *testing.T) {
	t.Run("只打开开关其它全默认", func(t *testing.T) {
		cfg, err := LoadConfig(writeConfigFile(t, adhocEnabledBase))
		require.NoError(t, err)
		assert.True(t, cfg.Executors.Adhoc.Enabled)
		assert.Empty(t, cfg.Executors.Adhoc.PathPrefixes, "空列表是合法取值：不限目录（设计文档 §12 P1）")
		assert.Empty(t, cfg.Executors.Adhoc.URLHosts, "空列表是合法取值：不限主机")
	})

	t.Run("收紧后的写法", func(t *testing.T) {
		cfg, err := LoadConfig(writeConfigFile(t, adhocEnabledBase+
			"    shell_runtime: sh\n"+
			"    path_prefixes: [./scripts, D:/work/scripts]\n"+
			"    url_hosts: [api.example.com, \"*.corp.example\", internal:8443]\n"+
			"    http_timeout: 2m\n"))
		require.NoError(t, err)
		assert.Equal(t, "sh", cfg.Executors.Adhoc.ShellRuntime)
		assert.Len(t, cfg.Executors.Adhoc.PathPrefixes, 2)
		assert.Len(t, cfg.Executors.Adhoc.URLHosts, 3)
		assert.Equal(t, 2*time.Minute, cfg.Executors.Adhoc.HTTPTimeout)
	})

	t.Run("整节关闭时不判解释器成员", func(t *testing.T) {
		// 默认 shell_runtime 是 bash，而把 runtime_allow 收成 [node, php] 是常见配置。
		// 关闭状态下这一节的取值都不参与行为，不该让启动失败
		// （判据位置见 validateAdhocConfig 的注释；打开以后这条会立刻生效）。
		_, err := LoadConfig(writeConfigFile(t,
			"executors:\n  enabled: true\n  runtime_allow: [node, php]\n  adhoc:\n    enabled: false\n"))
		require.NoError(t, err)
	})

	t.Run("打开后同一份白名单就要判", func(t *testing.T) {
		_, err := LoadConfig(writeConfigFile(t,
			"executors:\n  enabled: true\n  runtime_allow: [node, php]\n  adhoc:\n    enabled: true\n"))
		require.Error(t, err, "默认 bash 不在这份白名单里，打开整节就必须回答它")
		assert.Contains(t, err.Error(), `executors.adhoc.shell_runtime "bash" is not in executors.runtime_allow`)
	})

	t.Run("关闭时留空全部放过", func(t *testing.T) {
		_, err := LoadConfig(writeConfigFile(t,
			"executors:\n  enabled: false\n  adhoc:\n    enabled: false\n    http_timeout: 0s\n"))
		require.NoError(t, err)
	})
}

func TestAdhocEnvOverride(t *testing.T) {
	// 这一节只有 enabled 绑了环境变量：开发环境要临时试一次"提交时给路径"，靠它就够了。
	// BindEnv 的字符串列表漏项不会报错，只会让该项无法被覆盖，所以这里正向钉一次。
	t.Setenv("GODELAYQ_EXECUTORS_ADHOC_ENABLED", "true")
	cfg, err := LoadConfig(writeConfigFile(t, "executors:\n  enabled: true\n"))
	require.NoError(t, err)
	assert.True(t, cfg.Executors.Adhoc.Enabled, "GODELAYQ_EXECUTORS_ADHOC_ENABLED 必须能打开这一节")

	// 其余六项故意不绑（列表绑了会把"覆盖"变成"替换"，与 executors.commands 不绑同一条理由）。
	// 这条断言的意义是：将来有人给它们加了绑定，这里会红，逼他重新回答"该不该绑"。
	t.Setenv("GODELAYQ_EXECUTORS_ADHOC_SHELL_RUNTIME", "node")
	t.Setenv("GODELAYQ_EXECUTORS_ADHOC_HTTP_TIMEOUT", "3m")
	unbound, err := LoadConfig(writeConfigFile(t, "executors:\n  enabled: true\n"))
	require.NoError(t, err)
	assert.Equal(t, DefaultAdhocShellRuntime, unbound.Executors.Adhoc.ShellRuntime,
		"shell_runtime 故意不绑环境变量")
	assert.Equal(t, DefaultAdhocHTTPTimeout, unbound.Executors.Adhoc.HTTPTimeout,
		"http_timeout 故意不绑环境变量")
}

func TestAdhocReloadClassesAreRestart(t *testing.T) {
	// 整节都是重启档：它们决定注册表里有没有那四条键，以及任务提交时能给到哪里的边界。
	// 归错档的后果不是报错而是"改了静默不生效"，所以逐条断言。
	for _, path := range []string{
		"executors.adhoc.enabled",
		"executors.adhoc.shell_runtime",
		"executors.adhoc.path_prefixes",
		"executors.adhoc.require_extension",
		"executors.adhoc.url_hosts",
		"executors.adhoc.url_allow_private",
		"executors.adhoc.http_timeout",
	} {
		class, ok := classify(path)
		require.True(t, ok, "%q 必须出现在重载分类表里", path)
		assert.Equal(t, ClassRestart, class, "%q 的重启档归属不能改", path)
	}
}
