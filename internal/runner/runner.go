package runner

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"vpngate/internal/vpngate"
)

const runnerLogTailLimit = 80

const openVPNConnectTimeoutGrace = 2 * time.Second

type Runner struct {
	logger *log.Logger
	socks  *SOCKSServer

	mu                      sync.RWMutex
	autoConfig              AutoPilotConfig
	httpClient              *http.Client
	testing                 bool
	state                   State
	current                 *ConnectionInfo
	lastError               string
	connectedAt             time.Time
	updatedAt               time.Time
	logTail                 []string
	proc                    *exec.Cmd
	configPath              string
	disconnectRequested     bool
	bypassCIDRs             []string
	localCIDRs              []localBypassRoute
	originalGateway         string
	originalInterface       string
	autoPaused              bool
	lastAutoAttempt         time.Time
	lastMonitorConfirm      time.Time
	monitorFailureCount     int
	connectHandshakeSeen    bool
	connectTimeoutTriggered bool
	monitorIPs              []string
	monitorCancel           context.CancelFunc
	quarantine              map[string]nodeHealth
}

type openVPNScanResult struct {
	lines []string
	err   error
}

type localBypassRoute struct {
	CIDR      string
	Interface string
}

type bypassRouteSpec struct {
	CIDR      string
	Gateway   string
	Interface string
	Direct    bool
}

func New(logger *log.Logger, socksListenAddr string, bypassCIDRs []string, autoConfig AutoPilotConfig) (*Runner, error) {
	if logger == nil {
		logger = log.Default()
	}

	autoConfig = autoConfig.withDefaults()

	r := &Runner{
		logger:      logger,
		state:       StateDisconnected,
		updatedAt:   time.Now(),
		logTail:     make([]string, 0, runnerLogTailLimit),
		bypassCIDRs: sanitizeBypassCIDRs(bypassCIDRs),
		autoConfig:  autoConfig,
		httpClient:  &http.Client{Timeout: autoConfig.FetchTimeout},
		quarantine:  make(map[string]nodeHealth),
	}

	socks, err := newSOCKSServer(logger, socksListenAddr, r.canProxy)
	if err != nil {
		return nil, err
	}

	r.socks = socks
	return r, nil
}

func (r *Runner) Start(ctx context.Context) {
	if !r.autoConfig.Enabled {
		return
	}

	go r.autoLoop(ctx)
}

func (r *Runner) Close() error {
	if r.socks != nil {
		_ = r.socks.Close()
	}

	return r.Disconnect()
}

func (r *Runner) Status() Status {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var current *ConnectionInfo
	if r.current != nil {
		copied := *r.current
		current = &copied
	}

	logs := append([]string(nil), r.logTail...)

	return Status{
		State:           r.state,
		Current:         current,
		SocksListenAddr: r.socks.ListenAddr(),
		LastError:       r.lastError,
		ConnectedAt:     r.connectedAt,
		UpdatedAt:       r.updatedAt,
		LogTail:         logs,
	}
}

