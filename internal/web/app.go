package web

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"maps"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"vpngate/internal/runner"
	"vpngate/internal/vpngate"
)

//go:embed templates/*.html
var templateFS embed.FS

type App struct {
	client *http.Client
	logger *log.Logger
	tmpl   *template.Template
	runner RunnerControl

	mu                  sync.RWMutex
	servers             []vpngate.Server
	lastUpdated         time.Time
	lastRefreshDuration time.Duration
	lastError           string
	testResults         map[string]serverTestState
	testSlot            chan struct{}
}

type RunnerControl interface {
	Enabled() bool
	Status(ctx context.Context) (runner.Status, error)
	Connect(ctx context.Context, server vpngate.Server) (runner.Status, error)
	Disconnect(ctx context.Context) (runner.Status, error)
	TestServer(ctx context.Context, server vpngate.Server) (vpngate.OpenVPNTestResult, error)
}

type PageData struct {
	Title               string
	Description         string
	StatusText          string
	StatusClass         string
	Notice              string
	FlashError          string
	Error               string
	Query               string
	SelectedCountry     string
	Countries           []CountryOption
	UpdatedAt           string
	RefreshDuration     string
	TotalCount          int
	SourceCount         int
	CountryCount        int
	AveragePing         string
	HighestSpeed        string
	Rows                []ServerRow
	CurrentYear         int
	SupportsRefresh     bool
	SupportsVPNControl  bool
	HasData             bool
	LastUpdatedReadable string
	VPNStatusText       string
	VPNStatusClass      string
	VPNStatusDetail     string
	VPNSocksAddress     string
	VPNCurrentNode      string
	VPNCurrentIP        string
	VPNConnectedSince   string
	VPNCanDisconnect    bool
}

type CountryOption struct {
	Value string
	Label string
}

type ServerRow struct {
	ServerKey   string
	Rank        int
	Name        string
	Country     string
	IP          string
	Protocol    string
	Ping        string
	Speed       string
	Sessions    string
	Uptime      string
	Users       string
	Traffic     string
	Operator    string
	Message     string
	Score       string
	CountryTag  string
	TestStatus  string
	TestClass   string
	TestDetail  string
	IsVPNActive bool
}

type serverTestState struct {
	Status    string
	ClassName string
	Detail    string
	UpdatedAt time.Time
}

type actionResponse struct {
	OK     bool               `json:"ok"`
	Notice string             `json:"notice,omitempty"`
	Error  string             `json:"error,omitempty"`
	Reload bool               `json:"reload,omitempty"`
	Test   *serverTestPayload `json:"test,omitempty"`
}

type serverTestPayload struct {
	Key       string `json:"key"`
	HostName  string `json:"hostName"`
	IP        string `json:"ip"`
	Status    string `json:"status"`
	ClassName string `json:"className"`
	Detail    string `json:"detail"`
}

type responseRecorder struct {
	http.ResponseWriter
	status int
}

func (rw *responseRecorder) WriteHeader(status int) {
	rw.status = status
	rw.ResponseWriter.WriteHeader(status)
}

func NewApp(logger *log.Logger, client *http.Client, runnerClient RunnerControl) (*App, error) {
	if logger == nil {
		logger = log.Default()
	}

	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}

	tmpl, err := template.ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("加载 HTML 模板失败: %w", err)
	}

	return &App{
		client:      client,
		logger:      logger,
		tmpl:        tmpl,
		runner:      runnerClient,
		testResults: make(map[string]serverTestState),
		testSlot:    make(chan struct{}, 1),
	}, nil
}

func (a *App) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", a.handleIndex)
	mux.HandleFunc("/refresh", a.handleRefresh)
	mux.HandleFunc("/servers/test", a.handleServerTest)
	mux.HandleFunc("/vpn/connect/recommended", a.handleVPNConnectRecommended)
	mux.HandleFunc("/vpn/connect", a.handleVPNConnect)
	mux.HandleFunc("/vpn/disconnect", a.handleVPNDisconnect)
	mux.HandleFunc("/vpn/status", a.handleVPNStatus)
	mux.HandleFunc("/health", a.handleHealth)

	return a.loggingMiddleware(mux)
}

func (a *App) Refresh(ctx context.Context) error {
	_, err := a.refreshServers(ctx)
	return err
}

