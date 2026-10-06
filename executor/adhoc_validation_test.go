package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"godelayq/core"
)

// 这一组是 TASK-N05 的用例：内置自由执行档位在提交期收下任务给出的位置，
// 并在执行期按它组装 argv 或请求。
//
// 全部判据都从 ValidateSubmission 这一入口验，而不是直接调 checkAdhocScript——
// 接口侧（api 的 gateExecutorSubmission）与执行侧（Runner / HTTPRunner）走的是同一个函数，
// 用例打在这个点上，两侧的行为就是同一份证据。

// adhocProfileOf 按档位名取一条内置档位（php / python / shell / http）。
func adhocProfileOf(t *testing.T, cfg core.Config, name string) *Profile {
	t.Helper()

	profiles, skipped, err := AdhocProfiles(cfg)
	require.NoError(t, err)
	require.Empty(t, skipped, "内置档位没全部构造出来，用例的前提就变了：%+v", skipped)
	return findAdhocProfile(t, profiles, name)
}

// writeAdhocScript 真实创建 dir/name 这个文件并返回它的绝对路径。
// 内置档位要求文件存在（提交期就判），所以这些用例都需要现场文件。
func writeAdhocScript(t *testing.T, dir, name string) string {
	t.Helper()

	require.NoError(t, os.MkdirAll(dir, 0o750))
	absolute := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(absolute, []byte("#!/bin/sh\necho placeholder\n"), 0o750))
	return absolute
}

// locationOf 把绝对路径还原成 resolveAnywhere 会给出的写法，
// 免得用例因为 C:\ 与 C:/ 两种同义写法各比一次。
func locationOf(t *testing.T, absolute string) string {
	t.Helper()

	resolved, err := filepath.Abs(filepath.Clean(absolute))
	require.NoError(t, err)
	return resolved
}

func jsonPayload(t *testing.T, values map[string]any) []byte {
	t.Helper()

	data, err := json.Marshal(values)
	require.NoError(t, err)
	return data
}

func TestAdhocSubmission_ScriptLocationAccepted(t *testing.T) {
	workspace := t.TempDir()
	profile := adhocProfileOf(t, adhocEnabled(configWith(workspace)), AdhocPHPName)

	outside := filepath.Join(t.TempDir(), "jobs")
	cases := []struct {
		name  string
		value func() string
		want  func(absolute string) string
	}{
		{"绝对路径", func() string {
			return filepath.ToSlash(writeAdhocScript(t, outside, "absolute.php"))
		}, nil},
		{"本机原生写法", func() string {
			// Windows 上这就是反斜杠路径（Unix 上与正斜杠同形）。
			// 判据必须单独看这一条：参数值那一份规则禁反斜杠，位置走的是放开它的那一份。
			return writeAdhocScript(t, outside, "native.php")
		}, nil},
		{"上跳再下来的写法", func() string {
			// PathAnywhere 的语义允许这种写法；限定目录由 executors.adhoc.path_prefixes 管（另见下组用例）。
			// 手工拼分隔符而不是 filepath.Join：Join 会把 ".." 那段当场收掉，测不到上跳。
			file := writeAdhocScript(t, filepath.Join(outside, "deep"), "upjump.php")
			return filepath.Dir(file) + string(filepath.Separator) + "scratch" +
				string(filepath.Separator) + ".." + string(filepath.Separator) + filepath.Base(file)
		}, nil},
		{"带空格", func() string {
			return writeAdhocScript(t, filepath.Join(outside, "my work"), "task one.php")
		}, nil},
		{"中文目录", func() string {
			// 中文出现在路径里是合法取值，与 TASK-N01 的任务名称规则无关：
			// 一个管"这条任务叫什么"，一个管"这条任务跑哪个文件"。
			return writeAdhocScript(t, filepath.Join(outside, "脚本目录"), "报告.php")
		}, nil},
		{"大写扩展名", func() string {
			return writeAdhocScript(t, outside, "UPPER.PHP")
		}, nil},
		{"workspace 相对写法", func() string {
			writeAdhocScript(t, filepath.Join(workspace, "scripts"), "relative.php")
			return filepath.Join("scripts", "relative.php")
		}, func(string) string {
			// 相对写法的基准是 executors.workspace，不是进程的工作目录。
			return filepath.Join(workspace, "scripts", "relative.php")
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			value := tc.value()
			sub, err := ValidateSubmission(profile, jsonPayload(t, map[string]any{"script": value}))
			require.NoError(t, err, "取值 %q 应当通过", value)
			if tc.want == nil {
				assert.Equal(t, locationOf(t, value), sub.Script)
			} else {
				assert.Equal(t, locationOf(t, tc.want(value)), sub.Script)
			}
			assert.Empty(t, sub.URL)
			assert.Empty(t, sub.Args, "内置档位不参数化：位置之外没有参数注入的口子")
		})
	}
}

