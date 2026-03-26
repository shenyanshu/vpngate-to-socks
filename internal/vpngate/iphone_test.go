package vpngate

import (
	"strings"
	"testing"
)

const sampleResponse = `*vpn_servers
#HostName,IP,Score,Ping,Speed,CountryLong,CountryShort,NumVpnSessions,Uptime,TotalUsers,TotalTraffic,LogType,Operator,Message,OpenVPN_ConfigData_Base64
public-vpn-1,1.2.3.4,100,10,200,Japan,JP,2,300,4,500,2weeks,Operator One,,dGVzdA==
public-vpn-2,5.6.7.8,101,11,201,Korea Republic of,KR,3,301,5,501,2weeks,Operator Two,hello,d29ybGQ=
*`

func TestParseIPhoneResponse(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantLen int
		wantErr string
	}{
		{
			name:    "valid response",
			input:   sampleResponse,
			wantLen: 2,
		},
		{
			name:    "invalid marker",
			input:   strings.Replace(sampleResponse, "*vpn_servers", "*wrong", 1),
			wantErr: "响应标记不正确",
		},
		{
			name:    "invalid number",
			input:   strings.Replace(sampleResponse, ",100,", ",abc,", 1),
			wantErr: "解析 Score 失败",
		},
		{
			name:    "dash ping is allowed",
			input:   strings.Replace(sampleResponse, ",10,", ",- ,", 1),
			wantLen: 2,
		},
		{
			name:    "missing rows",
			input:   "*vpn_servers\n#HostName,IP,Score,Ping,Speed,CountryLong,CountryShort,NumVpnSessions,Uptime,TotalUsers,TotalTraffic,LogType,Operator,Message,OpenVPN_ConfigData_Base64\n*",
			wantErr: "响应中没有任何节点数据",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			servers, err := ParseIPhoneResponse(strings.NewReader(tt.input))
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.wantErr)
				}

				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("expected error containing %q, got %q", tt.wantErr, err.Error())
				}

				return
			}

			if err != nil {
				t.Fatalf("ParseIPhoneResponse() error = %v", err)
			}

			if len(servers) != tt.wantLen {
				t.Fatalf("len(servers) = %d, want %d", len(servers), tt.wantLen)
			}
		})
	}
}

func TestParseIPhoneResponseMapsUnknownPing(t *testing.T) {
	input := strings.Replace(sampleResponse, ",10,", ",- ,", 1)
	input = strings.ReplaceAll(input, "- ,", "-,")

	servers, err := ParseIPhoneResponse(strings.NewReader(input))
	if err != nil {
		f := "ParseIPhoneResponse() error = %v"
		t.Fatalf(f, err)
	}

	if servers[0].Ping != unknownPing {
		t.Fatalf("Ping = %d, want %d", servers[0].Ping, unknownPing)
	}
}

func TestDecodeOpenVPNConfig(t *testing.T) {
	servers, err := ParseIPhoneResponse(strings.NewReader(sampleResponse))
	if err != nil {
		t.Fatalf("ParseIPhoneResponse() error = %v", err)
	}

	decoded, err := servers[0].DecodeOpenVPNConfig()
	if err != nil {
		t.Fatalf("DecodeOpenVPNConfig() error = %v", err)
	}

	if decoded != "test" {
		t.Fatalf("decoded config = %q, want %q", decoded, "test")
	}
}

func TestParseIPhoneResponseMapsFields(t *testing.T) {
	servers, err := ParseIPhoneResponse(strings.NewReader(sampleResponse))
	if err != nil {
		t.Fatalf("ParseIPhoneResponse() error = %v", err)
	}

	first := servers[0]
	if first.HostName != "public-vpn-1" {
		t.Fatalf("HostName = %q, want %q", first.HostName, "public-vpn-1")
	}

	if first.IP != "1.2.3.4" {
		t.Fatalf("IP = %q, want %q", first.IP, "1.2.3.4")
	}

	if first.Score != 100 {
		t.Fatalf("Score = %d, want %d", first.Score, 100)
	}

	if first.Ping != 10 {
		t.Fatalf("Ping = %d, want %d", first.Ping, 10)
	}

	if first.Operator != "Operator One" {
		t.Fatalf("Operator = %q, want %q", first.Operator, "Operator One")
	}

	if first.Message != "" {
		t.Fatalf("Message = %q, want empty string", first.Message)
	}

	if first.OpenVPNConfigDataBase64 != "dGVzdA==" {
		t.Fatalf("OpenVPNConfigDataBase64 = %q, want %q", first.OpenVPNConfigDataBase64, "dGVzdA==")
	}
}
