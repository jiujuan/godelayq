package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"godelayq/core"
)

// newGroupServer 构造带分组注册表的服务器：注册表是可选依赖，
// 分组端点的测试必须显式注入，否则测的是 503 分支。
func newGroupServer(t *testing.T, sec Security) (*Server, *core.JSONFileGroupStore) {
	t.Helper()

	groups, err := core.NewJSONFileGroupStore(filepath.Join(t.TempDir(), "groups.json"))
	require.NoError(t, err)

	return newSecurityServer(t, sec, WithGroupStore(groups)), groups
}

func decodeGroups(t *testing.T, recorder *httptest.ResponseRecorder) []GroupResponse {
	t.Helper()

	var items []GroupResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &items), recorder.Body.String())
	return items
}

// listJobsByQuery 读取任务列表，用于断言改名/detach 之后任务到底挂在哪个组上。
func listJobsByQuery(t *testing.T, srv *Server, query string) (int, ListJobsResponse) {
	t.Helper()

	recorder := doGet(t, srv, "/api/v1/jobs"+query, nil)

	var resp ListJobsResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &resp), recorder.Body.String())
	return recorder.Code, resp
}

func TestGroupCRUDOverHTTP(t *testing.T) {
	srv, _ := newGroupServer(t, Security{})

	var created GroupResponse
	recorder := doJSON(t, srv, http.MethodPost, "/api/v1/groups",
		`{"name":"nightly","description":"夜间批处理","color":"#2563eb"}`, nil)
	require.Equal(t, http.StatusCreated, recorder.Code, recorder.Body.String())
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &created))
	assert.Equal(t, "nightly", created.Name)
	assert.Equal(t, "#2563eb", created.Color)
	assert.True(t, created.Registered)
	assert.Zero(t, created.JobCount)

	// 重名（含只差大小写）是 409：注册表主键忽略大小写
	assert.Equal(t, http.StatusConflict, doJSON(t, srv, http.MethodPost, "/api/v1/groups",
		`{"name":"Nightly"}`, nil).Code)

	// 非法名称/颜色都是 400，不该落进存储
	assert.Equal(t, http.StatusBadRequest, doJSON(t, srv, http.MethodPost, "/api/v1/groups",
		`{"name":"运维 组"}`, nil).Code)
	assert.Equal(t, http.StatusBadRequest, doJSON(t, srv, http.MethodPost, "/api/v1/groups",
		`{"name":"ok","color":"red"}`, nil).Code)

	listed := doGet(t, srv, "/api/v1/groups", nil)
	require.Equal(t, http.StatusOK, listed.Code)
	items := decodeGroups(t, listed)
	require.Len(t, items, 1)
	assert.Equal(t, "夜间批处理", items[0].Description)

	var updated GroupResponse
	recorder = doJSON(t, srv, http.MethodPut, "/api/v1/groups/nightly",
		`{"description":""}`, nil)
	require.Equal(t, http.StatusOK, recorder.Code)
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &updated))
	assert.Empty(t, updated.Description, "描述要能被清空")
	assert.Equal(t, "nightly", updated.Name, "没传 name 就是原地改名以外都不动")
	assert.True(t, updated.CreatedAt.Before(updated.UpdatedAt.Add(time.Second)), "更新时间应前进")

	assert.Equal(t, http.StatusNotFound, doJSON(t, srv, http.MethodPut, "/api/v1/groups/nope",
		`{"description":"x"}`, nil).Code)

	assert.Equal(t, http.StatusNoContent, doJSON(t, srv, http.MethodDelete, "/api/v1/groups/nightly", "", nil).Code)
	assert.Empty(t, decodeGroups(t, doGet(t, srv, "/api/v1/groups", nil)))
	assert.Equal(t, http.StatusNotFound, doJSON(t, srv, http.MethodDelete, "/api/v1/groups/nightly", "", nil).Code)
}