func (r *Runner) Connect(server vpngate.Server) error {
	summary := &ConnectionInfo{
		HostName:     server.HostName,
		IP:           server.IP,
		CountryLong:  server.CountryLong,
		CountryShort: server.CountryShort,
	}

	r.mu.Lock()
	if r.testing || r.proc != nil || r.state == StateConnecting || r.state == StateConnected || r.state == StateDisconnecting {
		currentHost := "当前节点"
		if r.current != nil && strings.TrimSpace(r.current.HostName) != "" {
			currentHost = r.current.HostName
		}
		if r.testing {
			currentHost = "测试流程"
		}
		r.mu.Unlock()
		return fmt.Errorf("当前已有活跃连接流程，请先断开节点 %s", currentHost)
	}

	r.state = StateConnecting
	r.current = summary
	r.lastError = ""
	r.connectedAt = time.Time{}
	r.updatedAt = time.Now()
	r.disconnectRequested = false
	r.autoPaused = false
	r.lastMonitorConfirm = time.Time{}
	r.monitorFailureCount = 0
	r.connectHandshakeSeen = false
	r.connectTimeoutTriggered = false
	r.logTail = r.logTail[:0]
	r.mu.Unlock()

	launchConfig, err := vpngate.PrepareOpenVPNLaunch(server)
	if err != nil {
		r.fail(summary, err.Error())
		return err
	}

	if len(r.bypassCIDRs) > 0 || r.autoConfig.Enabled {
		gateway, iface, err := discoverDefaultRoute()
		if err != nil {
			wrapped := fmt.Errorf("读取原始出口路由失败: %w", err)
			r.fail(summary, wrapped.Error())
			return wrapped
		}

		localCIDRs, err := discoverLocalCIDRs()
		if err != nil {
			r.logger.Printf("Runner 自动探测本地保留网段失败：%v", err)
		}

		r.mu.Lock()
		r.originalGateway = gateway
		r.originalInterface = iface
		r.localCIDRs = sanitizeLocalBypassRoutes(localCIDRs)
		r.mu.Unlock()
	}

	if err := r.prepareMonitorTargets(); err != nil {
		r.logger.Printf("Runner 准备监控目标失败：%v", err)
	}

	tmpFile, err := os.CreateTemp("", "vpngate-openvpn-runner-*.ovpn")
	if err != nil {
		wrapped := fmt.Errorf("创建临时 OpenVPN 配置文件失败: %w", err)
		r.fail(summary, wrapped.Error())
		return wrapped
	}

	if _, err := io.WriteString(tmpFile, launchConfig.ConfigText); err != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmpFile.Name())
		wrapped := fmt.Errorf("写入临时 OpenVPN 配置文件失败: %w", err)
		r.fail(summary, wrapped.Error())
		return wrapped
	}

	if err := tmpFile.Close(); err != nil {
		_ = os.Remove(tmpFile.Name())
		wrapped := fmt.Errorf("关闭临时 OpenVPN 配置文件失败: %w", err)
		r.fail(summary, wrapped.Error())
		return wrapped
	}

	args := vpngate.BuildOpenVPNConnectArgs(tmpFile.Name(), launchConfig.Cipher)
	cmd := exec.Command(launchConfig.Executable, args...)
	reader, writer := io.Pipe()
	cmd.Stdout = writer
	cmd.Stderr = writer

	scanDone := make(chan openVPNScanResult, 1)
	go func() {
		scanDone <- r.scanOpenVPNOutput(reader)
	}()

	r.mu.RLock()
	cancelledBeforeStart := r.disconnectRequested
	r.mu.RUnlock()
	if cancelledBeforeStart {
		_ = writer.Close()
		_ = reader.Close()
		_ = os.Remove(tmpFile.Name())
		<-scanDone
		r.resetAfterCancelledConnect()
		return fmt.Errorf("连接请求已取消")
	}

	if err := cmd.Start(); err != nil {
		_ = writer.Close()
		_ = reader.Close()
		_ = os.Remove(tmpFile.Name())
		scanResult := <-scanDone
		if scanResult.err != nil {
			r.fail(summary, fmt.Sprintf("启动 openvpn 失败，且读取日志失败: %v", scanResult.err))
			return fmt.Errorf("启动 openvpn 失败，且读取日志失败: %w", scanResult.err)
		}

		wrapped := fmt.Errorf("启动 openvpn 失败: %w", err)
		r.fail(summary, wrapped.Error())
		return wrapped
	}

	r.mu.Lock()
	r.proc = cmd
	r.configPath = tmpFile.Name()
	r.updatedAt = time.Now()
	disconnectRequested := r.disconnectRequested
	r.mu.Unlock()

	if disconnectRequested && cmd.Process != nil {
		_ = cmd.Process.Signal(syscall.SIGTERM)
	}

	r.logger.Printf("Runner 开始连接节点：%s（%s）", server.HostName, server.IP)
	go r.watchConnectTimeout(cmd, summary)
	go r.waitOpenVPN(cmd, writer, scanDone, tmpFile.Name(), summary)
	return nil
}

func (r *Runner) Disconnect() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.testing {
		return fmt.Errorf("当前正在进行节点测试，请等待测试结束后再断开连接")
	}

	if r.proc == nil || r.proc.Process == nil {
		if r.state == StateConnecting {
			r.disconnectRequested = true
			r.state = StateDisconnecting
			r.autoPaused = true
			r.monitorFailureCount = 0
			r.connectTimeoutTriggered = false
			r.updatedAt = time.Now()
			return nil
		}

		r.state = StateDisconnected
		r.current = nil
		r.lastError = ""
		r.connectedAt = time.Time{}
		r.autoPaused = true
		r.monitorFailureCount = 0
		r.connectHandshakeSeen = false
		r.connectTimeoutTriggered = false
		r.updatedAt = time.Now()
		return nil
	}

	r.disconnectRequested = true
	r.state = StateDisconnecting
	r.autoPaused = true
	r.monitorFailureCount = 0
	r.updatedAt = time.Now()

	if err := r.proc.Process.Signal(syscall.SIGTERM); err != nil {
		if killErr := r.proc.Process.Kill(); killErr != nil {
			return fmt.Errorf("停止 openvpn 失败: %w", err)
		}
	}

	return nil
}

