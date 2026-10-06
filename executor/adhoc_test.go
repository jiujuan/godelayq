package executor

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"godelayq/core"
)

// 这一组是 TASK-N04 的用例：四条内置自由执行档位的构造、解释器过滤、
// 撞名时内置让位、探测只判解释器，以及整节关闭时的零变化。
//
// 提交期的路径与地址判据在 TASK-N05（executor/adhoc_validation_test.go）。

// adhocEnabled 在既有 configWith 之上打开整节，其它取值全用默认。
func adhocEnabled(cfg core.Config) core.Config {
	cfg.Executors.Adhoc.Enabled = true
	return cfg
}

// adhocNames 是四条内置档位的注册键，顺序与 adhocSpecs 一致。
var adhocNames = []string{"exec.php", "exec.python", "exec.shell", "exec.http"}

func TestAdhocProfiles_DisabledByDefault(t *testing.T) {
	// DoD 第 5 条的第一半：默认配置下这四条键不存在，构造函数一条都不返回。
	cfg := core.DefaultConfig()
	assert.False(t, cfg.Executors.Adhoc.Enabled, "整节默认必须关闭")

	profiles, skipped, err := AdhocProfiles(cfg)
	require.NoError(t, err)
	assert.Empty(t, profiles)
	assert.Empty(t, skipped, "关闭时不该有跳过说明：那时这四条本来就不该存在")

	registry, err := NewRegistry(cfg, nil)
	require.NoError(t, err)
	for _, key := range adhocNames {
		assert.NotContains(t, registry.Keys(), key)
	}
}

func TestAdhocProfiles_BuildsFourInFixedOrder(t *testing.T) {
	workspace := t.TempDir()
	cfg := adhocEnabled(configWith(workspace))

	profiles, skipped, err := AdhocProfiles(cfg)
	require.NoError(t, err)
	assert.Empty(t, skipped, "默认白名单含 php、python、bash，四条都该构造出来")
	require.Len(t, profiles, 4)

	for i, want := range adhocNames {
		assert.Equal(t, want, profiles[i].HandlerKey(), "顺序固定，接口与日志的输出因此稳定")
	}

	php := profiles[0]
	assert.True(t, php.Adhoc, "标记必须落下：提交期校验与探测都靠它分流")
	assert.Equal(t, KindScript, php.Kind)
	assert.Equal(t, "php", php.Runtime)
	assert.Empty(t, php.ScriptPath, "内置档位没有脚本路径可解析——那一项来自任务的 payload")
	assert.Empty(t, php.ScriptRel)
	assert.Equal(t, "script", php.AdhocLocationKey, "payload 顶层键，与 TASK-N05 的判据同源")
	assert.Equal(t, "path", php.AdhocLocationKind)
	assert.Equal(t, []string{".php"}, php.AdhocExtensions)

	python := profiles[1]
	assert.Equal(t, "python", python.Runtime)
	assert.Equal(t, []string{".py"}, python.AdhocExtensions)

	shell := profiles[2]
	assert.Equal(t, "bash", shell.Runtime, "默认解释器来自 executors.adhoc.shell_runtime")
	assert.Equal(t, []string{".sh", ".bash"}, shell.AdhocExtensions)

	http := profiles[3]
	assert.Equal(t, KindHTTP, http.Kind)
	assert.Empty(t, http.URLTemplate, "内置 http 档位没有地址模板——整条地址来自任务")
	assert.Equal(t, "url", http.AdhocLocationKey)
	assert.Equal(t, "url", http.AdhocLocationKind)
	assert.Equal(t, "POST", http.Method, "设计文档 §12 P5 的推荐值")
	assert.Equal(t, "json", http.Body)
	assert.Equal(t, 30*time.Second, http.Timeout, "executors.adhoc.http_timeout 的默认值")
	assert.True(t, http.DenyPrivate, "地址范围守卫不放宽（§D9 第③层）")
	assert.Empty(t, http.AllowedHosts, "url_hosts 留空 = 不限主机")

	// 超时上限与配置文件里的档位同一条规则：内置档位也受 executors.max_timeout 约束。
	for _, profile := range profiles {
		assertPositiveDuration(t, profile.Timeout, profile.Name)
	}
}

