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

func TestMachineWSEventDropsStaleConfig(t *testing.T) {
	const nodeID = 43
	mailbox := controlplane.NewNodeMailbox()
	mailbox.MarkReady()
	orchestrator := &Orchestrator{
		cfg: &config.Config{Kernel: config.KernelConfig{Type: "singbox"}},
		nodes: map[int]*nodeHandle{
			nodeID: {kernelType: "singbox", revision: 9, node: panel.MachineNode{ID: nodeID, Type: "vless"}},
		},
		mailboxes: map[int]*controlplane.NodeMailbox{nodeID: mailbox},
	}
	orchestrator.onWSEvent(panel.WSEvent{
		Type: panel.WSEventSyncConfig, NodeID: nodeID,
		Config: &panel.NodeConfig{
			RuntimeRevision: 8, Protocol: "vless", Network: "tcp", ServerPort: 443,
		},
	})
	if state := mailbox.DrainIfReady(); state.HasConfig {
		t.Fatal("stale config pushed by panel must not replace the active node")
	}
}

func TestInvalidWSPushDoesNotAdvanceNodeRevision(t *testing.T) {
	const nodeID = 44
	mailbox := controlplane.NewNodeMailbox()
	mailbox.MarkReady()
	handle := &nodeHandle{kernelType: "singbox", revision: 7, node: panel.MachineNode{ID: nodeID, Type: "shadowsocks"}}
	orchestrator := &Orchestrator{
		cfg:       &config.Config{Kernel: config.KernelConfig{Type: "singbox"}},
		nodes:     map[int]*nodeHandle{nodeID: handle},
		mailboxes: map[int]*controlplane.NodeMailbox{nodeID: mailbox},
	}
	// Unsupported OBFS with a higher revision must not poison the revision
	// cursor and block a subsequent valid update.
	orchestrator.onWSEvent(panel.WSEvent{
		Type: panel.WSEventSyncConfig, NodeID: nodeID,
		Config: &panel.NodeConfig{RuntimeRevision: 10, Protocol: "shadowsocks", ServerPort: 8080, Plugin: "obfs"},
	})
	if handle.revision != 7 {
		t.Fatalf("invalid update unexpectedly advanced revision to %d", handle.revision)
	}
	orchestrator.onWSEvent(panel.WSEvent{
		Type: panel.WSEventSyncConfig, NodeID: nodeID,
		Config: &panel.NodeConfig{RuntimeRevision: 9, Protocol: "shadowsocks", ServerPort: 8080, Cipher: "aes-128-gcm"},
	})
	if state := mailbox.DrainIfReady(); !state.HasConfig {
		t.Fatal("valid update was lost after rejecting invalid higher revision")
	}
	if handle.revision != 9 {
		t.Fatalf("valid update did not advance revision, got %d", handle.revision)
	}
}

func TestFailedNodeHandleCleanupPreservesReplacement(t *testing.T) {
	const nodeID = 51
	oldDone := make(chan struct{})
	newDone := make(chan struct{})
	orchestrator := &Orchestrator{nodes: map[int]*nodeHandle{
		nodeID: {done: oldDone},
	}}
	orchestrator.removeHandleIfCurrent(nodeID, oldDone)
	if _, found := orchestrator.nodes[nodeID]; found {
		t.Fatal("failed node must be removed so machine rediscovery can retry")
	}
	orchestrator.nodes[nodeID] = &nodeHandle{done: newDone}
	orchestrator.removeHandleIfCurrent(nodeID, oldDone)
	if orchestrator.nodes[nodeID] == nil || orchestrator.nodes[nodeID].done != newDone {
		t.Fatal("old generation cleanup must not delete replacement service")
	}
}
