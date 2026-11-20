package singbox

import (
	"testing"

	"github.com/cedar2025/xboard-node/internal/kernel"
	"github.com/cedar2025/xboard-node/internal/panel"
)

// This matrix covers configuration generation for every protocol exposed by
// the XBoard panel. It does not claim full network-level interoperability.
func TestXBoardElevenProtocolsGenerateInbounds(t *testing.T) {
	cases := []struct {
		name     string
		protocol string
		version  int
		expected string
	}{
		{"Shadowsocks", "shadowsocks", 0, "shadowsocks"},
		{"VMess", "vmess", 0, "vmess"},
		{"Trojan", "trojan", 0, "trojan"},
		{"VLESS", "vless", 0, "vless"},
		{"Hysteria2 via hysteria", "hysteria", 2, "hysteria2"},
		{"TUIC", "tuic", 0, "tuic"},
		{"AnyTLS", "anytls", 0, "anytls"},
		{"SOCKS", "socks", 0, "socks"},
		{"Naive", "naive", 0, "naive"},
		{"HTTP", "http", 0, "http"},
		{"Mieru", "mieru", 0, "mieru"},
		{"Hysteria2 alias", "hysteria2", 0, "hysteria2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			node := testNodeSpec(&panel.NodeConfig{
				Protocol:   tc.protocol,
				Version:    tc.version,
				ServerPort: 9000,
				Cipher:     "aes-128-gcm",
			})
			got := buildInbound(node, testUsers, kernel.TLSCert{})
			if got == nil {
				t.Fatal("protocol unexpectedly has no inbound builder")
			}
			if actual, ok := got["type"].(string); !ok || actual != tc.expected {
				t.Fatalf("unexpected protocol inbound type: %v, expected %s", got["type"], tc.expected)
			}
			if users, ok := got["users"]; !ok || users == nil {
				t.Fatal("inbound missing user credentials")
			}
		})
	}
}
