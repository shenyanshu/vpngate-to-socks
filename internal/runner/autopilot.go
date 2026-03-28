package runner

import (
	"context"
	"crypto/tls"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"

	"vpngate/internal/vpngate"
)

const (
	autoLoopInterval               = 5 * time.Second
	maxQuarantineBackoff           = 4
	linuxSocketMarkOption          = 36
	fullMonitorConfirmTTL          = time.Minute
	defaultMonitorFailureThreshold = 3
)

type AutoPilotConfig struct {
	Enabled                 bool
	MonitorURL              string
	MonitorFailureThreshold int
	TCPProbeAddress         string
	TCPProbeTimeout         time.Duration
	OpenVPNConnectTimeout   time.Duration
	MonitorInterval         time.Duration
	MonitorTimeout          time.Duration
	FetchTimeout            time.Duration
	ConnectCooldown         time.Duration
	StableAfter             time.Duration
	BaseQuarantine          time.Duration
	BypassRouteTable        int
	BypassMark              int
}

type nodeHealth struct {
	FailureCount     int
	QuarantinedUntil time.Time
	LastReason       string
}

func (c AutoPilotConfig) withDefaults() AutoPilotConfig {
	if strings.TrimSpace(c.MonitorURL) == "" {
		c.MonitorURL = "https://www.gstatic.com/generate_204"
	}
	if c.MonitorFailureThreshold <= 0 {
		c.MonitorFailureThreshold = defaultMonitorFailureThreshold
	}
	if c.TCPProbeTimeout <= 0 {
		c.TCPProbeTimeout = 3 * time.Second
	}
	if c.OpenVPNConnectTimeout <= 0 {
		c.OpenVPNConnectTimeout = 30 * time.Second
	}
	if c.MonitorInterval <= 0 {
		c.MonitorInterval = 20 * time.Second
	}
	if c.MonitorTimeout <= 0 {
		c.MonitorTimeout = 6 * time.Second
	}
	if c.FetchTimeout <= 0 {
		c.FetchTimeout = 30 * time.Second
	}
	if c.ConnectCooldown <= 0 {
		c.ConnectCooldown = 5 * time.Second
	}
	if c.StableAfter <= 0 {
		c.StableAfter = 10 * time.Second
	}
	if c.BaseQuarantine <= 0 {
		c.BaseQuarantine = 5 * time.Minute
	}
	if c.BypassRouteTable <= 0 {
		c.BypassRouteTable = 100
	}
	if c.BypassMark <= 0 {
		c.BypassMark = 1
	}

	return c
}

func (r *Runner) autoLoop(ctx context.Context) {
	ticker := time.NewTicker(autoLoopInterval)
	defer ticker.Stop()

	r.runAutoStep(ctx)

	for {
		select {
		case <-ctx.Done():
			r.stopMonitorLoop()
			return
		case <-ticker.C:
			r.runAutoStep(ctx)
		}
	}
}

func (r *Runner) runAutoStep(ctx context.Context) {
	r.mu.RLock()
	if r.autoPaused || !r.autoConfig.Enabled || r.testing || r.proc != nil || r.state == StateConnecting || r.state == StateConnected || r.state == StateDisconnecting {
		r.mu.RUnlock()
		return
	}

	if !r.lastAutoAttempt.IsZero() && time.Since(r.lastAutoAttempt) < r.autoConfig.ConnectCooldown {
		r.mu.RUnlock()
		return
	}
	r.mu.RUnlock()

	fetchCtx, cancel := context.WithTimeout(ctx, r.autoConfig.FetchTimeout)
	defer cancel()

	servers, err := vpngate.FetchIPhoneServers(fetchCtx, r.httpClient)
	if err != nil {
		r.setLastError(fmt.Sprintf("自动拉取节点列表失败: %v", err))
		r.logger.Printf("Runner 自动拉取节点列表失败：%v", err)
		return
	}
	r.logger.Printf("Runner 自动拉取节点列表成功：共 %d 个节点", len(servers))

	server, err := r.selectCandidate(servers)
	if err != nil {
		r.setLastError(err.Error())
		r.logger.Printf("Runner 自动选点失败：%v", err)
		return
	}

	r.mu.Lock()
	r.lastAutoAttempt = time.Now()
	r.mu.Unlock()

	r.logger.Printf("Runner 自动选择节点：%s（%s），总用户数 %d", server.HostName, server.IP, server.TotalUsers)
	if err := r.Connect(server); err != nil {
		r.logger.Printf("Runner 自动连接失败：%v", err)
	}
}

