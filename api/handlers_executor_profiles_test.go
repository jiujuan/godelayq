package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"godelayq/core"
	"godelayq/executor"
)

// 档位在线管理的端点测试（TASK-W05 卡 §5）。这一组的分量都在"真实"两个字上：
// 调度器、任务存储、档位文件、登记表、同步器全是生产实现，替身只出现在"这一步注定要失败"的那些用例里。
// 卡 §5.2 那条命门用例（建完档位不重启就能跑成一条任务）用真执行，
// 因此解释器取本机一定有的那个（Windows 用 cmd、其它平台用 sh），拿不到就跳过而不是假绿。

const profilesRoute = "/api/v1/executors/profiles"

// profileFixture 是一套完整装配。
type profileFixture struct {
	srv       *Server
	scheduler *core.Scheduler
	cfg       core.Config
	registry  *executor.Registry
	profiles  core.ExecutorProfileStore
	workspace string
}

// newProfileFixture 装配一台"开着档位在线管理"的服务。
//
// 用 NewServer + 两个真实 Option 而不是直接改字段：注入形状本身也是被测对象的一部分
// （requireExecutorProfiles 判的就是这两个字段齐不齐）。
func newProfileFixture(t *testing.T, sec Security, tune func(*core.Config), extra ...Option) *profileFixture {
	t.Helper()

	gin.SetMode(gin.TestMode)

	workspace := t.TempDir()
	cfg := core.DefaultConfig()
	cfg.Executors.Enabled = true
	cfg.Executors.WebEnabled = true
	cfg.Executors.Workspace = workspace
	cfg.Executors.Output.Dir = filepath.Join(t.TempDir(), "exec")
	if tune != nil {
		tune(&cfg)
	}

	registry, err := executor.NewRegistry(cfg, newTestLogger())
	require.NoError(t, err)

	profileStore, err := core.NewJSONFileExecutorProfileStore(filepath.Join(t.TempDir(), "exec-profiles.json"))
	require.NoError(t, err)

	jobs, err := core.NewJSONFileStore(filepath.Join(t.TempDir(), "jobs.json"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = jobs.Close() })

	artifacts, err := executor.NewArtifactStore(executor.ArtifactOptions{
		Dir:      cfg.Executors.Output.Dir,
		MaxBytes: cfg.Executors.Output.MaxBytes,
	}, newTestLogger())
	require.NoError(t, err)

	scheduler := core.NewScheduler(jobs, nil, nil)
	applier, err := executor.NewApplier(profileStore, scheduler, registry, artifacts, newTestLogger())
	require.NoError(t, err)

	opts := append([]Option{
		WithExecutorRegistry(registry),
		WithArtifacts(artifacts),
		WithExecutorProfileStore(profileStore),
		WithExecutorProfileApplier(applier),
	}, extra...)

	return &profileFixture{
		srv:       NewServer(scheduler, jobs, "0", sec, newTestLogger(), opts...),
		scheduler: scheduler,
		cfg:       cfg,
		registry:  registry,
		profiles:  profileStore,
		workspace: workspace,
	}
}

// profileBody 把一条档位定义编成请求体。字段名与档位文件里的记录一字不差，
// 所以"页面上写的"与"文件里存的"不会有第三种拼法。
func profileBody(t *testing.T, cmd core.ExecutorCommand) string {
	t.Helper()

	data, err := json.Marshal(core.NewExecutorProfileRecord(cmd))
	require.NoError(t, err)
	return string(data)
}

// echoProfile 造一条"打印固定一句"的脚本档位：用本机一定有的 shell 系外壳，
// 命令串属于配置侧（审查过的那一侧），payload 影响不到它。
func echoProfile(t *testing.T, name, line string) core.ExecutorCommand {
	t.Helper()

	cmd, ok := shellProfile(t, name, line)
	if !ok {
		t.Skipf("no shell runner available on this machine")
	}
	return cmd
}

func shellProfile(t *testing.T, name, line string) (core.ExecutorCommand, bool) {
	t.Helper()

	program, prefix := "sh", []string{"-c"}
	if runtime.GOOS == "windows" {
		program, prefix = "cmd", []string{"/c"}
	}
	body := "echo " + line
	if _, err := exec.LookPath(program); err != nil {
		return core.ExecutorCommand{}, false
	}
	return core.ExecutorCommand{
		Name:      name,
		Kind:      string(executor.KindBinary),
		Program:   program,
		FixedArgs: append(append([]string{}, prefix...), body),
		Timeout:   20 * time.Second,
	}, true
}

// allowProgram 把 program 加进 runtime_allow（配置里没写它的话探测与校验都会拒）。
func allowProgram(cfg *core.Config, program string) {
	cfg.Executors.RuntimeAllow = append(cfg.Executors.RuntimeAllow, program)
}

func opsToken(t *testing.T, srv *Server) http.Header {
	t.Helper()
	return bearer(login(t, srv, testOpsName, testPassword).AccessToken)
}

