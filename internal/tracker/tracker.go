package tracker

import (
	"net/netip"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/cedar2025/xboard-node/internal/nlog"
)

// snapshot is an immutable point-in-time view of tracker state.
// It is swapped atomically so readers never block writers.
type snapshot struct {
	traffic   map[int][2]int64        // userID → [upload, download] delta
	aliveIPs  map[int]map[string]bool // userID → set of source IPs
	online    map[int]int             // userID → distinct IP count
	connCount int
	inSpeed   int64
	outSpeed  int64
}

// Tracker computes per-user traffic deltas from cumulative counters
// provided by the kernel, and accumulates totals for panel reporting.
//
// Architecture: the kernel maintains per-user atomic counters and IP sets.
// Each tick, the service calls Process() with cumulative per-user traffic.
// Tracker computes deltas against the previous cycle's values — O(users),
// not O(connections).
//
// Thread safety: Process() acquires mu to update internal state, then
// atomically publishes a new snapshot. All read methods (Flush*,
// CurrentOnline, LogStats, *Speed) read the snapshot lock-free.
// This eliminates contention between the 10s Process tick and the 60s
// flush/push tick.
type Tracker struct {
	mu sync.Mutex

	// lastSeen stores the cumulative traffic from the previous Process() call.
	// Protected by mu — only written by Process.
	lastSeen map[int][2]int64 // userID → [upload, download] cumulative

	// pending accumulates deltas between flushes.
	// Protected by mu — written by Process, drained by FlushTraffic.
	pendingTraffic map[int][2]int64

	// live holds the current snapshot, swapped atomically.
	// Readers load this pointer without any lock.
	live atomic.Pointer[snapshot]

	// Separate HTTP and WS delivery state. HTTP has a receipt; WS is
	// best effort and must never consume HTTP's pending offline notices.
	reportedAlive map[int]struct{}
	wsAlive       map[int]struct{}
}

func New() *Tracker {
	t := &Tracker{
		lastSeen:       make(map[int][2]int64),
		pendingTraffic: make(map[int][2]int64),
		reportedAlive:  make(map[int]struct{}),
		wsAlive:        make(map[int]struct{}),
	}
	// Publish initial empty snapshot.
	t.live.Store(&snapshot{
		traffic:  make(map[int][2]int64),
		aliveIPs: make(map[int]map[string]bool),
		online:   make(map[int]int),
	})
	return t
}

// Process computes per-user traffic deltas from cumulative kernel counters.
// Also stores alive IPs and connection count. O(users).
//
// This is the only writer to lastSeen and pendingTraffic.
// After computing deltas, it publishes a new snapshot for lock-free reads.
func (t *Tracker) Process(
	cumTraffic map[int][2]int64,
	kernelAliveIPs map[int]map[string]bool,
	connCount int,
) {
	t.mu.Lock()
	defer t.mu.Unlock()

	var cycleIn, cycleOut int64

	for uid, cum := range cumTraffic {
		prev := t.lastSeen[uid]
		deltaUp := cum[0] - prev[0]
		deltaDown := cum[1] - prev[1]

		// Guard against counter reset (kernel restart).
		if deltaUp < 0 {
			deltaUp = cum[0]
		}
		if deltaDown < 0 {
			deltaDown = cum[1]
		}

		t.lastSeen[uid] = cum

		if deltaUp > 0 || deltaDown > 0 {
			cur := t.pendingTraffic[uid]
			cur[0] += deltaUp
			cur[1] += deltaDown
			t.pendingTraffic[uid] = cur

			cycleOut += deltaUp
			cycleIn += deltaDown
		}
	}

	// Canonicalize addresses before counting and publishing.
	normalized := make(map[int]map[string]bool, len(kernelAliveIPs))
	online := make(map[int]int, len(kernelAliveIPs))
	for uid, ips := range kernelAliveIPs {
		set := make(map[string]bool, len(ips))
		for ip := range ips {
			if addr, err := netip.ParseAddr(ip); err == nil {
				set[addr.WithZone("").Unmap().String()] = true
			} else if ap, err := netip.ParseAddrPort(ip); err == nil {
				set[ap.Addr().WithZone("").Unmap().String()] = true
			}
		}
		if len(set) > 0 {
			normalized[uid] = set
			online[uid] = len(set)
		}
	}

	// Publish new snapshot (readers will see this atomically).
	t.live.Store(&snapshot{
		traffic:   copyTrafficMap(t.pendingTraffic),
		aliveIPs:  normalized,
		online:    online,
		connCount: connCount,
		inSpeed:   cycleIn,
		outSpeed:  cycleOut,
	})
}

