package executor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"time"

	"godelayq/core"
)

// outputWaitDelay 是"进程已结束（或被杀掉）但输出管道还开着"时的兜底等待时长。
//
// 档位脚本 fork 出去的进程会继承 stdout/stderr 的写端：直接子进程退出了，
// os/exec 的拷贝协程还在等一个再也没人写的 EOF。没有这条上限，Handler 会一直不返回，
// 表现为"任务 timed out 了但 worker 名额没还回来"，比杀掉进程更难排查。
// 取值要明显小于最短的合理执行超时，又不能短到在正常收尾时抢跑：5 秒是这两者之间的折中，
// 与 TASK-E10 讨论的 killGrace/优雅关闭时长的关系在那一卡一并定稿。
const outputWaitDelay = 5 * time.Second

// Runner 是一个档位的执行主体：把 payload 变成一次真实的进程执行，
// 把输出落到产物文件，把结论写进 job.Exec。
//
// 一个档位一个 Runner（Register 里逐档位构造），所以档位并发许可也是按档位各持一份。
// 注意它只限制"同一档位同时跑几个"，不解决"执行器任务把普通任务的 worker 名额占满"——
// 那是执行池隔离（TASK-E13）的范围；本卡里等待许可的过程仍然占着当前 worker。
type Runner struct {
	profile   *Profile
	artifacts *ArtifactStore
	cfg       core.ExecutorsConfig
	logger    *slog.Logger

	// programPath 是探测得到的可执行文件绝对路径，用作 argv[0]；探测失败时为空。
	// 留空就用档位里的写法（解释器名或程序名），让操作系统去报"找不到文件"——
	// 那比在本包再实现一套 PATH 搜索更诚实，错误也更接近真实原因。
	programPath string

	// permits 是档位并发许可，容量等于 profile.MaxParallel。
	// 用带缓冲通道而不是引入 golang.org/x/sync/semaphore：要表达的只有
	// "占一个名额 / 退一个名额"，通道的收发就是它，不必为这点语义加一个直接依赖。
	permits chan struct{}
}

// NewRunner 为档位构造执行器。
//
// 构造时做一次探测，取可执行文件的绝对路径。这次探测与登记表启动那次是同一个函数：
// 重复的代价是几次文件系统查询，换来的是 Runner 不依赖"调用方必须先探过"这个隐含前提——
// 自行接入本包的程序拿到一个 Profile 也能直接构造出可执行的 Runner。
//
// cfg 传 core.Config.Normalized() 之后的 Executors 一节：字段为零值时本包仍会补默认值，
// 但不重复做整份配置的归一化。
func NewRunner(p *Profile, a *ArtifactStore, cfg core.ExecutorsConfig, l *slog.Logger) *Runner {
	if l == nil {
		l = slog.Default()
	}

	runner := &Runner{
		profile:   p,
		artifacts: a,
		cfg:       cfg,
		logger:    l,
		permits:   make(chan struct{}, permitCapacity(p.MaxParallel)),
	}
	if probe := Probe(p); probe.Available {
		runner.programPath = probe.Path
	}
	return runner
}

// permitCapacity 把档位声明的并发数抬到至少 1：
// 0 或负值会让信号量要么永远拿不到、要么形同不存在，两种都比"串行执行"更难解释。
func permitCapacity(maxParallel int) int {
	if maxParallel < 1 {
		return 1
	}
	return maxParallel
}

