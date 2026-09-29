package core

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestJobStatus_Constants(t *testing.T) {
	tests := []struct {
		name   string
		status JobStatus
		value  int
	}{
		{"StatusPending", StatusPending, 0},
		{"StatusRunning", StatusRunning, 1},
		{"StatusSuccess", StatusSuccess, 2},
		{"StatusFailed", StatusFailed, 3},
		{"StatusCancelled", StatusCancelled, 4},
		// paused 必须是 5：快照里的 status 是裸 int，插在中间会错位历史数据
		{"StatusPaused", StatusPaused, 5},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if int(tt.status) != tt.value {
				t.Errorf("Expected %s to be %d, got %d", tt.name, tt.value, int(tt.status))
			}
		})
	}
}

func TestJob_GetTriggerTime(t *testing.T) {
	triggerTime := time.Date(2024, 6, 15, 10, 30, 0, 0, time.UTC)
	job := &Job{
		ID:        "test-job-1",
		TriggerAt: triggerTime,
	}

	result := job.GetTriggerTime()
	if !result.Equal(triggerTime) {
		t.Errorf("Expected trigger time %v, got %v", triggerTime, result)
	}
}

func TestJob_GetID(t *testing.T) {
	job := &Job{
		ID: "test-job-123",
	}

	result := job.GetID()
	if result != "test-job-123" {
		t.Errorf("Expected ID 'test-job-123', got '%s'", result)
	}
}

func TestJob_CloneForRetry(t *testing.T) {
	originalTime := time.Now()
	nextTime := originalTime.Add(5 * time.Minute)

	handler := func(ctx context.Context, job *Job) error {
		return nil
	}

	original := &Job{
		ID:         "original-job",
		Name:       "Test Job",
		Payload:    []byte("test payload"),
		TriggerAt:  originalTime,
		Handler:    handler,
		CronExpr:   "*/5 * * * *",
		IsRepeat:   true,
		MaxRetries: 3,
		RetryCount: 1,
		RetryDelay: 10 * time.Second,
		Status:     StatusFailed,
		CreatedAt:  originalTime.Add(-1 * time.Hour),
		UpdatedAt:  originalTime,
		Attempts:   2,
	}

	clone := original.CloneForRetry(nextTime)

	// Verify ID is preserved to keep the retry chain intact
	if clone.ID != original.ID {
		t.Errorf("Clone ID should match original: got %s vs %s", clone.ID, original.ID)
	}

	// Verify copied fields
	if clone.Name != original.Name {
		t.Errorf("Expected Name %s, got %s", original.Name, clone.Name)
	}
	if string(clone.Payload) != string(original.Payload) {
		t.Errorf("Expected Payload %s, got %s", original.Payload, clone.Payload)
	}
	if !clone.TriggerAt.Equal(nextTime) {
		t.Errorf("Expected TriggerAt %v, got %v", nextTime, clone.TriggerAt)
	}
	// Retry clones must drop cron semantics, otherwise a failed one-shot job
	// would be re-armed as a recurring task after its final retry.
	if clone.CronExpr != "" {
		t.Errorf("Expected empty CronExpr on retry clone, got %s", clone.CronExpr)
	}
	if clone.IsRepeat {
		t.Error("Expected IsRepeat false on retry clone")
	}
	if clone.MaxRetries != original.MaxRetries {
		t.Errorf("Expected MaxRetries %d, got %d", original.MaxRetries, clone.MaxRetries)
	}

	// Verify incremented retry count
	if clone.RetryCount != original.RetryCount+1 {
		t.Errorf("Expected RetryCount %d, got %d", original.RetryCount+1, clone.RetryCount)
	}

	// Verify exponential backoff
	expectedDelay := original.RetryDelay * 2
	if clone.RetryDelay != expectedDelay {
		t.Errorf("Expected RetryDelay %v, got %v", expectedDelay, clone.RetryDelay)
	}

	// Verify status reset to pending
	if clone.Status != StatusPending {
		t.Errorf("Expected Status %v, got %v", StatusPending, clone.Status)
	}

	// Verify CreatedAt preserved
	if !clone.CreatedAt.Equal(original.CreatedAt) {
		t.Errorf("Expected CreatedAt %v, got %v", original.CreatedAt, clone.CreatedAt)
	}

	// Verify UpdatedAt is recent
	if clone.UpdatedAt.Before(original.UpdatedAt) {
		t.Error("Clone UpdatedAt should be after or equal to original UpdatedAt")
	}
}

