package executor

import (
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"godelayq/core"
)

// envNames 从 BuildEnv 的结果里取键名，保持顺序。
func envNames(env []string) []string {
	names := make([]string, 0, len(env))
	for _, item := range env {
		name, _, _ := strings.Cut(item, "=")
		names = append(names, name)
	}
	return names
}

// envValue 按折叠后的键名查值，查不到返回第二个 false。
func envValue(env []string, key string) (string, bool) {
	for _, item := range env {
		name, value, ok := strings.Cut(item, "=")
		if ok && envKeyFold(name) == envKeyFold(key) {
			return value, true
		}
	}
	return "", false
}

func assertSortedUniqueEnvKeys(t *testing.T, env []string) {
	t.Helper()

	names := envNames(env)
	seen := make(map[string]bool, len(names))
	for i, name := range names {
		folded := envKeyFold(name)
		assert.False(t, seen[folded], "环境变量里 %s 出现了两次：os/exec 对重复键的行为依赖平台", name)
		seen[folded] = true
		if i > 0 {
			assert.GreaterOrEqual(t, strings.Compare(folded, envKeyFold(names[i-1])), 0,
				"输出要按键名有序，便于测试断言与人工比对")
		}
	}
}

func TestBuildEnv_ReturnsNonNilForAnEmptyProfile(t *testing.T) {
	// 这是本函数最要紧的一条行为：返回 nil 会被 os/exec 解释成"继承当前进程的全部环境变量"，
	// 于是服务凭据整包交给子进程。空档位也必须给出一个明确的空切片。
	env := BuildEnv(core.ExecutorsConfig{}, &Profile{Name: "empty", Env: map[string]string{}}, &Submission{})

	assert.NotNil(t, env)
	assertSortedUniqueEnvKeys(t, env)
}

func TestBuildEnv_NeverPassesCredentialPrefixedKeys(t *testing.T) {
	t.Setenv("GODELAYQ_SERVER_AUTH_TOKEN", "super-secret-token")

	// 正常情形：不在白名单里，本来就不该出现
	env := BuildEnv(core.ExecutorsConfig{EnvAllow: []string{"PATH"}},
		&Profile{Name: "p", Env: map[string]string{}}, &Submission{})
	assert.NotContains(t, strings.Join(env, "\n"), "GODELAYQ_SERVER_AUTH_TOKEN")

	// 配置写错的情形：把它列进白名单也照样排除——服务凭据不进子进程是硬规则，不是运维选项
	env = BuildEnv(core.ExecutorsConfig{EnvAllow: []string{"GODELAYQ_SERVER_AUTH_TOKEN"}},
		&Profile{Name: "p", Env: map[string]string{"GODELAYQ_SERVER_AUTH_TOKEN": "x"}},
		&Submission{Env: map[string]string{"GODELAYQ_SERVER_AUTH_TOKEN": "y"}})
	value, present := envValue(env, "GODELAYQ_SERVER_AUTH_TOKEN")
	assert.False(t, present, "三层来源里的 GODELAYQ_ 前缀键都要挡掉，实际得到 %q", value)
}

func TestBuildEnv_MergesThreeLayers(t *testing.T) {
	t.Setenv("E09_FROM_PROCESS", "process-value")
	t.Setenv("E09_FROM_PROFILE", "process-value-to-be-overridden")

	cfg := core.ExecutorsConfig{EnvAllow: []string{"E09_FROM_PROCESS", "E09_FROM_PROFILE"}}
	profile := &Profile{
		Name:     "merge",
		EnvAllow: []string{"E09_FROM_PAYLOAD"},
		Env:      map[string]string{"E09_FROM_PROFILE": "profile-value", "REPORT_HOME": "/srv/report"},
	}
	sub := &Submission{Env: map[string]string{"E09_FROM_PAYLOAD": "payload-value"}}

	env := BuildEnv(cfg, profile, sub)

	// 白名单里的进程变量透传
	assert.Equal(t, "process-value", mustEnvValue(t, env, "E09_FROM_PROCESS"))
	// 档位固定值覆盖进程同名值
	assert.Equal(t, "profile-value", mustEnvValue(t, env, "E09_FROM_PROFILE"))
	// payload 注入的变量在档位 env_allow 里，照常注入
	assert.Equal(t, "payload-value", mustEnvValue(t, env, "E09_FROM_PAYLOAD"))
	// 档位固定值与来源无关，只要配了就在
	assert.Equal(t, "/srv/report", mustEnvValue(t, env, "REPORT_HOME"))
	assertSortedUniqueEnvKeys(t, env)
}

func TestBuildEnv_DropsKeysOutsideAllowList(t *testing.T) {
	t.Setenv("E09_NOT_ALLOWED", "should-not-travel")

	env := BuildEnv(core.ExecutorsConfig{EnvAllow: []string{"PATH"}},
		&Profile{Name: "p", Env: map[string]string{}}, &Submission{})

	_, present := envValue(env, "E09_NOT_ALLOWED")
	assert.False(t, present)
	assertSortedUniqueEnvKeys(t, env)
}

// TestBuildEnv_KeepsWindowsBootVariables 守住"白名单没写也能起进程"这条底线：
// 缺 SystemRoot 时子进程里的系统调用会失败，缺 COMSPEC 时 cmd.exe 找不到自己的 shell。
func TestBuildEnv_KeepsWindowsBootVariables(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("these keys only exist on Windows; on Unix the minimum set is a no-op")
	}

	env := BuildEnv(core.ExecutorsConfig{EnvAllow: []string{"LANG"}},
		&Profile{Name: "p", Env: map[string]string{}}, &Submission{})

	for _, key := range []string{"SystemRoot", "COMSPEC", "PATHEXT"} {
		if os.Getenv(key) == "" {
			continue // 本机没有这个键就没什么可断言
		}
		_, present := envValue(env, key)
		assert.True(t, present, "%s 在本机存在，就该无条件透给子进程", key)
	}
	_, hasPath := envValue(env, "PATH")
	assert.True(t, hasPath, "PATH 属于最低必要集：连它都挡住的话，档位里的程序名就无法解析")
}

func TestBuildEnv_FoldsKeyNamesOnWindowsOnly(t *testing.T) {
	env := BuildEnv(core.ExecutorsConfig{EnvAllow: []string{"PATH"}},
		&Profile{Name: "p", Env: map[string]string{}}, &Submission{})

	if runtime.GOOS == "windows" {
		// Windows 上 os.Environ() 给的是 Path 这类混合大小写写法，白名单里是全大写：
		// 逐字比较会让整条白名单失效，进程拿不到 PATH 也就起不来。
		assert.NotEmpty(t, mustEnvValue(t, env, "PATH"))
		return
	}
	// Unix 上 PATH 与 Path 是两个变量，折成同一个小写键会让大小写检查失去意义
	assert.Equal(t, "PATH", mustEnvName(t, env, "PATH"))
}

func mustEnvValue(t *testing.T, env []string, key string) string {
	t.Helper()

	value, present := envValue(env, key)
	require.True(t, present, "环境变量里应该有 %s", key)
	return value
}

// mustEnvName 返回某个键在结果里的实际写法（大小写按写入方原样）。
func mustEnvName(t *testing.T, env []string, key string) string {
	t.Helper()

	for _, item := range env {
		name, _, ok := strings.Cut(item, "=")
		if ok && envKeyFold(name) == envKeyFold(key) {
			return name
		}
	}
	require.FailNow(t, "环境变量里应该有这个键", key)
	return ""
}
