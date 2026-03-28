package vpngate

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

const OpenVPNSuccessMarker = "Initialization Sequence Completed"

const openVPNLogTailLimit = 40

const openVPNDefaultCipher = "AES-128-CBC"

// OpenVPNLaunchConfig contains the prepared values needed to start a local
// openvpn process safely for a VPN Gate server.
type OpenVPNLaunchConfig struct {
	Executable string
	ConfigText string
	Cipher     string
}

var allowedOpenVPNDirectives = map[string]struct{}{
	"auth":            {},
	"auth-nocache":    {},
	"cipher":          {},
	"client":          {},
	"comp-lzo":        {},
	"compress":        {},
	"connect-timeout": {},
	"dev":             {},
	"float":           {},
	"key-direction":   {},
	"nobind":          {},
	"persist-key":     {},
	"persist-tun":     {},
	"proto":           {},
	"remote":          {},
	"remote-cert-tls": {},
	"resolv-retry":    {},
	"verb":            {},
}

var allowedOpenVPNBlocks = map[string]string{
	"<ca>":        "</ca>",
	"<cert>":      "</cert>",
	"<key>":       "</key>",
	"<tls-auth>":  "</tls-auth>",
	"<tls-crypt>": "</tls-crypt>",
}

var blockedOpenVPNDirectives = map[string]struct{}{
	"client-connect":    {},
	"client-disconnect": {},
	"down":              {},
	"ipchange":          {},
	"learn-address":     {},
	"management":        {},
	"plugin":            {},
	"route-pre-down":    {},
	"route-up":          {},
	"script-security":   {},
	"tls-verify":        {},
	"up":                {},
}

// OpenVPNTestResult describes the outcome of a short OpenVPN connectivity test.
type OpenVPNTestResult struct {
	Duration time.Duration
	Detail   string
}

