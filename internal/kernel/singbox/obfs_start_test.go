package singbox

import (
	"bufio"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

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
		Protocol: "shadowsocks",
		ListenIP: "127.0.0.1",
		ServerPort: port,
		Cipher: "aes-128-gcm",
		Plugin: "obfs",
		PluginOpt: "obfs=http;obfs-host=www.bing.com;path=/",
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
