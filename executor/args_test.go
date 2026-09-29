package executor

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"godelayq/core"
)

// 本文件的用例一律从 core.Config 出发经 LoadProfiles 取档位：
// 手工拼出来的 Profile 测出的"通过"，在真实加载路径上未必成立。

func loadOneProfile(t *testing.T, command core.ExecutorCommand) (*Profile, string) {
	t.Helper()

	workspace := t.TempDir()
	profiles := mustLoad(t, configWith(workspace, command))
	require.Len(t, profiles, 1)
	return profiles[0], workspace
}

// reportCommand 是设计文档 §5.2 的示例档位，原样搬过来（DoD 最后一条）。
func reportCommand() core.ExecutorCommand {
	return core.ExecutorCommand{
		Name:    "nightly_report",
		Kind:    "script",
		Runtime: "node",
		Script:  "scripts/report.mjs",
		Args: []core.ExecutorArg{
			{Name: "day", Required: true, Pattern: `^(yesterday|today|\d{4}-\d{2}-\d{2})$`},
			{Name: "level", Default: "info", Pattern: `^(info|debug)$`},
		},
		ArgsRender:  []string{"--day={day}", "--level={level}"},
		Cwd:         ".",
		Env:         map[string]string{"REPORT_HOME": "/srv/report"},
		Timeout:     10 * time.Minute,
		MaxParallel: 1,
	}
}

// etlCommand 与 jarCommand 是设计文档 §5.3 的两条示例档位。
func etlCommand() core.ExecutorCommand {
	return core.ExecutorCommand{
		Name:       "etl_full",
		Kind:       "binary",
		Program:    "bin/etl",
		Positional: &core.ExecutorPositional{Max: 3, Pattern: `^[A-Za-z0-9._/-]+$`},
		Args:       []core.ExecutorArg{{Name: "window", Pattern: `^\d{8}$`}},
		ArgsRender: []string{"--window={window}"},
		Timeout:    30 * time.Minute,
	}
}

func jarCommand() core.ExecutorCommand {
	return core.ExecutorCommand{
		Name:       "jar_batch",
		Kind:       "binary",
		Program:    "java",
		FixedArgs:  []string{"-jar", "app/app.jar"},
		Timeout:    30 * time.Minute,
		Positional: &core.ExecutorPositional{Max: 2},
	}
}

func httpCommand() core.ExecutorCommand {
	return core.ExecutorCommand{
		Name:         "rebuild_index",
		Kind:         "http",
		Method:       "POST",
		URLTemplate:  "https://api.internal/v1/tenants/{tenant}/rebuild",
		Args:         []core.ExecutorArg{{Name: "tenant", Required: true, Pattern: `^[a-z0-9-]{1,32}$`}},
		AllowedHosts: []string{"api.internal"},
		Headers:      map[string][]string{"X-Source": {"godelayq"}},
		HeaderAllow:  []string{"X-Trace-Id"},
		Body:         "json",
		ExpectStatus: []int{200, 201, 202},
		Timeout:      30 * time.Second,
	}
}

