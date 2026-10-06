package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"godelayq/core"
	"godelayq/executor"
)

// 这一组是 TASK-N06 的用例：接口说清"哪几条类型是自由执行档位、它的位置输入框长什么样、
// 这份部署放开到哪一步"，并把提交门禁、结果门槛与台账那三处在内置档位上各验一遍。
//
// 用例一律走真实登记表（NewRegistry 里那条内置档位构造链）：要验的正是
// "接口把内置档位说成什么样"，换成替身就变成在测自己的抄写。

// adhocMissingRuntime 是一个故意不存在的解释器名：用它构造的内置档位是
// "键存在但这台机器跑不了"那一种状态，与"类型未注册"是两条不同的文案。
const adhocMissingRuntime = "godelayq-api-test-runtime-xyz"

// adhocBuiltInKeys 是四条内置档位的注册键，按字典序（接口与启动日志都按这个顺序给）。
var adhocBuiltInKeys = []string{"exec.http", "exec.php", "exec.python", "exec.shell"}

// adhocEnv 是一台打开了 executors.adhoc 的服务器加它的四种身份凭据。
type adhocEnv struct {
	srv       *Server
	creds     func(string) http.Header
	artifacts *executor.ArtifactStore
	logs      *bytes.Buffer
	workspace string
}

// newAdhocEnv 建现场：四条内置档位是仅有的档位（配置侧 commands 由 tune 追加），
// 同名处理函数一并注册——提交路径先查注册表再查档位，两者都得在。
func newAdhocEnv(t *testing.T, requiredRole string, tune ...func(*core.Config)) *adhocEnv {
	t.Helper()

	workspace := t.TempDir()
	cfg := core.DefaultConfig()
	cfg.Executors.Enabled = true
	cfg.Executors.Workspace = workspace
	cfg.Executors.RequiredRole = requiredRole
	cfg.Executors.Adhoc.Enabled = true
	for _, adjust := range tune {
		if adjust != nil {
			adjust(&cfg)
		}
	}

	logs := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	registry, err := executor.NewRegistry(cfg, logger)
	require.NoError(t, err)

	store, err := core.NewJSONFileStore(filepath.Join(t.TempDir(), "jobs.json"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	artifacts := newArtifactStoreFor(t, 1<<20)
	sec := accountsSecurity(t)
	sec.Auth.Token = testToken

	srv := NewServer(core.NewScheduler(store, nil, nil), store, "0", sec, logger,
		WithExecutorRegistry(registry), WithArtifacts(artifacts))
	registerNopHandler(srv)

	nop := func(context.Context, *core.Job) error { return nil }
	for _, key := range adhocBuiltInKeys {
		srv.RegisterJobHandler(key, nop)
	}
	for _, command := range cfg.Executors.Commands {
		srv.RegisterJobHandler(executor.HandlerKeyPrefix+command.Name, nop)
	}

	return &adhocEnv{srv: srv, creds: identities(t, srv), artifacts: artifacts,
		logs: logs, workspace: workspace}
}

// rows 读一次 GET /api/v1/executors，返回顶层对象、按注册键索引的行与原始正文。
func (e *adhocEnv) rows(t *testing.T, who string) (ListExecutorsResponse, map[string]ExecutorProfileResponse, string) {
	t.Helper()

	recorder := doGet(t, e.srv, executorsRoute, e.creds(who))
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())

	var resp ListExecutorsResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &resp), recorder.Body.String())

	rows := make(map[string]ExecutorProfileResponse, len(resp.Profiles))
	for _, row := range resp.Profiles {
		require.False(t, row.Degraded, "本文件的用例里没有降级条目")
		rows[row.Key] = row
	}
	return resp, rows, recorder.Body.String()
}

