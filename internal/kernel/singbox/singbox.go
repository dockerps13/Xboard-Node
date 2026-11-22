package singbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	box "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/include"
	singLog "github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/auth"
	singJSON "github.com/sagernet/sing/common/json"
	"github.com/sagernet/sing/service"
	"golang.org/x/time/rate"

	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/kernel"
	"github.com/cedar2025/xboard-node/internal/kernel/obfs"
	"github.com/cedar2025/xboard-node/internal/model"
	"github.com/cedar2025/xboard-node/internal/nlog"
	"github.com/cedar2025/xboard-node/internal/tcpprobe"
)

// drainTimeout is how long stop() waits for in-flight connections to finish
// naturally after closing all listen sockets, before hard-killing them.
const drainTimeout = 5 * time.Second

// SingBox implements kernel.Kernel by embedding sing-box as a Go library.
//
// User operations (AddUsers/RemoveUsers/UpdateUsers) use sing-box's native
// UpdatableInbound interface, which hot-swaps user credentials without
// restarting listeners — zero connection disruption.
type SingBox struct {
	cfg config.KernelConfig

	mu     sync.RWMutex
	box    *box.Box
	ctx    context.Context
	cancel context.CancelFunc

	users      []model.UserSpec
	nodeConfig *model.NodeSpec
	tls        kernel.TLSCert

	// connTracker is our lightweight in-process byte/IP tracker.
	// Created fresh on every Start (full restart).
	// Survives Reload (hot-swap) since live connections persist.
	connTracker *ConnTracker
	// Traffic survives full kernel swaps while old connections drain.
	// This avoids resetting externally observed cumulative counters.
	retiredTrackers []*ConnTracker
	archivedTraffic map[int][2]int64
	obfsHTTP        *obfs.HTTPServer
	obfsPublicPort  int
	tcpProbe        *tcpprobe.Listener

	// speedLimitFunc resolves a user UUID to a *rate.Limiter.
	// Set once by SetSpeedLimitFunc and forwarded to every new ConnTracker.
	speedLimitFunc func(string) *rate.Limiter

	// deviceLimitFunc resolves a user UUID to (limit, hasLimit) for gate-keeping.
	// Set once by SetDeviceLimitFunc and forwarded to every new ConnTracker.
	deviceLimitFunc  func(string) (int, bool)
	admissionFunc    func(userID int, sourceIP string, limit int) (func(), bool)
	deviceChangeFunc func()

	// trackerRegistered prevents duplicate AppendTracker calls on the same
	// Router instance during Reload. Reset to false on full restart.
	trackerRegistered bool
}

func New(cfg config.KernelConfig) *SingBox {
	return &SingBox{cfg: cfg}
}

var _ kernel.Kernel = (*SingBox)(nil)

func (s *SingBox) Name() string { return "sing-box" }

func (s *SingBox) Capabilities() kernel.Capabilities {
	return kernel.Capabilities{
		PerUserSpeedLimit:    true,
		DeviceLimit:          true,
		BuiltInTrafficStats:  false,
		AliveIPTracking:      true,
		ForceCloseConnection: false,
		ForceCloseUser:       true,
	}
}

func (s *SingBox) Protocols() []string {
	return []string{
		"vmess", "vless", "trojan", "shadowsocks",
		"hysteria", "hysteria2", "tuic", "naive", "socks", "http", "anytls", "mieru",
	}
}

