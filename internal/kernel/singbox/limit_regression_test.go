package singbox

import (
	"bytes"
	"context"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

func TestLimitedTCPWriteAcrossBursts(t *testing.T) {
	tracker := NewConnTracker(0)
	tracker.SetUserMap(map[string]int{"test-user": 1})
	lim := rate.NewLimiter(1000000, 16)
	tracker.SetSpeedLimitFunc(func(string) *rate.Limiter { return lim })
	base := &testConn{}
	conn := tracker.RoutedConnection(context.Background(), base, testInboundContext("test-user", "192.0.2.1"), nil, nil)
	defer conn.Close()
	payload := bytes.Repeat([]byte("abc"), 100)
	n, err := conn.Write(payload)
	if err != nil || n != len(payload) {
		t.Fatalf("write = %d, %v; want %d", n, err, len(payload))
	}
	if !bytes.Equal(bytes.Join(base.writes, nil), payload) {
		t.Fatal("payload corrupted across burst boundaries")
	}
	for _, chunk := range base.writes {
		if len(chunk) > 16 {
			t.Fatal("write exceeds burst")
		}
	}
	traffic, _, _ := tracker.GetUserTraffic()
	if traffic[1] != [2]int64{0, int64(len(payload))} {
		t.Fatalf("incorrect accounting: %v", traffic)
	}
}

func TestSharedLimiterAcrossTCPConnections(t *testing.T) {
	tracker := NewConnTracker(0)
	tracker.SetUserMap(map[string]int{"test-user": 1})
	lim := rate.NewLimiter(100, 10)
	tracker.SetSpeedLimitFunc(func(string) *rate.Limiter { return lim })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	first := tracker.RoutedConnection(ctx, &testConn{}, testInboundContext("test-user", "192.0.2.1"), nil, nil)
	defer first.Close()
	second := tracker.RoutedConnection(ctx, &testConn{}, testInboundContext("test-user", "192.0.2.2"), nil, nil)
	defer second.Close()
	start := time.Now()
	for _, c := range []interface{ Write([]byte) (int, error) }{first, second} {
		if n, err := c.Write(make([]byte, 10)); err != nil || n != 10 {
			t.Fatalf("write = %d, %v", n, err)
		}
	}
	if elapsed := time.Since(start); elapsed < 80*time.Millisecond {
		t.Fatalf("shared limiter was bypassed: %v", elapsed)
	}
	traffic, _, _ := tracker.GetUserTraffic()
	if traffic[1] != [2]int64{0, 20} {
		t.Fatalf("incorrect accounting: %v", traffic)
	}
}

func TestUDPUnwrapAccountingDirections(t *testing.T) {
	stats := &userStats{}
	conn := &trackedPacketConn{us: stats}
	_, reads := conn.UnwrapPacketReader()
	_, writes := conn.UnwrapPacketWriter()
	for _, count := range reads {
		count(123)
	}
	for _, count := range writes {
		count(456)
	}
	if stats.upload.Load() != 123 || stats.download.Load() != 456 {
		t.Fatalf("incorrect UDP accounting: upload=%d download=%d", stats.upload.Load(), stats.download.Load())
	}
}

func TestUDPPacketBudgetLargerThanBurst(t *testing.T) {
	lim := rate.NewLimiter(1000, 10)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	if err := waitPacketBudget(ctx, lim, 100); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < 70*time.Millisecond {
		t.Fatalf("oversized datagram bypassed limit: %v", elapsed)
	}
}

func TestUDPPacketBudgetCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waitPacketBudget(ctx, rate.NewLimiter(1, 1), 100); err == nil {
		t.Fatal("canceled packet budget unexpectedly succeeded")
	}
}

func TestUDPPacketBudgetInvalidBurst(t *testing.T) {
	if err := waitPacketBudget(context.Background(), rate.NewLimiter(100, 0), 1); err == nil {
		t.Fatal("zero burst unexpectedly accepted data")
	}
}

