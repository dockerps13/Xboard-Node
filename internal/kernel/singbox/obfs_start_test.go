package singbox

import (
	"bufio"
	"context"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	shadowsocks "github.com/sagernet/sing-shadowsocks2"
	Mdata "github.com/sagernet/sing/common/metadata"

	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/kernel"
	"github.com/cedar2025/xboard-node/internal/model"
)

// This integration test starts the embedded sing-box core and the Go HTTP
// simple-obfs frontend together; unlike the config-only tests it checks
// that the local listeners actually bind and accept the obfs HTTP preface.
func TestSingBoxStartsHTTPObfsWithNativeUDP(t *testing.T) {
	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := reservation.Addr().(*net.TCPAddr).Port
	_ = reservation.Close()

	server := New(config.KernelConfig{Type: "singbox", LogLevel: "error", ConfigDir: t.TempDir()})
	node := &model.NodeSpec{
		Protocol:   "shadowsocks",
		ListenIP:   "127.0.0.1",
		ServerPort: port,
		Cipher:     "aes-128-gcm",
		Plugin:     "obfs",
		PluginOpt:  "obfs=http;obfs-host=www.bing.com;path=/",
	}
	users := []model.UserSpec{{ID: 1, UUID: "test-ss-password-1"}}
	if err := server.Start(node, users, kernel.TLSCert{}); err != nil {
		t.Fatalf("failed to launch actual sing-box SS+obfs node: %v", err)
	}
	defer server.Stop()

	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	_, err = conn.Write([]byte("GET / HTTP/1.1\r\nHost: www.bing.com\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Key: dGVzdA==\r\n\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil || !strings.Contains(line, "101 Switching Protocols") {
		t.Fatalf("obfs frontend did not upgrade the connection: %q, %v", line, err)
	}
	_ = conn.Close()

	// A configuration update must not lose the public HTTP obfs port.
	if err := server.Reload(node, users, kernel.TLSCert{}); err != nil {
		t.Fatalf("HTTP obfs reload must preserve the listener: %v", err)
	}
	if !server.IsRunning() {
		t.Fatal("kernel went offline during SS obfs reload")
	}
}

func TestSingBoxHTTPObfsEncryptsTCPAndNativeUDP(t *testing.T) {
	tcpTarget, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tcpTarget.Close()
	tcpDone := make(chan error, 1)
	go func() {
		conn, acceptErr := tcpTarget.Accept()
		if acceptErr != nil {
			tcpDone <- acceptErr
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		body := make([]byte, len("hello"))
		if _, readErr := io.ReadFull(conn, body); readErr != nil {
			tcpDone <- readErr
			return
		}
		if string(body) != "hello" {
			tcpDone <- &unexpectedPayloadError{want: "hello", got: string(body)}
			return
		}
		_, writeErr := io.WriteString(conn, "world")
		tcpDone <- writeErr
	}()

	udpTarget, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		tcpTarget.Close()
		t.Fatal(err)
	}
	defer udpTarget.Close()
	udpDone := make(chan error, 1)
	go func() {
		buf := make([]byte, 256)
		n, addr, readErr := udpTarget.ReadFrom(buf)
		if readErr != nil {
			udpDone <- readErr
			return
		}
		if string(buf[:n]) != "udp-hello" {
			udpDone <- &unexpectedPayloadError{want: "udp-hello", got: string(buf[:n])}
			return
		}
		_, writeErr := udpTarget.WriteTo([]byte("udp-world"), addr)
		udpDone <- writeErr
	}()

	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := reservation.Addr().(*net.TCPAddr).Port
	_ = reservation.Close()
	const password = "test-ss-password-1"
	server := New(config.KernelConfig{
		Type:        "singbox",
		LogLevel:    "error",
		ConfigDir:   t.TempDir(),
		CustomRoute: []map[string]any{{"ip_cidr": []string{"127.0.0.1/32"}, "outbound": "direct"}},
	})
	node := &model.NodeSpec{
		Protocol:   "shadowsocks",
		ListenIP:   "127.0.0.1",
		ServerPort: port,
		Cipher:     "aes-128-gcm",
		Plugin:     "obfs",
		PluginOpt:  "obfs=http;obfs-host=www.bing.com;path=/",
	}
	if err := server.Start(node, []model.UserSpec{{ID: 1, UUID: password}}, kernel.TLSCert{}); err != nil {
		t.Fatalf("failed to launch actual sing-box SS+obfs node: %v", err)
	}
	defer server.Stop()

	method, err := shadowsocks.CreateMethod(context.Background(), node.Cipher, shadowsocks.MethodOptions{Password: password})
	if err != nil {
		t.Fatal(err)
	}

	tcpConn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = tcpConn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(tcpConn, "GET / HTTP/1.1\r\nHost: www.bing.com\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Key: dGVzdA==\r\n\r\n"); err != nil {
		tcpConn.Close()
		t.Fatal(err)
	}
	response := bufio.NewReader(tcpConn)
	line, err := response.ReadString('\n')
	if err != nil || !strings.Contains(line, "101 Switching Protocols") {
		tcpConn.Close()
		t.Fatalf("obfs frontend did not upgrade encrypted client: %q, %v", line, err)
	}
	for {
		line, err = response.ReadString('\n')
		if err != nil {
			tcpConn.Close()
			t.Fatal(err)
		}
		if line == "\r\n" {
			break
		}
	}
	encrypted, err := method.DialConn(tcpConn, Mdata.ParseSocksaddr(tcpTarget.Addr().String()))
	if err != nil {
		tcpConn.Close()
		t.Fatal(err)
	}
	if _, err := io.WriteString(encrypted, "hello"); err != nil {
		tcpConn.Close()
		t.Fatal(err)
	}
	got := make([]byte, len("world"))
	if _, err := io.ReadFull(encrypted, got); err != nil || string(got) != "world" {
		tcpConn.Close()
		t.Fatalf("encrypted TCP obfs roundtrip got %q: %v", got, err)
	}
	_ = encrypted.Close()
	if err := <-tcpDone; err != nil {
		t.Fatal(err)
	}

	udpConn, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: port})
	if err != nil {
		t.Fatal(err)
	}
	defer udpConn.Close()
	packet := method.DialPacketConn(udpConn)
	if _, err := packet.WriteTo([]byte("udp-hello"), udpTarget.LocalAddr()); err != nil {
		t.Fatal(err)
	}
	_ = packet.SetReadDeadline(time.Now().Add(5 * time.Second))
	udpReply := make([]byte, 256)
	n, _, err := packet.ReadFrom(udpReply)
	if err != nil || string(udpReply[:n]) != "udp-world" {
		t.Fatalf("encrypted UDP obfs roundtrip got %q: %v", udpReply[:n], err)
	}
	if err := <-udpDone; err != nil {
		t.Fatal(err)
	}
}

type unexpectedPayloadError struct {
	want string
	got  string
}

func (e *unexpectedPayloadError) Error() string {
	return "unexpected payload: want " + e.want + ", got " + e.got
}
