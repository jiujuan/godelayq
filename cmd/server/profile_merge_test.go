package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"godelayq/core"
	"godelayq/executor"
)

// 启动合并（TASK-W05）：executors.web_enabled 打开后，档位文件里的那份来源要和
// executors.commands 合成一张表，而且必须赶在崩溃恢复守卫之前登记完。
// 顺序是本文件最容易做反的一件事——晚注册的那批档位，它的崩溃现场查不到类别，
// 本该停在 paused 的任务会直接重排，而且不会有任何其它测试提醒。

// profileRunConfig 造一份"打开执行器、允许测试程序自身当解释器"的配置。
// 解释器用测试程序是既有手法（见 scriptRegistry）：它一定存在，探测结论必为可用，
// 用例因此不依赖本机装了 node 还是 php。
func profileRunConfig(t *testing.T) (core.Config, string) {
	t.Helper()

	executable, err := os.Executable()
	require.NoError(t, err)

	workspace := t.TempDir()
	cfg := core.DefaultConfig()
	cfg.Executors.Enabled = true
	cfg.Executors.WebEnabled = true
	cfg.Executors.Workspace = workspace
	cfg.Executors.RuntimeAllow = append(cfg.Executors.RuntimeAllow, executable)
	cfg.Executors.Output.Dir = filepath.Join(t.TempDir(), "exec")
	cfg.Executors.ProfilesPath = filepath.Join(t.TempDir(), "exec-profiles.json")
	return cfg, workspace
}

// declareStoredRecord 在 workspace 里造一条真实脚本，并给出它在档位文件里的记录形态。
func declareStoredRecord(t *testing.T, workspace, name string) core.ExecutorProfileRecord {
	t.Helper()

	executable, err := os.Executable()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(workspace, "scripts"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "scripts", name+".mjs"),
		[]byte("console.log('ok')\n"), 0o600))

	return core.NewExecutorProfileRecord(core.ExecutorCommand{
		Name:    name,
		Kind:    "script",
		Runtime: executable,
		Script:  "scripts/" + name + ".mjs",
	})
}

// declareStoredCommand 与 declareStoredRecord 同源，只是交给 executors.commands 用。
func declareStoredCommand(t *testing.T, workspace, name string) core.ExecutorCommand {
	record := declareStoredRecord(t, workspace, name)
	cmd, err := record.Command()
	require.NoError(t, err)
	return cmd
}

// profileStoreWith 交回一个"装着这些记录"的真实档位文件存储，
// 让 run() 走的是生产那条 List() 路径而不是凭空构造的切片。
func profileStoreWith(t *testing.T, records ...core.ExecutorProfileRecord) func(core.Config) (core.ExecutorProfileStore, error) {
	return func(core.Config) (core.ExecutorProfileStore, error) {
		store, err := core.NewJSONFileExecutorProfileStore(filepath.Join(t.TempDir(), "exec-profiles.json"))
		if err != nil {
			t.Fatalf("open profile store: %v", err)
		}
		for _, record := range records {
			if err := store.Save(record); err != nil {
				t.Fatalf("save profile %q: %v", record.Name, err)
			}
		}
		return store, nil
	}
}

// runWithProfileStore 跑一次完整装配，档位文件由 store 那个闭包顶替（不碰配置文件里的路径）。
func runWithProfileStore(t *testing.T, cfg core.Config, store func(core.Config) (core.ExecutorProfileStore, error)) (*spyScheduler, string, error) {
	t.Helper()

	stub := newStubStore()
	scheduler := newSpyScheduler()
	server := newFakeServer()
	var logs strings.Builder

	err := run(runtimeDeps{
		config:                  cfg,
		newStore:                func() (core.Store, error) { return stub, nil },
		newScheduler:            func(core.Store, core.RetryPolicy, *core.EventBus) schedulerAPI { return scheduler },
		newExecutorRegistry:     executor.NewRegistry,
		newExecutorProfileStore: store,
		newArtifactStore:        artifactStoreFromConfig,
		newObservabilityDB:      unopenedObservabilityDB(t),
		newServer: func(schedulerAPI, core.Store, string, *executor.Registry,
			*executor.ArtifactStore, *observabilityAPI, *profileStoreAPI) (serverAPI, error) {
			return server, nil
		},
		notifySignals: func(ch chan<- os.Signal, sig ...os.Signal) {
			go func() { ch <- syscall.SIGTERM }()
		},
		timeout: 20 * time.Millisecond,
		logger:  newCaptureLogger(&logs),
	})

	return scheduler, logs.String(), err
}

