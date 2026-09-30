package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"godelayq/core"
	"godelayq/executor"
)

// fakeArtifactIndex 是 executor.ArtifactIndexer 的替身：列表端点与 purged 标注只关心
// 自己被怎么调用，真读库在 store/sqlite 那一侧有用例。
type fakeArtifactIndex struct {
	rows      []executor.IndexRecord
	listErr   error
	markErr   error
	recordErr error

	listCalls   []string
	markPurged  []artifactMark
	recorded    []executor.IndexRecord
	deletedJobs []string
}

// artifactMark 是一次 MarkPurged 或 MarkAllPurged 调用；后者没有 attempt，记成 -1。
type artifactMark struct {
	JobID   string
	Attempt int
}

func (f *fakeArtifactIndex) Record(rec executor.IndexRecord) error {
	if f.recordErr != nil {
		return f.recordErr
	}
	f.recorded = append(f.recorded, rec)
	return nil
}

func (f *fakeArtifactIndex) MarkPurged(jobID string, attempt int) error {
	if f.markErr != nil {
		return f.markErr
	}
	f.markPurged = append(f.markPurged, artifactMark{JobID: jobID, Attempt: attempt})
	return nil
}

func (f *fakeArtifactIndex) MarkAllPurged(jobID string) error {
	f.markPurged = append(f.markPurged, artifactMark{JobID: jobID, Attempt: -1})
	return nil
}

func (f *fakeArtifactIndex) DeleteByJob(jobID string) error {
	f.deletedJobs = append(f.deletedJobs, jobID)
	return nil
}

func (f *fakeArtifactIndex) List(jobID string) ([]executor.IndexRecord, error) {
	f.listCalls = append(f.listCalls, jobID)
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.rows, nil
}

func (f *fakeArtifactIndex) Exists(string, int) (bool, error) { return false, nil }

// indexArtifact 造一行索引记录，字段填满以便对照响应。
func indexArtifact(jobID string, attempt int, profile string, out, errBytes int64,
	truncated bool, state string) executor.IndexRecord {
	return executor.IndexRecord{
		JobID:     jobID,
		Attempt:   attempt,
		Kind:      string(executor.KindScript),
		Profile:   profile,
		State:     state,
		CreatedAt: time.Date(2026, 10, 1, 12, 0, attempt, 0, time.UTC),
		Info: executor.ArtifactInfo{
			JobID:     jobID,
			Attempt:   attempt,
			OutBytes:  out,
			ErrBytes:  errBytes,
			Truncated: truncated,
		},
	}
}

func decodeArtifacts(t *testing.T, recorder *httptest.ResponseRecorder) JobArtifactsResponse {
	t.Helper()

	var resp JobArtifactsResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &resp), recorder.Body.String())
	return resp
}

// serverWithIndex 造一台挂了产物索引的服务：索引装在产物存储上，与真实装配同形。
func serverWithIndex(t *testing.T, sec Security, index executor.ArtifactIndexer) *Server {
	t.Helper()

	artifacts := newArtifactStoreFor(t, 1<<20)
	if index != nil {
		artifacts.SetIndex(index)
	}
	return newSecurityServer(t, sec, WithArtifacts(artifacts))
}

func TestListJobArtifacts_NoIndex503(t *testing.T) {
	// 有产物存储、没挂索引：这就是"这台服务器没在记"，不能回一份空列表装作记过了
	srv := serverWithIndex(t, Security{}, nil)

	recorder := doGet(t, srv, "/api/v1/jobs/job-1/artifacts", nil)
	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)

	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &resp))
	assert.Equal(t, http.StatusServiceUnavailable, resp.Code)
	assert.Equal(t, "artifact index is not configured", resp.Message)
	assert.Contains(t, resp.Details, "observability.artifacts.enabled")
}

func TestListJobArtifacts_NoArtifacts503(t *testing.T) {
	// 连产物存储都没有（executors.enabled=false）：同一句 503。
	// 索引挂在存储上，所以判空只有一层，不会把"没存储"说成"没索引"。
	srv := newSecurityServer(t, Security{})

	recorder := doGet(t, srv, "/api/v1/jobs/job-1/artifacts", nil)
	assert.Equal(t, http.StatusServiceUnavailable, recorder.Code)
}

func TestListJobArtifacts_Empty200(t *testing.T) {
	srv := serverWithIndex(t, Security{}, &fakeArtifactIndex{})

	recorder := doGet(t, srv, "/api/v1/jobs/job-none/artifacts", nil)
	require.Equal(t, http.StatusOK, recorder.Code)
	assert.Contains(t, recorder.Body.String(), `"items":[]`, "空列表必须是 []，不是 null")

	resp := decodeArtifacts(t, recorder)
	assert.Equal(t, "job-none", resp.JobID)
	assert.Zero(t, resp.Count)
	assert.Empty(t, resp.Items)
}

