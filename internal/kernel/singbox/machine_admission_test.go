package singbox

import (
	"context"
	"testing"

	"github.com/cedar2025/xboard-node/internal/devicegate"
)

func TestSharedMachineAdmissionRejectsThirdCrossProtocolIP(t *testing.T) {
	authority := devicegate.New()
	const scope = "panel/machine24"
	// One protocol has already reserved two devices for the same panel user.
	first, ok := authority.Acquire(scope, 7, "192.0.2.1", 2)
	if !ok {
		t.Fatal("first protocol should admit")
	}
	defer first()
	second, ok := authority.Acquire(scope, 7, "192.0.2.2", 2)
	if !ok {
		t.Fatal("second protocol should admit")
	}
	defer second()

	tracker := NewConnTracker(0)
	tracker.SetUserMap(map[string]int{"test-user": 7})
	tracker.SetDeviceLimitFunc(func(string) (int, bool) { return 2, true })
	tracker.SetAdmissionFunc(func(uid int, ip string, limit int) (func(), bool) {
		return authority.Acquire(scope, uid, ip, limit)
	})

	third := &testConn{}
	tracker.RoutedConnection(context.Background(), third, testInboundContext("test-user", "192.0.2.3"), nil, nil)
	if !third.closed {
		t.Fatal("third client crossed shared machine device cap")
	}
	if authority.DistinctIPs(scope, 7) != 2 {
		t.Fatal("rejected connection reserved a slot")
	}

	// Same client IP on another protocol shares its occupied slot.
	shared := &testConn{}
	conn := tracker.RoutedConnection(context.Background(), shared, testInboundContext("test-user", "192.0.2.2"), nil, nil)
	if shared.closed {
		t.Fatal("same IP rejected despite existing reservation")
	}
	if authority.DistinctIPs(scope, 7) != 2 {
		t.Fatal("same IP consumed another device slot")
	}
	conn.Close()
	second()
	if authority.DistinctIPs(scope, 7) != 1 {
		t.Fatal("last connection release did not free slot")
	}
	fresh := &testConn{}
	replacement := tracker.RoutedConnection(context.Background(), fresh, testInboundContext("test-user", "192.0.2.3"), nil, nil)
	if fresh.closed {
		t.Fatal("third device not admitted after release")
	}
	replacement.Close()
}