// TestGroupsListShowsUnregisteredTags 覆盖 §5.5 的兜底：组名可以不带注册就写在任务上，
// 列表必须看得见它，否则改组名半途失败留下的"半个旧组"无处下手。
func TestGroupsListShowsUnregisteredTags(t *testing.T) {
	srv, groups := newGroupServer(t, Security{})

	require.NoError(t, groups.Save(core.Group{Name: "nightly", Color: "#2563eb"}))
	require.NoError(t, srv.scheduler.Schedule(&core.Job{
		ID: "orphan-1", Name: "payment_check", TriggerAt: time.Now().Add(time.Hour), Group: "adhoc",
	}))
	require.NoError(t, srv.scheduler.Schedule(&core.Job{
		ID: "known-1", Name: "payment_check", TriggerAt: time.Now().Add(time.Hour), Group: "Nightly",
	}))
	_, err := srv.scheduler.Pause("known-1")
	require.NoError(t, err)

	items := decodeGroups(t, doGet(t, srv, "/api/v1/groups", nil))
	require.Len(t, items, 2, "注册表里的组 + 只存在于任务标签上的组")

	// 合并后的列表按名称字典序：前端只有一个分组下拉，不该分成两段
	assert.Equal(t, "adhoc", items[0].Name)
	assert.False(t, items[0].Registered)
	assert.Equal(t, 1, items[0].JobCount)

	assert.Equal(t, "nightly", items[1].Name)
	assert.True(t, items[1].Registered)
	assert.Equal(t, 1, items[1].JobCount, "任务标签的大小写与注册名不同也算同一个组")
	assert.Equal(t, 1, items[1].PausedCount)
}

func TestGroupRenameRetagsEveryJob(t *testing.T) {
	srv, groups := newGroupServer(t, Security{})

	require.NoError(t, groups.Save(core.Group{Name: "nightly"}))
	require.NoError(t, srv.scheduler.Schedule(&core.Job{
		ID: "rn-1", Name: "payment_check", TriggerAt: time.Now().Add(time.Hour), Group: "nightly",
	}))
	require.NoError(t, srv.scheduler.Schedule(&core.Job{
		ID: "rn-2", Name: "payment_check", TriggerAt: time.Now().Add(time.Hour), Group: "nightly",
	}))
	_, err := srv.scheduler.Pause("rn-2")
	require.NoError(t, err)
	// 终态留痕也跟着改名，否则历史列表会一直显示旧组名
	require.NoError(t, srv.store.Update(core.JobSnapshot{
		ID: "rn-3", Name: "payment_check", Group: "nightly",
		Status: int(core.StatusSuccess), UpdatedAt: time.Now(),
	}))

	var renamed GroupResponse
	recorder := doJSON(t, srv, http.MethodPut, "/api/v1/groups/nightly", `{"name":"nightly-b"}`, nil)
	require.Equal(t, http.StatusOK, recorder.Code)
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &renamed))
	assert.Equal(t, "nightly-b", renamed.Name)
	assert.Equal(t, 3, renamed.JobCount)

	// 注册表只剩新名：旧键必须一起消失，否则改名等于加了个组
	_, found, err := groups.Get("nightly")
	require.NoError(t, err)
	assert.False(t, found, "改名后旧组名不该还能查到")

	code, listing := listJobsByQuery(t, srv, "?group=nightly-b")
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, 3, listing.Total, "堆里的、暂停的、终态留痕的任务都要跟着改名")

	code, oldName := listJobsByQuery(t, srv, "?group=nightly")
	require.Equal(t, http.StatusOK, code)
	assert.Zero(t, oldName.Total)
}

func TestGroupRenameToExistingNameConflicts(t *testing.T) {
	srv, groups := newGroupServer(t, Security{})

	require.NoError(t, groups.Save(core.Group{Name: "a"}))
	require.NoError(t, groups.Save(core.Group{Name: "b"}))

	recorder := doJSON(t, srv, http.MethodPut, "/api/v1/groups/a", `{"name":"B"}`, nil)
	assert.Equal(t, http.StatusConflict, recorder.Code)

	// 改名失败不能留下半个注册表
	_, found, err := groups.Get("b")
	require.NoError(t, err)
	assert.True(t, found)
}

