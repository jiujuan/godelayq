package api

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"godelayq/core"
	"godelayq/executor"
)

// GET /api/v1/executors 的来源、可编辑性与降级（TASK-W07）。
//
// 本文件的用例一律走真实登记表：两条来源与那条降级条目由生产路径合出来
// （NewRegistry + MergeStoreProfiles + ApplyStore），替身进不去——
// 要验的正是"登记表怎么看这条档位"，换成替身就变成在测自己的抄写。

const executorsRoute = "/api/v1/executors"

// httpCommand 造一条探测结论恒为可用的 http 档位（探测不访问网络）。
func httpCommand(name, url string) core.ExecutorCommand {
	return core.ExecutorCommand{
		Name: name, Kind: string(executor.KindHTTP), Method: "GET",
		URLTemplate: url, AllowedHosts: []string{"api.example.com"},
		Timeout: 30 * time.Second,
	}
}

// rowKey 是"同一个注册键可能两行"的索引写法：生效那条与降级那条键位相同（撞名的本义），
// 用例里靠 degraded 标记分开取。
func rowKey(handlerKey string, degraded bool) string {
	return handlerKey + "#degraded=" + boolMarker(degraded)
}

func boolMarker(degraded bool) string {
	if degraded {
		return "true"
	}
	return "false"
}

// twoSourceFixture 是一份"配置两条 + 文件三条"的登记表，其中文件那条与 cfg_health 撞名。
//
// root 是这份夹具的临时目录根：两份登记表要共享同一个 root 才能逐字段比较（越界那条给的是
// 绝对路径，各建一个 root 会连路径一起比出差别，那就不是在比在线管理的开关了）。
//
// 撞名走的是 MergeStoreProfiles 的降级路径（待拍板 P1 取的答案：config 赢、文件那条标未生效），
// 所以它是本卡最要紧的一种状态：接口必须说得出"这条为什么没生效"。
type twoSourceFixture struct {
	registry  *executor.Registry
	workspace string
	outside   string
}

func newTwoSourceFixture(t *testing.T, webEnabled bool, root string) *twoSourceFixture {
	t.Helper()

	workspace := filepath.Join(root, "w07-workspace")
	outside := filepath.Join(root, "elsewhere", "abs_report.mjs")

	cfg := core.DefaultConfig()
	cfg.Executors.Enabled = true
	cfg.Executors.WebEnabled = webEnabled
	cfg.Executors.Workspace = workspace
	cfg.Executors.Commands = []core.ExecutorCommand{
		httpCommand("cfg_health", "https://api.example.com/health"),
		httpCommand("cfg_only", "https://api.example.com/only"),
	}

	registry, err := executor.NewRegistry(cfg, newTestLogger())
	require.NoError(t, err)

	stored := []core.ExecutorProfileRecord{
		core.NewExecutorProfileRecord(httpCommand("store_health", "https://api.example.com/stored")),
		// 与配置侧同名：这一条该被降级，看得见但没注册
		core.NewExecutorProfileRecord(httpCommand("cfg_health", "https://api.example.com/shadowed")),
		// 指向 workspace 之外的绝对路径：D5 允许的写法，path_display 要原样给出来
		core.NewExecutorProfileRecord(core.ExecutorCommand{
			Name: "store_abs", Kind: string(executor.KindScript),
			Runtime: "node", Script: outside, Timeout: time.Minute,
		}),
	}
	entries, warnings := executor.MergeStoreProfiles(cfg, stored)
	require.NoError(t, registry.ApplyStore(entries))
	require.Empty(t, warnings, "合并本身不该有跳过项，降级由登记表那一侧标出")

	return &twoSourceFixture{registry: registry, workspace: workspace, outside: outside}
}

func (f *twoSourceFixture) server(t *testing.T) *Server {
	t.Helper()
	return newSecurityServer(t, Security{}, WithExecutorRegistry(f.registry))
}

// listExecutors 读一次端点，返回顶层对象、按 rowKey 索引的行，以及原始响应正文。
func listExecutors(t *testing.T, srv *Server) (ListExecutorsResponse, map[string]ExecutorProfileResponse, string) {
	t.Helper()

	recorder := doGet(t, srv, executorsRoute, nil)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())

	var resp ListExecutorsResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &resp), recorder.Body.String())

	rows := make(map[string]ExecutorProfileResponse, len(resp.Profiles))
	for _, row := range resp.Profiles {
		rows[rowKey(row.Key, row.Degraded)] = row
	}
	return resp, rows, recorder.Body.String()
}

