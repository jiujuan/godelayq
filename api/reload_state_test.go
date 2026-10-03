package api

// /admin/runtime 的 reload 对象（TASK-R06 §5.4、§5.5）。
//
// 三条读数判据加一条反证：
//   - 没注入读口时**整个键都不出现**（不是 "reload":{}）；
//   - 注入之后十个字段齐、result 是封闭取值之一、两个时间是能解析的 RFC3339；
//   - 状态里没有配置取值（canary 反证 + 键集合封闭：多一个键就要显式改这条用例）；
//   - 读端点不写台账（本卡没新增写操作，auditActions 因此不需要加行）。

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"godelayq/core"
)

// stubReloadReader 是 ReloadStateReader 的替身：交回用例给的那一份状态，并记被读了几次。
type stubReloadReader struct {
	state core.ReloadState
	calls int
}

func (s *stubReloadReader) State() core.ReloadState {
	s.calls++
	return s.state
}

// runtimeBody 请求一次 /admin/runtime 并把响应解成 map，返回状态码与键集合。
//
// 判"没有 reload 这个键"必须走 map 而不是解码进 RuntimeResponse：解进结构体之后，
// "键不存在"与"键是空对象"都是 Reload == nil，看不出差别（decodeRuntime 就是这种读法）。
func runtimeBody(t *testing.T, srv *Server, header http.Header) (map[string]any, string) {
	t.Helper()

	recorder := doGet(t, srv, "/api/v1/admin/runtime", header)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())

	var raw map[string]any
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &raw), recorder.Body.String())
	return raw, recorder.Body.String()
}

// TestGetRuntime_HasNoReloadFieldWhenNotInjected 判"未启用热重载的部署"的读数形状：
// reload 这个键根本不在响应里，而其余字段一条不少。
//
// 值类型会给出 {"reload":{"result":""}} —— 那会被读成"启用过但从没重载"，
// 而这里想说的是这个开关压根没打开（设计文档 §5.4 选指针的理由）。
func TestGetRuntime_HasNoReloadFieldWhenNotInjected(t *testing.T) {
	srv := newSecurityServer(t, Security{})

	raw, body := runtimeBody(t, srv, nil)

	_, hasReload := raw["reload"]
	assert.False(t, hasReload, `响应里出现了 reload 键：%s`, body)
	assert.NotContains(t, body, `"reload"`, "整份原文里都不该出现这个键名")
	// 其余键照旧在位：证明上面那个"没有"不是整份响应空了。
	for _, key := range []string{"uptime", "started_at", "scheduler", "event_history"} {
		assert.Contains(t, raw, key)
	}

	// 注入一个 nil 读口也不该给出对象：WithReloadState(nil, true) 是装配错误，
	// 但它的后果不该是"运维看到一个假的重载状态"。
	nilSrv := newSecurityServer(t, Security{}, WithReloadState(nil, true))
	_, nilBody := runtimeBody(t, nilSrv, nil)
	assert.NotContains(t, nilBody, `"reload"`)
}

// reloadStatusKeys 是 reload 对象的十个键名，两份断言（"该在的都在"与"不许冒出别的键"）
// 读同一份清单。原来两份各写一遍字面量，结果 error/watcher_error 既没断出现也没断不出现。
var reloadStatusKeys = []string{
	"enabled", "watched_path", "last_attempt_at", "last_applied_at", "result", "error",
	"applied_keys", "ignored_keys", "rejected_keys", "watcher_error",
}