// TestValidateSubmission_UnknownKeys 是 D1/D2 两条口径的核心守卫：
// payload 里自己指定执行内容的写法必须全部被拒。这几条用例一旦失败，
// 意味着"只能执行配置里写好的命令"这条白名单不起作用了。
func TestValidateSubmission_UnknownKeys(t *testing.T) {
	profile, _ := loadOneProfile(t, reportCommand())

	for _, key := range []string{"cmd", "script", "url", "program", "shell", "runtime", "argv"} {
		payload := fmt.Sprintf(`{"%s":"anything"}`, key)
		_, err := ValidateSubmission(profile, []byte(payload))
		require.Error(t, err, "payload %s 必须被拒绝", payload)
		assert.Contains(t, err.Error(), key, "错误要指出是哪个键：%v", err)
		assert.Contains(t, err.Error(), "is not accepted by profile", "错误要说清是白名单拦下的：%v", err)
	}

	// args 里面的键同样受白名单约束：档位没声明过的名字一律拒绝
	_, err := ValidateSubmission(profile, []byte(`{"args":{"day":"today","evil":"ls"}}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "args.evil")
	assert.Contains(t, err.Error(), "does not declare this argument")

	// 两个顶层 JSON 对象拼在一起也不是一个 payload
	_, err = ValidateSubmission(profile, []byte(`{"args":{"day":"today"}}{"args":{"day":"yesterday"}}`))
	assert.Error(t, err)
}

func TestValidateSubmission_ValueTypes(t *testing.T) {
	command := reportCommand()
	command.Args = []core.ExecutorArg{{Name: "count", Pattern: `^[0-9.]+$`}}
	command.ArgsRender = []string{"--count={count}"}
	profile, _ := loadOneProfile(t, command)

	rejected := map[string]string{
		"true":   `{"args":{"count":true}}`,
		"null":   `{"args":{"count":null}}`,
		"object": `{"args":{"count":{"a":1}}}`,
		"array":  `{"args":{"count":[1]}}`,
	}
	for kind, payload := range rejected {
		_, err := ValidateSubmission(profile, []byte(payload))
		require.Error(t, err, "%s 必须被拒绝", kind)
		assert.Contains(t, err.Error(), "use a string or a number", kind)
	}

	// 字符串与数字都接受，数字取 JSON 原文（1.50 不会被浮点往返写成 1.5）
	for _, payload := range []string{`{"args":{"count":"123"}}`, `{"args":{"count":123}}`, `{"args":{"count":1.50}}`} {
		sub, err := ValidateSubmission(profile, []byte(payload))
		require.NoError(t, err, payload)
		assert.NotEmpty(t, sub.Args["count"], payload)
	}
	sub, err := ValidateSubmission(profile, []byte(`{"args":{"count":1.50}}`))
	require.NoError(t, err)
	assert.Equal(t, "1.50", sub.Args["count"], "数字要保留调用方写的原文")
}

// TestValidateSubmission_DashPrefix 守住"参数值被解释成选项"这条路：默认拒绝，
// 例外必须在档位里显式写 allow_dash。
func TestValidateSubmission_DashPrefix(t *testing.T) {
	// 默认安全字符集本身是允许减号开头的，所以挡住它的是这条规则而不是 pattern
	strict, _ := loadOneProfile(t, core.ExecutorCommand{
		Name: "tool", Kind: "binary", Program: "bin/tool",
		Args:       []core.ExecutorArg{{Name: "level"}},
		ArgsRender: []string{"--level={level}"},
	})
	_, err := ValidateSubmission(strict, []byte(`{"args":{"level":"-config=/tmp/x"}}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "args.level")
	assert.Contains(t, err.Error(), "allow_dash", "错误要指出例外怎么开")

	lenient, _ := loadOneProfile(t, core.ExecutorCommand{
		Name: "tool", Kind: "binary", Program: "bin/tool",
		Args:       []core.ExecutorArg{{Name: "level", AllowDash: true}},
		ArgsRender: []string{"{level}"},
	})
	sub, err := ValidateSubmission(lenient, []byte(`{"args":{"level":"-config=/tmp/x"}}`))
	require.NoError(t, err)
	argv, err := lenient.Render(sub)
	require.NoError(t, err)
	assert.Equal(t, []string{filepath.Join(lenient.Workspace, "bin", "tool"), "-config=/tmp/x"}, argv)

	// 减号不在开头不算越界：默认字符集里到处都是减号
	_, err = ValidateSubmission(strict, []byte(`{"args":{"level":"warn-only"}}`))
	assert.NoError(t, err)
}

func TestValidateSubmission_PatternRequiredAndControl(t *testing.T) {
	profile, _ := loadOneProfile(t, reportCommand())

	cases := []struct {
		name    string
		payload string
		want    []string
	}{
		{"缺必填", `{"args":{"level":"debug"}}`, []string{"args.day", "required"}},
		{"模式不符", `{"args":{"day":"tomorrow"}}`, []string{"args.day", "does not match pattern"}},
		{"值超长", `{"args":{"day":"` + strings.Repeat("a", 300) + `"}}`, []string{"args.day", "the limit is 256"}},
		{"含换行", "{\"args\":{\"day\":\"a\\nb\"}}", []string{"args.day", "control characters"}},
		{"含制表", "{\"args\":{\"day\":\"a\\tb\"}}", []string{"args.day", "control characters"}},
		{"含 NUL", "{\"args\":{\"day\":\"a\\u0000b\"}}", []string{"args.day", "control characters"}},
		{"可选参数给了空值", `{"args":{"day":"today","level":""}}`, []string{"args.level", "does not match pattern"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ValidateSubmission(profile, []byte(tc.payload))
			require.Error(t, err)
			for _, want := range tc.want {
				assert.Contains(t, err.Error(), want, "错误要能定位到参数并说明原因")
			}
		})
	}

	// 空 payload 是合法的：level 用默认值，day 没有默认所以报错，两条都要能对上
	_, err := ValidateSubmission(profile, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "args.day")

	defaulted, err := ValidateSubmission(profile, []byte(`{"args":{"day":"today"}}`))
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"day": "today"}, defaulted.Args)
	argv, err := profile.Render(defaulted)
	require.NoError(t, err)
	assert.Equal(t, []string{"node", filepath.Join(profile.Workspace, "scripts", "report.mjs"), "--day=today", "--level=info"}, argv,
		"没提供的可选参数用档位声明的默认值渲染")
}