func TestJob_ToSnapshot(t *testing.T) {
	now := time.Now()
	handler := func(ctx context.Context, job *Job) error {
		return nil
	}

	job := &Job{
		ID:         "job-123",
		Name:       "Test Job",
		Payload:    []byte("test data"),
		TriggerAt:  now,
		Handler:    handler,
		Ctx:        context.Background(),
		CronExpr:   "0 * * * *",
		IsRepeat:   true,
		MaxRetries: 5,
		RetryCount: 2,
		RetryDelay: 30 * time.Second,
		Status:     StatusRunning,
		CreatedAt:  now.Add(-1 * time.Hour),
		UpdatedAt:  now,
		Attempts:   3,
	}

	snapshot := job.ToSnapshot()

	// Verify all fields are correctly copied
	if snapshot.ID != job.ID {
		t.Errorf("Expected ID %s, got %s", job.ID, snapshot.ID)
	}
	if snapshot.Name != job.Name {
		t.Errorf("Expected Name %s, got %s", job.Name, snapshot.Name)
	}
	if string(snapshot.Payload) != string(job.Payload) {
		t.Errorf("Expected Payload %s, got %s", job.Payload, snapshot.Payload)
	}
	if !snapshot.TriggerAt.Equal(job.TriggerAt) {
		t.Errorf("Expected TriggerAt %v, got %v", job.TriggerAt, snapshot.TriggerAt)
	}
	if snapshot.CronExpr != job.CronExpr {
		t.Errorf("Expected CronExpr %s, got %s", job.CronExpr, snapshot.CronExpr)
	}
	if snapshot.IsRepeat != job.IsRepeat {
		t.Errorf("Expected IsRepeat %v, got %v", job.IsRepeat, snapshot.IsRepeat)
	}
	if snapshot.MaxRetries != job.MaxRetries {
		t.Errorf("Expected MaxRetries %d, got %d", job.MaxRetries, snapshot.MaxRetries)
	}
	if snapshot.RetryCount != job.RetryCount {
		t.Errorf("Expected RetryCount %d, got %d", job.RetryCount, snapshot.RetryCount)
	}
	if snapshot.RetryDelay != int64(job.RetryDelay) {
		t.Errorf("Expected RetryDelay %d, got %d", int64(job.RetryDelay), snapshot.RetryDelay)
	}
	if snapshot.Status != int(job.Status) {
		t.Errorf("Expected Status %d, got %d", int(job.Status), snapshot.Status)
	}
	if !snapshot.CreatedAt.Equal(job.CreatedAt) {
		t.Errorf("Expected CreatedAt %v, got %v", job.CreatedAt, snapshot.CreatedAt)
	}
	if !snapshot.UpdatedAt.Equal(job.UpdatedAt) {
		t.Errorf("Expected UpdatedAt %v, got %v", job.UpdatedAt, snapshot.UpdatedAt)
	}
	if snapshot.Attempts != job.Attempts {
		t.Errorf("Expected Attempts %d, got %d", job.Attempts, snapshot.Attempts)
	}
}

