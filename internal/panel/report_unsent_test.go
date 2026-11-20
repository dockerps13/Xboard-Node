package panel

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cedar2025/xboard-node/internal/config"
)

func TestReportDialFailureIsUnsent(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	ts.Close()
	c := NewClient(config.PanelConfig{URL: ts.URL, Token: "secret", MachineID: 24, NodeID: 14})
	err := c.postJSON("/api/v2/server/report", map[string]interface{}{})
	if !IsReportNotSent(err) {
		t.Fatalf("expected unsent classification: %v", err)
	}
}

func TestReportHTTPFailureIsAmbiguous(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()
	c := NewClient(config.PanelConfig{URL: ts.URL, Token: "secret", MachineID: 24, NodeID: 14})
	err := c.postJSON("/api/v2/server/report", map[string]interface{}{})
	if err == nil || IsReportNotSent(err) {
		t.Fatalf("expected ambiguous failure: %v", err)
	}
}