func decodeProfileResponse(t *testing.T, recorder *httptest.ResponseRecorder) ExecutorProfileResponse {
	t.Helper()

	var resp ExecutorProfileResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &resp), recorder.Body.String())
	return resp
}

func decodeErrorResponse(t *testing.T, recorder *httptest.ResponseRecorder) ErrorResponse {
	t.Helper()

	var failure ErrorResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &failure), recorder.Body.String())
	return failure
}

// jobStatus 读一条任务现在的状态（走端点，不碰存储：端点看到的才等于运维看到的）。
func jobStatus(t *testing.T, srv *Server, id string) string {
	t.Helper()

	recorder := doGet(t, srv, "/api/v1/jobs/"+id, nil)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())

	var job JobResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &job), recorder.Body.String())
	return job.Status
}

// createJob 建一条任务并返回它的 ID。
func createJob(t *testing.T, srv *Server, header http.Header, body string) string {
	t.Helper()

	recorder := doJSON(t, srv, http.MethodPost, "/api/v1/jobs", body, header)
	require.Equal(t, http.StatusCreated, recorder.Code, recorder.Body.String())

	var job JobResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &job), recorder.Body.String())
	return job.ID
}

func waitForStatus(t *testing.T, srv *Server, id, want string, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if status := jobStatus(t, srv, id); status == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("在 %v 内任务 %s 没有变成 %s（最后一次读到 %s）", timeout, id, want, jobStatus(t, srv, id))
}

// TestExecutorProfiles_RoleGate 守住写端点的档位门槛：
// ops 是唯一能改"这台机器能执行什么"的身份，machine 静态凭据也不行。
func TestExecutorProfiles_RoleGate(t *testing.T) {
	sec := accountsSecurity(t)
	sec.Auth.Token = testToken
	fixture := newProfileFixture(t, sec, nil)
	srv := fixture.srv

	body := profileBody(t, core.ExecutorCommand{
		Name: "gated", Kind: string(executor.KindHTTP), Method: "GET",
		URLTemplate: "https://example.invalid/ping", AllowedHosts: []string{"example.invalid"},
	})

	for _, who := range []string{testViewerName, testOperatorName, testAdminName} {
		header := bearer(login(t, srv, who, testPassword).AccessToken)
		assert.Equal(t, http.StatusForbidden,
			doJSON(t, srv, http.MethodPost, profilesRoute, body, header).Code, who+" 不该能建档位")
	}

	// machine 身份的静态 token：能提交任务，但档位写端点要 ops
	assert.Equal(t, http.StatusForbidden,
		doJSON(t, srv, http.MethodPost, profilesRoute, body, bearer(testToken)).Code)

	recorder := doJSON(t, srv, http.MethodPost, profilesRoute, body, opsToken(t, srv))
	require.Equal(t, http.StatusCreated, recorder.Code, recorder.Body.String())

	// 同一个档位名的改与删也要求 ops：不然门槛只建了一半
	assert.Equal(t, http.StatusForbidden,
		doJSON(t, srv, http.MethodPut, profilesRoute+"/gated", body, bearer(testToken)).Code)
	assert.Equal(t, http.StatusForbidden,
		doJSON(t, srv, http.MethodDelete, profilesRoute+"/gated", "", bearer(login(t, srv, testAdminName, testPassword).AccessToken)).Code)

	// 读端点不受影响：它一直是 reader
	assert.Equal(t, http.StatusOK, doGet(t, srv, "/api/v1/executors", bearer(testToken)).Code)
}

// TestExecutorProfiles_TwoKindsOfUnavailable 证明两种 503 在文案上分得开：
// "这台没打开在线管理"与"打开了却没装配依赖"，运维要的处理方式完全不同。
func TestExecutorProfiles_TwoKindsOfUnavailable(t *testing.T) {
	// 1. web_enabled=false：连依赖都不该被读到
	cfgTune := func(cfg *core.Config) { cfg.Executors.WebEnabled = false }
	disabled := newProfileFixture(t, Security{}, cfgTune)
	recorder := doJSON(t, disabled.srv, http.MethodPost, profilesRoute, "{}", nil)
	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
	failure := decodeErrorResponse(t, recorder)
	assert.Contains(t, failure.Message, "not enabled")
	assert.Contains(t, failure.Details, "web_enabled")

	// 2. 打开了却没注入依赖
	unwired := newProfileFixture(t, Security{}, nil)
	unwired.srv.profileStore = nil
	unwired.srv.profiles = nil
	recorder = doJSON(t, unwired.srv, http.MethodPost, profilesRoute, "{}", nil)
	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
	failure = decodeErrorResponse(t, recorder)
	assert.Contains(t, failure.Message, "not configured")
	assert.Contains(t, failure.Details, "WithExecutorProfileStore")

	// 状态码之外，两条分支的文案必须互不包含，否则前端没法分派提示
	assert.NotContains(t, failure.Details, "web_enabled=true")
}

