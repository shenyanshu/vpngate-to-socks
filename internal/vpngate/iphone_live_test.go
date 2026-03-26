package vpngate

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestFetchIPhoneServersLive(t *testing.T) {
	if os.Getenv("VPNGATE_LIVE_TEST") != "1" {
		t.Skip("set VPNGATE_LIVE_TEST=1 to run the live VPN Gate integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	servers, err := FetchIPhoneServers(ctx, nil)
	if err != nil {
		t.Fatalf("FetchIPhoneServers() error = %v", err)
	}

	if len(servers) == 0 {
		t.Fatal("FetchIPhoneServers() returned no servers")
	}

	first := servers[0]
	if first.HostName == "" {
		t.Fatal("first server HostName is empty")
	}

	if first.IP == "" {
		t.Fatal("first server IP is empty")
	}

	if first.OpenVPNConfigDataBase64 == "" {
		t.Fatal("first server OpenVPNConfigDataBase64 is empty")
	}
}
