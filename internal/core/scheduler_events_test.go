package core

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newEventScheduler 造一个只用来观察事件的调度器：executeJob 直接调用，
// 不起 worker 池也不等定时器，事件顺序因此是确定的。
func newEventScheduler(t *testing.T) (*Scheduler, *eventLog) {
	t.Helper()

	bus := NewEventBus(32)
	t.Cleanup(bus.Close)
	log := collectEvents(bus)

	return NewScheduler(newMockStore(), nil, bus), log
}

// eventResultOf 取出事件附加数据里的 result；第二个返回值为 false 表示这条事件没带摘要。
func eventResultOf(t *testing.T, event Event) (ExecMeta, bool) {
	t.Helper()

	if len(event.Data) == 0 {
		return ExecMeta{}, false
	}

	var fields struct {
		Error  string   `json:"error"`
		Result ExecMeta `json:"result"`
	}
	require.NoError(t, json.Unmarshal(event.Data, &fields), "事件 data 必须是 JSON：%s", event.Data)
	if fields.Result.Kind == "" {
		return ExecMeta{}, false
	}
	return fields.Result, true
}

// TestEvent_CarriesExecResult 固定"结论进事件、正文不进事件"这条口径：
// 实时页和详情页时间线要看得见退出码，但不该为此读产物文件。
func TestEvent_CarriesExecResult(t *testing.T) {
	scheduler, events := newEventScheduler(t)

	job := &Job{ID: "exec-ok", Name: "exec.demo", TriggerAt: time.Now()}
	job.Handler = func(_ context.Context, j *Job) error {
		j.Exec = &ExecMeta{
			Kind:     "script",
			Profile:  "demo",
			ExitCode: 3,
			Preview:  "last line",
			Artifact: ArtifactAvailable,
		}
		return nil
	}
	scheduler.executeJob(job)

	events.waitFor(t, EventJobCompleted, job.ID, time.Second)
	event, ok := events.last(EventJobCompleted, job.ID)
	require.True(t, ok, "完成事件必须发布")
	summary, ok := eventResultOf(t, event)
	require.True(t, ok, "带执行结论的完成事件要能在 data.result 里读到摘要")
	assert.Equal(t, "script", summary.Kind)
	assert.Equal(t, "demo", summary.Profile)
	assert.Equal(t, 3, summary.ExitCode)
	assert.Equal(t, "last line", summary.Preview)
	assert.Equal(t, ArtifactAvailable, summary.Artifact, "产物状态是摘要的一部分")
}

// TestEvent_FailureCarriesErrorAndResult 固定失败事件的形状：既有的 error 键不能被挤掉，
// 新的 result 键是追加的（api/history.go 与前端都按 error 读失败原因）。
func TestEvent_FailureCarriesErrorAndResult(t *testing.T) {
	scheduler, events := newEventScheduler(t)

	job := &Job{ID: "exec-bad", Name: "exec.demo", TriggerAt: time.Now()}
	job.Handler = func(_ context.Context, j *Job) error {
		j.Exec = &ExecMeta{Kind: "http", Profile: "demo", HTTPStatus: 500, DurationMs: 42}
		return errors.New("status 500")
	}
	scheduler.executeJob(job)

	events.waitFor(t, EventJobFailed, job.ID, time.Second)
	event, ok := events.last(EventJobFailed, job.ID)
	require.True(t, ok)

	var fields struct {
		Error  string   `json:"error"`
		Result ExecMeta `json:"result"`
	}
	require.NoError(t, json.Unmarshal(event.Data, &fields))
	assert.Equal(t, "status 500", fields.Error)
	assert.Equal(t, 500, fields.Result.HTTPStatus)
	assert.Equal(t, int64(42), fields.Result.DurationMs)
}

// TestEvent_NoExecMeansUnchangedData 是本卡回归面的守门用例：
// 非执行器任务的事件必须与改动前逐字节一致，否则 api 与前端对 data 的既有断言全要重写。
func TestEvent_NoExecMeansUnchangedData(t *testing.T) {
	scheduler, events := newEventScheduler(t)

	okJob := &Job{ID: "plain-ok", Name: "payment_check", TriggerAt: time.Now()}
	okJob.Handler = func(context.Context, *Job) error { return nil }
	scheduler.executeJob(okJob)

	badJob := &Job{ID: "plain-bad", Name: "payment_check", TriggerAt: time.Now()}
	badJob.Handler = func(context.Context, *Job) error { return errors.New("boom") }
	scheduler.executeJob(badJob)

	events.waitFor(t, EventJobCompleted, okJob.ID, time.Second)
	events.waitFor(t, EventJobFailed, badJob.ID, time.Second)
	completed, found := events.last(EventJobCompleted, okJob.ID)
	require.True(t, found)
	assert.Empty(t, completed.Data, "没有执行结论时成功事件的 data 整个缺席")
	failed, found := events.last(EventJobFailed, badJob.ID)
	require.True(t, found)
	assert.Equal(t, `{"error":"boom"}`, string(failed.Data),
		"只有 error 时必须与改动前的编码结果逐字节一致")
}

// TestEvent_PreviewLimitRespected 检查第二次裁剪：摘要里可能带着按旧配置写下的长预览，
// 事件却是在当前配置下发出的，而一条事件会推给全部订阅者。
func TestEvent_PreviewLimitRespected(t *testing.T) {
	scheduler, events := newEventScheduler(t)
	scheduler.SetEventPreviewLimit(8)

	job := &Job{ID: "exec-preview", Name: "exec.demo", TriggerAt: time.Now()}
	job.Handler = func(_ context.Context, j *Job) error {
		j.Exec = &ExecMeta{Kind: "script", Profile: "demo", Preview: strings.Repeat("x", 100)}
		return nil
	}
	scheduler.executeJob(job)

	events.waitFor(t, EventJobCompleted, job.ID, time.Second)
	event, ok := events.last(EventJobCompleted, job.ID)
	require.True(t, ok)
	summary, ok := eventResultOf(t, event)
	require.True(t, ok)
	assert.Len(t, summary.Preview, 8, "事件里的预览不能超过配置上限")

	// 裁剪只发生在事件副本上：快照引用的还是原对象，改它会让接口读到被削短的内容
	assert.Len(t, job.Exec.Preview, 100, "任务上的摘要不该被事件发布裁掉")
}

// TestEvent_PreviewLimitDefault 确认没装配执行器时的取值：上限来自默认配置而不是 0，
// 否则事件里永远只剩一个空预览。
func TestEvent_PreviewLimitDefault(t *testing.T) {
	scheduler, _ := newEventScheduler(t)
	assert.Equal(t, DefaultExecInlinePreview, scheduler.eventPreviewLimit)

	scheduler.SetEventPreviewLimit(0)
	assert.Equal(t, DefaultExecInlinePreview, scheduler.eventPreviewLimit, "非正数回退到默认值")
}