// ClearLiveDevices publishes an empty connection snapshot after the kernel
// stops, without discarding pending traffic or unconfirmed offline notices.
func (t *Tracker) ClearLiveDevices() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.live.Store(&snapshot{
		traffic:  copyTrafficMap(t.pendingTraffic),
		aliveIPs: make(map[int]map[string]bool),
		online:   make(map[int]int),
	})
}

// FlushTraffic returns accumulated per-user traffic and resets the pending buffer.
// Lock-free: reads from live snapshot, then acquires mu only to drain.
func (t *Tracker) FlushTraffic() map[int][2]int64 {
	t.mu.Lock()
	data := t.pendingTraffic
	t.pendingTraffic = make(map[int][2]int64, len(data))
	t.mu.Unlock()
	return data
}

// RestoreTraffic adds traffic back (used when push to panel fails).
func (t *Tracker) RestoreTraffic(data map[int][2]int64) {
	t.mu.Lock()
	for uid, d := range data {
		cur := t.pendingTraffic[uid]
		cur[0] += d[0]
		cur[1] += d[1]
		t.pendingTraffic[uid] = cur
	}
	t.mu.Unlock()
}

// HasTraffic returns true if there is accumulated traffic to report.
func (t *Tracker) HasTraffic() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.pendingTraffic) > 0
}

// FlushAliveIPs prepares a full HTTP refresh plus explicit per-user
// offline notices. Only an acknowledged report may forget an offline user.
func (t *Tracker) FlushAliveIPs() map[int][]string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.prepareAlive(t.reportedAlive)
}

// FlushWSAliveIPs is independent of HTTP receipts. The HTTP refresh
// guarantees recovery if a best-effort WS notification is dropped.
func (t *Tracker) FlushWSAliveIPs() map[int][]string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := t.prepareAlive(t.wsAlive)
	for uid, ips := range out {
		if len(ips) == 0 {
			delete(t.wsAlive, uid)
		}
	}
	return out
}

func (t *Tracker) prepareAlive(known map[int]struct{}) map[int][]string {
	s := t.live.Load()
	out := make(map[int][]string, len(s.aliveIPs)+len(known))
	for uid := range known {
		out[uid] = []string{}
	}
	for uid, ips := range s.aliveIPs {
		list := make([]string, 0, len(ips))
		for ip := range ips {
			list = append(list, ip)
		}
		sort.Strings(list)
		out[uid] = list
		known[uid] = struct{}{}
	}
	return out
}

// AcknowledgeAliveIPs clears offline notices only after a successful HTTP
// receipt. A later reconnect will be included in the next full refresh.
func (t *Tracker) AcknowledgeAliveIPs(sent map[int][]string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for uid, ips := range sent {
		if len(ips) == 0 {
			delete(t.reportedAlive, uid)
		}
	}
}

// CurrentOnline returns a snapshot copy of user_id → device count (distinct IPs).
// Lock-free: reads from live snapshot.
func (t *Tracker) CurrentOnline() map[int]int {
	s := t.live.Load()
	cp := make(map[int]int, len(s.online))
	for k, v := range s.online {
		cp[k] = v
	}
	return cp
}

// RestoreAliveIPs retains the identities of potentially delivered reports.
// Never restore old addresses; the next report uses the latest snapshot.
func (t *Tracker) RestoreAliveIPs(data map[int][]string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for uid := range data {
		t.reportedAlive[uid] = struct{}{}
	}
}

// LogStats logs current tracking statistics.
// Lock-free: reads from live snapshot.
func (t *Tracker) LogStats() {
	s := t.live.Load()
	nlog.TrackerStats(s.connCount, len(s.online))
}

// ActiveConnections returns the last observed active connection count.
// Lock-free: reads from live snapshot.
func (t *Tracker) ActiveConnections() int {
	return t.live.Load().connCount
}

// TotalConnections is deprecated — no longer tracked per-connection.
// Returns 0 for backward compatibility.
func (t *Tracker) TotalConnections() int64 {
	return 0
}

// InboundSpeed returns the last observed inbound (download) speed in bytes/second.
// Lock-free: reads from live snapshot.
func (t *Tracker) InboundSpeed() int64 {
	return t.live.Load().inSpeed / 10
}

// OutboundSpeed returns the last observed outbound (upload) speed in bytes/second.
// Lock-free: reads from live snapshot.
func (t *Tracker) OutboundSpeed() int64 {
	return t.live.Load().outSpeed / 10
}

// copyTrafficMap creates a shallow copy of the traffic map.
func copyTrafficMap(src map[int][2]int64) map[int][2]int64 {
	dst := make(map[int][2]int64, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}
