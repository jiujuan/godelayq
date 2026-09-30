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

// killGrace 是"先礼后兵"里礼的那一段：向进程组发 SIGTERM 之后，留给脚本自己收尾的时间。
//
// 取值 2 秒的理由（卡片 §3.5 要求二选一并记录结论，这里选的是"调小两个常量"那条）：
// 够一个正常脚本刷完缓冲、关掉临时文件，又不至于把关停流程拖到运维等不及；
// 另一条路（WaitDelay 取 min(2*killGrace, 剩余关闭时间)）要 Handler 知道进程还剩多少关闭预算，
// 而那份预算并不存在——core 的 Scheduler.Stop 取消在途任务后是无期限等 worker 的
// （cmd/server 的 shutdown_timeout 只管 HTTP 服务的优雅关闭），拿不到"剩余时间"就无从取 min。
const killGrace = 2 * time.Second

// processWaitDelay 是取消之后等待输出管道关闭的上限。
//
// 档位脚本 fork 出去的进程会继承 stdout/stderr 的写端：直接子进程退出了，
// os/exec 的拷贝协程还在等一个再也没人写的 EOF。没有这条上限，Handler 会一直不返回，
// 表现为"任务 timed out 了但 worker 名额没还回来"，比杀掉进程更难排查。
//
// 与 killGrace 相加是 Handler 最晚返回的时长（约 6 秒）；E19 的部署文档要把
// scheduler.shutdown_timeout 的推荐值写到 10s 以上，否则关停会先于收尾被运维打断。
const processWaitDelay = 4 * time.Second

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

	return func(ctx context.Context, job *core.Job) (err error) {
		result := NewResult(p)

		// 摘要在每条返回路径上都要落进 job.Exec：接口与事件读的就是它，
		// "没跑起来"的执行同样需要一条能解释的结论，而不是留下 nil 让人猜。
		//
		// permanent 也从这里出（TASK-E12）：调度器按返回的错误决定要不要重试，
		// 接口与事件读的是摘要，两处必须同源。各分支自己写这个布尔的话，
		// "摘要说会重试、实际不再重试"这种自相矛盾迟早会出现在某条早退路径上。
		defer func() {
			result.Meta.Permanent = summaryPermanent(err)
			// 摘要里的预览按值掩掉 secret 参数的取值（TASK-E16 §3.3 第 2 条）：
			// 这一份预览会随快照落盘、被完成与失败事件带走，并从任务详情与列表接口出去，
			// 读到它的身份远低于读完整产物的身份。产物文件保持原样，那里由 api 的档位判断守着。
			// 放在这条 defer 里而不是各分支：每条返回路径都要经过这里，早退路径也不例外。
			result.Meta.Preview = p.MaskSecretText(job.Payload, result.Meta.Preview)
			job.Exec = &result.Meta
			r.logRun(key, job, result.Meta)
		}()

		if p.Kind == KindHTTP {
			// 进程执行器只跑 script/binary：http 档位由 HTTPRunner 执行（TASK-E15）。
			// 走到这里说明调用方绕过了 Register 的按 kind 分流，属于装配错误：
			// 按永久失败返回，而不是去起一个根本不存在的进程。
			return newFailure(p, failureProfileUnavailable, 0,
				"http profiles are executed by the http executor", nil)
		}
		if r.artifacts == nil {
			return newFailure(p, failureNotAllowed, 0,
				"no artifact store is configured, execution output has nowhere to go", nil)
		}

		sub, err := ValidateSubmission(p, job.Payload)
		if err != nil {
			return newFailure(p, failureInvalidSubmission, 0, "invalid submission", err)
		}
		argv, err := p.Render(sub)
		if err != nil {
			return newFailure(p, failureInvalidSubmission, 0, "cannot build the command line", err)
		}

		timeout := p.timeoutWithin(r.cfg, sub.TimeoutValue)
		if !acquirePermit(ctx, r.permits, timeout) {
			return permitFailure(p, ctx, timeout)
		}
		defer releasePermit(r.permits)

		writer, err := r.artifacts.Open(job.ID, job.Attempts)
		if err != nil {
			// 没地方写输出就不执行：脚本跑完却拿不到结论，比不跑更糟
			return newFailure(p, failureNotAllowed, 0, "cannot create the output files", err)
		}

		runErr := r.execute(ctx, timeout, argv, sub, writer, result)

		// permanent 在这里就先落一次：产物目录里的 meta.json 与任务快照读同一份结论，
		// 而写文件发生在返回之前，等不到外层那条 defer。defer 里还会按同样的函数再算一遍，
		// 两处都出自 summaryPermanent，不存在两个地方各写各的规则。
		result.Meta.Permanent = summaryPermanent(runErr)

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
	// 平台差异只留在这里的三行里（卡片 §6/DoD 的口径）：
	// Unix 让子进程自成进程组，取消时整组结束（proc_unix.go）；
	// Windows 没有进程组，改用 taskkill /T 连树一起结束再兜底杀直接子进程（proc_windows.go）。
	cmd.SysProcAttr = sysProcAttr()
	cmd.Cancel = func() error { return killTree(cmd, killGrace) }
	cmd.WaitDelay = processWaitDelay

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
// 进程执行器与 HTTP 执行器共用这一份实现（参数是各自的 permits 通道）：
// 两条通路要表达的只有"占一个名额 / 退一个名额"，规则一分叉就会出现
// "同一档位下脚本与请求各自算一套并发"。
//
// 许可不是无限的资源：等不到就按超时失败，绝不无限等——挂在这里的协程既占着 worker 名额，
// 又让调用方等不到任何答复。TASK-E13 会把执行任务与普通任务分池，本函数的等待时长上限
// 是那时的前置事实。
func acquirePermit(ctx context.Context, permits chan struct{}, limit time.Duration) bool {
	select {
	case permits <- struct{}{}:
		return true
	default:
	}

	timer := time.NewTimer(limit)
	defer timer.Stop()

	select {
	case permits <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	case <-timer.C:
		return false
	}
}

// permitFailure 区分"等许可时被取消"与"等满超时仍没许可"。
//
// 两条路径的类别都由 newFailure 负责，重试标记因此与进程执行路径出自同一个判定函数
// （TASK-E12 §3.5：等不到许可是可重试的，被打断不是）。
func permitFailure(p *Profile, ctx context.Context, limit time.Duration) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		if errors.Is(ctxErr, context.DeadlineExceeded) {
			return newFailure(p, failureTimeout, 0, "timed out", ctxErr)
		}
		return newFailure(p, failureInterrupted, 0, "cancelled", ctxErr)
	}

	// 等不到许可也记成超时：调度器据此把它归到"超时"这一类，重试与告警口径一致。
	return newFailure(p, failurePermitWait, 0, "concurrency limit",
		fmt.Errorf("profile %q runs at most %d job(s) at a time and none finished within %v",
			p.Name, permitCapacity(p.MaxParallel), limit))
}

// releasePermit 退回一个名额。调用方必须先持有许可（acquirePermit 返回 true），
// 所以这里的接收永远不会阻塞；写成阻塞版本正是为了在违反前提时立刻暴露成死锁，
// 而不是悄悄把一个没占名额的请求算成占过。
func releasePermit(permits chan struct{}) {
	<-permits
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
