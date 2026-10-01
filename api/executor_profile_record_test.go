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

// 档位定义的读取口与 env 的保留规则（TASK-W08 前置，收掉 W07 登记的 D-0702）。
//
// 页面上要"编辑一条已有档位"，就得看得见它的 runtime/script/args_render 这些定义字段——
// 而 GET /executors 的每一行说的是处境，不是定义。这一组用例同时守住另一件事：
// 定义给回来可以，固定环境变量的取值不行，并且"表单没带 env"不能被理解成"把 env 清空"。

const envCanaryValue2 = "record-endpoint-canary"

// recordFixture 建一条带固定环境变量的 store 档位，返回它的名字与创建响应。
func recordProfile(t *testing.T) core.ExecutorCommand {
	t.Helper()
	return core.ExecutorCommand{
		Name: "ui_py", Kind: string(executor.KindScript),
		Runtime: "python", Script: "scripts/py_hello.py",
		ArgsRender:  []string{"--day={day}"},
		Args:        []core.ExecutorArg{{Name: "day", Required: true, Pattern: "^(yesterday|today)$"}},
		Env:         map[string]string{"REPORT_HOME": envCanaryValue2, "LANG_PACK": "zh"},
		EnvAllow:    []string{"TRACE_ID"},
		Cwd:         ".",
		Timeout:     2 * time.Minute,
		MaxParallel: 2,
		RetryOnExit: []int{75},
	}
}

// createRecordProfile 建档位；header 在没有鉴权的那几条用例里传 nil。
func createRecordProfile(t *testing.T, fixture *profileFixture, header http.Header) {
	t.Helper()
	recorder := doJSON(t, fixture.srv, http.MethodPost, profilesRoute, profileBody(t, recordProfile(t)), header)
	require.Equal(t, http.StatusCreated, recorder.Code, recorder.Body.String())
}

// withoutKeys 把一段 JSON 对象里的若干键去掉，模拟"表单没带这个字段"。
func withoutKeys(t *testing.T, body string, keys ...string) string {
	t.Helper()
	var obj map[string]any
	require.NoError(t, json.Unmarshal([]byte(body), &obj))
	for _, key := range keys {
		delete(obj, key)
	}
	data, err := json.Marshal(obj)
	require.NoError(t, err)
	return string(data)
}

func storedProfileRecord(t *testing.T, fixture *profileFixture) core.ExecutorProfileRecord {
	t.Helper()
	record, found, err := fixture.profiles.Get("ui_py")
	require.NoError(t, err)
	require.True(t, found)
	return record
}

func TestExecutorProfileRecord_ReturnsTheDefinition(t *testing.T) {
	fixture := newProfileFixture(t, Security{}, nil)
	createRecordProfile(t, fixture, nil)

	recorder := doGet(t, fixture.srv, profilesRoute+"/ui_py", nil)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())

	var resp ExecutorProfileRecordResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &resp), recorder.Body.String())
	assert.Equal(t, "ui_py", resp.Name)
	assert.Equal(t, "python", resp.Runtime)
	assert.Equal(t, "scripts/py_hello.py", resp.Script)
	assert.Equal(t, []string{"--day={day}"}, resp.ArgsRender)
	require.Len(t, resp.Args, 1)
	assert.Equal(t, "day", resp.Args[0].Name)
	assert.True(t, resp.Args[0].Required)
	assert.Equal(t, "2m0s", resp.Timeout)
	assert.Equal(t, 2, resp.MaxParallel)
	assert.Equal(t, []int{75}, resp.RetryOnExit)
	assert.Equal(t, []string{"TRACE_ID"}, resp.EnvAllow)

	// 键名给得出、取值不外露，连键名都不该出现在值的位置上
	assert.Equal(t, []string{"LANG_PACK", "REPORT_HOME"}, resp.EnvKeys, "按字典序，界面不用自己排")
	assert.NotContains(t, recorder.Body.String(), envCanaryValue2)
	assert.NotContains(t, recorder.Body.String(), `"env"`, "env 这个键整个省略，不给空对象（那是清空的写法）")
}

func TestExecutorProfileRecord_EnvRoundTripsIntoTheFile(t *testing.T) {
	fixture := newProfileFixture(t, Security{}, nil)
	createRecordProfile(t, fixture, nil)

	body, found, err := fixture.profiles.Get("ui_py")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, envCanaryValue2, body.Env["REPORT_HOME"], "端点不外露取值，但文件里必须还是那份")
}

func TestExecutorProfileUpdate_KeepsEnvWhenTheBodyOmitsIt(t *testing.T) {
	fixture := newProfileFixture(t, Security{}, nil)
	createRecordProfile(t, fixture, nil)

	record, found, err := fixture.profiles.Get("ui_py")
	require.NoError(t, err)
	require.True(t, found)

	// 只改超时：把存着的记录去掉 env 之后发回去，等价于页面上"没动那一栏"
	obj := mustDecode(t, withoutKeys(t, string(mustMarshal(t, record)), "env", "created_at", "updated_at"))
	obj["timeout"] = "3m"

	recorder := doJSON(t, fixture.srv, http.MethodPut, profilesRoute+"/ui_py", string(mustMarshal(t, obj)), nil)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())

	after := storedProfileRecord(t, fixture)
	assert.Equal(t, "3m", after.Timeout)
	assert.Equal(t, envCanaryValue2, after.Env["REPORT_HOME"], "没带 env 就是不改")
	assert.Equal(t, "zh", after.Env["LANG_PACK"])

	// 显式给空对象才是清空
	obj["env"] = map[string]any{}
	recorder = doJSON(t, fixture.srv, http.MethodPut, profilesRoute+"/ui_py", string(mustMarshal(t, obj)), nil)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	assert.Empty(t, storedProfileRecord(t, fixture).Env, `"env": {} 是明确的清空`)
}

