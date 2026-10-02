package executor

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"godelayq/core"
)

// Applier 守的是"页面写完到调度器可用"这一段：
// 顺序（整表替换在先、同步处理函数在后）、范围（只动 store 来源与 exec. 前缀）、
// 以及并发写入时后一次 Apply 必须看到前一次的落盘结果。

type applierFixture struct {
	workspace string
	executor  core.ExecutorsConfig
	store     core.ExecutorProfileStore
	registry  *Registry
	syncer    *fakeRegistrar
	applier   *Applier
}

// newApplierFixture 造一套"档位文件 + 登记表 + 替身调度器"。
// 解释器用测试程序自身（与 scriptRegistry 同一手法），探测必为可用，
// 用例因此不依赖本机装了 node 还是 python。
// configNames 是 executors.commands 里声明的配置侧档位；装配时它们已经登记进调度器。
func newApplierFixture(t *testing.T, configNames ...string) *applierFixture {
	t.Helper()

	executable := selfExecutable(t)
	workspace := t.TempDir()
	cfg := configAllowing(workspace, []string{executable})
	for _, name := range configNames {
		cfg.Executors.Commands = append(cfg.Executors.Commands, namedScript(t, workspace, name))
	}

	registry, err := NewRegistry(cfg, quietLogger())
	require.NoError(t, err)

	store, err := core.NewJSONFileExecutorProfileStore(filepath.Join(t.TempDir(), "exec-profiles.json"))
	require.NoError(t, err)

	syncer := newFakeRegistrar()
	for _, key := range registry.Keys() {
		if source, ok := registry.SourceOf(key); ok && source == SourceConfig {
			profile, _ := registry.Lookup(key)
			syncer.RegisterHandlerClass(key, Handler(profile, nil, cfg.Normalized().Executors, quietLogger()),
				core.JobClassExec)
		}
	}

	applier, err := NewApplier(store, syncer, registry, nil, quietLogger())
	require.NoError(t, err)

	return &applierFixture{
		workspace: workspace,
		executor:  cfg.Normalized().Executors,
		store:     store,
		registry:  registry,
		syncer:    syncer,
		applier:   applier,
	}
}

// save 往档位文件里写一条脚本档位（脚本文件真实创建在 fixture 的 workspace 里）。
func (f *applierFixture) save(t *testing.T, name string) {
	t.Helper()
	require.NoError(t, f.store.Save(core.NewExecutorProfileRecord(namedScript(t, f.workspace, name))))
}

// newConfigApplierFixture 是 TASK-R04 的 ApplyConfig 用例用的 fixture：在 newApplierFixture
// 之上只多预登记一个不带 exec. 前缀的普通任务键 payment_check，用来断言
// 任何 config 路径都不碰它（"摘除只看 exec. 前缀"的既有承诺）。
//
// config 键的处理函数仍按启动语义预登记（executor.Register 在装配期就是这么干的），
// 所以改一条既有条位的闭包时 Added/Removed 都空、只重登记一次。既有用例继续走
// newApplierFixture，本构造函数只增不改，applier_test.go 的断言一字未动。
func newConfigApplierFixture(t *testing.T, configNames ...string) *applierFixture {
	t.Helper()

	f := newApplierFixture(t, configNames...)
	f.syncer.RegisterHandlerClass("payment_check",
		func(context.Context, *core.Job) error { return nil }, core.JobClassDefault)
	return f
}

// configWithCommands 造一份"冻结的非 Commands 取值 + 给定新命令列表"的合成配置：
// 这正是 Applier.ApplyConfig 第 2 步拿去做 LoadProfiles 严格校验的那一份
// （用例与实现共用同一个构造点，避免造出实现拿不到的配置）。
// 每个 name 对应的脚本文件真实创建在 fixture 的 workspace 里，探测必然可用。
func (f *applierFixture) configWithCommands(t *testing.T, names ...string) core.Config {
	t.Helper()

	exec := f.executor // 冻结的 workspace / runtime_allow / timeout 取值
	exec.Commands = make([]core.ExecutorCommand, 0, len(names))
	for _, name := range names {
		exec.Commands = append(exec.Commands, namedScript(t, f.workspace, name))
	}
	return core.Config{Executors: exec}
}

