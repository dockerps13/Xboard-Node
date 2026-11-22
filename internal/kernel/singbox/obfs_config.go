package singbox

import (
	"fmt"
	"net"
	"strconv"

	"github.com/cedar2025/xboard-node/internal/kernel"
	"github.com/cedar2025/xboard-node/internal/model"
)

// prepareHTTPObfsConfig builds two SS listeners inside the same sing-box core:
// a TCP-only loopback listener receiving deobfuscated bytes, and a
// regular public UDP listener for direct Shadowsocks UDP. The Go frontend
// records each TCP original IP for device accounting.
func prepareHTTPObfsConfig(
	kcfgConfig M,
	nc *model.NodeSpec,
	users []model.UserSpec,
	tc kernel.TLSCert,
) (backend string, host string, err error) {
	enabled, host, err := model.ParseSimpleObfs(nc.Plugin, nc.PluginOpt)
	if err != nil || !enabled {
		return "", host, err
	}
	if nc.ServerPort < 1 || nc.ServerPort > 65535 {
		return "", "", fmt.Errorf("invalid public Shadowsocks obfs port %d", nc.ServerPort)
	}
	reserved, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", "", fmt.Errorf("allocate SS obfs loopback port: %w", err)
	}
	port := reserved.Addr().(*net.TCPAddr).Port
	_ = reserved.Close()
	backend = net.JoinHostPort("127.0.0.1", strconv.Itoa(port))

	tcpConfig, ok := kcfgConfig["inbounds"].([]M)
	if !ok || len(tcpConfig) == 0 {
		return "", "", fmt.Errorf("sing-box Shadowsocks inbound missing")
	}
	tcpInbound := tcpConfig[0]
	tcpInbound["listen"] = "127.0.0.1"
	tcpInbound["listen_port"] = port
	tcpInbound["network"] = "tcp"

	udpNode := *nc
	udpNode.Plugin = ""
	udpNode.PluginOpt = ""
	udpInbound := buildInbound(&udpNode, users, tc)
	if udpInbound == nil {
		return "", "", fmt.Errorf("failed to build UDP Shadowsocks inbound")
	}
	udpInbound["tag"] = "shadowsocks-udp-in"
	udpInbound["network"] = "udp"
	if nc.ListenIP != "" {
		udpInbound["listen"] = nc.ListenIP
	}
	udpInbound["reuse_addr"] = true
	kcfgConfig["inbounds"] = append(tcpConfig, udpInbound)
	return backend, host, nil
}