func TestRun_MergesStoredProfilesIntoTheScheduler(t *testing.T) {
	cfg, workspace := profileRunConfig(t)
	// config 侧声明 alpha，文件里也有一条 alpha 加一条 beta：alpha 以配置为准
	cfg.Executors.Commands = []core.ExecutorCommand{declareStoredCommand(t, workspace, "alpha")}

	scheduler, logs, err := runWithProfileStore(t, cfg, profileStoreWith(t,
		declareStoredRecord(t, workspace, "alpha"),
		declareStoredRecord(t, workspace, "beta"),
	))
	require.NoError(t, err)

	keys := scheduler.registeredKeys()
	assert.Contains(t, keys, "exec.alpha")
	assert.Contains(t, keys, "exec.beta", "文件里的新档位要在没有端点参与的情况下注册上")
	// 撞名只留一条：注册表里 exec.alpha 的类别来自 config 那条，两条来源共用一个键
	assert.Equal(t, core.JobClassExec, scheduler.classes["exec.alpha"])
	assert.Contains(t, logs, `msg="executor handlers registered" total=2 registered=2 unavailable=0 degraded=1`,
		"启动日志要说清注册了几条、有几条白写在文件里")
}

// 顺序断言（卡 §5.5）：注册早于守卫、守卫早于 Start。
// 只做前一半的话，"文件里的档位"在崩溃恢复时会被守卫当成"查不到类别"而放行重排。
func TestRun_StoredProfilesAreRegisteredBeforeTheRestoreGuard(t *testing.T) {
	cfg, workspace := profileRunConfig(t)
	cfg.Executors.RestorePolicy = "pause"

	scheduler, _, err := runWithProfileStore(t, cfg, profileStoreWith(t,
		declareStoredRecord(t, workspace, "beta")))
	require.NoError(t, err)

	registerAt := scheduler.callIndex("register:exec.beta")
	guardAt := scheduler.callIndex("restore_guard")
	startAt := scheduler.callIndex("start")
	if registerAt < 0 || guardAt < 0 || startAt < 0 {
		t.Fatalf("三个关键调用都要发生过：%v", scheduler.calls)
	}
	assert.Less(t, registerAt, guardAt, "档位注册必须早于崩溃恢复守卫")
	assert.Less(t, guardAt, startAt, "守卫必须早于 Start（Start 的第一步是 Restore）")

	// 守卫真的认得这条 store 档位：崩溃瞬间仍在执行的它停在 paused 上等人确认
	guard, beforeStart := scheduler.installedGuard()
	require.NotNil(t, guard, "打开执行器且策略为 pause 时要装守卫")
	assert.True(t, beforeStart)
	status, hold := guard(core.JobSnapshot{ID: "crashed", Type: "exec.beta", Status: int(core.StatusRunning)})
	assert.True(t, hold, "store 档位的崩溃任务也要被停住，不能因为来源不同而漏判")
	assert.Equal(t, core.StatusPaused, status)
}

func TestRun_InvalidStoredRecordIsSkippedAndLogged(t *testing.T) {
	cfg, workspace := profileRunConfig(t)

	broken := core.NewExecutorProfileRecord(core.ExecutorCommand{
		Name: "broken",
		Kind: "script",
		// 缺 runtime：字段组合非法，归合并那一步跳过
		Script: "scripts/broken.mjs",
	})

	scheduler, logs, err := runWithProfileStore(t, cfg, profileStoreWith(t,
		broken, declareStoredRecord(t, workspace, "survivor")))

	require.NoError(t, err, "一条手改坏的记录不该让整个进程起不来")
	keys := scheduler.registeredKeys()
	assert.Contains(t, keys, "exec.survivor")
	assert.NotContains(t, keys, "exec.broken")
	assert.Contains(t, logs, "stored executor profile not registered")
	assert.Contains(t, logs, "profile=broken")
	assert.Contains(t, logs, "level=WARN")
}

