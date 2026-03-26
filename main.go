package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"vpngate/internal/runnerclient"
	"vpngate/internal/web"
)

func main() {
	logger := log.New(os.Stdout, "[VPNGate] ", log.LstdFlags)
	port := serverPort()
	runnerClient := runnerclient.New(runnerAPIURL(), nil)

	app, err := web.NewApp(logger, nil, runnerClient)
	if err != nil {
		logger.Fatalf("初始化页面服务失败：%v", err)
	}

	startupCtx, startupCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer startupCancel()

	if err := app.Refresh(startupCtx); err != nil {
		logger.Printf("启动时首次刷新失败，服务仍会继续启动：%v", err)
	}

	server := &http.Server{
		Addr:              ":" + port,
		Handler:           app.Routes(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		logger.Printf("服务启动成功，监听端口：%s", port)
		logger.Printf("请在浏览器中访问：http://127.0.0.1:%s 或 http://<宿主机IP>:8082", port)

		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatalf("启动 HTTP 服务失败：%v", err)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	logger.Println("收到停止信号，正在关闭服务……")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Fatalf("关闭服务失败：%v", err)
	}

	logger.Println("服务已安全退出")
}

func serverPort() string {
	if port := os.Getenv("PORT"); port != "" {
		return port
	}

	return "8080"
}

func runnerAPIURL() string {
	if value := os.Getenv("RUNNER_API_URL"); value != "" {
		return value
	}

	return "http://127.0.0.1:18081"
}
