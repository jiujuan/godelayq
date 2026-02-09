package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"core"
	"core/api"
)

func main() {
	// 初始化存储
	store, err := core.NewJSONFileStore("./data/jobs.json")
	if err != nil {
		log.Fatal(err)
	}

	// 创建调度器
	scheduler := core.NewScheduler(store, &core.ExponentialBackoffRetry{
		MaxDelay: 30 * time.Minute,
	})

	// 创建API服务器
	server := api.NewServer(scheduler, store, "8080")

	// 注册Job处理器（业务逻辑）
	server.RegisterJobHandler("payment_check", handlePaymentCheck)
	server.RegisterJobHandler("email_send", handleEmailSend)
	server.RegisterJobHandler("data_sync", handleDataSync)
	server.RegisterJobHandler("report_generate", handleReportGenerate)

	// 启动调度器
	scheduler.Start()

	// 启动HTTP API
	if err := server.Start(); err != nil {
		log.Fatal(err)
	}

	// 优雅关闭处理
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("Shutting down server...")

	// 优雅关闭HTTP（5秒超时）
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := server.Stop(ctx); err != nil {
		log.Printf("Server forced to shutdown: %v", err)
	}

	// 停止调度器（等待正在执行的任务）
	scheduler.Stop()

	log.Println("Server exited")
}

// Job Handlers 实现

func handlePaymentCheck(ctx context.Context, job *goschedjob.Job) error {
	fmt.Printf("执行支付检查: %s\n", string(job.Payload))
	// 模拟调用支付API
	time.Sleep(2 * time.Second)
	return nil
}

func handleEmailSend(ctx context.Context, job *goschedjob.Job) error {
	fmt.Printf("发送邮件: %s\n", string(job.Payload))
	// 模拟SMTP发送
	return nil
}

func handleDataSync(ctx context.Context, job *goschedjob.Job) error {
	fmt.Printf("数据同步: %s\n", string(job.Payload))
	// 模拟数据同步
	return nil
}

func handleReportGenerate(ctx context.Context, job *goschedjob.Job) error {
	fmt.Printf("生成报表: %s\n", string(job.Payload))
	// 模拟报表生成（可能耗时较长）
	select {
	case <-time.After(10 * time.Second):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
