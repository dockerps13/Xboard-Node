package model

import (
	"fmt"
	"strings"
)

// ParseSimpleObfs checks the panel's SIP003 plugin configuration.
// An empty plugin is plain Shadowsocks; HTTP is currently the supported
// server-side compatibility mode. TLS mode is deliberately rejected until
// it has a tested handshake adapter.
func ParseSimpleObfs(plugin, opts string) (enabled bool, host string, err error) {
	if strings.TrimSpace(plugin) == "" {
		return false, "", nil
	}
	if !strings.EqualFold(strings.TrimSpace(plugin), "obfs") {
		return false, "", fmt.Errorf("unsupported Shadowsocks plugin %q", plugin)
	}
	mode := ""
	for _, part := range strings.Split(opts, ";") {
		key, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "obfs":
			mode = strings.ToLower(strings.TrimSpace(value))
		case "obfs-host":
			host = strings.TrimSpace(value)
		}
	}
	if mode != "http" {
		return false, "", fmt.Errorf("Shadowsocks simple-obfs mode %q is unsupported: expected obfs=http", mode)
	}
	if strings.ContainsAny(host, "/\r\n\x00") {
		return false, "", fmt.Errorf("invalid obfs-host value")
	}
	return true, host, nil
}
