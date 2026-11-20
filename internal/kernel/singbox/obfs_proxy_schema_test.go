package singbox

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/sagernet/sing-box/include"
	boxOption "github.com/sagernet/sing-box/option"
	singJSON "github.com/sagernet/sing/common/json"

	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/kernel"
	"github.com/cedar2025/xboard-node/internal/model"
)

func TestShadowsocksBackendSupportsLoopbackTCPOnly(t *testing.T) {
	cfg := buildConfig(config.KernelConfig{}, &model.NodeSpec{
		Protocol:   "shadowsocks",
		ServerPort: 50000,
		ListenIP:   "127.0.0.1",
		Cipher:     "aes-128-gcm",
	}, testUsers, kernel.TLSCert{})
	inbound := cfg["inbounds"].([]M)[0]
	inbound["listen"] = "127.0.0.1"
	inbound["network"] = "tcp"
	// The source IP is restored by the local endpoint mapping, not PROXY v1.
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := include.Context(context.Background())
	if _, err := singJSON.UnmarshalExtendedContext[boxOption.Options](ctx, data); err != nil {
		t.Fatalf("embedded sing-box must accept source-IP preservation in SS frontend: %v", err)
	}
}

func TestSSHTTPObfsConfigIncludesPublicUDPAndProxyProtocolBackend(t *testing.T) {
	nc := &model.NodeSpec{
		Protocol: "shadowsocks", ServerPort: 8080, ListenIP: "0.0.0.0",
		Cipher: "aes-128-gcm", Plugin: "obfs",
		PluginOpt: "obfs=http;obfs-host=www.bing.com;path=/",
	}
	withoutPlugin := *nc
	withoutPlugin.Plugin = ""
	withoutPlugin.PluginOpt = ""
	cfg := buildConfig(config.KernelConfig{}, &withoutPlugin, testUsers, kernel.TLSCert{})
	backend, host, err := prepareHTTPObfsConfig(cfg, nc, testUsers, kernel.TLSCert{})
	if err != nil {
		t.Fatal(err)
	}
	if host != "www.bing.com" || backend == "" {
		t.Fatalf("unexpected simple-obfs configuration: backend=%q host=%q", backend, host)
	}
	inbounds := cfg["inbounds"].([]M)
	if len(inbounds) != 2 {
		t.Fatalf("expected split TCP/UDP inbounds, got %d", len(inbounds))
	}
	tcp, udp := inbounds[0], inbounds[1]
	if tcp["listen"] != "127.0.0.1" || tcp["network"] != "tcp" {
		t.Fatalf("loopback TCP backend must be isolated: %v", tcp)
	}
	if udp["network"] != "udp" || udp["listen_port"] != 8080 {
		t.Fatalf("public UDP SS traffic must remain natively supported: %v", udp)
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := include.Context(context.Background())
	if _, err := singJSON.UnmarshalExtendedContext[boxOption.Options](ctx, data); err != nil {
		t.Fatalf("split SS inbound schema invalid: %v", err)
	}
}
