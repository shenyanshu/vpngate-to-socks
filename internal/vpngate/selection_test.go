package vpngate

import "testing"

func TestIsRecommendedServer(t *testing.T) {
	tests := []struct {
		name   string
		server Server
		want   bool
	}{
		{name: "valid server", server: Server{OpenVPNConfigDataBase64: "cfg", TotalUsers: 1}, want: true},
		{name: "missing config", server: Server{TotalUsers: 1}, want: false},
		{name: "zero users", server: Server{OpenVPNConfigDataBase64: "cfg", TotalUsers: 0}, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsRecommendedServer(tt.server); got != tt.want {
				t.Fatalf("IsRecommendedServer() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSortServersByRecommendation(t *testing.T) {
	servers := []Server{
		{HostName: "higher-users", TotalUsers: 20, Uptime: 10, NumVPNSessions: 1, Ping: 5, Score: 999, Speed: 999},
		{HostName: "higher-uptime", TotalUsers: 5, Uptime: 30, NumVPNSessions: 1, Ping: 5, Score: 999, Speed: 999},
		{HostName: "higher-sessions", TotalUsers: 5, Uptime: 10, NumVPNSessions: 8, Ping: 5, Score: 999, Speed: 999},
		{HostName: "winner", TotalUsers: 5, Uptime: 10, NumVPNSessions: 2, Ping: 5, Score: 999, Speed: 999},
	}

	SortServersByRecommendation(servers)

	if servers[0].HostName != "winner" {
		t.Fatalf("first host = %q, want %q", servers[0].HostName, "winner")
	}
	if servers[1].HostName != "higher-sessions" {
		t.Fatalf("second host = %q, want %q", servers[1].HostName, "higher-sessions")
	}
	if servers[2].HostName != "higher-uptime" {
		t.Fatalf("third host = %q, want %q", servers[2].HostName, "higher-uptime")
	}
	if servers[3].HostName != "higher-users" {
		t.Fatalf("fourth host = %q, want %q", servers[3].HostName, "higher-users")
	}
}
