package service

import (
	"strings"
	"testing"

	"github.com/cedar2025/xboard-node/internal/model"
)

func TestRuntimeAttestationUsesSuccessfullyAppliedConfig(t *testing.T) {
	config := &model.NodeSpec{
		ControlPlaneRevision:   16,
		ControlPlaneConfigHash: strings.Repeat("A", 64),
		Protocol:               "AnyTLS",
		ServerPort:             13440,
	}
	actual := runtimeAttestationMap(config)
	if actual == nil || actual["revision"] != int64(16) || actual["protocol"] != "anytls" || actual["listen_port"] != 13440 || actual["config_hash"] != strings.Repeat("a", 64) {
		t.Fatalf("unexpected attestation: %#v", actual)
	}
}

func TestRuntimeAttestationRejectsUnversionedConfig(t *testing.T) {
	if actual := runtimeAttestationMap(&model.NodeSpec{Protocol: "anytls", ServerPort: 443}); actual != nil {
		t.Fatalf("unversioned config must not be attested: %#v", actual)
	}
	invalid := &model.NodeSpec{ControlPlaneRevision: 1, ControlPlaneConfigHash: strings.Repeat("z", 64), Protocol: "anytls", ServerPort: 443}
	if actual := runtimeAttestationMap(invalid); actual != nil {
		t.Fatalf("non-hex config hash must not be attested: %#v", actual)
	}
	invalid.ControlPlaneConfigHash = strings.Repeat("a", 64)
	invalid.ServerPort = 65536
	if actual := runtimeAttestationMap(invalid); actual != nil {
		t.Fatalf("invalid listen port must not be attested: %#v", actual)
	}
}