func (s *SingBox) Start(nodeConfig *model.NodeSpec, users []model.UserSpec, tls kernel.TLSCert) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	buildNode := nodeConfig
	if nodeConfig.Protocol == "shadowsocks" && nodeConfig.Plugin != "" {
		copyNode := *nodeConfig
		copyNode.Plugin = ""
		copyNode.PluginOpt = ""
		buildNode = &copyNode
	}
	cfgMap := buildConfig(s.cfg, buildNode, users, tls)
	obfsBackend, obfsHost := "", ""
	if nodeConfig.Protocol == "shadowsocks" && nodeConfig.Plugin != "" {
		var err error
		obfsBackend, obfsHost, err = prepareHTTPObfsConfig(cfgMap, nodeConfig, users, tls)
		if err != nil {
			return err
		}
	}
	data, err := json.Marshal(cfgMap)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}

	nlog.Core().Debug("sing-box config generated", "len", len(data))

	ctx, cancel := context.WithCancel(context.Background())
	ctx = include.Context(ctx)

	opts, err := singJSON.UnmarshalExtendedContext[option.Options](ctx, data)
	if err != nil {
		cancel()
		return fmt.Errorf("parse sing-box options: %w", err)
	}

	// Save old state before creating the new instance.
	oldBox := s.box
	oldCancel := s.cancel
	oldCtx := s.ctx
	oldTracker := s.connTracker
	oldObfsHTTP := s.obfsHTTP

	instance, err := box.New(box.Options{
		Context: ctx,
		Options: opts,
	})
	if err != nil {
		cancel()
		return fmt.Errorf("create sing-box instance: %w", err)
	}

	if err := instance.Start(); err != nil {
		instance.Close()
		cancel()
		return fmt.Errorf("start sing-box: %w", err)
	}

	// Attach or reuse the public HTTP obfs listener only after the new SS
	// backend starts. Reuse avoids rebinding the same TCP port on reload.
	var front *obfs.HTTPServer
	if obfsBackend != "" {
		if oldObfsHTTP != nil && s.obfsPublicPort == nodeConfig.ServerPort {
			front = oldObfsHTTP
			front.UpdateBackend(obfsBackend, obfsHost)
		} else {
			host := nodeConfig.ListenIP
			if host == "" {
				host = "::"
			}
			publicAddr := net.JoinHostPort(host, strconv.Itoa(nodeConfig.ServerPort))
			var listenErr error
			front, listenErr = obfs.ListenHTTP(publicAddr, obfsBackend, obfsHost)
			if listenErr != nil {
				instance.Close()
				cancel()
				return fmt.Errorf("start HTTP simple-obfs on %s: %w", publicAddr, listenErr)
			}
		}
	}
	// New instance started successfully — swap state.
	s.box = instance
	s.ctx = ctx
	s.cancel = cancel
	s.users = users
	s.nodeConfig = nodeConfig
	s.tls = tls
	s.obfsHTTP = front
	s.syncTCPProbe(nodeConfig)
	if front != nil {
		s.obfsPublicPort = nodeConfig.ServerPort
	} else {
		s.obfsPublicPort = 0
	}

	// Keep the old tracker visible for traffic reporting until its
	// connections have drained and the final bytes are archived.
	if oldTracker != nil {
		s.retiredTrackers = append(s.retiredTrackers, oldTracker)
	}
	// Fresh tracker on full restart.
	s.connTracker = NewConnTracker(0)
	setTrackerUsers(s.connTracker, nodeConfig, users)
	if s.speedLimitFunc != nil {
		s.connTracker.SetSpeedLimitFunc(s.speedLimitFunc)
	}
	if s.deviceLimitFunc != nil {
		s.connTracker.SetDeviceLimitFunc(s.deviceLimitFunc)
	}
	if s.admissionFunc != nil {
		s.connTracker.SetAdmissionFunc(s.admissionFunc)
	}
	if s.deviceChangeFunc != nil {
		s.connTracker.SetDeviceChangeFunc(s.deviceChangeFunc)
	}

	s.trackerRegistered = false
	s.registerTracker(ctx)

	// Recycle old instance in background — drain then close. Old obfs
	// sessions remain on their old encrypted backend until drained.
	if oldObfsHTTP != nil && oldObfsHTTP != front {
		oldObfsHTTP.StopAccepting()
	}
	if oldBox != nil {
		go recycleOldBox(oldBox, oldCancel, oldCtx, oldTracker, func() {
			s.archiveRetiredTracker(oldTracker)
			if oldObfsHTTP != nil && oldObfsHTTP != front {
				oldObfsHTTP.Close()
			}
		})
	} else if oldObfsHTTP != nil && oldObfsHTTP != front {
		oldObfsHTTP.Close()
	}

	nlog.Core().Debug("sing-box started", "users", len(users))
	return nil
}