// TestServerWithOpenVPN writes the embedded OpenVPN config to a temporary file,
// starts the local openvpn client, and treats a completed initialization sequence
// as a successful test. The process is terminated immediately after success.
func TestServerWithOpenVPN(ctx context.Context, server Server) (OpenVPNTestResult, error) {
	launchConfig, err := PrepareOpenVPNLaunch(server)
	if err != nil {
		return OpenVPNTestResult{}, err
	}

	tmpFile, err := os.CreateTemp("", "vpngate-openvpn-test-*.ovpn")
	if err != nil {
		return OpenVPNTestResult{}, fmt.Errorf("创建临时 OpenVPN 配置文件失败: %w", err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := io.WriteString(tmpFile, launchConfig.ConfigText); err != nil {
		_ = tmpFile.Close()
		return OpenVPNTestResult{}, fmt.Errorf("写入临时 OpenVPN 配置文件失败: %w", err)
	}

	if err := tmpFile.Close(); err != nil {
		return OpenVPNTestResult{}, fmt.Errorf("关闭临时 OpenVPN 配置文件失败: %w", err)
	}

	return runOpenVPNTest(ctx, launchConfig.Executable, tmpFile.Name(), launchConfig.Cipher)
}

// PrepareOpenVPNLaunch decodes and sanitizes a VPN Gate OpenVPN configuration,
// and resolves the local openvpn executable for subsequent execution.
func PrepareOpenVPNLaunch(server Server) (OpenVPNLaunchConfig, error) {
	config, err := server.DecodeOpenVPNConfig()
	if err != nil {
		return OpenVPNLaunchConfig{}, err
	}

	sanitizedConfig, cipher, err := sanitizeOpenVPNConfig(config)
	if err != nil {
		return OpenVPNLaunchConfig{}, err
	}

	executable, err := resolveOpenVPNExecutable()
	if err != nil {
		return OpenVPNLaunchConfig{}, err
	}

	if strings.TrimSpace(cipher) == "" {
		cipher = openVPNDefaultCipher
	}

	return OpenVPNLaunchConfig{
		Executable: executable,
		ConfigText: sanitizedConfig,
		Cipher:     cipher,
	}, nil
}

// BuildOpenVPNTestArgs returns conservative short-lived arguments suitable for
// connectivity testing without keeping the tunnel active.
func BuildOpenVPNTestArgs(configPath, cipher string) []string {
	if strings.TrimSpace(cipher) == "" {
		cipher = openVPNDefaultCipher
	}

	return []string{
		"--verb", "4",
		"--config", configPath,
		"--script-security", "2",
		"--connect-retry-max", "3",
		"--connect-timeout", "10",
		"--route-nopull",
		"--data-ciphers", cipher,
	}
}

// BuildOpenVPNConnectArgs returns arguments for a real long-lived VPN
// connection. It intentionally stays close to the reference vpngate project
// and avoids overly aggressive retry limits that can prematurely fail
// otherwise usable VPN Gate nodes.
func BuildOpenVPNConnectArgs(configPath, cipher string) []string {
	if strings.TrimSpace(cipher) == "" {
		cipher = openVPNDefaultCipher
	}

	return []string{
		"--verb", "4",
		"--config", configPath,
		"--data-ciphers", cipher,
	}
}

func sanitizeOpenVPNConfig(raw string) (string, string, error) {
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	raw = strings.ReplaceAll(raw, "\r", "\n")
	lines := strings.Split(raw, "\n")

	output := make([]string, 0, len(lines))
	seenClient := false
	seenDev := false
	seenProto := false
	seenRemote := false
	seenCA := false
	seenCert := false
	seenKey := false
	var cipher string

	for i := 0; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, ";") {
			continue
		}

		lower := strings.ToLower(trimmed)
		if endTag, ok := allowedOpenVPNBlocks[lower]; ok {
			switch lower {
			case "<ca>":
				seenCA = true
			case "<cert>":
				seenCert = true
			case "<key>":
				seenKey = true
			}

			output = append(output, lower)
			closed := false
			for i = i + 1; i < len(lines); i++ {
				blockLine := strings.TrimSpace(lines[i])
				output = append(output, blockLine)
				if strings.EqualFold(blockLine, endTag) {
					closed = true
					break
				}
			}

			if !closed {
				return "", "", fmt.Errorf("OpenVPN 配置中的 %s 块缺少结束标记 %s", lower, endTag)
			}

			continue
		}

		fields := strings.Fields(trimmed)
		if len(fields) == 0 {
			continue
		}

		directive := strings.ToLower(fields[0])
		if _, blocked := blockedOpenVPNDirectives[directive]; blocked {
			return "", "", fmt.Errorf("OpenVPN 配置包含不安全指令 %q，已拒绝执行", directive)
		}

		if _, allowed := allowedOpenVPNDirectives[directive]; !allowed {
			return "", "", fmt.Errorf("OpenVPN 配置包含暂不支持的指令 %q，已拒绝执行", directive)
		}

		switch directive {
		case "client":
			seenClient = true
		case "dev":
			seenDev = true
		case "proto":
			seenProto = true
		case "remote":
			seenRemote = true
		case "cipher":
			if len(fields) > 1 {
				cipher = fields[1]
			}
		}

		output = append(output, trimmed)
	}

	if !seenClient {
		output = append([]string{"client"}, output...)
	}

	if !seenDev || !seenProto || !seenRemote {
		return "", "", fmt.Errorf("OpenVPN 配置缺少必要的 dev/proto/remote 指令")
	}

	if !seenCA || !seenCert || !seenKey {
		return "", "", fmt.Errorf("OpenVPN 配置缺少必要的证书块信息")
	}

	return strings.Join(output, "\n") + "\n", cipher, nil
}