func (r *Runner) canProxy() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return r.state == StateConnected
}

func (r *Runner) TestServer(ctx context.Context, server vpngate.Server) (vpngate.OpenVPNTestResult, error) {
	r.mu.Lock()
	if r.testing || r.proc != nil || r.state == StateConnecting || r.state == StateConnected || r.state == StateDisconnecting {
		currentHost := "当前节点"
		if r.current != nil && strings.TrimSpace(r.current.HostName) != "" {
			currentHost = r.current.HostName
		}
		if r.testing {
			currentHost = "测试流程"
		}
		r.mu.Unlock()
		return vpngate.OpenVPNTestResult{}, fmt.Errorf("当前已有活跃连接流程，请先结束 %s 后再测试其他节点", currentHost)
	}

	r.testing = true
	r.updatedAt = time.Now()
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.testing = false
		r.updatedAt = time.Now()
		r.mu.Unlock()
	}()

	r.logger.Printf("Runner 开始测试节点：%s（%s）", server.HostName, server.IP)
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	result, err := vpngate.TestServerWithOpenVPN(ctx, server)
	if err != nil {
		r.logger.Printf("Runner 测试节点失败：%s（%s）：%v", server.HostName, server.IP, err)
		return vpngate.OpenVPNTestResult{}, err
	}

	r.logger.Printf("Runner 测试节点成功：%s（%s），耗时 %s", server.HostName, server.IP, result.Duration)
	return result, nil
}

func (r *Runner) scanOpenVPNOutput(reader io.Reader) openVPNScanResult {
	scanner := bufio.NewScanner(reader)
	lines := make([]string, 0, runnerLogTailLimit)
	connected := false

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		lines = append(lines, line)
		if len(lines) > runnerLogTailLimit {
			lines = append([]string(nil), lines[len(lines)-runnerLogTailLimit:]...)
		}

		r.appendLog(line)

		if !connected && strings.Contains(line, vpngate.OpenVPNSuccessMarker) {
			r.markConnectHandshakeSeen()
			if err := r.applyBypassRoutes(); err != nil {
				r.logger.Printf("Runner 应用局域网保留路由失败：%v", err)
				r.setLastError(err.Error())
			} else if len(r.bypassCIDRs) > 0 {
				r.logger.Printf("Runner 已应用局域网保留路由：%s", strings.Join(r.bypassCIDRs, ", "))
			}

			connected = true
			r.markConnected()
			r.startMonitorLoop()
		}
	}

	return openVPNScanResult{lines: lines, err: scanner.Err()}
}

