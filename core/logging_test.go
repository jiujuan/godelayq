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
