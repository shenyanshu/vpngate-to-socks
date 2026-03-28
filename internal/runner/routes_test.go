package runner

import (
	"net"
	"testing"
)

func TestBuildBypassRouteSpecsUsesGatewayForManualAndDirectForLocal(t *testing.T) {
	specs := buildBypassRouteSpecs(
		[]string{"192.168.31.0/24"},
		[]localBypassRoute{{CIDR: "172.19.0.0/16", Interface: "docker0"}, {CIDR: "192.168.31.0/24", Interface: "eth0"}},
		"192.168.31.1",
		"eth0",
	)

	if len(specs) != 3 {
		t.Fatalf("spec count = %d, want 3", len(specs))
	}

	if specs[0] != (bypassRouteSpec{CIDR: "192.168.31.0/24", Gateway: "192.168.31.1", Interface: "eth0"}) {
		t.Fatalf("spec[0] = %#v, want manual gateway route", specs[0])
	}

	if specs[1] != (bypassRouteSpec{CIDR: "172.19.0.0/16", Interface: "docker0", Direct: true}) {
		t.Fatalf("spec[1] = %#v, want local direct docker route", specs[1])
	}

	if specs[2] != (bypassRouteSpec{CIDR: "192.168.31.0/24", Interface: "eth0", Direct: true}) {
		t.Fatalf("spec[2] = %#v, want local direct eth0 route", specs[2])
	}
}

func TestBuildRouteReplaceArgsUsesScopeLinkForDirectRoutes(t *testing.T) {
	args := buildRouteReplaceArgs(bypassRouteSpec{CIDR: "172.19.0.0/16", Interface: "docker0", Direct: true})
	want := []string{"route", "replace", "172.19.0.0/16", "dev", "docker0", "scope", "link"}

	if len(args) != len(want) {
		t.Fatalf("arg count = %d, want %d (%v)", len(args), len(want), args)
	}

	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("args[%d] = %q, want %q; full args = %v", i, args[i], want[i], args)
		}
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

func TestSanitizeLocalBypassRoutesDeduplicatesByInterfaceAndCIDR(t *testing.T) {
	routes := sanitizeLocalBypassRoutes([]localBypassRoute{
		{CIDR: "172.19.0.0/16", Interface: "docker0"},
		{CIDR: "172.19.0.0/16", Interface: "docker0"},
		{CIDR: "172.19.0.0/16", Interface: "eth0"},
		{CIDR: " ", Interface: "eth1"},
	})

	if len(routes) != 2 {
		t.Fatalf("route count = %d, want 2", len(routes))
	}

	if routes[0] != (localBypassRoute{CIDR: "172.19.0.0/16", Interface: "docker0"}) {
		t.Fatalf("routes[0] = %#v, want docker0 route", routes[0])
	}

	if routes[1] != (localBypassRoute{CIDR: "172.19.0.0/16", Interface: "eth0"}) {
		t.Fatalf("routes[1] = %#v, want eth0 route", routes[1])
	}
}