func TestAdhocSubmission_ScriptLocationRejected(t *testing.T) {
	workspace := t.TempDir()
	outside := filepath.Join(t.TempDir(), "jobs")
	existing := writeAdhocScript(t, outside, "real.php")
	looksLikeScript := writeAdhocScript(t, outside, "a.txt")

	// 一个名叫 *.php 的目录：文件判据要给出"不是普通文件"，而不是"扩展名不符"。
	directory := filepath.Join(outside, "looks_like.php")
	require.NoError(t, os.MkdirAll(directory, 0o750))

	cases := []struct {
		name      string
		value     string
		wantInErr string
	}{
		{"空串", "", "must not be empty"},
		{"只有空白", "   ", "must not be empty"},
		{"含换行", existing + "\nrm -rf /", "control characters"},
		{"含分号", existing + "; rm -rf /", "must not contain \"';'\""},
		{"含竖线", existing + " | tee log", "must not contain \"'|'\""},
		{"含与号", existing + " && echo done", "must not contain \"'&'\""},
		{"含美元符", "$HOME/a.php", "must not contain \"'$'\""},
		{"含反引号", "`id`.php", "must not contain \"'`'\""},
		{"指向目录", directory, "not a regular file"},
		{"文件不存在", filepath.Join(outside, "absent.php"), "does not exist"},
		{"扩展名不符", looksLikeScript, "runs .php files"},
		{"系统程序", filepath.Join(outside, "..", "windows", "system32", "cmd.exe"), "runs .php files"},
	}

	profile := adhocProfileOf(t, adhocEnabled(configWith(workspace)), AdhocPHPName)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ValidateSubmission(profile, jsonPayload(t, map[string]any{"script": tc.value}))
			require.Error(t, err, "取值 %q 不该通过", tc.value)
			assert.Contains(t, err.Error(), tc.wantInErr)
		})
	}
}