func (r *Runner) selectCandidate(servers []vpngate.Server) (vpngate.Server, error) {
	now := time.Now()
	candidates := make([]vpngate.Server, 0, len(servers))

	r.mu.RLock()
	quarantine := make(map[string]nodeHealth, len(r.quarantine))
	maps.Copy(quarantine, r.quarantine)
	r.mu.RUnlock()

	for _, server := range servers {
		if !vpngate.IsRecommendedServer(server) {
			continue
		}

		health, blocked := quarantine[nodeKey(server.HostName, server.IP)]
		if blocked && health.QuarantinedUntil.After(now) {
			continue
		}

		candidates = append(candidates, server)
	}

	if len(candidates) == 0 {
		return vpngate.Server{}, fmt.Errorf("没有可用于自动连接的 VPN 节点")
	}

	vpngate.SortServersByRecommendation(candidates)

	return candidates[0], nil
}

func (r *Runner) prepareMonitorTargets() error {
	if !r.autoConfig.Enabled {
		return nil
	}

	parsedURL, err := url.Parse(r.autoConfig.MonitorURL)
	if err != nil {
		return fmt.Errorf("解析监控 URL 失败: %w", err)
	}

	host := parsedURL.Hostname()
	if strings.TrimSpace(host) == "" {
		return fmt.Errorf("监控 URL 缺少主机名")
	}

	ctx, cancel := context.WithTimeout(context.Background(), r.autoConfig.MonitorTimeout)
	defer cancel()

	addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return fmt.Errorf("解析监控主机失败: %w", err)
	}

	ips := make([]string, 0, len(addresses))
	seen := make(map[string]struct{}, len(addresses))
	for _, address := range addresses {
		ip := strings.TrimSpace(address.IP.String())
		if ip == "" {
			continue
		}
		if _, ok := seen[ip]; ok {
			continue
		}
		seen[ip] = struct{}{}
		ips = append(ips, ip)
	}

	if len(ips) == 0 {
		return fmt.Errorf("未解析到可用的监控目标 IP")
	}

	r.mu.Lock()
	r.monitorIPs = ips
	r.mu.Unlock()
	return nil
}

func (r *Runner) startMonitorLoop() {
	if !r.autoConfig.Enabled {
		return
	}

	r.mu.Lock()
	if r.monitorCancel != nil {
		r.monitorCancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.monitorCancel = cancel
	r.mu.Unlock()

	go r.monitorLoop(ctx)
}

func (r *Runner) stopMonitorLoop() {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.monitorCancel != nil {
		r.monitorCancel()
		r.monitorCancel = nil
	}
}

func (r *Runner) monitorLoop(ctx context.Context) {
	if r.autoConfig.StableAfter > 0 {
		select {
		case <-ctx.Done():
			return
		case <-time.After(r.autoConfig.StableAfter):
		}
	}

	r.runMonitorCheck(ctx)
	if ctx.Err() != nil {
		return
	}

	ticker := time.NewTicker(r.autoConfig.MonitorInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.runMonitorCheck(ctx)
		}
	}
}

