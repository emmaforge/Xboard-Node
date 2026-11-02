package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cedar2025/xboard-node/internal/config"
)

func TestParsePositiveIntStrict(t *testing.T) {
	if got, err := parsePositiveInt("42", "node-id"); err != nil || got != 42 {
		t.Fatalf("parsePositiveInt(42) = %d, %v", got, err)
	}
	// Sscanf("%d") used to accept these; strict parsing must reject them.
	for _, bad := range []string{"12abc", "12.5", " 12", "12 ", "", "0", "-3", "abc"} {
		if got, err := parsePositiveInt(bad, "node-id"); err == nil {
			t.Fatalf("parsePositiveInt(%q) = %d, want error", bad, got)
		}
	}
}

func TestValidateBindArgs(t *testing.T) {
	base := []string{"--panel-url", "https://panel.example.com", "--token", "secret", "--node-id", "7", "--kernel", "singbox"}
	if err := validateBindArgs("node", base); err != nil {
		t.Fatalf("valid node bind rejected: %v", err)
	}
	cases := []struct {
		name string
		mode string
		args []string
	}{
		{"empty token", "node", []string{"--panel-url", "https://panel.example.com", "--token", "", "--node-id", "7"}},
		{"missing token", "node", []string{"--panel-url", "https://panel.example.com", "--node-id", "7"}},
		{"missing token value", "node", []string{"--panel-url", "https://panel.example.com", "--token"}},
		{"bad kernel", "node", []string{"--panel-url", "https://panel.example.com", "--token", "s", "--node-id", "7", "--kernel", "v2ray"}},
		{"bad mode", "bogus", []string{"--panel-url", "https://panel.example.com", "--token", "s", "--node-id", "7"}},
		{"missing node-id", "node", []string{"--panel-url", "https://panel.example.com", "--token", "s"}},
		{"trailing node-id", "machine", []string{"--panel-url", "https://panel.example.com", "--token", "s", "--machine-id", "12abc"}},
		{"unknown flag", "node", []string{"--panel-url", "https://panel.example.com", "--token", "s", "--node-id", "7", "--bogus", "x"}},
		{"missing panel", "node", []string{"--token", "s", "--node-id", "7"}},
	}
	for _, tc := range cases {
		if err := validateBindArgs(tc.mode, tc.args); err == nil {
			t.Fatalf("%s: expected error, got nil", tc.name)
		}
	}
	if err := validateBindArgs("machine", []string{"--panel-url", "https://panel.example.com", "--token", "s", "--machine-id", "3", "--kernel", "sing-box"}); err != nil {
		t.Fatalf("sing-box kernel spelling should validate: %v", err)
	}
}

func TestDownloadHTTPStatus(t *testing.T) {
	if got := downloadHTTPStatus(fmt.Errorf("HTTP %d from %s", 404, "https://example.com/x")); got != 404 {
		t.Fatalf("404 error -> %d", got)
	}
	if got := downloadHTTPStatus(errors.New("connection refused")); got != 0 {
		t.Fatalf("network error -> %d, want 0", got)
	}
	if got := downloadHTTPStatus(nil); got != 0 {
		t.Fatalf("nil error -> %d, want 0", got)
	}
}

func TestWriteRootConfigStandaloneRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	root := &config.RootConfig{
		Instances: []config.Config{{
			Panel: config.PanelConfig{URL: "https://panel.example.com", NodeID: 7},
			Kernel: config.KernelConfig{
				Type:      "singbox",
				ConfigDir: filepath.Join(dir, "instances", "x"),
				LogLevel:  "warn",
			},
			Log: config.LogConfig{Level: "info", Output: "stdout"},
			Standalone: &config.StandaloneConfig{
				Enabled: true,
				Node: config.StandaloneNodeConfig{
					Protocol:   "vless",
					ServerPort: 8443,
				},
				Users: []config.StandaloneUser{{ID: 1, UUID: "11111111-1111-1111-1111-111111111111"}},
			},
		}},
	}
	if err := writeRootConfig(path, root); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadWritableRootConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	instances := normalizeRootInstances(loaded)
	if len(instances) != 1 || !instances[0].IsStandalone() {
		t.Fatalf("standalone lost on rewrite: %+v", instances)
	}
	if instances[0].Standalone.Node.Protocol != "vless" || instances[0].Standalone.Node.ServerPort != 8443 {
		t.Fatalf("standalone node mismatch: %+v", instances[0].Standalone.Node)
	}
	if len(instances[0].Standalone.Users) != 1 || instances[0].Standalone.Users[0].UUID != "11111111-1111-1111-1111-111111111111" {
		t.Fatalf("standalone users mismatch: %+v", instances[0].Standalone.Users)
	}
}

