//go:build windows

package executor

import (
	"os"
	"os/exec"
	"syscall"
	"time"
)

// sysProcAttr 在 Windows 上返回 nil：进程组写法（Setpgid）在这里没有对应概念。
// TASK-E11 会把它换成 CreationFlags: CREATE_NEW_PROCESS_GROUP，
// 避免控制台的 Ctrl+C 同时打到服务进程自己。
func sysProcAttr() *syscall.SysProcAttr {
	return nil
}

// killTree 目前只结束直接子进程：Windows 没有可向进程组发送的终止信号，宽限期无处可用。
//
// 已知的后果（TASK-E11 处理）：脚本派生的进程会活下来，并继续持有输出管道的写端，
// 于是 Handler 要靠 cmd.WaitDelay 才返回得回来。E11 的写法是先 Kill 直接子进程，
// 再用 taskkill /PID <pid> /T /F 清理它派生的那一棵树。
func killTree(cmd *exec.Cmd, grace time.Duration) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}

// signalOf 在 Windows 上恒返回空串。
//
// Windows 的退出状态里没有"终止信号"这个概念：进程被结束就是拿到了一个退出码
// （我们自己的 Kill 走 TerminateProcess，固定是 1），至于它是被谁结束的，
// 只能靠 ExitError 里的 TimedOut/Cancelled 标记说清楚。
// Unix 侧的实现见 proc_unix.go。
func signalOf(state *os.ProcessState) string {
	return ""
}
