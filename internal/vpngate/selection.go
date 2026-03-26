package vpngate

import (
	"sort"
	"strings"
)

// IsRecommendedServer reports whether a server is eligible for automatic/recommended connection.
// Servers without OpenVPN config or without any observed online users are skipped.
func IsRecommendedServer(server Server) bool {
	return strings.TrimSpace(server.OpenVPNConfigDataBase64) != "" && server.TotalUsers > 0
}

// SortServersByRecommendation sorts servers by connection preference.
// Lower total users, shorter uptime, and fewer active VPN sessions are preferred.
// Remaining fields act as deterministic fallbacks.
func SortServersByRecommendation(servers []Server) {
	sort.SliceStable(servers, func(i, j int) bool {
		if servers[i].TotalUsers != servers[j].TotalUsers {
			return servers[i].TotalUsers < servers[j].TotalUsers
		}

		if servers[i].Uptime != servers[j].Uptime {
			return servers[i].Uptime < servers[j].Uptime
		}

		if servers[i].NumVPNSessions != servers[j].NumVPNSessions {
			return servers[i].NumVPNSessions < servers[j].NumVPNSessions
		}

		if servers[i].Ping != servers[j].Ping {
			if servers[i].Ping <= 0 {
				return false
			}
			if servers[j].Ping <= 0 {
				return true
			}

			return servers[i].Ping < servers[j].Ping
		}

		if servers[i].Score != servers[j].Score {
			return servers[i].Score > servers[j].Score
		}

		if servers[i].Speed != servers[j].Speed {
			return servers[i].Speed > servers[j].Speed
		}

		return servers[i].HostName < servers[j].HostName
	})
}
