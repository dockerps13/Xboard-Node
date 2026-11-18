package singbox

import (
	"context"
	"encoding/json"
	"testing"

	boxOption "github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing-box/include"
	singJSON "github.com/sagernet/sing/common/json"

	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/kernel"
	"github.com/cedar2025/xboard-node/internal/model"
)

func TestShadowsocksBackendSupportsProxyProtocolWithTCPOnly(t *testing.T) {
	cfg := buildConfig(config.KernelConfig{}, &model.NodeSpec{
		Protocol: "shadowsocks",
		ServerPort: 50000,
		ListenIP: "127.0.0.1",
		Cipher: "aes-128-gcm",
	}, testUsers, kernel.TLSCert{})
	inbound := cfg["inbounds"].([]M)[0]
	inbound["listen"] = "127.0.0.1"
	inbound["network"] = "tcp"
	inbound["proxy_protocol"] = true
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := include.Context(context.Background())
	if _, err := singJSON.UnmarshalExtendedContext[boxOption.Options](ctx, data); err != nil {
		t.Fatalf("embedded sing-box must accept source-IP preservation in SS frontend: %v", err)
	}
}
