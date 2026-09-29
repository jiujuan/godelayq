//go:build !windows

package executor

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// sysProcAttr 让子进程自成进程组：它是组长，组长 PID 同时就是进程组 ID，
// 于是取消与超时可以一次结束它派生出的整棵树（TASK-E10 §3.2）。
//
// 前提是档位脚本不脱离进程组：脚本里如果用 setsid 或 nohup 另起会话，
// 那部分进程就不在这个组里，本卡的终止覆盖不到它（§9 第一条风险）。
func sysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

// signalGroup 向子进程所在的进程组发信号；负 PID 是"发给整个组"的写法。
func signalGroup(pid int, sig syscall.Signal) error {
	return syscall.Kill(-pid, sig)
}

// killTree 结束整棵进程树：先 SIGTERM 给整个组，宽限期后再 SIGKILL 一次。
//
// 关于"怎么判断进程是否还活着"（§3.3 要求写清选了哪一种）：这里不判断。
// 卡片提的两种探测方式都有问题——`cmd.ProcessState` 在 Wait 返回之前一直是 nil，
// 而 `signalGroup(pid, 0)` 会把"已退出但还没被收尸的僵尸"也读成活着，
// 那样每条正常取消路径都要多绕一次判断，还是判不准。
// 本函数的做法是：SIGTERM 拿到 ESRCH 就说明进程组已经不存在，直接结束；
// 否则固定等一个宽限期，再无条件补一次 SIGKILL（组已退出的话同样只拿到 ESRCH，忽略即可）。
// 代价是取消路径最晚多花一个宽限期，换来的是不在这里重复实现 os/exec 已经有的状态跟踪。
//
// 返回的 error 只留给"发信号本身失败且原因不是没这个进程组"（例如权限不足、PID 被复用），
// 返回 nil 时 os/exec 会把这次执行归因到上下文结束（超时或取消），
// 这正是 ExitError.TimedOut/Cancelled 需要的口径。
func killTree(cmd *exec.Cmd, grace time.Duration) error {
	if cmd.Process == nil {
		return nil
	}
	pid := cmd.Process.Pid

	err := signalGroup(pid, syscall.SIGTERM)
	switch {
	case errors.Is(err, syscall.ESRCH):
		// 进程组已经没了：没什么可终止，也不算失败
		return nil
	case err != nil:
		return err
	}

	time.Sleep(grace)

	if err := signalGroup(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

// signalNames 用平台自己的常量做键，因此每个平台写出的信号名都与它的取值一致
// （Linux 与 macOS 的 SIGUSR1/SIGCHLD/SIGSTOP 等编号不同，照数字硬编会写错名字）。
// 没列进来的信号走下面的数字兜底写法。
var signalNames = map[syscall.Signal]string{
	syscall.SIGHUP:   "SIGHUP",
	syscall.SIGINT:   "SIGINT",
	syscall.SIGQUIT:  "SIGQUIT",
	syscall.SIGILL:   "SIGILL",
	syscall.SIGTRAP:  "SIGTRAP",
	syscall.SIGABRT:  "SIGABRT",
	syscall.SIGFPE:   "SIGFPE",
	syscall.SIGKILL:  "SIGKILL",
	syscall.SIGSEGV:  "SIGSEGV",
	syscall.SIGPIPE:  "SIGPIPE",
	syscall.SIGALRM:  "SIGALRM",
	syscall.SIGTERM:  "SIGTERM",
	syscall.SIGUSR1:  "SIGUSR1",
	syscall.SIGUSR2:  "SIGUSR2",
	syscall.SIGCHLD:  "SIGCHLD",
	syscall.SIGCONT:  "SIGCONT",
	syscall.SIGSTOP:  "SIGSTOP",
	syscall.SIGTSTP:  "SIGTSTP",
	syscall.SIGWINCH: "SIGWINCH",
}

// signalOf 从退出状态里取致死信号名；正常退出（不是被信号结束的）返回空串。
//
// Windows 没有信号概念，那边的同名函数在 proc_windows.go 里恒返回空串。
func signalOf(state *os.ProcessState) string {
	status, ok := state.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() {
		return ""
	}

	sig := status.Signal()
	if name, known := signalNames[sig]; known {
		return name
	}
	// 没收录的信号给出编号：比返回空串更好解释——摘要里确实有一个信号，只是本表没名字
	return fmt.Sprintf("SIG#%d", int(sig))
}