// TestExecutorProfiles_CreateThenRunWithoutRestart 是整条系列的命门（卡 §5.2）：
// 建一条档位 → 不重启 → 提交该类型任务 → 跑到 success → 读得到输出。
func TestExecutorProfiles_CreateThenRunWithoutRestart(t *testing.T) {
	cmd := echoProfile(t, "smoke_run", "created-without-restart")
	fixture := newProfileFixture(t, Security{}, func(cfg *core.Config) {
		allowProgram(cfg, cmd.Program)
	})
	srv := fixture.srv
	fixture.scheduler.Start()
	t.Cleanup(func() { fixture.scheduler.Stop() })

	recorder := doJSON(t, srv, http.MethodPost, profilesRoute, profileBody(t, cmd), nil)
	require.Equal(t, http.StatusCreated, recorder.Code, recorder.Body.String())
	created := decodeProfileResponse(t, recorder)
	assert.Equal(t, "exec.smoke_run", created.Key)
	assert.True(t, created.RuntimeOK, created.Reason)

	// 文件里有了、登记表里有了、调度器也认得这个类型
	stored, found, err := fixture.profiles.Get("smoke_run")
	require.NoError(t, err)
	require.True(t, found, "响应成功的前提是那条记录真的落盘了")
	assert.Equal(t, "smoke_run", stored.Name)
	_, ok := fixture.registry.Lookup("exec.smoke_run")
	require.True(t, ok)
	_, ok = fixture.scheduler.LookupHandler("exec.smoke_run")
	require.True(t, ok, "热注册必须已经生效")

	id := createJob(t, srv, nil, `{"name":"exec.smoke_run","payload":{"args":{}}}`)
	waitForStatus(t, srv, id, "success", 15*time.Second)

	result := decodeJobResult(t, doGet(t, srv, "/api/v1/jobs/"+id+"/result?stream=out", nil))
	assert.Contains(t, result.Content, "created-without-restart")
}

// TestExecutorProfiles_CreateConflicts 覆盖两种同名：文件里已有、配置里已有。
// 两条都必须在落盘之前就拒掉——留下"文件里有但没生效"是最难查的状态。
func TestExecutorProfiles_CreateConflicts(t *testing.T) {
	httpCmd := core.ExecutorCommand{
		Name: "dup", Kind: string(executor.KindHTTP), Method: "GET",
		URLTemplate: "https://example.invalid/a", AllowedHosts: []string{"example.invalid"},
	}
	fixture := newProfileFixture(t, Security{}, func(cfg *core.Config) {
		cfg.Executors.Commands = []core.ExecutorCommand{{
			Name: "from_config", Kind: string(executor.KindHTTP), Method: "GET",
			URLTemplate: "https://example.invalid/b", AllowedHosts: []string{"example.invalid"},
		}}
	})
	srv := fixture.srv

	require.Equal(t, http.StatusCreated,
		doJSON(t, srv, http.MethodPost, profilesRoute, profileBody(t, httpCmd), nil).Code)

	recorder := doJSON(t, srv, http.MethodPost, profilesRoute, profileBody(t, httpCmd), nil)
	require.Equal(t, http.StatusConflict, recorder.Code)
	assert.Contains(t, decodeErrorResponse(t, recorder).Details, "PUT")

	// 只差大小写也算重名（存储主键忽略大小写）
	assert.Equal(t, http.StatusConflict,
		doJSON(t, srv, http.MethodPost, profilesRoute,
			profileBody(t, core.ExecutorCommand{Name: "DUP", Kind: string(executor.KindHTTP),
				Method: "GET", URLTemplate: "https://example.invalid/c", AllowedHosts: []string{"example.invalid"}}), nil).Code)

	// 与配置侧同名：409，而且配置文件那份没被碰、文件里也没多出记录
	recorder = doJSON(t, srv, http.MethodPost, profilesRoute,
		profileBody(t, core.ExecutorCommand{Name: "from_config", Kind: string(executor.KindHTTP),
			Method: "GET", URLTemplate: "https://example.invalid/d", AllowedHosts: []string{"example.invalid"}}), nil)
	require.Equal(t, http.StatusConflict, recorder.Code)
	assert.Contains(t, decodeErrorResponse(t, recorder).Details, "executors.commands")

	_, found, err := fixture.profiles.Get("from_config")
	require.NoError(t, err)
	assert.False(t, found, "冲突必须在落盘之前判掉")

	source, ok := fixture.registry.SourceOf("exec.from_config")
	require.True(t, ok)
	assert.Equal(t, executor.SourceConfig, source, "配置那条不该被顶掉")
}