func TestAdhocSubmission_PathPrefixesLimitTheDirectory(t *testing.T) {
	workspace := t.TempDir()
	cfg := adhocEnabled(configWith(workspace))
	cfg.Executors.Adhoc.PathPrefixes = []string{"scripts"}
	profile := adhocProfileOf(t, cfg, AdhocPHPName)

	// 相对写法以 executors.workspace 为基准，与档位自己的 script 字段同一条基准。
	inside := writeAdhocScript(t, filepath.Join(workspace, "scripts"), "ok.php")
	_, err := ValidateSubmission(profile, jsonPayload(t, map[string]any{"script": inside}))
	require.NoError(t, err)

	elsewhere := writeAdhocScript(t, filepath.Join(t.TempDir(), "elsewhere"), "ok.php")
	_, err = ValidateSubmission(profile, jsonPayload(t, map[string]any{"script": elsewhere}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "outside the directories this server allows")

	// 同盘相邻的相似前缀目录不算落在内：HasPrefix 会放行它，withinDirectory 不会。
	lookAlike := writeAdhocScript(t, filepath.Join(workspace, "scripts-evil"), "ok.php")
	_, err = ValidateSubmission(profile, jsonPayload(t, map[string]any{"script": lookAlike}))
	require.Error(t, err, "workspace 下的 scripts-evil 不在 scripts 之内")
	assert.Contains(t, err.Error(), "outside the directories this server allows")
}

func TestAdhocSubmission_ExtensionRequirementCanBeTurnedOff(t *testing.T) {
	workspace := t.TempDir()
	file := writeAdhocScript(t, filepath.Join(workspace, "scripts"), "run.txt")

	cfg := adhocEnabled(configWith(workspace))
	profile := adhocProfileOf(t, cfg, AdhocPHPName)
	_, err := ValidateSubmission(profile, jsonPayload(t, map[string]any{"script": file}))
	require.Error(t, err, "默认要求扩展名：.txt 不是 php 文件")

	off := false
	cfg.Executors.Adhoc.RequireExtension = &off
	loose := adhocProfileOf(t, cfg, AdhocPHPName)
	sub, err := ValidateSubmission(loose, jsonPayload(t, map[string]any{"script": file}))
	require.NoError(t, err, "同一个取值在关掉要求之后通过——这是配置能表达的放宽，不是代码里的例外")
	assert.NotEmpty(t, sub.Script)
}

func TestAdhocSubmission_URLLocationAccepted(t *testing.T) {
	workspace := t.TempDir()

	cases := []struct {
		name       string
		urlHosts   []string
		value      string
		wantStored string
	}{
		{
			name: "不限主机", value: "https://api.example.com/healthz",
			wantStored: "https://api.example.com/healthz",
		},
		{
			name: "带端口", urlHosts: []string{"api.example.com:8443"},
			value: "https://api.example.com:8443/v1/ping", wantStored: "https://api.example.com:8443/v1/ping",
		},
		{
			name: "带查询串", value: "https://api.example.com/search?q=godelayq&page=2",
			wantStored: "https://api.example.com/search?q=godelayq&page=2",
		},
		{
			name: "名单精确命中", urlHosts: []string{"hook.example.com"},
			value: "http://hook.example.com/orders/1", wantStored: "http://hook.example.com/orders/1",
		},
		{
			name: "名单通配命中", urlHosts: []string{"*.example.com"},
			value: "https://a.b.example.com/x", wantStored: "https://a.b.example.com/x",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := adhocEnabled(configWith(workspace))
			cfg.Executors.Adhoc.URLHosts = tc.urlHosts
			profile := adhocProfileOf(t, cfg, AdhocHTTPName)

			sub, err := ValidateSubmission(profile, jsonPayload(t, map[string]any{"url": tc.value}))
			require.NoError(t, err, "取值 %q 应当通过", tc.value)
			assert.Equal(t, tc.wantStored, sub.URL)
			assert.Empty(t, sub.Script)
			assert.Empty(t, sub.Params, "内置 http 档位不用占位符：整条地址来自任务")
		})
	}
}

func TestAdhocSubmission_URLLocationRejected(t *testing.T) {
	workspace := t.TempDir()

	cases := []struct {
		name      string
		urlHosts  []string
		value     string
		wantInErr string
	}{
		{"空串", nil, "", "must not be empty"},
		{"非 http 方案", nil, "ftp://files.example.com/a.txt", "http or https URL"},
		{"脚本式方案", nil, "javascript:alert(1)", "http or https URL"},
		{"带凭据", nil, "http://user:pass@api.example.com/x", "must not carry user credentials"},
		{"含空格", nil, "https://api.example.com/a b", "whitespace or control characters"},
		{"含制表符", nil, "https://api.example.com/a\tb", "whitespace or control characters"},
		{"无主机", nil, "https:///healthz", "has no host"},
		{"写法不成 URL", nil, "://callback", "is not a valid URL"},
		{"主机不在名单", []string{"hook.example.com"}, "https://api.example.com/x", "not in executors.adhoc.url_hosts"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := adhocEnabled(configWith(workspace))
			cfg.Executors.Adhoc.URLHosts = tc.urlHosts
			profile := adhocProfileOf(t, cfg, AdhocHTTPName)

			_, err := ValidateSubmission(profile, jsonPayload(t, map[string]any{"url": tc.value}))
			require.Error(t, err, "取值 %q 不该通过", tc.value)
			assert.Contains(t, err.Error(), tc.wantInErr)
			if tc.value != "" {
				assert.NotContains(t, err.Error(), tc.value,
					"拒绝理由里不回显整条地址：地址里可能带口令，而这条错误会进响应、事件与日志")
			}
		})
	}
}

