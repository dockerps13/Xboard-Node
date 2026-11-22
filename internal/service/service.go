package service

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cedar2025/xboard-node/internal/cert"
	"github.com/cedar2025/xboard-node/internal/cert/dnsproviders"
	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/controlplane"
	"github.com/cedar2025/xboard-node/internal/kernel"
	"github.com/cedar2025/xboard-node/internal/kernel/singbox"
	"github.com/cedar2025/xboard-node/internal/kernel/xray"
	"github.com/cedar2025/xboard-node/internal/limiter"
	"github.com/cedar2025/xboard-node/internal/model"
	"github.com/cedar2025/xboard-node/internal/monitor"
	"github.com/cedar2025/xboard-node/internal/nlog"
	"github.com/cedar2025/xboard-node/internal/panel"
	"github.com/cedar2025/xboard-node/internal/reportjournal"
	"github.com/cedar2025/xboard-node/internal/tracker"
)

type Service struct {
	cfg     *config.Config
	source  controlplane.Source
	sink    controlplane.Sink
	kernel  kernel.Kernel
	tracker *tracker.Tracker
	// Enabled only when the panel advertises durable report-ID deduplication.
	reportJournal *reportjournal.Journal
	deviceState   *deviceReportState
	limiter       *limiter.Limiter
	speedTracker  *limiter.SpeedTracker
	cert          *cert.Manager

	certApplyFailed bool // main-loop owned; do not serve TLS with stale or absent material

	lastConfig *model.NodeSpec
	lastUsers  []model.UserSpec

	// nodeLog is the logger with node context for this service instance.
	nodeLog *nlog.NodeLog

	// appliedState tracks the configuration and users that are currently
	// successfully running in the kernel.
	appliedState struct {
		Config *model.NodeSpec
		Users  []model.UserSpec
	}
	appliedMu sync.RWMutex

	pushInterval int // seconds
	pullInterval int // seconds

	lastUserHash   string     // hash of user list for change detection
	lastConfigHash string     // hash of full config for change detection
	pullBackoff    apiBackoff // backoff for panel pull failures
	pushBackoff    apiBackoff // backoff for panel push failures

	// pushActive prevents overlapping push/pull goroutines.
	pushActive atomic.Bool
	pushWG     sync.WaitGroup // ensure asynchronous reports finish before final shutdown flush
	pullActive atomic.Bool
	// pullResults delivers async pullViaAPI results back to the main goroutine.
	pullResults chan pullResult
	// Buffered edge-trigger: kernel connection goroutines never block on WS/network.
	deviceChanges chan struct{}

	wsClient         controlplane.PushClient        // Push client (nil if push is not enabled)
	wsEvents         chan controlplane.Event        // receives data events from push transport
	wsStatusCh       chan controlplane.StatusChange // receives push connectivity notifications
	wsCancel         context.CancelFunc             // cancels the WS client goroutine
	wsDisconnectAt   time.Time                      // when WS last disconnected (zero if connected)
	wsResyncPending  atomic.Bool
	machineMailbox   *controlplane.NodeMailbox
	machineMailboxCh <-chan struct{}

	// metricsMu: lastUsers, lastConfig, wsClient, wsDisconnectAt (buildMetrics vs main loop).
	metricsMu sync.RWMutex
}

// pullResult carries the outcome of an async pullViaAPI back to the main goroutine.
type pullResult struct {
	deviceUsers map[int][]string
	config      *model.NodeSpec
	users       []model.UserSpec
	configHash  string
	userHash    string
	certChanged bool
}

const certificateRecoveryInterval = 15 * time.Second

// apiBackoff implements simple exponential backoff for API failures.
type apiBackoff struct {
	mu            sync.Mutex
	skipRemaining int
}

func (b *apiBackoff) shouldSkip() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.skipRemaining > 0 {
		b.skipRemaining--
		return true
	}
	return false
}

func (b *apiBackoff) onSuccess() {
	b.mu.Lock()
	b.skipRemaining = 0
	b.mu.Unlock()
}

func (b *apiBackoff) onFailure() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.skipRemaining <= 0 {
		b.skipRemaining = 1
	} else if b.skipRemaining < 8 {
		b.skipRemaining *= 2
	}
}

func New(cfg *config.Config) *Service {
	var cp controlplane.ControlPlane
	if cfg.IsStandalone() {
		cp = controlplane.NewLocalControlPlane(cfg)
	} else {
		cp = controlplane.NewPanelControlPlane(cfg.Panel, cfg.WS, cfg.Kernel)
	}
	return newService(cfg, cp)
}

// SetAdmissionFunc wires the shared per-machine admission authority before Run.
// Standalone services are unchanged unless explicitly configured.
func (s *Service) SetAdmissionFunc(fn func(userID int, sourceIP string, limit int) (func(), bool)) {
	if k, ok := s.kernel.(interface {
		SetAdmissionFunc(func(userID int, sourceIP string, limit int) (func(), bool))
	}); ok {
		k.SetAdmissionFunc(fn)
	}
}

// NewWithControlPlane creates a Service with an externally-provided
// ControlPlane. Used by the machine orchestrator to inject a
// MachinePanelControlPlane with WS mux routing.
func NewWithControlPlane(cfg *config.Config, cp controlplane.ControlPlane) *Service {
	return newService(cfg, cp)
}

func newService(cfg *config.Config, cp controlplane.ControlPlane) *Service {
	certMgr := cert.NewManager(cfg.Cert)

	var k kernel.Kernel
	switch cfg.Kernel.Type {
	case "singbox":
		k = singbox.New(cfg.Kernel)
	case "xray":
		k = xray.New(cfg.Kernel)
	default:
		nlog.Core().Warn("unsupported kernel type, defaulting to sing-box", "type", cfg.Kernel.Type)
		k = singbox.New(cfg.Kernel)
	}

	l := limiter.New()
	st := limiter.NewSpeedTracker(l)

	return &Service{
		cfg:           cfg,
		source:        cp,
		sink:          cp,
		kernel:        k,
		tracker:       tracker.New(),
		limiter:       l,
		speedTracker:  st,
		cert:          certMgr,
		wsEvents:      make(chan controlplane.Event, 16),
		wsStatusCh:    make(chan controlplane.StatusChange, 4),
		pullResults:   make(chan pullResult, 1),
		deviceChanges: make(chan struct{}, 1),
	}
}

