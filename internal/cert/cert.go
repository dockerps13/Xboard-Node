package cert

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/caddyserver/certmagic"
	"golang.org/x/crypto/acme"

	"github.com/cedar2025/xboard-node/internal/cert/dnsproviders"
	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/kernel"
	"github.com/cedar2025/xboard-node/internal/nlog"
)

// Manager handles TLS certificate lifecycle.
//
// Supported modes (CertConfig.CertMode):
//   - "http"    — ACME HTTP-01 challenge via certmagic (needs port 80).
//   - "dns"     — ACME DNS-01 challenge via certmagic + libdns provider.
//   - "self"    — Self-signed certificate generated in memory.
//   - "file"    — User-provided certificate and key file paths (CertFile/KeyFile).
//   - "content" — Certificate and key PEM content pushed from the panel.
//   - "none"    — No TLS. The node will handle plain connections.
//
// All modes normalise to in-memory PEM. Kernels never see file paths —
// they always receive PEM bytes via TLSCert().
//
// Priority Logic (when CertMode is empty):
//
//  1. "http" (if auto_tls is true)
//  2. "content" (if both CertContent and KeyContent are provided)
//  3. "file" (if both CertFile and KeyFile paths are provided)
//  4. "none" (default)
type Manager struct {
	cfg config.CertConfig

	// Atomic so the ACME renewal goroutine can swap without racing readers.
	mat atomic.Pointer[certMaterial]

	magic           *certmagic.Config
	cache           *certmagic.Cache
	renewed         atomic.Bool
	acmeStarted     bool
	acmeFingerprint string
	acmeCancel      context.CancelFunc
	acmeStorage     certmagic.Storage
	acmeIssuerKey   string
	acmeDomain      string
}

// certMaterial is an immutable snapshot of PEM-encoded cert + key.
type certMaterial struct {
	certPEM []byte
	keyPEM  []byte
	domain  string // immutable issuance scope, safe for concurrent health readers
}

// NewManager creates a certificate manager.
func NewManager(cfg config.CertConfig) *Manager {
	return &Manager{cfg: cfg}
}

// storePEM atomically swaps the in-memory cert material and persists to disk
// so that restarts can reload without re-generating or re-requesting.
func (m *Manager) storePEM(cert, key []byte) {
	m.mat.Store(&certMaterial{certPEM: cert, keyPEM: key, domain: m.cfg.Domain})
	m.persistPEM(cert, key)
}

// persistPEM writes cert/key to cert_dir for restart recovery.
// Errors are logged but not returned — persistence is best-effort.
func (m *Manager) persistPEM(cert, key []byte) {
	dir := m.cfg.CertDir
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		nlog.Core().Warn("cert: failed to create cert_dir", "dir", dir, "error", err)
		return
	}
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")
	if err := atomicWriteFile(certPath, cert, 0o644); err != nil {
		nlog.Core().Warn("cert: failed to persist cert", "path", certPath, "error", err)
	}
	if err := atomicWriteFile(keyPath, key, 0o600); err != nil {
		nlog.Core().Warn("cert: failed to persist key", "path", keyPath, "error", err)
	}
}

// atomicWriteFile writes data to a temp file and renames, preventing partial reads.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmpFile, err := os.CreateTemp(dir, ".xboard-cert-*")
	if err != nil {
		return err
	}
	tmp := tmpFile.Name()
	defer os.Remove(tmp)
	if err := tmpFile.Chmod(perm); err != nil {
		_ = tmpFile.Close()
		return err
	}
	if _, err := tmpFile.Write(data); err != nil {
		_ = tmpFile.Close()
		return err
	}
	if err := tmpFile.Sync(); err != nil {
		_ = tmpFile.Close()
		return err
	}
	if err := tmpFile.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// loadPersistedPEM loads previously persisted cert material from cert_dir.
