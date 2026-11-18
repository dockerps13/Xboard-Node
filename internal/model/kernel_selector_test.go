package model

import "testing"

func TestResolveKernelForNode(t *testing.T) {
	tests := []struct {
		name      string
		node      NodeSpec
		preferred string
		want      string
		wantErr   bool
	}{
		{"keep vless singbox", NodeSpec{Protocol: "vless", Network: "tcp"}, "singbox", "singbox", false},
		{"vless xhttp", NodeSpec{Protocol: "vless", Network: "xhttp"}, "singbox", "xray", false},
		{"vmess xhttp", NodeSpec{Protocol: "vmess", Network: "xhttp"}, "singbox", "xray", false},
		{"unsupported ss transport", NodeSpec{Protocol: "shadowsocks", Network: "xhttp"}, "singbox", "", true},
		{"hysteria forces singbox", NodeSpec{Protocol: "hysteria"}, "xray", "singbox", false},
		{"hysteria2 forces singbox", NodeSpec{Protocol: "hysteria2"}, "xray", "singbox", false},
		{"trojan keeps xray", NodeSpec{Protocol: "trojan"}, "xray", "xray", false},
		{"vmess keeps singbox", NodeSpec{Protocol: "vmess"}, "singbox", "singbox", false},
		{"socks keeps singbox", NodeSpec{Protocol: "socks"}, "singbox", "singbox", false},
		{"http keeps xray", NodeSpec{Protocol: "http"}, "xray", "xray", false},
		{"tuic forces singbox", NodeSpec{Protocol: "tuic"}, "xray", "singbox", false},
		{"anytls forces singbox", NodeSpec{Protocol: "anytls"}, "xray", "singbox", false},
		{"mieru forces singbox", NodeSpec{Protocol: "mieru"}, "xray", "singbox", false},
		{"naive forces singbox", NodeSpec{Protocol: "naive"}, "xray", "singbox", false},
		{"ss obfs rejected", NodeSpec{Protocol: "shadowsocks", Plugin: "obfs"}, "singbox", "", true},
		{"plain ss", NodeSpec{Protocol: "shadowsocks"}, "singbox", "singbox", false},
		{"vless decryption", NodeSpec{Protocol: "vless", Decryption: "mlkem768x25519plus"}, "singbox", "xray", false},
		{"unknown protocol", NodeSpec{Protocol: "unknown"}, "singbox", "", true},
		{"unsupported preferred", NodeSpec{Protocol: "trojan"}, "bad", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveKernelForNode(&tc.node, tc.preferred)
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Fatalf("got (%q, %v), want (%q, error=%v)", got, err, tc.want, tc.wantErr)
			}
		})
	}
}
