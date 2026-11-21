package cert

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caddyserver/certmagic"
	"golang.org/x/crypto/acme"

	"github.com/cedar2025/xboard-node/internal/config"
)

// generateSelfSignedPair returns a fresh self-signed certificate and matching
// private key in PEM form, suitable for storing as a libdns/certmagic site
// resource in tests.
func generateSelfSignedPair(t *testing.T, domain string) (certPEM, keyPEM []byte) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: domain},
		DNSNames:     []string{domain},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return
}

// TestLoadPEMFromStorage_RestartScenario simulates the warm-restart bug fix:
// certmagic.ObtainCertSync returns nil without firing cert_obtained when a
// valid cert already exists in storage. The manager must still populate its
// in-memory PEM cache by reading storage explicitly.
func TestLoadPEMFromStorage_RestartScenario(t *testing.T) {
	dir := t.TempDir()
	storage := &certmagic.FileStorage{Path: dir}
	const domain = "example.test"
	const issuerKey = "test-issuer"

	certPEM, keyPEM := generateSelfSignedPair(t, domain)

	ctx := context.Background()
	if err := storage.Store(ctx, certmagic.StorageKeys.SiteCert(issuerKey, domain), certPEM); err != nil {
		t.Fatalf("seed cert: %v", err)
	}
	if err := storage.Store(ctx, certmagic.StorageKeys.SitePrivateKey(issuerKey, domain), keyPEM); err != nil {
		t.Fatalf("seed key: %v", err)
	}

	m := NewManager(config.CertConfig{})
	if m.HasCert() {
		t.Fatal("manager should start with no cert in memory")
	}

	if err := m.loadPEMFromStorage(ctx, storage, issuerKey, domain); err != nil {
		t.Fatalf("loadPEMFromStorage: %v", err)
	}
	if !m.HasCert() {
		t.Fatal("expected HasCert() == true after explicit load")
	}
	got := m.TLSCert()
	if string(got.CertPEM) != string(certPEM) {
		t.Errorf("certPEM mismatch")
	}
	if string(got.KeyPEM) != string(keyPEM) {
		t.Errorf("keyPEM mismatch")
	}
}

// TestLoadPEMFromStorage_RenewalRefresh confirms that calling load a second
// time with newer bytes on disk swaps the in-memory cache atomically — this
// is the renewal hot-reload path triggered from the cert_obtained event.
func TestLoadPEMFromStorage_RenewalRefresh(t *testing.T) {
	dir := t.TempDir()
	storage := &certmagic.FileStorage{Path: dir}
	const domain = "example.test"
	const issuerKey = "test-issuer"
	ctx := context.Background()

	certV1, keyV1 := generateSelfSignedPair(t, domain)
	if err := storage.Store(ctx, certmagic.StorageKeys.SiteCert(issuerKey, domain), certV1); err != nil {
		t.Fatalf("seed v1 cert: %v", err)
	}
	if err := storage.Store(ctx, certmagic.StorageKeys.SitePrivateKey(issuerKey, domain), keyV1); err != nil {
		t.Fatalf("seed v1 key: %v", err)
	}

	m := NewManager(config.CertConfig{})
	if err := m.loadPEMFromStorage(ctx, storage, issuerKey, domain); err != nil {
		t.Fatalf("initial load: %v", err)
	}

	certV2, keyV2 := generateSelfSignedPair(t, domain)
	if err := storage.Store(ctx, certmagic.StorageKeys.SiteCert(issuerKey, domain), certV2); err != nil {
		t.Fatalf("seed v2 cert: %v", err)
	}
	if err := storage.Store(ctx, certmagic.StorageKeys.SitePrivateKey(issuerKey, domain), keyV2); err != nil {
		t.Fatalf("seed v2 key: %v", err)
	}

	if err := m.loadPEMFromStorage(ctx, storage, issuerKey, domain); err != nil {
		t.Fatalf("refresh load: %v", err)
	}
	got := m.TLSCert()
	if string(got.CertPEM) != string(certV2) {
		t.Error("expected v2 cert after refresh")
	}
	if string(got.KeyPEM) != string(keyV2) {
		t.Error("expected v2 key after refresh")
	}
}