func TestAdhocSubmission_KeyOwnership(t *testing.T) {
	workspace := t.TempDir()
	enabled := adhocEnabled(configWith(workspace))
	php := adhocProfileOf(t, enabled, AdhocPHPName)
	httpProfile := adhocProfileOf(t, enabled, AdhocHTTPName)

	t.Run("脚本档位收到 url", func(t *testing.T) {
		_, err := ValidateSubmission(php, []byte(`{"script":"a.php","url":"https://api.example.com/x"}`))
		require.Error(t, err)
		assert.Contains(t, err.Error(), `payload key "url" is not accepted by profile "php"`)
		assert.Contains(t, err.Error(), `takes "script"`, "拒绝文本要指向这条档位真正收的那个键")
	})

	t.Run("http 档位收到 script", func(t *testing.T) {
		_, err := ValidateSubmission(httpProfile, []byte(`{"url":"https://api.example.com/x","script":"a.php"}`))
		require.Error(t, err)
		assert.Contains(t, err.Error(), `payload key "script" is not accepted by profile "http"`)
		assert.Contains(t, err.Error(), `takes "url"`)
	})

	t.Run("位置键必填", func(t *testing.T) {
		_, err := ValidateSubmission(php, []byte(`{}`))
		require.Error(t, err)
		assert.Contains(t, err.Error(), `payload must give "script"`)

		_, err = ValidateSubmission(httpProfile, []byte(`{"timeout":"5s"}`))
		require.Error(t, err)
		assert.Contains(t, err.Error(), `payload must give "url"`)
	})

	t.Run("内置档位不接受参数注入", func(t *testing.T) {
		// 风险表第 4 条：用户可能以为位置写在 args 里面。
		_, err := ValidateSubmission(php, []byte(`{"args":{"script":"a.php"}}`))
		require.Error(t, err)
		assert.Contains(t, err.Error(), `payload key "args" is not accepted by profile "php"`)
		assert.Contains(t, err.Error(), `takes "script" and timeout only`)

		for _, payload := range []string{
			`{"url":"https://api.example.com/x","params":{"id":"1"}}`,
			`{"url":"https://api.example.com/x","headers":{"x-a":"1"}}`,
			`{"url":"https://api.example.com/x","body":{"a":1}}`,
			`{"url":"https://api.example.com/x","env":{"A":"1"}}`,
		} {
			_, err := ValidateSubmission(httpProfile, []byte(payload))
			require.Error(t, err)
			assert.Contains(t, err.Error(), "is not accepted by profile",
				"内置 http 档位只收 url 与 timeout")
		}
	})

	t.Run("位置键是字符串", func(t *testing.T) {
		_, err := ValidateSubmission(php, []byte(`{"script":["a.php"]}`))
		require.Error(t, err)
		assert.Contains(t, err.Error(), `payload key "script" must be a string`)
	})
}

