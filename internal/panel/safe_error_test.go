package panel

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cedar2025/xboard-node/internal/config"
)

type credentialErrorTransport struct{}

func (credentialErrorTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return nil, errors.New("untrusted failure " + r.URL.String())
}

func TestPanelTransportErrorOmitsCredentials(t *testing.T) {
	client := NewClient(config.PanelConfig{URL: "https://example.invalid", Token: "secret-test-value", MachineID: 24, NodeID: 14})
	client.httpClient.Transport = credentialErrorTransport{}
	_, err := client.GetConfig()
	if err == nil || strings.Contains(err.Error(), "secret-test-value") || strings.Contains(err.Error(), "token=") {
		t.Fatal("expected credential-free request error")
	}
	if !strings.Contains(err.Error(), "panel transport failure") {
		t.Fatalf("missing diagnostic category: %v", err)
	}
}

func TestPanelStatusErrorOmitsReflectedBody(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(r.URL.RawQuery))
	}))
	defer ts.Close()
	client := NewClient(config.PanelConfig{URL: ts.URL, Token: "secret-test-value", MachineID: 24, NodeID: 14})
	_, err := client.GetConfig()
	if err == nil || strings.Contains(err.Error(), "secret-test-value") || strings.Contains(err.Error(), "token=") {
		t.Fatal("expected credential-free status error")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatal("missing HTTP status")
	}
}
