//go:build windows

package executor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

// Windows 侧的进程终止与它的 Unix 对应实现（proc_unix.go）不是一回事，本文件把差异全部收在这里：
//
//   - 没有进程组：`Setpgid` 在 Windows 上不存在，`CREATE_NEW_PROCESS_GROUP` 只是让子进程
//     收不到打到控制台上的 Ctrl+C，并不能"向整组发一个信号"。
//   - 没有终止信号：`signalOf` 恒为空串，进程被结束时拿到的只是一个退出码。
//   - 结束整棵树要靠系统工具 `taskkill /T`，而它是先枚举父子关系、再逐个结束，
//     **不是原子操作**：枚举之后新派生出来的进程不会被终止。
//     彻底的做法是把子进程放进 Job Object 并用 `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE`
//     让关句柄即清干净，那需要 `golang.org/x/sys/windows` 或手写 DLL 调用，
//     已按卡片 §3.3 登记成后续任务 E11a，本卡不声称已彻底解决。
//   - 以服务账户运行时，`taskkill /F` 可能因为目标进程权限更高而失败；
//     这条路径必须降级而不是把执行卡住，降级行为见 killTree。

// taskkillTimeout 是给 taskkill 自身的等待上限：它是外部工具，卡住时不能把 Handler 一起带走。
const taskkillTimeout = 3 * time.Second

// taskkillPath 是给测试留的注入点（卡片 §5.2 要构造"机器上没有 taskkill"的情形）。
// 正常部署不要改它：留空字符串会让 exec 去找当前目录下的同名文件。
var taskkillPath = "taskkill"

// sysProcAttr 让子进程自成进程组，只为了不把控制台的 Ctrl+C 引到服务进程自己身上。
// 它不承担终止语义：取消与超时走 killTree。
func sysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

// killTree 结束整棵进程树：先用 taskkill /T 连树一起清，再兜底杀直接子进程。
//
// 顺序与卡片 §3.1 相反（先 taskkill、后 Kill），理由是本机实测：taskkill 的 /T 靠父子关系枚举，
// 直接子进程一旦先死，`taskkill /PID <pid> /T /F` 只会回"没有找到进程 X"（退出码 128），
// 它派生的那一层原样活着——也就是卡片给的顺序会让本卡的主要目标失效。
// 反过来先 taskkill 时，直接子进程与它派生的进程一起被结束，随后那次 Kill
// 只是给"taskkill 缺失或失败"的降级路径兜底。
//
// grace 在 Windows 上用不到：没有可先发的温和信号，只能强杀。参数保留是为了与
// proc_unix.go 的同名函数一致，`proc.go` 的调用点因此不需要平台判断。
//
// 返回 nil 表示"这次执行可以按上下文结束来归类"（os/exec 据此把错误归到超时或取消）；
// 只有在直接子进程都没能杀掉时才返回错误，taskkill 的问题一律降级为日志。
func killTree(cmd *exec.Cmd, grace time.Duration) error {
	if cmd.Process == nil {
		return nil
	}
	pid := cmd.Process.Pid

	if err := runTaskkill(pid); err != nil {
		// 降级：树没清干净，但至少要把直接子进程结束掉，所以只记日志不返回错误
		slog.Default().Warn("executor could not clean the whole process tree",
			"pid", pid, "taskkill", taskkillPath, "error", err)
	}

	if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}

// runTaskkill 执行 taskkill /PID <pid> /T /F。
//
// 参数逐个传给 exec.Command，绝不拼成一整条命令串——与 E08 的"不经 shell"是同一条原则。
// taskkill 的输出只进 debug 日志：写进产物文件就会让执行器自己的动静混进任务输出
// （卡片 §3.2 第 2 条）。它是系统工具，中文 Windows 上输出的是本机代码页文本，
// 日志里按原样记录，不在这里做转码。
func runTaskkill(pid int) error {
	ctx, cancel := context.WithTimeout(context.Background(), taskkillTimeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, taskkillPath,
		"/PID", strconv.Itoa(pid), "/T", "/F").CombinedOutput()
	text := trimTaskkillOutput(out)

	if ctx.Err() != nil {
		return fmt.Errorf("taskkill did not finish within %v: %w", taskkillTimeout, ctx.Err())
	}
	if err != nil {
		// 找不到 taskkill（精简镜像）与权限不足都走这条：由调用方降级
		return fmt.Errorf("taskkill failed: %w (output: %s)", err, text)
	}

	slog.Default().Debug("executor taskkill finished", "pid", pid, "output", text)
	return nil
}

func trimTaskkillOutput(out []byte) string {
	const limit = 512
	text := string(out)
	if len(text) > limit {
		return text[:limit] + "...(truncated)"
	}
	return text
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
