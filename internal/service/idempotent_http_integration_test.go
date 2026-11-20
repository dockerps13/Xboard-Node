package service

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/controlplane"
	"github.com/cedar2025/xboard-node/internal/panel"
	"github.com/cedar2025/xboard-node/internal/reportjournal"
)

type idempotentPanelSink struct{ client *panel.Client }

func (s *idempotentPanelSink) Report(p controlplane.ReportPayload) error {
	return s.client.ReportWithBatchID(p.BatchID, p.Traffic, p.Alive, p.Online, p.CPU, p.Mem, p.Swap, p.Disk, p.Metrics)
}
func (s *idempotentPanelSink) ReportDevices(controlplane.PushClient, map[int][]string) {}
func (s *idempotentPanelSink) SupportsReporting() bool                                 { return true }
func (s *idempotentPanelSink) SupportsDeviceReports() bool                             { return false }

func TestIdempotentBillingLostReceiptDoesNotChargeTwice(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]bool{}
	var charged [2]int64
	var attempts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ReportID string              `json:"report_id"`
			Traffic  map[string][2]int64 `json:"traffic"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		if body.ReportID == "" {
			t.Error("missing deduplication key")
			w.WriteHeader(400)
			return
		}
		mu.Lock()
		attempts++
		if !seen[body.ReportID] {
			seen[body.ReportID] = true
			chunk := body.Traffic["27"]
			charged[0] += chunk[0]
			charged[1] += chunk[1]
		}
		first := attempts == 1
		mu.Unlock()
		if first {
			// Simulate a successful DB transaction followed by a lost ack.
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
			"report_id": body.ReportID, "accepted": true,
		}})
	}))
	defer server.Close()

	panelClient := panel.NewClient(config.PanelConfig{URL: server.URL, Token: "test", MachineID: 4, NodeID: 16})
	journalPath := filepath.Join(t.TempDir(), "node-16")
	j, err := reportjournal.Open(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	svc := newService(cfg, controlplane.NewLocalControlPlane(cfg))
	svc.reportJournal = j
	svc.sink = &idempotentPanelSink{client: panelClient}
	svc.tracker.Process(map[int][2]int64{27: {1234, 5678}}, nil, 0)
	if err := svc.reportWithJournal(controlplane.ReportPayload{Traffic: svc.tracker.FlushTraffic()}); err == nil {
		t.Fatal("first response should be ambiguous")
	}
	pending, err := j.Pending()
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending=%+v err=%v", pending, err)
	}

	reopened, err := reportjournal.Open(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	afterRestart := newService(cfg, controlplane.NewLocalControlPlane(cfg))
	afterRestart.reportJournal = reopened
	afterRestart.sink = &idempotentPanelSink{client: panelClient}
	if err := afterRestart.reportWithJournal(controlplane.ReportPayload{}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	gotCharged, gotAttempts := charged, attempts
	mu.Unlock()
	if gotCharged != [2]int64{1234, 5678} || gotAttempts != 2 {
		t.Fatalf("charged=%v attempts=%d; want exactly one charge over two attempts", gotCharged, gotAttempts)
	}
	pending, err = reopened.Pending()
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending after verified ack=%+v err=%v", pending, err)
	}
}