func (a *App) refreshServers(ctx context.Context) ([]vpngate.Server, error) {
	a.logger.Println("开始刷新 VPNGate 节点列表……")
	start := time.Now()

	servers, err := vpngate.FetchIPhoneServers(ctx, a.client)
	duration := time.Since(start)
	if err != nil {
		a.mu.Lock()
		a.lastError = "刷新失败，请稍后重试"
		a.lastRefreshDuration = duration
		a.mu.Unlock()

		a.logger.Printf("刷新 VPNGate 节点列表失败：%v", err)
		return nil, fmt.Errorf("刷新 VPNGate 节点列表失败: %w", err)
	}

	sortServers(servers)

	a.mu.Lock()
	a.servers = servers
	a.lastUpdated = time.Now()
	a.lastRefreshDuration = duration
	a.lastError = ""
	a.testResults = make(map[string]serverTestState)
	a.mu.Unlock()

	a.logger.Printf("刷新 VPNGate 节点列表成功，共 %d 个节点，耗时 %s", len(servers), formatDurationCN(duration))
	return append([]vpngate.Server(nil), servers...), nil
}

func (a *App) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "仅支持 GET 请求", http.StatusMethodNotAllowed)
		return
	}

	page := a.buildPageData(
		r.URL.Query().Get("notice"),
		r.URL.Query().Get("error"),
		r.URL.Query().Get("q"),
		r.URL.Query().Get("country"),
	)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := a.tmpl.ExecuteTemplate(w, "index.html", page); err != nil {
		a.logger.Printf("渲染管理页面失败：%v", err)
		http.Error(w, "页面渲染失败，请稍后重试", http.StatusInternalServerError)
	}
}

func (a *App) handleRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		a.writeActionError(w, r, http.StatusMethodNotAllowed, "仅支持 POST 请求")
		return
	}

	if err := validateSameOriginRequest(r); err != nil {
		a.writeActionError(w, r, http.StatusForbidden, err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	notice := "节点数据已刷新"
	flashError := ""
	if err := a.Refresh(ctx); err != nil {
		notice = ""
		flashError = "刷新失败，请稍后重试"
	}

	if wantsJSONResponse(r) {
		a.writeJSON(w, http.StatusOK, actionResponse{
			OK:     flashError == "",
			Notice: notice,
			Error:  flashError,
			Reload: flashError == "",
		})
		return
	}

	http.Redirect(w, r, buildIndexURL(notice, flashError, "", ""), http.StatusSeeOther)
}

func (a *App) handleServerTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		a.writeActionError(w, r, http.StatusMethodNotAllowed, "仅支持 POST 请求")
		return
	}

	if err := validateSameOriginRequest(r); err != nil {
		a.writeActionError(w, r, http.StatusForbidden, err.Error())
		return
	}

	if err := parseSubmittedForm(r); err != nil {
		a.writeActionError(w, r, http.StatusBadRequest, "读取表单失败")
		return
	}

	hostName := strings.TrimSpace(r.FormValue("hostname"))
	ip := strings.TrimSpace(r.FormValue("ip"))
	query := strings.TrimSpace(r.FormValue("q"))
	selectedCountry := strings.TrimSpace(r.FormValue("country"))

	redirectToIndex := func(notice, flashError string) {
		http.Redirect(w, r, buildIndexURL(notice, flashError, query, selectedCountry), http.StatusSeeOther)
	}

	respondTestAction := func(statusCode int, ok bool, notice, flashError string, server *vpngate.Server, state *serverTestState) {
		if wantsJSONResponse(r) {
			response := actionResponse{OK: ok, Notice: notice, Error: flashError}
			if server != nil && state != nil {
				response.Test = buildServerTestPayload(*server, *state)
			}
			a.writeJSON(w, statusCode, response)
			return
		}

		redirectToIndex(notice, flashError)
	}

	if hostName == "" || ip == "" {
		respondTestAction(http.StatusBadRequest, false, "", "缺少节点标识，无法发起测试", nil, nil)
		return
	}

	server, ok := a.findServer(hostName, ip)
	if !ok {
		respondTestAction(http.StatusNotFound, false, "", "未找到对应节点，请先刷新列表后再试", nil, nil)
		return
	}

	select {
	case a.testSlot <- struct{}{}:
		defer func() {
			<-a.testSlot
		}()
	default:
		respondTestAction(http.StatusConflict, false, "", "已有节点正在进行 OpenVPN 测试，请等待当前测试结束后再试", &server, nil)
		return
	}

	key := serverTestKey(server.HostName, server.IP)
	a.setServerTestState(key, serverTestState{
		Status:    "测试中",
		ClassName: "test-running",
		Detail:    "OpenVPN 正在尝试建立连接，请耐心等待测试完成",
		UpdatedAt: time.Now(),
	})

	a.logger.Printf("开始测试节点：%s（%s）", server.HostName, server.IP)

	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()

	var result vpngate.OpenVPNTestResult
	var err error
	if a.runner != nil && a.runner.Enabled() {
		result, err = a.runner.TestServer(ctx, server)
	} else {
		result, err = vpngate.TestServerWithOpenVPN(ctx, server)
	}
	if err != nil {
		failureDetail := err.Error()
		failureState := serverTestState{
			Status:    "测试失败",
			ClassName: "test-failure",
			Detail:    failureDetail,
			UpdatedAt: time.Now(),
		}
		a.setServerTestState(key, failureState)

		a.logger.Printf("测试节点失败：%s（%s）：%v", server.HostName, server.IP, err)
		respondTestAction(http.StatusOK, false, "", fmt.Sprintf("节点 %s 测试失败：%s", server.HostName, failureDetail), &server, &failureState)
		return
	}

	successDetail := result.Detail
	if strings.TrimSpace(successDetail) == "" {
		successDetail = fmt.Sprintf("在 %s 内完成握手并自动断开", formatDurationCN(result.Duration))
	} else {
		successDetail = fmt.Sprintf("%s，用时 %s", successDetail, formatDurationCN(result.Duration))
	}

	successState := serverTestState{
		Status:    "测试通过",
		ClassName: "test-success",
		Detail:    successDetail,
		UpdatedAt: time.Now(),
	}
	a.setServerTestState(key, successState)

	a.logger.Printf("测试节点成功：%s（%s），耗时 %s", server.HostName, server.IP, formatDurationCN(result.Duration))
	respondTestAction(http.StatusOK, true, fmt.Sprintf("节点 %s 测试通过，用时 %s", server.HostName, formatDurationCN(result.Duration)), "", &server, &successState)
}