func TestApplier_NewApplierRequiresItsDeps(t *testing.T) {
	store, err := core.NewJSONFileExecutorProfileStore(filepath.Join(t.TempDir(), "exec-profiles.json"))
	require.NoError(t, err)
	registry, err := NewRegistry(configWith(t.TempDir()), quietLogger())
	require.NoError(t, err)
	syncer := newFakeRegistrar()

	cases := []struct {
		name     string
		store    core.ExecutorProfileStore
		syncer   HandlerSync
		registry *Registry
		want     string
	}{
		{name: "store", store: nil, syncer: syncer, registry: registry, want: "profile store"},
		{name: "syncer", store: store, syncer: nil, registry: registry, want: "handler registry"},
		{name: "registry", store: store, syncer: syncer, registry: nil, want: "profile registry"},
	}
	for _, tc := range cases {
		_, err := NewApplier(tc.store, tc.syncer, tc.registry, nil, nil)
		require.Error(t, err, tc.name)
		assert.Contains(t, err.Error(), tc.want)
	}
}

func TestApplier_RegistersAndUnregistersStoreProfiles(t *testing.T) {
	fixture := newApplierFixture(t)
	fixture.save(t, "beta")

	result, err := fixture.applier.Apply()
	require.NoError(t, err)
	assert.Equal(t, []string{"exec.beta"}, result.Added)
	assert.Empty(t, result.Removed)
	assert.Equal(t, 1, result.Stored)
	assert.Zero(t, result.Config)

	_, ok := fixture.syncer.LookupHandler("exec.beta")
	require.True(t, ok, "写完文件并 Apply 之后调度器就该认得这个类型")
	class, ok := fixture.syncer.classOf("exec.beta")
	require.True(t, ok)
	assert.Equal(t, core.JobClassExec, class, "store 档位同样落执行器池")

	require.NoError(t, fixture.store.Delete("beta"))
	result, err = fixture.applier.Apply()
	require.NoError(t, err)
	assert.Equal(t, []string{"exec.beta"}, result.Removed)

	_, ok = fixture.syncer.LookupHandler("exec.beta")
	assert.False(t, ok)
	_, ok = fixture.syncer.classOf("exec.beta")
	assert.False(t, ok, "摘除必须连类别一起删（W04 的成对删）")
}

// 配置侧的处理函数归启动时的 Register 管：文件里出现同名条目只降级它自己，
// 配置那条既不摘也不重建。
func TestApplier_NeverTouchesConfigSourcedHandlers(t *testing.T) {
	fixture := newApplierFixture(t, "alpha")
	fixture.syncer.writes = nil

	fixture.save(t, "alpha")
	result, err := fixture.applier.Apply()
	require.NoError(t, err)

	assert.Empty(t, result.Added)
	assert.Empty(t, result.Removed, "降级条目不该把配置那条的键摘掉")
	assert.Equal(t, 1, result.Degraded)
	assert.Zero(t, result.Stored)
	assert.Equal(t, 1, result.Config)

	assert.NotContains(t, fixture.syncer.writeLog(), "exec.alpha", "配置侧的处理函数一次都不该重建")
	_, ok := fixture.syncer.LookupHandler("exec.alpha")
	assert.True(t, ok)
}

func TestApplier_UpdateReplacesTheProfileAndIsIdempotent(t *testing.T) {
	fixture := newApplierFixture(t)

	record := core.NewExecutorProfileRecord(namedScript(t, fixture.workspace, "beta"))
	record.Timeout = "30s"
	require.NoError(t, fixture.store.Save(record))
	first, err := fixture.applier.Apply()
	require.NoError(t, err)
	require.Equal(t, []string{"exec.beta"}, first.Added)

	profile, ok := fixture.registry.Lookup("exec.beta")
	require.True(t, ok)
	require.Equal(t, "30s", profile.Timeout.String())

	// 同一条档位换个超时再 Apply：键位没变，所以既不算新增也不算摘除，
	// 但登记表与调度器拿到的必须是新的一条。
	record.Timeout = "1m"
	require.NoError(t, fixture.store.Save(record))
	second, err := fixture.applier.Apply()
	require.NoError(t, err)
	assert.Empty(t, second.Added)
	assert.Empty(t, second.Removed)

	profile, _ = fixture.registry.Lookup("exec.beta")
	assert.Equal(t, "1m0s", profile.Timeout.String())

	third, err := fixture.applier.Apply()
	require.NoError(t, err)
	assert.Empty(t, third.Added)
	assert.Empty(t, third.Removed)
}