func (s *Service) Run(ctx context.Context) error {
	// Start cert manager (handles auto-TLS or manual cert verification)
	if err := s.cert.Start(ctx); err != nil {
		// A temporary ACME/DNS/file failure must not turn a TLS node into a
		// process crash loop.  Keep the listener disabled and let the main loop
		// retry; startKernel and canServeTLS make the no-certificate state
		// fail closed.
		s.certApplyFailed = true
		nlog.Core().Error("cert manager unavailable; TLS listener disabled until recovery", "error", err)
	}
	defer s.cert.Stop()

	// Load per-node device recovery state before accepting connections.
	if s.sink.SupportsReporting() && s.cfg.Kernel.ConfigDir != "" {
		state, err := openDeviceReportState(s.cfg)
		if err != nil {
			return fmt.Errorf("open device recovery state: %w", err)
		}
		s.deviceState = state
		s.tracker.RestoreAliveIPs(state.snapshot())
		if len(state.users) > 0 {
			nlog.Core().Info("device recovery loaded", "users", len(state.users), "node_id", s.cfg.Panel.NodeID)
		}
	}

	// Handshake: get WS config + initial data in one call
	if err := s.initialSetup(ctx); err != nil {
		return fmt.Errorf("initial setup: %w", err)
	}
	kernelStopped := false
	defer func() {
		if !kernelStopped {
			s.kernel.Stop()
		}
	}()

	// Surface unresolved billing records on restart. We deliberately
	// never auto-replay them against a non-idempotent legacy panel.
	s.warnUnconfirmedReports()

	// Set up tickers
	trackTicker := time.NewTicker(time.Duration(s.cfg.Node.TrackInterval) * time.Second)
	pushInterval := time.Duration(math.Max(float64(s.pushInterval), 5)) * time.Second
	pullInterval := time.Duration(s.pullInterval) * time.Second
	reportTicker := time.NewTicker(pushInterval)
	pullTicker := time.NewTicker(pullInterval)
	deviceReportTicker := time.NewTicker(time.Duration(s.cfg.Node.DeviceReportInterval) * time.Second)
	// Coalesce high-frequency connection churn without delaying indefinitely.
	deviceEventTimer := time.NewTimer(time.Hour)
	if !deviceEventTimer.Stop() {
		<-deviceEventTimer.C
	}
	deviceEventPending := false
	certRecoveryTicker := time.NewTicker(certificateRecoveryInterval)

	// WS discovery: when in REST-only mode, periodically re-handshake to check
	// if WS has been enabled. When WS is disconnected for too long, re-check
	// if it's still available.
	wsDiscoveryTicker := time.NewTicker(time.Duration(s.cfg.WS.DiscoveryInterval) * time.Second)

	defer trackTicker.Stop()
	defer reportTicker.Stop()
	defer pullTicker.Stop()
	defer deviceReportTicker.Stop()
	defer deviceEventTimer.Stop()
	defer certRecoveryTicker.Stop()
	defer wsDiscoveryTicker.Stop()

	s.startWSClient(ctx)

	for {
		select {
		case <-ctx.Done():
			// Capture the final bytes even when cancellation arrives between
			// periodic tracking ticks. The kernel is still running here.
			finalCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			s.trackAndEnforce(finalCtx)
			cancel()
			// Wait for an in-flight report to finish restoring failed deltas
			// before draining the final buffer, preventing lost accounting.
			s.pushWG.Wait()
			// Stop accepting connections before publishing the final offline
			// snapshot. Keep the previously collected traffic deltas intact.
			s.kernel.Stop()
			kernelStopped = true
			s.tracker.ClearLiveDevices()
			s.pushReportSync()
			return nil

		case <-trackTicker.C:
			s.trackAndEnforce(ctx)

		case <-reportTicker.C:
			s.pushReportAsync()

		case <-deviceReportTicker.C:
			s.reportDevices()

		case <-s.deviceChanges:
			if !deviceEventPending {
				deviceEventPending = true
				deviceEventTimer.Reset(350 * time.Millisecond)
			}

		case <-deviceEventTimer.C:
			deviceEventPending = false
			// Refresh online IPs without flushing or changing billing deltas.
			s.trackAndEnforce(ctx)
			s.sendDeviceBatch()

		case <-certRecoveryTicker.C:
			s.handleCertificateState(ctx)

		case <-pullTicker.C:
			// When WebSocket is connected, skip REST polling entirely.
			// Config/user updates arrive via WS push.
			if s.wsClient != nil && s.wsClient.IsConnected() {
				continue
			}
			nlog.Core().Debug("polling from API (ws not connected)")
			s.pullViaAPIAsync(ctx)

		case result := <-s.pullResults:
			s.applyPullResult(ctx, result)

		case <-wsDiscoveryTicker.C:
			s.wsDiscovery(ctx)

		case status := <-s.wsStatusCh:
			s.handleWSStatus(ctx, status)

		case <-s.machineMailboxCh:
			s.drainMachineMailbox(ctx)

		case event := <-s.wsEvents:
			s.handleWSEvent(ctx, event)
		}
	}
}

func (s *Service) initialSetup(ctx context.Context) error {
	// Register speed limit lookup with kernel unconditionally (before push/poll branch).
	s.kernel.SetSpeedLimitFunc(s.speedTracker.GetLimiter)
	s.kernel.SetDeviceLimitFunc(s.limiter.GetDeviceLimitByUUID)
	if notifying, ok := s.kernel.(interface{ SetDeviceChangeFunc(func()) }); ok {
		notifying.SetDeviceChangeFunc(s.signalDeviceChange)
	}

	bootstrap, err := s.source.Initial(ctx, s.wsMetrics, s.wsEvents, s.wsStatusCh)
	if err != nil {
		return err
	}
	if bootstrap.IdempotentReports && s.sink.SupportsReporting() {
		if s.cfg.Kernel.ConfigDir == "" {
			return fmt.Errorf("panel requires idempotent billing but kernel.config_dir is empty")
		}
		journal, journalErr := reportjournal.Open(filepath.Join(s.cfg.Kernel.ConfigDir, "idempotent-reports"))
		if journalErr != nil {
			return fmt.Errorf("open durable idempotent billing journal: %w", journalErr)
		}
		s.reportJournal = journal
	}

	if s.cfg.Node.PushInterval == 0 && bootstrap.PushInterval > 0 {
		s.pushInterval = bootstrap.PushInterval
	} else {
		s.pushInterval = s.cfg.Node.PushInterval
	}
	if s.pushInterval == 0 {
		s.pushInterval = 60
	}

	if s.cfg.Node.PullInterval == 0 && bootstrap.PullInterval > 0 {
		s.pullInterval = bootstrap.PullInterval
	} else {
		s.pullInterval = s.cfg.Node.PullInterval
	}
	if s.pullInterval == 0 {
		s.pullInterval = 60
	}

	if bootstrap.Push != nil {
		s.wsClient = bootstrap.Push
	}
	s.machineMailbox = bootstrap.Mailbox
	if s.machineMailbox != nil {
		s.machineMailboxCh = s.machineMailbox.NotifyCh()
	}
	if bootstrap.Config == nil {
		if bootstrap.Push != nil {
			// In machine mode a shared WS client may be available before the first
			// per-node snapshot arrives. In that case we wait for subsequent WS/REST
			// updates instead of failing startup.
			return nil
		}
		return fmt.Errorf("initial config is nil")
	}
	if err := validateNodeRuntime(s.cfg, s.kernel.Protocols(), bootstrap.Config, s.cert.TLSCert()); err != nil {
		// The protocol and its non-certificate settings are still validated.
		// Only the temporary missing-material error is deferred so the service
		// can recover after ACME/file material becomes available.
		pendingCert := s.certApplyFailed && requiresCertificate(bootstrap.Config) &&
			!s.cert.HasCert() && strings.HasSuffix(err.Error(), "requires TLS certificate files")
		if !pendingCert {
			return err
		}
		nlog.Core().Warn("initial TLS listener deferred until certificate recovery", "error", err)
	}

	s.metricsMu.Lock()
	s.lastConfig = bootstrap.Config
	s.metricsMu.Unlock()
	s.lastConfigHash = computeConfigHash(bootstrap.Config)
	s.updateUserState(bootstrap.Users)

	nlog.Core().Info("initial snapshot ready",
		"protocol", bootstrap.Config.Protocol,
		"port", bootstrap.Config.ServerPort,
		"users", len(bootstrap.Users),
	)

	if len(bootstrap.Users) == 0 {
		nlog.Core().Warn("no users, kernel will not start until users are available")
		s.markMailboxReadyAndDrain(ctx)
		return nil
	}

	s.applyRemoteOverrides(ctx, bootstrap.Config)
	if !s.startKernel(bootstrap.Config, bootstrap.Users) {
		if requiresCertificate(bootstrap.Config) && !s.cert.HasCert() {
			nlog.Core().Warn("initial TLS listener disabled until certificate recovery")
			s.markMailboxReadyAndDrain(ctx)
			return nil
		}
		return fmt.Errorf("start kernel")
	}
	if bootstrap.DeviceUsers != nil && (s.wsClient == nil || !s.wsClient.IsConnected()) {
		s.kernel.UpdateGlobalDevices(bootstrap.DeviceUsers)
	}
	s.markMailboxReadyAndDrain(ctx)
	return nil
}