func (r *Runner) runMonitorCheck(parent context.Context) {
	if !r.canProxy() {
		return
	}

	if strings.TrimSpace(r.autoConfig.TCPProbeAddress) != "" {
		err := r.runTCPPrecheck(parent)
		if err == nil && !r.needsFullMonitorConfirm() {
			return
		}
		if err != nil {
			r.logger.Printf("Runner VPN TCP 快检失败：%v", err)
		}
	}

	vpnErr, bypassErr := r.runConcurrentMonitorConfirm(parent)
	if vpnErr == nil {
		r.resetMonitorFailureCount()
		r.markFullMonitorConfirm()
		return
	}

	r.logger.Printf("Runner SOCKS 路径探活失败：%v", vpnErr)
	if bypassErr == nil {
		current := r.currentSnapshot()
		if current != nil {
			failures := r.incrementMonitorFailureCount()
			threshold := r.autoConfig.MonitorFailureThreshold
			if failures < threshold {
				r.logger.Printf("Runner 将继续观察当前节点：%s（%s），已连续 SOCKS 探活失败 %d/%d", current.HostName, current.IP, failures, threshold)
				return
			}

			r.resetMonitorFailureCount()
			reason := fmt.Sprintf("SOCKS 探活失败，但直连站点 %s 成功", r.autoConfig.MonitorURL)
			r.logger.Printf("Runner 判定节点失效：%s（%s），原因：%s", current.HostName, current.IP, reason)
			r.markQuarantine(*current, reason)
			if err := r.disconnectForRecovery(); err != nil {
				r.logger.Printf("Runner 自动断开失效节点失败：%v", err)
			}
		}
		return
	}

	r.resetMonitorFailureCount()
	r.logger.Printf("Runner 直连复核也失败，暂不切换节点：%v", bypassErr)
}

func (r *Runner) incrementMonitorFailureCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.monitorFailureCount++
	return r.monitorFailureCount
}

func (r *Runner) resetMonitorFailureCount() {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.monitorFailureCount = 0
}

func (r *Runner) needsFullMonitorConfirm() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return r.lastMonitorConfirm.IsZero() || time.Since(r.lastMonitorConfirm) >= fullMonitorConfirmTTL
}

func (r *Runner) markFullMonitorConfirm() {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.lastMonitorConfirm = time.Now()
}

func (r *Runner) runTCPPrecheck(parent context.Context) error {
	if strings.TrimSpace(r.autoConfig.TCPProbeAddress) == "" {
		return fmt.Errorf("未配置 TCP 快检目标")
	}

	ctx, cancel := context.WithTimeout(parent, r.autoConfig.TCPProbeTimeout)
	defer cancel()

	return r.probeTCP(ctx, false)
}

func (r *Runner) runConcurrentMonitorConfirm(parent context.Context) (error, error) {
	vpnCtx, vpnCancel := context.WithTimeout(parent, r.autoConfig.MonitorTimeout)
	bypassCtx, bypassCancel := context.WithTimeout(parent, r.autoConfig.MonitorTimeout)
	defer vpnCancel()
	defer bypassCancel()

	vpnCh := make(chan error, 1)
	bypassCh := make(chan error, 1)

	go func() {
		vpnCh <- r.probeMonitor(vpnCtx, false)
	}()
	go func() {
		bypassCh <- r.probeMonitor(bypassCtx, true)
	}()

	var vpnErr error
	var bypassErr error
	for vpnCh != nil || bypassCh != nil {
		select {
		case err := <-vpnCh:
			vpnErr = err
			vpnCh = nil
			if err == nil {
				bypassCancel()
				return nil, nil
			}
		case err := <-bypassCh:
			bypassErr = err
			bypassCh = nil
		}
	}

	return vpnErr, bypassErr
}

func (r *Runner) probeTCP(ctx context.Context, bypass bool) error {
	address := strings.TrimSpace(r.autoConfig.TCPProbeAddress)
	if address == "" {
		return fmt.Errorf("TCP 快检目标为空")
	}

	var dialContext func(context.Context, string, string) (net.Conn, error)
	if bypass {
		dialContext = newMarkedDialContext(r.autoConfig.TCPProbeTimeout, r.autoConfig.BypassMark)
	} else {
		dialContext = (&net.Dialer{Timeout: r.autoConfig.TCPProbeTimeout}).DialContext
	}

	conn, err := dialContext(ctx, "tcp", address)
	if err != nil {
		return err
	}
	defer conn.Close()

	return nil
}

func (r *Runner) probeMonitor(ctx context.Context, bypass bool) error {
	parsedURL, err := url.Parse(r.autoConfig.MonitorURL)
	if err != nil {
		return err
	}

	if bypass {
		return r.probeMonitorBypass(ctx, parsedURL)
	}

	return r.probeMonitorViaSOCKS(ctx, parsedURL)
}

