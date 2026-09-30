package executor

import (
	"bytes"
	"context"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"godelayq/core"
)

// 这一组是 TASK-E15 的 HTTP 执行器用例。全部用 net/http/httptest 加桩解析，不访问任何真实网络：
// 卡片 §5 的口径是"涉及 IP 判断的用例通过注入 resolver 完成"。
//
// 桩解析同时承担两件事：把"主机名解析到哪个地址"变成可指定的（私网拒绝、重绑定、TLS 主机名），
// 以及让测试不会因为一条真实 DNS 查询而变慢或漂移。

// countingTarget 是一个带命中计数的测试服务。
// 计数是 DoD 第一条的断言手段：地址判断做错最直接的后果，就是这里多出一次命中。
type countingTarget struct {
	server *httptest.Server
	hits   atomic.Int32
}

func newCountingTarget(t *testing.T, handler http.Handler, useTLS bool) *countingTarget {
	t.Helper()

	target := &countingTarget{}
	counted := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target.hits.Add(1)
		handler.ServeHTTP(w, r)
	})

	if useTLS {
		target.server = httptest.NewTLSServer(counted)
	} else {
		target.server = httptest.NewServer(counted)
	}
	t.Cleanup(func() { target.server.Close() })
	return target
}

// hostPort 返回测试服务的 host:port 写法，用于填 allowed_hosts。
func (c *countingTarget) hostPort() string {
	return strings.TrimPrefix(strings.TrimPrefix(c.server.URL, "https://"), "http://")
}

// port 只取端口那一段。
func (c *countingTarget) port() string {
	_, port, err := net.SplitHostPort(c.hostPort())
	if err != nil {
		panic(err)
	}
	return port
}

// httpFixture 是一条 HTTP 用例的现场：档位、执行器、产物存储与日志。
type httpFixture struct {
	t         *testing.T
	cfg       core.Config
	profile   *Profile
	runner    *HTTPRunner
	artifacts *ArtifactStore
	logs      *bytes.Buffer

	// lookups 数解析被调了几次。重绑定那条用例断言的就是这个数：
	// 只解析一次并按那一个地址建连，第二份 DNS 答案就没有机会改变要连哪儿。
	lookups atomic.Int32
}

// newHTTPFixture 按给定档位构造执行器。档位仍然走 LoadProfiles 的完整校验，
// 因此这些用例同时是"http 档位能配上真实执行器"的证据。
func newHTTPFixture(t *testing.T, command core.ExecutorCommand, tune func(*core.Config, *ArtifactOptions)) *httpFixture {
	t.Helper()

	cfg := configWith(t.TempDir(), command)
	opts := ArtifactOptions{Dir: filepath.Join(t.TempDir(), "exec"), MaxBytes: core.DefaultExecMaxOutputBytes}
	if tune != nil {
		tune(&cfg, &opts)
	}

	normalized := cfg.Normalized()
	profiles := mustLoad(t, normalized)
	require.Len(t, profiles, 1)

	store, err := NewArtifactStore(opts, quietLogger())
	require.NoError(t, err)

	fixture := &httpFixture{t: t, cfg: normalized, profile: profiles[0], artifacts: store, logs: &bytes.Buffer{}}
	runner, err := NewHTTPRunner(profiles[0], store, normalized.Executors, bufferLogger(fixture.logs))
	require.NoError(t, err)
	fixture.runner = runner

	// 默认桩解析：任何主机名都指向回环，真实 DNS 一律不碰。需要别的地址时用 stubResolver 覆盖。
	fixture.stubResolver("127.0.0.1")
	return fixture
}

// stubResolver 指定第 n 次解析返回的地址；超出列表长度之后一直返回最后一条。
func (f *httpFixture) stubResolver(answers ...string) {
	f.runner.lookupHost = func(ctx context.Context, host string) ([]net.IPAddr, error) {
		index := int(f.lookups.Add(1)) - 1
		if index >= len(answers) {
			index = len(answers) - 1
		}
		ip := net.ParseIP(answers[index])
		require.NotNil(f.t, ip, "桩解析的取值必须是 IP 字面量")
		return []net.IPAddr{{IP: ip}}, nil
	}
}