// applyRemoteOverrides updates service-level settings (log level, cert config)
// from the panel's NodeConfig. Returns true if cert paths changed (kernel restart needed).
func (s *Service) applyRemoteOverrides(ctx context.Context, nc *model.NodeSpec) bool {
	if nc == nil {
		return false
	}

	// Dynamic Log Level (Kernel)
	if nc.KernelLogLevel != "" && nc.KernelLogLevel != s.cfg.Kernel.LogLevel {
		nlog.Core().Info("cert: kernel log level override", "old", s.cfg.Kernel.LogLevel, "new", nc.KernelLogLevel)
		s.cfg.Kernel.LogLevel = nc.KernelLogLevel
	}

	// Certificate configuration from panel (panel-first: takes precedence over local config)
	if nc.CertConfig != nil {
		// Stop a previously cleartext listener before attempting a TLS upgrade.
		if requiresCertificate(nc) && !requiresCertificate(s.appliedConfig()) && s.kernel.IsRunning() {
			s.stopForTLS(ctx)
		}
		return s.applyNodeCert(ctx, nc.CertConfig)
	}

	// Legacy fields (deprecated: prefer cert_config)
	if nc.AutoTLS != s.cfg.Cert.AutoTLS {
		nlog.Core().Info("cert: auto_tls policy changed (deprecated field)", "new", nc.AutoTLS)
		s.cfg.Cert.AutoTLS = nc.AutoTLS
	}
	if nc.Domain != "" && nc.Domain != s.cfg.Cert.Domain {
		s.cfg.Cert.Domain = nc.Domain
	}

	return false
}

// applyPanelCert converts a panel CertConfig into the local config format and
// reconfigures the cert manager. Reports whether cert paths changed.
func (s *Service) applyNodeCert(ctx context.Context, newCfg *config.CertConfig) bool {
	if newCfg == nil {
		return false
	}
	cfgCopy := *newCfg
	cfgCopy.CertDir = s.cfg.Cert.CertDir
	cfgCopy.ACMEStorageDir = s.cfg.Cert.ACMEStorageDir

	changed, err := s.cert.Reconfigure(ctx, cfgCopy)
	s.certApplyFailed = err != nil
	if err != nil {
		nlog.Core().Error("failed to apply runtime cert config", "mode", cfgCopy.CertMode, "error", err)
		return false
	}
	s.cfg.Cert = cfgCopy
	if changed {
		msg := fmt.Sprintf("cert: material updated, has_cert=%v", s.cert.HasCert())
		if s.nodeLog != nil {
			s.nodeLog.Info(msg)
		} else {
			nlog.Core().Info(msg)
		}
	}
	return changed
}

// handleCertificateState is the main-loop owner of certificate recovery.
// ACME renewal callbacks atomically refresh Manager material and set the
// renewed flag.  A recovery retry can also populate material after a
// transient startup or runtime failure.  In both cases the kernel must be
// restarted/reloaded with the new PEM before TLS traffic is accepted.
func (s *Service) handleCertificateState(ctx context.Context) {
	if s.cert == nil {
		return
	}

	renewed := s.cert.CertRenewed()
	sharedChanged, err := s.cert.RefreshSharedCertificate(ctx)
	if err != nil {
		nlog.Core().Debug("shared certificate refresh failed", "error", err)
	}
	renewed = renewed || sharedChanged
	// Certificate validity can change while the process runs. A certificate
	// that expired or became invalid must fail closed, not leave an existing
	// TLS listener running until another panel config update arrives.
	if s.lastConfig != nil && requiresCertificate(s.lastConfig) && !s.cert.HasCert() {
		s.stopForTLS(ctx)
	}
	recovered := false
	if s.certApplyFailed || (s.lastConfig != nil && requiresCertificate(s.lastConfig) && !s.cert.HasCert()) {
		if err := s.cert.Start(ctx); err != nil {
			nlog.Core().Debug("certificate recovery retry failed", "error", err)
			return
		}
		s.certApplyFailed = false
		recovered = true
	}
	if !renewed && !recovered {
		return
	}
	if s.lastConfig == nil || len(s.lastUsers) == 0 || !s.canServeTLS(s.lastConfig) {
		return
	}

	if s.kernel.IsRunning() {
		// applyChanges deliberately snapshots traffic before replacing the TLS
		// listener, preserving accounting across a certificate hot reload.
		s.applyChanges(ctx, true, false)
	} else if !s.startKernel(s.lastConfig, s.lastUsers) {
		nlog.Core().Warn("certificate recovered but listener restart failed")
	}
}

// startWSClient starts the push client goroutine if a client is configured.
func (s *Service) startWSClient(ctx context.Context) {
	if s.wsClient == nil {
		return
	}
	wsCtx, wsCancel := context.WithCancel(ctx)
	s.wsCancel = wsCancel
	go s.wsClient.Run(wsCtx)
}

func (s *Service) markMailboxReadyAndDrain(ctx context.Context) {
	if s.machineMailbox == nil {
		return
	}
	// Seed mailbox with bootstrap state so delta events can be applied
	// incrementally instead of always triggering REST reconciliation.
	s.metricsMu.RLock()
	users := s.lastUsers
	config := s.lastConfig
	s.metricsMu.RUnlock()
	s.machineMailbox.SeedBaseline(users, config)
	s.machineMailbox.MarkReady()
	s.drainMachineMailbox(ctx)
}

func (s *Service) drainMachineMailbox(ctx context.Context) {
	if s.machineMailbox == nil {
		return
	}
	state := s.machineMailbox.DrainIfReady()
	if state.HasConfig {
		s.handleWSEvent(ctx, controlplane.Event{Type: controlplane.EventSyncConfig, Config: state.Config})
	}
	if state.HasUsers {
		s.handleWSEvent(ctx, controlplane.Event{Type: controlplane.EventSyncUsers, Users: state.Users})
	}
	if state.HasDevices {
		s.handleWSEvent(ctx, controlplane.Event{Type: controlplane.EventSyncDevices, DeviceUsers: state.DeviceUsers})
	}
	if state.NeedsReconcile {
		s.requestWSResync(ctx, "machine_mailbox_reconcile")
	}
}

func (s *Service) requestWSResync(ctx context.Context, reason string) {
	if !s.wsResyncPending.CompareAndSwap(false, true) {
		return
	}
	if s.nodeLog != nil {
		s.nodeLog.Warn("ws state may be stale, scheduling REST reconciliation", "reason", reason)
	} else {
		nlog.Core().Warn("ws state may be stale, scheduling REST reconciliation", "reason", reason)
	}
	s.pullViaAPIAsync(ctx)
}

func (s *Service) wsMetrics() map[string]interface{} {
	status := monitor.Collect()
	m := s.buildMetrics(status)
	m["kernel_status"] = s.kernel.IsRunning()
	return m
}

// handleWSStatus reacts to WS connectivity changes.

// - On disconnect: record timestamp, immediately REST poll.
// - On reconnect: clear disconnect timestamp, REST poll to catch missed events.
func (s *Service) handleWSStatus(ctx context.Context, status controlplane.StatusChange) {
	if status.NeedsResync {
		s.requestWSResync(ctx, "drop_detected")
	}
	if status.Connected {
		s.metricsMu.Lock()
		s.wsDisconnectAt = time.Time{}
		s.metricsMu.Unlock()
		// Use nodeLog if available, otherwise core
		if s.nodeLog != nil {
			s.nodeLog.Info("ws connected")
		} else {
			nlog.Core().Info("ws connected")
		}
		// After reconnect, proactively pull once to ensure we haven't missed
		// any updates during the disconnection window.
		s.pullViaAPIAsync(ctx)
	} else {
		s.metricsMu.Lock()
		if s.wsDisconnectAt.IsZero() {
			s.wsDisconnectAt = time.Now()
		}
		s.metricsMu.Unlock()
		if s.nodeLog != nil {
			s.nodeLog.Info("ws disconnected")
		} else {
			nlog.Core().Info("ws disconnected")
		}
		// Clear global device state on disconnect
		s.kernel.ClearGlobalDevices()
		s.pullViaAPIAsync(ctx)
	}
}

