package web

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"vpngate/internal/runner"
	"vpngate/internal/runnerclient"
	"vpngate/internal/vpngate"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func TestSelectRecommendedServer(t *testing.T) {
	servers := []vpngate.Server{
		{HostName: "jp-zero-users", IP: "0.0.0.0", CountryLong: "Japan", CountryShort: "JP", TotalUsers: 0, Uptime: 1, NumVPNSessions: 1, OpenVPNConfigDataBase64: "cfg0"},
		{HostName: "jp-more-users", IP: "1.1.1.1", CountryLong: "Japan", CountryShort: "JP", TotalUsers: 20, Uptime: 10, NumVPNSessions: 1, OpenVPNConfigDataBase64: "cfg1"},
		{HostName: "kr-top", IP: "2.2.2.2", CountryLong: "Korea Republic of", CountryShort: "KR", TotalUsers: 1, Uptime: 1, NumVPNSessions: 1, OpenVPNConfigDataBase64: "cfg2"},
		{HostName: "jp-best", IP: "3.3.3.3", CountryLong: "Japan", CountryShort: "JP", TotalUsers: 5, Uptime: 3, NumVPNSessions: 2, OpenVPNConfigDataBase64: "cfg3"},
		{HostName: "jp-higher-uptime", IP: "4.4.4.4", CountryLong: "Japan", CountryShort: "JP", TotalUsers: 5, Uptime: 9, NumVPNSessions: 1, OpenVPNConfigDataBase64: "cfg4"},
	}

	server, ok := selectRecommendedServer(servers, "", "JP")
	if !ok {
		t.Fatal("selectRecommendedServer() ok = false, want true")
	}

	if server.HostName != "jp-best" {
		t.Fatalf("selectRecommendedServer() host = %q, want %q", server.HostName, "jp-best")
	}
}

func TestHandleVPNConnectUsesLatestServerList(t *testing.T) {
	var connectCalls atomic.Int32
	runnerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/connect" {
			http.NotFound(w, r)
			return
		}

		connectCalls.Add(1)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": runner.Status{State: runner.StateConnecting, SocksListenAddr: "127.0.0.1:1080"},
		})
	}))
	defer runnerServer.Close()

	app := mustNewTestApp(t, latestListResponse("fresh-node", "2.2.2.2", 200), runnerServer.URL, runnerServer.Client())
	app.mu.Lock()
	app.servers = []vpngate.Server{{HostName: "stale-node", IP: "1.1.1.1", CountryLong: "Japan", CountryShort: "JP"}}
	app.mu.Unlock()

	form := url.Values{
		"hostname": []string{"stale-node"},
		"ip":       []string{"1.1.1.1"},
	}
	req := httptest.NewRequest(http.MethodPost, "/vpn/connect", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
	req.Header.Set("Accept", "application/json")

	recorder := httptest.NewRecorder()
	app.handleVPNConnect(recorder, req)

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("handleVPNConnect() status = %d, want %d", recorder.Code, http.StatusNotFound)
	}

	var response actionResponse
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatalf("Decode() error = %v", err)
	}

	if response.OK {
		t.Fatal("handleVPNConnect() response.OK = true, want false")
	}
	if !strings.Contains(response.Error, "未在最新节点列表中找到对应节点") {
		t.Fatalf("handleVPNConnect() error = %q, want substring %q", response.Error, "未在最新节点列表中找到对应节点")
	}
	if connectCalls.Load() != 0 {
		t.Fatalf("runner connect calls = %d, want 0", connectCalls.Load())
	}
}

func TestHandleVPNConnectForwardsLatestServerPayload(t *testing.T) {
	type connectPayload struct {
		Server vpngate.Server `json:"server"`
	}

	payloadCh := make(chan connectPayload, 1)
	runnerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/connect" {
			http.NotFound(w, r)
			return
		}

		defer r.Body.Close()
		var payload connectPayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		payloadCh <- payload

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": runner.Status{State: runner.StateConnecting, SocksListenAddr: "127.0.0.1:1080"},
		})
	}))
	defer runnerServer.Close()

	app := mustNewTestApp(t, strings.Join([]string{
		"*vpn_servers",
		"#HostName,IP,Score,Ping,Speed,CountryLong,CountryShort,NumVpnSessions,Uptime,TotalUsers,TotalTraffic,LogType,Operator,Message,OpenVPN_ConfigData_Base64",
		"shared-node,1.1.1.1,300,12,450,Japan,JP,1,10,100,1000,2weeks,Fresh Operator,Fresh Message,ZnJlc2gtY29uZmln",
		"*",
	}, "\n"), runnerServer.URL, runnerServer.Client())
	app.mu.Lock()
	app.servers = []vpngate.Server{{
		HostName:                "shared-node",
		IP:                      "1.1.1.1",
		CountryLong:             "Japan",
		CountryShort:            "JP",
		Operator:                "Stale Operator",
		Message:                 "Stale Message",
		OpenVPNConfigDataBase64: "c3RhbGUtY29uZmln",
	}}
	app.mu.Unlock()

	form := url.Values{
		"hostname": []string{"shared-node"},
		"ip":       []string{"1.1.1.1"},
	}
	req := httptest.NewRequest(http.MethodPost, "/vpn/connect", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
	req.Header.Set("Accept", "application/json")

	recorder := httptest.NewRecorder()
	app.handleVPNConnect(recorder, req)

	if recorder.Code != http.StatusAccepted {
		t.Fatalf("handleVPNConnect() status = %d, want %d", recorder.Code, http.StatusAccepted)
	}

	var received connectPayload
	select {
	case received = <-payloadCh:
	default:
		t.Fatal("runner connect request was not received")
	}

	if received.Server.Operator != "Fresh Operator" {
		t.Fatalf("forwarded operator = %q, want %q", received.Server.Operator, "Fresh Operator")
	}
	if received.Server.OpenVPNConfigDataBase64 != "ZnJlc2gtY29uZmln" {
		t.Fatalf("forwarded config = %q, want %q", received.Server.OpenVPNConfigDataBase64, "ZnJlc2gtY29uZmln")
	}
}

