package singbox

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing/common/buf"
	singM "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"golang.org/x/time/rate"

	"github.com/cedar2025/xboard-node/internal/kernel/obfs"
	"github.com/cedar2025/xboard-node/internal/nlog"
)

// ipPool caches ipSnapshot maps to reduce allocations.
var ipPool = sync.Pool{
	New: func() interface{} {
		return make(map[string]struct{}, 16)
	},
}

// ─── Per-user statistics ────────────────────────────────────────────────────

// userStats holds per-user traffic counters and alive IP tracking.
// Traffic counters are lock-free atomics (read on every packet).
// IP tracking uses a lightweight mutex (only touched at connect/disconnect).
type userStats struct {
	upload   atomic.Int64
	download atomic.Int64

	mu        sync.RWMutex   // RWMutex for concurrent reads
	ips       map[string]int // sourceIP → refcount (number of active conns from that IP)
	connCount int            // total active connections
}

// addConn registers a new connection from sourceIP.
func (u *userStats) addConn(sourceIP string) {
	u.mu.Lock()
	u.connCount++
	u.ips[sourceIP]++
	u.mu.Unlock()
}

// removeConn unregisters a connection from sourceIP.
func (u *userStats) removeConn(sourceIP string) {
	u.mu.Lock()
	u.connCount--
	u.ips[sourceIP]--
	if u.ips[sourceIP] <= 0 {
		delete(u.ips, sourceIP)
	}
	u.mu.Unlock()
}

// distinctIPs returns the number of distinct IPs currently connected.
func (u *userStats) distinctIPs() int {
	u.mu.Lock()
	n := len(u.ips)
	u.mu.Unlock()
	return n
}

// ipSnapshot returns a pooled copy of the current IP set.
func (u *userStats) ipSnapshot() map[string]struct{} {
	cp := ipPool.Get().(map[string]struct{})
	for k := range cp {
		delete(cp, k)
	}
	u.mu.Lock()
	for ip := range u.ips {
		cp[ip] = struct{}{}
	}
	u.mu.Unlock()
	return cp
}

// releaseIPSnapshot returns the snapshot to pool.
// Large maps are discarded to prevent memory bloat.
func releaseIPSnapshot(m map[string]struct{}) {
	// Discard large maps to prevent pool memory bloat
	if len(m) > 64 {
		return
	}
	ipPool.Put(m)
}

// aliveIPList returns the set of alive IPs as a boolean map (for reporting).
func (u *userStats) aliveIPList() map[string]bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.ips) == 0 {
		return nil
	}
	result := make(map[string]bool, len(u.ips))
	for ip := range u.ips {
		result[ip] = true
	}
	return result
}

// ─── ConnTracker ────────────────────────────────────────────────────────────

// ConnTracker provides per-user traffic counting, alive IP tracking, device
// limit gate-keeping, and per-user speed limiting for the sing-box kernel.
//
// Architecture: instead of maintaining a per-connection map (O(connections)),
// it maintains per-user atomic counters and IP refcount sets (O(users)).
// This means:
//   - GetUserTraffic is O(users), not O(connections)
//   - No Snapshot/pending buffer needed
//   - No per-connection map lock contention
//   - Bytes are counted directly into per-user atomics via CountFunc callbacks
//
// The per-connection wrapper (trackedConn) is still needed for:
//   - Hooking into sing's ReadCounter/WriteCounter for zero-copy byte counting
//   - Close callback to decrement IP refcounts
//   - Per-connection rate limit token accumulation (amortized WaitN)
type trackedReference struct {
	uuid   string
	closer io.Closer
}

type ConnTracker struct {
	usersMu         sync.RWMutex
	users           map[int]*userStats          // userID → stats
	identityAliases map[string]string           // protocol username -> canonical UUID
	uuidMap         map[string]int              // UUID → userID (for lookup in RoutedConnection)
	connMap         map[string]trackedReference // connID → conn (only for force-close support)

	idCounter atomic.Int64

	// speedLimitFunc resolves a user UUID to a *rate.Limiter.
	speedLimitFunc atomic.Pointer[func(uuid string) *rate.Limiter]

	// deviceLimitFunc resolves a user UUID to their device limit.
	deviceLimitFunc  atomic.Pointer[func(uuid string) (int, bool)]
	admissionFunc    atomic.Pointer[func(userID int, sourceIP string, limit int) (func(), bool)]
	deviceChangeFunc atomic.Pointer[func()]

	// Multi-node device state from panel
	globalDevices    map[int]map[string]bool // userID → IP → exists
	globalMu         sync.RWMutex
	globalLastUpdate time.Time
}

