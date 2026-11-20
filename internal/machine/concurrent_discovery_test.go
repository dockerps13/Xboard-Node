package machine

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cedar2025/xboard-node/internal/panel"
)

func TestBoundedMachineNodeDiscoveryLimitsConcurrency(t *testing.T) {
	nodes := make([]panel.MachineNode, 12)
	for i := range nodes {
		nodes[i] = panel.MachineNode{ID: i + 1}
	}
	var inFlight, peak, completed atomic.Int32
	runBoundedMachineNodes(context.Background(), nodes, 4, func(_ panel.MachineNode) {
		now := inFlight.Add(1)
		for {
			prev := peak.Load()
			if now <= prev || peak.CompareAndSwap(prev, now) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
		inFlight.Add(-1)
		completed.Add(1)
	})
	if completed.Load() != int32(len(nodes)) {
		t.Fatalf("started only %d of %d nodes", completed.Load(), len(nodes))
	}
	if peak.Load() > 4 || peak.Load() < 2 {
		t.Fatalf("invalid startup concurrency peak: %d", peak.Load())
	}
}

func TestBoundedMachineNodeDiscoveryHonorsCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := false
	runBoundedMachineNodes(ctx, []panel.MachineNode{{ID: 1}}, 4, func(_ panel.MachineNode) {
		started = true
	})
	if started {
		t.Fatal("cancelled machine must not initiate another node request")
	}
}