func TestListJobArtifacts_RecordsAndOrder(t *testing.T) {
	index := &fakeArtifactIndex{rows: []executor.IndexRecord{
		indexArtifact("job-1", 1, "hello", 42, 512, false, core.ArtifactAvailable),
		indexArtifact("job-1", 2, "hello", 0, 128, true, core.ArtifactPurged),
	}}
	srv := serverWithIndex(t, Security{}, index)

	resp := decodeArtifacts(t, doGet(t, srv, "/api/v1/jobs/job-1/artifacts", nil))
	require.Equal(t, 2, resp.Count)
	require.Len(t, resp.Items, 2)
	assert.Equal(t, "job-1", resp.JobID)

	assert.Equal(t, 1, resp.Items[0].Attempt, "按 attempt 升序：重试链的自然阅读顺序")
	assert.Equal(t, "script", resp.Items[0].Kind)
	assert.Equal(t, "hello", resp.Items[0].Profile)
	assert.Equal(t, int64(42), resp.Items[0].OutBytes)
	assert.Equal(t, int64(512), resp.Items[0].ErrBytes)
	assert.False(t, resp.Items[0].Truncated)
	assert.Equal(t, core.ArtifactAvailable, resp.Items[0].State)

	assert.Equal(t, 2, resp.Items[1].Attempt)
	assert.True(t, resp.Items[1].Truncated)
	assert.Equal(t, core.ArtifactPurged, resp.Items[1].State)

	// 响应里不该出现任何路径写法：连相对路径那一列都不给
	body := doGet(t, srv, "/api/v1/jobs/job-1/artifacts", nil).Body.String()
	for _, forbidden := range []string{"out_rel", "err_rel", "out_path", "err_path", "a1.out"} {
		assert.NotContains(t, body, forbidden)
	}
	assert.Equal(t, []string{"job-1", "job-1"}, index.listCalls)
}

func TestListJobArtifacts_ReadErrorIs500(t *testing.T) {
	srv := serverWithIndex(t, Security{}, &fakeArtifactIndex{
		listErr: errors.New("no such table: artifact_index"),
	})

	recorder := doGet(t, srv, "/api/v1/jobs/job-1/artifacts", nil)
	require.Equal(t, http.StatusInternalServerError, recorder.Code)

	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &resp))
	assert.Equal(t, "failed to load artifact records", resp.Message)
	assert.Contains(t, resp.Details, "artifact_index")
}

// TestListJobArtifacts_BadJobID400 守住"客户端写错的 ID 不该被算成服务器坏了"：
// 索引实现会拒绝任何不成目录名的 ID，那份错误如果原样透出就是 500。
func TestListJobArtifacts_BadJobID400(t *testing.T) {
	index := &fakeArtifactIndex{}
	srv := serverWithIndex(t, Security{}, index)

	recorder := doGet(t, srv, "/api/v1/jobs/..escape/artifacts", nil)
	require.Equal(t, http.StatusBadRequest, recorder.Code, "%s", recorder.Body.String())

	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &resp))
	assert.Equal(t, "invalid id", resp.Message)
	assert.Empty(t, index.listCalls, "校验不过就不该去问库")
}

func TestListJobArtifacts_RoleViewer(t *testing.T) {
	srv := serverWithIndex(t, accountsSecurity(t), &fakeArtifactIndex{})

	anonymous := doGet(t, srv, "/api/v1/jobs/job-1/artifacts", nil)
	assert.Equal(t, http.StatusUnauthorized, anonymous.Code, "未认证进不了 reader 档")

	viewer := login(t, srv, testViewerName, testPassword)
	assert.Equal(t, http.StatusOK,
		doGet(t, srv, "/api/v1/jobs/job-1/artifacts", bearer(viewer.AccessToken)).Code,
		"元信息不含正文，viewer 档够用")
}

// TestMarkArtifactPurged_UpdatesIndex 走一次真实的读取失败路径：正文文件不在，
// 快照与索引两侧要同时变成 purged，而且标的是本次读的那一次尝试。
func TestMarkArtifactPurged_UpdatesIndex(t *testing.T) {
	index := &fakeArtifactIndex{}
	artifacts := newArtifactStoreFor(t, 1<<20)
	artifacts.SetIndex(index)

	inner, err := core.NewJSONFileStore(filepath.Join(t.TempDir(), "jobs.json"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = inner.Close() })

	srv := NewServer(core.NewScheduler(inner, nil, nil), inner, "0", Security{}, newTestLogger(),
		WithArtifacts(artifacts))
	putSnapshot(t, srv, "job-purged", 3, &core.ExecMeta{
		Kind: "script", Profile: "hello", Artifact: core.ArtifactAvailable,
	})

	result := decodeJobResult(t, doGet(t, srv, "/api/v1/jobs/job-purged/result", nil))
	require.NotNil(t, result.Meta)
	assert.False(t, result.Found)
	assert.Equal(t, core.ArtifactPurged, result.Meta.Artifact)

	require.Len(t, index.markPurged, 1, "索引要跟着标注一次")
	assert.Equal(t, "job-purged", index.markPurged[0].JobID)
	assert.Equal(t, result.Attempt, index.markPurged[0].Attempt,
		"标的是本次读取的那一次尝试，不是 snapshot.Attempts")

	// 标注失败不改响应：快照那份结论已经写成功，正文本来就读不到
	index.markErr = errors.New("index is locked")
	repeat := decodeJobResult(t, doGet(t, srv, "/api/v1/jobs/job-purged/result?attempt=2", nil))
	require.NotNil(t, repeat.Meta)
	assert.Equal(t, core.ArtifactPurged, repeat.Meta.Artifact)
	assert.Equal(t, 2, repeat.Attempt)
}