// TestGetRuntime_ExposesReloadState 判注入之后的完整读数。
//
// 卡 §5.4 第 2 条写的是"七个字段齐"，那是卡面上的旧数：core.ReloadState 一共十个字段
// （设计文档 §5.4 列全了）。这一份十个字段全部给非空取值，于是十个键必须全部出现，
// 而 `error` / `watcher_error` 那两条自由文本要有**正向判据**——否则把
// `Error: state.Error` 那一行删掉，全包的用例照样绿，而运维看不到失败原因（I3）。
func TestGetRuntime_ExposesReloadState(t *testing.T) {
	attempt := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	applied := attempt.Add(90 * time.Millisecond)
	reader := &stubReloadReader{state: core.ReloadState{
		Enabled:       false, // 故意给假值：响应里的 enabled 必须来自注入参数而不是 State()
		WatchedPath:   `C:\deploy\configs\config.yaml`,
		LastAttemptAt: attempt,
		LastAppliedAt: applied,
		Result:        core.ReloadDegraded,
		Error:         "第 6 步 scheduler.workers 应用失败：超过运行期上界；回滚也有失败项：observability.events",
		AppliedKeys:   []string{"logging.level", "scheduler.workers"},
		IgnoredKeys:   []string{"server.port"},
		RejectedKeys:  []string{"server.auth.token"},
		WatcherError:  "watch directory inotify_add_watch failed: permission denied",
	}}

	srv := newSecurityServer(t, Security{}, WithReloadState(reader, true))
	raw, body := runtimeBody(t, srv, nil)

	require.Contains(t, raw, "reload", "注入过读口却没有这个键：%s", body)
	state, ok := raw["reload"].(map[string]any)
	require.True(t, ok, "reload 不是对象：%T", raw["reload"])

	for _, key := range reloadStatusKeys {
		assert.Contains(t, state, key, "十个字段全给了非空取值，却少这个键：%s", body)
	}
	// 键集合封闭：除了这十个已知键之外不许有任何别的键（多一个键就得显式改这条用例）。
	for key := range state {
		assert.Contains(t, reloadStatusKeys, key, "reload 对象里冒出了一个未知键")
	}

	enabled, isBool := state["enabled"].(bool)
	require.True(t, isBool, "enabled 应是布尔读数，实际 %T", state["enabled"])
	assert.True(t, enabled, "enabled 必须来自 WithReloadState 的那个进程配置值（D-R0503）")
	// 卡 §5.4 要的"result 是封闭取值之一"在 api 侧只能这样判：这里原样透出，
	// 取值集合的封闭由 core 那一侧保证（core.ReloadResult 的五个常量 + 空串）。
	assert.Contains(t, []string{"", "ok", "unchanged", "rejected", "failed", "degraded"},
		state["result"], "result 不是封闭取值之一")
	assert.Equal(t, string(core.ReloadDegraded), state["result"])
	assert.Equal(t, `C:\deploy\configs\config.yaml`, state["watched_path"])
	// 两条自由文本的正向判据：产出方给什么，读数里就是什么，不许在这一层被丢掉。
	assert.Equal(t, reader.state.Error, state["error"])
	assert.Equal(t, reader.state.WatcherError, state["watcher_error"])

	assert.ElementsMatch(t, []any{"logging.level", "scheduler.workers"}, state["applied_keys"])
	assert.ElementsMatch(t, []any{"server.port"}, state["ignored_keys"])
	assert.ElementsMatch(t, []any{"server.auth.token"}, state["rejected_keys"])

	// 两个时间是 RFC3339Nano 字符串（不是 core.ReloadState 直接序列化出来的对象）：
	// 这正是 D-R0502 的落点，解析不回来就说明有人改回了直接 marshal。
	// 断的是**逐字相等**而不只是"能解析"：两者都非零、都能解析的话，
	// 把两个字段填反或都填同一个源头，只有取值相等判得出来。
	assert.Equal(t, attempt.Format(time.RFC3339Nano), state["last_attempt_at"])
	assert.Equal(t, applied.Format(time.RFC3339Nano), state["last_applied_at"])
	for _, key := range []string{"last_attempt_at", "last_applied_at"} {
		value, isString := state[key].(string)
		require.True(t, isString, "%s 不是字符串：%T", key, state[key])
		parsed, err := time.Parse(time.RFC3339Nano, value)
		require.NoError(t, err, "%s 解析不回来：%q", key, value)
		assert.False(t, parsed.IsZero())
	}
	assert.Equal(t, 1, reader.calls, "一次端点请求应当只读一遍状态")
}