// recycleOldBox gracefully shuts down a previous sing-box instance in the
// background. It closes listen sockets first, waits for connections to drain,
// then hard-closes. This avoids blocking the new instance's startup.
func recycleOldBox(oldBox *box.Box, oldCancel context.CancelFunc, oldCtx context.Context, oldTracker *ConnTracker, onClosed func()) {
	// Step 1: close listen sockets so no new connections arrive on old ports.
	if im := service.FromContext[adapter.InboundManager](oldCtx); im != nil {
		_ = im.Close()
	}

	// Step 2: drain in-flight connections (best-effort).
	if oldTracker != nil {
		deadline := time.Now().Add(drainTimeout)
		for time.Now().Before(deadline) {
			if oldTracker.ActiveCount() == 0 {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
	}

	// Step 3: hard-close everything.
	oldBox.Close()
	if oldCancel != nil {
		oldCancel()
	}
	if onClosed != nil {
		onClosed()
	}
	nlog.Core().Debug("sing-box: old instance recycled")
}

// archiveRetiredTracker transfers its final counters into durable-in-process
// accumulated totals before releasing the retired instance. The same lock
// protects GetUserTraffic so the handoff cannot lose or duplicate bytes.
func (s *SingBox) archiveRetiredTracker(ct *ConnTracker) {
	if ct == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, tracker := range s.retiredTrackers {
		if tracker != ct {
			continue
		}
		if s.archivedTraffic == nil {
			s.archivedTraffic = make(map[int][2]int64)
		}
		traffic, _, _ := ct.GetUserTraffic()
		for uid, values := range traffic {
			existing := s.archivedTraffic[uid]
			existing[0] += values[0]
			existing[1] += values[1]
			s.archivedTraffic[uid] = existing
		}
		s.retiredTrackers = append(s.retiredTrackers[:i], s.retiredTrackers[i+1:]...)
		return
	}
}

// Reload hot-swaps the inbound users and routing rules without restarting the box.
// Routes, outbounds, and the connTracker stay alive so in-flight connections
// continue to be tracked correctly.
func (s *SingBox) Reload(nodeConfig *model.NodeSpec, users []model.UserSpec, tls kernel.TLSCert) error {
	// The obfs TCP public listener and the backend TCP/UDP split require
	// a full transactional kernel replacement, not a single-inbound reload.
	s.mu.RLock()
	hasObfs := s.obfsHTTP != nil
	s.mu.RUnlock()
	if hasObfs || (nodeConfig.Protocol == "shadowsocks" && nodeConfig.Plugin != "") {
		return s.Start(nodeConfig, users, tls)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.box == nil {
		return fmt.Errorf("not running")
	}

	cfgMap := buildConfig(s.cfg, nodeConfig, users, tls)
	data, err := json.Marshal(cfgMap)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}

	opts, err := singJSON.UnmarshalExtendedContext[option.Options](s.ctx, data)
	if err != nil {
		return fmt.Errorf("parse options: %w", err)
	}

	im := service.FromContext[adapter.InboundManager](s.ctx)
	if im == nil {
		return fmt.Errorf("inbound manager not available")
	}

	router := service.FromContext[adapter.Router](s.ctx)
	if router == nil {
		return fmt.Errorf("router not available")
	}

	// Update routing rules
	if err := router.UpdateRules(opts.Route.Rules, opts.Route.RuleSet); err != nil {
		nlog.Core().Debug("routing reload failed", "error", err)
	} else {
		nlog.Core().Debug("sing-box routing reloaded")
	}

	nopFactory := singLog.NewNOPFactory()

	// Configuration hash check for inbound reconstruction
	tlsChanged := !bytes.Equal(s.tls.CertPEM, tls.CertPEM) || !bytes.Equal(s.tls.KeyPEM, tls.KeyPEM)
	configChanged := tlsChanged || s.nodeConfig == nil || kernel.ComputeHash(nodeConfig, users) != kernel.ComputeHash(s.nodeConfig, s.users)

	for _, inb := range opts.Inbounds {
		tag := inb.Tag
		if !configChanged {
			if existing, ok := im.Get(tag); ok && existing.Type() == inb.Type {
				var err error
				updated := false
				switch v := existing.(type) {
				case adapter.UpdatableInbound[option.VMessUser]:
					if opts, ok := inb.Options.(*option.VMessInboundOptions); ok {
						updated = true
						err = v.UpdateUsers(opts.Users)
					}
				case adapter.UpdatableInbound[option.VLESSUser]:
					if opts, ok := inb.Options.(*option.VLESSInboundOptions); ok {
						updated = true
						err = v.UpdateUsers(opts.Users)
					}
				case adapter.UpdatableInbound[option.TrojanUser]:
					if opts, ok := inb.Options.(*option.TrojanInboundOptions); ok {
						updated = true
						err = v.UpdateUsers(opts.Users)
					}
				case adapter.UpdatableInbound[option.Hysteria2User]:
					if opts, ok := inb.Options.(*option.Hysteria2InboundOptions); ok {
						updated = true
						err = v.UpdateUsers(opts.Users)
					}
				case adapter.UpdatableShadowsocksInbound:
					if opts, ok := inb.Options.(*option.ShadowsocksInboundOptions); ok {
						updated = true
						err = v.UpdateUsersByOptions(opts.Users)
					}
				case adapter.UpdatableInbound[option.TUICUser]:
					if opts, ok := inb.Options.(*option.TUICInboundOptions); ok {
						updated = true
						err = v.UpdateUsers(opts.Users)
					}
				case adapter.UpdatableInbound[option.AnyTLSUser]:
					if opts, ok := inb.Options.(*option.AnyTLSInboundOptions); ok {
						updated = true
						err = v.UpdateUsers(opts.Users)
					}
				case adapter.UpdatableInbound[option.MieruUser]:
					if opts, ok := inb.Options.(*option.MieruInboundOptions); ok {
						updated = true
						err = v.UpdateUsers(opts.Users)
					}
				case adapter.UpdatableInbound[auth.User]:
					switch opts := inb.Options.(type) {
					case *option.NaiveInboundOptions:
						updated = true
						err = v.UpdateUsers(opts.Users)
					case *option.SocksInboundOptions:
						updated = true
						err = v.UpdateUsers(opts.Users)
					case *option.HTTPMixedInboundOptions:
						updated = true
						err = v.UpdateUsers(opts.Users)
					}
				}
				if updated && err == nil {
					continue
				}
				if !updated {
					err = fmt.Errorf("inbound %s options type %T is not compatible with existing %T", tag, inb.Options, existing)
				}
				nlog.Core().Warn("incremental update failed, falling back to recreate", "tag", tag, "error", err)
			}
		}

		// Config mismatch or incremental update failure: Reconstruct Inbound.
		// Remove the existing inbound first so im.Create() can bind the same port.
		// im.Create() cannot atomically swap a TCP listener — it tries to start the
		// new socket before the old one is closed, causing "address already in use".
		// The brief listen gap (< 1 ms) is far less disruptive than a full restart.
		_ = im.Remove(tag) // ignore error when tag doesn't exist yet
		logger := nopFactory.NewLogger(fmt.Sprintf("inbound/%s[%s]", inb.Type, tag))
		if err := im.Create(s.ctx, router, logger, tag, inb.Type, inb.Options); err != nil {
			return fmt.Errorf("recreate inbound %s: %w", tag, err)
		}
	}

	// Trackers remain registered on the Router (which survives ReloadUsers).
	// Only update the user map — do NOT re-register or traffic is double-counted.
	if s.connTracker != nil {
		setTrackerUsers(s.connTracker, nodeConfig, users)
	}

	nlog.Core().Debug("sing-box reloaded", "users", len(users))
	s.users = users
	s.nodeConfig = nodeConfig
	s.tls = tls
	s.syncTCPProbe(nodeConfig)
	return nil
}

// registerTracker wires the ConnTracker to the current Router exactly once.
// The ConnTracker handles both byte counting and optional rate limiting
// in a single wrapper, so no additional trackers are needed.
func (s *SingBox) registerTracker(ctx context.Context) {
	if s.trackerRegistered {
		return
	}
	router := service.FromContext[adapter.Router](ctx)
	if router == nil {
		return
	}
	if s.connTracker != nil {
		router.AppendTracker(s.connTracker)
	}
	s.trackerRegistered = true
}

// syncTCPProbe is called with s.mu held and only after a successful inbound
// start/reload. Binding failure never prevents the UDP proxy from starting.
// A former listener stays active across a same-address kernel replacement.
func (s *SingBox) syncTCPProbe(node *model.NodeSpec) {
	var addr string
	if s.cfg.TCPProbe && node != nil && node.ServerPort > 0 && node.ServerPort <= 65535 {
		if node.Protocol == "tuic" || node.Protocol == "hysteria2" || (node.Protocol == "hysteria" && node.Version == 2) {
			host := node.ListenIP
			if host == "" {
				host = "0.0.0.0"
			}
			addr = net.JoinHostPort(host, strconv.Itoa(node.ServerPort))
		}
	}
	if s.tcpProbe != nil && s.tcpProbe.Addr() == addr {
		return
	}
	if s.tcpProbe != nil {
		_ = s.tcpProbe.Close()
		s.tcpProbe = nil
	}
	if addr == "" {
		return
	}
	probe, err := tcpprobe.Listen(addr)
	if err != nil {
		nlog.Core().Warn("TCP latency probe unavailable; UDP inbound remains active", "address", addr, "error", err)
		return
	}
	s.tcpProbe = probe
	nlog.Core().Info("TCP latency probe listening (handshake only; not QUIC RTT)", "address", addr)
}

func (s *SingBox) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tcpProbe != nil {
		_ = s.tcpProbe.Close()
		s.tcpProbe = nil
	}
	if s.obfsHTTP != nil {
		s.obfsHTTP.StopAccepting()
	}
	s.stop()
	if s.obfsHTTP != nil {
		s.obfsHTTP.Close()
		s.obfsHTTP = nil
		s.obfsPublicPort = 0
	}
}

func (s *SingBox) stop() {
	if s.box == nil {
		return
	}

	// Step 1: close all listen sockets so no new connections are accepted.
	// im.Close() sets m.started=false so the subsequent box.Close() call is a no-op
	// for inbounds (avoids double-close), but box.Close() still tears down routers
	// and outbounds.
	if im := service.FromContext[adapter.InboundManager](s.ctx); im != nil {
		_ = im.Close()
	}

	// Step 2: wait for in-flight connections to drain naturally (best-effort).
	if s.connTracker != nil {
		deadline := time.Now().Add(drainTimeout)
		for time.Now().Before(deadline) {
			if s.connTracker.ActiveCount() == 0 {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
	}

	// Step 3: hard-close everything that is still open.
	s.box.Close()
	s.box = nil
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	s.ctx = nil
}

func (s *SingBox) IsRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.box != nil
}

// connTrackerLocked returns the connTracker under a read-optimised lock.
func (s *SingBox) connTrackerSafe() *ConnTracker {
	s.mu.Lock()
	ct := s.connTracker
	s.mu.Unlock()
	return ct
}

// SetSpeedLimitFunc configures per-user bandwidth throttling.
func (s *SingBox) SetSpeedLimitFunc(fn func(uuid string) *rate.Limiter) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.speedLimitFunc = fn
	if s.connTracker != nil {
		s.connTracker.SetSpeedLimitFunc(fn)
	}
}

// SetDeviceLimitFunc configures per-user device limit gate-keeping.
// Connections exceeding the limit are rejected at connect time.
func (s *SingBox) SetDeviceLimitFunc(fn func(uuid string) (int, bool)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deviceLimitFunc = fn
	if s.connTracker != nil {
		s.connTracker.SetDeviceLimitFunc(fn)
	}
}

// SetAdmissionFunc attaches the shared machine-mode IP admission authority.
func (s *SingBox) SetAdmissionFunc(fn func(userID int, sourceIP string, limit int) (func(), bool)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.admissionFunc = fn
	if s.connTracker != nil {
		s.connTracker.SetAdmissionFunc(fn)
	}
}

// SetDeviceChangeFunc wires fast device status updates without changing billing.
func (s *SingBox) SetDeviceChangeFunc(fn func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deviceChangeFunc = fn
	if s.connTracker != nil {
		s.connTracker.SetDeviceChangeFunc(fn)
	}
}

// UpdateGlobalDevices updates the global device state from panel (for multi-node).
func (s *SingBox) UpdateGlobalDevices(users map[int][]string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.connTracker != nil {
		s.connTracker.UpdateGlobalDevices(users)
	}
}

// ClearGlobalDevices clears the global device state (on WS disconnect).
func (s *SingBox) ClearGlobalDevices() {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.connTracker != nil {
		s.connTracker.ClearGlobalDevices()
	}
}

// ─── User management (non-disruptive) ───────────────────────────────────────

// AddUsers hot-swaps users into running inbounds. Zero connection disruption.
func (s *SingBox) AddUsers(users []model.UserSpec) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.box == nil {
		return 0, fmt.Errorf("not running")
	}

	existing := make(map[int]struct{}, len(s.users))
	for _, u := range s.users {
		existing[u.ID] = struct{}{}
	}
	var toAdd []model.UserSpec
	for _, u := range users {
		if _, dup := existing[u.ID]; !dup {
			toAdd = append(toAdd, u)
		}
	}
	if len(toAdd) == 0 {
		return 0, nil
	}

	merged := append(append([]model.UserSpec{}, s.users...), toAdd...)
	if err := s.reloadInboundsLocked(merged); err != nil {
		return 0, err
	}
	s.users = merged
	return len(toAdd), nil
}