func (r *Runner) waitOpenVPN(cmd *exec.Cmd, writer *io.PipeWriter, scanDone <-chan openVPNScanResult, configPath string, summary *ConnectionInfo) {
	waitErr := cmd.Wait()
	r.stopMonitorLoop()
	_ = writer.Close()
	scanResult := <-scanDone
	_ = os.Remove(configPath)

	detail := vpngate.SummarizeOpenVPNFailure(scanResult.lines)
	if scanResult.err != nil {
		if detail != "" {
			detail += " | "
		}
		detail += fmt.Sprintf("读取 OpenVPN 日志失败: %v", scanResult.err)
	}

	r.mu.Lock()
	disconnectRequested := r.disconnectRequested
	timeoutTriggered := r.connectTimeoutTriggered
	handshakeSeen := r.connectHandshakeSeen
	connectTimeout := r.autoConfig.OpenVPNConnectTimeout
	r.proc = nil
	r.configPath = ""
	r.disconnectRequested = false
	r.connectHandshakeSeen = false
	r.connectTimeoutTriggered = false
	r.monitorFailureCount = 0
	r.updatedAt = time.Now()

	if disconnectRequested {
		r.state = StateDisconnected
		r.current = nil
		r.connectedAt = time.Time{}
		r.lastError = ""
		r.mu.Unlock()
		r.logger.Println("Runner 已断开当前 OpenVPN 连接")
		return
	}
	r.mu.Unlock()

	timedOutBeforeHandshake := timeoutTriggered && !handshakeSeen
	if timedOutBeforeHandshake {
		timeoutDetail := fmt.Sprintf("连接握手超时，超过 %s", connectTimeout.Round(time.Second))
		if strings.TrimSpace(detail) == "" {
			detail = timeoutDetail
		} else if !strings.Contains(detail, timeoutDetail) {
			detail = timeoutDetail + "；" + detail
		}
	}

	if waitErr != nil || timedOutBeforeHandshake {
		if strings.TrimSpace(detail) == "" {
			if timedOutBeforeHandshake {
				detail = fmt.Sprintf("连接握手超时，超过 %s", connectTimeout.Round(time.Second))
			} else {
				detail = waitErr.Error()
			}
		}

		r.markQuarantine(*summary, detail)
		r.mu.Lock()
		r.state = StateFailed
		r.current = summary
		r.lastError = detail
		r.connectedAt = time.Time{}
		r.mu.Unlock()
		r.logger.Printf("Runner 节点连接失败：%s（%s）：%s", summary.HostName, summary.IP, detail)
		return
	}

	r.mu.Lock()
	r.state = StateDisconnected
	r.current = nil
	r.connectedAt = time.Time{}
	r.lastError = detail
	if strings.TrimSpace(detail) == "" {
		r.lastError = "OpenVPN 连接已结束"
	}
	r.mu.Unlock()
	r.logger.Printf("Runner OpenVPN 连接已结束：%s（%s）", summary.HostName, summary.IP)
}

func (r *Runner) appendLog(line string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.logTail = append(r.logTail, line)
	if len(r.logTail) > runnerLogTailLimit {
		r.logTail = append([]string(nil), r.logTail[len(r.logTail)-runnerLogTailLimit:]...)
	}
	r.updatedAt = time.Now()
}

func (r *Runner) markConnected() {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.state = StateConnected
	r.lastError = ""
	if r.connectedAt.IsZero() {
		r.connectedAt = time.Now()
	}
	r.monitorFailureCount = 0
	r.updatedAt = time.Now()
	if r.current != nil {
		r.logger.Printf("Runner 节点连接成功：%s（%s）", r.current.HostName, r.current.IP)
	}
}

func (r *Runner) fail(summary *ConnectionInfo, detail string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.state = StateFailed
	r.current = summary
	r.lastError = detail
	r.connectedAt = time.Time{}
	r.monitorFailureCount = 0
	r.connectHandshakeSeen = false
	r.connectTimeoutTriggered = false
	r.updatedAt = time.Now()
}

func (r *Runner) resetAfterCancelledConnect() {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.state = StateDisconnected
	r.current = nil
	r.lastError = ""
	r.connectedAt = time.Time{}
	r.monitorFailureCount = 0
	r.connectHandshakeSeen = false
	r.connectTimeoutTriggered = false
	r.updatedAt = time.Now()
	r.disconnectRequested = false
}

func (r *Runner) markConnectHandshakeSeen() {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.connectHandshakeSeen = true
}

func (r *Runner) markConnectTimeoutTriggered() {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.connectTimeoutTriggered = true
	r.updatedAt = time.Now()
}

func (r *Runner) shouldAbortConnectTimeout(cmd *exec.Cmd) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return r.proc == cmd && r.state == StateConnecting && !r.disconnectRequested && !r.connectHandshakeSeen
}

func (r *Runner) watchConnectTimeout(cmd *exec.Cmd, summary *ConnectionInfo) {
	timeout := r.autoConfig.OpenVPNConnectTimeout
	if timeout <= 0 {
		return
	}

	timer := time.NewTimer(timeout + openVPNConnectTimeoutGrace)
	defer timer.Stop()

	<-timer.C
	if !r.shouldAbortConnectTimeout(cmd) {
		return
	}
	if cmd.Process == nil {
		return
	}

	r.logger.Printf("Runner 连接握手超时：%s（%s），超过 %s，正在终止当前 OpenVPN 进程", summary.HostName, summary.IP, timeout.Round(time.Second))
	r.markConnectTimeoutTriggered()
	_ = cmd.Process.Signal(syscall.SIGTERM)

	time.AfterFunc(2*time.Second, func() {
		if !r.shouldAbortConnectTimeout(cmd) {
			return
		}
		if cmd.Process == nil {
			return
		}
		_ = cmd.Process.Kill()
	})
}

func (r *Runner) setLastError(detail string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.lastError = detail
	r.updatedAt = time.Now()
}