func TestJob_FromSnapshot(t *testing.T) {
	now := time.Now()
	snapshot := JobSnapshot{
		ID:         "snapshot-job-456",
		Name:       "Snapshot Job",
		Payload:    []byte("snapshot data"),
		TriggerAt:  now,
		CronExpr:   "*/10 * * * *",
		IsRepeat:   false,
		MaxRetries: 3,
		RetryCount: 1,
		RetryDelay: int64(15 * time.Second),
		Status:     int(StatusFailed),
		CreatedAt:  now.Add(-2 * time.Hour),
		UpdatedAt:  now.Add(-1 * time.Hour),
		Attempts:   2,
	}

	job := &Job{}
	job.FromSnapshot(snapshot)

	// Verify all fields are correctly restored
	if job.ID != snapshot.ID {
		t.Errorf("Expected ID %s, got %s", snapshot.ID, job.ID)
	}
	if job.Name != snapshot.Name {
		t.Errorf("Expected Name %s, got %s", snapshot.Name, job.Name)
	}
	if string(job.Payload) != string(snapshot.Payload) {
		t.Errorf("Expected Payload %s, got %s", snapshot.Payload, job.Payload)
	}
	if !job.TriggerAt.Equal(snapshot.TriggerAt) {
		t.Errorf("Expected TriggerAt %v, got %v", snapshot.TriggerAt, job.TriggerAt)
	}
	if job.CronExpr != snapshot.CronExpr {
		t.Errorf("Expected CronExpr %s, got %s", snapshot.CronExpr, job.CronExpr)
	}
	if job.IsRepeat != snapshot.IsRepeat {
		t.Errorf("Expected IsRepeat %v, got %v", snapshot.IsRepeat, job.IsRepeat)
	}
	if job.MaxRetries != snapshot.MaxRetries {
		t.Errorf("Expected MaxRetries %d, got %d", snapshot.MaxRetries, job.MaxRetries)
	}
	if job.RetryCount != snapshot.RetryCount {
		t.Errorf("Expected RetryCount %d, got %d", snapshot.RetryCount, job.RetryCount)
	}
	if job.RetryDelay != time.Duration(snapshot.RetryDelay) {
		t.Errorf("Expected RetryDelay %v, got %v", time.Duration(snapshot.RetryDelay), job.RetryDelay)
	}
	if !job.CreatedAt.Equal(snapshot.CreatedAt) {
		t.Errorf("Expected CreatedAt %v, got %v", snapshot.CreatedAt, job.CreatedAt)
	}
	if !job.UpdatedAt.Equal(snapshot.UpdatedAt) {
		t.Errorf("Expected UpdatedAt %v, got %v", snapshot.UpdatedAt, job.UpdatedAt)
	}
	if job.Attempts != snapshot.Attempts {
		t.Errorf("Expected Attempts %d, got %d", snapshot.Attempts, job.Attempts)
	}

	// 状态按快照原样还原：列表与详情接口靠它显示真实终态
	if job.Status != StatusFailed {
		t.Errorf("Expected Status %v, got %v", StatusFailed, job.Status)
	}
}

func TestJob_SnapshotRoundTrip(t *testing.T) {
	now := time.Now()
	original := &Job{
		ID:         "roundtrip-job",
		Name:       "Round Trip Test",
		Payload:    []byte("round trip data"),
		TriggerAt:  now,
		CronExpr:   "0 0 * * *",
		IsRepeat:   true,
		MaxRetries: 10,
		RetryCount: 5,
		RetryDelay: 60 * time.Second,
		Status:     StatusSuccess,
		CreatedAt:  now.Add(-3 * time.Hour),
		UpdatedAt:  now.Add(-30 * time.Minute),
		Attempts:   7,
	}

	// Convert to snapshot and back
	snapshot := original.ToSnapshot()
	restored := &Job{}
	restored.FromSnapshot(snapshot)

	// Verify key fields match (Status included)
	if restored.ID != original.ID {
		t.Errorf("ID mismatch: expected %s, got %s", original.ID, restored.ID)
	}
	if restored.Name != original.Name {
		t.Errorf("Name mismatch: expected %s, got %s", original.Name, restored.Name)
	}
	if string(restored.Payload) != string(original.Payload) {
		t.Errorf("Payload mismatch")
	}
	if !restored.TriggerAt.Equal(original.TriggerAt) {
		t.Errorf("TriggerAt mismatch")
	}
	if restored.RetryDelay != original.RetryDelay {
		t.Errorf("RetryDelay mismatch: expected %v, got %v", original.RetryDelay, restored.RetryDelay)
	}
	if restored.Status != original.Status {
		t.Errorf("Status should survive the round trip, got %v", restored.Status)
	}
}

func TestJob_CloneForRetry_ExponentialBackoff(t *testing.T) {
	job := &Job{
		ID:         "backoff-test",
		RetryDelay: 1 * time.Second,
		RetryCount: 0,
	}

	delays := []time.Duration{
		1 * time.Second,
		2 * time.Second,
		4 * time.Second,
		8 * time.Second,
		16 * time.Second,
	}

	current := job
	for i, expectedDelay := range delays {
		if current.RetryDelay != expectedDelay {
			t.Errorf("Retry %d: expected delay %v, got %v", i, expectedDelay, current.RetryDelay)
		}
		current = current.CloneForRetry(time.Now())
	}
}

