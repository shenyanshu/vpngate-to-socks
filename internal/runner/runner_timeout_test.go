package runner

import (
	"os/exec"
	"testing"
	"time"
)

func TestShouldAbortConnectTimeout(t *testing.T) {
	cmd := &exec.Cmd{}

	tests := []struct {
		name                 string
		proc                 *exec.Cmd
		state                State
		disconnectRequested  bool
		connectHandshakeSeen bool
		want                 bool
	}{
		{name: "abort while still connecting", proc: cmd, state: StateConnecting, want: true},
		{name: "ignore different process", proc: &exec.Cmd{}, state: StateConnecting, want: false},
		{name: "ignore connected state", proc: cmd, state: StateConnected, want: false},
		{name: "ignore requested disconnect", proc: cmd, state: StateConnecting, disconnectRequested: true, want: false},
		{name: "ignore observed handshake", proc: cmd, state: StateConnecting, connectHandshakeSeen: true, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &Runner{
				proc:                 tt.proc,
				state:                tt.state,
				disconnectRequested:  tt.disconnectRequested,
				connectHandshakeSeen: tt.connectHandshakeSeen,
			}

			if got := r.shouldAbortConnectTimeout(cmd); got != tt.want {
				t.Fatalf("shouldAbortConnectTimeout() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMarkConnectTimeoutTriggered(t *testing.T) {
	r := &Runner{autoConfig: AutoPilotConfig{OpenVPNConnectTimeout: 10 * time.Second}}

	r.markConnectTimeoutTriggered()

	if !r.connectTimeoutTriggered {
		t.Fatal("connectTimeoutTriggered = false, want true")
	}
}
