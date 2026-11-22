package panel

import (
	"net/http"
	"strings"
	"testing"
)

func TestReportDiagnosticRedaction(t *testing.T) {
	ts, client := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("private-token-and-panel-data"))
	})
	defer ts.Close()
	err := client.Report(nil, nil, nil, 0, [2]uint64{}, [2]uint64{}, [2]uint64{}, nil)
	if err == nil || !strings.Contains(err.Error(), "HTTP 500") || strings.Contains(err.Error(), "private-token") {
		t.Fatalf("unexpected diagnostic: %v", err)
	}
	if got := client.SnapshotMetrics(); got.Failure != 1 || got.Success != 0 {
		t.Fatalf("unexpected metrics: %+v", got)
	}
}

func TestReportCountsSuccessfulHTTPOnce(t *testing.T) {
	ts, client := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":true}`))
	})
	defer ts.Close()
	if err := client.Report(nil, nil, nil, 0, [2]uint64{}, [2]uint64{}, [2]uint64{}, nil); err != nil {
		t.Fatal(err)
	}
	if got := client.SnapshotMetrics(); got.Success != 1 || got.Failure != 0 {
		t.Fatalf("unexpected metrics: %+v", got)
	}
}