// trust 把测试服务的自签证书加进执行器的信任根（§5.11 允许的注入方式）。
func (f *httpFixture) trust(cert *x509.Certificate) {
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	f.runner.rootCAs = pool
}

// run 执行一次任务。payload 传空串表示"不带 payload"。
func (f *httpFixture) run(ctx context.Context, jobID, payload string) (*core.Job, error) {
	f.t.Helper()

	job := &core.Job{ID: jobID, Name: f.profile.HandlerKey(), Attempts: 1}
	if payload != "" {
		job.Payload = []byte(payload)
	}
	return job, f.runner.Handler()(ctx, job)
}

// artifact 读出第一次尝试的某条流。out 是响应体，err 是请求与响应的头。
func (f *httpFixture) artifact(jobID, stream string) string {
	f.t.Helper()

	data, _, err := f.artifacts.Read(jobID, 1, stream, 0)
	require.NoError(f.t, err)
	return string(data)
}

// httpGetCommand 造一条最小可用的 http 档位：GET、不收请求体、保留响应体。
func httpGetCommand(urlTemplate string, allowedHosts ...string) core.ExecutorCommand {
	return core.ExecutorCommand{
		Name:            "callback",
		Kind:            string(KindHTTP),
		Method:          "GET",
		Body:            "none",
		URLTemplate:     urlTemplate,
		AllowedHosts:    allowedHosts,
		CaptureResponse: true,
		// 实现做错时（比如漏了地址判断）会去拨一个不可路由的地址，2 秒之内必须认错返回，
		// 否则一条用例能把整包测试拖到默认的 5 分钟超时。
		Timeout: 2 * time.Second,
	}
}

func boolPtr(v bool) *bool { return &v }

// TestHTTP_Dial_RefusesPrivate 覆盖第 5.1 条：解析到这些地址时一律拒绝，且拒绝发生在建连之前。
//
// "没建连"的断言力度来自同一个现场：测试服务确实在桩解析给出的那个回环地址上听着，
// 关掉防线之后同一条档位能连上（见 TestHTTP_Dial_Rebinding 与 TestHTTP_IPHostRequiresExplicitAllow），
// 所以这里的 hits==0 说的是"被这道判断拦下了"，不是"端口本来就不对"。
func TestHTTP_Dial_RefusesPrivate(t *testing.T) {
	target := newCountingTarget(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("should not be reached"))
	}), true)

	addresses := []string{
		"127.0.0.1",        // 回环
		"10.0.0.5",         // RFC 1918
		"172.16.0.9",       // RFC 1918
		"192.168.1.1",      // RFC 1918
		"169.254.169.254",  // 链路本地：云主机的元数据地址
		"100.64.0.1",       // RFC 6598 共享地址段
		"::1",              // IPv6 回环
		"fd00::1",          // IPv6 唯一本地地址
		"fe80::1",          // IPv6 链路本地
		"224.0.0.1",        // 组播
		"0.0.0.0",          // 未指定
		"::ffff:127.0.0.1", // IPv4 映射地址：折成 IPv4 之后仍是回环，不能被当成公网 IPv6
	}

	for _, address := range addresses {
		t.Run(address, func(t *testing.T) {
			host := fmt.Sprintf("example.com:%s", target.port())
			fixture := newHTTPFixture(t, httpGetCommand(fmt.Sprintf("https://%s/ping", host), host), nil)
			fixture.stubResolver(address)
			fixture.trust(target.server.Certificate())

			job, err := fixture.run(context.Background(), "job-refused", "")
			require.Error(t, err, "解析到 %s 必须拒绝", address)
			assert.ErrorIs(t, err, errAddressRefused, "策略拒绝要能和真正的连接失败区分开")

			failure := asExitError(t, err)
			assert.True(t, failure.Permanent(), "策略不会自己变，重试没有意义")
			assert.Equal(t, int32(0), target.hits.Load(), "拒绝必须发生在建连之前")
			assert.Equal(t, int32(1), fixture.lookups.Load(), "只解析一次")

			output := fixture.logs.String()
			assert.Contains(t, output, "level=WARN", "被拒地址要留下 warn 线索")
			assert.Contains(t, output, "executor http target address refused")
			assert.Contains(t, output, "address="+net.ParseIP(address).String())
			assert.NotContains(t, output, "/ping", "完整 URL 不进日志")

			require.NotNil(t, job.Exec)
			assert.Equal(t, 0, job.Exec.HTTPStatus, "没连上就没有状态码可言")
		})
	}
}