// TestLoadPEMFromStorage_MissingFiles ensures we surface a clear error when
// storage doesn't have the expected keys (defensive — should not happen in
// practice because ObtainCertSync guarantees presence on success).
func TestLoadPEMFromStorage_MissingFiles(t *testing.T) {
	dir := t.TempDir()
	storage := &certmagic.FileStorage{Path: dir}
	m := NewManager(config.CertConfig{})
	err := m.loadPEMFromStorage(context.Background(), storage, "test-issuer", "example.test")
	if err == nil {
		t.Fatal("expected error when storage is empty")
	}
}

// TestLoadPEMFromStorage_RejectsMismatchedPair guards against silently loading
// a cert and key that do not pair (e.g. partial write or external tampering).
func TestLoadPEMFromStorage_RejectsMismatchedPair(t *testing.T) {
	dir := t.TempDir()
	storage := &certmagic.FileStorage{Path: dir}
	const domain = "example.test"
	const issuerKey = "test-issuer"
	ctx := context.Background()

	certA, _ := generateSelfSignedPair(t, domain)
	_, keyB := generateSelfSignedPair(t, domain)
	if err := storage.Store(ctx, certmagic.StorageKeys.SiteCert(issuerKey, domain), certA); err != nil {
		t.Fatalf("seed cert: %v", err)
	}
	if err := storage.Store(ctx, certmagic.StorageKeys.SitePrivateKey(issuerKey, domain), keyB); err != nil {
		t.Fatalf("seed key: %v", err)
	}

	m := NewManager(config.CertConfig{})
	err := m.loadPEMFromStorage(ctx, storage, issuerKey, domain)
	if err == nil {
		t.Fatal("expected error for mismatched cert/key pair")
	}
	if m.HasCert() {
		t.Error("manager should not store invalid pair")
	}
	_ = filepath.Join(dir, "stub")
}

// TestACMEFingerprint covers the config-equality logic that decides whether
// Reconfigure must tear down a running certmagic instance.
func TestACMEFingerprint(t *testing.T) {
	base := config.CertConfig{
		CertMode:    "dns",
		Domain:      "a.example.com",
		Email:       "ops@example.com",
		DNSProvider: "cloudflare",
		DNSEnv:      map[string]string{"CLOUDFLARE_API_TOKEN": "tok"},
	}

	cases := []struct {
		name    string
		mutate  func(*config.CertConfig)
		differs bool
	}{
		{"same", func(*config.CertConfig) {}, false},
		{"domain", func(c *config.CertConfig) { c.Domain = "b.example.com" }, true},
		{"mode", func(c *config.CertConfig) { c.CertMode = "http" }, true},
		{"email", func(c *config.CertConfig) { c.Email = "other@example.com" }, true},
		{"provider", func(c *config.CertConfig) { c.DNSProvider = "alidns" }, true},
		{"env_value", func(c *config.CertConfig) { c.DNSEnv = map[string]string{"CLOUDFLARE_API_TOKEN": "other"} }, true},
		{"env_key_only", func(c *config.CertConfig) { c.DNSEnv = map[string]string{"OTHER": "tok"} }, true},
	}

	baseFp := acmeFingerprint(base)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			if tc.mutate != nil {
				// deep-copy DNSEnv before mutation to avoid sharing
				cfg.DNSEnv = map[string]string{}
				for k, v := range base.DNSEnv {
					cfg.DNSEnv[k] = v
				}
				tc.mutate(&cfg)
			}
			got := acmeFingerprint(cfg)
			if (got != baseFp) != tc.differs {
				t.Errorf("differs=%v but got==base=%v", tc.differs, got == baseFp)
			}
		})
	}
}

