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
	header, err := TCPHeaderType(n)
	if err != nil {
		return "", err
	}
	if header == "http" || (protocol == "socks" && n.TLS == 1) {
		return "xray", nil
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

// TCPHeaderType validates the panel's raw TCP header configuration.
func TCPHeaderType(n *NodeSpec) (string, error) {
	network := strings.ToLower(strings.TrimSpace(n.Network))
	if network != "" && network != "tcp" {
		return "", nil
	}
	raw, exists := n.NetworkSettings["header"]
	if !exists || raw == nil {
		return "", nil
	}
	header, ok := raw.(map[string]any)
	if !ok {
		return "", fmt.Errorf("TCP header must be an object")
	}
	kind, ok := header["type"].(string)
	if !ok && header["type"] != nil {
		return "", fmt.Errorf("TCP header type must be a string")
	}
	switch kind {
	case "", "none":
		return "", nil
	case "http":
		switch strings.ToLower(strings.TrimSpace(n.Protocol)) {
		case "vmess", "vless", "trojan":
			return kind, nil
		}
	}
	return "", fmt.Errorf("unsupported TCP header %q for %s", kind, n.Protocol)
}
