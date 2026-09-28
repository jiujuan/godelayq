package api

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newLoggedEngine(logger *slog.Logger) *gin.Engine {
	gin.SetMode(gin.TestMode)

	engine := gin.New()
	engine.Use(recovery(logger))
	engine.Use(requestLogger(logger))
	engine.GET("/ok", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	engine.GET("/bad", func(c *gin.Context) { c.String(http.StatusBadRequest, "nope") })
	engine.GET("/boom", func(c *gin.Context) { panic("kaboom") })

	return engine
}

func doRequest(t *testing.T, engine *gin.Engine, method, target string) int {
	t.Helper()

	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(method, target, nil))
	return recorder.Code
}

func TestRequestLoggerLevels(t *testing.T) {
	buf := &bytes.Buffer{}
	engine := newLoggedEngine(slog.New(slog.NewTextHandler(buf, nil)))

	assert.Equal(t, http.StatusNoContent, doRequest(t, engine, http.MethodGet, "/ok"))
	assert.Equal(t, http.StatusBadRequest, doRequest(t, engine, http.MethodGet, "/bad?reason=test"))

	lines := buf.String()
	assert.Contains(t, lines, `level=INFO msg="http request"`)
	assert.Contains(t, lines, "path=/ok")
	assert.Contains(t, lines, "status=204")
	assert.Contains(t, lines, "latency_ms=")

	assert.Contains(t, lines, `level=WARN msg="http request"`)
	assert.Contains(t, lines, "path=/bad")
	assert.Contains(t, lines, "status=400")
	assert.Contains(t, lines, `query="reason=test"`)
}

// panic 必须被兜住并返回 500，同时堆栈进日志而不是打到 stdout
func TestRecoveryLogsPanicAndReturns500(t *testing.T) {
	buf := &bytes.Buffer{}
	engine := newLoggedEngine(slog.New(slog.NewTextHandler(buf, nil)))

	assert.Equal(t, http.StatusInternalServerError, doRequest(t, engine, http.MethodGet, "/boom"))

	logText := buf.String()
	require.Contains(t, logText, `level=ERROR msg="panic recovered"`)
	assert.Contains(t, logText, "kaboom")
	assert.Contains(t, logText, "path=/boom")
	assert.Contains(t, logText, "stack=")
	assert.False(t, strings.Contains(logText, "level=INFO msg=\"http request\" method=GET path=/boom"),
		"panic 后不应再写一条正常访问日志")
}
