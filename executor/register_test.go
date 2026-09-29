package executor

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"sort"
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
}

func newFakeRegistrar(keys ...string) *fakeRegistrar {
	f := &fakeRegistrar{handlers: make(map[string]core.Handler)}
	for _, key := range keys {
		f.handlers[key] = func(context.Context, *core.Job) error { return nil }
	}
	return f
}

func (f *fakeRegistrar) RegisterHandler(jobType string, handler core.Handler) {
	f.handlers[jobType] = handler
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

	registry, err := NewRegistry(configAllowing(workspace, []string{executable, missingProgram},
		named("alpha", executable),
		named("beta", executable),
		named("zeta", missingProgram), // 探测失败：仍然要注册
	), quietLogger())
	require.NoError(t, err)

	registrar := newFakeRegistrar()
	var logs bytes.Buffer
	result, err := Register(registrar, registry, slog.New(slog.NewTextHandler(&logs, nil)))
	require.NoError(t, err)

	assert.Equal(t, Registration{Total: 3, Registered: 3, Unavailable: 1}, result)
	assert.Equal(t, []string{"exec.alpha", "exec.beta", "exec.zeta"}, registrar.registeredKeys())

	for _, key := range []string{"exec.alpha", "exec.beta", "exec.zeta"} {
		handler, ok := registrar.handlers[key]
		require.True(t, ok, "%s 应该已注册", key)
		require.NotNil(t, handler)
	}

	output := logs.String()
	assert.Contains(t, output, "executor handlers registered")
	assert.Contains(t, output, "total=3")
	assert.Contains(t, output, "registered=3")
	assert.Contains(t, output, "unavailable=1")
}

func TestRegister_KeyConflict(t *testing.T) {
	workspace := t.TempDir()

	registry, err := NewRegistry(configAllowing(workspace, []string{selfExecutable(t)},
		namedScript(t, workspace, "alpha"),
		namedScript(t, workspace, "beta"),
	), nil)
	require.NoError(t, err)

	// 模拟代码里已经占用过这个键的处理函数
	registrar := newFakeRegistrar("exec.beta")
	result, err := Register(registrar, registry, nil)

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
	result, err := Register(registrar, registry, slog.New(slog.NewTextHandler(&logs, nil)))
	require.NoError(t, err)

	assert.Equal(t, Registration{}, result)
	assert.Empty(t, registrar.handlers)
	// 关闭状态下登记表为空是正常情况，启动日志里不该出现注册行或告警
	assert.Empty(t, logs.String())
}

func TestRegister_LogsWarnWithoutProfiles(t *testing.T) {
	registry, err := NewRegistry(configWith(t.TempDir()), nil)
	require.NoError(t, err)
	require.True(t, registry.Enabled())
	require.Empty(t, registry.Keys())

	var logs bytes.Buffer
	result, err := Register(newFakeRegistrar(), registry, slog.New(slog.NewTextHandler(&logs, nil)))
	require.NoError(t, err)

	assert.Equal(t, Registration{}, result)
	output := logs.String()
	assert.Contains(t, output, "level=WARN")
	assert.Contains(t, output, "executors are enabled but no profile is declared")
}

func TestStubHandler_ReturnsNotImplemented(t *testing.T) {
	workspace := t.TempDir()
	profile := profileFrom(t, configAllowing(workspace, []string{selfExecutable(t)},
		namedScript(t, workspace, "nightly")), 0)

	var logs bytes.Buffer
	restore := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(restore) })

	handler := profile.StubHandler()

	err := handler(context.Background(), &core.Job{ID: "job-1"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exec.nightly")
	assert.Contains(t, err.Error(), "not implemented yet")

	output := logs.String()
	assert.Contains(t, output, "executor stub invoked")
	assert.Contains(t, output, "job_id=job-1")
	assert.Contains(t, output, "handler_key=exec.nightly")

	// 已取消的上下文不改变结论：占位实现不做任何等待，也返回同样的错误
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	assert.ErrorContains(t, handler(ctx, &core.Job{ID: "job-2"}), "not implemented yet")
}