// TestHTTP_Dial_Rebinding 覆盖第 5.2 条，也是"为什么必须自己解析"的证据。
//
// 桩解析第一次给回环、第二次给一个私网地址。执行器只解析一次，并按那一个地址建连，
// 因此第二份答案永远不会被用到——lookups==1 就是这条防线的全部含义。
// 反过来说，如果实现改成"把主机名交给标准库去解析"，这里的 lookups 会是 0，
// 而标准库在多个地址之间自己重试时最终连了哪儿，我们无从判断也无法设防。
func TestHTTP_Dial_Rebinding(t *testing.T) {
	target := newCountingTarget(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("rebinding-proof"))
	}), true)
	host := fmt.Sprintf("example.com:%s", target.port())

	opened := httpGetCommand(fmt.Sprintf("https://%s/ping", host), host)
	opened.DenyPrivate = boolPtr(false)
	fixture := newHTTPFixture(t, opened, nil)
	fixture.stubResolver("127.0.0.1", "10.9.9.9")
	fixture.trust(target.server.Certificate())

	job, err := fixture.run(context.Background(), "job-rebinding", "")
	require.NoError(t, err, "防线关掉之后，第一次解析给的回环应当连上本地服务")
	assert.Contains(t, fixture.artifact("job-rebinding", "out"), "rebinding-proof")
	assert.Equal(t, int32(1), target.hits.Load())
	assert.Equal(t, int32(1), fixture.lookups.Load(), "解析只能发生一次")
	require.NotNil(t, job.Exec)

	// 同一个档位、同一份答案，只是防线打开：第一次的回环就被拦下，请求一次都没到对端
	guarded := newHTTPFixture(t, httpGetCommand(fmt.Sprintf("https://%s/ping", host), host), nil)
	guarded.stubResolver("127.0.0.1", "10.9.9.9")
	guarded.trust(target.server.Certificate())

	_, err = guarded.run(context.Background(), "job-rebinding-guarded", "")
	assert.ErrorIs(t, err, errAddressRefused)
	assert.Equal(t, int32(1), target.hits.Load(), "被拦下之后仍然没有请求到达")
	assert.Equal(t, int32(1), guarded.lookups.Load())
}

// TestHTTP_AllowedHostsMatch 覆盖第 5.3 条：拼出来的主机命中没命中白名单。
// 只判这一层，不建连，所以直接调 renderURL。
//
// 档位是手工构造的："主机与 allowed_hosts 不匹配"那种写法在启动阶段就被 E02 拒了，
// 而本用例要的恰恰是执行期再判一次这一层，因此绕开配置加载。
func TestHTTP_AllowedHostsMatch(t *testing.T) {
	cases := []struct {
		name     string
		allowed  []string
		template string
		wantErr  string
	}{
		{name: "exact host", allowed: []string{"api.example"}, template: "https://api.example/v1/job"},
		{name: "suffix lookalike", allowed: []string{"api.example"}, template: "https://api.example.evil.com/v1", wantErr: "allowed_hosts"},
		{name: "wildcard subdomain", allowed: []string{"*.example"}, template: "https://a.example/v1"},
		{name: "wildcard deeper suffix", allowed: []string{"*.example"}, template: "https://a.example.evil.com/v1", wantErr: "allowed_hosts"},
		{name: "wildcard without dot", allowed: []string{"*.example"}, template: "https://evil-example.com/v1", wantErr: "allowed_hosts"},
		{name: "wildcard does not match bare domain", allowed: []string{"*.example"}, template: "https://example/v1", wantErr: "allowed_hosts"},
		{name: "port must match too", allowed: []string{"api.example:8443"}, template: "https://api.example/v1", wantErr: "allowed_hosts"},
		{name: "ip host needs an explicit entry", allowed: []string{"other.example"}, template: "http://127.0.0.1:8080/ping", wantErr: "allowed_hosts"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			profile := &Profile{
				Name:         "matrix",
				Kind:         KindHTTP,
				Method:       "GET",
				Body:         "none",
				URLTemplate:  tc.template,
				AllowedHosts: tc.allowed,
				DenyPrivate:  true,
			}
			runner, err := NewHTTPRunner(profile, nil, core.DefaultConfig().Executors, quietLogger())
			require.NoError(t, err)

			_, _, renderErr := runner.renderURL(&Submission{Params: map[string]string{}})
			if tc.wantErr == "" {
				require.NoError(t, renderErr)
				return
			}
			require.Error(t, renderErr)
			assert.Contains(t, renderErr.Error(), tc.wantErr)
		})
	}
}