// Returns true if valid material was loaded.
func (m *Manager) loadPersistedPEM() bool {
	dir := m.cfg.CertDir
	if dir == "" {
		return false
	}
	certPEM, err := os.ReadFile(filepath.Join(dir, "cert.pem"))
	if err != nil {
		return false
	}
	keyPEM, err := os.ReadFile(filepath.Join(dir, "key.pem"))
	if err != nil {
		return false
	}
	if err := validateCertificatePair(certPEM, keyPEM, m.cfg.Domain); err != nil {
		nlog.Core().Warn("cert: persisted cert invalid, will regenerate", "error", err)
		return false
	}
	// Store in memory only (don't re-persist what we just read).
	m.mat.Store(&certMaterial{certPEM: certPEM, keyPEM: keyPEM, domain: m.cfg.Domain})
	nlog.Core().Info("cert: loaded persisted certificate from disk", "dir", dir)
	return true
}

// Reconfigure applies a new cert configuration at runtime (e.g. from panel push).
// Returns true when the PEM material changed (caller should restart kernel).
func (m *Manager) Reconfigure(ctx context.Context, newCfg config.CertConfig) (bool, error) {
	if newCfg.CertDir == "" {
		newCfg.CertDir = m.cfg.CertDir
	}
	if newCfg.ACMEStorageDir == "" {
		newCfg.ACMEStorageDir = m.cfg.ACMEStorageDir
	}
	oldTLS := m.TLSCert()

	// If ACME is running and the new config materially differs (or switches
	// away from ACME), tear down the old certmagic instance first.
	if m.acmeStarted {
		newFp := acmeFingerprint(newCfg)
		newMode := resolveModeFor(newCfg)
		if newMode != "http" && newMode != "dns" || newFp != m.acmeFingerprint {
			m.tearDownACME()
		}
	}

	m.cfg = newCfg

	if err := m.Start(ctx); err != nil {
		return false, fmt.Errorf("cert reconfigure: %w", err)
	}

	newTLS := m.TLSCert()
	return !pemEqual(oldTLS, newTLS), nil
}

// tearDownACME cancels the running certmagic background goroutines and clears
// related state so a fresh ACME init can run.
func (m *Manager) tearDownACME() {
	if m.acmeCancel != nil {
		m.acmeCancel()
		m.acmeCancel = nil
	}
	if m.cache != nil {
		m.cache.Stop()
		m.cache = nil
	}
	m.magic = nil
	m.acmeStarted = false
	m.acmeFingerprint = ""
	m.acmeStorage = nil
	m.acmeIssuerKey = ""
	m.acmeDomain = ""
	m.renewed.Store(false)
	m.mat.Store(nil)
}

