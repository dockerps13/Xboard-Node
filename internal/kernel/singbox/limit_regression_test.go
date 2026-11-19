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
