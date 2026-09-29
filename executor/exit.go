package executor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
)

// ExitError 是一次执行的失败结论。
//
// 它同时服务三个读者：调度器按 errors.Is 判超时与取消（见 Is），
// 重试判定按 Permanent 读"这次失败能不能重试"（TASK-E12），
// 事件与日志读 Error 的文本。因此文本里只有档位名、类别与退出码，
// 没有 argv、参数值与环境变量——设计文档 §7 要求秘密止步于子进程边界。
//
// 成功时不返回它：Handler 返回 nil 就是成功，退出码非 0 一律算失败。
type ExitError struct {
	// Profile 是档位名（不含 exec. 前缀），用来把错误归到正确的档位上
	Profile string

	// ExitCode 是进程退出码。
	// 没跑起来的进程没有退出码，这里保持 0：宁可留空，也不编一个 1 或 -1 让人以为程序真的这样退出过。
	ExitCode int

	// Signal 是终止进程的信号名（如 SIGKILL），正常退出为空。
	// 取值来自平台实现 signalOf：Unix 上从退出状态里解析出实际信号（proc_unix.go），
	// Windows 的退出状态里没有这个概念，那边恒返回空串（proc_windows.go）。
	Signal string

	// TimedOut 表示这次执行是被超时结束的
	TimedOut bool

	// Cancelled 表示这次执行是被取消结束的（用户 Cancel 或进程关停）
	Cancelled bool

	// Retryable 是"这次失败可以让调度器再试一次"的意思。
	//
	// 字段之所以取反向、方法叫 Permanent：结构体零值必须是"不重试"。
	// 若字段本身叫 Permanent，漏写它的构造点就默认允许重试，
	// 于是一个分类不明的失败会被反复投给同一个脚本，把副作用执行好几遍。
	Retryable bool

	// Reason 是失败的类别文本，用于起进程之前就失败的情形
	// （如 "invalid submission"、"concurrency limit"）。
	Reason string

	// Detail 是底层错误（参数校验文本、os/exec 的错误），可以为空。
	// 它的文本已经过 E08 的脱敏规则：secret 参数的值不会出现。
	Detail error
}

func (e *ExitError) Error() string {
	message := e.Reason
	switch {
	case message == "" && e.Detail != nil:
		message = e.Detail.Error()
	case message != "" && e.Detail != nil:
		message = fmt.Sprintf("%s: %v", message, e.Detail)
	case message == "":
		message = "command failed"
	}

	label := "profile"
	if e.Profile != "" {
		label = "profile " + strconv.Quote(e.Profile)
	}
	return fmt.Sprintf("%s: %s", label, message)
}

// Is 把这次的失败接到既有的上下文哨兵错误上。
//
// core/scheduler.go 的 executeJob 用 errors.Is(err, context.Canceled) 认"被打断"、
// 用 errors.Is(err, context.DeadlineExceeded) 认超时，本方法是这两条判断在执行器这一侧的落点：
// 分类正确，事件与重试计数都不必改动（设计文档 §6.3）。
// TimedOut 与 Cancelled 不会同时为真；真出现时按超时处理，因为超时的后果更具体。
func (e *ExitError) Is(target error) bool {
	switch target {
	case context.DeadlineExceeded:
		return e.TimedOut
	case context.Canceled:
		return e.Cancelled && !e.TimedOut
	default:
		return false
	}
}

// Unwrap 让 Detail 参与 errors.Is/errors.As 的链条：
// 校验失败时 E08 的哨兵错误（如 ErrWrongKind）仍能从这里的错误读出来。
func (e *ExitError) Unwrap() error { return e.Detail }

// Permanent 是 core 侧重试判定要读的接口方法（设计文档 §6.3）。
func (e *ExitError) Permanent() bool { return !e.Retryable }

// 这两条断言在编译期固定住对外承诺的形状：core 靠接口认"永久失败"，
// 方法签名一改，重试判定会静默退化成"所有失败都可重试"。
var (
	_ error                         = (*ExitError)(nil)
	_ interface{ Permanent() bool } = (*ExitError)(nil)
)

// classifyExit 决定"进程以这个退出码结束算不算可重试"。
//
// 默认不重试：非 0 退出多半是脚本自身或参数的问题，重跑只会把同样的错误再产生一遍，
// 还可能把副作用（写数据、发请求）重复执行一次。要重试哪些退出码，由档位的 retry_on_exit 显式列出。
func classifyExit(p *Profile, exitCode int) bool {
	if exitCode == 0 {
		// 0 不会走到失败分支；挡在这里是因为 retry_on_exit 里写了 0 时，
		// "成功但被判成需要重试"会无从解释。
		return false
	}
	for _, code := range p.RetryOnExit {
		if code == exitCode {
			return true
		}
	}
	return false
}

// classifyFailure 把 cmd.Run 的返回结果归到五种情形之一，填好重试标记。
//
// 判断顺序有讲究：上下文结束（超时/取消）优先于退出码——进程被我们杀掉了，
// 它退出时的状态是 killing 的结果，不是脚本自己的结论。
// state 与 ctxErr 分别来自 cmd.ProcessState 与执行上下文的 Err()，允许为 nil。
func classifyFailure(p *Profile, runErr error, state *os.ProcessState, ctxErr error) *ExitError {
	if runErr == nil {
		// 进程自己报告了成功。此刻上下文是否已结束不影响结论：
		// os/exec 在"上下文已结束但进程正常退出"时也是返回 nil 的。
		return nil
	}

	failure := &ExitError{Profile: p.Name, Detail: runErr}
	if state != nil {
		failure.ExitCode = state.ExitCode()
		failure.Signal = signalOf(state)
	}

	switch {
	case ctxErr != nil:
		// 超时按设计文档 §5.5 走重试；取消不算任务失败（调度器走中断分支，不消耗重试次数），
		// 所以它的 Retryable 留假：同时读两个标记的代码不会把取消当成一次可重试的失败。
		failure.TimedOut = errors.Is(ctxErr, context.DeadlineExceeded)
		failure.Cancelled = !failure.TimedOut
		failure.Retryable = failure.TimedOut
		failure.Reason = "cancelled"
		if failure.TimedOut {
			failure.Reason = "timed out"
		}

	case errors.Is(runErr, exec.ErrWaitDelay):
		// 进程已经结束（或被杀掉），但它派生的进程继承了输出管道的写端，
		// 拷贝协程只能靠 WaitDelay 强行关掉。
		// 退出码为 0 时也不重试：重跑会把脚本的副作用再做一遍，而问题只在"输出没收全"上。
		failure.Reason = "output stayed open past the wait limit"
		failure.Retryable = classifyExit(p, failure.ExitCode)

	case isExitStatus(runErr):
		failure.Retryable = classifyExit(p, failure.ExitCode)

	default:
		// 走到这里的是 Start 阶段的失败：程序不存在、不是可执行文件、权限不足。
		// 进程从未运行，退出码与信号都没有意义，一律留零值。
		failure.ExitCode = 0
		failure.Signal = ""
		failure.Reason = "cannot start the process"
	}

	return failure
}

// isExitStatus 判断错误是否来自"进程跑完且退出码非 0"。
func isExitStatus(runErr error) bool {
	var status *exec.ExitError
	return errors.As(runErr, &status)
}