// acmeFingerprint produces a stable string capturing every config field that
// requires re-running ACME when changed.
func acmeFingerprint(cfg config.CertConfig) string {
	keys := make([]string, 0, len(cfg.DNSEnv))
	for k := range cfg.DNSEnv {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(strings.ToLower(strings.TrimSpace(cfg.CertMode)))
	b.WriteByte('|')
	b.WriteString(strings.TrimSpace(cfg.Domain))
	b.WriteByte('|')
	b.WriteString(strings.TrimSpace(cfg.Email))
	b.WriteByte('|')
	b.WriteString(strings.TrimSpace(cfg.DNSProvider))
	b.WriteByte('|')
	b.WriteString(strconv.Itoa(cfg.HTTPPort))
	b.WriteByte('|')
	b.WriteString(strings.TrimSpace(cfg.CertDir))
	b.WriteByte('|')
	b.WriteString(strings.TrimSpace(cfg.ACMEStorageDir))
	b.WriteByte('|')
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(cfg.DNSEnv[k])
		b.WriteByte(';')
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// resolveModeFor mirrors (*Manager).resolveMode() for an arbitrary cfg, used
// by Reconfigure to decide tear-down without mutating manager state.
func resolveModeFor(cfg config.CertConfig) string {
	mode := strings.ToLower(strings.TrimSpace(cfg.CertMode))
	if mode != "" {
		return mode
	}
	if cfg.AutoTLS {
		return "http"
	}
	if cfg.CertContent != "" && cfg.KeyContent != "" {
		return "content"
	}
	if cfg.CertFile != "" && cfg.KeyFile != "" {
		return "file"
	}
	return "none"
}

// resolveMode returns the effective cert mode, handling backward compat for auto_tls.
func (m *Manager) resolveMode() string {
	mode := strings.ToLower(strings.TrimSpace(m.cfg.CertMode))
	if mode != "" {
		return mode
	}
	// Backward compat: auto_tls: true → "http"
	if m.cfg.AutoTLS {
		return "http"
	}
	// Inline PEM content provided → "content"
	if m.cfg.CertContent != "" && m.cfg.KeyContent != "" {
		return "content"
	}
	// If cert/key file paths are provided → "file"
	if m.cfg.CertFile != "" && m.cfg.KeyFile != "" {
		return "file"
	}
	return "none"
}

func (m *Manager) HasCert() bool {
	return m.TLSCert().HasCert()
}
func (m *Manager) CertRenewed() bool { return m.renewed.Swap(false) }

// RefreshSharedCertificate is called by the service main loop. A certificate
// renewed by another node is visible in shared storage even if this manager's
// own CertMagic cache did not perform the renewal.
func (m *Manager) RefreshSharedCertificate(ctx context.Context) (bool, error) {
	if !m.acmeStarted || m.acmeStorage == nil {
		return false, nil
	}
	old := m.TLSCert()
	if err := m.loadPEMFromStorage(ctx, m.acmeStorage, m.acmeIssuerKey, m.acmeDomain); err != nil {
		return false, err
	}
	return !pemEqual(old, m.TLSCert()), nil
}

// TLSCert returns the current PEM material as a kernel.TLSCert.
func (m *Manager) TLSCert() kernel.TLSCert {
	mat := m.mat.Load()
	if mat == nil {
		return kernel.TLSCert{}
	}
	// Expired or mis-scoped material must never be handed to a TLS listener,
	// even if its original in-memory snapshot was valid at startup.
	if err := validateCertificatePair(mat.certPEM, mat.keyPEM, mat.domain); err != nil {
		return kernel.TLSCert{}
	}
	return kernel.TLSCert{CertPEM: mat.certPEM, KeyPEM: mat.keyPEM}
}

// pemEqual reports whether two TLSCert values carry identical PEM content.
func pemEqual(a, b kernel.TLSCert) bool {
	return string(a.CertPEM) == string(b.CertPEM) && string(a.KeyPEM) == string(b.KeyPEM)
}

// Start initializes the cert manager based on the resolved mode.
func (m *Manager) Start(ctx context.Context) error {
	mode := m.resolveMode()

	switch mode {
	case "none", "":
		if m.acmeStarted {
			m.tearDownACME()
		} else {
			m.mat.Store(nil)
		}
		return nil
	case "file":
		return m.startFile()
	case "self":
		return m.startSelfSigned()
	case "content":
		return m.startContent()
	case "http":
		return m.startACME(ctx, nil)
	case "dns":
		solver, err := m.buildDNSSolver()
		if err != nil {
			return fmt.Errorf("build dns solver: %w", err)
		}
		return m.startACME(ctx, solver)
	default:
		return fmt.Errorf("unknown cert_mode: %q (supported: http, dns, self, file, content, none)", mode)
	}
}

// Stop cancels certmagic's background renewal goroutine and clears the
// in-memory material so no listener can continue using a stopped manager.
func (m *Manager) Stop() {
	if m.acmeStarted || m.acmeCancel != nil {
		m.tearDownACME()
		return
	}
	m.mat.Store(nil)
}

// ─── Mode: file ────────────────────────────────────────────────────────────

func (m *Manager) startFile() error {
	certPEM, err := os.ReadFile(m.cfg.CertFile)
	if err != nil {
		return fmt.Errorf("cert file: %w", err)
	}
	keyPEM, err := os.ReadFile(m.cfg.KeyFile)
	if err != nil {
		return fmt.Errorf("key file: %w", err)
	}
	if err := validateCertificatePair(certPEM, keyPEM, m.cfg.Domain); err != nil {
		return fmt.Errorf("invalid certificate pair: %w", err)
	}
	m.storePEM(certPEM, keyPEM)
	nlog.Core().Debug("TLS certificate loaded from files", "cert", m.cfg.CertFile, "key", m.cfg.KeyFile)
	return nil
}

// ─── Mode: self ────────────────────────────────────────────────────────────

func (m *Manager) startSelfSigned() error {
	// Reuse persisted self-signed cert if available and valid.
	if m.loadPersistedPEM() {
		return nil
	}

	domain := m.cfg.Domain
	if domain == "" {
		domain = "localhost"
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generate key: %w", err)
	}

	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return fmt.Errorf("generate serial: %w", err)
	}

	template := &x509.Certificate{
		SerialNumber: serialNumber,
		Subject:      pkix.Name{CommonName: domain},
		NotBefore:    time.Now().Add(-1 * time.Hour),
		NotAfter:     time.Now().Add(10 * 365 * 24 * time.Hour), // 10 years
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}

	// Add SANs
	if ip := net.ParseIP(domain); ip != nil {
		template.IPAddresses = []net.IP{ip}
	} else {
		template.DNSNames = []string{domain}
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return fmt.Errorf("create certificate: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return fmt.Errorf("marshal key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	m.storePEM(certPEM, keyPEM)
	nlog.Core().Info("self-signed certificate generated (in-memory)", "domain", domain, "valid_years", 10)
	return nil
}

// ─── Mode: content ────────────────────────────────────────────────────────

// startContent stores panel-supplied PEM strings in memory for inline use.
func (m *Manager) startContent() error {
	if m.cfg.CertContent == "" || m.cfg.KeyContent == "" {
		// No fresh content from panel — try previously persisted material.
		if m.loadPersistedPEM() {
			return nil
		}
		return fmt.Errorf("cert_mode 'content' requires both cert_content and key_content")
	}
	certPEM := []byte(m.cfg.CertContent)
	keyPEM := []byte(m.cfg.KeyContent)
	if err := validateCertificatePair(certPEM, keyPEM, m.cfg.Domain); err != nil {
		return fmt.Errorf("invalid certificate content: %w", err)
	}
	m.storePEM(certPEM, keyPEM)
	nlog.Core().Info("TLS certificate loaded from panel content (in-memory)")
	return nil
}

// ─── Mode: http / dns (ACME) ──────────────────────────────────────────────

// acmeStorageDirectory isolates standalone configurations while sharing ACME
// resources between nodes belonging to the same machine instance.
func acmeStorageDirectory(cfg config.CertConfig) string {
	if cfg.ACMEStorageDir != "" {
		return cfg.ACMEStorageDir
	}
	return cfg.CertDir
}

// migrateACMEStorage copies files from the pre-shared CertDir into the
// machine-wide certmagic directory without overwriting existing shared state.
// Existing shared files win because they may have been created by another
// node. Temporary files and lock files are deliberately not migrated.
func migrateACMEStorage(legacyDir, storageDir string) error {
	if legacyDir == "" || storageDir == "" {
		return nil
	}
	legacyAbs, err := filepath.Abs(legacyDir)
	if err != nil {
		return fmt.Errorf("resolve legacy ACME directory: %w", err)
	}
	storageAbs, err := filepath.Abs(storageDir)
	if err != nil {
		return fmt.Errorf("resolve shared ACME directory: %w", err)
	}
	if filepath.Clean(legacyAbs) == filepath.Clean(storageAbs) {
		return nil
	}
	if _, err := os.Stat(legacyAbs); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat legacy ACME directory: %w", err)
	}
	return copyMissingTree(legacyAbs, storageAbs)
}

func copyMissingTree(srcDir, dstDir string) error {
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dstDir, 0o700); err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if name == ".acme.lock" || name == "locks" || name == "cert.pem" || name == "key.pem" || strings.HasPrefix(name, ".acme-retry-") || strings.HasSuffix(name, ".tmp") {
			continue
		}
		srcPath := filepath.Join(srcDir, name)
		dstPath := filepath.Join(dstDir, name)
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing to migrate symlink %q", srcPath)
		}
		if entry.IsDir() {
			if info, statErr := os.Stat(dstPath); statErr == nil && !info.IsDir() {
				continue
			} else if statErr != nil && !os.IsNotExist(statErr) {
				return statErr
			}
			if err := copyMissingTree(srcPath, dstPath); err != nil {
				return fmt.Errorf("migrate %q: %w", srcPath, err)
			}
			continue
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("refusing to migrate non-regular file %q", srcPath)
		}
		if _, err := os.Stat(dstPath); err == nil {
			continue
		} else if !os.IsNotExist(err) {
			return err
		}
		if err := copyFileExclusive(srcPath, dstPath, entry); err != nil {
			return fmt.Errorf("migrate %q: %w", srcPath, err)
		}
	}
	return nil
}