// TestHTTP_IPHostRequiresExplicitAllow 覆盖第 5.4 条：主机写成 IP 也要出现在 allowed_hosts 里；
// 写进白名单之后仍然过私网判断；只有显式关掉这道防线才放行（本机回环调试那条路）。
func TestHTTP_IPHostRequiresExplicitAllow(t *testing.T) {
	target := newCountingTarget(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("loopback-ok"))
	}), false)

	// ① allowed_hosts 里没有这个 host:port：连地址都不该拼出来
	unlisted := &Profile{
		Name: "callback", Kind: KindHTTP, Method: "GET", Body: "none",
		URLTemplate: target.url(), AllowedHosts: []string{"some.other.host"}, DenyPrivate: true,
	}
	unlistedRunner, err := NewHTTPRunner(unlisted, nil, core.DefaultConfig().Executors, quietLogger())
	require.NoError(t, err)
	_, _, renderErr := unlistedRunner.renderURL(&Submission{Params: map[string]string{}})
	require.Error(t, renderErr)
	assert.Contains(t, renderErr.Error(), "allowed_hosts")

	// ② 写进白名单、防线照开：回环仍然被拒，且没有请求到达
	guarded := newHTTPFixture(t, httpGetCommand(target.url(), target.hostPort()), nil)
	_, err = guarded.run(context.Background(), "job-ip-guarded", "")
	assert.ErrorIs(t, err, errAddressRefused)
	assert.Equal(t, int32(0), target.hits.Load())

	// ③ 显式 deny_private_ranges:false：这条组合确实能通
	opened := httpGetCommand(target.url(), target.hostPort())
	opened.DenyPrivate = boolPtr(false)
	dev := newHTTPFixture(t, opened, nil)
	job, err := dev.run(context.Background(), "job-ip-dev", "")
	require.NoError(t, err)
	assert.Equal(t, int32(1), target.hits.Load())
	assert.Equal(t, 200, job.Exec.HTTPStatus)
	assert.Contains(t, job.Exec.Preview, "loopback-ok")
}

// url 返回测试服务的完整地址（同 server.URL，取个名字让调用处读得顺）。
func (c *countingTarget) url() string { return c.server.URL + "/ping" }

// TestHTTP_StatusClassification 覆盖第 5.5 条：状态码判定与重试分类共用 E12 那一张表。
func TestHTTP_StatusClassification(t *testing.T) {
	cases := []struct {
		status        int
		wantSuccess   bool
		wantPermanent bool
	}{
		{status: 201, wantSuccess: true},
		{status: 200, wantPermanent: true}, // 200 不在显式 expect_status 里：按配置与对端不符处理
		{status: 400, wantPermanent: true},
		{status: 404, wantPermanent: true},
		{status: 408, wantPermanent: false},
		{status: 429, wantPermanent: false},
		{status: 500, wantPermanent: false},
		{status: 503, wantPermanent: false},
	}

	for _, tc := range cases {
		t.Run(fmt.Sprintf("status_%d", tc.status), func(t *testing.T) {
			target := newCountingTarget(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte("body"))
			}), false)

			command := httpGetCommand(target.url(), target.hostPort())
			command.ExpectStatus = []int{201}
			command.DenyPrivate = boolPtr(false)
			fixture := newHTTPFixture(t, command, nil)

			job, err := fixture.run(context.Background(), "job-status", "")
			require.Equal(t, int32(1), target.hits.Load())
			require.NotNil(t, job.Exec)
			assert.Equal(t, tc.status, job.Exec.HTTPStatus, "状态码要进摘要，成功与否都记")

			if tc.wantSuccess {
				require.NoError(t, err)
				assert.False(t, job.Exec.Permanent)
				return
			}
			require.Error(t, err)
			failure := asExitError(t, err)
			assert.Equal(t, tc.wantPermanent, failure.Permanent(),
				"4xx（除 408/429）永久，5xx 与 408/429 可重试")
			assert.Equal(t, tc.wantPermanent, job.Exec.Permanent, "摘要与返回的错误同源（E12 的口径）")
		})
	}
}