// TestAdhocMetadata_BuiltInRowsCarryLocation 是 §3.1 与 DoD 第 1 条：
// 四条内置条目各带 adhoc 与 location，普通条目不带——前端只靠 location 的有无分流表单。
func TestAdhocMetadata_BuiltInRowsCarryLocation(t *testing.T) {
	env := newAdhocEnv(t, "operator", func(cfg *core.Config) {
		cfg.Executors.Commands = []core.ExecutorCommand{
			httpCommand("cfg_health", "https://api.example.com/health"),
		}
	})
	_, rows, body := env.rows(t, "admin")

	cases := []struct {
		key         string
		locationKey string
		kind        string
		labelPart   string
	}{
		{"exec.php", "script", "path", ".php"},
		{"exec.python", "script", "path", ".py"},
		{"exec.shell", "script", "path", ".sh"},
		{"exec.http", "url", "url", "请求 URL"},
	}

	for _, tc := range cases {
		row, ok := rows[tc.key]
		require.True(t, ok, "内置档位 %s 没出现在列表里", tc.key)
		assert.True(t, row.Adhoc, "%s 是自由执行档位", tc.key)
		require.NotNil(t, row.Location, "%s 必须带 location，否则前端不知道该给哪个输入框", tc.key)

		assert.Equal(t, tc.locationKey, row.Location.Key)
		assert.Equal(t, tc.kind, row.Location.Kind)
		assert.True(t, row.Location.Required, "没有位置就不知道跑什么：这条档位没有默认值可退")
		assert.Contains(t, row.Location.Label, tc.labelPart)
		assert.NotEmpty(t, row.Location.Hint, "范围说明由后端给整句，前端不再拼句子")

		// 内置档位没有参数、没有脚本路径、没有地址模板：这三样在响应里的形态要说清，
		// 否则前端会把"空 args"读成"这份后端还不认识参数"。
		assert.Empty(t, row.Args)
		assert.Empty(t, row.URL, "整条地址来自任务，档位自己没有模板")
		assert.Empty(t, row.PathDisplay, "档位自己不指向任何文件")
		assert.Equal(t, string(executor.SourceAdhoc), row.Source)
	}

	ordinary := rows["exec.cfg_health"]
	require.Nil(t, ordinary.Location, "普通档位不该带 location")
	assert.False(t, ordinary.Adhoc)

	// 键存在性只能看原始正文：结构体里"没有这个键"与"值是零值"解出来是同一个东西。
	raw := rawProfileRows(t, body)
	ordinaryRow := raw[rowKey("exec.cfg_health", false)]
	_, hasLocation := ordinaryRow["location"]
	assert.False(t, hasLocation, "普通条目的响应里不能出现 location 键")
	assert.Contains(t, ordinaryRow, "adhoc",
		"adhoc 一律出现：省略这个键会让前端分不出普通档位与尚未认识自由执行档位的后端")
	assert.Equal(t, false, ordinaryRow["adhoc"])

	for _, key := range adhocBuiltInKeys {
		builtIn := raw[rowKey(key, false)]
		assert.Equal(t, true, builtIn["adhoc"])
		require.Contains(t, builtIn, "location")

		location, ok := builtIn["location"].(map[string]any)
		require.True(t, ok, "location 必须是对象：%+v", builtIn["location"])
		for _, field := range []string{"key", "kind", "label", "required", "hint"} {
			assert.Contains(t, location, field, "输入说明缺字段 %s 就无法呈现（键 %s）", field, key)
		}
	}
}

