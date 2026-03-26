package runner

import "time"

type State string

const (
	StateDisconnected  State = "disconnected"
	StateConnecting    State = "connecting"
	StateConnected     State = "connected"
	StateDisconnecting State = "disconnecting"
	StateFailed        State = "failed"
)

type ConnectionInfo struct {
	HostName     string `json:"hostName"`
	IP           string `json:"ip"`
	CountryLong  string `json:"countryLong,omitempty"`
	CountryShort string `json:"countryShort,omitempty"`
}

type Status struct {
	State           State           `json:"state"`
	Current         *ConnectionInfo `json:"current,omitempty"`
	SocksListenAddr string          `json:"socksListenAddr"`
	LastError       string          `json:"lastError,omitempty"`
	ConnectedAt     time.Time       `json:"connectedAt,omitempty"`
	UpdatedAt       time.Time       `json:"updatedAt"`
	LogTail         []string        `json:"logTail,omitempty"`
}
