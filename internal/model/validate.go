package model

import (
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/cedar2025/xboard-node/internal/config"
)

func ValidateNodeSpec(n *NodeSpec, kcfg config.KernelConfig) error {
	if n == nil {
		return nil
	}

	effectiveKernelType := strings.TrimSpace(kcfg.Type)
	if effectiveKernelType == "" {
		effectiveKernelType = strings.TrimSpace(n.KernelType)
	}
	kernelType, err := normalizeKernelType(effectiveKernelType)
	if err != nil {
		return fmt.Errorf("normalize kernel type: %w", err)
	}

	additionalOutboundSources, err := collectAdditionalOutboundTagSources(kcfg.CustomConfig, kcfg.CustomOutbound)
	if err != nil {
		return fmt.Errorf("collect additional outbound tags: %w", err)
	}
	if err := validateOutboundTagCollisions(n.CustomOutbounds, additionalOutboundSources); err != nil {
		return fmt.Errorf("validate outbound tags: %w", err)
	}
	additionalTags := additionalTagNames(additionalOutboundSources)
	availableTags := buildAvailableOutboundTags(n.CustomOutbounds, additionalTags)
	if err := ValidateCustomOutboundsForKernel(n.CustomOutbounds, kernelType, additionalTags); err != nil {
		return fmt.Errorf("validate custom outbounds: %w", err)
	}
	if err := ValidateCustomRouteRules(n.CustomRouteRules, kernelType, availableTags); err != nil {
		return fmt.Errorf("validate custom route rules: %w", err)
	}
	if err := validateProtocolTransport(n.Protocol, n.Network); err != nil {
		return err
	}
	if err := validateMieruOptions(n); err != nil {
		return err
	}
	if err := validateTransportKernel(n.Network, kernelType); err != nil {
		return err
	}
	header, err := TCPHeaderType(n)
	if err != nil {
		return err
	}
	if kernelType != "xray" && (header == "http" || (strings.EqualFold(n.Protocol, "socks") && n.TLS == 1)) {
		return fmt.Errorf("TCP HTTP header and SOCKS TLS require xray kernel")
	}
	// Neither embedded SS inbound implements SIP003 server-side plugins.
	// Reject explicitly in every mode, not just machine auto-selection.
	if strings.EqualFold(strings.TrimSpace(n.Protocol), "shadowsocks") && strings.TrimSpace(n.Plugin) != "" {
		if _, _, err := ParseSimpleObfs(n.Plugin, n.PluginOpt); err != nil {
			return err
		}
		if kernelType != "singbox" {
			return fmt.Errorf("Shadowsocks simple-obfs HTTP adapter requires sing-box kernel, got %s", kernelType)
		}
	}
	return nil
}

func validateMieruOptions(n *NodeSpec) error {
	if !strings.EqualFold(strings.TrimSpace(n.Protocol), "mieru") {
		return nil
	}
	switch transport := strings.ToUpper(strings.TrimSpace(n.Transport)); transport {
	case "", "TCP", "UDP":
	default:
		return fmt.Errorf("unsupported Mieru transport %q (supported: TCP, UDP)", n.Transport)
	}
	if n.TrafficPattern == "" {
		return nil
	}
	if _, err := base64.StdEncoding.DecodeString(n.TrafficPattern); err != nil {
		return fmt.Errorf("invalid Mieru traffic_pattern: %w", err)
	}
	return nil
}

// Reject XHTTP for inbounds that do not implement that transport, even
// when another feature (such as SS obfs) would otherwise choose a kernel.
func validateProtocolTransport(protocol, network string) error {
	protocol = strings.ToLower(strings.TrimSpace(protocol))
	network = strings.ToLower(strings.TrimSpace(network))
	if network == "xhttp" || network == "splithttp" {
		switch protocol {
		case "vmess", "vless", "trojan":
			return nil
		default:
			return fmt.Errorf("unsupported transport %q for %s", network, protocol)
		}
	}
	return nil
}

// singboxUnsupportedTransports lists transport types that sing-box does not support.
var singboxUnsupportedTransports = map[string]bool{
	"xhttp":     true,
	"splithttp": true,
}

func validateTransportKernel(network, kernelType string) error {
	net := strings.ToLower(strings.TrimSpace(network))
	if kernelType == "singbox" && singboxUnsupportedTransports[net] {
		return fmt.Errorf("transport %q is not supported by sing-box kernel; use xray kernel instead", net)
	}
	return nil
}

// ResolveKernelForTransport returns the kernel type required by the given
// transport. If the configured kernel cannot handle the transport, it returns
// the kernel that can. Otherwise it returns configuredKernel unchanged.
// This is used in machine mode to auto-switch kernel per node.
func ResolveKernelForTransport(network, configuredKernel string) string {
	net := strings.ToLower(strings.TrimSpace(network))
	if configuredKernel == "singbox" && singboxUnsupportedTransports[net] {
		return "xray"
	}
	return configuredKernel
}

func normalizeKernelType(value string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "singbox", "sing-box":
		return "singbox", nil
	case "xray":
		return "xray", nil
	default:
		return "", fmt.Errorf("unsupported kernel type %q", value)
	}
}

func buildAvailableOutboundTags(structured []OutboundConfig, rawTags []string) map[string]struct{} {
	available := map[string]struct{}{
		"direct": {},
		"block":  {},
	}
	for _, outbound := range structured {
		tag := strings.ToLower(strings.TrimSpace(outbound.Tag))
		if tag != "" {
			available[tag] = struct{}{}
		}
	}
	for _, tag := range rawTags {
		tag = strings.ToLower(strings.TrimSpace(tag))
		if tag != "" {
			available[tag] = struct{}{}
		}
	}
	return available
}