// TestGetRuntime_ReloadStateWithoutAnyAttempt 判"起来就一次都没重载过"的读数形状（D-R0502 的
// 第二条）：两个零值时间整个键都不给，判"没试过"只能看 result 是空串。
// 顺带钉住真实进程里最常见的那一格：开关开着、监听器没建起来（没有可盯的文件），
// 此时 watched_path 与 watcher_error 要一起给，读数才说得出"为什么没反应"（I3）。
func TestGetRuntime_ReloadStateWithoutAnyAttempt(t *testing.T) {
	const why = "reload.enabled=true，但进程启动时没有读到任何配置文件，监听器没有建立"
	reader := &stubReloadReader{state: core.ReloadState{
		WatchedPath:  "/etc/godelayq/config.yaml",
		WatcherError: why,
	}}
	srv := newSecurityServer(t, Security{}, WithReloadState(reader, true))

	raw, body := runtimeBody(t, srv, nil)
	state, ok := raw["reload"].(map[string]any)
	require.True(t, ok, "注入过读口却没有 reload 对象：%s", body)

	assert.NotContains(t, state, "last_attempt_at", `零值时间不该出现（0001-01-01 是假时间）：%s`, body)
	assert.NotContains(t, state, "last_applied_at")
	assert.NotContains(t, state, "error", "没尝试过就不该有那次失败的文本")
	assert.Equal(t, "", state["result"], "result 空串才是从没尝试过的判据")
	assert.Equal(t, true, state["enabled"])
	assert.Equal(t, "/etc/godelayq/config.yaml", state["watched_path"])
	assert.Equal(t, why, state["watcher_error"], "监听器自己的故障要能从读数里读到")
}

// TestGetRuntime_LeaksNoConfigValues 是"不外泄取值"的防线（卡 §5.4 第 3 条）。
//
// 这一层能证的与不能证的要写清楚，否则这条用例会被读成一道它守不住的闸：
//   - 证得到的：api 这一层**不会主动把服务自己的配置值搬进读数**。canary 放在 Security 里
//     （静态 token、JWT 密钥、账号密码哈希三份凭据），响应正文里一处都不许出现；
//     将来谁把 `s.sec` 或存储内容拼进那个对象，这里红。
//   - 证不到的：读口**交回什么，这一层就原样透出什么**。所以卡面那句
//     "把 canary 值塞进读口的返回值，再 strings.Contains 反证"在这条路上是反的——
//     塞进去必然出现在响应里，那是对的而不是漏。取值不进状态的闸门在产出方
//     （core.ChangedKey 只带 Path，见 R01），不在这层；本卡把它记成 D-R0606。
//   - 路径可以出现（它不是凭据，运维要凭它定位文件）；三份键清单只许是键路径。
//
// 进这个端点要 ops 档，静态 token 是 machine 档（security.go 的 verifyMachineToken），
// 所以请求走"登录拿 JWT"这条路；请求头里带 canary 不算泄漏，判的是响应正文。
func TestGetRuntime_LeaksNoConfigValues(t *testing.T) {
	const (
		tokenCanary  = "canary-token-never-in-responses"
		secretCanary = "canary-jwt-secret-never-in-responses"
		configPath   = `C:\srv\godelayq\configs\config.yaml`
	)

	sec := accountsSecurity(t)
	sec.Auth.Token = tokenCanary
	sec.Auth.JWT.Secret = secretCanary

	srv := newSecurityServer(t, sec, WithReloadState(&stubReloadReader{state: core.ReloadState{
		WatchedPath:  configPath,
		Result:       core.ReloadRejected,
		Error:        "改动触碰了拒绝档（凭据与执行许可字段），整次作废、一项都没有应用：server.auth.token",
		RejectedKeys: []string{"server.auth.token", "executors.commands.echo.env"},
		AppliedKeys:  []string{"logging.level"},
	}}, true))

	bcryptCanary := cachedTestHash(t)
	session := login(t, srv, testOpsName, testPassword)
	raw, body := runtimeBody(t, srv, bearer(session.AccessToken))

	state, ok := raw["reload"].(map[string]any)
	require.True(t, ok, "注入过读口却没有 reload 对象：%s", body)
	// 前提：读口给的东西确实进到了响应里（否则下面的负向断言全是空转）。
	assert.Equal(t, configPath, state["watched_path"], "前提：路径确实进来了")
	assert.Equal(t, "改动触碰了拒绝档（凭据与执行许可字段），整次作废、一项都没有应用：server.auth.token",
		state["error"], "前提：文本通道也进来了")
	for _, canary := range []string{tokenCanary, secretCanary, bcryptCanary} {
		assert.NotContains(t, body, canary, "响应里带出了配置取值")
	}
	// 三份清单只许是键路径：出现等号、冒号或引号，就说明有人把"键=值"整段搬了进来。
	for _, key := range []string{"applied_keys", "rejected_keys", "ignored_keys"} {
		list, has := state[key]
		if !has {
			continue
		}
		items, isList := list.([]any)
		require.True(t, isList, "%s 不是数组：%T", key, list)
		for _, item := range items {
			name, isString := item.(string)
			require.True(t, isString, "%s 里有非字符串项：%T", key, item)
			assert.NotContains(t, name, "=", "%s 里的 %q 看着像键值对而不是键名", key, name)
			assert.False(t, strings.ContainsAny(name, `:"`), "%s 里的 %q 不像一条键路径", key, name)
		}
	}
}

