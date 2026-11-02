package geodata

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAtomicDownloadVerifiesChecksum(t *testing.T) {
	data := []byte("fake-geo-database-content")
	sum := sha256.Sum256(data)
	mux := http.NewServeMux()
	mux.HandleFunc("/geoip.dat", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(data) })
	mux.HandleFunc("/geoip.dat.sha256", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(hex.EncodeToString(sum[:]) + "  geoip.dat\n"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dir := t.TempDir()
	dst := filepath.Join(dir, "geoip.dat")
	if err := atomicDownload(dst, srv.URL+"/geoip.dat"); err != nil {
		t.Fatalf("atomicDownload: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if string(got) != string(data) {
		t.Fatalf("dst content = %q, want %q", got, data)
	}
}

func TestAtomicDownloadRejectsBadChecksum(t *testing.T) {
	data := []byte("fake-geo-database-content")
	mux := http.NewServeMux()
	mux.HandleFunc("/geoip.dat", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(data) })
	mux.HandleFunc("/geoip.dat.sha256", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("0", 64) + "  geoip.dat\n"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dir := t.TempDir()
	dst := filepath.Join(dir, "geoip.dat")
	if err := atomicDownload(dst, srv.URL+"/geoip.dat"); err == nil {
		t.Fatal("expected error on checksum mismatch")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatalf("dst should not exist after mismatch, stat err = %v", err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
}

func TestAtomicDownloadWithoutSidecarStillInstalls(t *testing.T) {
	data := []byte("fake-geo-database-content")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".sha256") {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(data)
	}))
	defer srv.Close()

	dir := t.TempDir()
	dst := filepath.Join(dir, "geoip.dat")
	if err := atomicDownload(dst, srv.URL+"/geoip.dat"); err != nil {
		t.Fatalf("atomicDownload without sidecar: %v", err)
	}
}