// RemoveUsers hot-swaps users out of running inbounds. Zero connection disruption
// for remaining users.
func (s *SingBox) RemoveUsers(users []model.UserSpec) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.box == nil {
		return 0, fmt.Errorf("not running")
	}

	removeSet := make(map[int]struct{}, len(users))
	for _, u := range users {
		removeSet[u.ID] = struct{}{}
	}
	var kept []model.UserSpec
	removed := 0
	for _, u := range s.users {
		if _, rm := removeSet[u.ID]; rm {
			removed++
		} else {
			kept = append(kept, u)
		}
	}
	if removed == 0 {
		return 0, nil
	}

	if err := s.reloadInboundsLocked(kept); err != nil {
		return 0, err
	}
	s.users = kept
	return removed, nil
}

// UpdateUsers replaces the entire user set atomically via hot-swap.
func (s *SingBox) UpdateUsers(users []model.UserSpec) (added, removed int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.box == nil {
		return 0, 0, fmt.Errorf("not running")
	}

	toAdd, toRemove := kernel.UserDiff(s.users, users)
	added, removed = len(toAdd), len(toRemove)

	if added == 0 && removed == 0 {
		// Only limits may have changed — update tracker map.
		if s.connTracker != nil {
			setTrackerUsers(s.connTracker, s.nodeConfig, users)
		}
		s.users = users
		return 0, 0, nil
	}

	if err = s.reloadInboundsLocked(users); err != nil {
		return 0, 0, err
	}
	s.users = users
	return
}

