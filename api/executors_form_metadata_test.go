package api

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"godelayq/core"
	"godelayq/executor"
)

// TestListExecutors_ExposesFormMetadata 是 TASK-E18 §3.1 与 §3.3 的接口前提：
// 新建表单的输入形状（参数、位置参数、允许的请求头、能不能带请求体）与超时区间
// 全部由这个响应给出，前端不复制任何档位规则。
func TestListExecutors_ExposesFormMetadata(t *testing.T) {
	registry, err := executor.NewRegistry(executorConfigFor(t.TempDir(),
		core.ExecutorCommand{
			Name:       "with_positional",
			Kind:       "script",
			Runtime:    "node",
			Script:     "scripts/deploy.mjs",
			ArgsRender: []string{"deploy.mjs"},
			Positional: &core.ExecutorPositional{Max: 2},
			Timeout:    90 * time.Second,
		},
		core.ExecutorCommand{
			Name:         "post_json",
			Kind:         "http",
			Method:       "POST",
			URLTemplate:  "https://api.example.com/hooks/{hook}",
			AllowedHosts: []string{"api.example.com"},
			Args:         []core.ExecutorArg{{Name: "hook", Required: true}},
			HeaderAllow:  []string{"X-Trace-Id"},
			Body:         "json",
		},
		core.ExecutorCommand{
			Name:         "no_body",
			Kind:         "http",
			Method:       "GET",
			URLTemplate:  "https://api.example.com/health",
			AllowedHosts: []string{"api.example.com"},
		},
	), newTestLogger())
	require.NoError(t, err)

	srv := newSecurityServer(t, Security{}, WithExecutorRegistry(registry))
	recorder := doGet(t, srv, "/api/v1/executors", nil)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())

	var resp ListExecutorsResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &resp), recorder.Body.String())

	// 超时区间的全局上限：表单要按它拦下"注定被拒"的取值
	assert.Equal(t, (30 * time.Minute).String(), resp.MaxTimeout,
		"没改配置时上限是 DefaultExecMaxTimeout，取值来自登记表而不是写在前端")

	require.Len(t, resp.Profiles, 3)
	byName := make(map[string]ExecutorProfileResponse, 3)
	for _, profile := range resp.Profiles {
		byName[profile.Name] = profile
	}

	script := byName["with_positional"]
	assert.Equal(t, "tail", script.PreferredResultDirection, "进程档位的结论通常在末尾")
	require.NotNil(t, script.Positional, "档位声明了位置参数，表单才知道要留这一格")
	assert.Equal(t, 2, script.Positional.Max)
	assert.NotEmpty(t, script.Positional.Pattern,
		"位置参数的正则要给出实际生效的那一份（配置留空时是默认安全字符集）")
	assert.Empty(t, script.Method, "method / header_allow / body_mode 只属于 http 档位")
	assert.Nil(t, script.HeaderAllow)
	assert.Empty(t, script.BodyMode)

	post := byName["post_json"]
	assert.Equal(t, "head", post.PreferredResultDirection,
		"http 档位的正文是响应体，出错信息通常在开头，读尾部会剩看不懂的那半截")
	assert.Equal(t, "POST", post.Method)
	assert.Equal(t, "json", post.BodyMode)
	assert.Equal(t, []string{"X-Trace-Id"}, post.HeaderAllow)
	require.Len(t, post.Args, 1)
	assert.Equal(t, "hook", post.Args[0].Name,
		"http 档位的 URL 占位符也走 args 声明，表单按同一份规格生成输入项")

	// 没写 body 的 http 档位：空串在执行侧与 none 是同一条判断，响应里归一成一个词，
	// 免得前端再各判一次空串（args.go 的 checkHTTPBody 就是两者同判）
	assert.Equal(t, "none", byName["no_body"].BodyMode)
	assert.Empty(t, byName["no_body"].HeaderAllow, "没声明请求头白名单时给空表而不是缺键")
}