// NewConnTracker creates a tracker.
func NewConnTracker(_ int) *ConnTracker {
	return &ConnTracker{
		users:         make(map[int]*userStats),
		uuidMap:       make(map[string]int),
		connMap:       make(map[string]trackedReference),
		globalDevices: make(map[int]map[string]bool),
	}
}

// SetSpeedLimitFunc configures the per-user speed limit lookup.
func (t *ConnTracker) SetSpeedLimitFunc(fn func(uuid string) *rate.Limiter) {
	t.speedLimitFunc.Store(&fn)
}

// SetDeviceLimitFunc configures the per-user device limit lookup for gate-keeping.
func (t *ConnTracker) SetDeviceLimitFunc(fn func(uuid string) (int, bool)) {
	t.deviceLimitFunc.Store(&fn)
}

// SetAdmissionFunc configures shared per-machine atomic admission.
func (t *ConnTracker) SetAdmissionFunc(fn func(userID int, sourceIP string, limit int) (func(), bool)) {
	t.admissionFunc.Store(&fn)
}

// SetDeviceChangeFunc supplies a nonblocking notification to report IP changes.
func (t *ConnTracker) SetDeviceChangeFunc(fn func()) {
	t.deviceChangeFunc.Store(&fn)
}

func (t *ConnTracker) notifyDeviceChange() {
	if fn := t.deviceChangeFunc.Load(); fn != nil {
		(*fn)()
	}
}

// SetUserMap replaces the UUID→userID mapping and ensures per-user stats
// structs exist for all users. Old users that are no longer present keep
// their stats until their connections drain.
func (t *ConnTracker) SetUserMap(m map[string]int) {
	t.setUserIdentities(m, nil)
}

func (t *ConnTracker) setUserIdentities(m map[string]int, aliases map[string]string) {
	t.usersMu.Lock()
	var revoked []io.Closer
	for _, ref := range t.connMap {
		if ref.uuid != "" && (m[ref.uuid] == 0 || m[ref.uuid] != t.uuidMap[ref.uuid]) {
			revoked = append(revoked, ref.closer)
		}
	}
	t.identityAliases = aliases
	t.uuidMap = m
	for _, uid := range m {
		if _, ok := t.users[uid]; !ok {
			t.users[uid] = &userStats{ips: make(map[string]int)}
		}
	}
	t.usersMu.Unlock()

	// A removed user must not leave a global device snapshot behind. Besides
	// wasting memory, stale entries could grant a re-created user ID access to
	// an old node's device allowance before the next panel snapshot arrives.
	t.globalMu.Lock()
	for uid := range t.globalDevices {
		keep := false
		for _, mapped := range m {
			if mapped == uid {
				keep = true
				break
			}
		}
		if !keep {
			delete(t.globalDevices, uid)
		}
	}
	t.globalMu.Unlock()
	for _, c := range revoked {
		_ = c.Close()
	}
}

// UpdateGlobalDevices syncs global device state from panel.
func canonicalDeviceIP(ip string) string {
	if addr, err := netip.ParseAddr(ip); err == nil {
		return addr.WithZone("").Unmap().String()
	}
	if ap, err := netip.ParseAddrPort(ip); err == nil {
		return ap.Addr().WithZone("").Unmap().String()
	}
	return ""
}

func (t *ConnTracker) UpdateGlobalDevices(users map[int][]string) {
	t.globalMu.Lock()
	t.globalDevices = make(map[int]map[string]bool, len(users))
	for uid, ips := range users {
		m := make(map[string]bool, len(ips))
		for _, ip := range ips {
			if normalized := canonicalDeviceIP(ip); normalized != "" {
				m[normalized] = true
			}
		}
		t.globalDevices[uid] = m
	}
	t.globalLastUpdate = time.Now()
	t.globalMu.Unlock()
	nlog.Core().Debug("global device state updated", "users", len(users))
}

// ClearGlobalDevices resets global device state (on WS disconnect).
func (t *ConnTracker) ClearGlobalDevices() {
	t.globalMu.Lock()
	t.globalDevices = make(map[int]map[string]bool)
	t.globalLastUpdate = time.Time{}
	t.globalMu.Unlock()
	nlog.Core().Debug("global device state cleared")
}