// wsDiscovery periodically checks WS availability:
//
//  1. REST-only mode (wsClient == nil): Re-handshake to check if panel now has
//     WS enabled. If so, create and start a WS client. This handles the case
//     where WS was not enabled at startup but enabled later.
//
//  2. WS disconnected for >10 min: Re-handshake to check if WS config changed.
//     If WS is now disabled, stop the WS client and switch to REST-only.
//     If WS config changed (different URL/channel), restart with new config.
func (s *Service) wsDiscovery(ctx context.Context) {
	if !s.source.SupportsDiscovery() {
		return
	}

	needsCheck := false
	if s.wsClient == nil {
		needsCheck = true
		nlog.Core().Debug("push discovery: no push client, checking if control plane enabled push")
	} else if !s.wsDisconnectAt.IsZero() && time.Since(s.wsDisconnectAt) > 10*time.Minute {
		needsCheck = true
		nlog.Core().Debug("push discovery: push disconnected for >10min, re-checking")
	}
	if !needsCheck {
		return
	}

	pushClient, err := s.source.Discover(ctx, s.wsMetrics, s.wsEvents, s.wsStatusCh)
	if err != nil {
		nlog.Core().Debug("push discovery failed", "error", err)
		return
	}
	if s.source.SupportsPolling() {
		s.pullViaAPIAsync(ctx)
	}

	if pushClient != nil {
		if s.wsClient == nil {
			nlog.Core().Info("push discovery: control plane enabled push, creating client")
			s.metricsMu.Lock()
			s.wsClient = pushClient
			s.wsDisconnectAt = time.Time{}
			s.metricsMu.Unlock()
			s.startWSClient(ctx)
		}
	} else if s.wsClient != nil {
		nlog.Core().Info("push discovery: control plane disabled push, switching to polling")
		if s.wsCancel != nil {
			s.wsCancel()
		}
		s.metricsMu.Lock()
		s.wsClient = nil
		s.wsDisconnectAt = time.Time{}
		s.metricsMu.Unlock()
		s.wsCancel = nil
	}
}

// handleWSEvent processes data events received via WebSocket
func (s *Service) handleWSEvent(ctx context.Context, event controlplane.Event) {
	switch event.Type {
	case controlplane.EventSyncConfig:
		if event.Config == nil {
			return
		}
		newConfigHash := computeConfigHash(event.Config)
		if newConfigHash == s.lastConfigHash {
			return
		}
		if err := validateNodeRuntime(s.cfg, s.kernel.Protocols(), event.Config, s.cert.TLSCert()); err != nil {
			nlog.Core().Warn("ws config validation failed, ignoring update", "error", err)
			return
		}
		// Initialize nodeLog on first config
		if s.nodeLog == nil {
			s.nodeLog = nlog.ForNode(event.Config.Protocol, event.Config.ServerPort)
		}
		s.nodeLog.Info(fmt.Sprintf("config updated, %d users", len(event.Users)))
		s.metricsMu.Lock()
		s.lastConfig = event.Config
		s.metricsMu.Unlock()
		s.lastConfigHash = newConfigHash
		s.applyRemoteOverrides(ctx, event.Config)
		s.applyChanges(ctx, true, false)

	case controlplane.EventSyncUsers:
		if event.Users == nil {
			return
		}
		newHash := computeUserHash(event.Users)
		if newHash == s.lastUserHash {
			return
		}
		if s.nodeLog != nil {
			s.nodeLog.Info(fmt.Sprintf("users updated, %d users", len(event.Users)))
		}
		s.applyUserUpdate(ctx, event.Users, newHash)

	case controlplane.EventSyncUserDelta:
		if len(event.DeltaUsers) == 0 {
			return
		}
		if s.nodeLog != nil {
			s.nodeLog.Info(fmt.Sprintf("users delta: %s, %d users", event.DeltaAction, len(event.DeltaUsers)))
		}
		s.applyUserDelta(ctx, event.DeltaAction, event.DeltaUsers)

	case controlplane.EventSyncDevices:
		// Sync global device state
		if event.DeviceUsers != nil {
			s.kernel.UpdateGlobalDevices(event.DeviceUsers)
		}

	default:
		nlog.Core().Debug(fmt.Sprintf("unknown ws event: %v", event.Type))
	}
}

// pullViaAPIAsync fetches config/users from the panel API in a background
// goroutine and sends the result to pullResults for the main goroutine to apply.
func (s *Service) pullViaAPIAsync(ctx context.Context) {
	if !s.source.SupportsPolling() {
		return
	}
	if !s.pullActive.CompareAndSwap(false, true) {
		nlog.Core().Debug("pull already in progress, skipping")
		return
	}
	if s.pullBackoff.shouldSkip() {
		nlog.Core().Debug("skipping pull due to backoff")
		s.pullActive.Store(false)
		return
	}

	currentConfigHash := s.lastConfigHash
	// Certificate recovery ticker owns renewal notifications.
	certChanged := false

	go func() {
		defer s.pullActive.Store(false)
		snapshot, err := s.source.Poll(ctx)
		if err != nil {
			nlog.Core().Error("poll control plane failed", "error", err)
			s.pullBackoff.onFailure()
			return
		}
		s.pullBackoff.onSuccess()

		result := pullResult{certChanged: certChanged, deviceUsers: snapshot.DeviceUsers}
		if snapshot.Config != nil {
			result.config = snapshot.Config
			result.configHash = computeConfigHash(snapshot.Config)
			if result.configHash == currentConfigHash && !certChanged {
				result.config = nil
			}
		}
		if snapshot.Users != nil {
			result.users = snapshot.Users
			result.userHash = computeUserHash(snapshot.Users)
		}

		select {
		case s.pullResults <- result:
		case <-ctx.Done():
		}
	}()
}

// applyPullResult processes the result of an async pullViaAPI on the main goroutine.
func (s *Service) applyPullResult(ctx context.Context, result pullResult) {
	s.wsResyncPending.Store(false)
	configChanged := false

	if result.certChanged {
		nlog.Core().Info("certificate renewed, kernel restart needed")
		configChanged = true
	}

	if result.config != nil {
		if err := validateNodeRuntime(s.cfg, s.kernel.Protocols(), result.config, s.cert.TLSCert()); err != nil {
			nlog.Core().Warn("runtime config validation failed", "error", err)
			result.config = nil
		} else {
			configChanged = true
			// Initialize or update node logger
			if s.nodeLog == nil {
				s.nodeLog = nlog.ForNode(result.config.Protocol, result.config.ServerPort)
			}
			s.nodeLog.Info(fmt.Sprintf("config updated, %d users", len(s.lastUsers)))
			s.metricsMu.Lock()
			s.lastConfig = result.config
			s.metricsMu.Unlock()
			s.lastConfigHash = result.configHash
			if s.applyRemoteOverrides(ctx, result.config) {
				configChanged = true
			}
		}
	}

	if result.users != nil {
		usersChanged := result.userHash != s.lastUserHash

		if usersChanged && !configChanged {
			s.applyUserUpdate(ctx, result.users, result.userHash)
		} else if usersChanged {
			s.updateUserState(result.users)
		}
	}

	if configChanged {
		s.applyChanges(ctx, true, false)
	}
	// A late REST result must not overwrite a connected WS snapshot.
	if result.deviceUsers != nil && (s.wsClient == nil || !s.wsClient.IsConnected()) {
		s.kernel.UpdateGlobalDevices(result.deviceUsers)
	}
}

// ─── User state helpers ─────────────────────────────────────────────────────

func (s *Service) updateUserState(users []model.UserSpec) {
	if users == nil {
		users = []model.UserSpec{}
	}
	_, _ = s.prepareUserState(users)
}

func (s *Service) prepareUserState(users []model.UserSpec) (prevUsers []model.UserSpec, prevHash string) {
	if users == nil {
		users = []model.UserSpec{}
	}

	s.metricsMu.RLock()
	prevUsers = append([]model.UserSpec(nil), s.lastUsers...)
	s.metricsMu.RUnlock()
	prevHash = s.lastUserHash

	s.limiter.UpdateUsers(users)
	s.speedTracker.UpdateBuckets()

	s.metricsMu.Lock()
	s.lastUsers = append([]model.UserSpec(nil), users...)
	s.metricsMu.Unlock()
	s.lastUserHash = computeUserHash(users)
	return prevUsers, prevHash
}

func (s *Service) restoreUserState(users []model.UserSpec, hash string) {
	if users == nil {
		users = []model.UserSpec{}
	}
	s.limiter.UpdateUsers(users)
	s.speedTracker.UpdateBuckets()
	s.metricsMu.Lock()
	s.lastUsers = append([]model.UserSpec(nil), users...)
	s.metricsMu.Unlock()
	s.lastUserHash = hash
}