// TestAdhocMetadata_HintFollowsTheEffectiveConfig 是 §3.2 的四条规则与 DoD 第 2 条：
// Hint 读的是生效配置，改一份配置就换一句话。
func TestAdhocMetadata_HintFollowsTheEffectiveConfig(t *testing.T) {
	t.Run("脚本档位限定目录", func(t *testing.T) {
		env := newAdhocEnv(t, "operator", func(cfg *core.Config) {
			cfg.Executors.Adhoc.PathPrefixes = []string{"scripts", "jobs"}
		})
		_, rows, _ := env.rows(t, "admin")

		hint := rows["exec.php"].Location.Hint
		assert.Contains(t, hint, "只能选这些目录里的文件")
		assert.Contains(t, hint, filepath.Join(env.workspace, "scripts"),
			"限定目录按 workspace 展开成绝对写法，与判据用的是同一份")
		assert.Contains(t, hint, "扩展名限 .php")
		assert.NotContains(t, hint, "任意位置")
	})

	t.Run("脚本档位不限目录", func(t *testing.T) {
		env := newAdhocEnv(t, "operator")
		_, rows, _ := env.rows(t, "admin")

		hint := rows["exec.shell"].Location.Hint
		assert.Contains(t, hint, "任意位置的脚本文件")
		assert.Contains(t, hint, "没有做目录范围限制",
			"这一句是给运维看的：不限目录不是漏填就是一种放开")
		assert.Contains(t, rows["exec.shell"].Location.Label, ".sh")
		assert.Contains(t, rows["exec.shell"].Location.Label, ".bash")
	})

	t.Run("脚本档位关掉扩展名要求", func(t *testing.T) {
		off := false
		env := newAdhocEnv(t, "operator", func(cfg *core.Config) {
			cfg.Executors.Adhoc.RequireExtension = &off
		})
		_, rows, _ := env.rows(t, "admin")

		row := rows["exec.php"]
		assert.NotContains(t, row.Location.Label, ".php", "不要求扩展名时标题不再列")
		assert.Contains(t, row.Location.Hint, "也不要求扩展名")
	})

	t.Run("http 档位限定主机", func(t *testing.T) {
		env := newAdhocEnv(t, "operator", func(cfg *core.Config) {
			cfg.Executors.Adhoc.URLHosts = []string{"hook.example.com", "*.inner.example.com"}
		})
		_, rows, _ := env.rows(t, "admin")

		hint := rows["exec.http"].Location.Hint
		assert.Contains(t, hint, "只能请求这些主机")
		assert.Contains(t, hint, "*.inner.example.com", "通配写法原样给出，与判据同一种形态")
		assert.Contains(t, hint, "回环、私网与链路本地地址")
	})

	t.Run("http 档位不限主机但守卫仍开", func(t *testing.T) {
		env := newAdhocEnv(t, "operator")
		_, rows, _ := env.rows(t, "admin")

		hint := rows["exec.http"].Location.Hint
		assert.Contains(t, hint, "可以请求任意主机")
		assert.Contains(t, hint, "仍会被拒绝")
	})

	t.Run("放开回环与私网", func(t *testing.T) {
		env := newAdhocEnv(t, "operator", func(cfg *core.Config) {
			cfg.Executors.Adhoc.URLAllowPrivate = true
		})
		_, rows, _ := env.rows(t, "admin")

		hint := rows["exec.http"].Location.Hint
		assert.Contains(t, hint, "executors.adhoc.url_allow_private 关闭")
		assert.NotContains(t, hint, "仍会被拒绝")
	})
}

// TestAdhocMetadata_EnabledFalseKeepsShape 是 §3.3 的兼容半边：整节关闭时
// 这四条键根本不存在，普通条目的 adhoc 是 false 而不是缺键。
func TestAdhocMetadata_EnabledFalseKeepsShape(t *testing.T) {
	workspace := t.TempDir()
	cfg := core.DefaultConfig()
	cfg.Executors.Enabled = true
	cfg.Executors.Workspace = workspace
	cfg.Executors.Commands = []core.ExecutorCommand{
		httpCommand("cfg_health", "https://api.example.com/health"),
	}

	registry, err := executor.NewRegistry(cfg, newTestLogger())
	require.NoError(t, err)

	srv := newSecurityServer(t, Security{}, WithExecutorRegistry(registry))
	resp, rows, body := func() (ListExecutorsResponse, map[string]ExecutorProfileResponse, string) {
		recorder := doGet(t, srv, executorsRoute, nil)
		require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())

		var parsed ListExecutorsResponse
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &parsed), recorder.Body.String())
		rows := map[string]ExecutorProfileResponse{}
		for _, row := range parsed.Profiles {
			rows[row.Key] = row
		}
		return parsed, rows, recorder.Body.String()
	}()

	require.True(t, resp.Enabled)
	for _, key := range adhocBuiltInKeys {
		assert.NotContains(t, rows, key, "整节关闭时这四条键不该出现在列表里")
	}

	ordinary := rows["exec.cfg_health"]
	assert.False(t, ordinary.Adhoc)
	assert.Nil(t, ordinary.Location)

	raw := rawProfileRows(t, body)
	_, hasLocation := raw[rowKey("exec.cfg_health", false)]["location"]
	assert.False(t, hasLocation)
}