func (r *Runner) applyBypassRoutes() error {
	r.mu.RLock()
	bypassCIDRs := append([]string(nil), r.bypassCIDRs...)
	localCIDRs := append([]localBypassRoute(nil), r.localCIDRs...)
	gateway := r.originalGateway
	iface := r.originalInterface
	autoConfig := r.autoConfig
	r.mu.RUnlock()

	bypassCIDRs = sanitizeBypassCIDRs(bypassCIDRs)
	localCIDRs = sanitizeLocalBypassRoutes(localCIDRs)

	if len(bypassCIDRs) == 0 && len(localCIDRs) == 0 && !autoConfig.Enabled {
		return nil
	}

	if (autoConfig.Enabled || len(bypassCIDRs) > 0) && (strings.TrimSpace(gateway) == "" || strings.TrimSpace(iface) == "") {
		return fmt.Errorf("缺少原始网关或接口信息，无法应用局域网保留路由")
	}

	ipExecutable, err := resolveIPExecutable()
	if err != nil {
		return err
	}

	if autoConfig.Enabled {
		if err := ensureBypassPolicyRouting(ipExecutable, gateway, iface, autoConfig.BypassRouteTable, autoConfig.BypassMark); err != nil {
			return err
		}
	}

	routeSpecs := buildBypassRouteSpecs(bypassCIDRs, localCIDRs, gateway, iface)
	if len(routeSpecs) == 0 {
		return nil
	}

	for _, spec := range routeSpecs {
		cmd := exec.Command(ipExecutable, buildRouteReplaceArgs(spec)...)
		if output, err := cmd.CombinedOutput(); err != nil {
			trimmed := strings.TrimSpace(string(output))
			if trimmed == "" {
				return fmt.Errorf("为 %s 应用保留路由失败: %w", spec.CIDR, err)
			}

			return fmt.Errorf("为 %s 应用保留路由失败: %s", spec.CIDR, trimmed)
		}
	}

	return nil
}

func buildBypassRouteSpecs(bypassCIDRs []string, localCIDRs []localBypassRoute, gateway, originalInterface string) []bypassRouteSpec {
	specs := make([]bypassRouteSpec, 0, len(bypassCIDRs)+len(localCIDRs))

	for _, cidr := range sanitizeBypassCIDRs(bypassCIDRs) {
		if strings.TrimSpace(originalInterface) == "" || strings.TrimSpace(gateway) == "" {
			continue
		}

		specs = append(specs, bypassRouteSpec{
			CIDR:      cidr,
			Gateway:   gateway,
			Interface: originalInterface,
		})
	}

	for _, localRoute := range sanitizeLocalBypassRoutes(localCIDRs) {
		specs = append(specs, bypassRouteSpec{
			CIDR:      localRoute.CIDR,
			Interface: localRoute.Interface,
			Direct:    true,
		})
	}

	return specs
}

func buildRouteReplaceArgs(spec bypassRouteSpec) []string {
	if spec.Direct {
		return []string{"route", "replace", spec.CIDR, "dev", spec.Interface, "scope", "link"}
	}

	return []string{"route", "replace", spec.CIDR, "via", spec.Gateway, "dev", spec.Interface}
}

func ensureBypassPolicyRouting(ipExecutable, gateway, iface string, table, mark int) error {
	routeArgs := []string{"route", "replace", "default", "via", gateway, "dev", iface, "table", fmt.Sprintf("%d", table)}
	output, err := exec.Command(ipExecutable, routeArgs...).CombinedOutput()
	if err != nil {
		trimmed := strings.TrimSpace(string(output))
		if strings.Contains(trimmed, "Nexthop has invalid gateway") {
			fallbackArgs := []string{"route", "replace", "default", "via", gateway, "dev", iface, "onlink", "table", fmt.Sprintf("%d", table)}
			output, err = exec.Command(ipExecutable, fallbackArgs...).CombinedOutput()
			if err == nil {
				goto ensureRule
			}
			trimmed = strings.TrimSpace(string(output))
		}

		if trimmed == "" {
			return fmt.Errorf("应用 bypass 策略路由失败: %w", err)
		}

		return fmt.Errorf("应用 bypass 策略路由失败: %s", trimmed)
	}

ensureRule:
	ruleArgs := []string{"rule", "add", "fwmark", fmt.Sprintf("%d", mark), "table", fmt.Sprintf("%d", table)}
	output, err = exec.Command(ipExecutable, ruleArgs...).CombinedOutput()
	if err != nil {
		trimmed := strings.TrimSpace(string(output))
		if strings.Contains(trimmed, "File exists") {
			return nil
		}

		if trimmed == "" {
			return fmt.Errorf("应用 bypass 策略路由失败: %w", err)
		}

		return fmt.Errorf("应用 bypass 策略路由失败: %s", trimmed)
	}

	return nil
}