func TestSubmissionKeys_ListedByProfileKind(t *testing.T) {
	workspace := t.TempDir()
	enabled := adhocEnabled(configWith(workspace))

	// 内置档位：只有本类型那一个位置键 + timeout。
	assert.Equal(t, []string{"script", "timeout"}, submissionKeys(adhocProfileOf(t, enabled, AdhocPHPName)))
	assert.Equal(t, []string{"url", "timeout"}, submissionKeys(adhocProfileOf(t, enabled, AdhocHTTPName)))

	// 普通档位一条都没放宽：允许键列表里不许出现 script / url，
	// 否则运维会把"这个键不认识"读成"这个版本不支持这种写法"。
	ordinary := mustLoad(t, configWith(workspace, validScriptProfile("scripts/x.mjs")))[0]
	_, err := ValidateSubmission(ordinary, []byte(`{"args":{"day":"today"},"script":"x.mjs"}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), `payload key "script" is not accepted by profile "nightly_report"`)
	assert.Contains(t, err.Error(), "allowed keys: args, env, timeout")

	httpOrdinary := mustLoad(t, configWith(workspace,
		httpGetCommand("https://api.example.com/x", "api.example.com")))[0]
	_, err = ValidateSubmission(httpOrdinary, []byte(`{"url":"https://api.example.com/x"}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "allowed keys: params, headers, body, timeout")
}

func TestAdhocSubmission_TimeoutStillCapped(t *testing.T) {
	workspace := t.TempDir()
	cfg := adhocEnabled(configWith(workspace))
	cfg.Executors.MaxTimeout = 30 * time.Second
	cfg.Executors.DefaultTimeout = 30 * time.Second
	normalized := cfg.Normalized()

	profile := adhocProfileOf(t, normalized, AdhocPHPName)
	require.Equal(t, 30*time.Second, profile.Timeout, "内置档位的生效上限来自 executors.max_timeout，与普通档位同一条")

	file := writeAdhocScript(t, workspace, "slow.php")

	// 超过档位上限的请求在提交期就拒（E08 那一条判据，内置档位不例外）。
	_, err := ValidateSubmission(profile, jsonPayload(t, map[string]any{"script": file, "timeout": "10m"}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exceeds the 30s allowed by profile")

	sub, err := ValidateSubmission(profile, jsonPayload(t, map[string]any{"script": file, "timeout": "20s"}))
	require.NoError(t, err)
	assert.Equal(t, 20*time.Second, sub.TimeoutValue)
	assert.Equal(t, 20*time.Second, profile.EffectiveTimeout(sub.TimeoutValue, normalized))

	// payload 不带 timeout 时走档位默认，仍然是有限值。
	bare, err := ValidateSubmission(profile, jsonPayload(t, map[string]any{"script": file}))
	require.NoError(t, err)
	assert.Equal(t, 30*time.Second, profile.EffectiveTimeout(bare.TimeoutValue, normalized))

	// 写法非法照样拒（同一条判据，不因内置档位而放过）。
	_, err = ValidateSubmission(profile, jsonPayload(t, map[string]any{"script": file, "timeout": "soon"}))
	require.Error(t, err)
}

func TestRender_AdhocScriptUsesJobPathAndStaysTwoArgs(t *testing.T) {
	workspace := t.TempDir()
	profile := adhocProfileOf(t, adhocEnabled(configWith(workspace)), AdhocPHPName)

	file := writeAdhocScript(t, filepath.Join(workspace, "my work"), "任务 one.php")
	sub, err := ValidateSubmission(profile, jsonPayload(t, map[string]any{"script": file}))
	require.NoError(t, err)

	argv, err := profile.Render(sub)
	require.NoError(t, err)
	require.Len(t, argv, 2, "内置档位没有 args_render，也没有位置参数：argv 就是解释器加那一个文件")
	assert.Equal(t, "php", argv[0], "argv[0] 仍是解释器名，绝对路径由执行侧的探测给（E09）")
	assert.Equal(t, locationOf(t, file), argv[1])
	assert.Equal(t, argv[1], sub.Script, "跑的就是判过的那一个路径，不再拼第二段")
}

// TestRender_NonAdhocUnchanged 是 DoD 第 3 条那一侧的证据：
// 新增的 Adhoc 分支不改变普通档位的 argv。非 adhoc 的全部既有用例零改动也是同一件事，
// 这一条把"改了哪一行"钉在最直接的位置上。
func TestRender_NonAdhocUnchanged(t *testing.T) {
	workspace := t.TempDir()
	script := declareFile(t, workspace, "scripts/deploy.sh")
	profile := mustLoad(t, configWith(workspace, scriptUsingRuntime("bash", script)))[0]

	assert.False(t, profile.Adhoc, "配置文件里的档位不是内置档位")
	sub, err := ValidateSubmission(profile, []byte(`{"args":{"day":"today"}}`))
	require.NoError(t, err)

	argv, err := profile.Render(sub)
	require.NoError(t, err)
	assert.Equal(t, []string{"bash", filepath.Join(workspace, "scripts", "deploy.sh"), "--day=today"}, argv,
		"普通档位的 argv 仍是解释器 + 档位里的脚本 + args_render")

	_, err = ValidateSubmission(profile, []byte(`{"args":{"day":"today"},"script":"scripts/deploy.sh"}`))
	require.Error(t, err, "普通档位不能用位置键跑另一个文件")
}

// TestHTTPRunner_AdhocRefusesPrivateAddress 落实卡片 §5 的"打 127.0.0.1 与 169.254.169.254 不建连"：
// 提交期只判主机名单，地址范围守卫在执行期建连之前判，两层各在一处。
func TestHTTPRunner_AdhocRefusesPrivateAddress(t *testing.T) {
	target := newCountingTarget(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("should not be reached"))
	}), false)

	for _, address := range []string{"127.0.0.1", "169.254.169.254"} {
		t.Run(address, func(t *testing.T) {
			fixture := newAdhocHTTPFixture(t, adhocEnabled(configWith(t.TempDir())))
			value := fmt.Sprintf("http://%s:%s/status", address, target.port())

			job, err := fixture.run(context.Background(), "job-adhoc-refused", fmt.Sprintf(`{"url":%q}`, value))
			require.Error(t, err, "内置 http 档位默认禁回环与私网")
			assert.ErrorIs(t, err, errAddressRefused)
			assert.True(t, asExitError(t, err).Permanent(), "策略不会自己变，重试没有意义")
			assert.Equal(t, int32(0), target.hits.Load(), "拒绝必须发生在建连之前")
			assert.Contains(t, fixture.logs.String(), "executor http target address refused",
				"被拒地址要留一条 warn 线索")

			require.NotNil(t, job.Exec)
			assert.Equal(t, 0, job.Exec.HTTPStatus, "没连上就没有状态码可言")
		})
	}

	// 对照：显式放开私网范围之后，同一个地址就是正常请求（开发机上打自己的服务）。
	open := adhocEnabled(configWith(t.TempDir()))
	open.Executors.Adhoc.URLAllowPrivate = true
	fixture := newAdhocHTTPFixture(t, open)
	job, err := fixture.run(context.Background(), "job-adhoc-allowed",
		fmt.Sprintf(`{"url":"http://127.0.0.1:%s/status"}`, target.port()))
	require.NoError(t, err, "关掉地址范围守卫之后不该拦这条请求")
	require.NotNil(t, job.Exec)
	assert.Equal(t, int32(1), target.hits.Load())
}

