package geodata

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cedar2025/xboard-node/internal/nlog"
)

const (
	singboxGeoIPURL   = "https://github.com/SagerNet/sing-geoip/releases/latest/download/geoip.db"
	singboxGeoSiteURL = "https://github.com/SagerNet/sing-geosite/releases/latest/download/geosite.db"
	xrayGeoIPURL      = "https://github.com/Loyalsoldier/v2ray-rules-dat/releases/latest/download/geoip.dat"
	xrayGeoSiteURL    = "https://github.com/Loyalsoldier/v2ray-rules-dat/releases/latest/download/geosite.dat"
)

var httpClient = &http.Client{Timeout: 10 * time.Minute}

// maxGeoDownloadBytes caps a geo database download. Real geo files are
// tens of MB; an unbounded copy (the old behaviour) let a stalled or
// malicious server fill the disk.
const maxGeoDownloadBytes = 256 << 20 // 256 MiB

var checksumClient = &http.Client{Timeout: 30 * time.Second}

func Ensure(dir string, needGeoIP, needGeoSite bool, kernelType string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create geo_data_dir %q: %w", dir, err)
	}

	type entry struct {
		name string
		url  string
	}

	var files []entry
	if kernelType == "singbox" {
		if needGeoIP {
			files = append(files, entry{"geoip.db", singboxGeoIPURL})
		}
		if needGeoSite {
			files = append(files, entry{"geosite.db", singboxGeoSiteURL})
		}
	} else {
		if needGeoIP {
			files = append(files, entry{"geoip.dat", xrayGeoIPURL})
		}
		if needGeoSite {
			files = append(files, entry{"geosite.dat", xrayGeoSiteURL})
		}
	}

	var firstErr error
	for _, f := range files {
		if err := ensureFile(dir, f.name, f.url); err != nil {
			nlog.Core().Warn("geo database auto-download failed", "file", f.name, "error", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

func ensureFile(dir, name, url string) error {
	dst := filepath.Join(dir, name)
	if fi, err := os.Stat(dst); err == nil {
		// An empty file can only be a truncated/corrupt download, so
		// re-download it instead of treating it as a valid database.
		// (Non-empty pre-existing files are still kept as-is: geo data
		// has no version channel to re-validate against on every start,
		// and re-downloading on every start would hammer upstream.)
		if fi.Size() > 0 {
			return nil
		}
	}
	nlog.Core().Info("geo database missing, downloading automatically", "file", name, "url", url)
	if err := atomicDownload(dst, url); err != nil {
		return err
	}
	if fi, err := os.Stat(dst); err == nil {
		nlog.Core().Info("geo database ready", "file", name, "size_kb", fi.Size()/1024)
	}
	return nil
}

func atomicDownload(dst, url string) error {
	resp, err := httpClient.Get(url)
	if err != nil {
		return fmt.Errorf("http get %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected HTTP status from %s: %s", url, resp.Status)
	}

	// Unique temp file in the destination directory (same filesystem, so
	// the final rename is atomic). The old fixed dst+".tmp" name let
	// concurrent downloads clobber each other and left a predictable
	// path for a local attacker to pre-plant.
	dir := filepath.Dir(dst)
	f, err := os.CreateTemp(dir, ".geo-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmp := f.Name()
	defer os.Remove(tmp)

	// Bounded copy + streaming SHA-256 in one pass.
	h := sha256.New()
	limited := io.LimitReader(resp.Body, maxGeoDownloadBytes+1)
	written, err := io.Copy(io.MultiWriter(f, h), limited)
	if err != nil {
		f.Close()
		return fmt.Errorf("write geo database: %w", err)
	}
	if written > maxGeoDownloadBytes {
		f.Close()
		return fmt.Errorf("geo database from %s exceeds %d bytes", url, maxGeoDownloadBytes)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}

	// Verify against the upstream's published "<file>.sha256" sidecar
	// when one exists. A mismatch is fatal and the temp file is never
	// renamed into place. If no sidecar is published, warn (the file is
	// still installed, as before) — geo upstreams do not all publish
	// sidecars, so failing hard here would break geo downloads outright.
	if err := verifyGeoChecksum(url, h); err != nil {
		return err
	}

	if err := os.Rename(tmp, dst); err != nil {
		return fmt.Errorf("rename to destination: %w", err)
	}
	return nil
}

// verifyGeoChecksum checks the downloaded data against url+".sha256"
// when the sidecar is available. sum is the streaming hash already
// computed during download.
func verifyGeoChecksum(url string, sum interface{ Sum([]byte) []byte }) error {
	resp, err := checksumClient.Get(url + ".sha256")
	if err != nil || resp.StatusCode != http.StatusOK {
		if resp != nil {
			resp.Body.Close()
		}
		nlog.Core().Warn("no SHA-256 sidecar published for geo database; skipping checksum verification", "url", url)
		return nil
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read geo checksum: %w", err)
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return fmt.Errorf("empty geo checksum file for %s", url)
	}
	expected := fields[0]
	actual := hex.EncodeToString(sum.Sum(nil))
	if actual != expected {
		return fmt.Errorf("geo database SHA-256 mismatch for %s: expected %s, got %s", url, expected, actual)
	}
	return nil
}
