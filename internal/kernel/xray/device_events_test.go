package xray

import (
	"sync/atomic"
	"testing"

	"github.com/xtls/xray-core/common/buf"
	"github.com/xtls/xray-core/transport"
)

func TestDeviceEventsOnXrayAdmitAndClose(t *testing.T) {
	ld := newTestDispatcher()
	email := userEmail(21)
	ld.UpdateLimits(map[string]int{email: 21}, map[string]int{email: 2}, nil)
	if ld.checkDeviceLimit(email, "192.0.2.21", true) {
		t.Fatal("first IP should be admitted")
	}

	var events atomic.Int64
	ld.SetDeviceChangeFunc(func() { events.Add(1) })
	link := &transport.Link{Reader: nopReader{}, Writer: buf.Discard}
	release := ld.trackLink(link, email, "192.0.2.21", true)
	if got := events.Load(); got != 1 {
		t.Fatalf("admission notifications = %d, want 1", got)
	}
	release()
	if got := events.Load(); got != 2 {
		t.Fatalf("close notifications = %d, want 2", got)
	}
	// Writer callbacks racing with failure cleanup must never double-report.
	link.Writer.(*closeTrackingWriter).Close()
	if got := events.Load(); got != 2 {
		t.Fatalf("duplicate close notifications = %d", got)
	}
}