// ─── adapter.ConnectionTracker ──────────────────────────────────────────────

// RoutedConnection wraps a TCP conn to count bytes per-user, track IPs,
// and optionally rate-limit. Gate-keeps device limits at connection time.
func (t *ConnTracker) RoutedConnection(
	ctx context.Context, conn net.Conn,
	metadata adapter.InboundContext,
	_ adapter.Rule, _ adapter.Outbound,
) net.Conn {
	uuid := metadata.User
	sourceIP := metadata.Source.Addr.String()
	// HTTP simple-obfs terminates before sing-box and connects over
	// loopback. Recover the real client IP using the TCP source port,
	// so device limits and online IP reporting remain accurate.
	if realIP, ok := obfs.ResolveOrigin(sourceIP, metadata.Source.Port); ok {
		sourceIP = realIP
	}
	if normalized := canonicalDeviceIP(sourceIP); normalized != "" {
		sourceIP = normalized
	}

	t.usersMu.RLock()
	if canonical, ok := t.identityAliases[uuid]; ok {
		uuid = canonical
	}
	uid := t.uuidMap[uuid]
	us := t.users[uid]
	t.usersMu.RUnlock()

	limit := 0
	if dlf := t.deviceLimitFunc.Load(); dlf != nil {
		if value, ok := (*dlf)(uuid); ok {
			limit = value
		}
	}
	if !t.admitDevice(us, uid, sourceIP, limit) {
		conn.Close()
		return conn
	}
	var releaseAdmission func()
	if admission := t.admissionFunc.Load(); admission != nil {
		var ok bool
		releaseAdmission, ok = (*admission)(uid, sourceIP, limit)
		if !ok {
			us.removeConn(sourceIP)
			conn.Close()
			return conn
		}
	}

	connID := t.nextID()

	var lim *rate.Limiter
	if slf := t.speedLimitFunc.Load(); slf != nil {
		lim = (*slf)(uuid)
	}

	wrapped := &trackedConn{
		Conn:             conn,
		tracker:          t,
		us:               us,
		userID:           uid,
		connID:           connID,
		sourceIP:         sourceIP,
		releaseAdmission: releaseAdmission,
		limiter:          lim,
		ctx:              ctx,
	}
	if !t.registerConnection(connID, uuid, uid, wrapped) {
		_ = wrapped.Close()
		return conn
	}
	t.notifyDeviceChange()
	return wrapped
}

// RoutedPacketConnection tracks UDP traffic and supports forced closure.
func (t *ConnTracker) RoutedPacketConnection(
	ctx context.Context, conn N.PacketConn,
	metadata adapter.InboundContext,
	_ adapter.Rule, _ adapter.Outbound,
) N.PacketConn {
	uuid := metadata.User
	sourceIP := metadata.Source.Addr.String()
	if normalized := canonicalDeviceIP(sourceIP); normalized != "" {
		sourceIP = normalized
	}

	t.usersMu.RLock()
	if canonical, ok := t.identityAliases[uuid]; ok {
		uuid = canonical
	}
	uid := t.uuidMap[uuid]
	us := t.users[uid]
	t.usersMu.RUnlock()

	limit := 0
	if dlf := t.deviceLimitFunc.Load(); dlf != nil {
		if value, ok := (*dlf)(uuid); ok {
			limit = value
		}
	}
	if !t.admitDevice(us, uid, sourceIP, limit) {
		conn.Close()
		return conn
	}
	var releaseAdmission func()
	if admission := t.admissionFunc.Load(); admission != nil {
		var ok bool
		releaseAdmission, ok = (*admission)(uid, sourceIP, limit)
		if !ok {
			us.removeConn(sourceIP)
			conn.Close()
			return conn
		}
	}

	connID := t.nextID()

	var lim *rate.Limiter
	if slf := t.speedLimitFunc.Load(); slf != nil {
		lim = (*slf)(uuid)
	}

	wrapped := &trackedPacketConn{
		PacketConn:       conn,
		tracker:          t,
		us:               us,
		userID:           uid,
		connID:           connID,
		sourceIP:         sourceIP,
		releaseAdmission: releaseAdmission,
		limiter:          lim,
		ctx:              ctx,
	}
	if !t.registerConnection(connID, uuid, uid, wrapped) {
		_ = wrapped.Close()
		return conn
	}
	t.notifyDeviceChange()
	return wrapped
}