func TestWriteRootConfigPreservesSharedAndInstanceFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	root := &config.RootConfig{}
	root.Config.Kernel = config.KernelConfig{
		Type:         "singbox",
		ConfigDir:    filepath.Join(dir, "top"),
		LogLevel:     "warn",
		GeoDataDir:   filepath.Join(dir, "geo"),
		CustomConfig: "/etc/xboard-node/custom.json",
		CustomRoute:  []map[string]any{{"rule": "x"}},
	}
	root.Config.Cert = config.CertConfig{
		Email:       "admin@example.com",
		DNSProvider: "cloudflare",
		DNSEnv:      map[string]string{"CF_API_TOKEN": "tok"},
		HTTPPort:    8080,
		CertDir:     filepath.Join(dir, "certs"),
		CertContent: "CERTPEM",
		KeyContent:  "KEYPEM",
	}
	root.Config.WS = config.WSConfig{
		StatusInterval:    10,
		BackoffMax:        120,
		DiscoveryInterval: 300,
	}
	root.Config.HealthPort = 65530
	root.Instances = []config.Config{{
		Panel: config.PanelConfig{URL: "https://panel.example.com", NodeID: 7},
		Kernel: config.KernelConfig{
			Type:      "singbox",
			ConfigDir: filepath.Join(dir, "inst"),
			LogLevel:  "warn",
		},
		Log: config.LogConfig{Level: "info", Output: "stdout"},
		// Cert carrying ONLY an email: the old gate (mode/domain/
		// file/auto_tls) dropped this block entirely.
		Cert: config.CertConfig{Email: "node@example.com"},
		WS:   config.WSConfig{BackoffMax: 90, DiscoveryInterval: 200},
	}}
	if err := writeRootConfig(path, root); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadWritableRootConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Kernel.CustomConfig != "/etc/xboard-node/custom.json" {
		t.Fatalf("top-level kernel.custom_config lost: %q", loaded.Kernel.CustomConfig)
	}
	if loaded.Kernel.GeoDataDir != filepath.Join(dir, "geo") || len(loaded.Kernel.CustomRoute) != 1 {
		t.Fatalf("top-level kernel fields lost: %+v", loaded.Kernel)
	}
	if loaded.Cert.Email != "admin@example.com" || loaded.Cert.DNSProvider != "cloudflare" ||
		loaded.Cert.DNSEnv["CF_API_TOKEN"] != "tok" || loaded.Cert.HTTPPort != 8080 ||
		loaded.Cert.CertDir != filepath.Join(dir, "certs") ||
		loaded.Cert.CertContent != "CERTPEM" || loaded.Cert.KeyContent != "KEYPEM" {
		t.Fatalf("top-level cert fields lost: %+v", loaded.Cert)
	}
	if loaded.WS.BackoffMax != 120 || loaded.WS.DiscoveryInterval != 300 {
		t.Fatalf("top-level WS fields lost: %+v", loaded.WS)
	}
	if loaded.HealthPort != 65530 {
		t.Fatalf("top-level health_port lost: %d", loaded.HealthPort)
	}
	instances := normalizeRootInstances(loaded)
	if len(instances) != 1 {
		t.Fatalf("instances = %d, want 1", len(instances))
	}
	if instances[0].Cert.Email != "node@example.com" {
		t.Fatalf("instance cert (email-only) lost: %+v", instances[0].Cert)
	}
	if instances[0].WS.BackoffMax != 90 || instances[0].WS.DiscoveryInterval != 200 {
		t.Fatalf("instance WS fields lost: %+v", instances[0].WS)
	}
}

func TestWriteRootConfigEmptyInstancesWritesNoBinding(t *testing.T) {
	// Mechanism behind removeBinding's last-binding path: a root with
	// no instances and no legacy panel must not resurrect a binding.
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	root := &config.RootConfig{}
	root.Config.Log = config.LogConfig{Level: "info", Output: "stdout"}
	if err := writeRootConfig(path, root); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadWritableRootConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := normalizeRootInstances(loaded); len(got) != 0 {
		t.Fatalf("expected no instances, got %+v", got)
	}
	// And the legacy path still works when a legacy panel IS present.
	legacy := &config.RootConfig{}
	legacy.Config.Panel = config.PanelConfig{URL: "https://panel.example.com", NodeID: 7}
	legacy.Config.Kernel = config.KernelConfig{Type: "singbox", ConfigDir: filepath.Join(dir, "legacy")}
	if err := writeRootConfig(path, legacy); err != nil {
		t.Fatal(err)
	}
	loaded, err = loadWritableRootConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := normalizeRootInstances(loaded); len(got) != 1 {
		t.Fatalf("legacy single config should still round-trip as one instance, got %d", len(got))
	}
}