// TestHTTPRunner_AdhocHostGuardRunsAtExecution 是 DoD 第 4 条：
// 提交期判过不等于执行期放过。这里模拟"入队之后名单收紧"（改的是档位对象上的主机名单），
// 已经排着的那条任务在执行期仍然要过同一道判断，并且一次连接都不发起。
//
// 拦下它的是执行侧重跑的那一遍 ValidateSubmission（takeAdhocLocation → checkAdhocURL），
// 而 HTTPRunner.checkTarget 在同一次调用链上再判一遍 scheme、凭据与名单：
// 两道都在建连之前，用例断言的是"没有连接、没有解析"这个结果。
func TestHTTPRunner_AdhocHostGuardRunsAtExecution(t *testing.T) {
	target := newCountingTarget(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("reached"))
	}), false)

	fixture := newAdhocHTTPFixture(t, adhocEnabled(configWith(t.TempDir())))
	value := fmt.Sprintf("http://127.0.0.1:%s/status", target.port())

	// 提交期：url_hosts 留空 = 不限主机，这条取值通过。
	_, err := ValidateSubmission(fixture.profile, []byte(fmt.Sprintf(`{"url":%q}`, value)))
	require.NoError(t, err)

	// 执行期之前把名单收紧（等同于一份改了 executors.adhoc.url_hosts 并重建档位的配置）。
	fixture.profile.AllowedHosts = []string{"hook.example.com"}

	job, err := fixture.run(context.Background(), "job-adhoc-host-narrowed", fmt.Sprintf(`{"url":%q}`, value))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is not in executors.adhoc.url_hosts")
	assert.True(t, asExitError(t, err).Permanent(), "入队时合法、触发时非法的取值不会被重试")
	require.NotNil(t, job.Exec)
	assert.True(t, job.Exec.Permanent, "摘要与错误同源")
	assert.Equal(t, int32(0), target.hits.Load(), "被主机名单拦下时不该有任何连接")
	assert.Equal(t, int32(0), fixture.lookups.Load(), "名单判在解析之前")
}

