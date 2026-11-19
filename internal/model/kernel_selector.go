package model

import (
	"fmt"
	"strings"
)

// ResolveKernelForNode chooses a supported kernel for a panel node.
// It deliberately rejects features neither embedded inbound implements,
// instead of silently removing them from the server configuration.
func ResolveKernelForNode(n *NodeSpec, preferred string) (string, error) {
	if n == nil {
		return "", fmt.Errorf("nil node specification")
	}
	protocol := strings.ToLower(strings.TrimSpace(n.Protocol))
	network := strings.ToLower(strings.TrimSpace(n.Network))
	preferred = strings.ToLower(strings.TrimSpace(preferred))
	if preferred == "sing-box" {
		preferred = "singbox"
	}
	if preferred == "" {
		preferred = "singbox"
	}
	if preferred != "singbox" && preferred != "xray" {
		return "", fmt.Errorf("unrecognized kernel %q", preferred)
	}
	if err := validateProtocolTransport(protocol, network); err != nil {
		return "", err
	}
	if protocol == "shadowsocks" && strings.TrimSpace(n.Plugin) != "" {
		if _, _, err := ParseSimpleObfs(n.Plugin, n.PluginOpt); err != nil {
			return "", err
		}
		return "singbox", nil
	}
	switch protocol {
	case "hysteria", "hysteria2", "tuic", "anytls", "naive", "mieru":
		if network == "xhttp" || network == "splithttp" {
			return "", fmt.Errorf("unsupported transport %q for %s", network, protocol)
		}
		return "singbox", nil
	case "vmess", "vless", "trojan", "shadowsocks", "socks", "http":
		if network == "xhttp" || network == "splithttp" {
			if protocol != "vmess" && protocol != "vless" && protocol != "trojan" {
				return "", fmt.Errorf("unsupported transport %q for %s", network, protocol)
			}
			return "xray", nil
		}
		if protocol == "vless" && strings.TrimSpace(n.Decryption) != "" {
			return "xray", nil
		}
		return preferred, nil
	default:
		return "", fmt.Errorf("unsupported protocol %q", protocol)
	}
}
