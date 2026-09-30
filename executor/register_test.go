package executor

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"godelayq/core"
)

// quietLogger 吞掉不打算断言的日志，避免无关行混进测试输出。
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// 装配方传进来的对象必须满足这个接口，编译期固定住，避免改了方法名要到 cmd 里才发现。
var _ Registrar = (*core.Scheduler)(nil)

type fakeRegistrar struct {
	handlers map[string]core.Handler
	classes  map[string]core.JobClass
}

func newFakeRegistrar(keys ...string) *fakeRegistrar {
	f := &fakeRegistrar{
		handlers: make(map[string]core.Handler),
		classes:  make(map[string]core.JobClass),
	}
	for _, key := range keys {
		f.handlers[key] = func(context.Context, *core.Job) error { return nil }
	}
	return f
}

func (f *fakeRegistrar) RegisterHandler(jobType string, handler core.Handler) {
	f.RegisterHandlerClass(jobType, handler, core.JobClassDefault)
}

// RegisterHandlerClass 把类别一起记下来：E13 的分池接线靠这个值，
// 注册链路只调用 RegisterHandler 的话，档位就会退回共享池。
func (f *fakeRegistrar) RegisterHandlerClass(jobType string, handler core.Handler, class core.JobClass) {
	f.handlers[jobType] = handler
	f.classes[jobType] = class
}

func (f *fakeRegistrar) LookupHandler(jobType string) (core.Handler, bool) {
	handler, ok := f.handlers[jobType]
	return handler, ok
}