// reloadInboundsLocked hot-swaps inbound users using UpdatableInbound.
// Must be called with s.mu held.
func (s *SingBox) reloadInboundsLocked(users []model.UserSpec) error {
	cfgMap := buildConfig(s.cfg, s.nodeConfig, users, s.tls)
	data, err := json.Marshal(cfgMap)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}

	opts, err := singJSON.UnmarshalExtendedContext[option.Options](s.ctx, data)
	if err != nil {
		return fmt.Errorf("parse options: %w", err)
	}

	im := service.FromContext[adapter.InboundManager](s.ctx)
	if im == nil {
		return fmt.Errorf("inbound manager not available")
	}

	router := service.FromContext[adapter.Router](s.ctx)
	if router == nil {
		return fmt.Errorf("router not available")
	}

	nopFactory := singLog.NewNOPFactory()

	for _, inb := range opts.Inbounds {
		tag := inb.Tag
		if existing, ok := im.Get(tag); ok && existing.Type() == inb.Type {
			var err error
			updated := false
			switch v := existing.(type) {
			case adapter.UpdatableInbound[option.VMessUser]:
				if opts, ok := inb.Options.(*option.VMessInboundOptions); ok {
					updated = true
					err = v.UpdateUsers(opts.Users)
				}
			case adapter.UpdatableInbound[option.VLESSUser]:
				if opts, ok := inb.Options.(*option.VLESSInboundOptions); ok {
					updated = true
					err = v.UpdateUsers(opts.Users)
				}
			case adapter.UpdatableInbound[option.TrojanUser]:
				if opts, ok := inb.Options.(*option.TrojanInboundOptions); ok {
					updated = true
					err = v.UpdateUsers(opts.Users)
				}
			case adapter.UpdatableInbound[option.Hysteria2User]:
				if opts, ok := inb.Options.(*option.Hysteria2InboundOptions); ok {
					updated = true
					err = v.UpdateUsers(opts.Users)
				}
			case adapter.UpdatableShadowsocksInbound:
				if opts, ok := inb.Options.(*option.ShadowsocksInboundOptions); ok {
					updated = true
					err = v.UpdateUsersByOptions(opts.Users)
				}
			case adapter.UpdatableInbound[option.TUICUser]:
				if opts, ok := inb.Options.(*option.TUICInboundOptions); ok {
					updated = true
					err = v.UpdateUsers(opts.Users)
				}
			case adapter.UpdatableInbound[option.AnyTLSUser]:
				if opts, ok := inb.Options.(*option.AnyTLSInboundOptions); ok {
					updated = true
					err = v.UpdateUsers(opts.Users)
				}
			case adapter.UpdatableInbound[option.MieruUser]:
				if opts, ok := inb.Options.(*option.MieruInboundOptions); ok {
					updated = true
					err = v.UpdateUsers(opts.Users)
				}
			case adapter.UpdatableInbound[auth.User]:
				switch opts := inb.Options.(type) {
				case *option.NaiveInboundOptions:
					updated = true
					err = v.UpdateUsers(opts.Users)
				case *option.SocksInboundOptions:
					updated = true
					err = v.UpdateUsers(opts.Users)
				case *option.HTTPMixedInboundOptions:
					updated = true
					err = v.UpdateUsers(opts.Users)
				}
			}
			if updated && err == nil {
				continue
			}
			if !updated {
				err = fmt.Errorf("inbound %s options type %T is not compatible with existing %T", tag, inb.Options, existing)
			}
			nlog.Core().Warn("incremental update failed, recreating inbound", "tag", tag, "error", err)
		}

		_ = im.Remove(tag)
		logger := nopFactory.NewLogger(fmt.Sprintf("inbound/%s[%s]", inb.Type, tag))
		if err := im.Create(s.ctx, router, logger, tag, inb.Type, inb.Options); err != nil {
			return fmt.Errorf("recreate inbound %s: %w", tag, err)
		}
	}

	if s.connTracker != nil {
		setTrackerUsers(s.connTracker, s.nodeConfig, users)
	}

	nlog.Core().Debug("sing-box users hot-swapped", "users", len(users))
	return nil
}

