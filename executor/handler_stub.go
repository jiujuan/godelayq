package executor

import (
	"context"
	"fmt"
	"log/slog"

	"godelayq/core"
)

// StubHandler 返回一个只报告"还没实现"的处理函数。
//
// 它是装配链路的占位：先让"配置里的档位"变成"调度器里能查到的处理函数"，
// 真正的进程执行要到 TASK-E09 才落地。占位返回的错误是 not implemented，
// 而不是"没有注册处理函数"，因此验收时能区分"链路没通"与"执行器还没写"。
//
// TASK-E09 会把它换成真实实现并删除本文件，不能让占位留在注册表里。
func (p *Profile) StubHandler() core.Handler {
	key := p.HandlerKey()

	return func(ctx context.Context, job *core.Job) error {
		// 用包级 slog：main 启动时已把默认日志器换成配置文件里那一份，
		// 处理函数运行在调度器的 worker 协程里，拿不到装配期的 logger。
		slog.Info("executor stub invoked", "job_id", job.ID, "handler_key", key)
		return fmt.Errorf("executor %s: not implemented yet", key)
	}
}