// TestExecutorProfiles_RejectsInvalidBodies 覆盖三种"写法不对"：未知键、非法字段组合、非法名字与超时。
// 共同点是一条都不该落到文件里。
func TestExecutorProfiles_RejectsInvalidBodies(t *testing.T) {
	fixture := newProfileFixture(t, Security{}, nil)
	srv := fixture.srv

	cases := map[string]string{
		"unknown key":            `{"name":"typo","kind":"http","method":"GET","url_template":"https://example.invalid","allowed_hosts":["example.invalid"],"scriptt":"x"}`,
		"script without runtime": `{"name":"no_runtime","kind":"script","script":"scripts/a.mjs"}`,
		"bad name":               `{"name":"运维 组","kind":"http","method":"GET","url_template":"https://example.invalid","allowed_hosts":["example.invalid"]}`,
		"bad timeout":            `{"name":"bad_timeout","kind":"http","method":"GET","url_template":"https://example.invalid","allowed_hosts":["example.invalid"],"timeout":"10w"}`,
		"empty body":             ``,
		"missing kind":           `{"name":"no_kind"}`,
	}
	for label, body := range cases {
		recorder := doJSON(t, srv, http.MethodPost, profilesRoute, body, nil)
		assert.Equal(t, http.StatusBadRequest, recorder.Code, label+" -> "+recorder.Body.String())
	}

	records, err := fixture.profiles.List()
	require.NoError(t, err)
	assert.Empty(t, records, "被拒的请求一条都不该进档位文件")
	assert.Empty(t, fixture.registry.Keys())
}

// TestExecutorProfiles_UpdateRules 覆盖 PUT 的四条前置：不存在 404、配置档位 409、
// 改名 400、禁改字段 400（D7，文案要给出路）。
func TestExecutorProfiles_UpdateRules(t *testing.T) {
	cmd := core.ExecutorCommand{
		Name: "editable", Kind: string(executor.KindHTTP), Method: "GET",
		URLTemplate: "https://example.invalid/one", AllowedHosts: []string{"example.invalid"},
		Timeout: 30 * time.Second,
	}
	fixture := newProfileFixture(t, Security{}, func(cfg *core.Config) {
		cfg.Executors.Commands = []core.ExecutorCommand{{
			Name: "locked", Kind: string(executor.KindHTTP), Method: "GET",
			URLTemplate: "https://example.invalid/two", AllowedHosts: []string{"example.invalid"},
		}}
	})
	srv := fixture.srv

	require.Equal(t, http.StatusCreated,
		doJSON(t, srv, http.MethodPost, profilesRoute, profileBody(t, cmd), nil).Code)

	// 正常改一个允许改的字段：超时
	changed := cmd
	changed.Timeout = 2 * time.Minute
	recorder := doJSON(t, srv, http.MethodPut, profilesRoute+"/editable", profileBody(t, changed), nil)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	profile, ok := fixture.registry.Lookup("exec.editable")
	require.True(t, ok)
	assert.Equal(t, 2*time.Minute, profile.Timeout)

	// 改 kind / script / program 都是 400，文案点名"删了重建"
	mutate := func(edit func(*core.ExecutorProfileRecord)) string {
		record := core.NewExecutorProfileRecord(cmd)
		edit(&record)
		data, err := json.Marshal(record)
		require.NoError(t, err)
		return string(data)
	}
	for label, body := range map[string]string{
		"kind":    mutate(func(r *core.ExecutorProfileRecord) { r.Kind = "script"; r.Runtime = "node"; r.Script = "scripts/x.mjs" }),
		"script":  mutate(func(r *core.ExecutorProfileRecord) { r.Script = "scripts/x.mjs" }),
		"program": mutate(func(r *core.ExecutorProfileRecord) { r.Program = "other"; r.Kind = "binary" }),
	} {
		recorder := doJSON(t, srv, http.MethodPut, profilesRoute+"/editable", body, nil)
		require.Equal(t, http.StatusBadRequest, recorder.Code, label+": "+recorder.Body.String())
		failure := decodeErrorResponse(t, recorder)
		assert.Contains(t, failure.Message, "cannot be changed")
		assert.Contains(t, failure.Details, "delete it and create a new one")
	}

	// 体内 name 与路径不一致（忽略大小写之外都算不一致）
	assert.Equal(t, http.StatusBadRequest, doJSON(t, srv, http.MethodPut, profilesRoute+"/editable",
		profileBody(t, core.ExecutorCommand{Name: "another", Kind: string(executor.KindHTTP),
			Method: "GET", URLTemplate: "https://example.invalid/x", AllowedHosts: []string{"example.invalid"}}), nil).Code)
	// 只差大小写算同一条：主键就是忽略大小写的
	recorder = doJSON(t, srv, http.MethodPut, profilesRoute+"/editable",
		strings.Replace(profileBody(t, changed), `"name":"editable"`, `"name":"Editable"`, 1), nil)
	assert.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())

	// 体内不写 name 时取路径上的那个（卡 §3.3 第 2 步）
	noName, err := json.Marshal(core.NewExecutorProfileRecord(changed))
	require.NoError(t, err)
	recorder = doJSON(t, srv, http.MethodPut, profilesRoute+"/editable",
		strings.Replace(string(noName), `"name":"editable",`, ``, 1), nil)
	assert.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())

	// 配置侧档位与不存在的名字：都要先过"体内 name 与路径一致"那一关
	locked := cmd
	locked.Name = "locked"
	recorder = doJSON(t, srv, http.MethodPut, profilesRoute+"/locked", profileBody(t, locked), nil)
	require.Equal(t, http.StatusConflict, recorder.Code)
	assert.Contains(t, decodeErrorResponse(t, recorder).Details, "executors.commands")

	ghost := cmd
	ghost.Name = "missing_one"
	recorder = doJSON(t, srv, http.MethodPut, profilesRoute+"/missing_one", profileBody(t, ghost), nil)
	require.Equal(t, http.StatusNotFound, recorder.Code)
	assert.Contains(t, decodeErrorResponse(t, recorder).Message, "not found")
}