func TestJob_EmptyPayload(t *testing.T) {
	job := &Job{
		ID:      "empty-payload-job",
		Payload: nil,
	}

	snapshot := job.ToSnapshot()
	if snapshot.Payload != nil {
		t.Error("Expected nil payload in snapshot")
	}

	restored := &Job{}
	restored.FromSnapshot(snapshot)
	if restored.Payload != nil {
		t.Error("Expected nil payload after restoration")
	}
}

func TestJob_ZeroValues(t *testing.T) {
	job := &Job{}

	if job.ID != "" {
		t.Error("Expected empty ID")
	}
	if job.Status != StatusPending {
		t.Errorf("Expected default status to be Pending (0), got %v", job.Status)
	}
	if job.MaxRetries != 0 {
		t.Errorf("Expected MaxRetries to be 0, got %d", job.MaxRetries)
	}
	if job.RetryCount != 0 {
		t.Errorf("Expected RetryCount to be 0, got %d", job.RetryCount)
	}
}

func TestJobStatus_Paused(t *testing.T) {
	if StatusPaused.String() != "paused" {
		t.Errorf("paused 的规范名应为 \"paused\"（与 HTTP API 的 status 取值一致），实际 %q", StatusPaused.String())
	}

	status, ok := ParseJobStatus("Paused")
	if !ok || status != StatusPaused {
		t.Errorf("ParseJobStatus 应大小写不敏感地解析 paused，得到 %v/%v", status, ok)
	}

	// paused 不是终态：它还要被 Resume 唤醒，也不该被终态留痕淘汰策略清掉
	if StatusPaused.IsTerminal() {
		t.Error("paused 不应是终态")
	}
	for _, terminal := range []JobStatus{StatusSuccess, StatusFailed, StatusCancelled} {
		if !terminal.IsTerminal() {
			t.Errorf("%s 应当是终态", terminal)
		}
	}

	// 未知名称仍然要被判失败，否则 status=paused 的拼写错误会静默变成 pending
	if _, ok := ParseJobStatus("pausable"); ok {
		t.Error("未知状态名不应解析成功")
	}
}

func TestJob_GroupSurvivesSnapshotRoundTrip(t *testing.T) {
	job := &Job{
		ID:        "job_group_1",
		Name:      "payment_check",
		Group:     "billing",
		Payload:   []byte(`{"order_id":"ORD-1"}`),
		TriggerAt: time.Now().Add(time.Minute),
		Status:    StatusPending,
	}

	snapshot := job.ToSnapshot()
	if snapshot.Group != "billing" {
		t.Errorf("ToSnapshot 应带上分组，实际 %q", snapshot.Group)
	}

	restored := &Job{}
	restored.FromSnapshot(snapshot)
	if restored.Group != "billing" {
		t.Errorf("FromSnapshot 应还原分组，实际 %q", restored.Group)
	}

	// 未分组是空串而不是某个占位名，列表过滤据此区分"未分组"与"无过滤"
	unassigned := &Job{ID: "job_group_2", Name: "email_send"}
	if encoded := unassigned.ToSnapshot().Group; encoded != "" {
		t.Errorf("未分组应为空串，实际 %q", encoded)
	}
}

func TestJob_CloneForRetry_KeepsGroup(t *testing.T) {
	// 重试副本沿用同一 ID 与分组；丢分组会让任务在重试后从分组视图里凭空消失
	original := &Job{ID: "job_retry_1", Name: "data_sync", Group: "nightly", MaxRetries: 3}
	clone := original.CloneForRetry(time.Now().Add(time.Minute))

	if clone.Group != "nightly" {
		t.Errorf("CloneForRetry 应保留分组，实际 %q", clone.Group)
	}
	if clone.ID != original.ID {
		t.Errorf("CloneForRetry 应保留原 ID，实际 %q", clone.ID)
	}
}