// ─── Observability ──────────────────────────────────────────────────────────
func (s *SingBox) GetUserTraffic(_ context.Context) (traffic map[int][2]int64, aliveIPs map[int]map[string]bool, connCount int, err error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	traffic = make(map[int][2]int64, len(s.archivedTraffic))
	aliveIPs = make(map[int]map[string]bool)
	for uid, values := range s.archivedTraffic {
		traffic[uid] = values
	}
	trackers := make([]*ConnTracker, 0, len(s.retiredTrackers)+1)
	trackers = append(trackers, s.retiredTrackers...)
	if s.connTracker != nil {
		trackers = append(trackers, s.connTracker)
	}
	for _, ct := range trackers {
		part, ips, connections := ct.GetUserTraffic()
		for uid, values := range part {
			existing := traffic[uid]
			existing[0] += values[0]
			existing[1] += values[1]
			traffic[uid] = existing
		}
		for uid, list := range ips {
			merged := aliveIPs[uid]
			if merged == nil {
				merged = make(map[string]bool, len(list))
				aliveIPs[uid] = merged
			}
			for ip := range list {
				merged[ip] = true
			}
		}
		connCount += connections
	}
	return traffic, aliveIPs, connCount, nil
}

// CloseConnection force-closes a specific connection by its ID.
func (s *SingBox) CloseConnection(_ context.Context, connID string) error {
	ct := s.connTrackerSafe()
	if ct == nil {
		return fmt.Errorf("not running")
	}
	if !ct.CloseByID(connID) {
		nlog.Core().Debug("CloseConnection: connection not found (already closed?)", "id", connID)
	}
	return nil
}

