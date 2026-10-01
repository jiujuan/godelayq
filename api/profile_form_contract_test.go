package api

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"godelayq/core"
)

// 前后端 DTO 的契约用例（TASK-W08 卡 §5.2）。
//
// 表单与请求体各写一份字段名，漂移的后果是"改了却不生效"或"保存直接 400"，
// 而这两种都要到点击之后才看得见。这里把 ProfileForm.vue 提交体的形状钉在 Go 侧：
// 样本里的键必须是 DTO 认识的键（DisallowUnknownFields 那条），
// DTO 里的键必须都在样本里出现（反射那份标签表），两边缺一边都红。
//
// 样本手抄自 web/src/components/executors/ProfileForm.vue 的 buildPayload：
// 改那张表单的字段时同步这里，否则这条用例会以"表单发了一个后端不认的键"的形式失败。

// formSampleProcess 是进程档位（binary）的一份完整提交体：表单能给这一族的字段一个不落。
const formSampleProcess = `{
  "name": "nightly_report",
  "kind": "binary",
  "program": "bin/report.exe",
  "fixed_args": ["--verbose"],
  "args": [
    {"name": "day", "required": true, "default": "today", "pattern": "^(yesterday|today)$", "secret": true, "allow_dash": true}
  ],
  "args_render": ["--day={day}"],
  "positional": {"max": 3, "pattern": "^[A-Za-z0-9._-]+$"},
  "cwd": "reports",
  "env": {"REPORT_HOME": "/tmp/reports"},
  "env_allow": ["TRACE_ID"],
  "timeout": "90s",
  "max_parallel": 2,
  "retry_on_exit": [75]
}`

// formSampleHTTP 是 http 档位的一份完整提交体。分成两份而不是一份大杂烩：
// 一个 kind 只会发出自己那一族字段（buildPayload 按 kind 组装），混在一起就测不到"表单发多了"。
const formSampleHTTP = `{
  "name": "weather",
  "kind": "http",
  "args": [{"name": "city", "required": true}],
  "method": "POST",
  "url_template": "https://api.example.com/{city}",
  "allowed_hosts": ["api.example.com"],
  "headers": {"X-Api-Key": ["k1", "k2"]},
  "header_allow": ["X-Trace-Id"],
  "body": "{\"city\":\"{city}\"}",
  "expect_status": [200, 201],
  "capture_response": true,
  "max_body_bytes": 4096,
  "max_redirects": 2,
  "deny_private_ranges": false,
  "timeout": "30s",
  "max_parallel": 1
}`

// formSampleScript 是 script 档位的一份提交体：runtime 与 script 这两个键只有这一族会发。
const formSampleScript = `{
  "name": "ui_py",
  "kind": "script",
  "runtime": "python",
  "script": "scripts/py_hello.py",
  "timeout": "2m"
}`

// serverOnlyFields 是页面不该发、表单也确实不发的键：时间戳是服务端事实。
// 新增这一类字段时要在这里登记，否则契约用例会把"后端专属"当成"表单漏了"。
var serverOnlyFields = []string{"created_at", "updated_at"}

func decodeFormSample(t *testing.T, body string) ExecutorProfileRequest {
	t.Helper()

	// 与 bindProfileRequest 同一个判据：后端认不认这个键，看的是拒绝未知键的解码。
	decoder := json.NewDecoder(strings.NewReader(body))
	decoder.DisallowUnknownFields()

	var req ExecutorProfileRequest
	require.NoError(t, decoder.Decode(&req), "表单提交体里有后端不认识的键")
	return req
}