func TestValidateSubmission_Env(t *testing.T) {
	command := reportCommand()
	command.EnvAllow = []string{"TRACE_ID", "BUILD_NUMBER"}
	profile, _ := loadOneProfile(t, command)

	// 合法注入通过，且档位固定的 env 不受影响
	sub, err := ValidateSubmission(profile, []byte(`{"args":{"day":"today"},"env":{"TRACE_ID":"abc-123"}}`))
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"TRACE_ID": "abc-123"}, sub.Env)
	assert.Equal(t, "/srv/report", profile.Env["REPORT_HOME"])

	cases := map[string]string{
		"不在 env_allow": `{"args":{"day":"today"},"env":{"OTHER":"x"}}`,
		"键名是小写":        `{"args":{"day":"today"},"env":{"trace_id":"x"}}`,
		"键名含点":         `{"args":{"day":"today"},"env":{"A.B":"x"}}`,
		"值含换行":         "{\"args\":{\"day\":\"today\"},\"env\":{\"TRACE_ID\":\"a\\nb\"}}",
		"覆盖档位固定值":      `{"args":{"day":"today"},"env":{"REPORT_HOME":"/tmp"}}`,
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ValidateSubmission(profile, []byte(payload))
			require.Error(t, err, "payload=%s", payload)
		})
	}

	// 条数上限按条数算：17 条合法键名也要被挡
	entries := make(map[string]string, maxSubmissionEnvEntries+1)
	for i := 0; i <= maxSubmissionEnvEntries; i++ {
		entries[fmt.Sprintf("TRACE_ID%d", i)] = "v"
	}
	_, err = ValidateSubmission(profile, mustJSON(t, map[string]any{"args": map[string]string{"day": "today"}, "env": entries}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "the limit is 16")

	// 上限之内的条数通过
	okEntries := make(map[string]string, 2)
	okEntries["TRACE_ID"] = "a"
	okEntries["BUILD_NUMBER"] = "b"
	_, err = ValidateSubmission(profile, mustJSON(t, map[string]any{"args": map[string]string{"day": "today"}, "env": okEntries}))
	assert.NoError(t, err)
}

