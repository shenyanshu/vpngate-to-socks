package vpngate

import (
	"strings"
	"testing"
)

const sampleOpenVPNConfig = `# comment
client
dev tun
proto tcp
remote 1.2.3.4 443
cipher AES-128-CBC
auth SHA1
resolv-retry infinite
nobind
persist-key
persist-tun
verb 3
<ca>
CA-DATA
</ca>
<cert>
CERT-DATA
</cert>
<key>
KEY-DATA
</key>
`

func TestSanitizeOpenVPNConfig(t *testing.T) {
	sanitized, cipher, err := sanitizeOpenVPNConfig(sampleOpenVPNConfig)
	if err != nil {
		t.Fatalf("sanitizeOpenVPNConfig() error = %v", err)
	}

	if cipher != "AES-128-CBC" {
		t.Fatalf("cipher = %q, want %q", cipher, "AES-128-CBC")
	}

	for _, want := range []string{
		"client",
		"dev tun",
		"proto tcp",
		"remote 1.2.3.4 443",
		"<ca>",
		"</key>",
	} {
		if !strings.Contains(sanitized, want) {
			t.Fatalf("sanitized config does not contain %q", want)
		}
	}

	if strings.Contains(sanitized, "# comment") {
		t.Fatal("sanitized config should not contain comments")
	}
}

func TestSanitizeOpenVPNConfigRejectsUnsafeDirective(t *testing.T) {
	unsafeConfig := sampleOpenVPNConfig + "up /tmp/test.sh\n"

	_, _, err := sanitizeOpenVPNConfig(unsafeConfig)
	if err == nil {
		t.Fatal("expected sanitizeOpenVPNConfig() to reject unsafe directive")
	}

	if !strings.Contains(err.Error(), "不安全指令") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestBuildOpenVPNConnectArgsUsesReferenceCompatibleFlags(t *testing.T) {
	args := BuildOpenVPNConnectArgs("/tmp/example.ovpn", "")

	assertArgValue(t, args, "--data-ciphers", openVPNDefaultCipher)
	assertArgAbsent(t, args, "--connect-timeout")
	assertArgAbsent(t, args, "--connect-retry-max")
}

func TestBuildOpenVPNTestArgsIncludesFastFailTimeout(t *testing.T) {
	args := BuildOpenVPNTestArgs("/tmp/example.ovpn", "")

	assertArgValue(t, args, "--connect-timeout", "10")
	assertArgValue(t, args, "--connect-retry-max", "3")
}

func assertArgValue(t *testing.T, args []string, key, value string) {
	t.Helper()

	for i := 0; i < len(args)-1; i++ {
		if args[i] != key {
			continue
		}

		if args[i+1] != value {
			t.Fatalf("arg %s value = %q, want %q", key, args[i+1], value)
		}
		return
	}

	t.Fatalf("arg %s not found in %v", key, args)
}

func assertArgAbsent(t *testing.T, args []string, key string) {
	t.Helper()

	for _, arg := range args {
		if arg == key {
			t.Fatalf("arg %s unexpectedly found in %v", key, args)
		}
	}
}