func copyFileExclusive(srcPath, dstPath string, entry os.DirEntry) error {
	src, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer src.Close()

	info, err := entry.Info()
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dstPath), ".xboard-acme-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(info.Mode().Perm()); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := io.Copy(tmp, src); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// Linking the completed temporary file is an atomic no-clobber install on
	// the same filesystem. A concurrent node that won the race is authoritative.
	if err := os.Link(tmpPath, dstPath); err != nil {
		if os.IsExist(err) {
			return nil
		}
		return err
	}
	return nil
}

// acquireACMELock serializes the initial ACME obtain operation across nodes
// sharing the same machine-local storage directory.
func acquireACMELock(ctx context.Context, storageDir string) (func(), error) {
	lockFile, err := os.OpenFile(filepath.Join(storageDir, ".acme.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	unlock := func() {
		_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
		_ = lockFile.Close()
	}
	for {
		err = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return unlock, nil
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN {
			unlock()
			return nil, err
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			unlock()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

// Let's Encrypt exposes this timestamp in the ACME problem detail even when
// the wrapped error does not expose the HTTP response headers.
var acmeRetryTimePattern = regexp.MustCompile(`(?i)retry after ([0-9]{4}-[0-9]{2}-[0-9]{2} [0-9]{2}:[0-9]{2}:[0-9]{2}) UTC`)

func acmeRetryDelay(err error, _ int) (time.Duration, bool) {
	var acmeErr *acme.Error
	isRateLimited := errors.As(err, &acmeErr) && acmeErr.StatusCode == http.StatusTooManyRequests
	detail := strings.ToLower(err.Error())
	if !isRateLimited && !strings.Contains(detail, "http 429") && !strings.Contains(detail, "ratelimited") {
		return 0, false
	}

	now := time.Now()
	var until time.Time
	if acmeErr != nil && acmeErr.Header != nil {
		if value := strings.TrimSpace(acmeErr.Header.Get("Retry-After")); value != "" {
			if seconds, parseErr := strconv.Atoi(value); parseErr == nil && seconds >= 0 && seconds <= 31536000 {
				until = now.Add(time.Duration(seconds) * time.Second)
			} else if when, parseErr := http.ParseTime(value); parseErr == nil {
				until = when
			}
		}
	}
	if match := acmeRetryTimePattern.FindStringSubmatch(err.Error()); len(match) == 2 {
		if when, parseErr := time.ParseInLocation("2006-01-02 15:04:05", match[1], time.UTC); parseErr == nil && when.After(until) {
			until = when
		}
	}
	if until.IsZero() {
		// Unknown deadline: prefer a conservative pause over hammering the CA.
		return 24 * time.Hour, true
	}
	if !until.After(now) {
		return time.Minute, true
	}
	return until.Sub(now), true
}

// The caller holds the machine storage lock. A per-domain marker survives
// process restarts and is shared by every node using that storage.
func acmeCooldownPath(storageDir, domain string) string {
	normalized := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(domain)), ".")
	sum := sha256.Sum256([]byte(normalized))
	return filepath.Join(storageDir, ".acme-retry-"+hex.EncodeToString(sum[:]))
}

func obtainACMEWithCooldown(storageDir, domain string, obtain func() error) error {
	path := acmeCooldownPath(storageDir, domain)
	if data, err := os.ReadFile(path); err == nil {
		until, parseErr := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(data)))
		if parseErr != nil {
			return fmt.Errorf("invalid ACME cooldown marker: %w", parseErr)
		}
		if time.Now().Before(until) {
			return fmt.Errorf("ACME rate limit cooldown for %s until %s", domain, until.UTC().Format(time.RFC3339))
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read ACME cooldown: %w", err)
	}
	err := obtain()
	if err == nil {
		if removeErr := os.Remove(path); removeErr != nil && !os.IsNotExist(removeErr) {
			return fmt.Errorf("clear ACME cooldown: %w", removeErr)
		}
		return nil
	}
	if delay, limited := acmeRetryDelay(err, 0); limited {
		until := time.Now().Add(delay).UTC()
		if writeErr := atomicWriteFile(path, []byte(until.Format(time.RFC3339Nano)+"\n"), 0o600); writeErr != nil {
			return fmt.Errorf("persist ACME cooldown: %w (issuer: %v)", writeErr, err)
		}
	}
	return err
}

// acmeRateLimitEventGuard applies to both initial issuance and background
// renewal. CertMagic invokes cert_obtaining before calling the issuer and
// cert_failed after a failed issuer attempt. The shared marker ensures a
// throttled renewal never creates another production ACME order.
func acmeRateLimitEventGuard(storageDir, event string, data map[string]any) error {
	domain, _ := data["identifier"].(string)
	if domain == "" {
		return nil
	}
	path := acmeCooldownPath(storageDir, domain)
	switch event {
	case "cert_obtaining":
		bytes, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return certmagic.ErrNoRetry{Err: fmt.Errorf("read ACME cooldown: %w", err)}
		}
		until, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(bytes)))
		if err != nil {
			return certmagic.ErrNoRetry{Err: fmt.Errorf("invalid ACME cooldown marker: %w", err)}
		}
		if time.Now().Before(until) {
			return certmagic.ErrNoRetry{Err: fmt.Errorf("ACME cooldown for %s until %s", domain, until.UTC().Format(time.RFC3339))}
		}
	case "cert_failed":
		failed, _ := data["error"].(error)
		if failed == nil {
			return nil
		}
		if delay, limited := acmeRetryDelay(failed, 0); limited {
			newUntil := time.Now().Add(delay).UTC()
			if old, err := os.ReadFile(path); err == nil {
				if oldUntil, parseErr := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(old))); parseErr == nil && oldUntil.After(newUntil) {
					newUntil = oldUntil
				}
			}
			if err := atomicWriteFile(path, []byte(newUntil.Format(time.RFC3339Nano)+"\n"), 0o600); err != nil {
				return fmt.Errorf("persist renewal ACME cooldown: %w", err)
			}
		}
	}
	return nil
}