// TestHTTP_TransportFailureIsRetryable 是分类表的另一半：连接层失败可重试。
// 取一个刚刚关掉的测试服务地址，那个端口上不再有人监听。
func TestHTTP_TransportFailureIsRetryable(t *testing.T) {
	target := newCountingTarget(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}), false)
	address := target.hostPort()
	target.server.Close()

	command := httpGetCommand("http://"+address+"/ping", address)
	command.DenyPrivate = boolPtr(false)
	fixture := newHTTPFixture(t, command, nil)

	job, err := fixture.run(context.Background(), "job-transport", "")
	require.Error(t, err)
	failure := asExitError(t, err)
	assert.False(t, failure.Permanent(), "连接层失败要给一次重试的机会（卡片 §3.4）")
	assert.Equal(t, 0, job.Exec.HTTPStatus)
	assert.NotContains(t, failure.Error(), "/ping", "错误文本里不该出现带参数的路径")
}

// TestHTTP_BodyCaptureLimit 覆盖第 5.6 条：响应体有硬上限，超限标记 truncated。
// OutBytes 记的是落盘字节数，不是响应全长——这个字段的口径同时写在 core.ExecMeta 的注释里。
func TestHTTP_BodyCaptureLimit(t *testing.T) {
	const (
		responseSize = 5000
		bodyLimit    = 1024
	)
	target := newCountingTarget(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("x"), responseSize))
	}), false)

	command := httpGetCommand(target.url(), target.hostPort())
	command.DenyPrivate = boolPtr(false)
	command.MaxBodyBytes = bodyLimit
	fixture := newHTTPFixture(t, command, nil)

	job, err := fixture.run(context.Background(), "job-limit", "")
	require.NoError(t, err)
	assert.True(t, job.Exec.Truncated, "响应体超过 max_body_bytes 必须标记截断")
	assert.Equal(t, int64(bodyLimit), job.Exec.OutBytes)
	assert.Len(t, fixture.artifact("job-limit", "out"), bodyLimit)
	assert.Equal(t, core.ArtifactAvailable, job.Exec.Artifact)
}

// TestHTTP_RedirectNotFollowed 覆盖第 5.7 条：拿到 302 就停，不跟到 Location 去。
func TestHTTP_RedirectNotFollowed(t *testing.T) {
	var landed atomic.Int32
	target := newCountingTarget(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/landed" {
			landed.Add(1)
			_, _ = w.Write([]byte("followed the redirect"))
			return
		}
		http.Redirect(w, r, "/landed", http.StatusFound)
	}), false)

	command := httpGetCommand(target.server.URL+"/start", target.hostPort())
	command.DenyPrivate = boolPtr(false)
	fixture := newHTTPFixture(t, command, nil)

	job, err := fixture.run(context.Background(), "job-redirect", "")
	require.Error(t, err, "302 不在 expect_status 里就该算失败")
	assert.Equal(t, http.StatusFound, job.Exec.HTTPStatus)
	assert.Zero(t, landed.Load(), "重定向目标由对端决定，跟过去等于绕开 allowed_hosts")

	failure := asExitError(t, err)
	assert.True(t, failure.Permanent(), "再试一次拿到的还是同一个 302")
}