func sampleKeys(body string) []string {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		panic(err)
	}
	keys := make([]string, 0, len(obj))
	for key := range obj {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

func TestProfileFormContract_FormSamplesAreAcceptedByTheDTO(t *testing.T) {
	// 反过来的方向：表单发的每一个键都必须是 DTO 认识的。这条会先于任何界面操作发现拼写漂移。
	for name, body := range map[string]string{
		"binary": formSampleProcess,
		"http":   formSampleHTTP,
		"script": formSampleScript,
	} {
		t.Run(name, func(t *testing.T) {
			decodeFormSample(t, body)
		})
	}
}

func TestProfileFormContract_DTOFieldsAllAppearInFormSamples(t *testing.T) {
	seen := map[string]bool{}
	for _, body := range []string{formSampleProcess, formSampleHTTP, formSampleScript} {
		for _, key := range sampleKeys(body) {
			seen[key] = true
		}
	}

	declared := jsonKeys(reflect.TypeOf(core.ExecutorProfileRecord{}))
	var missing []string
	for _, key := range declared {
		if slices.Contains(serverOnlyFields, key) {
			continue
		}
		if !seen[key] {
			missing = append(missing, key)
		}
	}
	assert.Empty(t, missing, "DTO 里有这些字段没有出现在表单样本里：页面漏发了，或者样本该更新")
}

func TestProfileFormContract_ProcessSampleCarriesTheValuesTheFormBuilds(t *testing.T) {
	// 键名对上之后还要对形状：这几处是表单里最容易拼错的（列表、映射、指针三簇）。
	req := decodeFormSample(t, formSampleProcess)

	assert.Equal(t, "nightly_report", req.Name)
	assert.Equal(t, "binary", req.Kind)
	assert.Equal(t, []string{"--verbose"}, req.FixedArgs)
	require.Len(t, req.Args, 1)
	assert.Equal(t, core.ExecutorArgRecord{
		Name: "day", Required: true, Default: "today",
		Pattern: "^(yesterday|today)$", Secret: true, AllowDash: true,
	}, req.Args[0])
	assert.Equal(t, []string{"--day={day}"}, req.ArgsRender)
	require.NotNil(t, req.Positional)
	assert.Equal(t, core.ExecutorPositionalRecord{Max: 3, Pattern: "^[A-Za-z0-9._-]+$"}, *req.Positional)
	assert.Equal(t, map[string]string{"REPORT_HOME": "/tmp/reports"}, req.Env)
	assert.Equal(t, []string{"TRACE_ID"}, req.EnvAllow)
	assert.Equal(t, "90s", req.Timeout)
	assert.Equal(t, 2, req.MaxParallel)
	assert.Equal(t, []int{75}, req.RetryOnExit)
	assert.True(t, req.CreatedAt.IsZero(), "样本没发时间戳，DTO 也不该凭空补一个")
}

func TestProfileFormContract_HTTPSampleCarriesTheValuesTheFormBuilds(t *testing.T) {
	req := decodeFormSample(t, formSampleHTTP)

	assert.Equal(t, "POST", req.Method)
	assert.Equal(t, "https://api.example.com/{city}", req.URLTemplate)
	assert.Equal(t, []string{"api.example.com"}, req.AllowedHosts)
	assert.Equal(t, map[string][]string{"X-Api-Key": {"k1", "k2"}}, req.Headers)
	assert.Equal(t, []string{"X-Trace-Id"}, req.HeaderAllow)
	assert.Equal(t, `{"city":"{city}"}`, req.Body)
	assert.Equal(t, []int{200, 201}, req.ExpectStatus)
	assert.True(t, req.CaptureResponse)
	assert.Equal(t, 4096, req.MaxBodyBytes)
	assert.Equal(t, 2, req.MaxRedirects)
	// deny_private_ranges 是三态指针：表单只在显式选了值时才发这个键，这里要收到 false 而不是 nil
	require.NotNil(t, req.DenyPrivate)
	assert.False(t, *req.DenyPrivate)
}

func TestProfileFormContract_OmittedFieldsStayEmptyRatherThanZeroWritten(t *testing.T) {
	// script 那份样本没发的字段必须是零值：表单按 kind 组装载荷，
	// 不属于当前档位的键整个不发，后端因此不该看到"空串当作显式取值"。
	req := decodeFormSample(t, formSampleScript)

	assert.Equal(t, "python", req.Runtime)
	assert.Equal(t, "scripts/py_hello.py", req.Script)
	assert.Empty(t, req.Program)
	assert.Empty(t, req.FixedArgs)
	assert.Empty(t, req.Env)
	assert.Nil(t, req.Positional)
	assert.Nil(t, req.DenyPrivate)
	assert.False(t, req.CaptureResponse)
}
