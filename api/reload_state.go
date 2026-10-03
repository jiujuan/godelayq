package api

// 配置热重载状态的读口（TASK-R06 §3.5）。
//
// 单独一个文件而不是塞进 handlers_admin.go：这里是"一个可选依赖 + 一份转换"两件事，
// 与那个文件里的五个端点不同类；GetRuntime 只是多填一个字段。
//
// 为什么不直接序列化 core.ReloadState（R05 交给本卡的 D-R0502）：
// 那两个时间字段带的是 `json:"...,omitempty"`，而 encoding/json 的 omitempty 对 time.Time
// **不生效**（结构体永远非"空值"），从未尝试过重载的部署会因此被读出
// "last_attempt_at":"0001-01-01T00:00:00Z" 这样一个假时间。判"没试过"也不能看时间零值，
// 得看 Result 是不是空串。所以这里自己格式化这两个字段：零值就整个键不给。

import (
	"slices"
	"time"

	"godelayq/core"
)

// ReloadStateReader 提供最近一次配置重载的结论。cmd/server 在装配时把它接进服务，
// 真实实现是那边的读口句柄（它转发给 core.ConfigWatcher，未建监听器时给出一个明确原因）。
//
// 契约两条：
//  1. State() 必须能在任何时刻被调，且不许阻塞在一次进行中的重载上；
//  2. 交回的三份键清单可能与写它们的那一方共享底层数组（R05 的 D-R0514），
//     读侧一律复制后再整理，绝不就地排序或改写。
type ReloadStateReader interface {
	State() core.ReloadState
}

// ReloadStatus 是 /admin/runtime 的 reload 对象。
//
// 字段与 core.ReloadState 一一对应（设计文档 §5.4 的十个），只有两个时间换成了字符串：
// 零值时间会被格式化空串并整个键省掉，那是 D-R0502 说的那件事。
//
// 三份键清单只有键名，永远不含取值：配置里可能有凭据（core.ChangedKey 把取值挡在
// 状态对象之外，这里再加一条用例守住，见 §5.4 第 3 条）。
type ReloadStatus struct {
	// Enabled 是进程配置里的 reload.enabled，不是"监听器建没建起来"（D-R0503）。
	// 它由注入方显式给值，见 WithReloadState。
	Enabled bool `json:"enabled"`
	// WatchedPath 是监听器盯的那个文件的绝对路径。
	WatchedPath string `json:"watched_path,omitempty"`
	// LastAttemptAt / LastAppliedAt 用 RFC3339Nano 文本，与台账行的时间列同一种格式家族
	// （列本身存的是 UnixMicro 整数，精度与存储形状都不同，别把两者当成一回事）；
	// 从未发生过的时间是空串并且整个键缺省。
	LastAttemptAt string `json:"last_attempt_at,omitempty"`
	LastAppliedAt string `json:"last_applied_at,omitempty"`
	// Result 空串表示从没尝试过；其余取值由产出方（core.ReloadResult）限定，
	// 这一层原样透出，不做第二份白名单——想在这里加一道枚举校验就得同步维护两份清单。
	Result string `json:"result"`
	Error  string `json:"error,omitempty"`
	// AppliedKeys 是本次重载处理过的键——注意"处理过"不等于"生效"：executors.enabled=false
	// 时改档位，键照样在这里出现，另有一条 warn 说明这一节没打开（D-R0605）。
	// IgnoredKeys 是改了但要重启的键；RejectedKeys 是那一次导致整次作废的键，
	// 它是"本次与现网不同的全部拒绝档键"的累计清单，不是"你刚改的那一条"（D-R0705）。
	// 三份都只含键名。
	AppliedKeys  []string `json:"applied_keys,omitempty"`
	IgnoredKeys  []string `json:"ignored_keys,omitempty"`
	RejectedKeys []string `json:"rejected_keys,omitempty"`
	// WatcherError 是监听器自己的故障，与"某次重载失败"是两件事，所以单列一份。
	WatcherError string `json:"watcher_error,omitempty"`
}

// WithReloadState 注入重载状态的读口，enabled 那一位给的是进程配置里的 reload.enabled。
//
// 未注入时 RuntimeResponse.Reload 整个字段缺省（指针 + omitempty），响应里连 "reload" 这个键
// 都不出现——值类型会给出一个 {"result":""} 的空对象，那会被读成"启用过但从没重载"。
// 反过来，"启用了但监听器没建起来"的部署必须**带着**这个键出现，读数才能说清是哪一种。
//
// 两条调用方契约（这条 Option 的语义全靠它们，本层不替调用方兜）：
//   - 只在进程配置 `reload.enabled=true` 时注入。enabled 为假却注入了，响应里就会出现一个
//     `{"enabled":false,...}` 的对象，"没启用"与"启用过"两种形状就此分不开；
//   - 不许传"装着 nil 指针的读口"（例如 `WithReloadState((*reloadStatusReader)(nil), true)`）。
//     接口值非 nil 而动态值是 nil，判空会放过它，随后在 State() 里炸成 500。
func WithReloadState(r ReloadStateReader, enabled bool) Option {
	return func(s *Server) {
		s.reloadState = r
		s.reloadEnabled = enabled
	}
}

// reloadStatusOf 把一份 core.ReloadState 转成响应里的那个对象。
//
// 键清单在这里复制一份（slices.Clone）而不是直接引用：产出方交回的那三份可能与它内部那份
// 共享底层数组（R05 的 D-R0514 说的就是这件事），这一层不该把那样的数组递给编码器——
// 编码器只读不写，但"递给外面的东西不是别人的活数据"这条不需要判据来维持，复制一次就行。
func reloadStatusOf(state core.ReloadState, enabled bool) *ReloadStatus {
	return &ReloadStatus{
		Enabled:      enabled,
		WatchedPath:  state.WatchedPath,
		Result:       string(state.Result),
		Error:        state.Error,
		WatcherError: state.WatcherError,

		AppliedKeys:  slices.Clone(state.AppliedKeys),
		IgnoredKeys:  slices.Clone(state.IgnoredKeys),
		RejectedKeys: slices.Clone(state.RejectedKeys),

		// 零值时间不给键：这是 D-R0502 的唯一收口处，别改回直接序列化结构体。
		LastAttemptAt: formatReloadTime(state.LastAttemptAt),
		LastAppliedAt: formatReloadTime(state.LastAppliedAt),
	}
}

// formatReloadTime 把重载时间格式化给读数；零值交回空串，配合 omitempty 就是"整个键缺省"。
// 判"从未尝试过"用 Result 是不是空串，不用这里的时间。
func formatReloadTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.Format(time.RFC3339Nano)
}
