package service

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/controlplane"
	"github.com/cedar2025/xboard-node/internal/reportjournal"
)

func TestIdempotentReportRetriesSameDiskBatchIDAfterRestart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "node-16", "idempotent-reports")
	j, err := reportjournal.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	old := newService(cfg, controlplane.NewLocalControlPlane(cfg))
	old.reportJournal = j
	sink := &finalReportSink{fail: true}
	old.sink = sink
	old.tracker.Process(map[int][2]int64{16: {1024, 2048}}, nil, 0)
	payload := controlplane.ReportPayload{Traffic: old.tracker.FlushTraffic()}
	if err := old.reportWithJournal(payload); err == nil {
		t.Fatal("expected first HTTP response to be ambiguous")
	}
	if old.tracker.HasTraffic() {
		t.Fatal("persisted batch should not be added back to in-memory counters")
	}
	pending, err := j.Pending()
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending after lost ack = %+v, %v", pending, err)
	}
	id := pending[0].ID
	if sink.reports[0].BatchID != id {
		t.Fatal("first transmission lost its batch ID")
	}

	// New process opens the same journal. The panel is now reachable.
	restored, err := reportjournal.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	restarted := newService(cfg, controlplane.NewLocalControlPlane(cfg))
	restarted.reportJournal = restored
	restarted.sink = &finalReportSink{}
	if err := restarted.reportWithJournal(controlplane.ReportPayload{}); err != nil {
		t.Fatal(err)
	}
	received := restarted.sink.(*finalReportSink).reports
	if len(received) != 1 || received[0].BatchID != id {
		t.Fatalf("batch ID changed after restart: %#v", received)
	}
	if received[0].Traffic[16] != [2]int64{1024, 2048} {
		t.Fatalf("raw traffic changed: %v", received[0].Traffic)
	}
	pending, err = restored.Pending()
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending after ack = %+v, %v", pending, err)
	}
}

func TestIdempotentStorageFailureRestoresCountersWithoutSending(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "missing-on-disk")
	j, err := reportjournal.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	s := newService(cfg, controlplane.NewLocalControlPlane(cfg))
	s.reportJournal = j
	s.sink = &finalReportSink{}
	s.tracker.Process(map[int][2]int64{18: {44, 55}}, nil, 0)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := s.reportWithJournal(controlplane.ReportPayload{Traffic: s.tracker.FlushTraffic()}); err == nil {
		t.Fatal("expected spool write error")
	}
	if !s.tracker.HasTraffic() {
		t.Fatal("failed durable write lost raw bytes")
	}
	if len(s.sink.(*finalReportSink).reports) != 0 {
		t.Fatal("transmitted unjournaled bytes")
	}
}

func TestJournalDeviceSnapshotHasIndependentReport(t *testing.T) {
	cfg := &config.Config{}
	s := newService(cfg, controlplane.NewLocalControlPlane(cfg))
	j, err := reportjournal.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.reportJournal = j
	sink := &finalReportSink{}
	s.sink = sink
	if _, err := j.Append(map[int][2]int64{2: {10, 20}}); err != nil {
		t.Fatal(err)
	}
	payload := controlplane.ReportPayload{Alive: map[int][]string{2: {}}, Online: map[int]int{2: 0}}
	if err := s.reportWithJournal(payload); err != nil {
		t.Fatal(err)
	}
	if len(sink.reports) != 2 || sink.reports[0].BatchID == "" {
		t.Fatalf("expected billing and independent device report: %+v", sink.reports)
	}
	device := sink.reports[1]
	if ips, ok := device.Alive[2]; !ok || ips == nil || len(ips) != 0 || device.BatchID != "" || len(device.Traffic) != 0 {
		t.Fatalf("incorrect independent offline report: %+v", device)
	}
}

func TestDeviceReportCounts(t *testing.T) {
	ips, offline := deviceReportCounts(map[int][]string{2: {}, 3: {"192.0.2.1", "2001:db8::1"}})
	if ips != 2 || offline != 1 {
		t.Fatalf("ips=%d offline=%d", ips, offline)
	}
}
