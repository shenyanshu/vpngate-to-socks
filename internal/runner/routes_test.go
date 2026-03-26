package runner

import (
	"net"
	"testing"
)

func TestApplyBypassRoutesMergesManualAndLocalCIDRs(t *testing.T) {
	r := &Runner{
		bypassCIDRs: []string{"192.168.31.0/24"},
		localCIDRs:  []string{"172.19.0.0/16", "192.168.31.0/24"},
	}

	merged := sanitizeBypassCIDRs(append(append([]string(nil), r.bypassCIDRs...), r.localCIDRs...))
	if len(merged) != 2 {
		t.Fatalf("merged CIDR count = %d, want 2", len(merged))
	}
	if merged[0] != "192.168.31.0/24" || merged[1] != "172.19.0.0/16" {
		t.Fatalf("merged CIDRs = %v, want [192.168.31.0/24 172.19.0.0/16]", merged)
	}
}

func TestNormalizeCIDRReturnsNetworkPrefix(t *testing.T) {
	_, prefix, err := net.ParseCIDR("172.19.0.2/16")
	if err != nil {
		t.Fatalf("ParseCIDR() error = %v", err)
	}

	normalized, err := normalizeCIDR(prefix)
	if err != nil {
		t.Fatalf("normalizeCIDR() error = %v", err)
	}

	if normalized != "172.19.0.0/16" {
		t.Fatalf("normalizeCIDR() = %q, want %q", normalized, "172.19.0.0/16")
	}
}