// TestRuntimeReadDoesNotWriteAudit 是卡 §5.5 要的那条反向用例：
// 本卡没有新增任何写操作，/admin/runtime 多出来的 reload 对象也不写台账。
// 判据取自台账替身收到的行（写面），而不是查询面——查询面读的是替身自己的返回值。
//
// 读口必须**注入**：不注入的话 `GetRuntime` 里那段 `if s.reloadState != nil` 根本不走，
// "读一次不写台账"就成了在测一条与本次改动无关的路径（复核第一轮抓到的正是这里）。
func TestRuntimeReadDoesNotWriteAudit(t *testing.T) {
	capture := &auditCapture{}
	srv := auditServer(t, capture, WithReloadState(&stubReloadReader{state: core.ReloadState{
		WatchedPath: "/etc/godelayq/config.yaml",
		Result:      core.ReloadFailed,
		Error:       "第 6 步 scheduler.workers 应用失败：超过运行期上界",
	}}, true))
	header := opsHeader(t, srv, capture)

	for i := 0; i < 3; i++ {
		recorder := doGet(t, srv, "/api/v1/admin/runtime", header)
		require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
		// 前提：这一次真的走到了填 reload 对象的那条分支上。
		var raw map[string]any
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &raw))
		require.Contains(t, raw, "reload", "第 %d 次读没有 reload 对象，后面的台账判据是空的", i)
	}
	assert.Empty(t, capture.entries, "读 /admin/runtime 不该产生台账行：%+v", capture.entries)

	// 正向对照：同一台服务器做一次写必须留下一行，否则上面那个"空"只是"中间件没挂上"。
	recorder := doJSON(t, srv, http.MethodPost, "/api/v1/admin/scheduler/suspend", "", header)
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Len(t, capture.entries, 1)
	assert.Equal(t, "admin.scheduler_suspend", capture.entries[0].Action)

	// 动作词表里没有 admin.runtime：这条断言把"本卡不需要加 auditActions 行"钉住。
	for _, action := range AuditActions() {
		assert.NotEqual(t, "admin.runtime", action, "台账动作词表里出现了 admin.runtime，与本卡的口径冲突")
	}
}
