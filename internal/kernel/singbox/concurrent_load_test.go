package singbox

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"

	box "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/include"
	"github.com/sagernet/sing-box/option"
	singJSON "github.com/sagernet/sing/common/json"
	Mdata "github.com/sagernet/sing/common/metadata"
	"github.com/sagernet/sing/service"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/kernel"
	"github.com/cedar2025/xboard-node/internal/model"
)

func TestMixedProtocolConcurrentLoad(t *testing.T) {
	testConcurrentLoad(t, []string{"http", "socks"})
}

func TestEncryptedProtocolConcurrentLoad(t *testing.T) {
	testConcurrentLoad(t, []string{"shadowsocks", "vless"})
}

func testConcurrentLoad(t *testing.T, protocols []string) {
	t.Helper()
	count := 100
	if value := os.Getenv("XBOARD_TEST_CONNECTIONS"); value != "" {
		n, err := strconv.Atoi(value)
		if err != nil || n < 2 || n > 5000 {
			t.Fatal("XBOARD_TEST_CONNECTIONS must be between 2 and 5000")
		}
		count = n
	}
	const uuid = "aaaaaaaa-1111-2222-3333-444444444444"
	const payload = "xboard-load-check"
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	arrived := make(chan struct{}, count)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1024))
		if err != nil || string(body) != payload {
			http.Error(w, "bad payload", 400)
			return
		}
		arrived <- struct{}{}
		<-release
		_, _ = io.WriteString(w, payload)
	}))
	defer target.Close()
	defer unblock()

	servers := make([]*SingBox, 2)
	ports := make([]int, 2)
	for i, protocol := range protocols {
		reservation, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		ports[i] = reservation.Addr().(*net.TCPAddr).Port
		reservation.Close()
		server := New(config.KernelConfig{Type: "singbox", LogLevel: "error", ConfigDir: t.TempDir(),
			CustomRoute: []map[string]any{{"ip_cidr": []string{"127.0.0.1/32"}, "outbound": "direct"}}})
		if err := server.Start(&model.NodeSpec{Protocol: protocol, ServerPort: ports[i], ListenIP: "127.0.0.1", Cipher: "aes-128-gcm"},
			[]model.UserSpec{{ID: 1, UUID: uuid}}, kernel.TLSCert{}); err != nil {
			t.Fatal(err)
		}
		servers[i] = server
		defer server.Stop()
	}
	// Shared embedded client instances, one for each encrypted protocol.
	// All destinations remain confined to the local echo server.
	outbounds := make([]adapter.Outbound, len(protocols))
	for i, protocol := range protocols {
		if protocol != "shadowsocks" && protocol != "vless" {
			continue
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ctx = include.Context(ctx)
		out := M{"type": protocol, "tag": "load-client", "server": "127.0.0.1", "server_port": ports[i]}
		if protocol == "shadowsocks" {
			out["method"], out["password"] = "aes-128-gcm", uuid
		} else {
			out["uuid"] = uuid
		}
		raw, err := json.Marshal(M{"log": M{"level": "error"}, "outbounds": []M{out}, "route": M{"final": "load-client"}})
		if err != nil {
			t.Fatal(err)
		}
		opts, err := singJSON.UnmarshalExtendedContext[option.Options](ctx, raw)
		if err != nil {
			t.Fatal(err)
		}
		client, err := box.New(box.Options{Context: ctx, Options: opts})
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		if err := client.Start(); err != nil {
			t.Fatal(err)
		}
		manager := service.FromContext[adapter.OutboundManager](ctx)
		if manager == nil {
			t.Fatal("missing outbound manager")
		}
		outbound, ok := manager.Outbound("load-client")
		if !ok {
			t.Fatal("missing load outbound")
		}
		outbounds[i] = outbound
	}
	var baseline, peak, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&baseline)
	baseG := runtime.NumGoroutine()
	baseFD, _ := os.ReadDir("/proc/self/fd")
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	errors := make(chan error, count)
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			transport := &http.Transport{DisableKeepAlives: true}
			defer transport.CloseIdleConnections()
			if outbound := outbounds[i%2]; outbound != nil {
				transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
					if addr != target.Listener.Addr().String() {
						return nil, fmt.Errorf("unexpected destination: %s", addr)
					}
					return outbound.DialContext(ctx, network, Mdata.ParseSocksaddr(addr))
				}
			} else if i%2 == 0 {
				transport.Proxy = func(*http.Request) (*url.URL, error) {
					return url.Parse(fmt.Sprintf("http://%s:%s@127.0.0.1:%d", uuid, uuid, ports[0]))
				}
			} else {
				transport.Proxy = func(*http.Request) (*url.URL, error) {
					return url.Parse(fmt.Sprintf("socks5://%s:%s@127.0.0.1:%d", uuid, uuid, ports[1]))
				}
			}
			req, err := http.NewRequestWithContext(ctx, "POST", target.URL, strings.NewReader(payload))
			if err != nil {
				errors <- err
				return
			}
			resp, err := (&http.Client{Transport: transport}).Do(req)
			if err != nil {
				errors <- err
				return
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err != nil || resp.StatusCode != 200 || string(body) != payload {
				errors <- fmt.Errorf("bad response: status=%d err=%v", resp.StatusCode, err)
			}
		}(i)
	}
	for i := 0; i < count; i++ {
		select {
		case <-arrived:
		case err := <-errors:
			unblock()
			cancel()
			wg.Wait()
			t.Fatal(err)
		case <-ctx.Done():
			unblock()
			wg.Wait()
			t.Fatalf("only %d/%d requests reached target", i, count)
		}
	}
	for i, server := range servers {
		_, _, active, err := server.GetUserTraffic(ctx)
		want := (count + 1 - i) / 2
		if err != nil || active != want {
			unblock()
			cancel()
			wg.Wait()
			t.Fatalf("protocol %d active=%d want=%d err=%v", i, active, want, err)
		}
	}
	runtime.ReadMemStats(&peak)
	peakFD, _ := os.ReadDir("/proc/self/fd")
	t.Logf("held=%d setup=%s heap_baseline=%d heap_held=%d goroutines_baseline=%d goroutines_held=%d fd_baseline=%d fd_held=%d", count, time.Since(start), baseline.HeapAlloc, peak.HeapAlloc, baseG, runtime.NumGoroutine(), len(baseFD), len(peakFD))
	unblock()
	wg.Wait()
	close(errors)
	for err := range errors {
		t.Error(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for i, server := range servers {
		want := int64((count + 1 - i) / 2 * len(payload))
		for {
			traffic, _, active, err := server.GetUserTraffic(context.Background())
			if err == nil && active == 0 && traffic[1][0] >= want && traffic[1][1] >= want {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("protocol %d traffic=%v active=%d err=%v", i, traffic, active, err)
			}
			time.Sleep(25 * time.Millisecond)
		}
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	afterFD, _ := os.ReadDir("/proc/self/fd")
	t.Logf("completed=%d elapsed=%s heap_after_gc=%d goroutines_after=%d fd_after=%d", count, time.Since(start), after.HeapAlloc, runtime.NumGoroutine(), len(afterFD))
	// Running protocol workers may retain idle UDP state legitimately.
	// Check bounded process-wide recovery after stopping both server kernels.
	if os.Getenv("XBOARD_TEST_RESOURCE_RECOVERY") != "1" {
		return
	}
	for _, server := range servers {
		server.Stop()
	}
	target.Close()
	deadline = time.Now().Add(20 * time.Second)
	for {
		runtime.GC()
		runtime.ReadMemStats(&after)
		fds, fdErr := os.ReadDir("/proc/self/fd")
		g := runtime.NumGoroutine()
		if fdErr != nil || baseFD == nil {
			t.Fatalf("resource recovery requires Linux fd measurements: %v", fdErr)
		}
		if len(fds) <= len(baseFD)+32 && g <= baseG+32 {
			t.Logf("resource recovery: goroutines=%d baseline=%d fd=%d baseline=%d heap=%d", g, baseG, len(fds), len(baseFD), after.HeapAlloc)
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("resources did not recover: goroutines=%d baseline=%d fd=%d baseline=%d", g, baseG, len(fds), len(baseFD))
		}
		time.Sleep(250 * time.Millisecond)
	}
}