// CloseUserConnections terminates all connections for a user UUID.
func (s *SingBox) CloseUserConnections(_ context.Context, uuid string) error {
	s.mu.RLock()
	trackers := append([]*ConnTracker(nil), s.retiredTrackers...)
	if s.connTracker != nil {
		trackers = append(trackers, s.connTracker)
	}
	s.mu.RUnlock()
	for _, ct := range trackers {
		ct.CloseByUUID(uuid)
	}
	return nil
}

// buildUserMap creates a UUID→userID mapping used by ConnTracker to attribute
// connections to the correct user. Naive numeric usernames are normalized separately.
func buildUserMap(users []model.UserSpec) map[string]int {
	m := make(map[string]int, len(users))
	for _, u := range users {
		m[u.UUID] = u.ID
	}
	return m
}

// setTrackerUsers preserves Naive's numeric login while using canonical UUIDs
// for accounting and limit callbacks. Aliases are scoped to this inbound.
func setTrackerUsers(tracker *ConnTracker, node *model.NodeSpec, users []model.UserSpec) {
	var aliases map[string]string
	if node != nil && node.Protocol == "naive" {
		aliases = make(map[string]string, len(users))
		for _, user := range users {
			aliases[strconv.Itoa(user.ID)] = user.UUID
		}
	}
	tracker.setUserIdentities(buildUserMap(users), aliases)
}