// startKernel starts (or restarts) the kernel with the given config/users and
// records the successfully applied state. Returns false on error.
func (s *Service) startKernel(nc *model.NodeSpec, users []model.UserSpec) bool {
	if !s.canServeTLS(nc) {
		s.stopForTLS(context.Background())
		return false
	}
	if err := s.kernel.Start(nc, users, s.cert.TLSCert()); err != nil {
		nlog.Core().Error("failed to start kernel", "error", err)
		return false
	}

	s.setAppliedState(nc, users)

	// Initialize node logger on first successful start
	if s.nodeLog == nil {
		s.nodeLog = nlog.ForNode(nc.Protocol, nc.ServerPort)
	}
	s.speedTracker.SetLogCallback(func(msg string) {
		fullMsg := fmt.Sprintf("speedtracker: %s active_limiters=%d", msg, s.speedTracker.LimitedUserCount())
		s.nodeLog.Info(fullMsg)
	})
	s.nodeLog.Info(fmt.Sprintf("started, %d users", len(users)))
	return true
}

func (s *Service) setAppliedState(config *model.NodeSpec, users []model.UserSpec) {
	s.appliedMu.Lock()
	s.appliedState.Config = config
	s.appliedState.Users = append([]model.UserSpec(nil), users...)
	s.appliedMu.Unlock()
}

func (s *Service) clearAppliedState() {
	s.appliedMu.Lock()
	s.appliedState.Config = nil
	s.appliedState.Users = nil
	s.appliedMu.Unlock()
}

func (s *Service) appliedConfig() *model.NodeSpec {
	s.appliedMu.RLock()
	defer s.appliedMu.RUnlock()
	return s.appliedState.Config
}

// ensureRunning starts the kernel if it is not running and there are users +
// config available. Returns true if the kernel is running afterwards.
func (s *Service) ensureRunning() bool {
	if s.kernel.IsRunning() {
		return true
	}
	if len(s.lastUsers) > 0 && s.lastConfig != nil {
		return s.startKernel(s.lastConfig, s.lastUsers)
	}
	return false
}

// ─── User update entry points ───────────────────────────────────────────────

// applyUserUpdate replaces the full user set and hot-swaps the kernel.
// Called from WS sync.users and REST polling.
func (s *Service) applyUserUpdate(ctx context.Context, users []model.UserSpec, newHash string) {
	if !s.ensureRunning() {
		return
	}

	// Capture counters while the old identities are still registered.
	// Otherwise removing users can drop their last unreported bytes.
	s.trackAndEnforce(ctx)
	prevUsers, prevHash := s.prepareUserState(users)
	added, removed, err := s.kernel.UpdateUsers(users)
	if err != nil {
		nlog.Core().Warn(fmt.Sprintf("UpdateUsers failed, restarting kernel: %v", err))
		if !s.startKernel(s.lastConfig, users) {
			s.restoreUserState(prevUsers, prevHash)
		}
		return
	}
	if newHash != "" {
		s.lastUserHash = newHash
	}
	if s.nodeLog != nil && (added > 0 || removed > 0) {
		s.nodeLog.Info(fmt.Sprintf("users updated: +%d -%d", added, removed))
	}
}

// applyUserDelta applies an incremental user change (add or remove) directly
// via the kernel's atomic user API. Kernel updates run before updateUserState.
func (s *Service) applyUserDelta(ctx context.Context, action string, deltaUsers []model.UserSpec) {
	switch action {
	case "add":
		// Defensive check for empty or nil deltaUsers
		if deltaUsers == nil || len(deltaUsers) == 0 {
			return
		}
		merged := mergeUsers(s.lastUsers, deltaUsers)

		if !s.ensureRunning() {
			return
		}

		for _, delta := range deltaUsers {
			for _, old := range s.lastUsers {
				if old.ID == delta.ID && old.UUID != delta.UUID {
					s.kernel.RemoveUsers([]model.UserSpec{old})
					break
				}
			}
		}

		// Count traffic before the user map changes, including replaced UUIDs.
		s.trackAndEnforce(ctx)
		prevUsers, prevHash := s.prepareUserState(merged)
		added, err := s.kernel.AddUsers(deltaUsers)
		if err != nil {
			nlog.Core().Warn(fmt.Sprintf("AddUsers failed: %v, falling back to UpdateUsers", err))
			if _, _, err := s.kernel.UpdateUsers(merged); err != nil {
				nlog.Core().Error(fmt.Sprintf("UpdateUsers fallback failed: %v", err))
				s.restoreUserState(prevUsers, prevHash)
				return
			}
		}
		if s.nodeLog != nil && added > 0 {
			s.nodeLog.Info(fmt.Sprintf("users added: +%d", added))
		}

	case "remove":
		// Defensive check for empty or nil deltaUsers
		if deltaUsers == nil || len(deltaUsers) == 0 {
			return
		}
		filtered := subtractUsers(s.lastUsers, deltaUsers)

		if !s.kernel.IsRunning() {
			return
		}

		// The kernel may discard counters for users when they are removed.
		s.trackAndEnforce(ctx)
		prevUsers, prevHash := s.prepareUserState(filtered)
		removed, err := s.kernel.RemoveUsers(deltaUsers)
		if err != nil {
			nlog.Core().Warn(fmt.Sprintf("RemoveUsers failed: %v, falling back to UpdateUsers", err))
			if _, _, err := s.kernel.UpdateUsers(filtered); err != nil {
				nlog.Core().Error(fmt.Sprintf("UpdateUsers fallback failed: %v", err))
				s.restoreUserState(prevUsers, prevHash)
				return
			}
		}
		if s.nodeLog != nil && removed > 0 {
			s.nodeLog.Info(fmt.Sprintf("users removed: -%d", removed))
		}

	default:
		nlog.Core().Warn(fmt.Sprintf("unknown user delta action: %s", action))
	}
}

// mergeUsers overlays deltaUsers onto base (keyed by ID). New users are
// appended, existing users have their properties overwritten.
func mergeUsers(base, delta []model.UserSpec) []model.UserSpec {
	// Handle nil slices
	if base == nil {
		base = []model.UserSpec{}
	}
	if delta == nil {
		return base
	}

	m := make(map[int]model.UserSpec, len(base))
	for _, u := range base {
		m[u.ID] = u
	}
	for _, u := range delta {
		m[u.ID] = u
	}
	out := make([]model.UserSpec, 0, len(m))
	for _, u := range m {
		out = append(out, u)
	}
	return out
}

// subtractUsers returns base with all users in delta removed.
func subtractUsers(base, delta []model.UserSpec) []model.UserSpec {
	if base == nil {
		return nil
	}
	if delta == nil || len(delta) == 0 {
		return base
	}
	removeSet := make(map[int]struct{}, len(delta))
	for _, u := range delta {
		removeSet[u.ID] = struct{}{}
	}
	out := make([]model.UserSpec, 0, len(base))
	for _, u := range base {
		if _, ok := removeSet[u.ID]; !ok {
			out = append(out, u)
		}
	}
	return out
}

// applyChanges applies config changes to the kernel. User-only changes are
// handled by applyUserUpdate/applyUserDelta directly via the atomic user API.
func (s *Service) applyChanges(ctx context.Context, configChanged, usersChanged bool) {
	if !configChanged {
		return
	}

	// Snapshot the old kernel counters before a reload/reconstruction.
	// Sampling after Reload can lose trailing traffic or see reset counters.
	s.trackAndEnforce(ctx)
	if s.lastConfig == nil || len(s.lastUsers) == 0 {
		if len(s.lastUsers) == 0 {
			s.kernel.Stop()
			s.clearAppliedState()
		}
		return
	}

	if !s.canServeTLS(s.lastConfig) {
		s.stopForTLS(ctx)
		return
	}

	// If config changed, delegate to kernel.Reload. The kernel implementation
	// decides whether to hot-swap users, reconstruct inbounds, or restart itself.
	if configChanged && s.kernel.IsRunning() {
		if err := s.kernel.Reload(s.lastConfig, s.lastUsers, s.cert.TLSCert()); err != nil {
			nlog.Core().Warn(fmt.Sprintf("reload failed, restarting: %v", err))
			s.startKernel(s.lastConfig, s.lastUsers)
		} else {
			s.setAppliedState(s.lastConfig, s.lastUsers)
			if s.nodeLog != nil {
				s.nodeLog.Info(fmt.Sprintf("config updated, %d users", len(s.lastUsers)))
			}
		}
	} else if !s.kernel.IsRunning() {
		s.startKernel(s.lastConfig, s.lastUsers)
	}
}