// Handler 返回交给调度器的处理函数。
//
// 流程固定为：校验 payload → 拼 argv → 求生效超时 → 取档位许可 → 建产物文件 →
// 起进程等退出 → 关产物文件并把结论写进 job.Exec → 返回错误。
// 前半段任何一步失败都不起进程，也不留产物文件：那时连命令行长什么样都还没定下来。
//
// 日志规范（设计文档 §7）：只记 job_id、handler_key、档位名、退出码、耗时、是否截断。
// 不记 payload、不记 argv、不记 env——示例处理函数里那句 slog.Info(..., "payload", ...)
// 在普通任务上没问题，在执行器任务上等于把口令类参数写进日志文件。
func (r *Runner) Handler() core.Handler {
	p := r.profile
	key := p.HandlerKey()

	return func(ctx context.Context, job *core.Job) error {
		result := NewResult(p)

		// 摘要在每条返回路径上都要落进 job.Exec：接口与事件读的就是它，
		// "没跑起来"的执行同样需要一条能解释的结论，而不是留下 nil 让人猜。
		defer func() {
			job.Exec = &result.Meta
			r.logRun(key, job, result.Meta)
		}()

		if p.Kind == KindHTTP {
			return &ExitError{Profile: p.Name,
				Reason: "http profiles are executed by the http executor, which lands in TASK-E15"}
		}
		if r.artifacts == nil {
			return &ExitError{Profile: p.Name,
				Reason: "no artifact store is configured, execution output has nowhere to go"}
		}

		sub, err := ValidateSubmission(p, job.Payload)
		if err != nil {
			return &ExitError{Profile: p.Name, Reason: "invalid submission", Detail: err}
		}
		argv, err := p.Render(sub)
		if err != nil {
			return &ExitError{Profile: p.Name, Reason: "cannot build the command line", Detail: err}
		}

		timeout := p.timeoutWithin(r.cfg, sub.TimeoutValue)
		if !r.acquirePermit(ctx, timeout) {
			return r.permitFailure(ctx, timeout)
		}
		defer r.releasePermit()

		writer, err := r.artifacts.Open(job.ID, job.Attempts)
		if err != nil {
			// 没地方写输出就不执行：脚本跑完却拿不到结论，比不跑更糟
			return &ExitError{Profile: p.Name, Reason: "cannot create the output files", Detail: err}
		}

		runErr := r.execute(ctx, timeout, argv, sub, writer, result)

		// 先关文件再读尾部：io.Writer 形态的输出由 os/exec 的拷贝协程写入，
		// cmd.Run 返回时它们已经等过（WaitDelay 兜底），关闭即把缓冲刷到磁盘。
		info, closeErr := writer.Close()
		result.Meta.OutBytes = info.OutBytes
		result.Meta.ErrBytes = info.ErrBytes
		result.Meta.Truncated = info.Truncated
		result.Meta.Artifact = core.ArtifactAvailable
		if closeErr != nil {
			r.logger.Warn("executor output files did not close cleanly",
				"job_id", job.ID, "handler_key", key, "error", closeErr)
		}

		r.fillPreview(result, job.ID, job.Attempts)
		if metaErr := writer.WriteMeta(result.Meta); metaErr != nil {
			// 结论已经写进任务快照，meta.json 只是产物目录里的自述文件：写失败不值得改写执行结果
			r.logger.Warn("executor artifact metadata was not written",
				"job_id", job.ID, "handler_key", key, "error", metaErr)
		}
		return runErr
	}
}

// execute 起进程、等退出，并把退出结论写进 result.Meta，返回分类后的失败（成功为 nil）。
func (r *Runner) execute(ctx context.Context, timeout time.Duration, argv []string,
	sub *Submission, writer *ArtifactWriter, result *Result) error {

	p := r.profile

	// 自己再套一层超时：调度器按 job.Timeout 建的上下文可能比档位算出的生效超时更长
	// （提交期没把有效值写进任务，或任务由恢复路径带回来）。绝不出现"无限等待"。
	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// argv[0] 换成探测到的绝对路径：PATH 搜索是操作系统的事，但把它交给操作系统再做一次
	// 意味着子进程可能命中另一个同名程序。探测结论是启动时审查过的那一条，优先用它。
	if r.programPath != "" {
		argv[0] = r.programPath
	}

	cmd := exec.CommandContext(execCtx, argv[0], argv[1:]...)
	// CommandContext 用 name 和 args 拼出 cmd.Args；显式赋回 Render 的结果，
	// 让"执行的就是 Render 给出的那串 argv"这条事实留在代码里。
	cmd.Args = argv
	cmd.Dir = p.CwdPath
	cmd.Env = BuildEnv(r.cfg, p, sub)
	cmd.Stdout = writer.Stdout()
	cmd.Stderr = writer.Stderr()
	// 取消时只结束直接子进程；整棵进程树的终止见 TASK-E10（Unix）与 TASK-E11（Windows）。
	cmd.Cancel = func() error { return cmd.Process.Kill() }
	cmd.WaitDelay = outputWaitDelay

	started := time.Now()
	runErr := cmd.Run()
	result.Meta.DurationMs = time.Since(started).Milliseconds()

	if state := cmd.ProcessState; state != nil {
		result.Meta.ExitCode = state.ExitCode()
		result.Meta.Signal = signalOf(state)
	}

	failure := classifyFailure(p, runErr, cmd.ProcessState, execCtx.Err())
	if failure == nil {
		// 显式返回 nil：把类型化的空指针交给 error 返回值，调用方看到的就"是一个错误"，
		// 一次成功的执行会被记成失败。
		return nil
	}
	result.Meta.Permanent = failure.Permanent()
	return failure
}

