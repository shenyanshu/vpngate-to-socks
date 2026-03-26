package runner

import (
	"testing"
	"time"
)

func TestMarkQuarantineDoesNotBlockStatusFlow(t *testing.T) {
	r := &Runner{
		state:      StateConnecting,
		quarantine: make(map[string]nodeHealth),
		autoConfig: AutoPilotConfig{BaseQuarantine: time.Minute}.withDefaults(),
	}

	done := make(chan struct{})
	go func() {
		r.markQuarantine(ConnectionInfo{HostName: "vpn", IP: "1.1.1.1"}, "timeout")
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("markQuarantine() blocked unexpectedly")
	}

	status := r.Status()
	if status.State != StateConnecting {
		t.Fatalf("Status().State = %q, want %q", status.State, StateConnecting)
	}
}
