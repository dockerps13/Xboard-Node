package singbox

import (
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/model"
)

func probeTestPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

func probeDial(t *testing.T, port int, success bool) {
	t.Helper()
	conn, err := net.DialTimeout("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 200*time.Millisecond)
	if err == nil {
		_ = conn.Close()
	}
	if success && err != nil {
		t.Fatalf("expected TCP probe on port %d: %v", port, err)
	}
	if !success && err == nil {
		t.Fatalf("unexpected TCP probe on port %d", port)
	}
}

func TestTCPProbeNodeLifecycle(t *testing.T) {
	s := New(config.KernelConfig{TCPProbe: true})
	port := probeTestPort(t)
	node := &model.NodeSpec{Protocol: "hysteria2", ServerPort: port, ListenIP: "127.0.0.1"}
	s.syncTCPProbe(node)
	if s.tcpProbe == nil {
		t.Fatal("Hysteria2 TCP probe was not created")
	}
	probeDial(t, port, true)
	same := s.tcpProbe
	s.syncTCPProbe(node)
	if s.tcpProbe != same {
		t.Fatal("same-address reload replaced the TCP listener")
	}

	next := probeTestPort(t)
	if next == port {
		t.Skip("ephemeral ports happened to match")
	}
	s.syncTCPProbe(&model.NodeSpec{Protocol: "tuic", ServerPort: next, ListenIP: "127.0.0.1"})
	probeDial(t, port, false)
	probeDial(t, next, true)

	s.syncTCPProbe(&model.NodeSpec{Protocol: "vless", ServerPort: next, ListenIP: "127.0.0.1"})
	if s.tcpProbe != nil {
		t.Fatal("non-QUIC node retained TCP probe")
	}
	probeDial(t, next, false)

	s.syncTCPProbe(node)
	s.Stop()
	probeDial(t, port, false)
}

func TestTCPProbeOnlySupportedProtocols(t *testing.T) {
	for _, tc := range []struct {
		name     string
		protocol string
		version  int
		enabled  bool
	}{
		{"hysteria2", "hysteria2", 0, true},
		{"hysteria-v2", "hysteria", 2, true},
		{"hysteria-v1", "hysteria", 1, false},
		{"tuic", "tuic", 0, true},
		{"trojan", "trojan", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := New(config.KernelConfig{TCPProbe: true})
			p := probeTestPort(t)
			s.syncTCPProbe(&model.NodeSpec{Protocol: tc.protocol, Version: tc.version, ServerPort: p, ListenIP: "127.0.0.1"})
			probeDial(t, p, tc.enabled)
			s.Stop()
			probeDial(t, p, false)
		})
	}
}

func TestTCPProbeDisabledAndPortConflict(t *testing.T) {
	port := probeTestPort(t)
	node := &model.NodeSpec{Protocol: "tuic", ServerPort: port, ListenIP: "127.0.0.1"}
	disabled := New(config.KernelConfig{})
	disabled.syncTCPProbe(node)
	probeDial(t, port, false)

	occupied, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	enabled := New(config.KernelConfig{TCPProbe: true})
	enabled.syncTCPProbe(node)
	if enabled.tcpProbe != nil {
		t.Fatal("probe replaced another TCP listener")
	}
	_ = occupied.Close()
	enabled.syncTCPProbe(node)
	probeDial(t, port, true)
	enabled.Stop()
	probeDial(t, port, false)
}