// fillPreview 从产物文件取输出尾部填进摘要。
//
// 为什么读文件而不是留一份内存缓冲：输出已经完整落盘，尾部直接从文件反向读
// inline_preview 字节即可，执行侧不需要再持有一份副本（大输出时那是几十 MB）。
// 取 stderr 优先，见 ChoosePreview。
func (r *Runner) fillPreview(result *Result, jobID string, attempt int) {
	limit := r.previewLimit()
	if limit <= 0 {
		result.Meta.Preview = ""
		return
	}

	stdout, _, stdoutErr := r.artifacts.Tail(jobID, attempt, "out", int64(limit))
	stderr, _, stderrErr := r.artifacts.Tail(jobID, attempt, "err", int64(limit))
	if stdoutErr != nil && stderrErr != nil {
		// 预览是锦上添花：读不到就不带，执行结论照常返回
		return
	}
	result.Meta.Preview = ChoosePreview(stdout, stderr, limit)
}

// previewLimit 返回事件与摘要里预览的字节上限（executors.output.inline_preview）。
func (r *Runner) previewLimit() int {
	if r.cfg.Output.InlinePreview <= 0 {
		return core.DefaultExecInlinePreview
	}
	return r.cfg.Output.InlinePreview
}

// acquirePermit 取档位并发许可，等待上限是本次执行的生效超时。
//
// 许可不是无限的资源：等不到就按超时失败，绝不无限等——挂在这里的协程既占着 worker 名额，
// 又让调用方等不到任何答复。TASK-E13 会把执行任务与普通任务分池，本卡的等待时长上限
// 是那时的前置事实。
func (r *Runner) acquirePermit(ctx context.Context, limit time.Duration) bool {
	select {
	case r.permits <- struct{}{}:
		return true
	default:
	}

	timer := time.NewTimer(limit)
	defer timer.Stop()

	select {
	case r.permits <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	case <-timer.C:
		return false
	}
}

// permitFailure 区分"等许可时被取消"与"等满超时仍没许可"。
func (r *Runner) permitFailure(ctx context.Context, limit time.Duration) error {
	p := r.profile
	if ctxErr := ctx.Err(); ctxErr != nil {
		timedOut := errors.Is(ctxErr, context.DeadlineExceeded)
		return &ExitError{
			Profile:   p.Name,
			Reason:    "cancelled",
			Cancelled: !timedOut,
			TimedOut:  timedOut,
			Retryable: timedOut,
			Detail:    ctxErr,
		}
	}

	// 等不到许可也记成超时：调度器据此把它归到"超时"这一类，重试与告警口径一致。
	return &ExitError{
		Profile:   p.Name,
		Reason:    "concurrency limit",
		TimedOut:  true,
		Retryable: true,
		Detail: fmt.Errorf("profile %q runs at most %d job(s) at a time and none finished within %v",
			p.Name, p.MaxParallel, limit),
	}
}

func (r *Runner) releasePermit() {
	<-r.permits
}

// logRun 记一次执行的收尾日志。字段集合就是设计文档 §7 允许的那些：
// 档位名可以外泄，参数值不可以。
func (r *Runner) logRun(key string, job *core.Job, meta core.ExecMeta) {
	r.logger.Info("executor run finished",
		"job_id", job.ID,
		"handler_key", key,
		"profile", meta.Profile,
		"kind", meta.Kind,
		"exit_code", meta.ExitCode,
		"duration_ms", meta.DurationMs,
		"truncated", meta.Truncated)
}