// TestHTTP_SecretHeaderNotLogged 覆盖第 5.8 条：固定头的值与 secret 参数都不进产物与日志。
func TestHTTP_SecretHeaderNotLogged(t *testing.T) {
	const (
		tokenHeader = "Bearer super-secret-token"
		urlSecret   = "pa55word-in-path"
	)
	received := make(chan string, 1)
	target := newCountingTarget(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- r.Header.Get("Authorization")
		_, _ = w.Write([]byte("ok"))
	}), false)

	command := httpGetCommand(target.server.URL+"/orders/{token}", target.hostPort())
	command.DenyPrivate = boolPtr(false)
	command.Headers = map[string][]string{
		"Authorization":    {tokenHeader},
		"X-Trace":          {"trace-1"},
		"x-lower-declared": {"lower-value"},
	}
	command.Args = []core.ExecutorArg{{Name: "token", Required: true, Secret: true}}
	fixture := newHTTPFixture(t, command, nil)

	job, err := fixture.run(context.Background(), "job-secret", `{"params":{"token":"`+urlSecret+`"}}`)
	require.NoError(t, err)
	assert.Equal(t, tokenHeader, <-received, "打码只管我们把它写去哪儿，发出去的请求仍然带真值")
	require.NotNil(t, job.Exec)

	recorded := fixture.artifact("job-secret", "err")
	assert.Contains(t, recorded, "Authorization: "+redactValue, "敏感头以占位形式留下")
	assert.NotContains(t, recorded, tokenHeader, "固定头的值不进产物文件")
	assert.NotContains(t, recorded, urlSecret, "secret 参数值不进产物文件")
	assert.Contains(t, recorded, "/orders/"+maskedURLValue, "请求行仍要看得出访问的是哪一类地址")
	assert.Contains(t, recorded, "X-Trace: trace-1", "不敏感的头照常留着")
	assert.Contains(t, recorded, "X-Lower-Declared: lower-value",
		"配置里写成小写的头名要按 HTTP 惯例归一之后再发出去与落盘")

	output := fixture.logs.String()
	assert.NotContains(t, output, tokenHeader)
	assert.NotContains(t, output, urlSecret)
	assert.Contains(t, output, `msg="executor run finished"`)
}

// TestHTTP_BadMethodOrBody 覆盖第 5.9 条：三种"请求根本不该发出去"的写法都在建连之前被拒，
// 一次命中都没有。
func TestHTTP_BadMethodOrBody(t *testing.T) {
	target := newCountingTarget(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}), false)

	cases := []struct {
		name     string
		mutate   func(*core.ExecutorCommand)
		payload  string
		contains string
	}{
		{
			name:     "profile that accepts no body",
			mutate:   func(*core.ExecutorCommand) {},
			payload:  `{"body":{"a":1}}`,
			contains: "does not accept a request body",
		},
		{
			name:     "json mode rejects a bare string",
			mutate:   func(c *core.ExecutorCommand) { c.Method = "POST"; c.Body = "json" },
			payload:  `{"body":"just a string"}`,
			contains: "JSON object or array",
		},
		{
			name:     "payload cannot set Host",
			mutate:   func(c *core.ExecutorCommand) { c.HeaderAllow = []string{"Host", "X-Trace"} },
			payload:  `{"headers":{"Host":"evil.example"}}`,
			contains: "set by the executor",
		},
		{
			name:     "url parameter cannot restructure the url",
			mutate:   func(c *core.ExecutorCommand) { c.Args = []core.ExecutorArg{{Name: "day", Required: true}} },
			payload:  `{"params":{"day":"2026/01/02"}}`,
			contains: "params.day",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			command := httpGetCommand(target.url(), target.hostPort())
			command.DenyPrivate = boolPtr(false)
			tc.mutate(&command)

			fixture := newHTTPFixture(t, command, nil)
			_, err := fixture.run(context.Background(), "job-bad", tc.payload)

			require.Error(t, err)
			failure := asExitError(t, err)
			assert.True(t, failure.Permanent(), "提交内容的问题不会因为重试而变好")
			assert.Contains(t, failure.Error(), tc.contains)
			assert.Equal(t, int32(0), target.hits.Load(), "这类失败不能已经到达对端")
		})
	}
}

