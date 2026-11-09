package api

// /admin/runtime 的 db 段与 dropped_events_total（G4/Q05）。
//
// 三条读数判据：
//   - 未注入读数源时整个 db 键不出现（JSON 部署的形状；"没这个能力"不是"读数为零"）；
//   - 注入后字段齐且键集合封闭：dialect/schema_version/pool/outbox_pending，
//     pool 六个读数——多一个键就要显式改这里的清单（与 reloadStatusKeys 同一口径）；
//   - 读数源报错时 db 键缺席、端点照常 200——半截读数（schema_version=0 读作空库）
//     比缺席更容易误读。

import (
	"errors"
	"testing"

	"github.com/jiujuan/godelayq/internal/core"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubDBRuntime 是 dbRuntimeSource 的替身：交回用例给的那份读数，并记被读了几次。
type stubDBRuntime struct {
	stats core.DBRuntimeStats
	err   error
	calls int
}

func (s *stubDBRuntime) DBRuntime() (core.DBRuntimeStats, error) {
	s.calls++
	return s.stats, s.err
}

// dbKeys / dbPoolKeys 是 db 段与其 pool 子对象的封闭键清单，
// "该在的都在"与"不许冒出别的键"两份断言读同一份清单。
var dbKeys = []string{"dialect", "schema_version", "pool", "outbox_pending"}

var dbPoolKeys = []string{"max_open", "open", "in_use", "idle", "wait_count", "wait_duration_ns"}

func TestGetRuntime_HasNoDBFieldWhenNotInjected(t *testing.T) {
	srv := newSecurityServer(t, Security{})

	raw, body := runtimeBody(t, srv, nil)

	_, hasDB := raw["db"]
	assert.False(t, hasDB, `未注入读数源时响应里出现了 db 键：%s`, body)
	assert.NotContains(t, body, `"db"`, "整份原文里都不该出现这个键名")
	// dropped_events_total 是值类型读数，始终在位：零值 = 没丢过，是真实读数不是缺席。
	assert.Contains(t, raw, "dropped_events_total")
	assert.Equal(t, float64(0), raw["dropped_events_total"])
}

func TestGetRuntime_ExposesDBRuntime(t *testing.T) {
	stub := &stubDBRuntime{stats: core.DBRuntimeStats{
		Dialect:       "sqlite",
		SchemaVersion: 4,
		Pool: core.DBPoolStats{
			MaxOpen: 1, Open: 1, InUse: 1, Idle: 0,
			WaitCount: 3, WaitDurationNs: 1500,
		},
		OutboxPending: 7,
	}}
	srv := newSecurityServer(t, Security{}, WithDBRuntime(stub))

	raw, body := runtimeBody(t, srv, nil)

	dbObj, ok := raw["db"].(map[string]any)
	require.True(t, ok, body)
	assert.Equal(t, "sqlite", dbObj["dialect"])
	assert.Equal(t, float64(4), dbObj["schema_version"])
	assert.Equal(t, float64(7), dbObj["outbox_pending"])

	// 键集合封闭：读数形状的变化必须显式改用例，不能悄悄混进响应。
	assert.ElementsMatch(t, dbKeys, mapKeys(dbObj))
	pool, ok := dbObj["pool"].(map[string]any)
	require.True(t, ok, body)
	assert.ElementsMatch(t, dbPoolKeys, mapKeys(pool))
	assert.Equal(t, float64(1), pool["max_open"])
	assert.Equal(t, float64(1500), pool["wait_duration_ns"])

	assert.Equal(t, 1, stub.calls, "一次请求恰好读一次")
}

// 读数源报错：db 键缺席、其余读数照旧、端点 200——诊断端点不因一个读数失败而整体失败。
func TestGetRuntime_DBAbsentWhenReadoutFails(t *testing.T) {
	stub := &stubDBRuntime{err: errors.New("readout boom")}
	srv := newSecurityServer(t, Security{}, WithDBRuntime(stub))

	raw, body := runtimeBody(t, srv, nil)

	_, hasDB := raw["db"]
	assert.False(t, hasDB, `读数失败时响应里出现了 db 键：%s`, body)
	assert.Contains(t, raw, "scheduler")
	assert.Contains(t, raw, "dropped_events_total")
}

// 丢弃计数经端点可见。确切数字不在这里断：端点读的是全总线共享的计数器，
// 测试服务器自带的订阅者（事件历史缓冲等）在超载时同样丢弃，增量不止本用例
// 订阅者的那份——"缓冲 2 丢 3"那类精确断言归 core 的 EventBus 单测，
// 这里只判两件事：端点读数与总线一致，制造超载后读数在增长。
func TestGetRuntime_DroppedEventsTotal(t *testing.T) {
	srv := newSecurityServer(t, Security{})
	bus := srv.scheduler.GetEventBus()

	_, ch := bus.Subscribe(core.EventJobStarted)
	before, _ := runtimeBody(t, srv, nil)
	d0, ok := before["dropped_events_total"].(float64)
	require.True(t, ok, before)

	for i := 0; i < 105; i++ {
		bus.Publish(core.Event{Type: core.EventJobStarted, JobID: "j"})
	}
	assert.Equal(t, 100, len(ch), "本用例订阅者的缓冲按容量收满")

	raw, _ := runtimeBody(t, srv, nil)
	d1, ok := raw["dropped_events_total"].(float64)
	require.True(t, ok, raw)
	assert.Equal(t, float64(bus.Dropped()), d1,
		"端点读数与总线计数器必须是同一个数字（测试内无并发发布者）")
	assert.Greater(t, d1, d0, "制造超载后读数必须增长")
}

// mapKeys 取对象的键集合（键集合封闭断言用）。
func mapKeys(obj map[string]any) []string {
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	return keys
}
