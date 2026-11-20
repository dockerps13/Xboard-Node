package singbox

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	box "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/include"
	"github.com/sagernet/sing-box/option"
	singJSON "github.com/sagernet/sing/common/json"
	Mdata "github.com/sagernet/sing/common/metadata"
	"github.com/sagernet/sing/service"

	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/kernel"
	"github.com/cedar2025/xboard-node/internal/model"
)

// Test traffic is confined to a loopback HTTP echo server. Only the test
// instance has an explicit route for that destination.
func TestProtocolClientRoundTrip(t *testing.T) {
	testProtocolClientRoundTrip(t, []string{"shadowsocks", "vmess", "vless", "trojan", "hysteria2", "tuic", "anytls", "socks", "http", "mieru"})
}

func testProtocolClientRoundTrip(t *testing.T, protocols []string) {
	t.Helper()
	const credential = "aaaaaaaa-1111-2222-3333-444444444444"
	body := bytes.Repeat([]byte("roundtrip-test"), 1024)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(w, r.Body)
	}))
	defer target.Close()
	targetAddr := target.Listener.Addr().String()

	for _, protocol := range protocols {
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
					out["username"] = credential
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
						return nil, fmt.Errorf("unexpected destination: %s", addr)
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

func TestAnyTLSRejectsWrongAndRemovedUser(t *testing.T) {
	const credential = "aaaaaaaa-1111-2222-3333-444444444444"
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "unexpectedly reached target")
	}))
	defer target.Close()

	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := reservation.Addr().(*net.TCPAddr).Port
	_ = reservation.Close()
	cert := testSelfSignedTLS(t)
	server := New(config.KernelConfig{
		Type:        "singbox",
		LogLevel:    "error",
		ConfigDir:   t.TempDir(),
		CustomRoute: []map[string]any{{"ip_cidr": []string{"127.0.0.1/32"}, "outbound": "direct"}},
	})
	if err := server.Start(&model.NodeSpec{Protocol: "anytls", ServerPort: port, ListenIP: "127.0.0.1", TLS: 1}, []model.UserSpec{{ID: 1, UUID: credential}}, cert); err != nil {
		t.Fatal(err)
	}
	defer server.Stop()

	if err := anyTLSHTTPPost(target.URL, port, "wrong-password", cert); err == nil {
		t.Fatal("wrong AnyTLS password unexpectedly authenticated")
	}
	if removed, err := server.RemoveUsers([]model.UserSpec{{ID: 1, UUID: credential}}); err != nil || removed != 1 {
		t.Fatalf("remove AnyTLS user: removed=%d err=%v", removed, err)
	}
	if err := anyTLSHTTPPost(target.URL, port, credential, cert); err == nil {
		t.Fatal("deleted AnyTLS user unexpectedly authenticated")
	}
}

func anyTLSHTTPPost(targetURL string, port int, password string, cert kernel.TLSCert) error {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	ctx = include.Context(ctx)
	out := M{
		"type":        "anytls",
		"tag":         "test-client",
		"server":      "127.0.0.1",
		"server_port": port,
		"password":    password,
		"tls": M{
			"enabled":     true,
			"server_name": "localhost",
			"certificate": string(cert.CertPEM),
		},
	}
	raw, err := json.Marshal(M{"log": M{"level": "error"}, "outbounds": []M{out}, "route": M{"final": "test-client"}})
	if err != nil {
		return err
	}
	opts, err := singJSON.UnmarshalExtendedContext[option.Options](ctx, raw)
	if err != nil {
		return err
	}
	clientBox, err := box.New(box.Options{Context: ctx, Options: opts})
	if err != nil {
		return err
	}
	defer clientBox.Close()
	if err := clientBox.Start(); err != nil {
		return err
	}
	manager := service.FromContext[adapter.OutboundManager](ctx)
	if manager == nil {
		return fmt.Errorf("client outbound manager missing")
	}
	outbound, ok := manager.Outbound("test-client")
	if !ok {
		return fmt.Errorf("client outbound missing")
	}
	target, err := url.Parse(targetURL)
	if err != nil {
		return err
	}
	transport := &http.Transport{DisableKeepAlives: true,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return outbound.DialContext(ctx, network, Mdata.ParseSocksaddr(addr))
		}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
	response, err := client.Post(target.String(), "text/plain", bytes.NewBufferString("probe"))
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %d", response.StatusCode)
	}
	return nil
}

