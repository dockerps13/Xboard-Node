package xray

import (
	"context"
	"errors"
	"net/netip"
	"reflect"
	"sync"
	"sync/atomic"
	"time"
	_ "unsafe"

	xrayDispatcher "github.com/xtls/xray-core/app/dispatcher"
	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/buf"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/session"
	"github.com/xtls/xray-core/features/routing"
	"github.com/xtls/xray-core/transport"

	"github.com/cedar2025/xboard-node/internal/nlog"
)

// Access xray's internal config creator registry so we can replace the
// default dispatcher factory with ours. This runs AFTER xray's init()
// functions because our package imports xray (dependency order guarantee).
//
//go:linkname typeCreatorRegistry github.com/xtls/xray-core/common.typeCreatorRegistry
var typeCreatorRegistry map[reflect.Type]common.ConfigCreator

var origDispatcherFactory common.ConfigCreator

// globalLimitDispatcher is set when the factory creates a LimitDispatcher.
// The Xray kernel reads it to configure limits and get connections.
var globalLimitDispatcher atomic.Pointer[LimitDispatcher]

func init() {
	configType := reflect.TypeOf((*xrayDispatcher.Config)(nil))
	origDispatcherFactory = typeCreatorRegistry[configType]
	typeCreatorRegistry[configType] = limitDispatcherFactory
}

func limitDispatcherFactory(ctx context.Context, config interface{}) (interface{}, error) {
	orig, err := origDispatcherFactory(ctx, config)
	if err != nil {
		return nil, err
	}
	inner, ok := orig.(routing.Dispatcher)
	if !ok {
		return orig, nil
	}
	ld := &LimitDispatcher{
		inner:      orig,
		innerDisp:  inner,
		limitedIPs: make(map[string]map[string]int),
	}
	globalLimitDispatcher.Store(ld)
	nlog.Core().Debug("xray: limit dispatcher installed")
	return ld, nil
}

// LimitDispatcher wraps xray's DefaultDispatcher to enforce per-user
// admission checks before a request is dispatched into xray-core.
//
// It intentionally does NOT mutate transport.Link.Reader/Writer. Xray's
// mux/XUDP close path requires the original concrete *pipe.Reader to remain
// intact, so the dispatcher is limited to gate-keeping and safe connection
// lifecycle bookkeeping.
type LimitDispatcher struct {
	inner     interface{}        // original DefaultDispatcher (Feature + Dispatcher)
	innerDisp routing.Dispatcher // same object, typed as Dispatcher

	// limitedUsers: users with device limit > 0, protected by mu.
	// Needs deterministic IP ordering for kick decisions.
	mu               sync.RWMutex
	limitedIPs       map[string]map[string]int // email → sourceIP → refcount
	deviceLimits     map[string]int            // email → max devices
	emailToUID       map[string]int            // email → panel user ID
	globalDevices    map[int]map[string]bool
	globalUpdated    time.Time
	admissionFunc    func(userID int, sourceIP string, limit int) (func(), bool)
	deviceChangeFunc func()

	// unlimitedIPs: users without device limit — sync.Map for lock-free access.
	// Each entry is *ipCounter{ips sync.Map}.
	unlimitedIPs sync.Map // email → *ipCounter

	connCount atomic.Int64 // total active connections tracked by dispatcher
}

// ipCounter tracks IPs for unlimited users without any lock.
type ipCounter struct {
	ips sync.Map // sourceIP → *atomic.Int64 (refcount)
}

// aliveIPs returns a snapshot of distinct IPs.
func (ic *ipCounter) aliveIPs() map[string]bool {
	result := make(map[string]bool)
	ic.ips.Range(func(key, _ interface{}) bool {
		if rv, ok := ic.ips.Load(key); ok && rv.(*atomic.Int64).Load() > 0 {
			result[key.(string)] = true
		}
		return true
	})
	return result
}

// SetAdmissionFunc installs the process-local shared admission authority.
func (d *LimitDispatcher) SetAdmissionFunc(fn func(userID int, sourceIP string, limit int) (func(), bool)) {
	d.mu.Lock()
	d.admissionFunc = fn
	d.mu.Unlock()
}

// SetDeviceChangeFunc installs a nonblocking notification supplied by Service.
func (d *LimitDispatcher) SetDeviceChangeFunc(fn func()) {
	d.mu.Lock()
	d.deviceChangeFunc = fn
	d.mu.Unlock()
}

func (d *LimitDispatcher) notifyDeviceChange() {
	d.mu.RLock()
	fn := d.deviceChangeFunc
	d.mu.RUnlock()
	if fn != nil {
		fn()
	}
}

// reserveMachineSlot uses authenticated account identity to allocate one
// distinct-IP slot across every protocol service on this machine.
func (d *LimitDispatcher) reserveMachineSlot(email, sourceIP string) (func(), bool) {
	d.mu.RLock()
	fn := d.admissionFunc
	uid := d.emailToUID[email]
	limit := d.deviceLimits[email]
	d.mu.RUnlock()
	if fn == nil || limit <= 0 {
		return nil, true
	}
	return fn(uid, sourceIP, limit)
}

