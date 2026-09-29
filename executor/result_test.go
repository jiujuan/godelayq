package executor

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewResult_FillsKindAndProfile(t *testing.T) {
	workspace := t.TempDir()
	profile := profileFrom(t, configAllowing(workspace, []string{selfExecutable(t)},
		namedScript(t, workspace, "nightly")), 0)

	result := NewResult(profile)

	assert.Equal(t, string(profile.Kind), result.Meta.Kind)
	assert.Equal(t, profile.Name, result.Meta.Profile)
	assert.Empty(t, result.Meta.Preview)
	assert.False(t, result.Meta.Truncated)
}

func TestSetPreview_TailAtCharBoundary(t *testing.T) {
	// "abc你好" 是 8 字节（3 个 ASCII + 2 个三字节汉字）；
	// 取尾部 10 字节会落在倒数第二个"好"字的中间，必须跳过 continuation 字节。
	text := strings.Repeat("abc你好", 500)

	result := &Result{Stdout: []byte(text)}
	result.SetPreview(10)

	assert.Equal(t, "abc你好", result.Meta.Preview)
	assert.True(t, utf8.ValidString(result.Meta.Preview), "预览必须是完整字符：%q", result.Meta.Preview)
	assert.LessOrEqual(t, len(result.Meta.Preview), 10)
}

func TestSetPreview_PicksSourceAndLimits(t *testing.T) {
	t.Run("stdout 为空时用 stderr", func(t *testing.T) {
		result := &Result{Stderr: []byte("runtime error: exit status 71")}
		result.SetPreview(200)
		assert.Equal(t, "runtime error: exit status 71", result.Meta.Preview)
	})

	t.Run("stdout 非空时不看 stderr", func(t *testing.T) {
		result := &Result{Stdout: []byte("ok"), Stderr: []byte("noise")}
		result.SetPreview(200)
		assert.Equal(t, "ok", result.Meta.Preview)
	})

	t.Run("比上限短就整段保留", func(t *testing.T) {
		result := &Result{Stdout: []byte("short output")}
		result.SetPreview(2048)
		assert.Equal(t, "short output", result.Meta.Preview)
	})

	t.Run("上限为 0 表示不带预览", func(t *testing.T) {
		result := &Result{Stdout: []byte("short output")}
		result.SetPreview(0)
		assert.Empty(t, result.Meta.Preview)

		result.SetPreview(-1)
		assert.Empty(t, result.Meta.Preview)
	})

	t.Run("空输出得到空预览", func(t *testing.T) {
		result := &Result{}
		result.SetPreview(64)
		assert.Empty(t, result.Meta.Preview)
	})
}

func TestTruncate_SetsFlag(t *testing.T) {
	result := &Result{
		Stdout: []byte("first 6!rest is dropped"),
		Stderr: []byte("err"),
	}
	result.Truncate(9)

	assert.True(t, result.Meta.Truncated, "stdout 超过上限必须留下截断标记")
	assert.Equal(t, "first 6!r", string(result.Stdout), "保留开头，与写入器到达上限即停的行为一致")
	assert.Equal(t, "err", string(result.Stderr))

	// 记账用的是裁剪后的长度：接口显示 out_bytes 时要能说清"产物文件里有几个字节"
	assert.Equal(t, int64(9), result.Meta.OutBytes)
	assert.Equal(t, int64(3), result.Meta.ErrBytes)
}

func TestTruncate_WithinLimitKeepsFlagFalse(t *testing.T) {
	result := &Result{Stdout: []byte("hello"), Stderr: []byte("world")}
	result.Truncate(1024)

	assert.False(t, result.Meta.Truncated, "没裁过就不该报告截断")
	assert.Equal(t, int64(5), result.Meta.OutBytes)
	assert.Equal(t, int64(5), result.Meta.ErrBytes)
}

func TestTruncate_BothStreamsOverLimit(t *testing.T) {
	result := &Result{
		Stdout: []byte(strings.Repeat("a", 100)),
		Stderr: []byte(strings.Repeat("b", 100)),
	}
	result.Truncate(40)

	assert.True(t, result.Meta.Truncated)
	assert.Equal(t, int64(40), result.Meta.OutBytes)
	assert.Equal(t, int64(40), result.Meta.ErrBytes)
}

// TestTruncate_Unlimited 钉住 maxBytes<=0 的含义：不裁剪，但字节数照常记账。
// 执行器配置里 max_bytes 有最小值约束（enabled 时 >=1024），
// 这里留一条明确的行为，免得以后有人把 0 当成"清空输出"。
func TestTruncate_Unlimited(t *testing.T) {
	result := &Result{Stdout: []byte("keep me"), Stderr: []byte("and me")}
	result.Truncate(0)

	assert.False(t, result.Meta.Truncated)
	assert.Equal(t, "keep me", string(result.Stdout))
	assert.Equal(t, int64(7), result.Meta.OutBytes)
	assert.Equal(t, int64(6), result.Meta.ErrBytes)
}

func TestTrimPreview(t *testing.T) {
	// 接口透传与执行侧生成预览必须用同一条规则，两处长度才可能对得上
	assert.Equal(t, "cdefghij", TrimPreview("abcdefghij", 8))
	assert.Equal(t, "abcdefghij", TrimPreview("abcdefghij", 20))
	assert.Empty(t, TrimPreview("abcdefghij", 0))
	assert.Empty(t, TrimPreview("", 10))

	trimmed := TrimPreview("abc你好", 4)
	require.True(t, utf8.ValidString(trimmed))
	assert.Equal(t, "好", trimmed, "4 字节会切进 你 的中间，跳过 continuation 后只剩 好")
}