// 文件损坏挡住启动，且文案要给出自救路径（卡 §3.5、待拍板 P2 取的答案）。
// 这里用真实的生产闭包，而不是替身：文案本身就是本卡的交付物。
func TestRun_CorruptProfileFileStopsStartupWithSelfHelp(t *testing.T) {
	cfg, _ := profileRunConfig(t)
	path := filepath.Join(t.TempDir(), "profiles", "exec-profiles.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
	require.NoError(t, os.WriteFile(path, []byte("{ this is not json"), 0o600))
	cfg.Executors.ProfilesPath = path

	profiles := defaultRuntimeDeps(cfg, "", nil, quietLogger()).newExecutorProfileStore
	scheduler, _, err := runWithProfileStore(t, cfg, profiles)

	require.Error(t, err)
	assert.Contains(t, err.Error(), filepath.Base(path), "错误要指出是哪个文件")
	assert.Contains(t, err.Error(), "delete it", "要给出删掉该文件即可退回的自救路径")
	assert.Contains(t, err.Error(), "executors.commands")

	assert.Zero(t, scheduler.startCalls, "挡住启动就不该把调度器跑起来")
	assert.Empty(t, scheduler.registeredKeys())
}

func TestRun_RealProfileClosureToleratesMissingFile(t *testing.T) {
	cfg, _ := profileRunConfig(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "exec-profiles.json")
	cfg.Executors.ProfilesPath = path

	store, err := defaultRuntimeDeps(cfg, "", nil, quietLogger()).newExecutorProfileStore(cfg)
	require.NoError(t, err, "还没人在页面上建过档位是正常状态")
	records, err := store.List()
	require.NoError(t, err)
	assert.Empty(t, records)

	_, statErr := os.Stat(path)
	assert.True(t, errors.Is(statErr, os.ErrNotExist), "读一个不存在的档位文件不该把它建出来")
}

// 默认关闭（DoD 第一条）：不构造存储、不读文件、连父目录都不碰，
// 配置侧档位的注册与本卡之前一字不差。
func TestRun_WebDisabledNeverTouchesTheProfilesPath(t *testing.T) {
	cfg, workspace := profileRunConfig(t)
	cfg.Executors.WebEnabled = false
	cfg.Executors.Commands = []core.ExecutorCommand{declareStoredCommand(t, workspace, "alpha")}
	dir := t.TempDir()
	cfg.Executors.ProfilesPath = filepath.Join(dir, "nested", "exec-profiles.json")

	// 用真实的生产闭包而不是替身：本卡要断言的是"它一次都没被调用到"，
	// 而闭包一旦被调用就会把 nested 目录建出来。
	profiles := defaultRuntimeDeps(cfg, "", nil, quietLogger()).newExecutorProfileStore
	scheduler, _, err := runWithProfileStore(t, cfg, profiles)
	require.NoError(t, err)

	_, statErr := os.Stat(filepath.Dir(cfg.Executors.ProfilesPath))
	assert.True(t, errors.Is(statErr, os.ErrNotExist), "关闭状态下不该创建档位文件的目录")
	assert.Contains(t, scheduler.registeredKeys(), "exec.alpha", "配置侧档位照旧注册")
}

// web_enabled 打开却没有这个依赖：与观测层同一口径，启动期就报错而不是静默不读。
func TestRun_MissingProfileDependencyIsRejected(t *testing.T) {
	cfg, _ := profileRunConfig(t)

	scheduler, _, err := runWithProfileStore(t, cfg, nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "runtime dependencies are incomplete")
	assert.Zero(t, scheduler.startCalls)
}

// 档位条目往关闭状态的登记表里合是编程错误：那张表会"接口看得见、调度器跑不了"。
// 正常装配走不到这里（executors.web_enabled 要求 enabled=true，core 的配置校验已经挡住），
// 这条钉的是接线那层的兜底。
func TestRun_StoredProfilesCannotBeAppliedWhileExecutorsAreDisabled(t *testing.T) {
	cfg, workspace := profileRunConfig(t)
	cfg.Executors.Enabled = false
	cfg.Executors.Output.Dir = ""

	_, _, err := runWithProfileStore(t, cfg, profileStoreWith(t,
		declareStoredRecord(t, workspace, "beta")))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "executors are disabled")
}

// 装配方交给服务的两面必须同时在场：api 侧的 requireExecutorProfiles 判的是"两个都非 nil"，
// 只给一个的话写端点会 503，而 503 的文案要说的是"没装配"——那比"少给了一半"更难查。
func TestRun_ProfileDependenciesReachTheServerTogether(t *testing.T) {
	cfg, workspace := profileRunConfig(t)

	var got *profileStoreAPI
	records := profileStoreWith(t, declareStoredRecord(t, workspace, "beta"))
	err := runWithCapturingServer(t, cfg, records, func(profiles *profileStoreAPI) { got = profiles })
	require.NoError(t, err)

	require.NotNil(t, got, "web_enabled=true 的部署要把档位存储与同步器一起交给服务")
	require.NotNil(t, got.store)
	require.NotNil(t, got.applier)
}

