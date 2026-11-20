package controlplane

import (
	"context"
	"testing"

	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/panel"
)

func TestMachineInitialReusesPrefetchedNodeConfig(t *testing.T) {
	// Deliberately serve a different config to catch an unnecessary re-fetch.
	server := newPanelTestServer(`{"protocol":"invalid-protocol","server_port":1}`)
	defer server.Close()

	client := panel.NewClient(config.PanelConfig{
		URL: server.URL, Token: "test-machine-token", MachineID: 1, NodeID: 16,
	})
	cp := NewMachinePanelControlPlane(client, config.KernelConfig{Type: "singbox"}, nil, nil)
	cp.SetInitialConfig(&panel.NodeConfig{
		Protocol: "shadowsocks", ServerPort: 8080, Cipher: "aes-128-gcm",
	})

	bootstrap, err := cp.Initial(context.Background(), nil, nil, nil)
	if err != nil {
		t.Fatalf("initial bootstrap with prefetched config failed: %v", err)
	}
	if bootstrap.Config == nil || bootstrap.Config.Protocol != "shadowsocks" || bootstrap.Config.ServerPort != 8080 {
		t.Fatalf("unexpected initial config: %+v", bootstrap.Config)
	}
}