// registeredKeys 返回假注册表里的键，按字典序，便于与登记表的输出顺序直接比较。
func (f *fakeRegistrar) registeredKeys() []string {
	keys := make([]string, 0, len(f.handlers))
	for key := range f.handlers {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func TestRegister_AllProfiles(t *testing.T) {
	workspace := t.TempDir()
	executable := selfExecutable(t)
	script := declareFile(t, workspace, "scripts/run.mjs")

	named := func(name, runtime string) core.ExecutorCommand {
		cmd := scriptUsingRuntime(runtime, script)
		cmd.Name = name
		return cmd
	}

	cfg := configAllowing(workspace, []string{executable, missingProgram},
		named("alpha", executable),
		named("beta", executable),
		named("zeta", missingProgram), // 探测失败：仍然要注册
	)
	registry, err := NewRegistry(cfg, quietLogger())
	require.NoError(t, err)

	registrar := newFakeRegistrar()
	var logs bytes.Buffer
	result, err := Register(registrar, registry, cfg, nil, slog.New(slog.NewTextHandler(&logs, nil)))
	require.NoError(t, err)

	assert.Equal(t, Registration{Total: 3, Registered: 3, Unavailable: 1}, result)
	assert.Equal(t, []string{"exec.alpha", "exec.beta", "exec.zeta"}, registrar.registeredKeys())

	for _, key := range []string{"exec.alpha", "exec.beta", "exec.zeta"} {
		handler, ok := registrar.handlers[key]
		require.True(t, ok, "%s 应该已注册", key)
		require.NotNil(t, handler)
		// 探测不可用的档位同样登记为 exec 类：它归执行器池管，
		// 落到共享池会让普通任务和一次注定失败的执行抢名额（TASK-E13 §3.2）。
		assert.Equal(t, core.JobClassExec, registrar.classes[key], "%s 的执行类别", key)
	}

	output := logs.String()
	assert.Contains(t, output, "executor handlers registered")
	assert.Contains(t, output, "total=3")
	assert.Contains(t, output, "registered=3")
	assert.Contains(t, output, "unavailable=1")
}

func TestRegister_KeyConflict(t *testing.T) {
	workspace := t.TempDir()

	cfg := configAllowing(workspace, []string{selfExecutable(t)},
		namedScript(t, workspace, "alpha"),
		namedScript(t, workspace, "beta"),
	)
	registry, err := NewRegistry(cfg, nil)
	require.NoError(t, err)

	// 模拟代码里已经占用过这个键的处理函数
	registrar := newFakeRegistrar("exec.beta")
	result, err := Register(registrar, registry, cfg, nil, nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "exec.beta")
	assert.Contains(t, err.Error(), "already registered")

	// 语义在这里固定下来：查重在写入之前全部做完，因此冲突时一个都不写，
	// 不会出现"注册了一半的档位"这种没人能解释的状态。
	assert.Equal(t, 2, result.Total)
	assert.Equal(t, 0, result.Registered)
	assert.Equal(t, []string{"exec.beta"}, registrar.registeredKeys())
}

func TestRegister_EmptyRegistry(t *testing.T) {
	cfg := configWith(t.TempDir())
	cfg.Executors.Enabled = false

	registry, err := NewRegistry(cfg, nil)
	require.NoError(t, err)

	var logs bytes.Buffer
	registrar := newFakeRegistrar()
	result, err := Register(registrar, registry, cfg, nil, slog.New(slog.NewTextHandler(&logs, nil)))
	require.NoError(t, err)

	assert.Equal(t, Registration{}, result)
	assert.Empty(t, registrar.handlers)
	// 关闭状态下登记表为空是正常情况，启动日志里不该出现注册行或告警
	assert.Empty(t, logs.String())
}

func TestRegister_LogsWarnWithoutProfiles(t *testing.T) {
	cfg := configWith(t.TempDir())
	registry, err := NewRegistry(cfg, nil)
	require.NoError(t, err)
	require.True(t, registry.Enabled())
	require.Empty(t, registry.Keys())

	var logs bytes.Buffer
	result, err := Register(newFakeRegistrar(), registry, cfg, nil, slog.New(slog.NewTextHandler(&logs, nil)))
	require.NoError(t, err)

	assert.Equal(t, Registration{}, result)
	output := logs.String()
	assert.Contains(t, output, "level=WARN")
	assert.Contains(t, output, "executors are enabled but no profile is declared")
}

// TestRegister_HandlerRunsTheProfile 确认注册链路已从占位实现切到真实执行器（卡片 §5.11）。
//
// 断言方式是跑一次并检查摘要与产物文件：错误文本里有没有 "not implemented" 只能证明
// 占位实现还在，证明不了真实执行可用。
func TestRegister_HandlerRunsTheProfile(t *testing.T) {
	cfg := configWith(t.TempDir(), shellCommand(t, "echo registered"))
	registry, err := NewRegistry(cfg, quietLogger())
	require.NoError(t, err)

	store := artifactStoreFor(t, core.DefaultExecMaxOutputBytes, 0, nil)
	registrar := newFakeRegistrar()
	_, err = Register(registrar, registry, cfg, store, quietLogger())
	require.NoError(t, err)

	handler, ok := registrar.handlers["exec.runner_case"]
	require.True(t, ok)

	job := &core.Job{ID: "job-registered", Name: "exec.runner_case", Attempts: 1}
	require.NoError(t, handler(context.Background(), job))

	require.NotNil(t, job.Exec, "真实执行器要留下执行摘要")
	assert.Equal(t, "runner_case", job.Exec.Profile)
	assert.Equal(t, "registered", strings.TrimSpace(job.Exec.Preview))
	assert.True(t, store.Exists(job.ID, job.Attempts))
}

// TestRegister_WarnsWhenAddressPolicyDisabled 钉住 TASK-E15 §9 要求的启动期 warn：
// 关掉地址防线的档位必须在启动日志里留一行。E02 只挡住最危险的那半种写法
// （deny_private_ranges:false 配通配主机名），写成具体内网主机的档位是能启动的。
func TestRegister_WarnsWhenAddressPolicyDisabled(t *testing.T) {
	relaxed := httpGetCommand("https://api.internal:8443/ping", "api.internal:8443")
	relaxed.Name = "dev_callback"
	relaxed.DenyPrivate = boolPtr(false)

	guarded := httpGetCommand("https://api.internal:8443/ping", "api.internal:8443")
	guarded.Name = "prod_callback"

	cfg := configWith(t.TempDir(), relaxed, guarded)
	registry, err := NewRegistry(cfg, quietLogger())
	require.NoError(t, err)

	var logs bytes.Buffer
	result, err := Register(newFakeRegistrar(), registry, cfg, nil, slog.New(slog.NewTextHandler(&logs, nil)))
	require.NoError(t, err)
	assert.Equal(t, 2, result.Registered)

	output := logs.String()
	assert.Contains(t, output, "level=WARN", "关掉防线的档位要留一条 warn")
	assert.Contains(t, output, "executor http profiles accept private and loopback addresses")
	assert.Contains(t, output, "profiles=exec.dev_callback", "只列关掉防线的那条档位")
	assert.NotContains(t, output, "exec.prod_callback", "开着防线的档位不该出现在这行里")
}
