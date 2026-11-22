package service

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/controlplane"
	"github.com/cedar2025/xboard-node/internal/panel"
	"github.com/cedar2025/xboard-node/internal/reportjournal"
)

// This tests two node journals and an atomic, shared-panel deduplication
// contract. It does NOT replace testing the real Laravel/SQL implementation.
func TestTwoNodesLostBillingReceiptsRecoverWithoutDoubleCharge(t *testing.T) {
	type ledgerKey struct {
		node int
		id   string
	}
	var mu sync.Mutex
	attempts := make(map[ledgerKey]int)
	charged := make(map[int][2]int64)
	seen := make(map[ledgerKey][2]int64)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			NodeID   int                 `json:"node_id"`
			ReportID string              `json:"report_id"`
			Traffic  map[string][2]int64 `json:"traffic"`
		}
		if r.URL.Path != "/api/v2/server/report" || json.NewDecoder(r.Body).Decode(&body) != nil || body.ReportID == "" {
			http.Error(w, "invalid simulated report", http.StatusBadRequest)
			return
		}
		key := ledgerKey{body.NodeID, body.ReportID}
		mu.Lock()
		attempts[key]++
		first := attempts[key] == 1
		if _, ok := seen[key]; !ok {
			traffic := body.Traffic["27"]
			seen[key] = traffic
			old := charged[body.NodeID]
			charged[body.NodeID] = [2]int64{old[0] + traffic[0], old[1] + traffic[1]}
		}
		mu.Unlock()
		if first {
			// Simulates a committed DB debit whose HTTP ACK was lost.
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
			"report_id": body.ReportID, "accepted": true,
		}})
	}))
	defer server.Close()

	type node struct {
		id          int
		journalPath string
		bytes       [2]int64
	}
	nodes := []node{
		{id: 21, journalPath: filepath.Join(t.TempDir(), "machine-24", "node-21"), bytes: [2]int64{1200, 3300}},
		{id: 22, journalPath: filepath.Join(t.TempDir(), "machine-24", "node-22"), bytes: [2]int64{2500, 4100}},
	}
	send := func(n node, first bool) error {
		j, err := reportjournal.Open(n.journalPath)
		if err != nil {
			return err
		}
		cfg := &config.Config{}
		svc := newService(cfg, controlplane.NewLocalControlPlane(cfg))
		svc.reportJournal = j
		client := panel.NewClient(config.PanelConfig{
			URL: server.URL, Token: "test", MachineID: 24, NodeID: n.id,
		})
		svc.sink = &idempotentPanelSink{client: client}
		if first {
			svc.tracker.Process(map[int][2]int64{27: n.bytes}, nil, 1)
			return svc.reportWithJournal(controlplane.ReportPayload{Traffic: svc.tracker.FlushTraffic()})
		}
		return svc.reportWithJournal(controlplane.ReportPayload{})
	}

	// Both nodes send concurrently. Each receives a lost ACK.
	var wg sync.WaitGroup
	firstErrors := make([]error, len(nodes))
	for i, n := range nodes {
		wg.Add(1)
		go func(i int, n node) {
			defer wg.Done()
			firstErrors[i] = send(n, true)
		}(i, n)
	}
	wg.Wait()
	for i, err := range firstErrors {
		if err == nil {
			t.Fatalf("node %d: lost ACK unexpectedly succeeded", nodes[i].id)
		}
	}
	// Each journal is reopened as if its process restarted.
	retryErrors := make([]error, len(nodes))
	for i, n := range nodes {
		wg.Add(1)
		go func(i int, n node) {
			defer wg.Done()
			retryErrors[i] = send(n, false)
		}(i, n)
	}
	wg.Wait()
	for i, n := range nodes {
		if retryErrors[i] != nil {
			t.Fatalf("node %d recovery failed: %v", n.id, retryErrors[i])
		}
		j, err := reportjournal.Open(n.journalPath)
		if err != nil {
			t.Fatal(err)
		}
		pending, err := j.Pending()
		if err != nil || len(pending) != 0 {
			t.Fatalf("node %d pending after ack: %v, %v", n.id, pending, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(attempts) != len(nodes) {
		t.Fatalf("journal identities collided between nodes: %d unique reports", len(attempts))
	}
	for _, n := range nodes {
		if charged[n.id] != n.bytes {
			t.Errorf("node %s charged %v, want %v", strconv.Itoa(n.id), charged[n.id], n.bytes)
		}
	}
	for key, count := range attempts {
		if count != 2 {
			t.Errorf("node %d batch %s attempted %d times, want twice", key.node, key.id, count)
		}
	}
}