func TestHTTPRunner_AdhocTargetWithoutTemplate(t *testing.T) {
	target := newCountingTarget(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}), false)

	open := adhocEnabled(configWith(t.TempDir()))
	open.Executors.Adhoc.URLAllowPrivate = true
	fixture := newAdhocHTTPFixture(t, open)
	require.Empty(t, fixture.profile.URLTemplate, "内置 http 档位没有地址模板")

	value := fmt.Sprintf("http://127.0.0.1:%s/orders/7?retry=1", target.port())
	job, err := fixture.run(context.Background(), "job-adhoc-target", fmt.Sprintf(`{"url":%q}`, value))
	require.NoError(t, err)
	require.NotNil(t, job.Exec)
	assert.Equal(t, 200, job.Exec.HTTPStatus)

	recorded := fixture.artifact("job-adhoc-target", "err")
	assert.Contains(t, recorded, value, "adhoc 档位不声明 secret 参数，产物里的地址就是原样那一条")
}

// TestAdhoc_LocationIsNotMasked 钉住卡片 §5 最后一行：
// 位置不是 secret 参数，响应、日志与产物里都允许出现原文。
// 后续若给内置档位加掩码，这条会先红——那时要回答的是"位置为什么算凭据"。
func TestAdhoc_LocationIsNotMasked(t *testing.T) {
	workspace := t.TempDir()
	profile := adhocProfileOf(t, adhocEnabled(configWith(workspace)), AdhocPHPName)

	file := writeAdhocScript(t, filepath.Join(workspace, "scripts"), "token_report.php")
	payload := jsonPayload(t, map[string]any{"script": file})
	sub, err := ValidateSubmission(profile, payload)
	require.NoError(t, err)

	assert.Equal(t, locationOf(t, file), sub.Script)
	assert.Empty(t, profile.Args, "内置档位没有任何声明参数，也就没有 secret 参数可掩")
	assert.Equal(t, string(payload), string(profile.MaskPayload(payload)),
		"没有 secret 参数时掩码必须原样返回")

	// 拒绝文本同样带原文路径：路径是这台机器上的事实，不是凭据。
	_, err = ValidateSubmission(profile, jsonPayload(t, map[string]any{
		"script": filepath.Join(workspace, "scripts", "absent.php")}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "absent.php")
}

// TestRunner_AdhocScriptRunsJobPath 是本卡的目标句："会真的跑那个文件"。
// 用一个带空格的脚本路径跑一遍，同时证明 argv 直传没有被二次切分（D3）。
func TestRunner_AdhocScriptRunsJobPath(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skipf("program %q is not available on this machine, the adhoc run case cannot run", "sh")
	}

	workspace := t.TempDir()
	cfg := adhocEnabled(configWith(workspace))
	cfg.Executors.Adhoc.ShellRuntime = "sh"

	script := writeAdhocScript(t, filepath.Join(workspace, "jobs one"), "print path.sh")
	// 脚本把自己的路径打到标准输出：跑起来了、路径没被拆断，两件事一起有证据。
	require.NoError(t, os.WriteFile(script,
		[]byte("#!/bin/sh\nprintf 'ran:%s\\n' \"$0\"\n"), 0o750))

	fixture := newAdhocRunnerFixture(t, cfg, AdhocShellName)
	job, err := fixture.run(context.Background(), "job-adhoc-run", fmt.Sprintf(`{"script":%q}`, script))
	require.NoError(t, err, "内置 shell 档位要真的执行任务给的那个文件")
	require.NotNil(t, job.Exec)

	output := string(fixture.stream("job-adhoc-run", 1, "out"))
	assert.Contains(t, output, "ran:"+locationOf(t, script),
		"解释器收到的路径必须是判过的那一个绝对路径，含空格也不能断成两段")
}

