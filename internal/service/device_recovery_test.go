package service

import (
	"testing"

	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/tracker"
)

func TestDeviceRecoveryAfterRestart(t *testing.T) {
	cfg := &config.Config{Kernel: config.KernelConfig{ConfigDir: t.TempDir()}}
	cfg.Panel.NodeID = 14
	first, err := openDeviceReportState(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.update(map[int][]string{2: {"192.0.2.1"}, 3: {"192.0.2.2"}}, false); err != nil {
		t.Fatal(err)
	}
	second, err := openDeviceReportState(cfg)
	if err != nil {
		t.Fatal(err)
	}
	tr := tracker.New()
	tr.RestoreAliveIPs(second.snapshot())
	tr.Process(nil, map[int]map[string]bool{3: {"192.0.2.3": true}}, 1)
	alive := tr.FlushAliveIPs()
	if ips, ok := alive[2]; !ok || ips == nil || len(ips) != 0 {
		t.Fatalf("missing recovered offline notice: %v", alive)
	}
	if len(alive[3]) != 1 || alive[3][0] != "192.0.2.3" {
		t.Fatalf("stale address recovered: %v", alive)
	}
	if err := second.update(alive, true); err != nil {
		t.Fatal(err)
	}
	third, err := openDeviceReportState(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if third.users[2] || !third.users[3] {
		t.Fatalf("wrong recovered users: %v", third.users)
	}
	cfg.Panel.NodeID = 16
	other, err := openDeviceReportState(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(other.users) != 0 {
		t.Fatal("device state leaked across nodes")
	}
}
