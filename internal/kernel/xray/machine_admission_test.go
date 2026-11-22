package xray

import (
	"testing"

	"github.com/cedar2025/xboard-node/internal/devicegate"
)

func TestXraySharedAdmissionUsesCanonicalUserID(t *testing.T) {
	authority := devicegate.New()
	const scope = "panel/machine24"
	first, ok := authority.Acquire(scope, 7, "192.0.2.1", 2)
	if !ok {
		t.Fatal("first different protocol reservation rejected")
	}
	defer first()
	second, ok := authority.Acquire(scope, 7, "192.0.2.2", 2)
	if !ok {
		t.Fatal("second different protocol reservation rejected")
	}
	defer second()

	ld := newTestDispatcher()
	// SOCKS5 uses a UUID as login; other Xray protocols use user@ID.
	ld.UpdateLimits(
		map[string]int{"socks-user-uuid": 7, "user@7": 7},
		map[string]int{"socks-user-uuid": 2, "user@7": 2},
		nil,
	)
	ld.SetAdmissionFunc(func(uid int, ip string, limit int) (func(), bool) {
		return authority.Acquire(scope, uid, ip, limit)
	})
	if release, ok := ld.reserveMachineSlot("socks-user-uuid", "192.0.2.3"); ok {
		if release != nil {
			release()
		}
		t.Fatal("SOCKS third IP bypassed cross-protocol limit")
	}
	if release, ok := ld.reserveMachineSlot("user@7", "192.0.2.3"); ok {
		if release != nil {
			release()
		}
		t.Fatal("another Xray identity bypassed cross-protocol limit")
	}
	release, ok := ld.reserveMachineSlot("socks-user-uuid", "::ffff:192.0.2.2")
	if !ok {
		t.Fatal("same IP should share device slot")
	}
	release()
	second()
	release, ok = ld.reserveMachineSlot("socks-user-uuid", "192.0.2.3")
	if !ok {
		t.Fatal("slot not released when second device left")
	}
	release()
}