// admitDevice checks and registers under one per-user lock. A device is
// a distinct source IP, not a physical device. Existing local connections
// are retained; a new IP cannot displace an already admitted IP.
func (t *ConnTracker) admitDevice(us *userStats, userID int, sourceIP string, limit int) bool {
	if us == nil {
		return true
	}
	us.mu.Lock()
	defer us.mu.Unlock()
	if limit > 0 && us.ips[sourceIP] == 0 {
		t.globalMu.RLock()
		global := t.globalDevices[userID]
		fresh := time.Since(t.globalLastUpdate) <= 60*time.Second
		count := len(us.ips)
		if fresh {
			for ip := range global {
				if us.ips[ip] == 0 {
					count++
				}
			}
		}
		known := fresh && global[sourceIP]
		t.globalMu.RUnlock()
		if !known && count >= limit {
			return false
		}
	}
	us.connCount++
	us.ips[sourceIP]++
	return true
}

func (t *ConnTracker) nextID() string {
	return "sb-" + formatInt36(t.idCounter.Add(1))
}

func formatInt36(n int64) string {
	const digits = "0123456789abcdefghijklmnopqrstuvwxyz"
	if n == 0 {
		return "0"
	}
	var buf [13]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = digits[n%36]
		n /= 36
	}
	return string(buf[i:])
}

// ─── Public API ─────────────────────────────────────────────────────────────

// GetUserTraffic returns per-user cumulative traffic and alive IPs.
// This is O(users), not O(connections).
func (t *ConnTracker) GetUserTraffic() (traffic map[int][2]int64, aliveIPs map[int]map[string]bool, connCount int) {
	t.usersMu.RLock()
	traffic = make(map[int][2]int64, len(t.users))
	aliveIPs = make(map[int]map[string]bool, len(t.users))
	for uid, us := range t.users {
		up := us.upload.Load()
		down := us.download.Load()
		if up > 0 || down > 0 {
			traffic[uid] = [2]int64{up, down}
		}

		ips := us.aliveIPList()
		if ips != nil {
			aliveIPs[uid] = ips
		}

		us.mu.Lock()
		connCount += us.connCount
		us.mu.Unlock()
	}
	t.usersMu.RUnlock()
	return
}

// registerConnection rechecks the identity under the same lock as user
// updates, so an in-flight authentication cannot register after revocation.
func (t *ConnTracker) registerConnection(id, uuid string, uid int, c io.Closer) bool {
	t.usersMu.Lock()
	defer t.usersMu.Unlock()
	if uuid != "" && (uid == 0 || t.uuidMap[uuid] != uid) {
		return false
	}
	t.connMap[id] = trackedReference{uuid: uuid, closer: c}
	return true
}

func (t *ConnTracker) CloseByID(id string) bool {
	t.usersMu.RLock()
	ref, ok := t.connMap[id]
	t.usersMu.RUnlock()
	if ok {
		_ = ref.closer.Close()
	}
	return ok
}

func (t *ConnTracker) CloseByUUID(uuid string) int {
	t.usersMu.RLock()
	var conns []io.Closer
	for _, ref := range t.connMap {
		if ref.uuid == uuid {
			conns = append(conns, ref.closer)
		}
	}
	t.usersMu.RUnlock()
	for _, c := range conns {
		_ = c.Close()
	}
	return len(conns)
}

// ActiveCount returns the total number of active connections.
func (t *ConnTracker) ActiveCount() int {
	t.usersMu.RLock()
	total := 0
	for _, us := range t.users {
		us.mu.Lock()
		total += us.connCount
		us.mu.Unlock()
	}
	t.usersMu.RUnlock()
	return total
}

// removeConnRef removes the connection reference from connMap on close.
func (t *ConnTracker) removeConnRef(connID string) {
	t.usersMu.Lock()
	delete(t.connMap, connID)
	t.usersMu.Unlock()
}

// ─── trackedConn (TCP) ──────────────────────────────────────────────────────

// RateLimitedWriter wraps a Writer with non-blocking rate limiting.
// Uses AllowN for instant checks and context-aware waiting when needed.
type RateLimitedWriter struct {
	writer  io.Writer
	limiter *rate.Limiter
	ctx     context.Context
}