// TestGroupDeleteDefaultsToDetach 是决策 D5 的落点：admin 不用传参数也绝不会删任务。
func TestGroupDeleteDefaultsToDetach(t *testing.T) {
	srv, groups := newGroupServer(t, Security{})

	require.NoError(t, groups.Save(core.Group{Name: "nightly"}))
	for _, id := range []string{"del-1", "del-2"} {
		require.NoError(t, srv.scheduler.Schedule(&core.Job{
			ID: id, Name: "payment_check", TriggerAt: time.Now().Add(time.Hour), Group: "nightly",
		}))
	}

	assert.Equal(t, http.StatusNoContent,
		doJSON(t, srv, http.MethodDelete, "/api/v1/groups/nightly", "", nil).Code)

	_, listing := listJobsByQuery(t, srv, "?group=")
	assert.Equal(t, 2, listing.Total, "组没了，任务还在，只是不再归组")
	assert.Equal(t, 2, srv.scheduler.HeapLen(), "删除分组不该取消任何任务")
}

func TestGroupDeleteBlockStrategyRefusesNonEmptyGroup(t *testing.T) {
	srv, groups := newGroupServer(t, Security{})

	require.NoError(t, groups.Save(core.Group{Name: "nightly"}))
	require.NoError(t, srv.scheduler.Schedule(&core.Job{
		ID: "blk-1", Name: "payment_check", TriggerAt: time.Now().Add(time.Hour), Group: "nightly",
	}))

	recorder := doJSON(t, srv, http.MethodDelete, "/api/v1/groups/nightly?strategy=block", "", nil)
	require.Equal(t, http.StatusConflict, recorder.Code)

	_, found, err := groups.Get("nightly")
	require.NoError(t, err)
	assert.True(t, found, "block 语义下组必须原样保留")

	// 空组用 block 也能删
	require.NoError(t, groups.Save(core.Group{Name: "empty"}))
	assert.Equal(t, http.StatusNoContent,
		doJSON(t, srv, http.MethodDelete, "/api/v1/groups/empty?strategy=block", "", nil).Code)

	assert.Equal(t, http.StatusBadRequest,
		doJSON(t, srv, http.MethodDelete, "/api/v1/groups/empty?strategy=cascade", "", nil).Code,
		"任何策略名都不该被静默当成 detach 处理")
}

// TestGroupEndpointsUnavailableWithoutRegistry 保证没装配注册表的部署得到的是一句人话，
// 而不是 panic 或 500。
func TestGroupEndpointsUnavailableWithoutRegistry(t *testing.T) {
	srv := newSecurityServer(t, Security{})

	for method, target := range map[string]string{
		http.MethodGet:    "/api/v1/groups",
		http.MethodDelete: "/api/v1/groups/nightly",
	} {
		recorder := doJSON(t, srv, method, target, "", nil)
		assert.Equal(t, http.StatusServiceUnavailable, recorder.Code, method+" "+target)
	}
}

func TestGroupEndpointsGateRoles(t *testing.T) {
	srv, _ := newGroupServer(t, accountsSecurity(t))

	viewer := login(t, srv, testViewerName, testPassword)
	operator := login(t, srv, testOperatorName, testPassword)
	admin := login(t, srv, testAdminName, testPassword)

	require.Equal(t, http.StatusOK,
		doGet(t, srv, "/api/v1/groups", bearer(operator.AccessToken)).Code)
	assert.Equal(t, http.StatusForbidden,
		doJSON(t, srv, http.MethodPost, "/api/v1/groups", `{"name":"nightly"}`, bearer(viewer.AccessToken)).Code)

	created := doJSON(t, srv, http.MethodPost, "/api/v1/groups", `{"name":"nightly"}`, bearer(operator.AccessToken))
	require.Equal(t, http.StatusCreated, created.Code)

	assert.Equal(t, http.StatusOK,
		doJSON(t, srv, http.MethodPut, "/api/v1/groups/nightly", `{"description":"改描述"}`, bearer(operator.AccessToken)).Code,
		"operator 可以改名与改描述")

	// 删除牵连一批任务，档位要求更高
	assert.Equal(t, http.StatusForbidden,
		doJSON(t, srv, http.MethodDelete, "/api/v1/groups/nightly", "", bearer(operator.AccessToken)).Code)
	assert.Equal(t, http.StatusNoContent,
		doJSON(t, srv, http.MethodDelete, "/api/v1/groups/nightly", "", bearer(admin.AccessToken)).Code)
}
