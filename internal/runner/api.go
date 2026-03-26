package runner

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"vpngate/internal/vpngate"
)

type connectRequest struct {
	Server vpngate.Server `json:"server"`
}

type connectResponse struct {
	Status Status `json:"status"`
	Error  string `json:"error,omitempty"`
}

type testResponse struct {
	Result vpngate.OpenVPNTestResult `json:"result"`
	Error  string                    `json:"error,omitempty"`
}

func NewAPIHandler(logger *log.Logger, runner *Runner) http.Handler {
	if logger == nil {
		logger = log.Default()
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "仅支持 GET 请求", http.StatusMethodNotAllowed)
			return
		}

		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "仅支持 GET 请求", http.StatusMethodNotAllowed)
			return
		}

		writeJSON(w, http.StatusOK, runner.Status())
	})

	mux.HandleFunc("/connect", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "仅支持 POST 请求", http.StatusMethodNotAllowed)
			return
		}

		var req connectRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, connectResponse{Status: runner.Status(), Error: "读取连接请求失败"})
			return
		}

		if req.Server.HostName == "" || req.Server.IP == "" || req.Server.OpenVPNConfigDataBase64 == "" {
			writeJSON(w, http.StatusBadRequest, connectResponse{Status: runner.Status(), Error: "缺少必要节点信息，无法建立连接"})
			return
		}

		if err := runner.Connect(req.Server); err != nil {
			statusCode := http.StatusInternalServerError
			if runner.Status().State == StateConnecting || runner.Status().State == StateConnected || runner.Status().State == StateDisconnecting {
				statusCode = http.StatusConflict
			}
			writeJSON(w, statusCode, connectResponse{Status: runner.Status(), Error: err.Error()})
			return
		}

		logger.Printf("控制接口已接受连接请求：%s（%s）", req.Server.HostName, req.Server.IP)
		writeJSON(w, http.StatusAccepted, connectResponse{Status: runner.Status()})
	})

	mux.HandleFunc("/test", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "仅支持 POST 请求", http.StatusMethodNotAllowed)
			return
		}

		var req connectRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, testResponse{Error: "读取测试请求失败"})
			return
		}

		if req.Server.HostName == "" || req.Server.IP == "" || req.Server.OpenVPNConfigDataBase64 == "" {
			writeJSON(w, http.StatusBadRequest, testResponse{Error: "缺少必要节点信息，无法执行测试"})
			return
		}

		result, err := runner.TestServer(r.Context(), req.Server)
		if err != nil {
			statusCode := http.StatusInternalServerError
			status := runner.Status().State
			if status == StateConnecting || status == StateConnected || status == StateDisconnecting {
				statusCode = http.StatusConflict
			}
			writeJSON(w, statusCode, testResponse{Result: result, Error: err.Error()})
			return
		}

		writeJSON(w, http.StatusOK, testResponse{Result: result})
	})

	mux.HandleFunc("/disconnect", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "仅支持 POST 请求", http.StatusMethodNotAllowed)
			return
		}

		if err := runner.Disconnect(); err != nil {
			writeJSON(w, http.StatusInternalServerError, connectResponse{Status: runner.Status(), Error: fmt.Sprintf("断开连接失败: %v", err)})
			return
		}

		logger.Println("控制接口已接受断开连接请求")
		writeJSON(w, http.StatusOK, connectResponse{Status: runner.Status()})
	})

	return loggingMiddleware(logger, mux)
}

func loggingMiddleware(logger *log.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		logger.Printf("Runner API 请求完成：%s %s（耗时 %s）", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
	})
}

func writeJSON(w http.ResponseWriter, statusCode int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(payload)
}