func obtainACMECertificate(ctx context.Context, magic *certmagic.Config, domain string) error {
	return magic.ObtainCertSync(ctx, domain)
}

func (m *Manager) startACME(ctx context.Context, dnsSolver *certmagic.DNS01Solver) error {
	// Idempotent for unchanged config: Reconfigure already tore down stale ACME state.
	if m.acmeStarted {
		return nil
	}

	domain := strings.TrimSpace(m.cfg.Domain)
	if domain == "" {
		return fmt.Errorf("cert.domain is required for ACME modes (http/dns)")
	}

	if err := os.MkdirAll(m.cfg.CertDir, 0o755); err != nil {
		return fmt.Errorf("create cert dir: %w", err)
	}

	storageDir := acmeStorageDirectory(m.cfg)
	if err := os.MkdirAll(storageDir, 0o700); err != nil {
		return fmt.Errorf("create shared ACME storage: %w", err)
	}
	acmeCtx, acmeCancel := context.WithCancel(ctx)
	unlock, err := acquireACMELock(acmeCtx, storageDir)
	if err != nil {
		acmeCancel()
		return fmt.Errorf("lock shared ACME storage: %w", err)
	}
	defer unlock()
	if err := migrateACMEStorage(m.cfg.CertDir, storageDir); err != nil {
		acmeCancel()
		return fmt.Errorf("migrate ACME storage: %w", err)
	}
	storage := &certmagic.FileStorage{Path: storageDir}

	var magic *certmagic.Config
	cache := certmagic.NewCache(certmagic.CacheOptions{
		GetConfigForCert: func(_ certmagic.Certificate) (*certmagic.Config, error) {
			return magic, nil
		},
	})

	magic = certmagic.New(cache, certmagic.Config{
		Storage: storage,
		OnEvent: func(evtCtx context.Context, event string, data map[string]any) error {
			if err := acmeRateLimitEventGuard(storageDir, event, data); err != nil {
				return err
			}
			// Only react to renewals; initial load happens explicitly after ObtainCertSync.
			if event != "cert_obtained" {
				return nil
			}
			if renewed, _ := data["renewal"].(bool); !renewed {
				return nil
			}
			issuerKey, _ := data["issuer"].(string)
			if issuerKey == "" {
				return nil
			}
			// The service main loop reads the updated shared pair. Do not
			// access mutable Manager configuration from this callback.
			m.renewed.Store(true)
			return nil
		},
	})

	issuer := certmagic.ACMEIssuer{
		CA:    certmagic.LetsEncryptProductionCA,
		Email: m.cfg.Email,
	}

	if dnsSolver != nil {
		// DNS-01 mode: no HTTP port needed, supports wildcards.
		issuer.DNS01Solver = dnsSolver
		issuer.DisableHTTPChallenge = true
		issuer.DisableTLSALPNChallenge = true
	} else {
		// HTTP-01 mode.
		httpPort := m.cfg.HTTPPort
		if httpPort == 0 {
			httpPort = 80
		}
		issuer.AltHTTPPort = httpPort
		issuer.DisableTLSALPNChallenge = true
	}

	magic.Issuers = []certmagic.Issuer{
		certmagic.NewACMEIssuer(magic, issuer),
	}
	m.magic = magic

	issuerKey := magic.Issuers[0].IssuerKey()
	// Load an already valid domain-specific cached pair before any issuance.
	if err := m.loadPEMFromStorage(acmeCtx, storage, issuerKey, domain); err != nil {
		if err := obtainACMEWithCooldown(storageDir, domain, func() error {
			return obtainACMECertificate(acmeCtx, magic, domain)
		}); err != nil {
			acmeCancel()
			cache.Stop()
			m.magic = nil
			return fmt.Errorf("obtain certificate: %w", err)
		}
		if err := m.loadPEMFromStorage(acmeCtx, storage, issuerKey, domain); err != nil {
			acmeCancel()
			cache.Stop()
			m.magic = nil
			return fmt.Errorf("load cert from storage: %w", err)
		}
	}

	if err := magic.ManageAsync(acmeCtx, []string{domain}); err != nil {
		acmeCancel()
		cache.Stop()
		m.magic = nil
		m.mat.Store(nil)
		return fmt.Errorf("start cert manager: %w", err)
	}

	m.cache = cache
	m.acmeStorage = storage
	m.acmeIssuerKey = issuerKey
	m.acmeDomain = domain
	m.acmeCancel = acmeCancel
	m.acmeFingerprint = acmeFingerprint(m.cfg)
	m.acmeStarted = true
	return nil
}