// TestAdhocGate_SubmissionEvidence 是 §3.5 的门禁半边：
// 身份、可用性、payload 三道判定在内置档位上各命中一次，且被判掉的任务没有进堆。
func TestAdhocGate_SubmissionEvidence(t *testing.T) {
	env := newAdhocEnv(t, "admin")

	t.Run("身份不够时 403，且不因名称是中文标签而绕过", func(t *testing.T) {
		recorder := doJSON(t, env.srv, http.MethodPost, "/api/v1/jobs",
			`{"name":"昨晚的回调任务","type":"exec.http","payload":{"url":"https://api.example.com/x"}}`,
			env.creds("operator"))
		require.Equal(t, http.StatusForbidden, recorder.Code, recorder.Body.String())
		assert.Contains(t, recorder.Body.String(), "exec.http",
			"拒绝要说清是哪个类型需要更高档位，而不是被标签名糊住")
	})

	t.Run("这台机器跑不了时说的是可用性", func(t *testing.T) {
		missing := newAdhocEnv(t, "operator", func(cfg *core.Config) {
			cfg.Executors.RuntimeAllow = append(cfg.Executors.RuntimeAllow, adhocMissingRuntime)
			cfg.Executors.Adhoc.ShellRuntime = adhocMissingRuntime
		})
		recorder := doJSON(t, missing.srv, http.MethodPost, "/api/v1/jobs",
			`{"name":"跑不了的任务","type":"exec.shell","payload":{"script":"scripts/x.sh"}}`,
			missing.creds("admin"))
		require.Equal(t, http.StatusBadRequest, recorder.Code, recorder.Body.String())
		assert.Contains(t, recorder.Body.String(), "not available on this server",
			"键存在但解释器不在 PATH，不能说成类型未注册")
		assert.NotContains(t, recorder.Body.String(), "unknown job type")
	})

	t.Run("位置非法时 400 且没有入队", func(t *testing.T) {
		for _, payload := range []string{
			`{}`,
			`{"url":"javascript:alert(1)"}`,
			`{"url":"http://user:pass@api.example.com/x"}`,
			`{"script":"scripts/x.sh"}`,
		} {
			recorder := doJSON(t, env.srv, http.MethodPost, "/api/v1/jobs",
				`{"name":"非法位置的任务","type":"exec.http","payload":`+payload+`}`,
				env.creds("admin"))
			require.Equal(t, http.StatusBadRequest, recorder.Code, "%s -> %s", payload, recorder.Body.String())

			var failure ErrorResponse
			require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &failure))
			assert.Equal(t, "invalid executor payload", failure.Message)
		}

		listed := doGet(t, env.srv, "/api/v1/jobs?limit=50", env.creds("admin"))
		assert.NotContains(t, listed.Body.String(), "非法位置的任务", "被判掉的不得入队")
	})

	t.Run("合法位置照常建成", func(t *testing.T) {
		recorder := doJSON(t, env.srv, http.MethodPost, "/api/v1/jobs",
			`{"name":"回调健康检查","type":"exec.http","payload":{"url":"https://api.example.com/health"}}`,
			env.creds("admin"))
		require.Equal(t, http.StatusCreated, recorder.Code, recorder.Body.String())

		resp := decodeJob(t, recorder)
		assert.Equal(t, "exec.http", resp.Type)
		assert.Equal(t, "回调健康检查", resp.Name)
	})
}