func (r *Runner) probeMonitorViaSOCKS(ctx context.Context, parsedURL *url.URL) error {
	if r.socks == nil {
		return fmt.Errorf("SOCKS5 代理未启动")
	}

	proxyAddr := strings.TrimSpace(r.socks.DialAddr())
	if proxyAddr == "" {
		return fmt.Errorf("SOCKS5 代理地址为空")
	}

	transport := &http.Transport{
		Proxy:               http.ProxyURL(&url.URL{Scheme: "socks5", Host: proxyAddr}),
		DisableKeepAlives:   true,
		ForceAttemptHTTP2:   false,
		TLSHandshakeTimeout: r.autoConfig.MonitorTimeout,
	}
	client := &http.Client{Timeout: r.autoConfig.MonitorTimeout, Transport: transport}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsedURL.String(), nil)
	if err != nil {
		return err
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("通过 SOCKS 探活失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("SOCKS 探活网站返回异常状态: %s", resp.Status)
	}

	return nil
}

func (r *Runner) probeMonitorBypass(ctx context.Context, parsedURL *url.URL) error {
	r.mu.RLock()
	ips := append([]string(nil), r.monitorIPs...)
	r.mu.RUnlock()

	if len(ips) == 0 {
		return fmt.Errorf("未准备好直连探活目标 IP")
	}

	hostHeader := parsedURL.Host
	serverName := parsedURL.Hostname()
	port := parsedURL.Port()
	if port == "" {
		if parsedURL.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}

	transport := &http.Transport{
		Proxy:               nil,
		DisableKeepAlives:   true,
		ForceAttemptHTTP2:   false,
		TLSHandshakeTimeout: r.autoConfig.MonitorTimeout,
		DialContext:         newMarkedDialContext(r.autoConfig.MonitorTimeout, r.autoConfig.BypassMark),
		TLSClientConfig:     &tls.Config{ServerName: serverName},
	}
	client := &http.Client{Timeout: r.autoConfig.MonitorTimeout, Transport: transport}

	var lastErr error
	for _, ip := range ips {
		targetURL := *parsedURL
		targetURL.Host = net.JoinHostPort(ip, port)

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL.String(), nil)
		if err != nil {
			lastErr = err
			continue
		}
		req.Host = hostHeader

		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}

		resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil
		}

		lastErr = fmt.Errorf("直连探活返回异常状态: %s", resp.Status)
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("直连探活失败")
	}

	return lastErr
}

func newMarkedDialContext(timeout time.Duration, mark int) func(context.Context, string, string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: timeout}
	if mark <= 0 {
		return dialer.DialContext
	}

	dialer.Control = func(network, address string, c syscall.RawConn) error {
		var controlErr error
		err := c.Control(func(fd uintptr) {
			controlErr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, linuxSocketMarkOption, mark)
		})
		if err != nil {
			return err
		}

		return controlErr
	}

	return dialer.DialContext
}

func (r *Runner) markQuarantine(info ConnectionInfo, reason string) {
	key := nodeKey(info.HostName, info.IP)

	r.mu.Lock()
	defer r.mu.Unlock()

	entry := r.quarantine[key]
	entry.FailureCount++
	entry.LastReason = reason
	backoffFactor := min(entry.FailureCount-1, maxQuarantineBackoff)
	entry.QuarantinedUntil = time.Now().Add(r.autoConfig.BaseQuarantine * time.Duration(1<<backoffFactor))
	r.quarantine[key] = entry
}

func nodeKey(hostName, ip string) string {
	return strings.TrimSpace(hostName) + "|" + strings.TrimSpace(ip)
}

func (r *Runner) currentSnapshot() *ConnectionInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if r.current == nil {
		return nil
	}

	copied := *r.current
	return &copied
}

func (r *Runner) disconnectForRecovery() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.proc == nil || r.proc.Process == nil {
		return nil
	}

	r.disconnectRequested = true
	r.state = StateDisconnecting
	r.updatedAt = time.Now()

	if err := r.proc.Process.Signal(syscall.SIGTERM); err != nil {
		if killErr := r.proc.Process.Kill(); killErr != nil {
			return fmt.Errorf("自动停止 openvpn 失败: %w", err)
		}
	}

	return nil
}
