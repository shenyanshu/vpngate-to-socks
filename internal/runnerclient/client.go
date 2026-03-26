package runnerclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"vpngate/internal/runner"
	"vpngate/internal/vpngate"
)

type Client struct {
	baseURL    string
	httpClient *http.Client
}

type connectRequest struct {
	Server vpngate.Server `json:"server"`
}

type connectResponse struct {
	Status runner.Status `json:"status"`
	Error  string        `json:"error,omitempty"`
}

type testResponse struct {
	Result vpngate.OpenVPNTestResult `json:"result"`
	Error  string                    `json:"error,omitempty"`
}

func New(baseURL string, httpClient *http.Client) *Client {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if httpClient == nil {
		httpClient = &http.Client{}
	}

	return &Client{baseURL: baseURL, httpClient: httpClient}
}

func (c *Client) Enabled() bool {
	return c != nil && c.baseURL != ""
}

func (c *Client) Status(ctx context.Context) (runner.Status, error) {
	if !c.Enabled() {
		return runner.Status{}, fmt.Errorf("Runner 控制接口未配置")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/status", nil)
	if err != nil {
		return runner.Status{}, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return runner.Status{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return runner.Status{}, fmt.Errorf("Runner 状态接口返回异常状态: %s", resp.Status)
	}

	var status runner.Status
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return runner.Status{}, fmt.Errorf("解析 Runner 状态失败: %w", err)
	}

	return status, nil
}

func (c *Client) Connect(ctx context.Context, server vpngate.Server) (runner.Status, error) {
	if !c.Enabled() {
		return runner.Status{}, fmt.Errorf("Runner 控制接口未配置")
	}

	body, err := json.Marshal(connectRequest{Server: server})
	if err != nil {
		return runner.Status{}, fmt.Errorf("序列化连接请求失败: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/connect", bytes.NewReader(body))
	if err != nil {
		return runner.Status{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	return c.doConnectRequest(req)
}

func (c *Client) Disconnect(ctx context.Context) (runner.Status, error) {
	if !c.Enabled() {
		return runner.Status{}, fmt.Errorf("Runner 控制接口未配置")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/disconnect", nil)
	if err != nil {
		return runner.Status{}, err
	}

	return c.doConnectRequest(req)
}

func (c *Client) TestServer(ctx context.Context, server vpngate.Server) (vpngate.OpenVPNTestResult, error) {
	if !c.Enabled() {
		return vpngate.OpenVPNTestResult{}, fmt.Errorf("Runner 控制接口未配置")
	}

	body, err := json.Marshal(connectRequest{Server: server})
	if err != nil {
		return vpngate.OpenVPNTestResult{}, fmt.Errorf("序列化测试请求失败: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/test", bytes.NewReader(body))
	if err != nil {
		return vpngate.OpenVPNTestResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return vpngate.OpenVPNTestResult{}, err
	}
	defer resp.Body.Close()

	var payload testResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return vpngate.OpenVPNTestResult{}, fmt.Errorf("解析 Runner 测试响应失败: %w", err)
	}

	if resp.StatusCode >= 400 {
		if strings.TrimSpace(payload.Error) != "" {
			return payload.Result, errors.New(payload.Error)
		}

		return payload.Result, fmt.Errorf("Runner 测试接口返回异常状态: %s", resp.Status)
	}

	if strings.TrimSpace(payload.Error) != "" {
		return payload.Result, errors.New(payload.Error)
	}

	return payload.Result, nil
}

func (c *Client) doConnectRequest(req *http.Request) (runner.Status, error) {
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return runner.Status{}, err
	}
	defer resp.Body.Close()

	var payload connectResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return runner.Status{}, fmt.Errorf("解析 Runner 响应失败: %w", err)
	}

	if resp.StatusCode >= 400 {
		if strings.TrimSpace(payload.Error) != "" {
			return payload.Status, errors.New(payload.Error)
		}

		return payload.Status, fmt.Errorf("Runner 接口返回异常状态: %s", resp.Status)
	}

	if strings.TrimSpace(payload.Error) != "" {
		return payload.Status, errors.New(payload.Error)
	}

	return payload.Status, nil
}