func TestRun_ProfileDependenciesAreAbsentWhenDisabled(t *testing.T) {
	cfg, workspace := profileRunConfig(t)
	cfg.Executors.WebEnabled = false
	cfg.Executors.Commands = []core.ExecutorCommand{declareStoredCommand(t, workspace, "alpha")}

	var got *profileStoreAPI
	called := false
	err := runWithCapturingServer(t, cfg, func(core.Config) (core.ExecutorProfileStore, error) {
		called = true
		return nil, errors.New("must not be called")
	}, func(profiles *profileStoreAPI) { got = profiles })
	require.NoError(t, err)

	assert.False(t, called, "关闭状态下不该打开档位文件")
	assert.Nil(t, got, "关闭状态下服务侧不该拿到任何档位写依赖")
}

// TASK-R04 把 Applier 的构造条件放宽到 `web_enabled || (reload.enabled && enabled)`。
// 这一支是全新的装配逻辑，判三件事：
//   - 档位文件的构造闭包一次都没被调用（web_enabled 才是它的条件，reload 不搭车）；
//   - 服务拿到的 profileStoreAPI 里 store 是 nil 而 applier 非 nil——
//     这正是"这种部署不打开档位文件，但 executors.commands 仍能热更"的形状；
//   - 启动期的注册链照常把配置侧档位登记上，放宽条件没碰到别的东西。
func TestRun_ReloadWithoutWebBuildsConfigOnlyApplier(t *testing.T) {
	cfg, workspace := profileRunConfig(t)
	cfg.Executors.WebEnabled = false
	cfg.Reload.Enabled = true
	cfg.Executors.Commands = []core.ExecutorCommand{declareStoredCommand(t, workspace, "alpha")}
	// 只打开开关即可：DefaultConfig 给 reload.debounce 的就是默认那份合并窗口，
	// Validate 只拒绝"写了但小于下界"的取值，这里不需要额外配。
	require.NoError(t, cfg.Validate())

	var got *profileStoreAPI
	called := false
	scheduler, err := runWithCapturingServerAndScheduler(t, cfg,
		func(core.Config) (core.ExecutorProfileStore, error) {
			called = true
			return nil, errors.New("must not be called")
		}, func(profiles *profileStoreAPI) { got = profiles })
	require.NoError(t, err)

	assert.False(t, called, "reload 打开不该把档位文件也一起打开")
	assert.Contains(t, scheduler.registeredKeys(), "exec.alpha", "配置侧档位照旧注册")

	require.NotNil(t, got, "reload 打开的部署要拿到一份配置侧热更依赖")
	assert.Nil(t, got.store, "这种部署没有档位存储：写端点继续靠'两个都在场'的判定回 503")
	require.NotNil(t, got.applier, "applier 在场才谈得上运行期换 executors.commands")
}

// runWithCapturingServer 与 runWithProfileStore 同一套替身，只是把 newServer 收到的
// 档位依赖交给回调。
func runWithCapturingServer(t *testing.T, cfg core.Config,
	store func(core.Config) (core.ExecutorProfileStore, error),
	capture func(*profileStoreAPI)) error {
	t.Helper()

	_, err := runWithCapturingServerAndScheduler(t, cfg, store, capture)
	return err
}

// runWithCapturingServerAndScheduler 在上面那份之上多交回替身调度器：
// 判装配放宽时既要看见服务拿到了哪两面，也要看见调度器里的注册结果。
func runWithCapturingServerAndScheduler(t *testing.T, cfg core.Config,
	store func(core.Config) (core.ExecutorProfileStore, error),
	capture func(*profileStoreAPI)) (*spyScheduler, error) {
	t.Helper()

	scheduler := newSpyScheduler()
	err := run(runtimeDeps{
		config:                  cfg,
		newStore:                func() (core.Store, error) { return newStubStore(), nil },
		newScheduler:            func(core.Store, core.RetryPolicy, *core.EventBus) schedulerAPI { return scheduler },
		newExecutorRegistry:     executor.NewRegistry,
		newExecutorProfileStore: store,
		newArtifactStore:        artifactStoreFromConfig,
		newObservabilityDB:      unopenedObservabilityDB(t),
		newServer: func(_ schedulerAPI, _ core.Store, _ string, _ *executor.Registry,
			_ *executor.ArtifactStore, _ *observabilityAPI, profiles *profileStoreAPI) (serverAPI, error) {
			capture(profiles)
			return newFakeServer(), nil
		},
		notifySignals: func(ch chan<- os.Signal, sig ...os.Signal) {
			go func() { ch <- syscall.SIGTERM }()
		},
		timeout: 20 * time.Millisecond,
		logger:  quietLogger(),
	})
	return scheduler, err
}
