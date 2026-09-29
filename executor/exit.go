package executor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"

	"godelayq/core"
)

// failureClass 是一次失败的类别。它存在的唯一理由是把"重试有没有意义"收敛到一处判定
// （classifyExit，对应卡片 §3.5 那张表），构造点不再各写各的布尔。
type failureClass int

const (
	// failureInvalidSubmission 提交内容非法：参数越界、payload 结构错、命令行拼不出来。
	// 重跑用的还是同一份 payload，结论不会变。
	failureInvalidSubmission failureClass = iota

	// failureProfileUnavailable 档位不可用：程序找不到、档位类型与本执行器不符。
	failureProfileUnavailable

	// failureNotAllowed 环境不允许：产物文件建不出来、目录不可写、进程因权限起不来。
	// 这一类里"改好环境之后重试就有意义"的情形由运维处理，不由调度器重跑负责。
	failureNotAllowed

	// failureTimeout 执行超时。外部条件（机器忙、下游慢）居多，重跑有意义。
	failureTimeout

	// failurePermitWait 等档位并发许可等满了超时。同上，可重试。
	failurePermitWait

	// failureInterrupted 被取消或优雅关闭打断。调度器走 handleInterrupted，
	// 既不记失败也不消耗重试，所以这一类不参与重试判定。
	failureInterrupted

	// failureExitCode 进程自己以某个退出码结束（含 WaitDelay 强关管道之后拿到的退出码）。
	failureExitCode
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

// 这两条断言在编译期固定住对外承诺的形状：core 靠 core.PermanentError 认"永久失败"，
// 方法签名一改，重试判定会静默退化成"所有失败都可重试"。
var (
	_ error               = (*ExitError)(nil)
	_ core.PermanentError = (*ExitError)(nil)
)

// summaryPermanent 给出任务摘要里该写的 permanent（core.ExecMeta.Permanent）。
//
// 判定口径与调度器完全一致——读的也是 core.PermanentError，这里不重复一遍规则。
// 只多一条处理：取消与优雅关闭打断不算任务失败（调度器走 handleInterrupted，
// 既不记失败也不消耗重试），所以那种路径的摘要里不留"重试没有意义"的判断，
// 免得一条被用户自己取消的任务在接口上看起来像永久失败。
func summaryPermanent(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}

	var permanent core.PermanentError
	return errors.As(err, &permanent) && permanent.Permanent()
}

// classifyExit 是卡片 §3.5 那张表的代码形式：给定失败类别与退出码，回答"重试有没有意义"。
//
// 全部执行路径的重试标记都从这里出（TASK-E12 §3.5），各分支不再手写布尔：
// 漏写一处，"要不要重试"就取决于代码写到哪儿了，而重复执行的后果是外部副作用。
//
// 两条可重试的形：超时（含等不到并发许可）与档位显式声明过的退出码。
// 默认不重试：非 0 退出多半是脚本自身或参数的问题，重跑只会把同样的错误再产生一遍，
// 还可能把副作用（写数据、发请求）重复执行一次。要重试哪些退出码，由 retry_on_exit 显式列出。
//
// failureInterrupted 不在这张表里给出结论：取消与关停打断由调度器走 handleInterrupted，
// 根本不读重试标记（表里那行"不适用"就是这个意思），这里的 Retryable 留假，
// 免得同时读两个标记的代码把取消当成一次可重试的失败。
func classifyExit(class failureClass, exitCode int, retryOnExit []int) bool {
	switch class {
	case failureTimeout, failurePermitWait:
		return true
	case failureExitCode:
		return exitCodeIn(retryOnExit, exitCode)
	default:
		return false
	}
}

// exitCodeIn 判断退出码是否被档位显式列入可重试。
// 0 不在这里放行：那是成功，走不到失败判定；retry_on_exit 里写了 0 也无从生效。
func exitCodeIn(retryOnExit []int, exitCode int) bool {
	if exitCode == 0 {
		return false
	}
	for _, code := range retryOnExit {
		if code == exitCode {
			return true
		}
	}
	return false
}

// newFailure 按类别造一条失败结论：TimedOut/Cancelled/Retryable 三个标记由类别决定，
// 调用方只负责给类别、退出码、类别文本与底层错误。
//
// 与退出码无关的构造点（提交非法、档位不可用、超时、许可等不到、被打断）exitCode 传 0：
// 那个位置的 0 意思是"没有退出码可言"，不是"进程以 0 退出"。
// reason 为空的退出码分支不需要额外文本：退出码本身就是结论（见 ExitError.Error）。
func newFailure(p *Profile, class failureClass, exitCode int, reason string, detail error) *ExitError {
	failure := &ExitError{
		Profile:   p.Name,
		ExitCode:  exitCode,
		Reason:    reason,
		Detail:    detail,
		TimedOut:  class == failureTimeout || class == failurePermitWait,
		Cancelled: class == failureInterrupted,
	}
	failure.Retryable = classifyExit(class, exitCode, p.RetryOnExit)
	return failure
}

// classifyFailure 把 cmd.Run 的返回结果归到情形之一，并按类别填好重试标记。
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

	exitCode := 0
	signal := ""
	if state != nil {
		exitCode = state.ExitCode()
		signal = signalOf(state)
	}

	var failure *ExitError
	switch {
	case ctxErr != nil:
		if errors.Is(ctxErr, context.DeadlineExceeded) {
			failure = newFailure(p, failureTimeout, exitCode, "timed out", runErr)
		} else {
			failure = newFailure(p, failureInterrupted, exitCode, "cancelled", runErr)
		}

	case errors.Is(runErr, exec.ErrWaitDelay):
		// 进程已经结束（或被杀掉），但它派生的进程继承了输出管道的写端，
		// 拷贝协程只能靠 WaitDelay 强行关掉。退出码仍按 retry_on_exit 判：
		// 这条路径的进程结论是真实的，缺的只是尾部输出。
		failure = newFailure(p, failureExitCode, exitCode, "output stayed open past the wait limit", runErr)

	case isExitStatus(runErr):
		failure = newFailure(p, failureExitCode, exitCode, "", runErr)

	default:
		// 走到这里的是 Start 阶段的失败：程序不存在、不是可执行文件、权限不足。
		// 进程从未运行，退出码与信号都没有意义，一律留零值。
		failure = newFailure(p, failureProfileUnavailable, 0, "cannot start the process", runErr)
	}

	failure.Signal = signal
	return failure
}

// isExitStatus 判断错误是否来自"进程跑完且退出码非 0"。
func isExitStatus(runErr error) bool {
	var status *exec.ExitError
	return errors.As(runErr, &status)
}
