// Package tcpprobe provides an optional TCP handshake endpoint for UDP-only
// QUIC nodes. It never forwards traffic or attempts to imitate the protocol.
// The observed RTT measures only TCP connection establishment.
package tcpprobe

import (
	"net"
	"sync"
)

// Listener owns a single TCP listening socket.
type Listener struct {
	listener net.Listener
	addr     string
	once     sync.Once
}

// Listen creates a TCP listener and immediately accepts and closes connections.
// It has no per-connection goroutine and does not read or echo client data.
func Listen(addr string) (*Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	p := &Listener{listener: ln, addr: addr}
	go p.serve()
	return p, nil
}

func (p *Listener) Addr() string {
	if p == nil {
		return ""
	}
	return p.addr
}

func (p *Listener) serve() {
	for {
		conn, err := p.listener.Accept()
		if err != nil {
			return // Close terminates Accept; no retry loop on a broken listener.
		}
		_ = conn.Close()
	}
}

func (p *Listener) Close() error {
	if p == nil {
		return nil
	}
	var err error
	p.once.Do(func() { err = p.listener.Close() })
	return err
}