// TestHTTP_CancelAndTimeout 覆盖第 5.10 条：取消与超时都在有界时间内返回，
// 且分类与进程路径一致——一个记 Cancelled、一个记 TimedOut，都不算永久失败。
func TestHTTP_CancelAndTimeout(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	target := newCountingTarget(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
			_, _ = w.Write([]byte("late"))
		case <-r.Context().Done():
		}
	}), false)

	t.Run("cancel", func(t *testing.T) {
		command := httpGetCommand(target.url(), target.hostPort())
		command.DenyPrivate = boolPtr(false)
		fixture := newHTTPFixture(t, command, nil)

		ctx, cancel := context.WithCancel(context.Background())
		type outcome struct {
			job *core.Job
			err error
		}
		returned := make(chan outcome, 1)
		go func() {
			job, err := fixture.run(ctx, "job-cancel", "")
			returned <- outcome{job, err}
		}()

		waitForHits(t, target, 1)
		cancel()

		select {
		case got := <-returned:
			require.Error(t, got.err)
			failure := asExitError(t, got.err)
			assert.True(t, failure.Cancelled, "取消要按被打断处理，不消耗重试名额")
			assert.False(t, failure.TimedOut)
			require.NotNil(t, got.job.Exec)
			assert.False(t, got.job.Exec.Permanent, "摘要不该把用户自己取消的任务标成永久失败")
		case <-time.After(5 * time.Second):
			t.Fatal("取消之后 Handler 没有返回")
		}
	})

	t.Run("timeout", func(t *testing.T) {
		command := httpGetCommand(target.url(), target.hostPort())
		command.DenyPrivate = boolPtr(false)
		command.Timeout = 300 * time.Millisecond
		fixture := newHTTPFixture(t, command, nil)

		started := time.Now()
		_, err := fixture.run(context.Background(), "job-timeout", "")
		elapsed := time.Since(started)

		require.Error(t, err)
		failure := asExitError(t, err)
		assert.True(t, failure.TimedOut, "档位超时要按超时分类，与进程路径同一口径")
		assert.False(t, failure.Cancelled)
		assert.False(t, failure.Permanent(), "超时是可重试的")
		assert.Less(t, elapsed, 5*time.Second, "超时之后不该再等别的东西")
	})
}

// waitForHits 等测试服务收到 n 次请求。
func waitForHits(t *testing.T, target *countingTarget, n int32) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if target.hits.Load() >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("测试服务在 5 秒内没有收到 %d 次请求，实际 %d 次", n, target.hits.Load())
}

// TestHTTP_TLSHostNamePreserved 覆盖第 5.11 条与 DoD 第三条：按 IP 建连之后，
// 证书校验仍然对着档位声明的那个主机名做，既没被绕过也没变成"校验 IP"。
//
// 测试服务的证书里写着 DNS 名 example.com 与回环地址，没有 localhost：
//   - 主机名 example.com：解析到回环、按 IP 建连，握手按 example.com 校验 → 通过。
//     这条同时排掉两种"看着也能跑"的做坏方式：拿拨号的那个 IP 当校验名（证书里没有这条 DNS 名），
//     以及干脆不校验。
//   - 主机名 localhost：同样解析到回环，但证书不含这个名字 → 握手失败。
//     实现若绕过了校验，这一条会成功——那正是卡片要防的隐蔽漏洞。
func TestHTTP_TLSHostNamePreserved(t *testing.T) {
	target := newCountingTarget(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("tls-ok"))
	}), true)

	for _, tc := range []struct {
		host      string
		wantError string
	}{
		{host: "example.com"},
		{host: "localhost", wantError: "certificate"},
	} {
		t.Run(tc.host, func(t *testing.T) {
			allowed := fmt.Sprintf("%s:%s", tc.host, target.port())
			command := httpGetCommand(fmt.Sprintf("https://%s/ping", allowed), allowed)
			command.DenyPrivate = boolPtr(false)
			fixture := newHTTPFixture(t, command, nil)
			fixture.stubResolver("127.0.0.1")
			fixture.trust(target.server.Certificate())

			before := target.hits.Load()
			job, err := fixture.run(context.Background(), "job-tls", "")
			if tc.wantError == "" {
				require.NoError(t, err)
				assert.Equal(t, 200, job.Exec.HTTPStatus)
				assert.Equal(t, before+1, target.hits.Load())
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantError)
			assert.Equal(t, before, target.hits.Load(), "握手没过的请求不会走到处理函数")
		})
	}
}