// TestExecutorProfiles_DeletePausesPendingAndLeavesRunning 是删除语义的主用例（卡 §5.6）：
// 待执行的钉住、正在执行的一条都不动、别种状态与别的类型都不受影响。
func TestExecutorProfiles_DeletePausesPendingAndLeavesRunning(t *testing.T) {
	slow, ok := shellProfile(t, "slow_run", "still-running-after-delete")
	if !ok {
		t.Skipf("no shell runner available on this machine")
	}
	if runtime.GOOS == "windows" {
		slow.FixedArgs = []string{"/c", "ping -n 4 127.0.0.1"}
	} else {
		slow.FixedArgs = []string{"-c", "sleep 3"}
	}
	quick := echoProfile(t, "quick_run", "quick")

	fixture := newProfileFixture(t, Security{}, func(cfg *core.Config) {
		allowProgram(cfg, slow.Program)
		allowProgram(cfg, quick.Program)
	})
	srv := fixture.srv
	fixture.scheduler.Start()
	t.Cleanup(func() { fixture.scheduler.Stop() })

	// 两种档位都要真的建出来：别的类型那条任务是用来证明"删除没有波及它"的
	for _, cmd := range []core.ExecutorCommand{slow, quick} {
		require.Equal(t, http.StatusCreated,
			doJSON(t, srv, http.MethodPost, profilesRoute, profileBody(t, cmd), nil).Code, cmd.Name)
	}

	// 三条待执行的同类型任务 + 一条别的类型
	pending := make([]string, 0, 3)
	for i := 0; i < 3; i++ {
		pending = append(pending, createJob(t, srv, nil,
			`{"name":"exec.slow_run","delay":"30m","payload":{"args":{}}}`))
	}
	other := createJob(t, srv, nil, `{"name":"exec.quick_run","delay":"30m","payload":{"args":{}}}`)

	// 一条真的在跑的执行
	running := createJob(t, srv, nil, `{"name":"exec.slow_run","payload":{"args":{}}}`)
	waitForStatus(t, srv, running, "running", 15*time.Second)

	recorder := doJSON(t, srv, http.MethodDelete, profilesRoute+"/slow_run", "", nil)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	var resp ExecutorProfileDeleteResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &resp), recorder.Body.String())
	assert.Equal(t, "exec.slow_run", resp.Key)
	assert.Equal(t, 3, resp.PausedJobs, "三条待执行的都要钉住")
	assert.Equal(t, 1, resp.RunningJobs, "在跑的那条要如实报出来，而且只是报、不动它")

	for _, id := range pending {
		assert.Equal(t, "paused", jobStatus(t, srv, id), id)
	}
	assert.Equal(t, "pending", jobStatus(t, srv, other), "别的类型一条都不该动")

	// 档位在文件、登记表、调度器三处都消失
	_, found, err := fixture.profiles.Get("slow_run")
	require.NoError(t, err)
	assert.False(t, found)
	_, ok = fixture.registry.Lookup("exec.slow_run")
	assert.False(t, ok)
	_, ok = fixture.scheduler.LookupHandler("exec.slow_run")
	assert.False(t, ok, "删除要连处理函数一起摘掉")

	// 在跑的那条按自己的节奏跑完，没被取消也没被改判
	waitForStatus(t, srv, running, "success", 30*time.Second)

	// 重复删除同名的已删档位：404
	recorder = doJSON(t, srv, http.MethodDelete, profilesRoute+"/slow_run", "", nil)
	assert.Equal(t, http.StatusNotFound, recorder.Code)
}

