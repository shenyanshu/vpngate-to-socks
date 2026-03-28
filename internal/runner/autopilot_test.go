package runner

import (
	"bytes"
	"context"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"vpngate/internal/vpngate"
)

func TestAutoPilotConfigWithDefaults(t *testing.T) {
	cfg := (AutoPilotConfig{}).withDefaults()

	if cfg.MonitorURL != "https://www.gstatic.com/generate_204" {
		t.Fatalf("MonitorURL = %q, want %q", cfg.MonitorURL, "https://www.gstatic.com/generate_204")
	}
	if cfg.MonitorFailureThreshold != defaultMonitorFailureThreshold {
		t.Fatalf("MonitorFailureThreshold = %d, want %d", cfg.MonitorFailureThreshold, defaultMonitorFailureThreshold)
	}
	if cfg.TCPProbeAddress != "" {
		t.Fatalf("TCPProbeAddress = %q, want empty string", cfg.TCPProbeAddress)
	}
	if cfg.TCPProbeTimeout != 3*time.Second {
		t.Fatalf("TCPProbeTimeout = %s, want %s", cfg.TCPProbeTimeout, 3*time.Second)
	}
	if cfg.OpenVPNConnectTimeout != 30*time.Second {
		t.Fatalf("OpenVPNConnectTimeout = %s, want %s", cfg.OpenVPNConnectTimeout, 30*time.Second)
	}
	if cfg.MonitorInterval != 20*time.Second {
		t.Fatalf("MonitorInterval = %s, want %s", cfg.MonitorInterval, 20*time.Second)
	}
	if cfg.MonitorTimeout != 6*time.Second {
		t.Fatalf("MonitorTimeout = %s, want %s", cfg.MonitorTimeout, 6*time.Second)
	}
	if cfg.ConnectCooldown != 5*time.Second {
		t.Fatalf("ConnectCooldown = %s, want %s", cfg.ConnectCooldown, 5*time.Second)
	}
	if cfg.StableAfter != 10*time.Second {
		t.Fatalf("StableAfter = %s, want %s", cfg.StableAfter, 10*time.Second)
	}
}

func TestRunTCPPrecheck(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	defer listener.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, acceptErr := listener.Accept()
		if acceptErr == nil {
			_ = conn.Close()
		}
	}()

	r := &Runner{autoConfig: AutoPilotConfig{TCPProbeAddress: listener.Addr().String(), TCPProbeTimeout: time.Second}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if err := r.runTCPPrecheck(ctx); err != nil {
		t.Fatalf("runTCPPrecheck() error = %v", err)
	}

	<-done
}