func validateCertificatePair(certPEM, keyPEM []byte, domain string) error {
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return err
	}
	if len(pair.Certificate) == 0 {
		return fmt.Errorf("certificate chain is empty")
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return fmt.Errorf("parse leaf certificate: %w", err)
	}
	now := time.Now()
	if now.Before(leaf.NotBefore) {
		return fmt.Errorf("certificate is not valid before %s", leaf.NotBefore.Format(time.RFC3339))
	}
	if !now.Before(leaf.NotAfter) {
		return fmt.Errorf("certificate expired at %s", leaf.NotAfter.Format(time.RFC3339))
	}
	if domain = strings.TrimSpace(domain); domain != "" {
		if err := leaf.VerifyHostname(domain); err != nil {
			return fmt.Errorf("certificate does not cover %q: %w", domain, err)
		}
	}
	return nil
}

func validateKeyPair(certPEM, keyPEM []byte) error {
	return validateCertificatePair(certPEM, keyPEM, "")
}

// ─── DNS Provider Factory ──────────────────────────────────────────────────

// buildDNSSolver builds a certmagic DNS01Solver from the configured provider.
func (m *Manager) buildDNSSolver() (*certmagic.DNS01Solver, error) {
	provider, err := m.newDNSProvider()
	if err != nil {
		return nil, err
	}
	return &certmagic.DNS01Solver{DNSManager: certmagic.DNSManager{DNSProvider: provider}}, nil
}

