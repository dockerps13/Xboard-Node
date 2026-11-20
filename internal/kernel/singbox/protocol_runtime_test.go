package singbox

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"testing"
	"time"

	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/kernel"
	"github.com/cedar2025/xboard-node/internal/model"
)

func testSelfSignedTLS(t *testing.T) kernel.TLSCert {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	privateDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return kernel.TLSCert{
		CertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		KeyPEM:  pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: privateDER}),
	}
}

// Smoke-test the embedded core start/stop for each XBoard protocol.
// Config generation alone is insufficient because sing-box can reject a
// syntactically valid JSON inbound at runtime.
func TestAllXBoardProtocolsCanBootEmbeddedSingBox(t *testing.T) {
	cert := testSelfSignedTLS(t)
	cases := []struct {
		protocol string
		tls      int
		version  int
	}{
		{"shadowsocks", 0, 0},
		{"vmess", 0, 0},
		{"trojan", 1, 0},
		{"vless", 0, 0},
		{"hysteria", 1, 2},
		{"tuic", 1, 0},
		{"anytls", 1, 0},
		{"socks", 0, 0},
		{"naive", 1, 0},
		{"http", 0, 0},
		{"mieru", 0, 0},
		{"hysteria2", 1, 0},
	}
	for _, tc := range cases {
		t.Run(tc.protocol, func(t *testing.T) {
			reservation, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			port := reservation.Addr().(*net.TCPAddr).Port
			_ = reservation.Close()
			node := &model.NodeSpec{
				Protocol:   tc.protocol,
				ServerPort: port,
				ListenIP:   "127.0.0.1",
				Cipher:     "aes-128-gcm",
				TLS:        tc.tls,
				Version:    tc.version,
			}
			server := New(config.KernelConfig{Type: "singbox", LogLevel: "error", ConfigDir: t.TempDir()})
			users := []model.UserSpec{{ID: 1, UUID: "aaaaaaaa-1111-2222-3333-444444444444"}}
			if err := server.Start(node, users, cert); err != nil {
				t.Fatalf("embedded sing-box failed to start protocol %s: %v", tc.protocol, err)
			}
			server.Stop()
		})
	}
}
