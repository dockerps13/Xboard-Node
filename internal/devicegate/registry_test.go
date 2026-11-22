package devicegate

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestCapAcrossProtocolsAndSameIP(t *testing.T) {
	r := New()
	a, ok := r.Acquire("panel-a/machine-24", 7, "192.0.2.1", 2)
	if !ok {
		t.Fatal("first protocol rejected")
	}
	defer a()
	b, ok := r.Acquire("panel-a/machine-24", 7, "::ffff:192.0.2.1", 2)
	if !ok {
		t.Fatal("same device on second protocol should share slot")
	}
	defer b()
	c, ok := r.Acquire("panel-a/machine-24", 7, "192.0.2.2", 2)
	if !ok {
		t.Fatal("second device rejected")
	}
	defer c()
	if _, ok := r.Acquire("panel-a/machine-24", 7, "192.0.2.3", 2); ok {
		t.Fatal("third IP bypassed two-device limit")
	}
	if got := r.DistinctIPs("panel-a/machine-24", 7); got != 2 {
		t.Fatalf("distinct IPs = %d, want 2", got)
	}

	// Releasing one protocol of a shared IP must not free its device slot.
	a()
	a()
	if _, ok := r.Acquire("panel-a/machine-24", 7, "192.0.2.3", 2); ok {
		t.Fatal("released shared IP too early")
	}
	b()
	d, ok := r.Acquire("panel-a/machine-24", 7, "192.0.2.3", 2)
	if !ok {
		t.Fatal("slot not released when last connection closed")
	}
	d()
	if got := r.DistinctIPs("panel-a/machine-24", 7); got != 1 {
		t.Fatalf("remaining slots = %d, want 1", got)
	}
}

func TestAdmissionIsolationAndFailClosed(t *testing.T) {
	r := New()
	for _, x := range []struct {
		scope string
		user  int
		ip    string
	}{
		{"", 1, "192.0.2.1"}, {"panel-a", 0, "192.0.2.1"},
		{"panel-a", 1, "invalid"}, {"panel-a", 1, "192.0.2.1:1234"},
	} {
		if _, ok := r.Acquire(x.scope, x.user, x.ip, 1); ok {
			t.Fatalf("accepted invalid %+v", x)
		}
	}
	a, ok := r.Acquire("panel-a", 1, "192.0.2.1", 1)
	if !ok {
		t.Fatal("first denied")
	}
	defer a()
	b, ok := r.Acquire("panel-b", 1, "192.0.2.2", 1)
	if !ok {
		t.Fatal("different panel scope denied")
	}
	defer b()
	c, ok := r.Acquire("panel-a", 2, "192.0.2.2", 1)
	if !ok {
		t.Fatal("different user denied")
	}
	defer c()
	if _, ok := r.Acquire("panel-a", 1, "192.0.2.2", 1); ok {
		t.Fatal("same user exceeded cap")
	}
}

func TestAtomicConcurrentAcquireAndRelease(t *testing.T) {
	r := New()
	first, ok := r.Acquire("panel-a", 1, "192.0.2.1", 2)
	if !ok {
		t.Fatal("first device rejected")
	}
	var wg sync.WaitGroup
	var admitted atomic.Int64
	releases := make(chan func(), 128)
	start := make(chan struct{})
	for i := 2; i <= 101; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			<-start
			ip := "192.0.2." + itoa(id)
			release, ok := r.Acquire("panel-a", 1, ip, 2)
			if ok {
				admitted.Add(1)
				releases <- release
			}
		}(i)
	}
	close(start)
	wg.Wait()
	close(releases)
	if got := admitted.Load(); got != 1 {
		t.Fatalf("concurrent arrivals admitted %d additional IPs, want 1", got)
	}
	for release := range releases {
		release()
	}
	first()
	if got := r.DistinctIPs("panel-a", 1); got != 0 {
		t.Fatalf("leaked %d IPs after release", got)
	}
}

func itoa(i int) string {
	// strconv imported separately to keep the test IP range explicit.
	if i < 10 {
		return string(rune('0' + i))
	}
	if i < 100 {
		return string(rune('0'+i/10)) + string(rune('0'+i%10))
	}
	return "1" + string(rune('0'+(i/10)%10)) + string(rune('0'+i%10))
}