func TestValidateSubmission_SecretNotLeakedInError(t *testing.T) {
	profile, _ := loadOneProfile(t, core.ExecutorCommand{
		Name: "with_secret", Kind: "binary", Program: "bin/tool",
		Args:       []core.ExecutorArg{{Name: "token", Secret: true, Required: true, Pattern: `^[a-f0-9]{8}$`}},
		ArgsRender: []string{"--token={token}"},
	})

	const secretValue = "deadbeef-not-the-right-shape"
	_, err := ValidateSubmission(profile, []byte(`{"args":{"token":"`+secretValue+`"}}`))
	require.Error(t, err)
	assert.NotContains(t, err.Error(), secretValue, "secret 参数的值不能出现在错误信息里")
	assert.Contains(t, err.Error(), `<redacted>`)
	assert.Contains(t, err.Error(), "args.token", "掩码之后仍要说清是哪个参数")
}

// TestValidateSubmission_KindGates 固定"哪种 payload 键属于哪种档位"：
// http 档位不接受 args/env，script/binary 不接受 params/headers/body。
// 口径与 core 侧的 ValidatePayloadKeys 一致，两条路径不该给出两种答案。
func TestValidateSubmission_KindGates(t *testing.T) {
	report, _ := loadOneProfile(t, reportCommand())
	httpProfile, _ := loadOneProfile(t, httpCommand())

	_, err := ValidateSubmission(report, []byte(`{"args":{"day":"today"},"params":{"tenant":"acme"}}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "belong to http profiles")

	_, err = ValidateSubmission(report, []byte(`{"args":{"day":"today"},"body":{"a":1}}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "belong to http profiles")

	_, err = ValidateSubmission(httpProfile, []byte(`{"params":{"tenant":"acme"},"args":{"tenant":"acme"}}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "belong to script/binary profiles")

	_, err = ValidateSubmission(httpProfile, []byte(`{"params":{"tenant":"acme"},"env":{"A":"B"}}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "belong to script/binary profiles")

	// http 的必填值要从 params 来：只填 args 等于没填
	_, err = ValidateSubmission(httpProfile, []byte(`{"body":{"force":true}}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "params.tenant")

	sub, err := ValidateSubmission(httpProfile, []byte(`{"params":{"tenant":"acme"},"headers":{"X-Trace-Id":"t-1"},"body":{"force":true}}`))
	require.NoError(t, err)
	assert.Equal(t, "acme", sub.Params["tenant"])

	// header 必须在 header_allow 里；大小写按 HTTP 的规矩不区分
	_, err = ValidateSubmission(httpProfile, []byte(`{"params":{"tenant":"acme"},"headers":{"Authorization":"Bearer x"}}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "header_allow")

	// 档位声明 body: none 时 payload 不该带体
	noBody, _ := loadOneProfile(t, core.ExecutorCommand{
		Name: "ping", Kind: "http", Method: "GET",
		URLTemplate: "https://api.internal/ping", AllowedHosts: []string{"api.internal"},
		Body: "none",
	})
	_, err = ValidateSubmission(noBody, []byte(`{"body":{"a":1}}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not accept a request body")
}

func TestValidateSubmission_Positional(t *testing.T) {
	etl, workspace := loadOneProfile(t, etlCommand())

	sub, err := ValidateSubmission(etl, []byte(`{"args":{"window":"20260929","_positional":["a.csv","b.csv"]}}`))
	require.NoError(t, err)
	assert.Equal(t, []string{"a.csv", "b.csv"}, sub.Positional)
	assert.Equal(t, map[string]string{"window": "20260929"}, sub.Args,
		"保留键不算具名参数，不该出现在 Args 里")

	argv, err := etl.Render(sub)
	require.NoError(t, err)
	assert.Equal(t, []string{filepath.Join(workspace, "bin", "etl"), "--window=20260929", "a.csv", "b.csv"}, argv)

	cases := map[string]string{
		"超过条数上限": `{"args":{"_positional":["a","b","c","d"]}}`,
		"模式不符":   `{"args":{"_positional":["a;rm -rf"]}}`,
		"值以减号开头": `{"args":{"_positional":["--evil"]}}`,
		"不是数组":   `{"args":{"_positional":"a.csv"}}`,
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ValidateSubmission(etl, []byte(payload))
			require.Error(t, err, "payload=%s", payload)
			assert.Contains(t, err.Error(), positionalKey)
		})
	}

	// 没声明 positional 的档位不能收位置参数
	report, _ := loadOneProfile(t, reportCommand())
	_, err = ValidateSubmission(report, []byte(`{"args":{"day":"today","_positional":["x"]}}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not accept positional arguments")

	// 档位里也不允许把保留键声明成参数名
	_, err = LoadProfiles(configWith(t.TempDir(), core.ExecutorCommand{
		Name: "tool", Kind: "binary", Program: "bin/tool",
		Args: []core.ExecutorArg{{Name: positionalKey}},
	}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reserved for positional values")
}

// TestRender_ScriptDesignExample 把设计文档 §5.2 的示例跑成断言：
// 文档写的 argv 与代码产出的 argv 必须逐元素相等。
func TestRender_ScriptDesignExample(t *testing.T) {
	profile, workspace := loadOneProfile(t, reportCommand())

	sub, err := ValidateSubmission(profile, []byte(`{"args":{"day":"yesterday","level":"debug"}}`))
	require.NoError(t, err)
	argv, err := profile.Render(sub)
	require.NoError(t, err)
	assert.Equal(t, []string{
		"node",
		filepath.Join(workspace, "scripts", "report.mjs"),
		"--day=yesterday",
		"--level=debug",
	}, argv)

	// 文档 §5.2 的 payload 带着 env，但那条档位没声明 env_allow——必须拒绝
	_, err = ValidateSubmission(profile, []byte(`{"args":{"day":"yesterday","level":"debug"},"env":{"TRACE_ID":"abc-123"}}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "env_allow")

	// jar_batch 没有任何 args_render：argv 就是程序名加写死的参数
	jar, _ := loadOneProfile(t, jarCommand())
	sub, err = ValidateSubmission(jar, nil)
	require.NoError(t, err)
	argv, err = jar.Render(sub)
	require.NoError(t, err)
	assert.Equal(t, []string{"java", "-jar", "app/app.jar"}, argv,
		"PATH 程序用程序名，workspace 内产物用绝对路径")
}

// TestRender_ValueStaysOneElement 是不经 shell 的直接证据：
// 含空格的值仍然是一个元素，没有因为"拼成一条字符串再拆开"而变成两个。
func TestRender_ValueStaysOneElement(t *testing.T) {
	profile, workspace := loadOneProfile(t, core.ExecutorCommand{
		Name: "spaces", Kind: "binary", Program: "bin/tool",
		// 自定义 pattern 允许空格：默认安全集不允许，这里就是为了测元素边界
		Args:       []core.ExecutorArg{{Name: "title", Pattern: `^[A-Za-z0-9 .:_/-]+$`}},
		ArgsRender: []string{"--title={title}"},
	})

	sub, err := ValidateSubmission(profile, []byte(`{"args":{"title":"two words here"}}`))
	require.NoError(t, err)
	argv, err := profile.Render(sub)
	require.NoError(t, err)

	require.Len(t, argv, 2, "argv = %v", argv)
	assert.Equal(t, "--title=two words here", argv[1], "含空格的值必须是单独一个元素")
	assert.Equal(t, filepath.Join(workspace, "bin", "tool"), argv[0])

	// 防御性检查：argv 里没有任何 shell 包装的形态
	joined := strings.Join(argv, " ")
	for _, shellForm := range []string{"sh -c", "bash -c", "cmd /c", "powershell -Command"} {
		assert.NotContains(t, joined, shellForm)
	}
	for _, element := range argv[1:] {
		assert.NotContains(t, []string{"-c", "/c"}, element, "没有一个元素是 shell 的 -c 开关：%v", argv)
	}
}

func TestRender_BraceInValue(t *testing.T) {
	profile, _ := loadOneProfile(t, core.ExecutorCommand{
		Name: "braces", Kind: "binary", Program: "bin/tool",
		Args:       []core.ExecutorArg{{Name: "raw", Pattern: `^[A-Za-z0-9.:_{}/-]+$`}},
		ArgsRender: []string{"--raw={raw}"},
	})

	sub, err := ValidateSubmission(profile, []byte(`{"args":{"raw":"{day}"}}`))
	require.NoError(t, err, "pattern 允许大括号时校验层放行，渲染层要拦住")

	_, err = profile.Render(sub)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "contains a brace")
}

func TestRender_MissingValueForTemplate(t *testing.T) {
	// 模板引用了一个既没有默认值也没被 payload 提供的参数：宁可报错，不要拼出 "--day="
	profile, _ := loadOneProfile(t, core.ExecutorCommand{
		Name: "half", Kind: "binary", Program: "bin/tool",
		Args:       []core.ExecutorArg{{Name: "day"}},
		ArgsRender: []string{"--day={day}"},
	})

	sub, err := ValidateSubmission(profile, nil)
	require.NoError(t, err)
	_, err = profile.Render(sub)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "{day}")
	assert.Contains(t, err.Error(), "no value")
}

func TestRender_HttpWrongKind(t *testing.T) {
	profile, _ := loadOneProfile(t, httpCommand())
	sub, err := ValidateSubmission(profile, []byte(`{"params":{"tenant":"acme"}}`))
	require.NoError(t, err)

	_, err = profile.Render(sub)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrWrongKind, "http 档位没有命令行，判断要用哨兵错误")

	// nil 提交也不能拼出半截 argv
	_, err = profile.Render(nil)
	assert.Error(t, err)
}

func TestEffectiveTimeout(t *testing.T) {
	cfg := core.DefaultConfig()
	normalized := cfg.Normalized()
	profile, _ := loadOneProfile(t, reportCommand()) // 档位声明 10m，全局上限 30m

	cases := []struct {
		name      string
		requested time.Duration
		want      time.Duration
	}{
		{"都为缺省：用档位声明值", 0, 10 * time.Minute},
		{"payload 在上限内：用 payload 值", 90 * time.Second, 90 * time.Second},
		{"payload 超全局上限：夹到上限", 4 * time.Hour, normalized.Executors.MaxTimeout},
		{"payload 比档位还长：夹到全局上限而不是档位值", 20 * time.Minute, 20 * time.Minute},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := profile.EffectiveTimeout(tc.requested, cfg)
			assert.Equal(t, tc.want, got)
			assert.Greater(t, got, time.Duration(0), "结果永远大于 0")
		})
	}

	// 档位没写 timeout 时用全局默认值；配置全是零值也不能给出 0
	noTimeout, _ := loadOneProfile(t, core.ExecutorCommand{
		Name: "plain", Kind: "binary", Program: "bin/tool",
	})
	assert.Equal(t, normalized.Executors.DefaultTimeout, noTimeout.EffectiveTimeout(0, cfg))
	assert.Greater(t, noTimeout.EffectiveTimeout(0, core.Config{}), time.Duration(0))

	// 提交期就把超上限的 payload 挡掉，不静默夹取
	_, err := ValidateSubmission(profile, []byte(`{"args":{"day":"today"},"timeout":"2h"}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exceeds the 10m0s allowed by profile")

	_, err = ValidateSubmission(profile, []byte(`{"args":{"day":"today"},"timeout":"soon"}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a duration")

	_, err = ValidateSubmission(profile, []byte(`{"args":{"day":"today"},"timeout":"-5s"}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must not be negative")
}

// mustJSON 把结构编码成 payload，省掉用例里的手写转义。
func mustJSON(t *testing.T, value any) []byte {
	t.Helper()

	encoded, err := json.Marshal(value)
	require.NoError(t, err)
	return encoded
}