// rawProfileRows 把 profiles 原样解成 JSON 对象，供"这个键到底存不存在"的断言用。
// 结构体字段办不到这件事：键不存在与值是空串解出来是同一个零值。
func rawProfileRows(t *testing.T, body string) map[string]map[string]any {
	t.Helper()

	var doc struct {
		Profiles []map[string]any `json:"profiles"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &doc))

	rows := make(map[string]map[string]any, len(doc.Profiles))
	for _, row := range doc.Profiles {
		key, _ := row["key"].(string)
		degraded, _ := row["degraded"].(bool)
		rows[rowKey(key, degraded)] = row
	}
	return rows
}

// TestExecutorsResponse_TwoSourcesAndDegradation 是卡 §5.1：三种来源状态各一条，
// 降级那条与它撞名的那条注册键相同、排在其后，而且那个键仍然归配置那条。
func TestExecutorsResponse_TwoSourcesAndDegradation(t *testing.T) {
	fixture := newTwoSourceFixture(t, true, t.TempDir())
	resp, rows, _ := listExecutors(t, fixture.server(t))

	require.Len(t, resp.Profiles, 5, "四条生效 + 一条降级")

	// 配置侧：读得到、改不了
	cfgOnly := rows[rowKey("exec.cfg_only", false)]
	assert.Equal(t, "config", cfgOnly.Source)
	assert.False(t, cfgOnly.Editable, "配置侧档位在这里是只读的：要改就改 yaml 并重启")
	assert.False(t, cfgOnly.Degraded)

	// 文件侧：改得了
	storeHealth := rows[rowKey("exec.store_health", false)]
	assert.Equal(t, "store", storeHealth.Source)
	assert.True(t, storeHealth.Editable)
	assert.False(t, storeHealth.Degraded)

	// 降级那条：同一个键的第二行，available=false，reason 说清为什么没生效
	degraded := rows[rowKey("exec.cfg_health", true)]
	require.True(t, degraded.Degraded, "撞名那条要在响应里看得见：%+v", resp.Profiles)
	assert.Equal(t, "store", degraded.Source, "它仍然来自档位文件")
	assert.False(t, degraded.RuntimeOK, "没注册处理函数的档位不可能可用")
	assert.False(t, degraded.Editable, "改它等于改一条没生效的记录，页面上不给入口")
	assert.Contains(t, degraded.Reason, "executors.commands", degraded.Reason)
	assert.Contains(t, degraded.Reason, "cfg_health", "文案要点名是哪条撞了")

	// 生效那条同时存在，且注册表里那个键属于配置侧
	configSide := rows[rowKey("exec.cfg_health", false)]
	assert.Equal(t, "config", configSide.Source)
	assert.Equal(t, "https://api.example.com/health", configSide.URL, "注册键归配置那条")
	source, ok := fixture.registry.SourceOf("exec.cfg_health")
	require.True(t, ok)
	assert.Equal(t, executor.SourceConfig, source, "降级条目不许顶掉注册表里的那条")

	// 顺序：按注册键字典序，同键时生效那条在前
	var order []string
	for _, row := range resp.Profiles {
		order = append(order, rowKey(row.Key, row.Degraded))
	}
	assert.Equal(t, []string{
		rowKey("exec.cfg_health", false), rowKey("exec.cfg_health", true),
		rowKey("exec.cfg_only", false), rowKey("exec.store_abs", false),
		rowKey("exec.store_health", false),
	}, order)
}

// TestExecutorsResponse_WebDisabledKeepsEverythingButEditable 是卡 §5.2 的对照用例：
// 关掉在线管理只该让 editable 变 false，其余字段一个都不该跟着动。
func TestExecutorsResponse_WebDisabledKeepsEverythingButEditable(t *testing.T) {
	root := t.TempDir()
	on := newTwoSourceFixture(t, true, root)
	off := newTwoSourceFixture(t, false, root)

	respOn, _, _ := listExecutors(t, on.server(t))
	respOff, _, _ := listExecutors(t, off.server(t))

	assert.True(t, respOn.WebEnabled)
	assert.False(t, respOff.WebEnabled, "关闭时这个键仍然给出而不是省略：它是前端唯一的判据")
	require.Len(t, respOff.Profiles, len(respOn.Profiles))

	for i := range respOn.Profiles {
		want := respOn.Profiles[i]
		want.Editable = false
		assert.Equal(t, want, respOff.Profiles[i],
			"第 %d 行除了 editable 之外不该有任何差别", i)
	}
}

// TestExecutorsResponse_PathDisplay 是卡 §5.3 的两种反面：http 档位整个键省略
// （不是空串），workspace 之外的档位给绝对写法（不是 ..\..\ 那种上跳形式）。
func TestExecutorsResponse_PathDisplay(t *testing.T) {
	fixture := newTwoSourceFixture(t, true, t.TempDir())
	_, rows, body := listExecutors(t, fixture.server(t))

	raw := rawProfileRows(t, body)
	_, hasKey := raw[rowKey("exec.store_health", false)]["path_display"]
	assert.False(t, hasKey, "http 档位不该带 path_display 这个键，实际 %s", body)
	assert.Empty(t, rows[rowKey("exec.store_health", false)].PathDisplay)

	abs := rows[rowKey("exec.store_abs", false)]
	assert.True(t, filepath.IsAbs(abs.PathDisplay), "越界档位给的是绝对路径，实际 %q", abs.PathDisplay)
	assert.True(t, strings.HasSuffix(filepath.ToSlash(abs.PathDisplay), "elsewhere/abs_report.mjs"),
		"绝对路径要指到那个文件本身，实际 %q", abs.PathDisplay)
	assert.NotContains(t, abs.PathDisplay, "..", "不该给出上跳形式：那等于让人自己拼绝对路径")
	assert.NotContains(t, abs.PathDisplay, fixture.workspace, "越界那条与 workspace 无关")
}

// TestExecutorsResponse_PathDisplayInsideWorkspace 单独一条走 workspace 内的相对写法。
// 判"在不在 workspace 内"只能用 withinDirectory（D-0201）：相对写法的形状不是判据，
// 同盘越界它同样能给出 scripts/../something 这种看着像相对的形式。
func TestExecutorsResponse_PathDisplayInsideWorkspace(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "w07-inside")
	cfg := core.DefaultConfig()
	cfg.Executors.Enabled = true
	cfg.Executors.WebEnabled = true
	cfg.Executors.Workspace = workspace
	registry, err := executor.NewRegistry(cfg, newTestLogger())
	require.NoError(t, err)

	cmd := core.ExecutorCommand{
		Name: "in_workspace", Kind: string(executor.KindScript),
		Runtime: "node", Script: "scripts/report.mjs", Timeout: time.Minute,
	}
	entries, warnings := executor.MergeStoreProfiles(cfg,
		[]core.ExecutorProfileRecord{core.NewExecutorProfileRecord(cmd)})
	require.Empty(t, warnings)
	require.NoError(t, registry.ApplyStore(entries))

	_, rows, _ := listExecutors(t, newSecurityServer(t, Security{}, WithExecutorRegistry(registry)))
	row := rows[rowKey("exec.in_workspace", false)]
	assert.Equal(t, "scripts/report.mjs", filepath.ToSlash(row.PathDisplay))
	assert.NotContains(t, row.PathDisplay, workspace, "workspace 内的档位不把绝对路径透出去")
}

// TestExecutorsResponse_TopLevelFacts 守住顶层两件事：runtime_allow 在执行器关闭时照样给
// （它是配置事实，与 required_role / max_timeout 的"关闭时不给"故意不同），
// 以及这份名单来自归一化之后的配置而不是写死在前端。
func TestExecutorsResponse_TopLevelFacts(t *testing.T) {
	cfg := core.DefaultConfig()
	cfg.Executors.Enabled = false
	cfg.Executors.WebEnabled = false
	cfg.Executors.RuntimeAllow = []string{"python", "node"}
	closed, err := executor.NewRegistry(cfg, newTestLogger())
	require.NoError(t, err)

	recorder := doGet(t, newSecurityServer(t, Security{}, WithExecutorRegistry(closed)), executorsRoute, nil)
	require.Equal(t, http.StatusOK, recorder.Code)

	var resp ListExecutorsResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &resp))
	assert.False(t, resp.Enabled)
	assert.False(t, resp.WebEnabled)
	assert.Equal(t, []string{"python", "node"}, resp.RuntimeAllow,
		"执行器关着也给出解释器名单：它说的是配置写了什么，不是这台机器现在能跑什么")
	assert.Nil(t, resp.RequiredRole, "required_role 的既有口径不变")
	assert.Empty(t, resp.MaxTimeout)
}

// TestExecutorsResponse_NewNamesAreNotRequestFields 是卡 §5.4 的前半：
// 新字段的名字不能与档位文件记录的字段撞名（否则页面回填时会把展示值写回定义），
// 而响应里与请求体同名的那批字段名不许漂移。
func TestExecutorsResponse_NewNamesAreNotRequestFields(t *testing.T) {
	writable := jsonKeys(reflect.TypeOf(core.ExecutorProfileRecord{}))
	for _, name := range []string{"source", "editable", "degraded", "path_display",
		"key", "runtime_ok", "reason", "has_secret_args", "preferred_result_direction"} {
		assert.NotContains(t, writable, name, "%q 是展示面的说法，不该出现在请求体字段里", name)
	}

	response := jsonKeys(reflect.TypeOf(ExecutorProfileResponse{}))
	for _, name := range []string{"name", "kind", "timeout", "max_parallel", "env_allow", "method", "args", "positional"} {
		assert.Contains(t, response, name, "响应里对得上请求体的字段名不能漂移：%+v", response)
	}
}

// TestExecutorsResponse_WriteRowMatchesTheListRow 是卡 §5.4 要的那条往返一致：
// 写端点的 201 响应与列表里的同一行必须一字不差——前端只有一份行渲染，
// 两处各算一次就会一处显示"可编辑"、另一处显示"只读"。
func TestExecutorsResponse_WriteRowMatchesTheListRow(t *testing.T) {
	// runtime 用 node：它在本机默认的解释器名单里（core.DefaultConfig 补齐），
	// 本卡的关注点是"这条档位的处境"，不是能不能跑。
	fixture := newProfileFixture(t, Security{}, nil)
	cmd := core.ExecutorCommand{
		Name: "listed", Kind: string(executor.KindScript),
		Runtime: "node", Script: "scripts/listed.mjs", Timeout: time.Minute,
	}
	recorder := doJSON(t, fixture.srv, http.MethodPost, profilesRoute, profileBody(t, cmd), nil)
	require.Equal(t, http.StatusCreated, recorder.Code, recorder.Body.String())

	created := decodeProfileResponse(t, recorder)
	assert.Equal(t, "store", created.Source)
	assert.True(t, created.Editable, "刚建出来的档位就是可编辑的那一种")
	assert.False(t, created.Degraded)
	assert.Equal(t, "scripts/listed.mjs", filepath.ToSlash(created.PathDisplay))

	resp, rows, _ := listExecutors(t, fixture.srv)
	require.NotEmpty(t, resp.Profiles)
	assert.Equal(t, created, rows[rowKey("exec.listed", false)],
		"同一个档位在写端点与读端点上必须给出同一份处境")
}

// TestExecutorsResponse_ListRowIsNotARequestBody 钉住往返的另一半：响应不是请求体。
// 它多出只给展示用的键，写端点一律按未知键拒掉——W08 的编辑表单因此必须自己留一份
// 档位定义，不能拿 GET 的结果直接 PUT 回去（响应里也没有 runtime/script 这些定义字段）。
func TestExecutorsResponse_ListRowIsNotARequestBody(t *testing.T) {
	fixture := newProfileFixture(t, Security{}, nil)
	cmd := core.ExecutorCommand{
		Name: "readonly_view", Kind: string(executor.KindScript),
		Runtime: "node", Script: "scripts/readonly.mjs", Timeout: time.Minute,
	}
	require.Equal(t, http.StatusCreated,
		doJSON(t, fixture.srv, http.MethodPost, profilesRoute, profileBody(t, cmd), nil).Code)

	_, rows, _ := listExecutors(t, fixture.srv)
	row := rows[rowKey("exec.readonly_view", false)]
	require.Equal(t, "store", row.Source, "先确认这一行本身是对的")

	body, err := json.Marshal(row)
	require.NoError(t, err)
	recorder := doJSON(t, fixture.srv, http.MethodPut, profilesRoute+"/readonly_view", string(body), nil)
	require.Equal(t, http.StatusBadRequest, recorder.Code, recorder.Body.String())
	assert.Contains(t, recorder.Body.String(), "unknown field", "只给展示用的键不该被写端点接受")
}

// jsonKeys 列出一个结构体所有字段的 json 名（去掉 omitempty 之类的选项）。
func jsonKeys(typ reflect.Type) []string {
	keys := make([]string, 0, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		tag := typ.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if name != "" {
			keys = append(keys, name)
		}
	}
	return keys
}
