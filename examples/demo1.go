package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"core"
)

func main() {
	// 初始化存储
	store, err := core.NewJSONFileStore("./jobs.json")
	if err != nil {
		log.Fatal(err)
	}

	// 创建调度器
	scheduler := core.NewScheduler(store, &core.ExponentialBackoffRetry{
		MaxDelay: 30 * time.Minute,
	})

	// 注册处理器
	scheduler.RegisterHandler("payment_check", handlePaymentCheck)
	scheduler.RegisterHandler("report_generate", handleReport)

	// 启动
	scheduler.Start()
	defer scheduler.Stop()

	// 示例1: 延迟10分钟的支付检查
	paymentJob := &core.Job{
		Name:       "payment_check",
		Payload:    []byte(`{"order_id": "ORD-12345", "amount": 99.99}`),
		TriggerAt:  time.Now().Add(10 * time.Minute),
		MaxRetries: 3,
		RetryDelay: 1 * time.Minute,
	}
	if err := scheduler.Schedule(paymentJob); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("Scheduled payment check at %v\n", paymentJob.TriggerAt)

	// 示例2: 每5秒执行一次的重复任务（Cron）
	cronJob := &core.Job{
		ID:         "heartbeat_001", // 固定ID确保只有一个实例
		Name:       "heartbeat",
		Payload:    []byte(`{"type": "ping"}`),
		CronExpr:   "*/5 * * * * *", // 每5秒（需robfig/cron支持秒级）
		IsRepeat:   true,
		MaxRetries: 2,
	}
	if err := scheduler.Schedule(cronJob); err != nil {
		log.Fatal(err)
	}

	// 示例3: 带取消的任务
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	timeoutJob := &core.Job{
		Name:      "long_task",
		TriggerAt: time.Now().Add(5 * time.Second),
	}
	scheduler.Schedule(timeoutJob)

	// 3秒后取消
	time.Sleep(3 * time.Second)
	scheduler.Cancel(timeoutJob.ID)

	// 恢复持久化的任务（程序重启后）
	snapshots, _ := store.LoadAll()
	for _, s := range snapshots {
		job := &core.Job{}
		job.FromSnapshot(s)
		scheduler.Schedule(job)
	}

	select {} // 保持运行
}

func handlePaymentCheck(ctx context.Context, job *core.Job) error {
	fmt.Printf("[%s] Checking payment: %s\n", time.Now().Format("15:04:05"), string(job.Payload))
	// 模拟调用支付API检查状态...
	return nil
}

func handleReport(ctx context.Context, job *core.Job) error {
	fmt.Printf("[%s] Generating report...\n", time.Now().Format("15:04:05"))
	return nil
}
