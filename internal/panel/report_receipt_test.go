package panel

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/cedar2025/xboard-node/internal/config"
	"net/http/httptest"
)

func TestReportExplicitNegativeReceipt(t *testing.T) {
	for _, tc := range []struct {
		name     string
		body     string
		rejected bool
	}{
		{"data_false", `{"data":false}`, true},
		{"success_false", `{"success":false}`, true},
		{"data_true", `{"data":true}`, false},
		{"legacy_empty", "", false},
		{"legacy_plaintext", "OK", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v2/server/report" {
					t.Errorf("unexpected path: %s", r.URL.Path)
				}
				_, _ = fmt.Fprint(w, tc.body)
			}))
			defer ts.Close()
			client := NewClient(config.PanelConfig{URL: ts.URL, MachineID: 1, NodeID: 16, Token: "test"})
			err := client.Report(map[int][2]int64{11: {123, 456}}, nil, nil, 0, [2]uint64{}, [2]uint64{}, [2]uint64{}, nil)
			if (err != nil) != tc.rejected {
				t.Fatalf("Report error = %v, rejected=%v", err, tc.rejected)
			}
		})
	}
}
