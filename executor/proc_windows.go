//go:build windows

package executor

import "os"

// signalOf 在 Windows 上恒返回空串。
//
// Windows 的退出状态里没有"终止信号"这个概念：进程被结束就是拿到了一个退出码
// （我们自己的 Kill 走 TerminateProcess，固定是 1），至于它是被谁结束的，
// 只能靠 ExitError 里的 TimedOut/Cancelled 标记说清楚。
// 整棵进程树的终止与更完整的状态解析归 TASK-E11；Unix 侧的实现见 proc_unix.go（TASK-E10）。
func signalOf(state *os.ProcessState) string {
	return ""
}