func TestRunConfigInitRebindPreservesCustomisation(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yml")
	credPath := filepath.Join(dir, "credentials.env")
	metaPath := filepath.Join(dir, "install-meta.json")
	instDir := filepath.Join(dir, "instances", "inst")
	root := &config.RootConfig{
		Instances: []config.Config{{
			Panel: config.PanelConfig{URL: "https://panel.example.com", NodeID: 7, TokenEnv: "INSTANCE_X_API_KEY"},
			Kernel: config.KernelConfig{
				Type:         "singbox",
				ConfigDir:    instDir,
				LogLevel:     "error",
				CustomConfig: "/etc/xboard-node/custom.json",
				CustomRoute:  []map[string]any{{"rule": "keepme"}},
			},
			Log:        config.LogConfig{Level: "debug", Output: "stdout"},
			HealthPort: 65531,
			Node:       config.NodeConfig{PushInterval: 11, PullInterval: 22, TrackInterval: 33, DeviceReportInterval: 44},
			WS:         config.WSConfig{StatusInterval: 15, BackoffMax: 77},
			Cert:       config.CertConfig{Email: "node@example.com", Domain: "node.example.com"},
		}},
	}
	if err := writeRootConfig(cfgPath, root); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(credPath, []byte("INSTANCE_X_API_KEY=oldtoken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{
		"--mode", "node",
		"--panel-url", "https://panel.example.com",
		"--node-id", "7",
		"--token", "newtoken",
		"--kernel", "singbox",
		"--config", cfgPath,
		"--output", cfgPath,
		"--credentials-in", credPath,
		"--credentials-out", credPath,
		"--meta", metaPath,
		"--install-root", dir,
	}
	if err := runConfigInit(args); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadWritableRootConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	instances := normalizeRootInstances(loaded)
	if len(instances) != 1 {
		t.Fatalf("instances = %d, want 1", len(instances))
	}
	inst := instances[0]
	if inst.HealthPort != 65531 {
		t.Fatalf("health_port not preserved: %d", inst.HealthPort)
	}
	if inst.Kernel.ConfigDir != instDir {
		t.Fatalf("config_dir not preserved: %q", inst.Kernel.ConfigDir)
	}
	if inst.Kernel.CustomConfig != "/etc/xboard-node/custom.json" || len(inst.Kernel.CustomRoute) != 1 {
		t.Fatalf("custom_config/custom_route not preserved: %+v", inst.Kernel)
	}
	if inst.Kernel.LogLevel != "error" {
		t.Fatalf("kernel log level not preserved: %q", inst.Kernel.LogLevel)
	}
	if inst.Log.Level != "debug" {
		t.Fatalf("log level not preserved: %q", inst.Log.Level)
	}
	if inst.Node.PushInterval != 11 || inst.Node.PullInterval != 22 || inst.Node.TrackInterval != 33 || inst.Node.DeviceReportInterval != 44 {
		t.Fatalf("node intervals not preserved: %+v", inst.Node)
	}
	if inst.WS.StatusInterval != 15 || inst.WS.BackoffMax != 77 {
		t.Fatalf("WS intervals not preserved: %+v", inst.WS)
	}
	if inst.Cert.Email != "node@example.com" || inst.Cert.Domain != "node.example.com" {
		t.Fatalf("cert not preserved: %+v", inst.Cert)
	}
	cred, err := os.ReadFile(credPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cred), "newtoken") {
		t.Fatalf("new token not written to credentials: %s", cred)
	}
}

func TestWriteInstallMetaLatestIsLast(t *testing.T) {
	dir := t.TempDir()
	metaPath := filepath.Join(dir, "install-meta.json")
	root := &config.RootConfig{
		Instances: []config.Config{
			{Panel: config.PanelConfig{URL: "https://panel.example.com", NodeID: 7}},
			{Panel: config.PanelConfig{URL: "https://panel.example.com", NodeID: 8}},
		},
	}
	if err := writeInstallMetaVersioned(metaPath, root, "test", ""); err != nil {
		t.Fatal(err)
	}
	meta, err := loadInstallMeta(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(meta.Instances) != 2 {
		t.Fatalf("instances = %d, want 2", len(meta.Instances))
	}
	if meta.LatestInstanceID != meta.Instances[1].ID {
		t.Fatalf("LatestInstanceID = %q, want last instance %q (first is %q)",
			meta.LatestInstanceID, meta.Instances[1].ID, meta.Instances[0].ID)
	}
	// An explicit latest ID is still honoured.
	if err := writeInstallMetaVersioned(metaPath, root, "test", "explicit-id"); err != nil {
		t.Fatal(err)
	}
	meta, err = loadInstallMeta(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	if meta.LatestInstanceID != "explicit-id" {
		t.Fatalf("explicit LatestInstanceID overwritten: %q", meta.LatestInstanceID)
	}
}

func TestRunConfigInitFlagErrors(t *testing.T) {
	if err := runConfigInit([]string{"--mode"}); err == nil || !strings.Contains(err.Error(), "missing value") {
		t.Fatalf("trailing flag should error with missing value, got %v", err)
	}
	if err := runConfigInit([]string{"--bogus", "x", "--mode", "node"}); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("unknown flag should error, got %v", err)
	}
}
