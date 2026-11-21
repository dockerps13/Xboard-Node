package tcpprobe

import (
	"net"
	"testing"
	"time"
)

func TestListenHandshakeAndClose(t *testing.T) {
	p, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := p.listener.Addr().String()
	c, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatalf("handshake failed: %v", err)
	}
	_ = c.Close()
	if _, err := Listen(addr); err == nil {
		t.Fatal("duplicate bind unexpectedly succeeded")
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal("double close:", err)
	}
	if c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
		_ = c.Close()
		t.Fatal("listener still accepted connections after Close")
	}
	p2, err := Listen(addr)
	if err != nil {
		t.Fatalf("port not released: %v", err)
	}
	_ = p2.Close()
}