// TestRunner_AdhocScriptGoneAtExecutionIsPermanent 落实 §3.5：
// 提交之后文件被删，执行期重跑那一份判据把它拦下（不起进程、不留产物文件），
// 按永久失败归类、不排重试。
//
// 分类沿用既有的那一条而不是新写：invalid submission 属于"重跑用的还是同一份 payload，
// 结论不会变"，与档位声明参数越界是同一种情形，位置在执行期消失不需要新规则。
func TestRunner_AdhocScriptGoneAtExecutionIsPermanent(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skipf("program %q is not available on this machine, the adhoc run case cannot run", "sh")
	}

	workspace := t.TempDir()
	cfg := adhocEnabled(configWith(workspace))
	cfg.Executors.Adhoc.ShellRuntime = "sh"

	script := writeAdhocScript(t, workspace, "will_vanish.sh")
	payload := fmt.Sprintf(`{"script":%q}`, script)

	fixture := newAdhocRunnerFixture(t, cfg, AdhocShellName)
	_, err := ValidateSubmission(fixture.profile, []byte(payload))
	require.NoError(t, err, "提交期文件在，判据必须放过")

	require.NoError(t, os.Remove(script))

	job, err := fixture.run(context.Background(), "job-adhoc-vanished", payload)
	require.Error(t, err, "文件在执行期消失了")
	assert.Contains(t, err.Error(), "does not exist", "拒绝理由要说清是文件不见了")
	require.NotNil(t, job.Exec)
	assert.True(t, asExitError(t, err).Permanent(), "重跑用的还是同一个路径，结论不会变")
	assert.True(t, job.Exec.Permanent, "摘要与错误同源")
	assert.NotEqual(t, core.ArtifactAvailable, job.Exec.Artifact,
		"判据在起进程之前拦住，因此没有产物文件")
}

// --- 夹具 ---

// newAdhocRunnerFixture 用内置档位构造进程执行器。
// 与 newRunnerFixture 的差别只在档位来源：这里必须走 AdhocProfiles，
// 否则测的是配置文件档位而不是内置那四条。
func newAdhocRunnerFixture(t *testing.T, cfg core.Config, name string) *runnerFixture {
	t.Helper()

	normalized := cfg.Normalized()
	profile := adhocProfileOf(t, normalized, name)

	store, err := NewArtifactStore(ArtifactOptions{
		Dir: filepath.Join(t.TempDir(), "exec"), MaxBytes: core.DefaultExecMaxOutputBytes,
	}, quietLogger())
	require.NoError(t, err)

	fixture := &runnerFixture{
		t: t, cfg: normalized, profile: profile, artifacts: store, logs: &bytes.Buffer{},
	}
	fixture.runner = NewRunner(profile, store, normalized.Executors, bufferLogger(fixture.logs))
	return fixture
}

// newAdhocHTTPFixture 同上，构造 HTTP 执行器。桩解析默认给回环，真实 DNS 一律不碰。
func newAdhocHTTPFixture(t *testing.T, cfg core.Config) *httpFixture {
	t.Helper()

	normalized := cfg.Normalized()
	profile := adhocProfileOf(t, normalized, AdhocHTTPName)

	store, err := NewArtifactStore(ArtifactOptions{
		Dir: filepath.Join(t.TempDir(), "exec"), MaxBytes: core.DefaultExecMaxOutputBytes,
	}, quietLogger())
	require.NoError(t, err)

	fixture := &httpFixture{t: t, cfg: normalized, profile: profile, artifacts: store, logs: &bytes.Buffer{}}
	runner, err := NewHTTPRunner(profile, store, normalized.Executors, bufferLogger(fixture.logs))
	require.NoError(t, err)
	fixture.runner = runner
	fixture.stubResolver("127.0.0.1")
	return fixture
}