func TestHandleVPNConnectRecommendedConnectsBestFilteredServer(t *testing.T) {
	type connectPayload struct {
		Server vpngate.Server `json:"server"`
	}

	payloadCh := make(chan connectPayload, 1)
	runnerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/connect" {
			http.NotFound(w, r)
			return
		}

		defer r.Body.Close()
		var payload connectPayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		payloadCh <- payload

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": runner.Status{State: runner.StateConnecting, SocksListenAddr: "127.0.0.1:1080"},
		})
	}))
	defer runnerServer.Close()

	app := mustNewTestApp(t, strings.Join([]string{
		"*vpn_servers",
		"#HostName,IP,Score,Ping,Speed,CountryLong,CountryShort,NumVpnSessions,Uptime,TotalUsers,TotalTraffic,LogType,Operator,Message,OpenVPN_ConfigData_Base64",
		"jp-zero,0.0.0.0,999,1,999,Japan,JP,1,1,0,1000,2weeks,Operator Zero,,ZHVtbXk=",
		"jp-mid,1.1.1.1,150,20,300,Japan,JP,10,10,100,1000,2weeks,Operator One,,ZHVtbXk=",
		"kr-top,2.2.2.2,400,30,500,Korea Republic of,KR,1,10,1,1000,2weeks,Operator Two,,ZHVtbXk=",
		"jp-best,3.3.3.3,300,25,450,Japan,JP,2,3,5,1000,2weeks,Operator Three,,ZHVtbXk=",
		"jp-higher-uptime,4.4.4.4,999,10,900,Japan,JP,1,9,5,1000,2weeks,Operator Four,,ZHVtbXk=",
		"*",
	}, "\n"), runnerServer.URL, runnerServer.Client())

	form := url.Values{
		"country": []string{"JP"},
	}
	req := httptest.NewRequest(http.MethodPost, "/vpn/connect/recommended", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
	req.Header.Set("Accept", "application/json")

	recorder := httptest.NewRecorder()
	app.handleVPNConnectRecommended(recorder, req)

	if recorder.Code != http.StatusAccepted {
		t.Fatalf("handleVPNConnectRecommended() status = %d, want %d", recorder.Code, http.StatusAccepted)
	}

	var received connectPayload
	select {
	case received = <-payloadCh:
	default:
		t.Fatal("runner connect request was not received")
	}

	if received.Server.HostName != "jp-best" {
		t.Fatalf("connected host = %q, want %q", received.Server.HostName, "jp-best")
	}

	var response actionResponse
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatalf("Decode() error = %v", err)
	}

	if !response.OK {
		t.Fatalf("handleVPNConnectRecommended() response.OK = false, error = %q", response.Error)
	}
	if !strings.Contains(response.Notice, "已开始连接推荐节点 jp-best") {
		t.Fatalf("handleVPNConnectRecommended() notice = %q, want substring %q", response.Notice, "已开始连接推荐节点 jp-best")
	}
}

func mustNewTestApp(t *testing.T, listResponse, runnerURL string, runnerHTTPClient *http.Client) *App {
	t.Helper()

	app, err := NewApp(
		log.New(io.Discard, "", 0),
		&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.String() != vpngate.IPhoneAPIURL {
				t.Fatalf("unexpected list request URL: %s", req.URL.String())
			}

			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(listResponse)),
				Request:    req,
			}, nil
		})},
		runnerclient.New(runnerURL, runnerHTTPClient),
	)
	if err != nil {
		t.Fatalf("NewApp() error = %v", err)
	}

	return app
}

func latestListResponse(hostName, ip string, score int64) string {
	return strings.Join([]string{
		"*vpn_servers",
		"#HostName,IP,Score,Ping,Speed,CountryLong,CountryShort,NumVpnSessions,Uptime,TotalUsers,TotalTraffic,LogType,Operator,Message,OpenVPN_ConfigData_Base64",
		hostName + "," + ip + "," + strconv.FormatInt(score, 10) + ",10,200,Japan,JP,1,10,100,1000,2weeks,Operator One,,ZHVtbXk=",
		"*",
	}, "\n")
}