// TestExecutorProfiles_DeleteBlockAndBadStrategy 覆盖 ?jobs 的白名单与 block 语义。
func TestExecutorProfiles_DeleteBlockAndBadStrategy(t *testing.T) {
	cmd := core.ExecutorCommand{
		Name: "blocked", Kind: string(executor.KindHTTP), Method: "GET",
		URLTemplate: "https://example.invalid/one", AllowedHosts: []string{"example.invalid"},
	}
	fixture := newProfileFixture(t, Security{}, nil)
	srv := fixture.srv

	require.Equal(t, http.StatusCreated,
		doJSON(t, srv, http.MethodPost, profilesRoute, profileBody(t, cmd), nil).Code)
	id := createJob(t, srv, nil, `{"name":"exec.blocked","delay":"30m","payload":{"args":{}}}`)

	recorder := doJSON(t, srv, http.MethodDelete, profilesRoute+"/blocked?jobs=whatever", "", nil)
	require.Equal(t, http.StatusBadRequest, recorder.Code)
	assert.Contains(t, decodeErrorResponse(t, recorder).Details, "pause or block")

	recorder = doJSON(t, srv, http.MethodDelete, profilesRoute+"/blocked?jobs=block", "", nil)
	require.Equal(t, http.StatusConflict, recorder.Code)
	failure := decodeErrorResponse(t, recorder)
	assert.Contains(t, failure.Details, "1 pending")

	// block 拒绝之后什么都没动：任务还在排期、档位还在、处理函数还在
	assert.Equal(t, "pending", jobStatus(t, srv, id))
	_, found, err := fixture.profiles.Get("blocked")
	require.NoError(t, err)
	assert.True(t, found)
	_, ok := fixture.scheduler.LookupHandler("exec.blocked")
	assert.True(t, ok)

	// 清干净之后 block 就能删了
	recorder = doJSON(t, srv, http.MethodDelete, "/api/v1/jobs/"+id, "", nil)
	require.Equal(t, http.StatusNoContent, recorder.Code, recorder.Body.String())

	recorder = doJSON(t, srv, http.MethodDelete, profilesRoute+"/blocked?jobs=block", "", nil)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
}

// TestExecutorProfiles_UnavailableProfileIsStillStored 钉住那条反直觉口径（设计文档 §5.3）：
// 探测失败不拒绝保存；但提交期仍然会拒。
func TestExecutorProfiles_UnavailableProfileIsStillStored(t *testing.T) {
	fixture := newProfileFixture(t, Security{}, nil)
	srv := fixture.srv

	body := profileBody(t, core.ExecutorCommand{
		Name: "not_deployed", Kind: string(executor.KindScript), Runtime: "node",
		Script: "scripts/definitely-not-here.mjs",
	})
	recorder := doJSON(t, srv, http.MethodPost, profilesRoute, body, nil)
	require.Equal(t, http.StatusCreated, recorder.Code, recorder.Body.String())

	created := decodeProfileResponse(t, recorder)
	assert.False(t, created.RuntimeOK)
	assert.Contains(t, created.Reason, "definitely-not-here.mjs")

	_, ok := fixture.registry.Lookup("exec.not_deployed")
	require.True(t, ok, "不可用的档位也要在表里，运维才看得见它")
	_, ok = fixture.scheduler.LookupHandler("exec.not_deployed")
	assert.True(t, ok, "注册与可用是两件事")

	recorder = doJSON(t, srv, http.MethodPost, "/api/v1/jobs",
		`{"name":"exec.not_deployed","payload":{"args":{}}}`, nil)
	require.Equal(t, http.StatusBadRequest, recorder.Code)
	assert.Contains(t, decodeErrorResponse(t, recorder).Message, "not available")
}