func TestApplier_ReportsWarningsAndSkipsBadRecords(t *testing.T) {
	fixture := newApplierFixture(t)

	broken := core.NewExecutorProfileRecord(core.ExecutorCommand{
		Name: "broken",
		Kind: "script",
		// 缺 runtime：字段组合非法
		Script: "scripts/broken.mjs",
	})
	require.NoError(t, fixture.store.Save(broken))
	fixture.save(t, "ok")

	result, err := fixture.applier.Apply()
	require.NoError(t, err)
	require.Len(t, result.Warnings, 1)
	assert.Equal(t, "broken", result.Warnings[0].Name)
	assert.Equal(t, []string{"exec.ok"}, result.Added)
}

// ApplyStore 失败时调度器一行都不动：那张表还是旧的，处理函数也不该被改。
func TestApplier_StoreFailureLeavesSchedulerAlone(t *testing.T) {
	fixture := newApplierFixture(t, "alpha")
	fixture.save(t, "beta")
	syncer := fixture.syncer
	syncer.writes = nil

	// 执行器关闭的登记表：ApplyStore 会拒（W03 的边界），此时同步一步都不该发生
	closed, err := NewRegistry(core.DefaultConfig(), quietLogger())
	require.NoError(t, err)
	require.False(t, closed.Enabled())
	closedApplier, err := NewApplier(fixture.store, syncer, closed, nil, quietLogger())
	require.NoError(t, err)

	_, err = closedApplier.Apply()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "executors are disabled")

	assert.Equal(t, []string{"exec.alpha"}, syncer.registeredKeys(), "生效失败不该动调度器")
	assert.Empty(t, syncer.writeLog())
	_, ok := syncer.LookupHandler("exec.beta")
	assert.False(t, ok)
}

func TestApplier_ValidateUsesTheRegistryConfig(t *testing.T) {
	fixture := newApplierFixture(t)
	executable := selfExecutable(t)

	// 宽松模式：越界路径也接受（D5 的口子）
	outside := t.TempDir()
	absScript := filepath.Join(outside, declareFile(t, outside, "x.mjs"))
	profile, probe, err := fixture.applier.Validate(core.ExecutorCommand{
		Name: "elsewhere", Kind: "script", Runtime: executable, Script: absScript,
	})
	require.NoError(t, err)
	assert.Equal(t, filepath.Clean(absScript), profile.ScriptPath)
	assert.True(t, probe.Available, probe.Reason)

	_, _, err = fixture.applier.Validate(core.ExecutorCommand{
		Name: "no_runtime", Kind: "script", Script: "scripts/x.mjs",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no_runtime")

	// 探测不可用不是错误：那是可以保存的状态（设计文档 §5.3）
	_, probe, err = fixture.applier.Validate(core.ExecutorCommand{
		Name: "gone", Kind: "script", Runtime: executable, Script: "scripts/not-there.mjs",
	})
	require.NoError(t, err)
	assert.False(t, probe.Available)
	assert.NotEmpty(t, probe.Reason)
}

// Validate 用的是登记表所持的那份 executors 取值，而不是调用方再传一遍配置：
// 两条路径（启动与页面）读到的 workspace 与 runtime_allow 因此必然是同一个。
func TestApplier_ValidateHonorsTheRuntimeAllow(t *testing.T) {
	fixture := newApplierFixture(t)

	// fixture 的白名单里只有测试程序自身：node 在默认列表里，missingProgram 不在任何列表里
	_, _, err := fixture.applier.Validate(core.ExecutorCommand{
		Name: "unlisted", Kind: "script", Runtime: missingProgram, Script: "scripts/x.mjs",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "runtime_allow")

	// 同一条改用装配方给的白名单程序就合法：证明判据来自登记表那份配置
	_, _, err = fixture.applier.Validate(core.ExecutorCommand{
		Name: "listed", Kind: "script", Runtime: selfExecutable(t), Script: "scripts/x.mjs",
	})
	require.NoError(t, err)
}

// 并发 Apply：writeMu 串行化整段，-race 下不该有数据竞争，
// 而且任何一次 Apply 之后调度器都还是"文件里那批"的一致投影。
func TestApplier_ConcurrentAppliesStayConsistent(t *testing.T) {
	fixture := newApplierFixture(t)
	for _, name := range []string{"a", "b", "c"} {
		fixture.save(t, name)
	}

	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := fixture.applier.Apply(); err != nil {
				t.Errorf("apply failed: %v", err)
			}
		}()
	}
	wg.Wait()

	assert.Equal(t, []string{"exec.a", "exec.b", "exec.c"}, fixture.syncer.registeredKeys(),
		"重复 Apply 之后注册表应当正好是文件里那三条")
}
