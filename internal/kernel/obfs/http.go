// Package obfs implements the HTTP handshake portion of the SIP003
// simple-obfs protocol. This code is an independent Go implementation;
// it does not incorporate the historical GPL-licensed simple-obfs C code.
package obfs

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/cedar2025/xboard-node/internal/nlog"
)

const (
	maxHTTPHeaderBytes = 8192
	headerTimeout      = 8 * time.Second
	backendTimeout     = 5 * time.Second
)

// HTTPServer terminates the simple-obfs HTTP preface and relays the remaining
// encrypted Shadowsocks stream to a loopback-only backend. Real client IPs
// are tracked by the local source-port mapping in origin.go.
type HTTPServer struct {
	mu        sync.RWMutex
	listener  net.Listener
	backend   string
	host      string
	active    map[net.Conn]struct{}
	wg        sync.WaitGroup
	failureMu sync.Mutex
	failures  [3]failureWindow
}

func ListenHTTP(addr, backend, host string) (*HTTPServer, error) {
	if backend == "" {
		return nil, errors.New("empty obfs backend")
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	s := &HTTPServer{listener: ln, backend: backend, host: host, active: make(map[net.Conn]struct{})}
	s.wg.Add(1)
	go s.serve()
	return s, nil
}

type failureWindow struct {
	last       time.Time
	suppressed uint64
}

// recordFailure uses a fixed number of buckets per listener, never per IP.
// Log the first failure immediately, then at most once per minute per category.
func (s *HTTPServer) recordFailure(category int) {
	now := time.Now()
	s.failureMu.Lock()
	w := &s.failures[category]
	if !w.last.IsZero() && now.Sub(w.last) < time.Minute {
		w.suppressed++
		s.failureMu.Unlock()
		return
	}
	count := w.suppressed
	w.last, w.suppressed = now, 0
	s.failureMu.Unlock()
	reasons := [...]string{"handshake_failed", "backend_unavailable", "response_write_failed"}
	nlog.Core().Warn("ss http obfs failure", "listener", s.Addr().String(), "category", reasons[category], "suppressed_since_last_log", count)
}

func (s *HTTPServer) Addr() net.Addr { return s.listener.Addr() }

// UpdateBackend reroutes only newly accepted connections. Existing sessions
// stay bound to their original backend while a kernel swap drains.
func (s *HTTPServer) UpdateBackend(backend, host string) {
	s.mu.Lock()
	s.backend, s.host = backend, host
	s.mu.Unlock()
}

func (s *HTTPServer) serve() {
	defer s.wg.Done()
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		if s.active == nil {
			s.mu.Unlock()
			conn.Close()
			return
		}
		s.active[conn] = struct{}{}
		s.wg.Add(1)
		s.mu.Unlock()
		go s.handle(conn)
	}
}

func (s *HTTPServer) handle(conn net.Conn) {
	defer s.wg.Done()
	defer conn.Close()
	defer func() {
		s.mu.Lock()
		delete(s.active, conn)
		s.mu.Unlock()
	}()

	s.mu.RLock()
	backendAddr, expectedHost := s.backend, s.host
	s.mu.RUnlock()

	if err := conn.SetReadDeadline(time.Now().Add(headerTimeout)); err != nil {
		return
	}
	reader := bufio.NewReaderSize(conn, 4096)
	key, err := readHTTPPreface(reader, expectedHost)
	if err != nil {
		s.recordFailure(0)
		return
	}
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		return
	}
	backend, err := net.DialTimeout("tcp", backendAddr, backendTimeout)
	if err != nil {
		s.recordFailure(1)
		return
	}
	defer backend.Close()

	backendLocal := backend.LocalAddr()
	registerOrigin(backendLocal, conn.RemoteAddr())
	defer unregisterOrigin(backendLocal)
	if _, err := io.WriteString(conn, switchingProtocolsResponse(key)); err != nil {
		s.recordFailure(2)
		return
	}
	// Terminate both directions when either side disconnects. Waiting for
	// two independent Copy loops to finish without closing their peers
	// could otherwise hang a node shutdown forever.
	finished := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(backend, reader)
		finished <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(conn, backend)
		finished <- struct{}{}
	}()
	<-finished
	_ = conn.Close()
	_ = backend.Close()
	<-finished
}

func readHTTPPreface(r *bufio.Reader, expectedHost string) (string, error) {
	total := 0
	var host, upgrade, connection, key string
	for lineNum := 0; ; lineNum++ {
		line, err := r.ReadString('\n')
		if err != nil {
			return "", err
		}
		total += len(line)
		if total > maxHTTPHeaderBytes {
			return "", errors.New("obfs HTTP header too large")
		}
		line = strings.TrimRight(line, "\r\n")
		if lineNum == 0 {
			fields := strings.Fields(line)
			if len(fields) != 3 || fields[2] != "HTTP/1.1" ||
				(fields[0] != "GET" && fields[0] != "POST") {
				return "", errors.New("invalid obfs HTTP request line")
			}
			continue
		}
		if line == "" {
			break
		}
		before, after, ok := strings.Cut(line, ":")
		if !ok {
			return "", errors.New("malformed HTTP header")
		}
		value := strings.TrimSpace(after)
		switch strings.ToLower(strings.TrimSpace(before)) {
		case "host":
			host = value
		case "upgrade":
			upgrade = value
		case "connection":
			connection = value
		case "sec-websocket-key":
			key = value
		}
	}
	if !strings.EqualFold(upgrade, "websocket") || !strings.Contains(strings.ToLower(connection), "upgrade") || key == "" {
		return "", errors.New("missing WebSocket upgrade headers")
	}
	if host == "" {
		return "", errors.New("missing HTTP host")
	}
	if expectedHost != "" && !strings.EqualFold(strings.Split(host, ":")[0], expectedHost) {
		return "", errors.New("obfs host mismatch")
	}
	return key, nil
}

func switchingProtocolsResponse(key string) string {
	sum := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	return "HTTP/1.1 101 Switching Protocols\r\n" +
		"Server: nginx\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + base64.StdEncoding.EncodeToString(sum[:]) + "\r\n\r\n"
}

// StopAccepting closes only the public listening socket. Existing sessions
// continue on their original backend while the old kernel drains.
func (s *HTTPServer) StopAccepting() {
	_ = s.listener.Close()
}

func (s *HTTPServer) Close() {
	s.StopAccepting()
	// Closing all active connections ensures a service stop cannot leak
	// reverse-proxy goroutines beyond the node lifecycle.
	s.mu.Lock()
	active := s.active
	s.active = nil
	for conn := range active {
		_ = conn.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
}