func TestJobSnapshot_LegacyJSONStillDecodes(t *testing.T) {
	// 真实历史数据：没有 group 字段、status 只用 0-4。升级后必须照常解码，
	// 缺字段落到空分组，而不是报错或把已有任务判成 paused。
	legacy := []byte(`{"id":"job_old_1","name":"payment_check","payload":"e30=",` +
		`"trigger_at":"2024-01-02T15:30:00+08:00","cron_expr":"","is_repeat":false,` +
		`"timeout":0,"max_retries":3,"retry_count":1,"retry_delay":60000000000,` +
		`"status":0,"created_at":"2024-01-02T15:20:00+08:00","updated_at":"2024-01-02T15:20:00+08:00","attempts":1}`)

	var snapshot JobSnapshot
	if err := json.Unmarshal(legacy, &snapshot); err != nil {
		t.Fatalf("旧快照解码失败: %v", err)
	}
	if snapshot.Group != "" {
		t.Errorf("旧数据应落到未分组，实际 %q", snapshot.Group)
	}
	if JobStatus(snapshot.Status) != StatusPending {
		t.Errorf("旧数据 status=0 应仍是 pending，实际 %s", JobStatus(snapshot.Status))
	}
	if snapshot.Exec != nil {
		t.Errorf("旧数据没有 exec 键，应解出 nil，实际 %+v", snapshot.Exec)
	}

	// 反向也要成立：未分组的任务落盘时不写 group 键，保持文件与旧版本一致
	encoded, err := json.Marshal((&Job{ID: "job_new_1", Name: "email_send", Status: StatusPending}).ToSnapshot())
	if err != nil {
		t.Fatalf("新快照编码失败: %v", err)
	}
	if bytes.Contains(encoded, []byte(`"group"`)) {
		t.Errorf("未分组不应写出 group 键，实际 %s", encoded)
	}
}

// fullExecMeta 是一份每个字段都有值的执行摘要，供搬运测试逐字段比对。
func fullExecMeta() *ExecMeta {
	return &ExecMeta{
		Kind:       "script",
		Profile:    "nightly_report",
		ExitCode:   3,
		Signal:     "SIGTERM",
		HTTPStatus: 502,
		DurationMs: 1500,
		OutBytes:   2048,
		ErrBytes:   512,
		Truncated:  true,
		Permanent:  true,
		Preview:    "tail of the output",
		Artifact:   "available",
	}
}

func TestJob_ExecSurvivesSnapshotRoundTrip(t *testing.T) {
	minimal := &ExecMeta{Kind: "http", Profile: "ping_home", DurationMs: 12}

	tests := []struct {
		name string
		meta *ExecMeta
	}{
		{name: "全部字段有值", meta: fullExecMeta()},
		{name: "只有必填字段", meta: minimal},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			job := &Job{ID: "job_exec_1", Name: "exec.nightly_report", Exec: tc.meta}

			snapshot := job.ToSnapshot()
			if snapshot.Exec != tc.meta {
				t.Error("ToSnapshot 应带上执行摘要（复制指针即可，不该丢）")
			}

			// 绕一圈真实的 JSON：字段名写错、标签漏写 omitempty 都只在这里才暴露
			encoded, err := json.Marshal(snapshot)
			if err != nil {
				t.Fatalf("快照编码失败: %v", err)
			}
			var decoded JobSnapshot
			if err := json.Unmarshal(encoded, &decoded); err != nil {
				t.Fatalf("快照解码失败: %v", err)
			}

			restored := &Job{}
			restored.FromSnapshot(decoded)
			if restored.Exec == nil {
				t.Fatalf("FromSnapshot 应还原执行摘要，实际为 nil（encoded=%s）", encoded)
			}
			if *restored.Exec != *tc.meta {
				t.Errorf("执行摘要逐字段应相等，实际 %+v", *restored.Exec)
			}
		})
	}
}