func discoverDefaultRoute() (string, string, error) {
	ipExecutable, err := resolveIPExecutable()
	if err != nil {
		return "", "", err
	}

	output, err := exec.Command(ipExecutable, "route", "show", "default").CombinedOutput()
	if err != nil {
		trimmed := strings.TrimSpace(string(output))
		if trimmed == "" {
			return "", "", fmt.Errorf("读取默认路由失败: %w", err)
		}

		return "", "", fmt.Errorf("读取默认路由失败: %s", trimmed)
	}

	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 || fields[0] != "default" {
			continue
		}

		var gateway string
		var iface string
		for i := range len(fields) {
			switch fields[i] {
			case "via":
				if i+1 < len(fields) {
					gateway = fields[i+1]
				}
			case "dev":
				if i+1 < len(fields) {
					iface = fields[i+1]
				}
			}
		}

		if gateway != "" && iface != "" {
			return gateway, iface, nil
		}
	}

	return "", "", fmt.Errorf("未找到有效的默认路由")
}

func resolveIPExecutable() (string, error) {
	candidates := []string{"ip", "/sbin/ip", "/usr/sbin/ip"}

	for _, candidate := range candidates {
		if strings.Contains(candidate, "/") {
			if _, err := os.Stat(candidate); err == nil {
				return candidate, nil
			}
			continue
		}

		path, err := exec.LookPath(candidate)
		if err == nil {
			return path, nil
		}
	}

	return "", fmt.Errorf("未找到 ip 命令，无法设置局域网保留路由")
}

func discoverLocalCIDRs() ([]localBypassRoute, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("读取本地网络接口失败: %w", err)
	}

	var routes []localBypassRoute
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		if strings.HasPrefix(strings.ToLower(iface.Name), "tun") {
			continue
		}

		addresses, err := iface.Addrs()
		if err != nil {
			return nil, fmt.Errorf("读取接口 %s 地址失败: %w", iface.Name, err)
		}

		for _, addr := range addresses {
			prefix, ok := addr.(*net.IPNet)
			if !ok || prefix == nil {
				continue
			}
			ip := prefix.IP
			if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
				continue
			}
			if ip.To4() == nil && ip.To16() == nil {
				continue
			}

			networkCIDR, err := normalizeCIDR(prefix)
			if err != nil {
				return nil, fmt.Errorf("规范化接口 %s 网段失败: %w", iface.Name, err)
			}
			routes = append(routes, localBypassRoute{CIDR: networkCIDR, Interface: iface.Name})
		}
	}

	return sanitizeLocalBypassRoutes(routes), nil
}

func normalizeCIDR(prefix *net.IPNet) (string, error) {
	if prefix == nil || prefix.IP == nil || prefix.Mask == nil {
		return "", fmt.Errorf("CIDR 为空")
	}

	networkIP := prefix.IP.Mask(prefix.Mask)
	if networkIP == nil {
		return "", fmt.Errorf("CIDR 无法按掩码归一化")
	}

	return (&net.IPNet{IP: networkIP, Mask: prefix.Mask}).String(), nil
}

func sanitizeBypassCIDRs(values []string) []string {
	cleaned := make([]string, 0, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" || slices.Contains(cleaned, trimmed) {
			continue
		}

		cleaned = append(cleaned, trimmed)
	}

	return cleaned
}

func sanitizeLocalBypassRoutes(values []localBypassRoute) []localBypassRoute {
	cleaned := make([]localBypassRoute, 0, len(values))
	seen := make(map[string]struct{}, len(values))

	for _, value := range values {
		cidr := strings.TrimSpace(value.CIDR)
		iface := strings.TrimSpace(value.Interface)
		if cidr == "" || iface == "" {
			continue
		}

		key := iface + "\x00" + cidr
		if _, ok := seen[key]; ok {
			continue
		}

		seen[key] = struct{}{}
		cleaned = append(cleaned, localBypassRoute{CIDR: cidr, Interface: iface})
	}

	return cleaned
}