func TestHTTPProxySupportsPlainAndConnectWithAuthentication(t *testing.T) {
	const credential = "aaaaaaaa-1111-2222-3333-444444444444"
	plainTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "plain-ok")
	}))
	defer plainTarget.Close()
	tlsTarget := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "connect-ok")
	}))
	defer tlsTarget.Close()

	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := reservation.Addr().(*net.TCPAddr).Port
	_ = reservation.Close()
	server := New(config.KernelConfig{
		Type:        "singbox",
		LogLevel:    "error",
		ConfigDir:   t.TempDir(),
		CustomRoute: []map[string]any{{"ip_cidr": []string{"127.0.0.1/32"}, "outbound": "direct"}},
	})
	if err := server.Start(&model.NodeSpec{Protocol: "http", ServerPort: port, ListenIP: "127.0.0.1"}, []model.UserSpec{{ID: 1, UUID: credential}}, kernel.TLSCert{}); err != nil {
		t.Fatal(err)
	}
	defer server.Stop()

	proxyURL, err := url.Parse(fmt.Sprintf("http://%s:%s@127.0.0.1:%d", credential, credential, port))
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{
		Proxy:           http.ProxyURL(proxyURL),
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // test target is ephemeral self-signed TLS
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	for _, target := range []struct {
		name string
		url  string
		body string
	}{
		{name: "plain", url: plainTarget.URL, body: "plain-ok"},
		{name: "connect", url: tlsTarget.URL, body: "connect-ok"},
	} {
		t.Run(target.name, func(t *testing.T) {
			response, err := client.Get(target.url)
			if err != nil {
				t.Fatal(err)
			}
			got, readErr := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if readErr != nil || response.StatusCode != http.StatusOK || string(got) != target.body {
				t.Fatalf("proxy response: status=%d body=%q err=%v", response.StatusCode, got, readErr)
			}
		})
	}
}

func TestMieruUDPClientRoundTrip(t *testing.T) {
	const credential = "aaaaaaaa-1111-2222-3333-444444444444"
	target, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	done := make(chan error, 1)
	go func() {
		buf := make([]byte, 256)
		n, addr, readErr := target.ReadFrom(buf)
		if readErr != nil {
			done <- readErr
			return
		}
		if string(buf[:n]) != "mieru-udp-request" {
			done <- fmt.Errorf("unexpected Mieru UDP payload %q", buf[:n])
			return
		}
		_, writeErr := target.WriteTo([]byte("mieru-udp-response"), addr)
		done <- writeErr
	}()

	tcpReservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := tcpReservation.Addr().(*net.TCPAddr).Port
	_ = tcpReservation.Close()
	udpReservation, err := net.ListenPacket("udp", net.JoinHostPort("127.0.0.1", fmt.Sprint(port)))
	if err != nil {
		t.Fatal(err)
	}
	_ = udpReservation.Close()

	server := New(config.KernelConfig{
		Type:        "singbox",
		LogLevel:    "error",
		ConfigDir:   t.TempDir(),
		CustomRoute: []map[string]any{{"ip_cidr": []string{"127.0.0.1/32"}, "outbound": "direct"}},
	})
	if err := server.Start(&model.NodeSpec{
		Protocol:       "mieru",
		ServerPort:     port,
		ListenIP:       "127.0.0.1",
		Transport:      "UDP",
		TrafficPattern: "CAE=",
	}, []model.UserSpec{{ID: 1, UUID: credential}}, kernel.TLSCert{}); err != nil {
		t.Fatal(err)
	}
	defer server.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ctx = include.Context(ctx)
	raw, err := json.Marshal(M{
		"log": M{"level": "error"},
		"outbounds": []M{{
			"type":            "mieru",
			"tag":             "mieru-client",
			"server":          "127.0.0.1",
			"server_port":     port,
			"username":        credential,
			"password":        credential,
			"transport":       "UDP",
			"traffic_pattern": "CAE=",
		}},
		"route": M{"final": "mieru-client"},
	})
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
	outbound, ok := manager.Outbound("mieru-client")
	if !ok {
		t.Fatal("Mieru outbound missing")
	}
	packetConn, err := outbound.ListenPacket(ctx, Mdata.ParseSocksaddr(target.LocalAddr().String()))
	if err != nil {
		t.Fatal(err)
	}
	defer packetConn.Close()
	if _, err := packetConn.WriteTo([]byte("mieru-udp-request"), target.LocalAddr()); err != nil {
		t.Fatal(err)
	}
	_ = packetConn.SetReadDeadline(time.Now().Add(5 * time.Second))
	response := make([]byte, 256)
	n, _, err := packetConn.ReadFrom(response)
	if err != nil || string(response[:n]) != "mieru-udp-response" {
		t.Fatalf("Mieru UDP roundtrip got %q: %v", response[:n], err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