// 往返一致：读端点给回来的东西，去掉只读键之后原样 PUT 回去必须成功且内容不变。
// 这条是 D-0702 的收口判据——W07 那次"响应不能当请求体"是因为形状根本不同，
// 而这个端点就是为"取回来改完再放回去"设计的。
func TestExecutorProfileRecord_RoundTripsThroughTheUpdateEndpoint(t *testing.T) {
	fixture := newProfileFixture(t, Security{}, nil)
	createRecordProfile(t, fixture, nil)

	recorder := doGet(t, fixture.srv, profilesRoute+"/ui_py", nil)
	require.Equal(t, http.StatusOK, recorder.Code)

	before := storedProfileRecord(t, fixture)
	back := withoutKeys(t, recorder.Body.String(), "env_keys")

	recorder = doJSON(t, fixture.srv, http.MethodPut, profilesRoute+"/ui_py", back, nil)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())

	after := storedProfileRecord(t, fixture)
	assert.Equal(t, before.Args, after.Args)
	assert.Equal(t, before.ArgsRender, after.ArgsRender)
	assert.Equal(t, before.Cwd, after.Cwd)
	assert.Equal(t, before.Env, after.Env, "整条记录的定义原样回来，一条都不掉")
}

func TestExecutorProfileRecord_Gates(t *testing.T) {
	sec := accountsSecurity(t)
	sec.Auth.Token = testToken
	fixture := newProfileFixture(t, sec, func(cfg *core.Config) {
		cfg.Executors.Commands = []core.ExecutorCommand{{
			Name: "from_config", Kind: string(executor.KindHTTP), Method: "GET",
			URLTemplate: "https://example.invalid/x", AllowedHosts: []string{"example.invalid"},
		}}
	})
	srv := fixture.srv
	createRecordProfile(t, fixture, opsToken(t, srv))

	for _, who := range []string{testViewerName, testOperatorName, testAdminName} {
		recorder := doGet(t, srv, profilesRoute+"/ui_py", bearer(login(t, srv, who, testPassword).AccessToken))
		assert.Equal(t, http.StatusForbidden, recorder.Code, who+" 读不到档位定义")
	}
	assert.Equal(t, http.StatusForbidden,
		doGet(t, srv, profilesRoute+"/ui_py", bearer(testToken)).Code, "machine 身份也不行")

	// 配置侧档位没有存储记录：与 PUT/DELETE 同一个 409 判据
	recorder := doGet(t, srv, profilesRoute+"/from_config", opsToken(t, srv))
	require.Equal(t, http.StatusConflict, recorder.Code)
	assert.Contains(t, decodeErrorResponse(t, recorder).Details, "executors.commands")

	recorder = doGet(t, srv, profilesRoute+"/no_such_profile", opsToken(t, srv))
	require.Equal(t, http.StatusNotFound, recorder.Code)

	// 关掉在线管理：连读口一起 503（这一组端点同生同灭）
	off := newProfileFixture(t, Security{}, func(cfg *core.Config) { cfg.Executors.WebEnabled = false })
	recorder = doGet(t, off.srv, profilesRoute+"/whatever", nil)
	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
}

// 读请求不进写操作台账（api/audit.go 的中间件只记 POST/PUT/DELETE）：
// 台账一页对应一次"改动"，把读口记进去会被前端轮询写满。
func TestExecutorProfileRecord_IsNotAudited(t *testing.T) {
	capture := &auditCapture{}
	sec := accountsSecurity(t)
	fixture := newProfileFixture(t, sec, nil, WithAuditLog(capture, capture))
	createRecordProfile(t, fixture, opsToken(t, fixture.srv))

	// 登录本身就是一行，先取凭据再清空：不然下面"只有一行"的断言其实在测登录
	header := opsToken(t, fixture.srv)
	capture.reset()
	require.Equal(t, http.StatusOK, doGet(t, fixture.srv, profilesRoute+"/ui_py", header).Code)
	assert.Empty(t, capture.entries, "读端点不该产生台账行")

	// 同一个名字写一次就有行，证明上面那格空不是替身没接上
	capture.reset()
	require.Equal(t, http.StatusOK,
		doJSON(t, fixture.srv, http.MethodPut, profilesRoute+"/ui_py",
			profileBody(t, recordProfile(t)), header).Code)
	require.Len(t, capture.entries, 1)
	assert.Equal(t, "executor.profile_update", capture.entries[0].Action)
}

func mustMarshal(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	require.NoError(t, err)
	return data
}

func mustDecode(t *testing.T, body string) map[string]any {
	t.Helper()
	var obj map[string]any
	require.NoError(t, json.Unmarshal([]byte(body), &obj), body)
	return obj
}