func (a *App) handleVPNConnect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		a.writeActionError(w, r, http.StatusMethodNotAllowed, "仅支持 POST 请求")
		return
	}

	if err := validateSameOriginRequest(r); err != nil {
		a.writeActionError(w, r, http.StatusForbidden, err.Error())
		return
	}

	if err := parseSubmittedForm(r); err != nil {
		a.writeActionError(w, r, http.StatusBadRequest, "读取表单失败")
		return
	}

	query := strings.TrimSpace(r.FormValue("q"))
	selectedCountry := strings.TrimSpace(r.FormValue("country"))
	hostName := strings.TrimSpace(r.FormValue("hostname"))
	ip := strings.TrimSpace(r.FormValue("ip"))

	respond := func(statusCode int, ok bool, notice, flashError string) {
		if wantsJSONResponse(r) {
			a.writeJSON(w, statusCode, actionResponse{OK: ok, Notice: notice, Error: flashError, Reload: ok})
			return
		}

		http.Redirect(w, r, buildIndexURL(notice, flashError, query, selectedCountry), http.StatusSeeOther)
	}

	if a.runner == nil || !a.runner.Enabled() {
		respond(http.StatusServiceUnavailable, false, "", "VPN Runner 未配置，暂时无法建立持久连接")
		return
	}

	if hostName == "" || ip == "" {
		respond(http.StatusBadRequest, false, "", "缺少节点标识，无法建立连接")
		return
	}

	fetchCtx, fetchCancel := context.WithTimeout(r.Context(), 30*time.Second)
	servers, err := a.refreshServers(fetchCtx)
	fetchCancel()
	if err != nil {
		respond(http.StatusBadGateway, false, "", "连接前刷新最新节点列表失败，请稍后重试")
		return
	}

	server, ok := findServerInList(servers, hostName, ip)
	if !ok {
		respond(http.StatusNotFound, false, "", "未在最新节点列表中找到对应节点，可能已失效，请改用“连接推荐节点”或重新选择节点")
		return
	}

	connectCtx, connectCancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer connectCancel()

	status, err := a.runner.Connect(connectCtx, server)
	if err != nil {
		message := fmt.Sprintf("连接节点 %s 失败：%v", server.HostName, err)
		if status.State == runner.StateConnecting || status.State == runner.StateConnected || status.State == runner.StateDisconnecting {
			respond(http.StatusConflict, false, "", message)
			return
		}

		respond(http.StatusInternalServerError, false, "", message)
		return
	}

	statusCode := http.StatusAccepted
	notice := fmt.Sprintf("已开始连接节点 %s，SOCKS5 代理地址 %s", server.HostName, status.SocksListenAddr)
	if status.State == runner.StateConnected {
		statusCode = http.StatusOK
		notice = fmt.Sprintf("节点 %s 已连接成功，SOCKS5 代理地址 %s", server.HostName, status.SocksListenAddr)
	}

	respond(statusCode, true, notice, "")
}