// TestHTTP_ResponseWithoutCaptureFlag 是 capture_response 那条开关的另一半：
// 档位明确不保留响应体时，产物里只有请求与响应的头，摘要也不带预览。
func TestHTTP_ResponseWithoutCaptureFlag(t *testing.T) {
	target := newCountingTarget(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("private-body-must-not-be-stored"))
	}), false)

	command := httpGetCommand(target.url(), target.hostPort())
	command.DenyPrivate = boolPtr(false)
	command.CaptureResponse = false
	fixture := newHTTPFixture(t, command, nil)

	job, err := fixture.run(context.Background(), "job-nocapture", "")
	require.NoError(t, err)
	assert.Empty(t, fixture.artifact("job-nocapture", "out"), "不保留响应体时 .out 里不该有内容")
	assert.Empty(t, job.Exec.Preview, "预览取自响应体，不保留时也不该留")
	assert.Zero(t, job.Exec.OutBytes)
	assert.Contains(t, fixture.artifact("job-nocapture", "err"), "200 OK", "头信息照常留下")
	assert.NotContains(t, fixture.artifact("job-nocapture", "err"), "private-body")
}

// TestHTTP_ProfileRejectedByWrongRunnerKind 钉住分流之外的保底：
// 手工构造时把非 http 档位交给 HTTPRunner，要构造失败而不是拿到一个能跑的实例。
func TestHTTP_ProfileRejectedByWrongRunnerKind(t *testing.T) {
	command := shellCommand(t, "echo hello")
	cfg := configWith(t.TempDir(), command).Normalized()
	profiles := mustLoad(t, cfg)

	runner, err := NewHTTPRunner(profiles[0], nil, cfg.Executors, quietLogger())
	require.Error(t, err)
	assert.Nil(t, runner)
	assert.Contains(t, err.Error(), "kind")
}

// TestHTTP_ResultDirectionHint 覆盖 §9 第三条留给 E18 的那个提示：
// http 档位的正文开头最有用，进程档位是末尾。
func TestHTTP_ResultDirectionHint(t *testing.T) {
	target := newCountingTarget(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}), false)
	fixture := newHTTPFixture(t, httpGetCommand(target.url(), target.hostPort()), nil)

	assert.Equal(t, ResultDirectionHead, PreferredResultDirection(fixture.profile))
	assert.Equal(t, ResultDirectionTail, PreferredResultDirection(&Profile{Kind: KindScript}))
	assert.Equal(t, ResultDirectionTail, PreferredResultDirection(&Profile{Kind: KindBinary}))
}

// TestHTTP_SecretValueMaskedInPreview 是 §3.3 第 2 条在 http 档位的落点：
// 对端把参数值回显进响应体时，摘要预览不能留明文（那份预览会进事件与低档位读取接口），
// 而产物文件保持原文——读它的门槛在 api 侧已收到提交档位，并带一句 redaction_note。
func TestHTTP_SecretValueMaskedInPreview(t *testing.T) {
	target := newCountingTarget(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"order": "` + r.URL.Path + `"}`))
	}), false)

	command := httpGetCommand(target.server.URL+"/orders/{token}", target.hostPort())
	command.DenyPrivate = boolPtr(false)
	command.Args = []core.ExecutorArg{{Name: "token", Secret: true}}
	fixture := newHTTPFixture(t, command, nil)

	job, err := fixture.run(context.Background(), "job-http-secret-preview", `{"params":{"token":"pa55,in-preview"}}`)
	require.NoError(t, err)

	require.NotNil(t, job.Exec)
	assert.NotContains(t, job.Exec.Preview, "pa55,in-preview", "原值不留进预览")
	assert.NotContains(t, job.Exec.Preview, "pa55%2Cin-preview", "URL 转义写法同样不留")
	assert.Contains(t, job.Exec.Preview, "orders/***", "预览仍要能看出打到了哪个地址")

	assert.Contains(t, fixture.artifact("job-http-secret-preview", "out"), "pa55,in-preview",
		"产物文件保持原文，由 api 的档位判断守着")
	assert.NotContains(t, fixture.logs.String(), "pa55,in-preview")
}