// Write implements io.Writer with rate limiting.
func (w *RateLimitedWriter) Write(b []byte) (int, error) {
	// Truncate to burst size
	if burst := w.limiter.Burst(); len(b) > burst {
		b = b[:burst]
	}

	// Fast path: check if tokens available (non-blocking)
	if w.limiter.AllowN(time.Now(), len(b)) {
		return w.writer.Write(b)
	}

	// Slow path: wait for tokens with context cancellation support
	resv := w.limiter.ReserveN(time.Now(), len(b))
	if delay := resv.Delay(); delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-timer.C:
			// Tokens available, proceed
		case <-w.ctx.Done():
			resv.Cancel()
			return 0, w.ctx.Err()
		}
	}

	return w.writer.Write(b)
}

// RateLimitedReadCloser wraps a Reader with non-blocking rate limiting.
type RateLimitedReadCloser struct {
	reader  io.Reader
	limiter *rate.Limiter
	ctx     context.Context
}

// Read implements io.Reader with rate limiting.
func (r *RateLimitedReadCloser) Read(b []byte) (int, error) {
	// Truncate to burst size
	if burst := r.limiter.Burst(); len(b) > burst {
		b = b[:burst]
	}

	n, err := r.reader.Read(b)
	if n > 0 && r.limiter != nil {
		// Fast path: check if tokens available
		if !r.limiter.AllowN(time.Now(), n) {
			// Slow path: wait with context cancellation
			resv := r.limiter.ReserveN(time.Now(), n)
			if delay := resv.Delay(); delay > 0 {
				timer := time.NewTimer(delay)
				defer timer.Stop()
				select {
				case <-timer.C:
					// Tokens available
				case <-r.ctx.Done():
					resv.Cancel()
					return n, r.ctx.Err()
				}
			}
		}
	}
	return n, err
}

type trackedConn struct {
	net.Conn
	tracker          *ConnTracker
	us               *userStats // per-user stats (upload/download atomics + IP tracking)
	userID           int        // user ID for device tracking
	connID           string
	sourceIP         string
	releaseAdmission func()
	limiter          *rate.Limiter
	ctx              context.Context
	closed           atomic.Bool
}

func (c *trackedConn) Read(b []byte) (int, error) {
	if c.limiter != nil {
		burst := c.limiter.Burst()
		if burst <= 0 && len(b) > 0 {
			return 0, fmt.Errorf("invalid rate limiter burst")
		}
		if len(b) > burst {
			b = b[:burst]
		}
	}
	n, err := c.Conn.Read(b)
	if n > 0 {
		if c.us != nil {
			c.us.upload.Add(int64(n))
		}
		if limitErr := waitPacketBudget(c.ctx, c.limiter, n); limitErr != nil {
			return n, limitErr
		}
	}
	return n, err
}

func (c *trackedConn) Write(b []byte) (int, error) {
	// net.Conn.Write must not return a short successful write just because
	// the limiter burst is smaller than the caller's buffer.
	total := 0
	for len(b) > 0 {
		n := len(b)
		if c.limiter != nil {
			if burst := c.limiter.Burst(); burst <= 0 {
				return total, fmt.Errorf("invalid rate limiter burst")
			} else if n > burst {
				n = burst
			}
			if err := c.limiter.WaitN(c.ctx, n); err != nil {
				return total, err
			}
		}
		written, err := c.Conn.Write(b[:n])
		total += written
		if written > 0 && c.us != nil {
			c.us.download.Add(int64(written))
		}
		if err != nil {
			return total, err
		}
		if written != n {
			return total, io.ErrShortWrite
		}
		b = b[written:]
	}
	return total, nil
}

func (c *trackedConn) Close() error {
	if c.closed.CompareAndSwap(false, true) {
		if c.us != nil {
			c.us.removeConn(c.sourceIP)
		}
		if c.releaseAdmission != nil {
			c.releaseAdmission()
		}
		c.tracker.removeConnRef(c.connID)
		c.tracker.notifyDeviceChange()
		return c.Conn.Close()
	}
	return nil
}

// makeCountFunc builds a CountFunc for zero-copy byte counting via sing's
// ReadCounter/WriteCounter unwrap interfaces.
func (c *trackedConn) makeCountFunc(counter *atomic.Int64) N.CountFunc {
	return func(n int64) {
		counter.Add(n)
		if c.limiter != nil && n > 0 {
			_ = waitPacketBudget(c.ctx, c.limiter, int(n))
		}
	}
}