// requiresCertificate distinguishes certificate TLS from REALITY.
func requiresCertificate(spec *model.NodeSpec) bool {
	if spec == nil {
		return false
	}
	if spec.TLS == 1 {
		return true
	}
	switch spec.Protocol {
	case "hysteria", "hysteria2", "tuic", "anytls":
		return true
	case "trojan":
		return spec.TLS != 2
	}
	return false
}

func (s *Service) canServeTLS(spec *model.NodeSpec) bool {
	return !requiresCertificate(spec) || (!s.certApplyFailed && s.cert.TLSCert().HasCert())
}

func (s *Service) stopForTLS(ctx context.Context) {
	if s.kernel.IsRunning() {
		s.trackAndEnforce(ctx)
		s.kernel.Stop()
		s.tracker.ClearLiveDevices()
	}
	s.clearAppliedState()
	nlog.Core().Warn("TLS certificate unavailable or configuration pending; listener disabled")
}

func (s *Service) trackAndEnforce(ctx context.Context) {
	if !s.kernel.IsRunning() {
		return
	}

	traffic, aliveIPs, connCount, err := s.kernel.GetUserTraffic(ctx)
	if err != nil {
		nlog.Core().Debug("get user traffic failed", "error", err)
		return
	}

	s.tracker.Process(traffic, aliveIPs, connCount)

	// Only log stats if there's actual traffic or connections
	if connCount > 0 || len(traffic) > 0 {
		if s.nodeLog != nil {
			s.nodeLog.Debug(fmt.Sprintf("tracker: %d conns, %d users online", connCount, len(traffic)))
		} else {
			nlog.TrackerStats(connCount, len(traffic))
		}
	}
}

// reportWithJournal stores every raw-traffic delta before contacting a panel
// that explicitly advertised idempotent report IDs. Replaying a pending batch
// always reuses its original ID, so lost HTTP replies cannot double-charge
// on a correctly implemented server.
func (s *Service) reportWithJournal(payload controlplane.ReportPayload) error {
	if s.reportJournal == nil {
		return fmt.Errorf("idempotent report journal is not initialized")
	}
	if len(payload.Traffic) > 0 {
		stored, err := s.reportJournal.Append(payload.Traffic)
		if err != nil {
			// Append returns a non-empty ID if rename succeeded but the
			// directory sync failed. Such a file may exist on disk; do not
			// restore the same bytes into a second batch in that case.
			if stored.ID == "" {
				s.tracker.RestoreTraffic(payload.Traffic)
			}
			return fmt.Errorf("persist report batch: %w", err)
		}
	}
	pending, err := s.reportJournal.Pending()
	if err != nil {
		return fmt.Errorf("read persisted report batches: %w", err)
	}
	// Bound work on each reporting tick. A large backlog is retried over
	// subsequent ticks without blocking kernel tracking for minutes.
	count := len(pending)
	if count > 4 {
		count = 4
	}
	for _, batch := range pending[:count] {
		attempt := payload
		attempt.BatchID = batch.ID
		attempt.Traffic = batch.Traffic
		if err := s.sink.Report(attempt); err != nil {
			return fmt.Errorf("batch %s: %w", batch.ID, err)
		}
		if err := s.reportJournal.Acknowledge(batch.ID); err != nil {
			return fmt.Errorf("acknowledge batch %s: %w", batch.ID, err)
		}
	}
	// A deduplicated billing receipt must not be the sole confirmation of
	// a fresh device snapshot. Send it without a billing ID as well.
	// Preserve the existing bounded traffic-only backlog behavior.
	if len(pending) == 0 || len(payload.Alive) > 0 {
		payload.BatchID = ""
		payload.Traffic = nil
		return s.sink.Report(payload)
	}
	return nil
}

// reportLogger uses the applied runtime rather than a pending config and
// returns immutable context safe to use from the report goroutine.
func (s *Service) reportLogger() (*nlog.NodeLog, int, int) {
	nodeID, machineID := 0, 0
	if s.cfg != nil {
		nodeID, machineID = s.cfg.Panel.NodeID, s.cfg.Panel.MachineID
	}
	if applied := s.appliedConfig(); applied != nil {
		return nlog.ForNode(applied.Protocol, applied.ServerPort), nodeID, machineID
	}
	return nlog.Core(), nodeID, machineID
}

// reportTrafficTotals summarizes raw bytes without changing billing payloads.
func reportTrafficTotals(traffic map[int][2]int64) (up, down uint64) {
	for _, delta := range traffic {
		if delta[0] > 0 {
			up += uint64(delta[0])
		}
		if delta[1] > 0 {
			down += uint64(delta[1])
		}
	}
	return
}

// reportLegacy records the attempted delta before sending it. Without
// a deduplicating receipt, a failure cannot safely be retried automatically.
// The local record is for operator reconciliation, never automatic replay.
func (s *Service) reportLegacy(payload controlplane.ReportPayload) error {
	if len(payload.Traffic) == 0 {
		if err := s.sink.Report(payload); err != nil {
			return err
		}
		return s.retryLegacyUnsent(payload)
	}
	if s.cfg == nil || s.cfg.Kernel.ConfigDir == "" {
		s.tracker.RestoreTraffic(payload.Traffic)
		return fmt.Errorf("legacy traffic reporting requires a writable kernel.config_dir")
	}
	j, err := reportjournal.Open(filepath.Join(s.cfg.Kernel.ConfigDir, "unconfirmed-reports"))
	if err != nil {
		s.tracker.RestoreTraffic(payload.Traffic)
		return fmt.Errorf("open legacy report audit: %w", err)
	}
	batch, err := j.Append(payload.Traffic)
	if err != nil {
		if batch.ID == "" {
			s.tracker.RestoreTraffic(payload.Traffic)
		}
		return fmt.Errorf("persist legacy report audit: %w", err)
	}
	if err := s.sink.Report(payload); err != nil {
		if panel.IsReportNotSent(err) {
			if markErr := j.MarkLegacyRetryable(batch.ID); markErr != nil {
				return fmt.Errorf("legacy batch %s retry transition failed: %w", batch.ID, markErr)
			}
			return fmt.Errorf("legacy batch %s proven unsent; queued for safe retry: %w", batch.ID, err)
		}
		return fmt.Errorf("legacy batch %s retained for manual reconciliation: %w", batch.ID, err)
	}
	// The legacy API reported success. This is NOT an idempotent receipt;
	// if removal fails, leave the audit record for manual reconciliation.
	if err := j.RemoveLegacyAudit(batch.ID); err != nil {
		return fmt.Errorf("legacy batch %s reported success but audit cleanup failed: %w", batch.ID, err)
	}
	return s.retryLegacyUnsent(payload)
}

