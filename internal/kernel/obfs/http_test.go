package obfs

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func TestSimpleObfsHTTPProxyRoundTrip(t *testing.T) {
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	answer := make(chan string, 1)
	go func() {
		conn, err := backend.Accept()
		if err != nil {
			answer <- "accept: " + err.Error()
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		reader := bufio.NewReader(conn)
		payload := make([]byte, 5)
		if _, err := io.ReadFull(reader, payload); err != nil {
			answer <- err.Error()
			return
		}
		// The frontend installs the mapping before transmitting the first
		// encrypted payload to the backend, not at TCP Accept time.
		origin := conn.RemoteAddr().(*net.TCPAddr)
		originalIP, ok := ResolveOrigin(origin.IP.String(), uint16(origin.Port))
		if !ok || originalIP != "127.0.0.1" {
			answer <- fmt.Sprintf("missing original client mapping for %v: %q", origin, originalIP)
			return
		}
		if string(payload) != "hello" {
			answer <- fmt.Sprintf("wrong body %q", payload)
			return
		}
		_, err = conn.Write([]byte("world"))
		if err != nil {
			answer <- err.Error()
			return
		}
		answer <- ""
	}()
	server, err := ListenHTTP("127.0.0.1:0", backend.Addr().String(), "www.bing.com")
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	client, err := net.DialTimeout("tcp", server.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(3 * time.Second))
	_, err = io.WriteString(client, "GET / HTTP/1.1\r\nHost: www.bing.com\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Key: dGVzdA==\r\n\r\nhello")
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(client)
	line, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(line, "101 Switching Protocols") {
		t.Fatalf("expected upgrade, got %q: %v", line, err)
	}
	for {
		line, err = reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\r\n" {
			break
		}
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(reader, buf); err != nil || string(buf) != "world" {
		t.Fatalf("backend response got %q: %v", buf, err)
	}
	if got := <-answer; got != "" {
		t.Fatal(got)
	}
}

func TestHTTPPrefaceRejectsIncorrectHost(t *testing.T) {
	request := "GET / HTTP/1.1\r\nHost: incorrect.test\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: test\r\n\r\n"
	_, err := readHTTPPreface(bufio.NewReader(strings.NewReader(request)), "www.bing.com")
	if err == nil {
		t.Fatal("wrong host should not reach Shadowsocks backend")
	}
}

func TestFailureLogWindows(t *testing.T) {
	server, err := ListenHTTP("127.0.0.1:0", "127.0.0.1:1", "")
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	server.recordFailure(0)
	first := server.failures[0].last
	server.recordFailure(0)
	server.recordFailure(0)
	if server.failures[0].suppressed != 2 || !server.failures[0].last.Equal(first) {
		t.Fatal("repeated failures should be suppressed")
	}
	server.recordFailure(1)
	if server.failures[1].last.IsZero() || server.failures[1].suppressed != 0 {
		t.Fatal("backend failures must have an independent window")
	}
	server.failures[0].last = time.Now().Add(-2 * time.Minute)
	server.recordFailure(0)
	if server.failures[0].suppressed != 0 || time.Since(server.failures[0].last) > time.Minute {
		t.Fatal("expired window should reset")
	}
}
