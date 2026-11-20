package cert

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
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
