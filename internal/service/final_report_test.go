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
	fail bool
	reports []controlplane.ReportPayload
}

func (s *finalReportSink) Report(p controlplane.ReportPayload) error {
	s.reports = append(s.reports, p)
	if s.fail { return errors.New("panel unavailable") }
	return nil
}
func (s *finalReportSink) ReportDevices(controlplane.PushClient, map[int][]string) {}
func (s *finalReportSink) SupportsReporting() bool { return true }
func (s *finalReportSink) SupportsDeviceReports() bool { return false }

func TestFinalReportFailureRetainsTrafficForControlledRetry(t *testing.T) {
	cfg := &config.Config{Kernel: config.KernelConfig{ConfigDir: t.TempDir()}}
	s := newService(cfg, controlplane.NewLocalControlPlane(cfg))
	s.tracker.Process(map[int][2]int64{12: {123, 456}}, nil, 0)
	sink := &finalReportSink{fail: true}
	s.sink = sink

	s.pushReportSync()
	journal, err := reportjournal.Open(filepath.Join(cfg.Kernel.ConfigDir, "unconfirmed-reports"))
	if err != nil { t.Fatal(err) }
	uncertain, err := journal.Pending()
	if err != nil { t.Fatal(err) }
	if len(uncertain) != 1 || uncertain[0].Traffic[12] != [2]int64{123, 456} {
		t.Fatalf("unconfirmed traffic archive = %+v", uncertain)
	}
	if !s.tracker.HasTraffic() {
		t.Fatal("shutdown report failure must not discard unreported traffic")
	}
	sink.fail = false
	s.pushReportSync()
	if s.tracker.HasTraffic() {
		t.Fatal("confirmed report must consume the pending traffic")
	}
	if len(sink.reports) != 2 {
		t.Fatalf("report attempts: got %d, want 2", len(sink.reports))
	}
	for i, report := range sink.reports {
		if got := report.Traffic[12]; got != [2]int64{123, 456} {
			t.Fatalf("attempt %d traffic = %v, want original delta", i, got)
		}
	}
}