func TestLimitedUDPDoesNotAdvertiseReplaceableIO(t *testing.T) {
	conn := &trackedPacketConn{limiter: rate.NewLimiter(100, 10)}
	if conn.ReaderReplaceable() || conn.WriterReplaceable() {
		t.Fatal("limited datagram path must not advertise replaceable I/O")
	}
}

func TestRemovedUserClosesOnlyTheirConnections(t *testing.T) {
	tracker := NewConnTracker(0)
	tracker.SetUserMap(map[string]int{"removed": 1, "kept": 2})
	first, second := &testConn{}, &testConn{}
	a := tracker.RoutedConnection(context.Background(), first, testInboundContext("removed", "192.0.2.1"), nil, nil)
	b := tracker.RoutedConnection(context.Background(), second, testInboundContext("kept", "192.0.2.2"), nil, nil)
	defer b.Close()
	if _, err := a.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	tracker.SetUserMap(map[string]int{"kept": 2})
	if !first.closed || second.closed {
		t.Fatal("wrong connection revoked")
	}
	if tracker.ActiveCount() != 1 {
		t.Fatal("revoked connection still counted")
	}
	traffic, _, _ := tracker.GetUserTraffic()
	if traffic[1][1] != 3 {
		t.Fatal("revocation lost billable bytes")
	}
	late := &testConn{}
	tracker.RoutedConnection(context.Background(), late, testInboundContext("removed", "192.0.2.3"), nil, nil)
	if !late.closed || tracker.ActiveCount() != 1 {
		t.Fatal("removed user re-admitted")
	}
}

func TestLimitedTCPDoesNotAdvertiseReplaceableIO(t *testing.T) {
	conn := &trackedConn{limiter: rate.NewLimiter(100, 10)}
	if conn.ReaderReplaceable() || conn.WriterReplaceable() {
		t.Fatal("limited TCP path must not advertise replaceable I/O")
	}
}

func TestGlobalDeviceAliasesCountOnce(t *testing.T) {
	tr := NewConnTracker(0)
	tr.UpdateGlobalDevices(map[int][]string{1: {"::ffff:192.0.2.1", "192.0.2.1", "[2001:db8::1]:443", "2001:0db8::1", "invalid"}})
	us := &userStats{ips: make(map[string]int)}
	if !tr.admitDevice(us, 1, "192.0.2.1", 2) {
		t.Fatal("known global IPv4 device rejected")
	}
	if !tr.admitDevice(us, 1, "2001:db8::1", 2) {
		t.Fatal("known global IPv6 device rejected")
	}
	if tr.admitDevice(us, 1, "192.0.2.2", 2) {
		t.Fatal("third distinct device admitted")
	}
	tr.ClearGlobalDevices()
	us.removeConn("2001:db8::1")
	if !tr.admitDevice(us, 1, "192.0.2.2", 2) {
		t.Fatal("released device slot not reusable")
	}
}

func TestGlobalDeviceAliasesDoNotConsumeExtraSlots(t *testing.T) {
	tr := NewConnTracker(0)
	tr.UpdateGlobalDevices(map[int][]string{1: {"::ffff:192.0.2.1", "192.0.2.1", "invalid"}})
	us := &userStats{ips: make(map[string]int)}
	if !tr.admitDevice(us, 1, "192.0.2.2", 2) {
		t.Fatal("duplicate aliases or invalid address consumed a second slot")
	}
	if tr.admitDevice(us, 1, "192.0.2.3", 2) {
		t.Fatal("third device admitted")
	}
}

func TestRemovedUserGlobalDevicesAreDiscarded(t *testing.T) {
	tr := NewConnTracker(0)
	tr.UpdateGlobalDevices(map[int][]string{7: {"192.0.2.7"}})
	tr.SetUserMap(map[string]int{"user-7": 7})
	tr.SetUserMap(map[string]int{})

	tr.globalMu.RLock()
	_, exists := tr.globalDevices[7]
	tr.globalMu.RUnlock()
	if exists {
		t.Fatal("global device snapshot survived user removal")
	}
}