// TestACMEFingerprintEnvOrderStable ensures map iteration order does not
// affect the fingerprint (must be sorted internally).
func TestACMEFingerprintEnvOrderStable(t *testing.T) {
	a := config.CertConfig{
		CertMode: "dns", Domain: "x", DNSProvider: "p",
		DNSEnv: map[string]string{"A": "1", "B": "2", "C": "3"},
	}
	b := config.CertConfig{
		CertMode: "dns", Domain: "x", DNSProvider: "p",
		DNSEnv: map[string]string{"C": "3", "A": "1", "B": "2"},
	}
	if acmeFingerprint(a) != acmeFingerprint(b) {
		t.Fatal("fingerprint not order-independent")
	}
}

func TestACMEStorageDirectory(t *testing.T) {
	cfg := config.CertConfig{CertDir: "/instance/node-17/certs", ACMEStorageDir: "/instance/shared-acme"}
	if got := acmeStorageDirectory(cfg); got != "/instance/shared-acme" {
		t.Fatalf("shared storage = %q", got)
	}
	cfg.CertDir = "/instance/node-25/certs"
	if got := acmeStorageDirectory(cfg); got != "/instance/shared-acme" {
		t.Fatalf("second node storage = %q", got)
	}
	cfg.ACMEStorageDir = ""
	if got := acmeStorageDirectory(cfg); got != cfg.CertDir {
		t.Fatalf("standalone storage = %q", got)
	}
}

func TestValidateCertificatePairChecksDomainAndValidity(t *testing.T) {
	certPEM, keyPEM := generateSelfSignedPair(t, "node.example.test")
	if err := validateCertificatePair(certPEM, keyPEM, "node.example.test"); err != nil {
		t.Fatalf("valid pair rejected: %v", err)
	}
	if err := validateCertificatePair(certPEM, keyPEM, "other.example.test"); err == nil {
		t.Fatal("expected hostname mismatch")
	}
}