// pushReportAsync sends the report in a background goroutine so the select
// loop is never blocked by slow HTTP. Only one push runs at a time.
func (s *Service) pushReportAsync() {
	if !s.sink.SupportsReporting() {
		return
	}
	if !s.pushActive.CompareAndSwap(false, true) {
		nlog.Core().Debug("push already in progress, skipping")
		return
	}
	if s.pushBackoff.shouldSkip() {
		nlog.Core().Debug("skipping report due to backoff")
		s.pushActive.Store(false)
		return
	}

	aliveIPs := s.tracker.FlushAliveIPs()
	if err := s.persistDeviceAttempt(aliveIPs); err != nil {
		nlog.Core().Error("cannot persist device report; retrying on next tick", "error", err)
		s.pushActive.Store(false)
		return
	}
	traffic := s.tracker.FlushTraffic()
	online := s.tracker.CurrentOnline()
	for uid, ips := range aliveIPs {
		online[uid] = len(ips)
	}
	status := monitor.Collect()
	metrics := s.buildMetrics(status)
	metrics["kernel_status"] = s.kernel.IsRunning()

	reportLog, nodeID, machineID := s.reportLogger()
	activeDevices, offlineUsers := deviceReportCounts(aliveIPs)
	started := time.Now()
	s.pushWG.Add(1)
	go func() {
		defer s.pushWG.Done()
		defer s.pushActive.Store(false)
		payload := controlplane.ReportPayload{Traffic: traffic, Alive: aliveIPs, Online: online, CPU: status.CPU, Mem: [2]uint64{status.MemTotal, status.MemUsed}, Swap: [2]uint64{status.SwapTotal, status.SwapUsed}, Disk: [2]uint64{status.DiskTotal, status.DiskUsed}, Metrics: metrics}
		var err error
		if s.reportJournal != nil {
			err = s.reportWithJournal(payload)
		} else {
			err = s.reportLegacy(payload)
		}
		if err != nil {
			reportLog.Warn("failed to push report", "node_id", nodeID, "machine_id", machineID, "device_report_users", len(aliveIPs), "device_report_ips", activeDevices, "device_offline_users", offlineUsers, "duration_ms", time.Since(started).Milliseconds(), "error", err)
			if len(aliveIPs) > 0 {
				s.tracker.RestoreAliveIPs(aliveIPs)
			}
			s.pushBackoff.onFailure()
			return
		}
		s.confirmDeviceAttempt(aliveIPs)
		if len(aliveIPs) > 0 {
			reportLog.Info("device report confirmed", "node_id", nodeID, "machine_id", machineID, "device_report_users", len(aliveIPs), "device_report_ips", activeDevices, "device_offline_users", offlineUsers, "duration_ms", time.Since(started).Milliseconds())
		}
		s.pushBackoff.onSuccess()
		if s.reportJournal != nil {
			// A successful journal tick can include older batches and leave
			// newer batches pending; do not label freshly collected bytes as sent.
			reportLog.Info("journal report tick succeeded", "node_id", nodeID, "machine_id", machineID, "online", len(s.tracker.CurrentOnline()))
		} else {
			up, down := reportTrafficTotals(traffic)
			reportLog.Info("report pushed", "node_id", nodeID, "machine_id", machineID, "traffic_users", len(traffic), "online", len(s.tracker.CurrentOnline()), "upload_bytes", up, "download_bytes", down)
		}
	}()
}

// warnUnconfirmedReports highlights ambiguous previous shutdown reports.
// They may or may not have been charged by XBoard and require operator
// reconciliation until the panel provides atomic report-ID deduplication.
func (s *Service) warnUnconfirmedReports() {
	if s.cfg == nil || s.cfg.Kernel.ConfigDir == "" || !s.sink.SupportsReporting() {
		return
	}
	path := filepath.Join(s.cfg.Kernel.ConfigDir, "unconfirmed-reports")
	j, err := reportjournal.Open(path)
	if err != nil {
		nlog.Core().Error("cannot open local unconfirmed report journal", "error", err)
		return
	}
	batches, err := j.Pending()
	if err != nil {
		nlog.Core().Error("corrupt or unreadable unconfirmed reports; manual repair required", "error", err, "path", path)
		return
	}
	if len(batches) > 0 {
		nlog.Core().Warn("unconfirmed traffic reports from previous runs require manual billing reconciliation; automatic replay disabled",
			"count", len(batches), "path", path)
	}
}

// pushReportSync is used only during shutdown to ensure final data is sent.
func (s *Service) pushReportSync() {
	if !s.sink.SupportsReporting() {
		return
	}
	aliveIPs := s.tracker.FlushAliveIPs()
	if err := s.persistDeviceAttempt(aliveIPs); err != nil {
		// Still preserve final traffic accounting; no untracked device send.
		nlog.Core().Error("cannot persist final device report", "error", err)
		aliveIPs = nil
	}
	traffic := s.tracker.FlushTraffic()
	online := s.tracker.CurrentOnline()
	for uid, ips := range aliveIPs {
		online[uid] = len(ips)
	}
	status := monitor.Collect()
	metrics := s.buildMetrics(status)
	metrics["kernel_status"] = s.kernel.IsRunning()

	payload := controlplane.ReportPayload{Traffic: traffic, Alive: aliveIPs, Online: online, CPU: status.CPU, Mem: [2]uint64{status.MemTotal, status.MemUsed}, Swap: [2]uint64{status.SwapTotal, status.SwapUsed}, Disk: [2]uint64{status.DiskTotal, status.DiskUsed}, Metrics: metrics}
	if s.reportJournal != nil {
		// A failed send remains safely on disk under the same ID; retry after
		// restart is permitted only because the panel opted into deduplication.
		if err := s.reportWithJournal(payload); err != nil {
			nlog.Core().Warn("failed to push idempotent final report; pending batch retained on disk", "error", err)
			s.tracker.RestoreAliveIPs(aliveIPs)
		} else {
			s.confirmDeviceAttempt(aliveIPs)
		}
		return
	}
	if err := s.reportLegacy(payload); err != nil {
		nlog.Core().Warn("final legacy report not confirmed; inspect audit records", "error", err)
		if len(aliveIPs) > 0 {
			s.tracker.RestoreAliveIPs(aliveIPs)
		}
	} else {
		s.confirmDeviceAttempt(aliveIPs)
	}
}

// deviceReportCounts summarizes a snapshot without logging user IDs or IPs.
func deviceReportCounts(alive map[int][]string) (ips, offline int) {
	for _, addresses := range alive {
		ips += len(addresses)
		if len(addresses) == 0 {
			offline++
		}
	}
	return
}

// buildMetrics aggregates node-level metrics to be reported to the panel.
// This includes active connections, per-core CPU, GC stats, API call stats,
// WebSocket status, and limiter hit counts.
func (s *Service) buildMetrics(status monitor.Status) map[string]interface{} {
	s.metricsMu.RLock()
	lastUsers := s.lastUsers
	wsClient := s.wsClient
	s.metricsMu.RUnlock()

	m := make(map[string]interface{})
	if applied := s.appliedConfig(); applied != nil {
		if attestation := runtimeAttestationMap(applied); attestation != nil {
			m["runtime_attestation"] = attestation
		}
	}
	online := s.tracker.CurrentOnline()

	m["uptime"] = status.Uptime
	m["goroutines"] = status.Goroutines

	// Active connections (last measured during tracker.Process()).
	m["active_connections"] = s.tracker.ActiveConnections()
	m["total_connections"] = s.tracker.TotalConnections()
	m["active_users"] = len(online)
	m["total_users"] = len(lastUsers)

	// These values describe this individual node, not just its host process.
	// A healthy machine-level /healthz does not imply all TLS listeners are up.
	m["runtime_health"] = map[string]interface{}{
		"listener_running":    s.kernel.IsRunning(),
		"certificate_present": s.cert != nil && s.cert.HasCert(),
		"applied_node":        s.appliedConfig() != nil,
	}

	// Speed
	m["inbound_speed"] = s.tracker.InboundSpeed()
	m["outbound_speed"] = s.tracker.OutboundSpeed()

	// Per-core CPU usage (if available).
	if len(status.CPUPerCore) > 0 {
		m["cpu_per_core"] = status.CPUPerCore
	}

	m["load"] = map[string]interface{}{
		"load1":  status.Load1,
		"load5":  status.Load5,
		"load15": status.Load15,
	}

	// Speed Limiter metrics
	m["speed_limiter"] = map[string]interface{}{
		"has_limits":    s.speedTracker.HasLimits(),
		"limited_users": s.speedTracker.LimitedUserCount(),
	}

	// GC metrics.
	m["gc"] = map[string]interface{}{
		"num_gc":        status.NumGC,
		"last_pause_ms": status.LastPauseMS,
	}

	// API metrics.
	api := s.source.Metrics()
	m["api"] = map[string]interface{}{
		"success": api.Success,
		"failure": api.Failure,
	}

	// WebSocket status.
	wsEnabled := wsClient != nil
	wsConnected := wsEnabled && wsClient.IsConnected()
	m["ws"] = map[string]interface{}{
		"enabled":   wsEnabled,
		"connected": wsConnected,
	}

	// Limiter metrics.
	lm := s.limiter.SnapshotMetrics()
	m["limits"] = map[string]interface{}{
		"device_limit_events": lm.DeviceLimitEvents,
		"speed_limited_users": s.speedTracker.LimitedUserCount(),
	}

	return m
}