// TestExecutorProfiles_StoreAndApplyFailures 覆盖 I2 的两个方向：
// 写盘失败时注册表与调度器一行不动；生效失败时文件回到原值。
func TestExecutorProfiles_StoreAndApplyFailures(t *testing.T) {
	cmd := core.ExecutorCommand{
		Name: "rolls_back", Kind: string(executor.KindHTTP), Method: "GET",
		URLTemplate: "https://example.invalid/one", AllowedHosts: []string{"example.invalid"},
		Timeout: 45 * time.Second,
	}

	t.Run("save fails", func(t *testing.T) {
		fixture := newProfileFixture(t, Security{}, nil)
		failing := &failingProfileStore{ExecutorProfileStore: fixture.profiles, saveErr: errors.New("simulated disk failure")}
		fixture.srv.profileStore = failing

		recorder := doJSON(t, fixture.srv, http.MethodPost, profilesRoute, profileBody(t, cmd), nil)
		require.Equal(t, http.StatusInternalServerError, recorder.Code, recorder.Body.String())

		failing.saveErr = nil
		_, found, err := fixture.profiles.Get("rolls_back")
		require.NoError(t, err)
		assert.False(t, found, "写盘失败不该留下半个记录")
		_, ok := fixture.registry.Lookup("exec.rolls_back")
		assert.False(t, ok, "写盘失败时注册表一行都不该动")
		_, ok = fixture.scheduler.LookupHandler("exec.rolls_back")
		assert.False(t, ok)
	})

	t.Run("apply fails on create rolls the file back", func(t *testing.T) {
		fixture := newProfileFixture(t, Security{}, nil)
		fixture.srv.profiles = &failingApplier{}

		recorder := doJSON(t, fixture.srv, http.MethodPost, profilesRoute, profileBody(t, cmd), nil)
		require.Equal(t, http.StatusInternalServerError, recorder.Code)
		assert.Contains(t, decodeErrorResponse(t, recorder).Details, "rolled back")

		_, found, err := fixture.profiles.Get("rolls_back")
		require.NoError(t, err)
		assert.False(t, found, "生效失败要把刚写的那条删回去")
	})

	t.Run("apply fails on update restores the old record", func(t *testing.T) {
		fixture := newProfileFixture(t, Security{}, nil)
		require.Equal(t, http.StatusCreated,
			doJSON(t, fixture.srv, http.MethodPost, profilesRoute, profileBody(t, cmd), nil).Code)

		updated := cmd
		updated.Timeout = 2 * time.Minute
		fixture.srv.profiles = &failingApplier{}
		recorder := doJSON(t, fixture.srv, http.MethodPut, profilesRoute+"/rolls_back",
			profileBody(t, updated), nil)
		require.Equal(t, http.StatusInternalServerError, recorder.Code)

		stored, found, err := fixture.profiles.Get("rolls_back")
		require.NoError(t, err)
		require.True(t, found)
		assert.Equal(t, "45s", stored.Timeout, "回滚要把原值写回去")
	})

	t.Run("apply fails on delete restores the record", func(t *testing.T) {
		fixture := newProfileFixture(t, Security{}, nil)
		require.Equal(t, http.StatusCreated,
			doJSON(t, fixture.srv, http.MethodPost, profilesRoute, profileBody(t, cmd), nil).Code)

		fixture.srv.profiles = &failingApplier{}
		recorder := doJSON(t, fixture.srv, http.MethodDelete, profilesRoute+"/rolls_back", "", nil)
		require.Equal(t, http.StatusInternalServerError, recorder.Code)

		_, found, err := fixture.profiles.Get("rolls_back")
		require.NoError(t, err)
		assert.True(t, found, "删不掉生效就别删文件")
	})
}

// failingProfileStore 只让 Save 失败，其余照原样走真实存储。
type failingProfileStore struct {
	core.ExecutorProfileStore
	saveErr error
}

func (f *failingProfileStore) Save(record core.ExecutorProfileRecord) error {
	if f.saveErr != nil {
		return f.saveErr
	}
	return f.ExecutorProfileStore.Save(record)
}

// failingApplier 让校验通过、生效失败：那正是"文件与内存不一致"要处理的组合。
type failingApplier struct{}

func (failingApplier) Validate(cmd core.ExecutorCommand) (*executor.Profile, executor.ProbeResult, error) {
	return &executor.Profile{Name: cmd.Name}, executor.ProbeResult{Available: true}, nil
}

func (failingApplier) Apply() (executor.ApplyResult, error) {
	return executor.ApplyResult{}, errors.New("simulated apply failure")
}

// TestExecutorProfiles_AuditActions 要求三个动作各产生一行台账，且行里没有 env 取值与请求体原文。
// 另加一条反证：把映射表里的一行去掉，那一行不会消失，而是变成 other。
func TestExecutorProfiles_AuditActions(t *testing.T) {
	capture := &auditCapture{}
	sec := accountsSecurity(t)
	sec.Auth.Token = testToken

	cmd := core.ExecutorCommand{
		Name: "audited", Kind: string(executor.KindScript), Runtime: "node",
		Script: "scripts/audited-canary.mjs",
		Env:    map[string]string{envCanaryKey: envCanaryValue},
	}
	fixture := newProfileFixture(t, sec, nil, WithAuditLog(capture, capture))
	srv := fixture.srv
	header := opsToken(t, srv)

	// 登录本身占一行，用例只看之后的写操作
	capture.reset()

	recorder := doJSON(t, srv, http.MethodPost, profilesRoute, profileBody(t, cmd), header)
	require.Equal(t, http.StatusCreated, recorder.Code, recorder.Body.String())
	entry := capture.only(t)
	assert.Equal(t, "executor.profile_create", entry.Action)
	assert.Equal(t, profilesRoute, entry.Route)
	assert.Equal(t, testOpsName, entry.Actor)
	for _, value := range fieldStrings(entry) {
		assert.NotContains(t, value, envCanaryValue, "台账不许带上固定环境变量的取值")
		assert.NotContains(t, value, "scripts/audited-canary.mjs", "台账不许带上请求体原文")
	}

	updated := cmd
	updated.Timeout = 2 * time.Minute
	capture.reset()
	require.Equal(t, http.StatusOK,
		doJSON(t, srv, http.MethodPut, profilesRoute+"/audited", profileBody(t, updated), header).Code)
	assert.Equal(t, "executor.profile_update", capture.only(t).Action)

	capture.reset()
	require.Equal(t, http.StatusOK,
		doJSON(t, srv, http.MethodDelete, profilesRoute+"/audited", "", header).Code)
	assert.Equal(t, "executor.profile_delete", capture.only(t).Action)

	// 三个动作词都进了查询端的合法列表（漏加的话 ?action= 会被当成非法取值拒掉）
	actions := strings.Join(AuditActions(), ",")
	for _, want := range []string{"executor.profile_create", "executor.profile_update", "executor.profile_delete"} {
		assert.Contains(t, actions, want)
	}

	// 反证：映射缺一行时不静默丢行，而是记成 other
	defer func() { auditActions[http.MethodPost+" "+profilesRoute] = "executor.profile_create" }()
	delete(auditActions, http.MethodPost+" "+profilesRoute)
	capture.reset()
	require.Equal(t, http.StatusCreated,
		doJSON(t, srv, http.MethodPost, profilesRoute, profileBody(t, cmd), header).Code)
	assert.Equal(t, "other", capture.only(t).Action)
}