func (c *trackedConn) UnwrapReader() (io.Reader, []N.CountFunc) {
	if c.us == nil {
		return c.Conn, nil
	}
	return c.Conn, []N.CountFunc{c.makeCountFunc(&c.us.upload)} // 从入站读取 = 用户上传
}

func (c *trackedConn) UnwrapWriter() (io.Writer, []N.CountFunc) {
	if c.us == nil {
		return c.Conn, nil
	}
	return c.Conn, []N.CountFunc{c.makeCountFunc(&c.us.download)} // 向入站写入 = 用户下载
}

func (c *trackedConn) Upstream() any           { return c.Conn }
func (c *trackedConn) ReaderReplaceable() bool { return c.limiter == nil }
func (c *trackedConn) WriterReplaceable() bool { return c.limiter == nil }

// ─── trackedPacketConn (UDP / QUIC) ─────────────────────────────────────────

type trackedPacketConn struct {
	N.PacketConn
	tracker          *ConnTracker
	us               *userStats
	userID           int
	connID           string
	sourceIP         string
	releaseAdmission func()
	limiter          *rate.Limiter
	ctx              context.Context
	closed           atomic.Bool
}

// waitPacketBudget charges an entire datagram without splitting it on the
// wire. WaitN rejects requests larger than Burst; consume bounded chunks
// instead, respecting cancellation and keeping the datagram intact.
func waitPacketBudget(ctx context.Context, limiter *rate.Limiter, n int) error {
	if limiter == nil {
		return nil
	}
	for n > 0 {
		burst := limiter.Burst()
		if burst <= 0 {
			return fmt.Errorf("invalid rate limiter burst")
		}
		chunk := n
		if chunk > burst {
			chunk = burst
		}
		if err := limiter.WaitN(ctx, chunk); err != nil {
			return err
		}
		n -= chunk
	}
	return nil
}

func (c *trackedPacketConn) ReadPacket(buffer *buf.Buffer) (singM.Socksaddr, error) {
	dest, err := c.PacketConn.ReadPacket(buffer)
	if err != nil {
		return dest, err
	}
	n := buffer.Len()
	if c.us != nil {
		c.us.upload.Add(int64(n))
	}
	return dest, waitPacketBudget(c.ctx, c.limiter, n)
}

func (c *trackedPacketConn) WritePacket(buffer *buf.Buffer, dest singM.Socksaddr) error {
	n := buffer.Len()
	if err := waitPacketBudget(c.ctx, c.limiter, n); err != nil {
		buffer.Release()
		return err
	}
	err := c.PacketConn.WritePacket(buffer, dest)
	if err == nil && c.us != nil {
		c.us.download.Add(int64(n))
	}
	return err
}

func (c *trackedPacketConn) Close() error {
	if c.closed.CompareAndSwap(false, true) {
		if c.us != nil {
			c.us.removeConn(c.sourceIP)
		}
		if c.releaseAdmission != nil {
			c.releaseAdmission()
		}
		c.tracker.removeConnRef(c.connID)
		c.tracker.notifyDeviceChange()
		return c.PacketConn.Close()
	}
	return nil
}

func (c *trackedPacketConn) makeCountFunc(counter *atomic.Int64) N.CountFunc {
	return func(n int64) {
		counter.Add(n)
		if c.limiter != nil && n > 0 {
			_ = waitPacketBudget(c.ctx, c.limiter, int(n))
		}
	}
}

func (c *trackedPacketConn) UnwrapPacketReader() (N.PacketReader, []N.CountFunc) {
	if c.us == nil {
		return c.PacketConn, nil
	}
	return c.PacketConn, []N.CountFunc{c.makeCountFunc(&c.us.upload)}
}

func (c *trackedPacketConn) UnwrapPacketWriter() (N.PacketWriter, []N.CountFunc) {
	if c.us == nil {
		return c.PacketConn, nil
	}
	return c.PacketConn, []N.CountFunc{c.makeCountFunc(&c.us.download)}
}

func (c *trackedPacketConn) Upstream() any           { return c.PacketConn }
func (c *trackedPacketConn) ReaderReplaceable() bool { return c.limiter == nil }
func (c *trackedPacketConn) WriterReplaceable() bool { return c.limiter == nil }
