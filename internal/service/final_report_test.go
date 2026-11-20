package service

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/controlplane"
	"github.com/cedar2025/xboard-node/internal/reportjournal"
)

type finalReportSink struct {
	fail    bool
	reports []controlplane.ReportPayload
}

func (s *finalReportSink) Report(p controlplane.ReportPayload) error {
	s.reports = append(s.reports, p)
	if s.fail {
		return errors.New("panel unavailable")
	}
	return nil
}
func (s *finalReportSink) ReportDevices(controlplane.PushClient, map[int][]string) {}
func (s *finalReportSink) SupportsReporting() bool                                 { return true }
func (s *finalReportSink) SupportsDeviceReports() bool                             { return false }

func TestLegacyReportQuarantinesAmbiguousFailureWithoutReplay(t *testing.T) {
	cfg := &config.Config{Kernel: config.KernelConfig{ConfigDir: t.TempDir()}}
	s := newService(cfg, controlplane.NewLocalControlPlane(cfg))
	s.tracker.Process(map[int][2]int64{12: {123, 456}}, nil, 0)
	sink := &finalReportSink{fail: true}
	s.sink = sink
	s.pushReportSync()
	j, err := reportjournal.Open(filepath.Join(cfg.Kernel.ConfigDir, "unconfirmed-reports"))
	if err != nil {
		t.Fatal(err)
	}
	pending, err := j.Pending()
	if err != nil || len(pending) != 1 || pending[0].Traffic[12] != [2]int64{123, 456} {
		t.Fatalf("audit = %+v, err=%v", pending, err)
	}
	if s.tracker.HasTraffic() {
		t.Fatal("ambiguous bytes must not be automatically replayed")
	}
	s.tracker.Process(map[int][2]int64{12: {130, 460}}, nil, 0)
	sink.fail = false
	s.pushReportSync()
	if len(sink.reports) != 2 || sink.reports[1].Traffic[12] != [2]int64{7, 4} {
		t.Fatalf("unexpected second report: %+v", sink.reports)
	}
	pending, err = j.Pending()
	if err != nil || len(pending) != 1 {
		t.Fatalf("audit lost: %+v %v", pending, err)
	}
}

func TestLegacyReportStorageFailurePreventsSend(t *testing.T) {
	cfg := &config.Config{Kernel: config.KernelConfig{ConfigDir: ""}}
	s := newService(cfg, controlplane.NewLocalControlPlane(cfg))
	sink := &finalReportSink{}
	s.sink = sink
	err := s.reportLegacy(controlplane.ReportPayload{Traffic: map[int][2]int64{12: {123, 456}}})
	if err == nil || len(sink.reports) != 0 || !s.tracker.HasTraffic() {
		t.Fatal("storage failure must retain unsent data and prevent send")
	}
}

func TestJournalRetryPreservesBatchIdentityAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{Kernel: config.KernelConfig{ConfigDir: dir}}
	first := newService(cfg, controlplane.NewLocalControlPlane(cfg))
	j, err := reportjournal.Open(filepath.Join(dir, "reports"))
	if err != nil {
		t.Fatal(err)
	}
	first.reportJournal = j
	sink := &finalReportSink{fail: true}
	first.sink = sink
	payload := controlplane.ReportPayload{Traffic: map[int][2]int64{12: {123, 456}}}
	if err := first.reportWithJournal(payload); err == nil {
		t.Fatal("failed receipt unexpectedly accepted")
	}
	if len(sink.reports) != 1 || sink.reports[0].BatchID == "" {
		t.Fatal("missing first report identity")
	}
	id := sink.reports[0].BatchID
	second := newService(cfg, controlplane.NewLocalControlPlane(cfg))
	second.reportJournal, err = reportjournal.Open(filepath.Join(dir, "reports"))
	if err != nil {
		t.Fatal(err)
	}
	sink.fail = false
	second.sink = sink
	if err := second.reportWithJournal(controlplane.ReportPayload{}); err != nil {
		t.Fatal(err)
	}
	if len(sink.reports) != 2 || sink.reports[1].BatchID != id || sink.reports[1].Traffic[12] != [2]int64{123, 456} {
		t.Fatalf("retry changed original batch: %+v", sink.reports)
	}
	pending, err := second.reportJournal.Pending()
	if err != nil || len(pending) != 0 {
		t.Fatalf("acknowledged batch remains pending: %v %v", pending, err)
	}
}

func TestJournalBacklogDrainsAtBoundedRate(t *testing.T) {
	cfg := &config.Config{Kernel: config.KernelConfig{ConfigDir: t.TempDir()}}
	s := newService(cfg, controlplane.NewLocalControlPlane(cfg))
	j, err := reportjournal.Open(filepath.Join(cfg.Kernel.ConfigDir, "reports"))
	if err != nil {
		t.Fatal(err)
	}
	s.reportJournal = j
	sink := &finalReportSink{}
	s.sink = sink
	for i := 0; i < 7; i++ {
		if _, err := j.Append(map[int][2]int64{12: {1, 2}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.reportWithJournal(controlplane.ReportPayload{}); err != nil {
		t.Fatal(err)
	}
	pending, err := j.Pending()
	if err != nil || len(pending) != 3 || len(sink.reports) != 4 {
		t.Fatalf("unexpected first tick: pending=%d reports=%d err=%v", len(pending), len(sink.reports), err)
	}
	if err := s.reportWithJournal(controlplane.ReportPayload{}); err != nil {
		t.Fatal(err)
	}
	pending, err = j.Pending()
	if err != nil || len(pending) != 0 || len(sink.reports) != 7 {
		t.Fatalf("unexpected second tick: pending=%d reports=%d err=%v", len(pending), len(sink.reports), err)
	}
	seen := make(map[string]bool)
	for _, p := range sink.reports {
		if p.BatchID == "" || seen[p.BatchID] || p.Traffic[12] != [2]int64{1, 2} {
			t.Fatalf("incorrect batch: %+v", p)
		}
		seen[p.BatchID] = true
	}
}