func assertPositiveDuration(t *testing.T, value time.Duration, profileName string) {
	t.Helper()
	assert.Greater(t, value, time.Duration(0),
		"档位 %s 的生效超时必须是正值：执行器任务不存在不限制这一档", profileName)
}

func TestAdhocProfiles_FiltersRuntimesOutsideAllowList(t *testing.T) {
	// 把白名单收成只有 bash：php 与 python 两条构造不出来，但这不是错误
	// ——一份只用 shell 与 HTTP 的部署是合法配置。
	workspace := t.TempDir()
	cfg := adhocEnabled(configWith(workspace))
	cfg.Executors.RuntimeAllow = []string{"bash"}

	profiles, skipped, err := AdhocProfiles(cfg)
	require.NoError(t, err)
	require.Len(t, profiles, 2)
	assert.Equal(t, []string{"exec.shell", "exec.http"}, []string{
		profiles[0].HandlerKey(), profiles[1].HandlerKey()})

	require.Len(t, skipped, 2)
	assert.Equal(t, "exec.php", skipped[0].HandlerKey)
	assert.Contains(t, skipped[0].Reason, "php")
	assert.Contains(t, skipped[0].Reason, "runtime_allow", "原因要说清是被哪一份白名单挡住，运维才有改的方向")
	assert.Equal(t, "exec.python", skipped[1].HandlerKey)
}

func TestAdhocProfiles_ShellRuntimeComesFromConfig(t *testing.T) {
	workspace := t.TempDir()
	cfg := adhocEnabled(configWith(workspace))
	cfg.Executors.Adhoc.ShellRuntime = "sh"

	profiles, _, err := AdhocProfiles(cfg)
	require.NoError(t, err)
	assert.Equal(t, "sh", findAdhocProfile(t, profiles, AdhocShellName).Runtime)
}

func TestAdhocProfiles_EmptyHostsAreAllowedForBuiltinOnly(t *testing.T) {
	workspace := t.TempDir()

	// 内置那条：url_hosts 留空（= 不限主机）是合法取值。
	profiles, _, err := AdhocProfiles(adhocEnabled(configWith(workspace)))
	require.NoError(t, err)
	require.Len(t, profiles, 4)

	// 对照：同一条放宽不许串到用户自建的 http 档位上——那份仍然必须有非空 allowed_hosts。
	_, err = BuildProfile(core.ExecutorCommand{
		Name: "no_hosts", Kind: "http", Method: "GET", URLTemplate: "https://api.example.com/x",
	}, core.DefaultConfig().Executors, PathWithinWorkspace)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "allowed_hosts must not be empty")
}

func TestAdhocProfiles_BuiltinHostsEntriesStillChecked(t *testing.T) {
	// 空列表放过，但写了的每一项仍按同一份主机判据走：内置档位不该比用户档位更宽松。
	workspace := t.TempDir()
	cfg := adhocEnabled(configWith(workspace))
	cfg.Executors.Adhoc.URLHosts = []string{"https://api.example.com"}

	_, _, err := AdhocProfiles(cfg)
	require.Error(t, err, "内置档位的主机写法错误也该在构造期暴露")
	assert.Contains(t, err.Error(), "must be a host, not a URL")
}

func TestAdhocProfiles_ExtensionRequirementCanBeTurnedOff(t *testing.T) {
	workspace := t.TempDir()
	cfg := adhocEnabled(configWith(workspace))
	off := false
	cfg.Executors.Adhoc.RequireExtension = &off

	profiles, _, err := AdhocProfiles(cfg)
	require.NoError(t, err)
	for _, profile := range profiles {
		if profile.Kind == KindScript {
			assert.Empty(t, profile.AdhocExtensions,
				"关掉要求时把空列表放进档位对象，判据只有一处（TASK-N05 那边不再读配置）")
		}
	}
}

