package main

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

func TestDownloadFileWithChecksum(t *testing.T) {
	data := []byte("fake-binary-content")
	sum := sha256.Sum256(data)
	mux := http.NewServeMux()
	mux.HandleFunc("/xboard-node-linux-amd64", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(data) })
	mux.HandleFunc("/xboard-node-linux-amd64.sha256", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(hex.EncodeToString(sum[:]) + "  xboard-node-linux-amd64\n"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dst := filepath.Join(t.TempDir(), "xboard-node")
	if err := downloadFileWithChecksum(srv.URL+"/xboard-node-linux-amd64", dst); err != nil {
		t.Fatalf("downloadFileWithChecksum: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil || string(got) != string(data) {
		t.Fatalf("dst = %q, err = %v", got, err)
	}
}

func TestDownloadFileWithChecksumMismatchDeletesFile(t *testing.T) {
	data := []byte("fake-binary-content")
	mux := http.NewServeMux()
	mux.HandleFunc("/xboard-node-linux-amd64", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(data) })
	mux.HandleFunc("/xboard-node-linux-amd64.sha256", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("0", 64) + "\n"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dst := filepath.Join(t.TempDir(), "xboard-node")
	if err := downloadFileWithChecksum(srv.URL+"/xboard-node-linux-amd64", dst); err == nil {
		t.Fatal("expected error on checksum mismatch")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatalf("dst should be deleted after mismatch, stat err = %v", err)
	}
}

func TestDownloadFileWithChecksumMissingSidecarWarnsAndKeeps(t *testing.T) {
	data := []byte("fake-binary-content")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".sha256") {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(data)
	}))
	defer srv.Close()

	dst := filepath.Join(t.TempDir(), "xboard-node")
	if err := downloadFileWithChecksum(srv.URL+"/xboard-node-linux-amd64", dst); err != nil {
		t.Fatalf("missing sidecar should warn, not fail: %v", err)
	}
}
