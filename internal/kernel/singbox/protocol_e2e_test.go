package singbox

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	box "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/include"
	"github.com/sagernet/sing-box/option"
	Mdata "github.com/sagernet/sing/common/metadata"
	singJSON "github.com/sagernet/sing/common/json"
	"github.com/sagernet/sing/service"

	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/kernel"
	"github.com/cedar2025/xboard-node/internal/model"
)

// Test traffic is confined to a loopback HTTP echo server. Only the test
// instance has an explicit route for that destination.
func TestProtocolClientRoundTrip(t *testing.T) {
	const credential = "aaaaaaaa-1111-2222-3333-444444444444"
	body := bytes.Repeat([]byte("roundtrip-test"), 1024)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(w, r.Body)
	}))
	defer target.Close()
	targetAddr := target.Listener.Addr().String()

	for _, protocol := range []string{"shadowsocks", "vmess", "vless", "trojan", "hysteria2", "tuic", "anytls", "socks", "http", "naive", "mieru"} {
		t.Run(protocol, func(t *testing.T) {
			reservation, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			port := reservation.Addr().(*net.TCPAddr).Port
			_ = reservation.Close()
			node := &model.NodeSpec{Protocol: protocol, ServerPort: port, ListenIP: "127.0.0.1", Cipher: "aes-128-gcm"}
			var cert kernel.TLSCert
			out := M{"type": protocol, "tag": "test-client", "server": "127.0.0.1", "server_port": port}
			switch protocol {
			case "shadowsocks":
				out["method"], out["password"] = node.Cipher, credential
			case "vmess":
				out["uuid"], out["security"] = credential, "auto"
			case "vless":
				out["uuid"] = credential
			case "socks", "http", "mieru":
				out["username"], out["password"] = credential, credential
				if protocol == "mieru" {
					out["transport"] = "TCP"
				}
			case "trojan", "hysteria2", "tuic", "anytls", "naive":
				node.TLS = 1
				cert = testSelfSignedTLS(t)
				out["password"] = credential
				out["tls"] = M{"enabled": true, "server_name": "localhost", "certificate": string(cert.CertPEM)}
				if protocol == "naive" {
					out["username"] = "1"
				}
				if protocol == "tuic" {
					out["uuid"] = credential
					// Match the server default ALPN for TUIC over QUIC.
					out["tls"].(M)["alpn"] = []string{"h3"}
				}
			}
			cfg := config.KernelConfig{Type: "singbox", LogLevel: "error", ConfigDir: t.TempDir(),
				CustomRoute: []map[string]any{{"ip_cidr": []string{"127.0.0.1/32"}, "outbound": "direct"}}}
			server := New(cfg)
			if err := server.Start(node, []model.UserSpec{{ID: 1, UUID: credential}}, cert); err != nil {
				t.Fatal(err)
			}
			defer server.Stop()

			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			ctx = include.Context(ctx)
			raw, err := json.Marshal(M{"log": M{"level": "error"}, "outbounds": []M{out}, "route": M{"final": "test-client"}})
			if err != nil {
				t.Fatal(err)
			}
			opts, err := singJSON.UnmarshalExtendedContext[option.Options](ctx, raw)
			if err != nil {
				t.Fatal(err)
			}
			clientBox, err := box.New(box.Options{Context: ctx, Options: opts})
			if err != nil {
				t.Fatal(err)
			}
			defer clientBox.Close()
			if err := clientBox.Start(); err != nil {
				t.Fatal(err)
			}
			manager := service.FromContext[adapter.OutboundManager](ctx)
			if manager == nil {
				t.Fatal("client outbound manager missing")
			}
			outbound, ok := manager.Outbound("test-client")
			if !ok {
				t.Fatal("client outbound missing")
			}
			transport := &http.Transport{DisableKeepAlives: true,
				DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
					if addr != targetAddr {
						t.Fatal("unexpected destination")
					}
					return outbound.DialContext(ctx, network, Mdata.ParseSocksaddr(addr))
				}}
			defer transport.CloseIdleConnections()
			client := &http.Client{Transport: transport, Timeout: 15 * time.Second}
			response, err := client.Post(target.URL, "application/octet-stream", bytes.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if err != nil || response.StatusCode != 200 || !bytes.Equal(got, body) {
				t.Fatalf("roundtrip failed: status=%d bytes=%d err=%v", response.StatusCode, len(got), err)
			}
			deadline := time.Now().Add(3 * time.Second)
			for {
				traffic, _, connections, err := server.GetUserTraffic(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				if traffic[1][0] >= int64(len(body)) && traffic[1][1] >= int64(len(body)) && connections == 0 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("accounting or connection cleanup failed: traffic=%v connections=%d", traffic, connections)
				}
				time.Sleep(25 * time.Millisecond)
			}
		})
	}
}