func TestJob_CloneForRetry_DropsExec(t *testing.T) {
	original := &Job{
		ID:         "job_exec_retry",
		Name:       "exec.nightly_report",
		MaxRetries: 3,
		RetryCount: 1,
		Exec:       fullExecMeta(),
	}

	clone := original.CloneForRetry(time.Now().Add(time.Minute))

	// 重试副本代表一次新的执行：留着旧结论会让 /jobs/:id 在新一轮跑完前显示上一次的退出码
	if clone.Exec != nil {
		t.Errorf("CloneForRetry 不应带上一次的执行摘要，实际 %+v", *clone.Exec)
	}
	if clone.RetryCount != original.RetryCount+1 {
		t.Errorf("重试次数应递增，实际 %d", clone.RetryCount)
	}
	if original.Exec == nil {
		t.Error("克隆副本不该改动原任务摘要，它还要作为本次失败的原因落盘")
	}
}

func TestExecMeta_OmitEmptyKeys(t *testing.T) {
	// 只填"没有 omitempty 的四个必填字段"，其余留零值
	encoded, err := json.Marshal(&ExecMeta{Kind: "script", Profile: "nightly_report"})
	if err != nil {
		t.Fatalf("编码失败: %v", err)
	}
	text := string(encoded)

	for _, key := range []string{`"exit_code"`, `"signal"`, `"http_status"`, `"truncated"`, `"permanent"`, `"preview"`, `"artifact"`} {
		if bytes.Contains(encoded, []byte(key)) {
			t.Errorf("零值字段 %s 应被省略，jobs.json 会被它撑大，实际 %s", key, text)
		}
	}
	// duration_ms / out_bytes / err_bytes 没写 omitempty：它们为 0 也要落盘，
	// 因为"跑了 0 毫秒"与"没有这个字段"在排障时是两件事。
	for _, key := range []string{`"kind"`, `"profile"`, `"duration_ms"`, `"out_bytes"`, `"err_bytes"`} {
		if !bytes.Contains(encoded, []byte(key)) {
			t.Errorf("必填字段 %s 应始终写出，实际 %s", key, text)
		}
	}
}

func TestJobSnapshot_OmitsExecWhenAbsent(t *testing.T) {
	// 绝大多数任务不是执行器任务：它们落盘时必须与升级前的字节结构一致，
	// 凭空多出 "exec":null 会让每份 jobs.json 都变大一点，也让旧代码读到陌生键。
	encoded, err := json.Marshal((&Job{ID: "job_plain", Name: "payment_check", Status: StatusPending}).ToSnapshot())
	if err != nil {
		t.Fatalf("编码失败: %v", err)
	}
	if bytes.Contains(encoded, []byte(`"exec"`)) {
		t.Errorf("没有执行摘要时不应写出 exec 键，实际 %s", encoded)
	}

	// 反向：未知键在快照解码时被忽略，所以带着 exec 键的文件在旧代码里也能读（回滚路径）
	withUnknown := []byte(`{"id":"job_1","name":"payment_check","payload":null,"trigger_at":"2024-01-02T15:30:00Z",` +
		`"exec":{"kind":"script","profile":"x","duration_ms":1,"out_bytes":2,"err_bytes":3},"status":0}`)
	var snapshot JobSnapshot
	if err := json.Unmarshal(withUnknown, &snapshot); err != nil {
		t.Errorf("带 exec 的快照解码失败: %v", err)
	}
}

// TestTrimExecPreview 检查输出预览的裁剪规则：取尾部、不超过上限、起点落在字符边界上。
// 执行侧写预览、事件侧与接口侧各自再裁一次，三处都调这一个函数，所以规则只在这里测。
func TestTrimExecPreview(t *testing.T) {
	if got := TrimExecPreview("abcdefghij", 8); got != "cdefghij" {
		t.Errorf("尾部裁剪不符，实际 %q", got)
	}
	if got := TrimExecPreview("abcdefghij", 20); got != "abcdefghij" {
		t.Errorf("短于上限时应原样返回，实际 %q", got)
	}
	if got := TrimExecPreview("abcdefghij", 0); got != "" {
		t.Errorf("上限为 0 表示不带预览，实际 %q", got)
	}

	// 一个汉字三个字节：直接从尾部数四个字节会把"你"切成一半，接口里就多出一个替换字符
	trimmed := TrimExecPreview("abc你好", 4)
	if trimmed != "好" {
		t.Errorf("裁剪起点要退到字符边界，实际 %q", trimmed)
	}
	if len(TrimExecPreview("a中b", 2)) > 2 {
		t.Error("裁剪结果不能超过上限")
	}
}