func runtimeAttestationMap(applied *model.NodeSpec) map[string]interface{} {
	if applied == nil || applied.ControlPlaneRevision < 1 || applied.ServerPort < 1 || applied.ServerPort > 65535 || strings.TrimSpace(applied.Protocol) == "" {
		return nil
	}
	hashBytes, err := hex.DecodeString(applied.ControlPlaneConfigHash)
	if err != nil || len(hashBytes) != sha256.Size {
		return nil
	}
	return map[string]interface{}{
		"revision":    applied.ControlPlaneRevision,
		"config_hash": strings.ToLower(applied.ControlPlaneConfigHash),
		"protocol":    strings.ToLower(strings.TrimSpace(applied.Protocol)),
		"listen_port": applied.ServerPort,
	}
}

// computeConfigHash returns a deterministic hash of the node config.
// It uses JSON marshaling to ensure all fields are captured, ensuring that
// any configuration change correctly triggers a kernel reload.
func computeConfigHash(cfg *model.NodeSpec) string {
	if cfg == nil {
		return ""
	}
	h := sha256.New()
	// We marshal the entire config to be safe. Node config updates are low-frequency,
	// so the robustness of capturing all fields outweighs the micro-performance of manual hashing.
	data, _ := json.Marshal(cfg)
	h.Write(data)
	return fmt.Sprintf("%x", h.Sum(nil))
}

// computeUserHash returns a deterministic hash of the user list for change detection.
// Uses direct byte encoding instead of binary.Write to avoid reflection overhead.
func computeUserHash(users []model.UserSpec) string {
	sorted := make([]model.UserSpec, len(users))
	copy(sorted, users)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })

	h := sha256.New()
	var buf [8]byte
	for _, u := range sorted {
		binary.LittleEndian.PutUint64(buf[:], uint64(u.ID))
		h.Write(buf[:])
		io.WriteString(h, u.UUID)
		binary.LittleEndian.PutUint64(buf[:], uint64(u.SpeedLimit))
		h.Write(buf[:])
		binary.LittleEndian.PutUint64(buf[:], uint64(u.DeviceLimit))
		h.Write(buf[:])
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

// signalDeviceChange is safe from transport goroutines and never waits for
// network I/O, tracking, or the service main loop.
func (s *Service) signalDeviceChange() {
	select {
	case s.deviceChanges <- struct{}{}:
	default:
	}
}

// ─── Device management ──────────────────────────────────────────────────

// sendDeviceBatch reports local device snapshot to panel via WS.
func (s *Service) sendDeviceBatch() {
	if s.wsClient == nil || !s.wsClient.IsConnected() {
		return
	}

	devices := s.tracker.FlushWSAliveIPs()
	if len(devices) == 0 {
		nlog.Core().Debug("no device snapshot to send")
		return
	}
	s.sink.ReportDevices(s.wsClient, devices)
	nlog.Core().Debug("device snapshot sent", "users", len(devices))
}

// reportDevices periodically reports device snapshot to panel.
func (s *Service) reportDevices() {
	s.sendDeviceBatch()
}

// ─── Runtime validation ─────────────────────────────────────────────────

func validateNodeRuntime(cfg *config.Config, kcfgSupported []string, spec *model.NodeSpec, tls kernel.TLSCert) error {
	if spec == nil {
		return fmt.Errorf("node spec is nil")
	}
	if !containsString(kcfgSupported, spec.Protocol) {
		return fmt.Errorf("protocol %q is not supported by kernel %q", spec.Protocol, cfg.Kernel.Type)
	}
	if err := validateTLSRequirements(spec, tls, cfgKernelType(cfg)); err != nil {
		return err
	}
	if err := validateRuntimeCertConfig(spec); err != nil {
		return err
	}
	return nil
}

func validateTLSRequirements(spec *model.NodeSpec, tls kernel.TLSCert, kernelType string) error {
	if requiresCertificate(spec) && !hasUsableTLSConfig(spec, tls) {
		return fmt.Errorf("protocol %q requires TLS certificate files", spec.Protocol)
	}
	if spec.TLS == 2 {
		if err := validateRealityRequirements(spec, kernelType); err != nil {
			return err
		}
	}
	return nil
}

func hasUsableTLSConfig(spec *model.NodeSpec, tls kernel.TLSCert) bool {
	if tls.HasCert() {
		return true
	}
	if spec == nil || spec.CertConfig == nil {
		return false
	}
	mode := strings.ToLower(strings.TrimSpace(spec.CertConfig.CertMode))
	switch mode {
	case "self":
		return true
	case "content":
		return strings.TrimSpace(spec.CertConfig.CertContent) != "" && strings.TrimSpace(spec.CertConfig.KeyContent) != ""
	case "file":
		return strings.TrimSpace(spec.CertConfig.CertFile) != "" && strings.TrimSpace(spec.CertConfig.KeyFile) != ""
	case "http":
		return strings.TrimSpace(spec.CertConfig.Domain) != ""
	case "dns":
		return strings.TrimSpace(spec.CertConfig.Domain) != "" && strings.TrimSpace(spec.CertConfig.DNSProvider) != ""
	default:
		return false
	}
}

func validateRuntimeCertConfig(spec *model.NodeSpec) error {
	if spec == nil || spec.CertConfig == nil {
		return nil
	}
	mode := strings.ToLower(strings.TrimSpace(spec.CertConfig.CertMode))
	if mode != "dns" {
		return nil
	}
	provider := strings.TrimSpace(spec.CertConfig.DNSProvider)
	if provider == "" {
		return fmt.Errorf("dns cert mode requires cert_config.dns_provider")
	}
	if _, ok := dnsproviders.Get(provider); !ok {
		return fmt.Errorf("unsupported cert_config.dns_provider %q (supported: %s)", provider, strings.Join(dnsproviders.CanonicalNames(), ", "))
	}
	return nil
}

func validateRealityRequirements(spec *model.NodeSpec, _ string) error {
	if spec.TLSSettings == nil {
		return fmt.Errorf("reality tls requires tls_settings")
	}
	privateKey := strings.TrimSpace(stringValue(spec.TLSSettings["private_key"]))
	serverName := strings.TrimSpace(stringValue(spec.TLSSettings["server_name"]))
	dest := strings.TrimSpace(stringValue(spec.TLSSettings["dest"]))
	if privateKey == "" {
		return fmt.Errorf("reality tls requires tls_settings.private_key")
	}
	if serverName == "" && dest == "" {
		return fmt.Errorf("reality tls requires tls_settings.server_name or tls_settings.dest")
	}
	return nil
}

func cfgKernelType(cfg *config.Config) string {
	if cfg == nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(cfg.Kernel.Type))
}

func containsString(items []string, target string) bool {
	for _, item := range items {
		if item == target {
			return true
		}
	}
	return false
}

func stringValue(v any) string {
	switch value := v.(type) {
	case string:
		return value
	default:
		return ""
	}
}

// retryLegacyUnsent drains at most four proven-unsent batches after a fresh
// report succeeds. No previous ambiguous audit record is replayed.
func (s *Service) retryLegacyUnsent(payload controlplane.ReportPayload) error {
	if s.cfg == nil || s.cfg.Kernel.ConfigDir == "" {
		return nil
	}
	j, err := reportjournal.Open(filepath.Join(s.cfg.Kernel.ConfigDir, "unconfirmed-reports"))
	if err != nil {
		return err
	}
	batches, err := j.LegacyRetryable()
	if err != nil {
		return err
	}
	if len(batches) > 4 {
		batches = batches[:4]
	}
	for _, batch := range batches {
		if err := j.BeginLegacyRetry(batch.ID); err != nil {
			return err
		}
		attempt := payload
		attempt.Traffic = batch.Traffic
		if err := s.sink.Report(attempt); err != nil {
			if panel.IsReportNotSent(err) {
				if markErr := j.MarkLegacyRetryable(batch.ID); markErr != nil {
					return markErr
				}
			}
			return fmt.Errorf("legacy retry batch %s: %w", batch.ID, err)
		}
		if err := j.RemoveLegacyAudit(batch.ID); err != nil {
			return err
		}
		nlog.Core().Info("proven-unsent legacy traffic batch recovered", "batch_id", batch.ID)
	}
	return nil
}
