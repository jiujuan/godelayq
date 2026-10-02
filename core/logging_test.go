package core

import (
	"bytes"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewLogger_LevelAndFormat(t *testing.T) {
	cases := []struct {
		name     string
		level    string
		format   string
		wantJSON bool
	}{
		{name: "默认级别", level: "", format: ""},
		{name: "大小写不敏感", level: "INFO", format: "TEXT"},
		{name: "json 格式", level: "info", format: "json", wantJSON: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf := &bytes.Buffer{}
			logger, err := NewLogger(tc.level, tc.format, buf)
			require.NoError(t, err)

			logger.Info("job started", "job_id", "j-1")
			line := buf.String()

			if tc.wantJSON {
				assert.Contains(t, line, `"msg":"job started"`)
				assert.Contains(t, line, `"job_id":"j-1"`)
			} else {
				assert.Contains(t, line, `msg="job started"`)
				assert.Contains(t, line, "job_id=j-1")
			}
		})
	}
}

func TestNewLogger_LevelFiltering(t *testing.T) {
	buf := &bytes.Buffer{}
	logger, err := NewLogger("warn", "text", buf)
	require.NoError(t, err)

	logger.Debug("dropped debug")
	logger.Info("dropped info")
	logger.Warn("kept warn")
	logger.Error("also kept")

	assert.NotContains(t, buf.String(), "dropped debug")
	assert.NotContains(t, buf.String(), "dropped info")
	assert.Contains(t, buf.String(), "kept warn")
	assert.Contains(t, buf.String(), "also kept")

	buf.Reset()
	debugLogger, err := NewLogger("debug", "text", buf)
	require.NoError(t, err)
	debugLogger.Debug("kept debug")
	assert.Contains(t, buf.String(), "kept debug")
}

func TestNewLogger_RejectsUnknownValues(t *testing.T) {
	_, err := NewLogger("verbose", "text", &bytes.Buffer{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "logging.level")

	_, err = NewLogger("info", "yaml", &bytes.Buffer{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "logging.format")
}

func TestWithLogger_InjectsIntoScheduler(t *testing.T) {
	buf := &bytes.Buffer{}
	logger, err := NewLogger("info", "text", buf)
	require.NoError(t, err)

	store, err := NewJSONFileStore(t.TempDir() + "/jobs.json")
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	s := NewScheduler(store, nil, nil, WithLogger(logger))
	require.NotNil(t, s.logger)

	// 运行期改并发数会被忽略并记日志，以此确认注入的是同一个日志器
	s.running = true
	s.SetConcurrency(4)
	assert.Contains(t, buf.String(), "SetConcurrency ignored")
	assert.Contains(t, buf.String(), "workers=4")
}

func TestWithLogger_InjectsIntoWSServer(t *testing.T) {
	buf := &bytes.Buffer{}
	logger, err := NewLogger("info", "text", buf)
	require.NoError(t, err)

	ws, u := newTestWSServer(t, NewEventBus(4))
	assert.NotSame(t, ws.logger, logger)

	ws = NewWSServer(NewEventBus(4), u, WithLogger(logger), WithAllowedOrigins("https://good.example"))
	assert.Equal(t, logger, ws.logger)
	assert.False(t, ws.allowAll)
	assert.True(t, ws.originSet["https://good.example"])

	conn := connect(t, ws, u, "https://good.example")
	assert.Contains(t, buf.String(), "websocket client connected")

	ws.Stop()
	assert.True(t, conn.isClosed())
}

func TestResolveLogger_FallsBackToDefault(t *testing.T) {
	assert.Equal(t, slog.Default(), resolveLogger(nil))

	custom := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	assert.Equal(t, custom, resolveLogger(custom))
}

// TestSetLogLevel_ChangesOutput 交回载体的目的只有一个：运行期改级别真的改变了输出。
// 三条断按 R02 §5.1 的顺序排——先证明 info 下 debug 不出现，再证明改完出现，
// 最后证明"写错的级别名"不会把已经在跑的级别带走（这条最容易写成"失败也置默认值"）。
func TestSetLogLevel_ChangesOutput(t *testing.T) {
	var buf bytes.Buffer
	logger, levelVar, err := NewLoggerWithLevelVar("info", "text", &buf)
	require.NoError(t, err)

	logger.Debug("first") // info 级别下不该出现
	assert.NotContains(t, buf.String(), "first")

	require.NoError(t, SetLogLevel(levelVar, "debug"))
	buf.Reset()
	logger.Debug("second")
	assert.Contains(t, buf.String(), "second", "debug record still hidden after SetLogLevel")

	// 解析失败保持原值：这一条守住"写错的级别名不会把日志关掉"
	require.Error(t, SetLogLevel(levelVar, "verbose"))
	buf.Reset()
	logger.Debug("third")
	assert.Contains(t, buf.String(), "third", "level changed by a rejected SetLogLevel call")
}

// TestSetLogLevel_NilCarrierIsRejected 载体是调用方交进来的，nil 只能来自接线的疏漏：
// 返回错误而不是 panic，重载链才能把它当成一次普通的失败收口。
func TestSetLogLevel_NilCarrierIsRejected(t *testing.T) {
	require.Error(t, SetLogLevel(nil, "debug"))
}

// TestNewLogger_DelegationWritesThrough 是 §5.1 那条"转调之后既有行为不变"的落点，
// 但只断一件既有用例没断过的事：NewLogger 丢掉载体之后，交出去的那条 logger 仍然按
// 解析出来的级别过滤输出（转调若把 handler 的 level 写死成常量，这里就会红）。
// 非法级别 / 非法格式 / 默认 info+text 三条预期由同文件既有的
// TestNewLogger_RejectsUnknownValues 与 TestNewLogger_LevelAndFormat 守着，
// 那两条未经修改即过，所以这里不再抄一遍。
func TestNewLogger_DelegationWritesThrough(t *testing.T) {
	var buf bytes.Buffer
	logger, err := NewLogger("info", "text", &buf)
	require.NoError(t, err)

	logger.Debug("hidden at info level")
	assert.NotContains(t, buf.String(), "hidden at info level")

	logger.Info("kept at info level")
	assert.Contains(t, buf.String(), "kept at info level")
}
