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
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/caddyserver/certmagic"

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
		{"http_port", func(c *config.CertConfig) { c.HTTPPort = 8080 }, true},
		{"cert_dir", func(c *config.CertConfig) { c.CertDir = "/other/certs" }, true},
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

func TestReconfigureFailureRestoresOldConfigAndMaterial(t *testing.T) {
	certPEM, keyPEM := generateSelfSignedPair(t, "example.test")
	m := NewManager(config.CertConfig{
		CertMode:    "content",
		CertContent: string(certPEM),
		KeyContent:  string(keyPEM),
	})
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("initial Start: %v", err)
	}
	oldTLS := m.TLSCert()

	// New config fails validation, so Start fails inside Reconfigure.
	changed, err := m.Reconfigure(context.Background(), config.CertConfig{
		CertMode:    "content",
		CertContent: "not-a-certificate",
		KeyContent:  "not-a-key",
	})
	if err == nil {
		t.Fatal("expected Reconfigure to fail on invalid content")
	}
	if changed {
		t.Error("expected changed=false on failed reconfigure")
	}
	if m.cfg.CertMode != "content" || m.cfg.CertContent != string(certPEM) {
		t.Errorf("config was not rolled back: %+v", m.cfg)
	}
	got := m.TLSCert()
	if string(got.CertPEM) != string(oldTLS.CertPEM) || string(got.KeyPEM) != string(oldTLS.KeyPEM) {
		t.Error("old PEM material was not restored after failed reconfigure")
	}
}

func TestReconfigureFailureRestartsOldACMEInstance(t *testing.T) {
	certPEM, keyPEM := generateSelfSignedPair(t, "example.test")
	m := NewManager(config.CertConfig{
		CertMode:    "content",
		CertContent: string(certPEM),
		KeyContent:  string(keyPEM),
	})
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("initial Start: %v", err)
	}
	// Simulate a previously running ACME instance: the tear-down path
	// must run, and the rollback must restart the old config afterwards.
	m.acmeStarted = true
	m.acmeFingerprint = "old-fingerprint"

	// DNS mode with no provider fails before any ACME work starts.
	_, err := m.Reconfigure(context.Background(), config.CertConfig{
		CertMode: "dns",
		Domain:   "example.test",
	})
	if err == nil {
		t.Fatal("expected Reconfigure to fail on missing DNS provider")
	}
	if m.cfg.CertMode != "content" {
		t.Errorf("config was not rolled back, got mode %q", m.cfg.CertMode)
	}
	if !m.HasCert() {
		t.Error("expected old certificate material to survive failed reconfigure")
	}
}

// TestStartSelfSigned_DomainMismatchRegenerates covers the stale-domain
// bug: a persisted self-signed cert for domain A must not be served after
// the configured domain changes to B, even though the key pair is valid.
func TestStartSelfSigned_DomainMismatchRegenerates(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	m1 := NewManager(config.CertConfig{CertMode: "self", Domain: "a.example.com", CertDir: dir})
	if err := m1.Start(ctx); err != nil {
		t.Fatalf("first start: %v", err)
	}
	m2 := NewManager(config.CertConfig{CertMode: "self", Domain: "b.example.com", CertDir: dir})
	if err := m2.Start(ctx); err != nil {
		t.Fatalf("second start: %v", err)
	}
	block, _ := pem.Decode(m2.TLSCert().CertPEM)
	if block == nil {
		t.Fatal("no PEM after regeneration")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse regenerated cert: %v", err)
	}
	for _, name := range cert.DNSNames {
		if name == "a.example.com" {
			t.Fatal("stale domain A cert was reused after domain change")
		}
	}
	found := false
	for _, name := range cert.DNSNames {
		if name == "b.example.com" {
			found = true
		}
	}
	if !found {
		t.Fatalf("regenerated cert does not cover domain B: %v", cert.DNSNames)
	}
}

// TestCheckFileChanged covers file-mode renewal pickup: external tools
// replacing the cert/key files must be noticed, while a mere rewrite of
// identical content must not trigger a reload.
func TestCheckFileChanged(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")
	certA, keyA := generateSelfSignedPair(t, "f.example.com")
	if err := os.WriteFile(certPath, certA, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, keyA, 0o600); err != nil {
		t.Fatal(err)
	}
	m := NewManager(config.CertConfig{CertMode: "file", CertFile: certPath, KeyFile: keyPath, CertDir: t.TempDir()})
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	if m.CheckFileChanged() {
		t.Fatal("unchanged files reported as changed")
	}
	// Same content, new mtime: must not count as a change.
	if err := os.WriteFile(certPath, certA, 0o644); err != nil {
		t.Fatal(err)
	}
	if m.CheckFileChanged() {
		t.Fatal("identical rewrite reported as changed")
	}
	// Genuinely new pair: must be picked up.
	certB, keyB := generateSelfSignedPair(t, "g.example.com")
	if err := os.WriteFile(certPath, certB, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, keyB, 0o600); err != nil {
		t.Fatal(err)
	}
	if !m.CheckFileChanged() {
		t.Fatal("changed files were not detected")
	}
	if string(m.TLSCert().CertPEM) != string(certB) {
		t.Fatal("material was not reloaded to the new pair")
	}
	if m.CheckFileChanged() {
		t.Fatal("second check after reload reported a change")
	}
}