func TestMigrateACMEStorageDoesNotOverwriteSharedState(t *testing.T) {
	legacyDir := t.TempDir()
	storageDir := filepath.Join(t.TempDir(), "shared")
	if err := os.MkdirAll(filepath.Join(legacyDir, "accounts"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyDir, "accounts", "account.json"), []byte("legacy"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyDir, "cert.pem"), []byte("legacy-cert"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyDir, "partial.tmp"), []byte("do-not-copy"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(storageDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(storageDir, "cert.pem"), []byte("shared-cert"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := migrateACMEStorage(legacyDir, storageDir); err != nil {
		t.Fatalf("migrateACMEStorage: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(storageDir, "cert.pem"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "shared-cert" {
		t.Fatalf("shared certificate overwritten: %q", got)
	}
	got, err = os.ReadFile(filepath.Join(storageDir, "accounts", "account.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "legacy" {
		t.Fatalf("account cache not migrated: %q", got)
	}
	if _, err := os.Stat(filepath.Join(storageDir, "partial.tmp")); !os.IsNotExist(err) {
		t.Fatalf("temporary file was migrated, stat error=%v", err)
	}
}

func TestACMERetryDelayHonorsRetryAfter(t *testing.T) {
	err := &acme.Error{
		StatusCode: http.StatusTooManyRequests,
		Header:     http.Header{"Retry-After": []string{"7"}},
	}
	delay, retry := acmeRetryDelay(err, 0)
	if !retry || delay != 7*time.Second {
		t.Fatalf("retry-after seconds: delay=%s retry=%v", delay, retry)
	}

	err.Header.Set("Retry-After", time.Now().Add(2*time.Second).UTC().Format(http.TimeFormat))
	delay, retry = acmeRetryDelay(err, 0)
	if !retry || delay <= 0 || delay > 2*time.Second {
		t.Fatalf("retry-after date: delay=%s retry=%v", delay, retry)
	}

	if _, retry := acmeRetryDelay(os.ErrNotExist, 0); retry {
		t.Fatal("non-429 error should not be retried")
	}
}

type countingIssuer struct {
	caCert *x509.Certificate
	caKey  *ecdsa.PrivateKey
	calls  atomic.Int32
	delay  time.Duration
}

func newCountingIssuer(t *testing.T) *countingIssuer {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(9000),
		Subject:      pkix.Name{CommonName: "test issuer"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		IsCA:         true,
		KeyUsage:     x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &countingIssuer{caCert: caCert, caKey: caKey}
}

func (i *countingIssuer) IssuerKey() string { return "test-shared-issuer" }

func (i *countingIssuer) Issue(ctx context.Context, csr *x509.CertificateRequest) (*certmagic.IssuedCertificate, error) {
	if i.delay > 0 {
		timer := time.NewTimer(i.delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	serial := int64(i.calls.Add(1))
	template := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      csr.Subject,
		DNSNames:     append([]string(nil), csr.DNSNames...),
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, i.caCert, csr.PublicKey, i.caKey)
	if err != nil {
		return nil, err
	}
	return &certmagic.IssuedCertificate{
		Certificate: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
	}, nil
}

func newTestMagic(storage certmagic.Storage, issuer certmagic.Issuer) *certmagic.Config {
	var magic *certmagic.Config
	cache := certmagic.NewCache(certmagic.CacheOptions{
		GetConfigForCert: func(_ certmagic.Certificate) (*certmagic.Config, error) {
			return magic, nil
		},
	})
	magic = certmagic.New(cache, certmagic.Config{
		Storage: storage,
		Issuers: []certmagic.Issuer{issuer},
	})
	return magic
}

func TestConcurrentACMEObtainUsesOneIssuerAndSharedCertificate(t *testing.T) {
	storage := &certmagic.FileStorage{Path: t.TempDir()}
	issuer := newCountingIssuer(t)
	issuer.delay = 50 * time.Millisecond
	const domain = "shared.example.test"

	magicA := newTestMagic(storage, issuer)
	magicB := newTestMagic(storage, issuer)
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, magic := range []*certmagic.Config{magicA, magicB} {
		magic := magic
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- obtainACMECertificate(context.Background(), magic, domain)
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := issuer.calls.Load(); got != 1 {
		t.Fatalf("certificate issuer calls = %d, want exactly one", got)
	}

	issuerKey := issuer.IssuerKey()
	certPEM, err := storage.Load(context.Background(), certmagic.StorageKeys.SiteCert(issuerKey, domain))
	if err != nil {
		t.Fatal(err)
	}
	keyPEM, err := storage.Load(context.Background(), certmagic.StorageKeys.SitePrivateKey(issuerKey, domain))
	if err != nil {
		t.Fatal(err)
	}
	managerA := NewManager(config.CertConfig{CertDir: filepath.Join(t.TempDir(), "node-a"), Domain: domain})
	managerB := NewManager(config.CertConfig{CertDir: filepath.Join(t.TempDir(), "node-b"), Domain: domain})
	if err := managerA.loadPEMFromStorage(context.Background(), storage, issuerKey, domain); err != nil {
		t.Fatal(err)
	}
	if err := managerB.loadPEMFromStorage(context.Background(), storage, issuerKey, domain); err != nil {
		t.Fatal(err)
	}
	if string(managerA.TLSCert().CertPEM) != string(certPEM) || string(managerB.TLSCert().CertPEM) != string(certPEM) {
		t.Fatal("both nodes did not load the same shared certificate")
	}
	if string(managerA.TLSCert().KeyPEM) != string(keyPEM) || string(managerB.TLSCert().KeyPEM) != string(keyPEM) {
		t.Fatal("both nodes did not load the same shared private key")
	}
}

func TestSharedACMECertificateRefreshAndNodeRestart(t *testing.T) {
	storage := &certmagic.FileStorage{Path: t.TempDir()}
	issuer := newCountingIssuer(t)
	const domain = "renewed.example.test"
	issuerKey := issuer.IssuerKey()

	magic := newTestMagic(storage, issuer)
	if err := obtainACMECertificate(context.Background(), magic, domain); err != nil {
		t.Fatal(err)
	}
	oldCert, err := storage.Load(context.Background(), certmagic.StorageKeys.SiteCert(issuerKey, domain))
	if err != nil {
		t.Fatal(err)
	}
	oldKey, err := storage.Load(context.Background(), certmagic.StorageKeys.SitePrivateKey(issuerKey, domain))
	if err != nil {
		t.Fatal(err)
	}

	// Simulate CertMagic's atomic renewal result in shared storage. Both
	// managers must reload the new pair, and a fresh manager must recover it.
	newCert, newKey := generateSelfSignedPair(t, domain)
	if err := storage.Store(context.Background(), certmagic.StorageKeys.SiteCert(issuerKey, domain), newCert); err != nil {
		t.Fatal(err)
	}
	if err := storage.Store(context.Background(), certmagic.StorageKeys.SitePrivateKey(issuerKey, domain), newKey); err != nil {
		t.Fatal(err)
	}
	managerA := NewManager(config.CertConfig{Domain: domain})
	managerB := NewManager(config.CertConfig{Domain: domain})
	for _, manager := range []*Manager{managerA, managerB} {
		if err := manager.loadPEMFromStorage(context.Background(), storage, issuerKey, domain); err != nil {
			t.Fatal(err)
		}
		if string(manager.TLSCert().CertPEM) != string(newCert) || string(manager.TLSCert().KeyPEM) != string(newKey) {
			t.Fatal("manager did not load renewed certificate pair")
		}
	}
	restarted := NewManager(config.CertConfig{Domain: domain})
	if err := restarted.loadPEMFromStorage(context.Background(), storage, issuerKey, domain); err != nil {
		t.Fatal(err)
	}
	if string(restarted.TLSCert().CertPEM) != string(newCert) || string(restarted.TLSCert().KeyPEM) != string(newKey) {
		t.Fatal("restarted node did not recover renewed certificate pair")
	}
	if string(oldCert) == string(newCert) || string(oldKey) == string(newKey) {
		t.Fatal("renewal fixture did not replace the stored certificate pair")
	}
}

func TestACMELockSerializesConcurrentNodes(t *testing.T) {
	dir := t.TempDir()
	unlock, err := acquireACMELock(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	acquired := make(chan func(), 1)
	go func() {
		nextUnlock, lockErr := acquireACMELock(context.Background(), dir)
		if lockErr != nil {
			return
		}
		acquired <- nextUnlock
	}()
	select {
	case <-acquired:
		t.Fatal("second node acquired ACME lock while first node held it")
	case <-time.After(100 * time.Millisecond):
	}
	unlock()
	select {
	case nextUnlock := <-acquired:
		nextUnlock()
	case <-time.After(2 * time.Second):
		t.Fatal("second node did not acquire ACME lock after first released it")
	}
}

func TestACMEStorageMigrationFailureCanRecover(t *testing.T) {
	legacyDir := t.TempDir()
	storageDir := filepath.Join(t.TempDir(), "shared")
	symlink := filepath.Join(legacyDir, "unsafe-link")
	if err := os.Symlink(filepath.Join(legacyDir, "missing"), symlink); err != nil {
		t.Fatal(err)
	}
	if err := migrateACMEStorage(legacyDir, storageDir); err == nil {
		t.Fatal("symlink migration unexpectedly succeeded")
	}
	if err := os.Remove(symlink); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyDir, "account.json"), []byte("account"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := migrateACMEStorage(legacyDir, storageDir); err != nil {
		t.Fatalf("migration did not recover after unsafe entry was removed: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(storageDir, "account.json")); err != nil || string(got) != "account" {
		t.Fatalf("migrated account = %q, err=%v", got, err)
	}
}

func TestACMESharedStorageFailureFailsClosed(t *testing.T) {
	storagePath := filepath.Join(t.TempDir(), "shared-acme")
	if err := os.WriteFile(storagePath, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	manager := NewManager(config.CertConfig{
		CertMode:       "http",
		Domain:         "shared.example.test",
		CertDir:        t.TempDir(),
		ACMEStorageDir: storagePath,
	})
	if err := manager.Start(context.Background()); err == nil {
		t.Fatal("ACME start unexpectedly succeeded with a non-directory shared store")
	}
	if manager.HasCert() || manager.magic != nil || manager.acmeStarted {
		t.Fatal("failed ACME startup left certificate state active")
	}
}

func TestACMERetryDelayProblemDetail(t *testing.T) {
	until := time.Now().UTC().Add(8 * time.Hour).Truncate(time.Second)
	err := fmt.Errorf("HTTP 429 urn:ietf:params:acme:error:rateLimited - too many certificates, retry after %s UTC", until.Format("2006-01-02 15:04:05"))
	delay, limited := acmeRetryDelay(err, 0)
	if !limited || delay < 7*time.Hour || delay > 8*time.Hour {
		t.Fatalf("problem detail not honored: %s, %v", delay, limited)
	}
	if delay, limited := acmeRetryDelay(fmt.Errorf("HTTP 429 rateLimited"), 0); !limited || delay < time.Hour {
		t.Fatalf("unsafe fallback: %s, %v", delay, limited)
	}
}

func TestACMECooldownSurvivesManagersAndIsolatesDomains(t *testing.T) {
	dir := t.TempDir()
	calls := 0
	limited := func() error {
		calls++
		return fmt.Errorf("HTTP 429 rateLimited, retry after %s UTC", time.Now().UTC().Add(time.Hour).Format("2006-01-02 15:04:05"))
	}
	if err := obtainACMEWithCooldown(dir, "shared.example.test", limited); err == nil {
		t.Fatal("expected rate limit")
	}
	for i := 0; i < 5; i++ {
		if err := obtainACMEWithCooldown(dir, "SHARED.example.test.", limited); err == nil {
			t.Fatal("expected persistent cooldown")
		}
	}
	if calls != 1 {
		t.Fatalf("issuer called %d times, want one", calls)
	}
	if err := obtainACMEWithCooldown(dir, "other.example.test", func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	path := acmeCooldownPath(dir, "shared.example.test")
	if err := atomicWriteFile(path, []byte(time.Now().Add(-time.Minute).Format(time.RFC3339Nano)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := obtainACMEWithCooldown(dir, "shared.example.test", func() error { calls++; return nil }); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("issuer calls after expiry = %d", calls)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("cooldown marker not cleared: %v", err)
	}
}

func TestSharedCertificateRefreshPropagatesRenewal(t *testing.T) {
	dir := t.TempDir()
	storage := &certmagic.FileStorage{Path: dir}
	const domain = "shared-refresh.example.test"
	const issuer = "test-issuer"
	ctx := context.Background()
	writePair := func(certPEM, keyPEM []byte) {
		t.Helper()
		if err := storage.Store(ctx, certmagic.StorageKeys.SiteCert(issuer, domain), certPEM); err != nil {
			t.Fatal(err)
		}
		if err := storage.Store(ctx, certmagic.StorageKeys.SitePrivateKey(issuer, domain), keyPEM); err != nil {
			t.Fatal(err)
		}
	}
	firstCert, firstKey := generateSelfSignedPair(t, domain)
	writePair(firstCert, firstKey)
	m := NewManager(config.CertConfig{Domain: domain, CertDir: t.TempDir()})
	m.acmeStarted = true
	m.acmeStorage = storage
	m.acmeIssuerKey = issuer
	m.acmeDomain = domain
	if changed, err := m.RefreshSharedCertificate(ctx); err != nil || !changed {
		t.Fatalf("initial refresh: changed=%v err=%v", changed, err)
	}
	if changed, err := m.RefreshSharedCertificate(ctx); err != nil || changed {
		t.Fatalf("unchanged refresh: changed=%v err=%v", changed, err)
	}
	nextCert, nextKey := generateSelfSignedPair(t, domain)
	writePair(nextCert, nextKey)
	if changed, err := m.RefreshSharedCertificate(ctx); err != nil || !changed {
		t.Fatalf("renewal refresh: changed=%v err=%v", changed, err)
	}
	if string(m.TLSCert().CertPEM) != string(nextCert) {
		t.Fatal("renewed material not loaded")
	}
	wrongCert, wrongKey := generateSelfSignedPair(t, "other.example.test")
	writePair(wrongCert, wrongKey)
	if changed, err := m.RefreshSharedCertificate(ctx); err == nil || changed {
		t.Fatalf("wrong domain accepted: changed=%v err=%v", changed, err)
	}
	if string(m.TLSCert().CertPEM) != string(nextCert) {
		t.Fatal("valid material replaced by wrong domain")
	}
}

func TestMigrationDoesNotCopyFlatPEMOrLocks(t *testing.T) {
	legacy, shared := t.TempDir(), t.TempDir()
	for _, name := range []string{"cert.pem", "key.pem", ".acme-retry-old"} {
		if err := os.WriteFile(filepath.Join(legacy, name), []byte("legacy"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(legacy, "locks"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := migrateACMEStorage(legacy, shared); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"cert.pem", "key.pem", ".acme-retry-old", "locks"} {
		if _, err := os.Stat(filepath.Join(shared, name)); !os.IsNotExist(err) {
			t.Fatalf("unexpected shared entry %s: %v", name, err)
		}
	}
}

func TestBackgroundACMERenewalCooldown(t *testing.T) {
	dir := t.TempDir()
	domain := "renewal.example.test"
	requests := 0
	if err := acmeRateLimitEventGuard(dir, "cert_obtaining", map[string]any{"identifier": domain}); err != nil {
		t.Fatal(err)
	}
	requests++
	limit := fmt.Errorf("HTTP 429 urn:ietf:params:acme:error:rateLimited - retry after %s UTC", time.Now().UTC().Add(time.Hour).Format("2006-01-02 15:04:05"))
	if err := acmeRateLimitEventGuard(dir, "cert_failed", map[string]any{"identifier": domain, "renewal": true, "error": limit}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		err := acmeRateLimitEventGuard(dir, "cert_obtaining", map[string]any{"identifier": domain})
		if err == nil {
			requests++
		}
		var noRetry certmagic.ErrNoRetry
		if !errors.As(err, &noRetry) {
			t.Fatalf("renewal cooldown did not suppress retries: %v", err)
		}
	}
	if requests != 1 {
		t.Fatalf("unexpected ACME requests: %d", requests)
	}
	if err := acmeRateLimitEventGuard(dir, "cert_obtaining", map[string]any{"identifier": "unrelated.example.test"}); err != nil {
		t.Fatalf("other domain blocked: %v", err)
	}
	if err := acmeRateLimitEventGuard(dir, "cert_failed", map[string]any{"identifier": domain, "error": fmt.Errorf("unrelated failure")}); err != nil {
		t.Fatal(err)
	}
	if err := acmeRateLimitEventGuard(dir, "cert_obtaining", map[string]any{"identifier": domain}); err == nil {
		t.Fatal("non-rate-limit error must not clear active cooldown")
	}
}

func TestExpiredAndWrongDomainTLSMaterialFailsClosed(t *testing.T) {
	valid, key := generateSelfSignedPair(t, "valid.example.test")
	manager := NewManager(config.CertConfig{Domain: "valid.example.test"})
	manager.mat.Store(&certMaterial{certPEM: valid, keyPEM: key, domain: "valid.example.test"})
	if !manager.HasCert() {
		t.Fatal("valid pair must be available")
	}
	manager.mat.Store(&certMaterial{certPEM: valid, keyPEM: key, domain: "other.example.test"})
	if manager.HasCert() || manager.TLSCert().HasCert() {
		t.Fatal("wrong-domain cert still exposed")
	}
	manager.mat.Store(&certMaterial{certPEM: []byte("expired or invalid"), keyPEM: key, domain: "valid.example.test"})
	if manager.HasCert() || manager.TLSCert().HasCert() {
		t.Fatal("invalid cert still exposed")
	}
}

func TestExpiredTLSMaterialNotExposed(t *testing.T) {
	domain := "expired.example.test"
	original, keyPEM := generateSelfSignedPair(t, domain)
	_ = original
	block, _ := pem.Decode(keyPEM)
	if block == nil {
		t.Fatal("missing PEM key")
	}
	key, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: big.NewInt(42),
		DNSNames:     []string{domain},
		NotBefore:    now.Add(-48 * time.Hour),
		NotAfter:     now.Add(-24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(config.CertConfig{Domain: domain})
	manager.mat.Store(&certMaterial{
		certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		keyPEM:  keyPEM,
		domain:  domain,
	})
	if manager.HasCert() || manager.TLSCert().HasCert() {
		t.Fatal("expired certificate must not be served")
	}
}

func TestACMERenewalRateLimitCannotShortenSharedDeadline(t *testing.T) {
	dir := t.TempDir()
	domain := "do-not-shorten.example.test"
	now := time.Now().UTC()
	long := fmt.Errorf("HTTP 429 rateLimited retry after %s UTC", now.Add(12*time.Hour).Format("2006-01-02 15:04:05"))
	short := fmt.Errorf("HTTP 429 rateLimited retry after %s UTC", now.Add(time.Hour).Format("2006-01-02 15:04:05"))
	for _, err := range []error{long, short} {
		if hookErr := acmeRateLimitEventGuard(dir, "cert_failed", map[string]any{"identifier": domain, "renewal": true, "error": err}); hookErr != nil {
			t.Fatal(hookErr)
		}
	}
	data, err := os.ReadFile(acmeCooldownPath(dir, domain))
	if err != nil {
		t.Fatal(err)
	}
	until, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	if until.Before(now.Add(11 * time.Hour)) {
		t.Fatalf("later 429 shortened active deadline: %s", until)
	}
	// Simulates a new manager after process restart.
	if err := acmeRateLimitEventGuard(dir, "cert_obtaining", map[string]any{"identifier": domain}); err == nil {
		t.Fatal("restarted manager bypassed the shared deadline")
	}
}

func TestACMERenewalCooldownMalformedFileFailsClosed(t *testing.T) {
	dir := t.TempDir()
	domain := "damaged-cooldown.example.test"
	if err := atomicWriteFile(acmeCooldownPath(dir, domain), []byte("not-a-time"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := acmeRateLimitEventGuard(dir, "cert_obtaining", map[string]any{"identifier": domain})
	var noRetry certmagic.ErrNoRetry
	if !errors.As(err, &noRetry) {
		t.Fatalf("invalid cooldown did not fail closed: %v", err)
	}
	calls := 0
	if err := obtainACMEWithCooldown(dir, domain, func() error { calls++; return nil }); err == nil {
		t.Fatal("invalid cooldown unexpectedly allowed direct obtain")
	}
	if calls != 0 {
		t.Fatalf("issuer called despite damaged cooldown: %d", calls)
	}
}

func TestACMERenewalCooldownExpiredAllowsRetry(t *testing.T) {
	dir := t.TempDir()
	domain := "expired-cooldown.example.test"
	if err := atomicWriteFile(acmeCooldownPath(dir, domain), []byte(time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := acmeRateLimitEventGuard(dir, "cert_obtaining", map[string]any{"identifier": domain}); err != nil {
		t.Fatalf("expired cooldown still blocks renewal: %v", err)
	}
}
