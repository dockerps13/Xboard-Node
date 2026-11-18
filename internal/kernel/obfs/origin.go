package obfs

import (
	"net"
	"strconv"
	"sync"
)

// sessionOrigins maps the loopback TCP source endpoint of an obfs relay
// to the original client IP. The mapping is scoped to a live connection;
// it is removed on disconnect. This preserves device/IP accounting without
// relying on the PROXY protocol feature removed by upstream sing-box.
var sessionOrigins sync.Map // "127.0.0.1:source-port" -> real source IP

func registerOrigin(backendLocal, clientRemote net.Addr) {
	client, ok := clientRemote.(*net.TCPAddr)
	if !ok {
		return
	}
	sessionOrigins.Store(backendLocal.String(), client.IP.String())
}

func unregisterOrigin(backendLocal net.Addr) {
	sessionOrigins.Delete(backendLocal.String())
}

// ResolveOrigin returns the real source IP of a recognized HTTP-obfs TCP
// relay; ordinary inbounds have no mapping and are handled unchanged.
func ResolveOrigin(srcIP string, sourcePort uint16) (string, bool) {
	value, ok := sessionOrigins.Load(net.JoinHostPort(srcIP, strconv.Itoa(int(sourcePort))))
	if !ok {
		return "", false
	}
	original, ok := value.(string)
	return original, ok
}