func TestNeedsFullMonitorConfirm(t *testing.T) {
	tests := []struct {
		name               string
		lastMonitorConfirm time.Time
		want               bool
	}{
		{name: "never confirmed", lastMonitorConfirm: time.Time{}, want: true},
		{name: "recent confirm", lastMonitorConfirm: time.Now().Add(-10 * time.Second), want: false},
		{name: "expired confirm", lastMonitorConfirm: time.Now().Add(-(fullMonitorConfirmTTL + time.Second)), want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &Runner{lastMonitorConfirm: tt.lastMonitorConfirm}
			if got := r.needsFullMonitorConfirm(); got != tt.want {
				t.Fatalf("needsFullMonitorConfirm() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSelectCandidatePrefersLowestUsersThenLowestUptimeThenLowestSessions(t *testing.T) {
	now := time.Now()
	r := &Runner{
		quarantine: map[string]nodeHealth{
			nodeKey("blocked", "1.1.1.1"): {QuarantinedUntil: now.Add(time.Minute)},
		},
	}

	servers := []vpngate.Server{
		{HostName: "blocked", IP: "1.1.1.1", TotalUsers: 1, Uptime: 1, NumVPNSessions: 1, OpenVPNConfigDataBase64: "blocked"},
		{HostName: "no-config", IP: "2.2.2.2", TotalUsers: 1, Uptime: 1, NumVPNSessions: 1},
		{HostName: "zero-users", IP: "3.3.3.3", TotalUsers: 0, Uptime: 1, NumVPNSessions: 1, OpenVPNConfigDataBase64: "cfg0"},
		{HostName: "higher-users", IP: "4.4.4.4", TotalUsers: 20, Uptime: 1, NumVPNSessions: 1, OpenVPNConfigDataBase64: "cfg1"},
		{HostName: "winner", IP: "5.5.5.5", TotalUsers: 5, Uptime: 3, NumVPNSessions: 2, OpenVPNConfigDataBase64: "cfg2"},
		{HostName: "same-users-higher-uptime", IP: "6.6.6.6", TotalUsers: 5, Uptime: 9, NumVPNSessions: 1, OpenVPNConfigDataBase64: "cfg3"},
		{HostName: "same-users-same-uptime-more-sessions", IP: "7.7.7.7", TotalUsers: 5, Uptime: 3, NumVPNSessions: 8, OpenVPNConfigDataBase64: "cfg4"},
	}

	server, err := r.selectCandidate(servers)
	if err != nil {
		t.Fatalf("selectCandidate() error = %v", err)
	}

	if server.HostName != "winner" {
		t.Fatalf("selectCandidate() host = %q, want %q", server.HostName, "winner")
	}
}

func TestRunMonitorCheckRequiresConsecutiveFailuresBeforeSwitch(t *testing.T) {
	r := &Runner{
		autoConfig: AutoPilotConfig{MonitorURL: "https://www.gstatic.com/generate_204", MonitorFailureThreshold: 3},
		state:      StateConnected,
		current:    &ConnectionInfo{HostName: "vpn", IP: "1.1.1.1"},
		quarantine: make(map[string]nodeHealth),
	}

	if failures := r.incrementMonitorFailureCount(); failures != 1 {
		t.Fatalf("incrementMonitorFailureCount() = %d, want %d", failures, 1)
	}
	if failures := r.incrementMonitorFailureCount(); failures != 2 {
		t.Fatalf("incrementMonitorFailureCount() = %d, want %d", failures, 2)
	}

	r.resetMonitorFailureCount()
	if r.monitorFailureCount != 0 {
		t.Fatalf("monitorFailureCount = %d, want 0", r.monitorFailureCount)
	}
}

func TestProbeMonitorViaSOCKS(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()

	var logBuffer bytes.Buffer
	logger := log.New(&logBuffer, "", 0)

	socks, err := newSOCKSServer(logger, "127.0.0.1:0", func() bool { return true })
	if err != nil {
		t.Fatalf("newSOCKSServer() error = %v", err)
	}
	defer socks.Close()

	r := &Runner{
		autoConfig: AutoPilotConfig{MonitorURL: target.URL, MonitorTimeout: 2 * time.Second},
		socks:      socks,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := r.probeMonitor(ctx, false); err != nil {
		t.Fatalf("probeMonitor() error = %v", err)
	}

	if !strings.Contains(logBuffer.String(), "SOCKS5 收到连接") {
		t.Fatalf("expected SOCKS server to receive a connection, logs = %q", logBuffer.String())
	}
}

func TestSOCKSServerDialAddrUsesLoopbackForWildcardListener(t *testing.T) {
	socks, err := newSOCKSServer(log.New(&bytes.Buffer{}, "", 0), "0.0.0.0:0", func() bool { return true })
	if err != nil {
		t.Fatalf("newSOCKSServer() error = %v", err)
	}
	defer socks.Close()

	dialAddr := socks.DialAddr()
	if strings.HasPrefix(dialAddr, "0.0.0.0:") {
		t.Fatalf("DialAddr() = %q, should use loopback host", dialAddr)
	}
	if !strings.HasPrefix(dialAddr, "127.0.0.1:") {
		t.Fatalf("DialAddr() = %q, want loopback IPv4 address", dialAddr)
	}
}