func (a *App) handleVPNConnectRecommended(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		a.writeActionError(w, r, http.StatusMethodNotAllowed, "仅支持 POST 请求")
		return
	}

	if err := validateSameOriginRequest(r); err != nil {
		a.writeActionError(w, r, http.StatusForbidden, err.Error())
		return
	}

	if err := parseSubmittedForm(r); err != nil {
		a.writeActionError(w, r, http.StatusBadRequest, "读取表单失败")
		return
	}

	query := strings.TrimSpace(r.FormValue("q"))
	selectedCountry := strings.TrimSpace(r.FormValue("country"))

	respond := func(statusCode int, ok bool, notice, flashError string) {
		if wantsJSONResponse(r) {
			a.writeJSON(w, statusCode, actionResponse{OK: ok, Notice: notice, Error: flashError, Reload: ok})
			return
		}

		http.Redirect(w, r, buildIndexURL(notice, flashError, query, selectedCountry), http.StatusSeeOther)
	}

	if a.runner == nil || !a.runner.Enabled() {
		respond(http.StatusServiceUnavailable, false, "", "VPN Runner 未配置，暂时无法建立持久连接")
		return
	}

	fetchCtx, fetchCancel := context.WithTimeout(r.Context(), 30*time.Second)
	servers, err := a.refreshServers(fetchCtx)
	fetchCancel()
	if err != nil {
		respond(http.StatusBadGateway, false, "", "连接前刷新最新节点列表失败，请稍后重试")
		return
	}

	server, ok := selectRecommendedServer(servers, query, selectedCountry)
	if !ok {
		respond(http.StatusNotFound, false, "", "当前筛选条件下没有可用于连接的节点，请调整筛选条件后重试")
		return
	}

	connectCtx, connectCancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer connectCancel()

	status, err := a.runner.Connect(connectCtx, server)
	if err != nil {
		message := fmt.Sprintf("连接推荐节点 %s 失败：%v", server.HostName, err)
		if status.State == runner.StateConnecting || status.State == runner.StateConnected || status.State == runner.StateDisconnecting {
			respond(http.StatusConflict, false, "", message)
			return
		}

		respond(http.StatusInternalServerError, false, "", message)
		return
	}

	statusCode := http.StatusAccepted
	notice := fmt.Sprintf("已开始连接推荐节点 %s，SOCKS5 代理地址 %s", server.HostName, status.SocksListenAddr)
	if status.State == runner.StateConnected {
		statusCode = http.StatusOK
		notice = fmt.Sprintf("推荐节点 %s 已连接成功，SOCKS5 代理地址 %s", server.HostName, status.SocksListenAddr)
	}

	respond(statusCode, true, notice, "")
}

func (a *App) handleVPNDisconnect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		a.writeActionError(w, r, http.StatusMethodNotAllowed, "仅支持 POST 请求")
		return
	}

	if err := validateSameOriginRequest(r); err != nil {
		a.writeActionError(w, r, http.StatusForbidden, err.Error())
		return
	}

	respond := func(statusCode int, ok bool, notice, flashError string) {
		if wantsJSONResponse(r) {
			a.writeJSON(w, statusCode, actionResponse{OK: ok, Notice: notice, Error: flashError, Reload: ok})
			return
		}

		http.Redirect(w, r, buildIndexURL(notice, flashError, "", ""), http.StatusSeeOther)
	}

	if a.runner == nil || !a.runner.Enabled() {
		respond(http.StatusServiceUnavailable, false, "", "VPN Runner 未配置，暂时无法断开连接")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	_, err := a.runner.Disconnect(ctx)
	if err != nil {
		respond(http.StatusInternalServerError, false, "", fmt.Sprintf("断开连接失败：%v", err))
		return
	}

	respond(http.StatusOK, true, "已发送断开连接请求", "")
}

