package machine

import (
	"testing"

	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/controlplane"
	"github.com/cedar2025/xboard-node/internal/panel"
)

func TestMachineWSEventUsesNodeKernelNotGlobalDefault(t *testing.T) {
	const nodeID = 42
	mailbox := controlplane.NewNodeMailbox()
	mailbox.MarkReady()

	orchestrator := &Orchestrator{
		cfg: &config.Config{Kernel: config.KernelConfig{Type: "singbox"}},
		nodes: map[int]*nodeHandle{
			nodeID: {kernelType: "xray", node: panel.MachineNode{ID: nodeID, Type: "vless"}},
		},
		mailboxes: map[int]*controlplane.NodeMailbox{nodeID: mailbox},
	}
	orchestrator.onWSEvent(panel.WSEvent{
		Type:   panel.WSEventSyncConfig,
		NodeID: nodeID,
		Config: &panel.NodeConfig{
			Protocol: "vless", Network: "xhttp", ServerPort: 12345,
		},
	})
	got := mailbox.DrainIfReady()
	if !got.HasConfig || got.Config == nil {
		t.Fatal("valid Xray-only transport must be accepted for an Xray-backed machine node")
	}
	if got.Config.Network != "xhttp" {
		t.Fatalf("wrong transport: %q", got.Config.Network)
	}
}