func TestAdhocProfiles_PrivateRangeSwitchReachesProfile(t *testing.T) {
	workspace := t.TempDir()
	cfg := adhocEnabled(configWith(workspace))
	cfg.Executors.Adhoc.URLAllowPrivate = true

	profiles, _, err := AdhocProfiles(cfg)
	require.NoError(t, err)
	http := profiles[3]
	assert.False(t, http.DenyPrivate,
		"这一项是唯一能关掉地址守卫的口子，必须原样传到 HTTPRunner 手上")
}

// TestProbe_AdhocScriptOnlyChecksRuntime 落实 §3.2 与 §10 风险 5：
// 内置档位没有文件可探，探测结论只能关于解释器。
func TestProbe_AdhocScriptOnlyChecksRuntime(t *testing.T) {
	workspace := t.TempDir()
	self := selfExecutable(t)

	cfg := adhocEnabled(configWith(workspace))
	cfg.Executors.RuntimeAllow = []string{self, missingProgram}
	cfg.Executors.Adhoc.ShellRuntime = self

	profiles, _, err := AdhocProfiles(cfg)
	require.NoError(t, err)
	shell := findAdhocProfile(t, profiles, AdhocShellName)

	result := Probe(shell)
	assert.True(t, result.Available, "解释器在 PATH 里，内置档位就是可用的")
	assert.NotEmpty(t, result.Path)
	assert.Contains(t, result.Reason, "payload",
		"结论里要说清位置来自任务，否则运维看见可用却不知道它能跑什么")

	// 解释器不在 PATH 时，原因必须指向解释器而不是文件。
	cfg.Executors.Adhoc.ShellRuntime = missingProgram
	profiles, _, err = AdhocProfiles(cfg)
	require.NoError(t, err)
	shell = findAdhocProfile(t, profiles, AdhocShellName)

	result = Probe(shell)
	assert.False(t, result.Available)
	assert.Contains(t, result.Reason, "not found in PATH")
	assert.NotContains(t, result.Reason, "script file")
}

// findAdhocProfile 按档位名取一条构造结果：内置四条的顺序固定，
// 但过滤掉几条之后"第几个下标是哪条"会随用例而变，所以按名字取。
func findAdhocProfile(t *testing.T, profiles []*Profile, name string) *Profile {
	t.Helper()

	for _, profile := range profiles {
		if profile.Name == name {
			return profile
		}
	}
	t.Fatalf("档位 %q 没被构造出来", name)
	return nil
}

// listedByKey 从登记表的一次读表结果里按注册键取一行。
func listedProfile(t *testing.T, items []ListedProfile, key string) ListedProfile {
	t.Helper()

	for _, item := range items {
		if item.Profile.HandlerKey() == key {
			return item
		}
	}
	t.Fatalf("登记表里没有 %q", key)
	return ListedProfile{}
}

func TestNewRegistry_RegistersAdhocAndYieldsOnClash(t *testing.T) {
	t.Run("打开后四条进登记表", func(t *testing.T) {
		registry, err := NewRegistry(adhocEnabled(configWith(t.TempDir())), nil)
		require.NoError(t, err)

		keys := registry.Keys()
		for _, want := range adhocNames {
			assert.Contains(t, keys, want)
		}
		assert.Empty(t, registry.AdhocSkipped())

		for _, key := range adhocNames {
			profile, ok := registry.Lookup(key)
			require.True(t, ok)
			assert.True(t, profile.Adhoc)
			assert.Equal(t, SourceAdhoc, listedProfile(t, registry.List(), key).Source,
				"来源要标成 adhoc：它既不在配置文件里也不在档位文件里")
		}
	})

	t.Run("与配置侧档位撞名时内置让位", func(t *testing.T) {
		workspace := t.TempDir()
		script := declareFile(t, workspace, "scripts/deploy.sh")

		cfg := adhocEnabled(configWith(workspace, scriptUsingRuntime("bash", script)))
		cfg.Executors.Commands[0].Name = AdhocShellName // 用户自己建了一条叫 shell 的档位

		registry, err := NewRegistry(cfg, nil)
		require.NoError(t, err)

		profile, ok := registry.Lookup("exec." + AdhocShellName)
		require.True(t, ok)
		assert.False(t, profile.Adhoc, "生效的是用户那条")
		assert.Equal(t, "scripts/deploy.sh", profile.ScriptRel)

		skipped := registry.AdhocSkipped()
		require.Len(t, skipped, 1, "让位必须可见")
		assert.Equal(t, "exec.shell", skipped[0].HandlerKey)
		assert.Contains(t, skipped[0].Reason, "executors.commands")
	})
}

