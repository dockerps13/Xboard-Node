package singbox

import (
	"context"
	"sync/atomic"
	"testing"
)

func TestDeviceEventsOnSingBoxConnectionAdmitAndClose(t *testing.T) {
	tracker := NewConnTracker(0)
	tracker.SetUserMap(map[string]int{"test-user": 11})
	var n atomic.Int64
	tracker.SetDeviceChangeFunc(func() { n.Add(1) })
	conn := tracker.RoutedConnection(context.Background(), &testConn{},
		testInboundContext("test-user", "192.0.2.11"), nil, nil)
	if n.Load() != 1 {
		t.Fatalf("admit events=%d, want 1", n.Load())
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if n.Load() != 2 {
		t.Fatalf("close events=%d, want 2", n.Load())
	}
	conn.Close()
	if n.Load() != 2 {
		t.Fatalf("duplicate close events=%d", n.Load())
	}
}

func TestNoDeviceEventOnRejectedSingBoxAdmission(t *testing.T) {
	tracker := NewConnTracker(0)
	tracker.SetUserMap(map[string]int{"test-user": 11})
	tracker.SetAdmissionFunc(func(int, string, int) (func(), bool) { return nil, false })
	var n atomic.Int64
	tracker.SetDeviceChangeFunc(func() { n.Add(1) })
	conn := &testConn{}
	tracker.RoutedConnection(context.Background(), conn,
		testInboundContext("test-user", "192.0.2.11"), nil, nil)
	if !conn.closed {
		t.Fatal("rejected connection was not closed")
	}
	if n.Load() != 0 {
		t.Fatalf("rejected device generated %d events", n.Load())
	}
}
