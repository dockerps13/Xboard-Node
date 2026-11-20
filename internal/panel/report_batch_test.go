package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cedar2025/xboard-node/internal/config"
)

func TestReportBatchIDIsOptInAndRawBytesUnchanged(t *testing.T) {
	var reports []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/server/report" {
			t.Errorf("wrong route: %s", r.URL.Path)
		}
		var report map[string]any
		if err := json.NewDecoder(r.Body).Decode(&report); err != nil {
			t.Error(err)
		}
		reports = append(reports, report)
		if id, ok := report["report_id"].(string); ok {
			_, _ = w.Write([]byte(`{"data":{"report_id":"` + id + `","accepted":true}}`))
		} else {
			_, _ = w.Write([]byte(`{"data":true}`))
		}
	}))
	defer server.Close()
	client := NewClient(config.PanelConfig{URL: server.URL, Token: "token", MachineID: 5, NodeID: 16})
	bytes := map[int][2]int64{99: {1024, 2048}}
	if err := client.ReportWithBatchID("0123456789abcdef0123456789abcdef", bytes, nil, nil, 0, [2]uint64{}, [2]uint64{}, [2]uint64{}, nil); err != nil {
		t.Fatal(err)
	}
	if err := client.Report(bytes, nil, nil, 0, [2]uint64{}, [2]uint64{}, [2]uint64{}, nil); err != nil {
		t.Fatal(err)
	}
	if len(reports) != 2 {
		t.Fatalf("reports = %d", len(reports))
	}
	if reports[0]["report_id"] != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("batch ID missing: %v", reports[0])
	}
	if _, ok := reports[1]["report_id"]; ok {
		t.Fatal("legacy report unexpectedly includes report_id")
	}
	for _, report := range reports {
		traffic, ok := report["traffic"].(map[string]any)
		if !ok {
			t.Fatalf("missing traffic: %v", report)
		}
		usage, ok := traffic["99"].([]any)
		if !ok || len(usage) != 2 || usage[0] != float64(1024) || usage[1] != float64(2048) {
			t.Fatalf("raw bytes changed: %v", traffic["99"])
		}
	}
}

func TestIdempotentReportDoesNotAcceptLegacyOrMismatchedReceipt(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"legacy_200", `{"data":true}`},
		{"wrong_id", `{"data":{"report_id":"different","accepted":true}}`},
		{"rejected", `{"data":{"report_id":"0123456789abcdef0123456789abcdef","accepted":false}}`},
		{"malformed", `not-json`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			}))
			defer ts.Close()
			client := NewClient(config.PanelConfig{URL: ts.URL, NodeID: 16, MachineID: 4, Token: "test"})
			err := client.ReportWithBatchID("0123456789abcdef0123456789abcdef", map[int][2]int64{1: {123, 456}}, nil, nil, 0, [2]uint64{}, [2]uint64{}, [2]uint64{}, nil)
			if err == nil {
				t.Fatal("unverified idempotent receipt accepted")
			}
		})
	}
}