func (m *Manager) newDNSProvider() (certmagic.DNSProvider, error) {
	name := strings.TrimSpace(m.cfg.DNSProvider)
	if name == "" {
		return nil, fmt.Errorf("dns_provider is required for cert_mode=dns")
	}
	env := m.cfg.DNSEnv
	if env == nil {
		env = map[string]string{}
	}
	p, ok := dnsproviders.Get(name)
	if !ok {
		return nil, fmt.Errorf("unsupported dns_provider: %q (supported: %s)",
			name, strings.Join(dnsproviders.CanonicalNames(), ", "))
	}
	return p.Build(env)
}

// ─── Helpers ───────────────────────────────────────────────────────────────

// loadPEMFromStorage reads cert + key for domain from certmagic storage into
// the in-memory cache. Single entry point for both initial load and renewal refresh.
func (m *Manager) loadPEMFromStorage(ctx context.Context, storage certmagic.Storage, issuerKey, domain string) error {
	if issuerKey == "" || domain == "" {
		return fmt.Errorf("loadPEMFromStorage: issuerKey and domain are required")
	}
	certKey := certmagic.StorageKeys.SiteCert(issuerKey, domain)
	keyKey := certmagic.StorageKeys.SitePrivateKey(issuerKey, domain)

	certPEM, err := storage.Load(ctx, certKey)
	if err != nil {
		return fmt.Errorf("load cert %q: %w", certKey, err)
	}
	keyPEM, err := storage.Load(ctx, keyKey)
	if err != nil {
		return fmt.Errorf("load key %q: %w", keyKey, err)
	}
	if err := validateCertificatePair(certPEM, keyPEM, domain); err != nil {
		return fmt.Errorf("invalid cert/key pair on disk: %w", err)
	}
	current := m.TLSCert()
	if string(current.CertPEM) != string(certPEM) || string(current.KeyPEM) != string(keyPEM) {
		m.storePEM(certPEM, keyPEM)
	}
	return nil
}
