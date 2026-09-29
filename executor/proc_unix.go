//go:build !windows

package executor

import "os"

// signalOf 目前返回空串：本卡只保证"被杀掉这件事"能从 ExitError 的 TimedOut/Cancelled
// 标记里读出来，不解析致死信号。
//
// Unix 上退出状态里确实带信号：state.Sys() 是 *syscall.WaitStatus，可以取 Signalled()
// 与 Signal()。那条实现归 TASK-E10（同卡还接上 Setpgid 与整棵进程树的终止），
// 因为要动 syscall 只能放在平台文件里。Windows 侧的写法见 proc_windows.go。
func signalOf(state *os.ProcessState) string {
	return ""
}
