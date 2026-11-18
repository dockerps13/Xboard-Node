package singbox

import (
	"bufio"
	"io"
	"net"
	"net/netip"
	"strconv"
	"testing"
	"time"

	"github.com/cedar2025/xboard-node/internal/kernel/obfs"
	"github.com/sagernet/sing-box/adapter"
	singM "github.com/sagernet/sing/common/metadata"
)

// Check that a TCP session terminated by the HTTP obfs listener is still
// attributed to its original public client IP for device-limit accounting.
func TestObfsFrontendPreservesOriginalDeviceIP(t *testing.T) {
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	front, err := obfs.ListenHTTP("127.0.0.1:0", backend.Addr().String(), "www.bing.com")
	if err != nil {
		t.Fatal(err)
	}
	defer front.Close()

	observed := make(chan string, 1)
	go func() {
		conn, err := backend.Accept()
		if err != nil {
			observed <- "accept error"
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		if _, err := io.ReadFull(conn, make([]byte, 5)); err != nil {
			observed <- "payload error"
			return
		}
		addr := conn.RemoteAddr().(*net.TCPAddr)
		tracker := NewConnTracker(0)
		tracker.SetUserMap(map[string]int{"uuid-1": 1})
		meta := adapter.InboundContext{
			User: "uuid-1",
			Source: singM.Socksaddr{
				Addr: netip.MustParseAddr(addr.IP.String()),
				Port: uint16(addr.Port),
			},
		}
		wrapped := tracker.RoutedConnection(nil, &testConn{}, meta, nil, nil)
		_, ips, _ := tracker.GetUserTraffic()
		if !ips[1]["127.0.0.2"] {
			observed <- "original source IP missing from tracker: " + addr.String()
		} else {
			observed <- ""
		}
		_ = wrapped.Close()
	}()

	// Different local loopback IP makes an incorrect localhost device count
	// visible (without origin recovery it would appear as 127.0.0.1).
	dialer := net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP("127.0.0.2")}, Timeout: 3 * time.Second}
	client, err := dialer.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(front.Addr().(*net.TCPAddr).Port)))
	if err != nil {
		t.Skipf("test loopback IP 127.0.0.2 not available: %v", err)
	}
	defer client.Close()
	_, err = io.WriteString(client, "GET / HTTP/1.1\r\nHost: www.bing.com\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: dGVzdA==\r\n\r\nhello")
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(client)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\r\n" {
			break
		}
	}
	select {
	case msg := <-observed:
		if msg != "" {
			t.Fatal(msg)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("timeout waiting for obfs device IP accounting")
	}
}