func resolveOpenVPNExecutable() (string, error) {
	candidates := []string{"openvpn"}

	switch runtime.GOOS {
	case "darwin":
		candidates = append(candidates, "/opt/homebrew/sbin/openvpn", "/usr/local/sbin/openvpn")
	case "linux":
		candidates = append(candidates, "/usr/sbin/openvpn", "/sbin/openvpn")
	case "windows":
		candidates = append(candidates, `C:\Program Files\OpenVPN\bin\openvpn.exe`)
	}

	for _, candidate := range candidates {
		if strings.Contains(candidate, "/") || strings.Contains(candidate, `\`) {
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

	return "", fmt.Errorf("未找到 openvpn 可执行文件，请先安装 OpenVPN，并确保当前服务可访问该命令")
}

func runOpenVPNTest(ctx context.Context, executable, configPath, cipher string) (OpenVPNTestResult, error) {
	args := BuildOpenVPNTestArgs(configPath, cipher)
	cmd := exec.CommandContext(ctx, executable, args...)
	reader, writer := io.Pipe()
	defer reader.Close()

	cmd.Stdout = writer
	cmd.Stderr = writer

	scanDone := make(chan openVPNScanResult, 1)
	go func() {
		scanDone <- scanOpenVPNOutput(reader, func() {
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
		})
	}()

	start := time.Now()
	if err := cmd.Start(); err != nil {
		_ = writer.Close()
		result := <-scanDone
		if result.err != nil {
			return OpenVPNTestResult{}, fmt.Errorf("启动 openvpn 失败，且读取日志失败: %w", result.err)
		}

		return OpenVPNTestResult{}, fmt.Errorf("启动 openvpn 失败: %w", err)
	}

	waitErr := cmd.Wait()
	duration := time.Since(start)
	_ = writer.Close()

	scanResult := <-scanDone
	if scanResult.err != nil {
		return OpenVPNTestResult{Duration: duration}, fmt.Errorf("读取 OpenVPN 输出失败: %w", scanResult.err)
	}

	if scanResult.success {
		return OpenVPNTestResult{
			Duration: duration,
			Detail:   "OpenVPN 已完成握手并自动断开测试连接",
		}, nil
	}

	detail := SummarizeOpenVPNFailure(scanResult.lines)
	if ctx.Err() != nil {
		if detail == "" {
			detail = "在限定时间内未完成连接握手"
		}

		return OpenVPNTestResult{Duration: duration, Detail: detail}, fmt.Errorf("OpenVPN 测试超时：%s", detail)
	}

	if waitErr != nil {
		if detail == "" {
			return OpenVPNTestResult{Duration: duration}, fmt.Errorf("OpenVPN 进程异常退出: %w", waitErr)
		}

		return OpenVPNTestResult{Duration: duration, Detail: detail}, fmt.Errorf("OpenVPN 测试失败：%s", detail)
	}

	if detail == "" {
		detail = "OpenVPN 未输出明确的成功日志"
	}

	return OpenVPNTestResult{Duration: duration, Detail: detail}, fmt.Errorf("OpenVPN 未成功建立连接：%s", detail)
}

type openVPNScanResult struct {
	success bool
	lines   []string
	err     error
}

func scanOpenVPNOutput(r io.Reader, onSuccess func()) openVPNScanResult {
	scanner := bufio.NewScanner(r)
	lines := make([]string, 0, openVPNLogTailLimit)
	success := false

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		lines = append(lines, line)
		if len(lines) > openVPNLogTailLimit {
			lines = append([]string(nil), lines[len(lines)-openVPNLogTailLimit:]...)
		}

		if !success && strings.Contains(line, OpenVPNSuccessMarker) {
			success = true
			if onSuccess != nil {
				onSuccess()
			}
		}
	}

	return openVPNScanResult{
		success: success,
		lines:   lines,
		err:     scanner.Err(),
	}
}

// SummarizeOpenVPNFailure returns the most useful recent OpenVPN log lines for
// surfacing a user-visible failure reason.
func SummarizeOpenVPNFailure(lines []string) string {
	meaningful := make([]string, 0, 3)
	fallback := make([]string, 0, 3)

	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}

		if len(fallback) < 3 {
			fallback = append([]string{line}, fallback...)
		}

		if isIgnorableOpenVPNFailureLine(line) {
			continue
		}

		meaningful = append([]string{line}, meaningful...)
		if len(meaningful) == 3 {
			break
		}
	}

	if len(meaningful) > 0 {
		return strings.Join(meaningful, " | ")
	}

	if len(fallback) > 0 {
		return strings.Join(fallback, " | ")
	}

	return ""
}

func isIgnorableOpenVPNFailureLine(line string) bool {
	if strings.Contains(line, OpenVPNSuccessMarker) || strings.Contains(line, "SIGTERM[hard,") {
		return true
	}

	genericMarkers := []string{
		"Exiting due to fatal error",
		"Process exiting",
	}

	for _, marker := range genericMarkers {
		if strings.Contains(line, marker) {
			return true
		}
	}

	return false
}