// ─── routing.Dispatcher ──────────────────────────────────────────────────────

func (d *LimitDispatcher) Dispatch(ctx context.Context, dest net.Destination) (*transport.Link, error) {
	email, sourceIP, isTCP, err := d.identifyAndCheck(ctx, dest)
	if err != nil {
		return nil, err
	}
	release, admitted := d.reserveMachineSlot(email, sourceIP)
	if !admitted {
		if isTCP {
			d.delConn(email, sourceIP)
		}
		return nil, errors.New("machine device limit exceeded")
	}

	link, err := d.innerDisp.Dispatch(ctx, dest)
	if err != nil {
		if email != "" && isTCP {
			d.delConn(email, sourceIP)
		}
		if release != nil {
			release()
		}
		return nil, err
	}

	if email != "" {
		d.trackLink(link, email, sourceIP, isTCP, release)
	}
	return link, nil
}

func (d *LimitDispatcher) DispatchLink(ctx context.Context, dest net.Destination, link *transport.Link) error {
	email, sourceIP, isTCP, err := d.identifyAndCheck(ctx, dest)
	if err != nil {
		return err
	}
	release, admitted := d.reserveMachineSlot(email, sourceIP)
	if !admitted {
		if isTCP {
			d.delConn(email, sourceIP)
		}
		return errors.New("machine device limit exceeded")
	}

	var cleanup func()
	if email != "" {
		cleanup = d.trackLink(link, email, sourceIP, isTCP, release)
	}
	err = d.innerDisp.DispatchLink(ctx, dest, link)
	if err != nil && cleanup != nil {
		cleanup()
	}
	return err
}

// identifyAndCheck extracts user identity from the session context, enforces
// device limits, and returns the user's email, source IP, and TCP flag.
// Returns a non-nil error only when the connection should be rejected.
func (d *LimitDispatcher) identifyAndCheck(ctx context.Context, dest net.Destination) (email, sourceIP string, isTCP bool, err error) {
	si := session.InboundFromContext(ctx)
	if si == nil || si.User == nil || len(si.User.Email) == 0 {
		return "", "", false, nil
	}
	email = si.User.Email
	sourceIP = si.Source.Address.IP().String()
	if canonical := normalizeDeviceIP(sourceIP); canonical != "" {
		sourceIP = canonical
	}
	isTCP = dest.Network == net.Network_TCP

	if d.checkDeviceLimit(email, sourceIP, isTCP) {
		nlog.Core().Debug("xray: device limit exceeded", "email", email, "ip", sourceIP)
		return "", "", false, errors.New("device limit exceeded for " + email)
	}
	return email, sourceIP, isTCP, nil
}

// trackLink records connection lifecycle without mutating xray-core owned
// transport primitives. This keeps mux/XUDP compatible while still allowing
// the dispatcher to release device-limit state when the link closes.
func (d *LimitDispatcher) trackLink(link *transport.Link, email, sourceIP string, isTCP bool, releases ...func()) func() {
	d.connCount.Add(1)
	var once sync.Once
	onClose := func() {
		once.Do(func() {
			if len(releases) > 0 && releases[0] != nil {
				releases[0]()
			}
			if isTCP {
				d.delConn(email, sourceIP)
			}
			d.connCount.Add(-1)
			d.notifyDeviceChange()
		})
	}

	d.notifyDeviceChange()
	link.Writer = &closeTrackingWriter{
		Writer:  link.Writer,
		onClose: onClose,
	}
	return onClose
}

// ─── features.Feature (delegated) ───────────────────────────────────────────

func (d *LimitDispatcher) Type() interface{} { return routing.DispatcherType() }

func (d *LimitDispatcher) Start() error {
	if s, ok := d.inner.(interface{ Start() error }); ok {
		return s.Start()
	}
	return nil
}

func (d *LimitDispatcher) Close() error {
	if c, ok := d.inner.(interface{ Close() error }); ok {
		return c.Close()
	}
	return nil
}

// ─── Limit management (called by Xray kernel) ──────────────────────────────

func (d *LimitDispatcher) UpdateLimits(emailToUID map[string]int, deviceLimits, _ map[string]int) {
	d.mu.Lock()
	d.emailToUID = emailToUID
	d.deviceLimits = deviceLimits
	d.mu.Unlock()

}

func (d *LimitDispatcher) ResetConns() {
	d.mu.Lock()
	d.limitedIPs = make(map[string]map[string]int)
	d.mu.Unlock()

	// Clear unlimited IPs
	d.unlimitedIPs.Range(func(key, _ interface{}) bool {
		d.unlimitedIPs.Delete(key)
		return true
	})

	d.connCount.Store(0)
}