// TestAdhocResult_ReaderLevelIsEnough 是 §3.5 的结果读取那条：
// 内置档位不声明 secret 参数，所以读取门槛不升档，viewer 就读得到正文。
func TestAdhocResult_ReaderLevelIsEnough(t *testing.T) {
	env := newAdhocEnv(t, "operator")

	writeArtifact(t, env.artifacts, "job-adhoc-out", 1, "out", []byte("ran the script named by the job"))
	require.NoError(t, env.srv.store.Update(core.JobSnapshot{
		ID: "job-adhoc-out", Name: "昨晚的脚本任务", Type: "exec.php",
		Payload:  []byte(`{"script":"C:\\work\\nightly.php"}`),
		Attempts: 1, Status: int(core.StatusSuccess),
		Exec:      &core.ExecMeta{Kind: "script", Profile: "php", ExitCode: 0},
		TriggerAt: time.Now(), CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}))

	recorder := doGet(t, env.srv, "/api/v1/jobs/job-adhoc-out/result", env.creds("viewer"))
	result := decodeJobResult(t, recorder)
	assert.Contains(t, result.Content, "ran the script named by the job")
	assert.Empty(t, result.RedactionNote, "没有 secret 声明就不该有升档说明")

	detail := decodeJob(t, doGet(t, env.srv, "/api/v1/jobs/job-adhoc-out", env.creds("viewer")))
	assert.Equal(t, "exec.php", detail.Type, "详情读的类型仍是那条注册键")
	assert.NotContains(t, detail.Payload, "<redacted>", "位置不是凭据：payload 原样给出")
}

// TestAdhocAudit_RowsUseTheExistingActionWord 是 §3.5 的台账那条：
// 内置档位落进既有的 job.create 一行，执行器两列记的是注册键与档位名。
func TestAdhocAudit_RowsUseTheExistingActionWord(t *testing.T) {
	capture := &auditCapture{}
	cfg := core.DefaultConfig()
	cfg.Executors.Enabled = true
	cfg.Executors.Workspace = t.TempDir()
	cfg.Executors.RequiredRole = "admin"
	cfg.Executors.Adhoc.Enabled = true

	registry, err := executor.NewRegistry(cfg, newTestLogger())
	require.NoError(t, err)

	store, err := core.NewJSONFileStore(filepath.Join(t.TempDir(), "jobs.json"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	sec := accountsSecurity(t)
	sec.Auth.Token = testToken
	srv := NewServer(core.NewScheduler(store, nil, nil), store, "0", sec, newTestLogger(),
		WithExecutorRegistry(registry), WithAuditLog(capture, capture))
	nop := func(context.Context, *core.Job) error { return nil }
	srv.RegisterJobHandler("exec.http", nop)

	accepted := doJSON(t, srv, http.MethodPost, "/api/v1/jobs",
		`{"name":"回调健康检查","type":"exec.http","payload":{"url":"https://api.example.com/health"}}`,
		identityHeader(t, srv, "admin"))
	require.Equal(t, http.StatusCreated, accepted.Code, accepted.Body.String())

	entry := capture.only(t)
	assert.Equal(t, "job.create", entry.Action, "不新增动作词")
	assert.Equal(t, "exec.http", entry.HandlerKey, "台账里那一列记的是注册键")
	assert.Equal(t, executor.AdhocHTTPName, entry.Profile)
	assert.Equal(t, auditExecAccepted, entry.ExecVerdict)

	capture.reset()
	rejected := doJSON(t, srv, http.MethodPost, "/api/v1/jobs",
		`{"name":"缺位置的任务","type":"exec.http","payload":{}}`,
		identityHeader(t, srv, "admin"))
	require.Equal(t, http.StatusBadRequest, rejected.Code, rejected.Body.String())

	denied := capture.only(t)
	assert.Equal(t, "job.create", denied.Action, "被拒的同一条动作词")
	assert.Equal(t, auditExecPayloadRejected, denied.ExecVerdict)
	assert.Equal(t, "exec.http", denied.HandlerKey)

	encoded, err := json.Marshal(denied)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "https://api.example.com/health",
		"台账不写 payload：那一列的原文只在响应与任务里")
}