func TestApplyStore_KeepsAdhocAndLetsItYield(t *testing.T) {
	registry, err := NewRegistry(adhocEnabled(configWith(t.TempDir())), nil)
	require.NoError(t, err)
	require.Len(t, registry.Keys(), 4)

	t.Run("整批替换 store 侧不动内置四条", func(t *testing.T) {
		require.NoError(t, registry.ApplyStore([]StoreEntry{
			{Profile: storeProfile("nightly", time.Minute)},
		}))

		keys := registry.Keys()
		assert.Contains(t, keys, "exec.php", "热更新档位表不能把四条内置的一起冲掉：它们由整节开关管，不属于 store 侧")
		assert.Contains(t, keys, "exec.http")
		assert.Contains(t, keys, "exec.nightly")
	})

	t.Run("页面建的同名档位顶掉内置那条", func(t *testing.T) {
		require.NoError(t, registry.ApplyStore([]StoreEntry{
			{Profile: storeProfile(AdhocPHPName, 2*time.Minute)},
		}))

		profile, ok := registry.Lookup("exec.php")
		require.True(t, ok)
		assert.False(t, profile.Adhoc, "用户随后建的显式意图优先（设计文档 §D15）")

		assert.Equal(t, SourceStore, listedProfile(t, registry.List(), "exec.php").Source,
			"让位是看得见的：这一行的来源从 adhoc 变成 store")
	})
}

func TestRegister_BuiltInProfilesUseExecPoolAndCountSkips(t *testing.T) {
	workspace := t.TempDir()
	script := declareFile(t, workspace, "scripts/deploy.sh")

	cfg := adhocEnabled(configWith(workspace, scriptUsingRuntime("bash", script)))
	cfg.Executors.Commands[0].Name = AdhocShellName

	registry, err := NewRegistry(cfg, nil)
	require.NoError(t, err)

	registrar := newFakeRegistrar()
	result, err := Register(registrar, registry, cfg, nil, nil)
	require.NoError(t, err)

	assert.Equal(t, 4, result.Total, "配置侧 1 条 + 内置 3 条（exec.shell 被配置侧顶掉）")
	assert.Equal(t, 4, result.Registered)
	assert.Equal(t, 1, result.AdhocSkipped, "启动日志因此能说清少的那一条去哪了")

	for _, key := range []string{"exec.php", "exec.python", "exec.http"} {
		class, registered := registrar.classOf(key)
		require.True(t, registered, "%s 必须注册进调度器", key)
		assert.Equal(t, core.JobClassExec, class, "内置档位与普通档位同属执行池，不占共享名额")
	}
}

// TestAdhocProfileNamesPassCoreRules 跨包钉一次：四个内置档位名必须同时被
// core 的档位名规则接受（那个规则管的是档位文件与写端点，这里是同一批字符集）。
func TestAdhocProfileNamesPassCoreRules(t *testing.T) {
	for _, name := range []string{AdhocPHPName, AdhocPythonName, AdhocShellName, AdhocHTTPName} {
		require.NoError(t, core.ValidateProfileName(name), "内置档位名 %q 不合法会让整节打不开", name)
	}
}