func (a *App) handleVPNStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "仅支持 GET 请求", http.StatusMethodNotAllowed)
		return
	}

	if a.runner == nil || !a.runner.Enabled() {
		a.writeJSON(w, http.StatusServiceUnavailable, actionResponse{OK: false, Error: "VPN Runner 未配置"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	status, err := a.runner.Status(ctx)
	if err != nil {
		a.writeJSON(w, http.StatusBadGateway, actionResponse{OK: false, Error: fmt.Sprintf("获取 VPN 状态失败：%v", err)})
		return
	}

	a.writeJSON(w, http.StatusOK, status)
}

func (a *App) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "仅支持 GET 请求", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"状态":"正常"}`))
}

func (a *App) buildPageData(notice, flashError, query, selectedCountry string) PageData {
	runnerStatus, runnerErr := a.fetchRunnerStatus()
	vpnStatusText, vpnStatusClass, vpnStatusDetail, vpnCurrentNode, vpnCurrentIP, vpnConnectedSince, vpnCanDisconnect := formatVPNStatus(runnerStatus, runnerErr)

	a.mu.RLock()
	servers := append([]vpngate.Server(nil), a.servers...)
	lastUpdated := a.lastUpdated
	lastRefreshDuration := a.lastRefreshDuration
	lastError := a.lastError
	testResults := make(map[string]serverTestState, len(a.testResults))
	maps.Copy(testResults, a.testResults)
	a.mu.RUnlock()

	rows := make([]ServerRow, 0, len(servers))
	countries := make(map[string]struct{})
	countryLabels := make(map[string]string)
	var totalPing int
	var knownPingCount int
	var highestSpeed int64

	normalizedQuery := strings.ToLower(strings.TrimSpace(query))
	normalizedCountry := strings.TrimSpace(selectedCountry)

	for _, server := range servers {
		countries[server.CountryLong] = struct{}{}
		if strings.TrimSpace(server.CountryShort) != "" {
			countryLabels[server.CountryShort] = fmt.Sprintf("%s（%s）", server.CountryLong, server.CountryShort)
		}
	}

	countryOptions := make([]CountryOption, 0, len(countryLabels))
	for value, label := range countryLabels {
		countryOptions = append(countryOptions, CountryOption{Value: value, Label: label})
	}
	sort.Slice(countryOptions, func(i, j int) bool {
		return countryOptions[i].Label < countryOptions[j].Label
	})

	for _, server := range servers {
		if !matchesFilters(server, normalizedQuery, normalizedCountry) {
			continue
		}

		rank := len(rows) + 1
		if server.Ping > 0 {
			totalPing += server.Ping
			knownPingCount++
		}
		if server.Speed > highestSpeed {
			highestSpeed = server.Speed
		}

		testState := formatServerTestState(testResults[serverTestKey(server.HostName, server.IP)])
		isVPNActive := runnerStatus.Current != nil && runnerStatus.Current.HostName == server.HostName && runnerStatus.Current.IP == server.IP && (runnerStatus.State == runner.StateConnecting || runnerStatus.State == runner.StateConnected || runnerStatus.State == runner.StateDisconnecting)

		rows = append(rows, ServerRow{
			ServerKey:   serverTestKey(server.HostName, server.IP),
			Rank:        rank,
			Name:        server.HostName,
			Country:     fmt.Sprintf("%s（%s）", server.CountryLong, server.CountryShort),
			IP:          server.IP,
			Protocol:    "OpenVPN",
			Ping:        formatPing(server.Ping),
			Speed:       formatBitRate(server.Speed),
			Sessions:    formatInt(server.NumVPNSessions),
			Uptime:      formatUptime(server.Uptime),
			Users:       formatInt(server.TotalUsers),
			Traffic:     formatBytes(server.TotalTraffic),
			Operator:    safeText(server.Operator, "未知提供者"),
			Message:     safeText(truncateText(server.Message, 48), "暂无备注"),
			Score:       formatInt(server.Score),
			CountryTag:  server.CountryShort,
			TestStatus:  testState.Status,
			TestClass:   testState.ClassName,
			TestDetail:  testState.Detail,
			IsVPNActive: isVPNActive,
		})
	}

	averagePing := "暂无数据"
	if knownPingCount > 0 {
		averagePing = fmt.Sprintf("%d ms", int(math.Round(float64(totalPing)/float64(knownPingCount))))
	}

	statusText := "数据已就绪"
	statusClass := "status-ok"
	if lastError != "" {
		statusText = "上次刷新失败"
		statusClass = "status-warn"
	}

	updatedAt := "尚未刷新"
	lastUpdatedReadable := "暂无可用数据"
	if !lastUpdated.IsZero() {
		updatedAt = lastUpdated.Format("2006-01-02 15:04:05")
		lastUpdatedReadable = relativeTimeCN(lastUpdated)
	}

	return PageData{
		Title:               "VPNGate 节点管理页面",
		Description:         "用于浏览当前可用的 VPNGate 在线节点，并支持按关键词与国家快速筛选。你现在还可以针对单个节点发起 OpenVPN 测试；测试会在当前服务所在主机上执行，成功握手后自动断开，不再提供一键全测。请确保宿主机已安装 openvpn，并具备创建网络接口所需权限。",
		StatusText:          statusText,
		StatusClass:         statusClass,
		Notice:              notice,
		FlashError:          flashError,
		Error:               lastError,
		Query:               query,
		SelectedCountry:     selectedCountry,
		Countries:           countryOptions,
		UpdatedAt:           updatedAt,
		RefreshDuration:     formatDurationCN(lastRefreshDuration),
		TotalCount:          len(rows),
		SourceCount:         len(servers),
		CountryCount:        len(countries),
		AveragePing:         averagePing,
		HighestSpeed:        formatBitRate(highestSpeed),
		Rows:                rows,
		CurrentYear:         time.Now().Year(),
		SupportsRefresh:     true,
		SupportsVPNControl:  a.runner != nil && a.runner.Enabled(),
		HasData:             len(rows) > 0,
		LastUpdatedReadable: lastUpdatedReadable,
		VPNStatusText:       vpnStatusText,
		VPNStatusClass:      vpnStatusClass,
		VPNStatusDetail:     vpnStatusDetail,
		VPNSocksAddress:     runnerStatus.SocksListenAddr,
		VPNCurrentNode:      vpnCurrentNode,
		VPNCurrentIP:        vpnCurrentIP,
		VPNConnectedSince:   vpnConnectedSince,
		VPNCanDisconnect:    vpnCanDisconnect,
	}
}

func (a *App) findServer(hostName, ip string) (vpngate.Server, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()

	return findServerInList(a.servers, hostName, ip)
}

func findServerInList(servers []vpngate.Server, hostName, ip string) (vpngate.Server, bool) {
	normalizedHostName := strings.TrimSpace(hostName)
	normalizedIP := strings.TrimSpace(ip)

	for _, server := range servers {
		if strings.TrimSpace(server.HostName) == normalizedHostName && strings.TrimSpace(server.IP) == normalizedIP {
			return server, true
		}
	}

	return vpngate.Server{}, false
}

func selectRecommendedServer(servers []vpngate.Server, query, selectedCountry string) (vpngate.Server, bool) {
	normalizedQuery := strings.ToLower(strings.TrimSpace(query))
	normalizedCountry := strings.TrimSpace(selectedCountry)
	rankedServers := make([]vpngate.Server, 0, len(servers))
	for _, server := range servers {
		if !vpngate.IsRecommendedServer(server) {
			continue
		}
		if !matchesFilters(server, normalizedQuery, normalizedCountry) {
			continue
		}
		rankedServers = append(rankedServers, server)
	}
	vpngate.SortServersByRecommendation(rankedServers)

	if len(rankedServers) > 0 {
		return rankedServers[0], true
	}

	return vpngate.Server{}, false
}

func (a *App) setServerTestState(key string, state serverTestState) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.testResults[key] = state
}

func (a *App) writeActionError(w http.ResponseWriter, r *http.Request, statusCode int, message string) {
	if wantsJSONResponse(r) {
		a.writeJSON(w, statusCode, actionResponse{OK: false, Error: message})
		return
	}

	http.Error(w, message, statusCode)
}

func (a *App) writeJSON(w http.ResponseWriter, statusCode int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(statusCode)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		a.logger.Printf("写入 JSON 响应失败：%v", err)
	}
}

func (a *App) fetchRunnerStatus() (runner.Status, error) {
	if a.runner == nil || !a.runner.Enabled() {
		return runner.Status{}, fmt.Errorf("VPN Runner 未配置")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	return a.runner.Status(ctx)
}

func buildIndexURL(notice, flashError, query, selectedCountry string) string {
	values := url.Values{}
	if strings.TrimSpace(notice) != "" {
		values.Set("notice", notice)
	}
	if strings.TrimSpace(flashError) != "" {
		values.Set("error", flashError)
	}
	if strings.TrimSpace(query) != "" {
		values.Set("q", query)
	}
	if strings.TrimSpace(selectedCountry) != "" {
		values.Set("country", selectedCountry)
	}

	if encoded := values.Encode(); encoded != "" {
		return "/?" + encoded
	}

	return "/"
}

func validateSameOriginRequest(r *http.Request) error {
	if origin := strings.TrimSpace(r.Header.Get("Origin")); origin != "" {
		originURL, err := url.Parse(origin)
		if err != nil {
			return fmt.Errorf("请求来源校验失败")
		}

		if !sameOriginHost(originURL.Host, r.Host) {
			return fmt.Errorf("仅允许从当前页面发起操作")
		}

		return nil
	}

	if referer := strings.TrimSpace(r.Header.Get("Referer")); referer != "" {
		refererURL, err := url.Parse(referer)
		if err != nil {
			return fmt.Errorf("请求来源校验失败")
		}

		if !sameOriginHost(refererURL.Host, r.Host) {
			return fmt.Errorf("仅允许从当前页面发起操作")
		}
	}

	return nil
}

func wantsJSONResponse(r *http.Request) bool {
	accept := strings.ToLower(strings.TrimSpace(r.Header.Get("Accept")))
	requestedWith := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Requested-With")))

	return strings.Contains(accept, "application/json") || requestedWith == "fetch"
}

func parseSubmittedForm(r *http.Request) error {
	contentType := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type")))
	if strings.HasPrefix(contentType, "multipart/form-data") {
		return r.ParseMultipartForm(1 << 20)
	}

	return r.ParseForm()
}

func sameOriginHost(left, right string) bool {
	return strings.EqualFold(strings.TrimSpace(left), strings.TrimSpace(right))
}

func serverTestKey(hostName, ip string) string {
	return hostName + "|" + ip
}

func formatServerTestState(state serverTestState) serverTestState {
	if strings.TrimSpace(state.Status) == "" {
		return serverTestState{
			Status:    "未测试",
			ClassName: "test-idle",
			Detail:    "可点击“测试节点”发起单节点 OpenVPN 测试",
		}
	}

	detail := strings.TrimSpace(state.Detail)
	if !state.UpdatedAt.IsZero() {
		detail = strings.TrimSpace(detail)
		if detail != "" {
			detail += " · " + relativeTimeCN(state.UpdatedAt)
		}
	}

	return serverTestState{
		Status:    state.Status,
		ClassName: safeText(state.ClassName, "test-idle"),
		Detail:    detail,
	}
}

func formatVPNStatus(status runner.Status, runnerErr error) (string, string, string, string, string, string, bool) {
	if runnerErr != nil {
		return "VPN Runner 不可用", "status-warn", runnerErr.Error(), "", "", "", false
	}

	socksAddress := strings.TrimSpace(status.SocksListenAddr)
	if socksAddress == "" {
		socksAddress = "未配置"
	}

	vpnNode := ""
	vpnIP := ""
	if status.Current != nil {
		vpnNode = status.Current.HostName
		vpnIP = status.Current.IP
	}

	connectedSince := ""
	if !status.ConnectedAt.IsZero() {
		connectedSince = relativeTimeCN(status.ConnectedAt)
	}

	switch status.State {
	case runner.StateConnected:
		detail := fmt.Sprintf("当前 SOCKS5 代理地址：%s", socksAddress)
		if connectedSince != "" {
			detail += " ｜ 已连接：" + connectedSince
		}
		return "VPN 已连接", "status-ok", detail, vpnNode, vpnIP, connectedSince, true
	case runner.StateConnecting:
		detail := "正在建立 OpenVPN 连接，请稍候"
		if vpnNode != "" {
			detail = fmt.Sprintf("正在连接节点 %s（%s）", vpnNode, safeText(vpnIP, "未知 IP"))
		}
		return "VPN 连接中", "status-warn", detail, vpnNode, vpnIP, connectedSince, true
	case runner.StateDisconnecting:
		return "VPN 断开中", "status-warn", "已发送断开请求，正在等待 OpenVPN 进程退出", vpnNode, vpnIP, connectedSince, true
	case runner.StateFailed:
		detail := safeText(status.LastError, "最近一次连接失败，请查看 Runner 日志")
		return "VPN 连接失败", "status-warn", detail, vpnNode, vpnIP, connectedSince, false
	default:
		detail := fmt.Sprintf("SOCKS5 监听地址：%s ｜ 当前尚未建立 VPN 连接", socksAddress)
		return "VPN 未连接", "status-warn", detail, vpnNode, vpnIP, connectedSince, false
	}
}

func buildServerTestPayload(server vpngate.Server, state serverTestState) *serverTestPayload {
	formatted := formatServerTestState(state)

	return &serverTestPayload{
		Key:       serverTestKey(server.HostName, server.IP),
		HostName:  server.HostName,
		IP:        server.IP,
		Status:    formatted.Status,
		ClassName: formatted.ClassName,
		Detail:    formatted.Detail,
	}
}

func matchesFilters(server vpngate.Server, query, selectedCountry string) bool {
	if selectedCountry != "" && !strings.EqualFold(server.CountryShort, selectedCountry) {
		return false
	}

	if query == "" {
		return true
	}

	searchable := strings.ToLower(strings.Join([]string{
		server.HostName,
		server.IP,
		server.CountryLong,
		server.CountryShort,
		server.Operator,
		server.Message,
	}, " "))

	return strings.Contains(searchable, query)
}

func (a *App) loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		recorder := &responseRecorder{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(recorder, r)

		a.logger.Printf("请求完成：%s %s -> %d（耗时 %s）", r.Method, r.URL.Path, recorder.status, formatDurationCN(time.Since(start)))
	})
}

func sortServers(servers []vpngate.Server) {
	vpngate.SortServersByRecommendation(servers)
}

func formatPing(ping int) string {
	if ping <= 0 {
		return "暂无数据"
	}

	return fmt.Sprintf("%d ms", ping)
}

func formatInt(value int64) string {
	text := fmt.Sprintf("%d", value)
	if len(text) <= 3 {
		return text
	}

	var builder strings.Builder
	remainder := len(text) % 3
	if remainder > 0 {
		builder.WriteString(text[:remainder])
		if len(text) > remainder {
			builder.WriteByte(',')
		}
	}

	for i := remainder; i < len(text); i += 3 {
		builder.WriteString(text[i : i+3])
		if i+3 < len(text) {
			builder.WriteByte(',')
		}
	}

	return builder.String()
}

func formatBitRate(value int64) string {
	if value <= 0 {
		return "暂无数据"
	}

	units := []string{"bps", "Kbps", "Mbps", "Gbps", "Tbps"}
	converted := float64(value)
	unitIndex := 0
	for converted >= 1000 && unitIndex < len(units)-1 {
		converted /= 1000
		unitIndex++
	}

	if unitIndex == 0 {
		return fmt.Sprintf("%d %s", value, units[unitIndex])
	}

	return fmt.Sprintf("%.2f %s", converted, units[unitIndex])
}

func formatBytes(value int64) string {
	if value <= 0 {
		return "暂无数据"
	}

	units := []string{"B", "KB", "MB", "GB", "TB", "PB"}
	converted := float64(value)
	unitIndex := 0
	for converted >= 1024 && unitIndex < len(units)-1 {
		converted /= 1024
		unitIndex++
	}

	if unitIndex == 0 {
		return fmt.Sprintf("%d %s", value, units[unitIndex])
	}

	return fmt.Sprintf("%.2f %s", converted, units[unitIndex])
}

func formatUptime(value int64) string {
	if value <= 0 {
		return "刚刚上线"
	}

	duration := time.Duration(value) * time.Millisecond
	days := duration / (24 * time.Hour)
	duration -= days * 24 * time.Hour
	hours := duration / time.Hour
	duration -= hours * time.Hour
	minutes := duration / time.Minute

	parts := make([]string, 0, 3)
	if days > 0 {
		parts = append(parts, fmt.Sprintf("%d 天", days))
	}
	if hours > 0 {
		parts = append(parts, fmt.Sprintf("%d 小时", hours))
	}
	if minutes > 0 && len(parts) < 2 {
		parts = append(parts, fmt.Sprintf("%d 分钟", minutes))
	}

	if len(parts) == 0 {
		return "不足 1 分钟"
	}

	return strings.Join(parts, " ")
}

func formatDurationCN(duration time.Duration) string {
	if duration <= 0 {
		return "暂无数据"
	}

	if duration < time.Second {
		return fmt.Sprintf("%d 毫秒", duration.Milliseconds())
	}

	if duration < time.Minute {
		return fmt.Sprintf("%.1f 秒", duration.Seconds())
	}

	minutes := int(duration / time.Minute)
	seconds := int((duration % time.Minute) / time.Second)
	return fmt.Sprintf("%d 分 %d 秒", minutes, seconds)
}

func relativeTimeCN(t time.Time) string {
	if t.IsZero() {
		return "暂无数据"
	}

	delta := time.Since(t)
	switch {
	case delta < time.Minute:
		return "刚刚更新"
	case delta < time.Hour:
		return fmt.Sprintf("%d 分钟前", int(delta/time.Minute))
	case delta < 24*time.Hour:
		return fmt.Sprintf("%d 小时前", int(delta/time.Hour))
	default:
		return fmt.Sprintf("%d 天前", int(delta/(24*time.Hour)))
	}
}

func truncateText(value string, maxRunes int) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return ""
	}

	runes := []rune(trimmed)
	if len(runes) <= maxRunes {
		return trimmed
	}

	return string(runes[:maxRunes]) + "…"
}

func safeText(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}

	return value
}
