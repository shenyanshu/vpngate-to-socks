package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"vpngate/internal/runner"
)

func main() {
	logger := log.New(os.Stdout, "[VPNRunner] ", log.LstdFlags)
	runtimeCtx, runtimeCancel := context.WithCancel(context.Background())
	defer runtimeCancel()

	r, err := runner.New(logger, socksListenAddr(), socksBypassCIDRs(), autoPilotConfig())
	if err != nil {
		logger.Fatalf("初始化 VPN Runner 失败：%v", err)
	}
	r.Start(runtimeCtx)

	server := &http.Server{
		Addr:              controlAddr(),
		Handler:           runner.NewAPIHandler(logger, r),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		logger.Printf("Runner 控制接口启动成功，监听地址：%s", controlAddr())
		logger.Printf("SOCKS5 监听地址：%s", r.Status().SocksListenAddr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatalf("启动 Runner HTTP 服务失败：%v", err)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	logger.Println("收到停止信号，正在关闭 Runner……")
	runtimeCancel()
	_ = r.Close()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Fatalf("关闭 Runner HTTP 服务失败：%v", err)
	}

	logger.Println("Runner 已安全退出")
}

func controlAddr() string {
	if value := os.Getenv("RUNNER_CONTROL_ADDR"); value != "" {
		return value
	}

	return ":18081"
}

func socksListenAddr() string {
	if value := os.Getenv("SOCKS_LISTEN_ADDR"); value != "" {
		return value
	}

	return "0.0.0.0:1080"
}

func socksBypassCIDRs() []string {
	raw := strings.TrimSpace(os.Getenv("SOCKS_BYPASS_CIDRS"))
	if raw == "" {
		return nil
	}

	parts := strings.Split(raw, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed == "" {
			continue
		}
		result = append(result, trimmed)
	}

	return result
}

func autoPilotConfig() runner.AutoPilotConfig {
	return runner.AutoPilotConfig{
		Enabled:                 envBool("AUTO_CONNECT", true),
		MonitorURL:              envString("MONITOR_URL", "https://www.gstatic.com/generate_204"),
		MonitorFailureThreshold: envInt("MONITOR_FAILURE_THRESHOLD", 3),
		TCPProbeAddress:         envString("TCP_PROBE_ADDRESS", ""),
		TCPProbeTimeout:         envDuration("TCP_PROBE_TIMEOUT", 3*time.Second),
		OpenVPNConnectTimeout:   envDuration("OPENVPN_CONNECT_TIMEOUT", 30*time.Second),
		MonitorInterval:         envDuration("MONITOR_INTERVAL", 20*time.Second),
		MonitorTimeout:          envDuration("MONITOR_TIMEOUT", 6*time.Second),
		FetchTimeout:            envDuration("FETCH_TIMEOUT", 30*time.Second),
		ConnectCooldown:         envDuration("CONNECT_COOLDOWN", 5*time.Second),
		StableAfter:             envDuration("MONITOR_STABLE_AFTER", 10*time.Second),
		BaseQuarantine:          envDuration("NODE_QUARANTINE", 5*time.Minute),
		BypassRouteTable:        envInt("BYPASS_ROUTE_TABLE", 100),
		BypassMark:              envInt("BYPASS_FWMARK", 1),
	}
}

func envString(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}

	return fallback
}

func envBool(key string, fallback bool) bool {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}

	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}

	return parsed
}

func envDuration(key string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}

	parsed, err := time.ParseDuration(value)
	if err != nil {
		return fallback
	}

	return parsed
}

func envInt(key string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}

	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}

	return parsed
}