// GetConnectionState returns dispatcher-tracked alive IPs and connection count.
// Traffic bytes are intentionally left to xray's built-in stats pipeline.
func (d *LimitDispatcher) GetConnectionState() (aliveIPs map[int]map[string]bool, connCount int) {
	d.mu.RLock()
	emailToUID := d.emailToUID
	limitedIPs := d.limitedIPs

	aliveIPs = make(map[int]map[string]bool)

	// Collect IPs from limited users (under RLock snapshot).
	for email, ipsMap := range limitedIPs {
		uid := emailToUID[email]
		if uid == 0 {
			continue
		}
		ipSet := make(map[string]bool, len(ipsMap))
		for ip := range ipsMap {
			ipSet[ip] = true
		}
		if len(ipSet) > 0 {
			aliveIPs[uid] = ipSet
		}
	}

	d.mu.RUnlock()

	// Collect IPs from unlimited users (lock-free).
	d.unlimitedIPs.Range(func(key, value interface{}) bool {
		email := key.(string)
		uid := emailToUID[email]
		if uid == 0 {
			return true
		}
		ic := value.(*ipCounter)
		if ips := ic.aliveIPs(); len(ips) > 0 {
			// Merge with limited IPs if any
			if existing, ok := aliveIPs[uid]; ok {
				for ip := range ips {
					existing[ip] = true
				}
			} else {
				aliveIPs[uid] = ips
			}
		}
		return true
	})

	connCount = int(d.connCount.Load())
	return
}

// ─── Internal helpers ───────────────────────────────────────────────────────

// checkDeviceLimit enforces per-user device limits.
// Fast path: unlimited users use lock-free sync.Map.
// Slow path: limited users use RWMutex with deterministic IP ordering.
func normalizeDeviceIP(ip string) string {
	if addr, err := netip.ParseAddr(ip); err == nil {
		return addr.WithZone("").Unmap().String()
	}
	if ap, err := netip.ParseAddrPort(ip); err == nil {
		return ap.Addr().WithZone("").Unmap().String()
	}
	return ""
}

func (d *LimitDispatcher) UpdateGlobalDevices(users map[int][]string) {
	next := make(map[int]map[string]bool, len(users))
	for uid, ips := range users {
		set := make(map[string]bool, len(ips))
		for _, ip := range ips {
			if normalized := normalizeDeviceIP(ip); normalized != "" {
				set[normalized] = true
			}
		}
		next[uid] = set
	}
	d.mu.Lock()
	d.globalDevices = next
	d.globalUpdated = time.Now()
	d.mu.Unlock()
}

func (d *LimitDispatcher) ClearGlobalDevices() {
	d.mu.Lock()
	d.globalDevices = nil
	d.globalUpdated = time.Time{}
	d.mu.Unlock()
}

func (d *LimitDispatcher) checkDeviceLimit(email, sourceIP string, isTCP bool) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	limit := d.deviceLimits[email]
	if limit <= 0 {
		if isTCP {
			v, _ := d.unlimitedIPs.LoadOrStore(email, &ipCounter{})
			ic := v.(*ipCounter)
			rv, _ := ic.ips.LoadOrStore(sourceIP, &atomic.Int64{})
			rv.(*atomic.Int64).Add(1)
		}
		return false
	}
	ips := d.limitedIPs[email]
	// Include existing connections from all aliases of this panel user.
	uid := d.emailToUID[email]
	known := make(map[string]bool)
	for alias, local := range d.limitedIPs {
		if alias == email || (uid != 0 && d.emailToUID[alias] == uid) {
			for ip, count := range local {
				if count > 0 {
					known[ip] = true
				}
			}
		}
	}
	d.unlimitedIPs.Range(func(key, value interface{}) bool {
		alias := key.(string)
		if alias == email || (uid != 0 && d.emailToUID[alias] == uid) {
			for ip := range value.(*ipCounter).aliveIPs() {
				known[ip] = true
			}
		}
		return true
	})
	if time.Since(d.globalUpdated) <= 60*time.Second {
		for ip := range d.globalDevices[uid] {
			known[ip] = true
		}
	}
	if !known[sourceIP] && len(known) >= limit {
		return true
	}
	if isTCP {
		if ips == nil {
			ips = make(map[string]int)
			d.limitedIPs[email] = ips
		}
		ips[sourceIP]++
	}
	return false
}

// delConn decrements the IP refcount when a connection closes.
func (d *LimitDispatcher) delConn(email, sourceIP string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if ips := d.limitedIPs[email]; ips[sourceIP] > 0 {
		ips[sourceIP]--
		if ips[sourceIP] == 0 {
			delete(ips, sourceIP)
		}
		if len(ips) == 0 {
			delete(d.limitedIPs, email)
		}
		return
	}
	if v, ok := d.unlimitedIPs.Load(email); ok {
		ic := v.(*ipCounter)
		if rv, ok := ic.ips.Load(sourceIP); ok {
			if rv.(*atomic.Int64).Add(-1) <= 0 {
				ic.ips.Delete(sourceIP)
			}
		}
	}
}

type closeTrackingWriter struct {
	buf.Writer
	onClose func()
	closed  atomic.Bool
}

func (w *closeTrackingWriter) Close() error {
	if w.closed.CompareAndSwap(false, true) {
		w.onClose()
	}
	return common.Close(w.Writer)
}

func (w *closeTrackingWriter) Interrupt() {
	if w.closed.CompareAndSwap(false, true) {
		w.onClose()
	}
	common.Interrupt(w.Writer)
}