const (
	envCanaryKey   = "TOKEN"
	envCanaryValue = "canary-env-value-must-not-leak"
)

// TestExecutorProfiles_EnvValuesAreNotEchoed 证明响应与只读端点都搜不到 env 的取值。
// 键名可以出现（表单要知道能覆盖哪些变量），值一律不外露。
func TestExecutorProfiles_EnvValuesAreNotEchoed(t *testing.T) {
	fixture := newProfileFixture(t, Security{}, nil)
	srv := fixture.srv

	cmd := core.ExecutorCommand{
		Name: "with_env", Kind: string(executor.KindScript), Runtime: "node",
		Script:   "scripts/with_env.mjs",
		Env:      map[string]string{envCanaryKey: envCanaryValue},
		EnvAllow: []string{"TRACE_ID"},
	}
	recorder := doJSON(t, srv, http.MethodPost, profilesRoute, profileBody(t, cmd), nil)
	require.Equal(t, http.StatusCreated, recorder.Code, recorder.Body.String())
	assert.NotContains(t, recorder.Body.String(), envCanaryValue)
	// env_allow 里的变量名照常给出（表单要用），固定 env 的取值不给
	assert.Contains(t, recorder.Body.String(), "TRACE_ID")

	listed := doGet(t, srv, "/api/v1/executors", nil)
	require.Equal(t, http.StatusOK, listed.Code)
	assert.NotContains(t, listed.Body.String(), envCanaryValue)

	// 绝对路径同样不外露：这条档位写的是模板与主机名，换 script 档位时也是一样的判据
	assert.NotContains(t, listed.Body.String(), fixture.workspace)
}

// TestExecutorProfiles_RecordRoundTripsThroughTheFile 证明页面上写的就是文件里存的：
// 重启（这里用重新打开一份存储读同一份内容）之后档位还在、写法没变形。
func TestExecutorProfiles_RecordRoundTripsThroughTheFile(t *testing.T) {
	fixture := newProfileFixture(t, Security{}, nil)
	cmd := core.ExecutorCommand{
		Name: "round_trip", Kind: string(executor.KindScript), Runtime: "node",
		Script: "scripts/round_trip.mjs", ArgsRender: []string{"--day={day}"},
		Args: []core.ExecutorArg{{Name: "day", Required: true, Pattern: "^(yesterday|today)$"}},
	}

	require.Equal(t, http.StatusCreated,
		doJSON(t, fixture.srv, http.MethodPost, profilesRoute, profileBody(t, cmd), nil).Code)

	stored, found, err := fixture.profiles.Get("round_trip")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "scripts/round_trip.mjs", stored.Script)
	assert.Equal(t, "^(yesterday|today)$", stored.Args[0].Pattern)
	assert.False(t, stored.CreatedAt.IsZero(), "时间戳由存储补齐")
}

// TestExecutorProfiles_WriteWithoutProfileFileKeepsWorking 是默认关闭那条 DoD 的端点侧对照：
// 同一条链路上 web_enabled=false 时读端点照常、写端点全拒，且档位文件一次都没被读过。
func TestExecutorProfiles_WriteWithoutProfileFileKeepsWorking(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "exec-profiles.json")
	fixture := newProfileFixture(t, Security{}, func(cfg *core.Config) {
		cfg.Executors.WebEnabled = false
		cfg.Executors.ProfilesPath = path
	})

	assert.Equal(t, http.StatusOK, doGet(t, fixture.srv, "/api/v1/executors", nil).Code)
	recorder := doJSON(t, fixture.srv, http.MethodPost, profilesRoute, `{"name":"late"}`, nil)
	assert.Equal(t, http.StatusServiceUnavailable, recorder.Code)

	_, err := os.Stat(filepath.Dir(path))
	assert.True(t, os.IsNotExist(err), "关闭状态下不该创建档位文件的目录")
}
