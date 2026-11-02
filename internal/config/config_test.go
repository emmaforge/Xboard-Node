package config

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cedar2025/xboard-node/internal/nlog"
)

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoad_ValidConfig(t *testing.T) {
	path := writeTemp(t, `
panel:
  url: "https://panel.example.com"
  token: "secret-token"
  node_id: 5
  node_type: "vmess"
kernel:
  type: singbox
  log_level: warn
log:
  level: debug
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Panel.URL != "https://panel.example.com" {
		t.Errorf("url: got %q", cfg.Panel.URL)
	}
	if cfg.Panel.Token != "secret-token" {
		t.Errorf("token: got %q", cfg.Panel.Token)
	}
	if cfg.Panel.NodeID != 5 {
		t.Errorf("node_id: got %d", cfg.Panel.NodeID)
	}
	if cfg.Panel.NodeType != "vmess" {
		t.Errorf("node_type: got %q", cfg.Panel.NodeType)
	}
	if cfg.Kernel.Type != "singbox" {
		t.Errorf("kernel.type: got %q", cfg.Kernel.Type)
	}
	if cfg.Log.Level != "debug" {
		t.Errorf("log.level: got %q", cfg.Log.Level)
	}
}

func TestLoad_Defaults(t *testing.T) {
	path := writeTemp(t, `
panel:
  url: "https://panel.example.com"
  token: "tok"
  node_id: 1
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Kernel.Type != "singbox" {
		t.Errorf("default kernel.type: got %q, want singbox", cfg.Kernel.Type)
	}
	// config_dir should default to the directory containing the config file.
	expectedDir := filepath.Dir(path)
	if cfg.Kernel.ConfigDir != expectedDir {
		t.Errorf("default config_dir: got %q, want %q", cfg.Kernel.ConfigDir, expectedDir)
	}
	if cfg.Kernel.LogLevel != "warn" {
		t.Errorf("default kernel log_level: got %q, want warn", cfg.Kernel.LogLevel)
	}
	if cfg.Log.Level != "info" {
		t.Errorf("default log.level: got %q, want info", cfg.Log.Level)
	}
	if cfg.Log.Output != "stdout" {
		t.Errorf("default log.output: got %q, want stdout", cfg.Log.Output)
	}
	if cfg.Cert.HTTPPort != 80 {
		t.Errorf("default http_port: got %d, want 80", cfg.Cert.HTTPPort)
	}
	expectedCertDir := filepath.Join(expectedDir, "certs")
	if cfg.Cert.CertDir != expectedCertDir {
		t.Errorf("default cert_dir: got %q, want %q", cfg.Cert.CertDir, expectedCertDir)
	}
}

func TestLoad_MissingURL(t *testing.T) {
	path := writeTemp(t, `
panel:
  token: "tok"
  node_id: 1
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for missing url")
	}
}

func TestLoad_MissingToken(t *testing.T) {
	path := writeTemp(t, `
panel:
  url: "https://example.com"
  node_id: 1
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for missing token")
	}
}

func TestLoad_InvalidNodeID(t *testing.T) {
	path := writeTemp(t, `
panel:
  url: "https://example.com"
  token: "tok"
  node_id: 0
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for node_id=0")
	}
}

func TestLoad_NegativeNodeID(t *testing.T) {
	path := writeTemp(t, `
panel:
  url: "https://example.com"
  token: "tok"
  node_id: -1
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for negative node_id")
	}
}

func TestLoad_InvalidKernelType(t *testing.T) {
	path := writeTemp(t, `
panel:
  url: "https://example.com"
  token: "tok"
  node_id: 1
kernel:
  type: "invalid"
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for invalid kernel type")
	}
}

func TestLoad_XrayKernel(t *testing.T) {
	path := writeTemp(t, `
panel:
  url: "https://example.com"
  token: "tok"
  node_id: 1
kernel:
  type: xray
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Kernel.Type != "xray" {
		t.Errorf("kernel.type: got %q, want xray", cfg.Kernel.Type)
	}
}

func TestLoad_AutoTLS_NoDomain(t *testing.T) {
	path := writeTemp(t, `
panel:
  url: "https://example.com"
  token: "tok"
  node_id: 1
cert:
  auto_tls: true
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for auto_tls without domain")
	}
}

func TestLoad_AutoTLS_WithDomain(t *testing.T) {
	path := writeTemp(t, `
panel:
  url: "https://example.com"
  token: "tok"
  node_id: 1
cert:
  auto_tls: true
  domain: "node.example.com"
  email: "admin@example.com"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Cert.AutoTLS {
		t.Error("auto_tls should be true")
	}
	if cfg.Cert.Domain != "node.example.com" {
		t.Errorf("domain: got %q", cfg.Cert.Domain)
	}
}

func TestLoad_FileNotFound(t *testing.T) {
	_, err := Load("/nonexistent/path/config.yaml")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestLoad_InvalidYAML(t *testing.T) {
	path := writeTemp(t, "{{{{invalid yaml}}}")
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for invalid YAML")
	}
}

func TestLoad_CustomCert(t *testing.T) {
	path := writeTemp(t, `
panel:
  url: "https://example.com"
  token: "tok"
  node_id: 1
cert:
  cert_file: "/custom/cert.pem"
  key_file: "/custom/key.pem"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Cert.CertFile != "/custom/cert.pem" {
		t.Errorf("cert_file: got %q", cfg.Cert.CertFile)
	}
	if cfg.Cert.KeyFile != "/custom/key.pem" {
		t.Errorf("key_file: got %q", cfg.Cert.KeyFile)
	}
}

func TestLoad_CustomIntervals(t *testing.T) {
	path := writeTemp(t, `
panel:
  url: "https://example.com"
  token: "tok"
  node_id: 1
node:
  push_interval: 30
  pull_interval: 60
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Node.PushInterval != 30 {
		t.Errorf("push_interval: got %d", cfg.Node.PushInterval)
	}
	if cfg.Node.PullInterval != 60 {
		t.Errorf("pull_interval: got %d", cfg.Node.PullInterval)
	}
}

func TestLoad_StandaloneConfig(t *testing.T) {
	path := writeTemp(t, `
standalone:
  enabled: true
  node:
    protocol: "vless"
    server_port: 8443
    network: "ws"
    tls: 1
    network_settings:
      path: "/ws"
  users:
    - id: 1
      uuid: "11111111-1111-1111-1111-111111111111"
kernel:
  type: xray
cert:
  cert_mode: none
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.IsStandalone() {
		t.Fatal("expected standalone mode to be enabled")
	}
	if cfg.Standalone.Node.Protocol != "vless" {
		t.Errorf("standalone.node.protocol: got %q", cfg.Standalone.Node.Protocol)
	}
	if cfg.Standalone.Node.ServerPort != 8443 {
		t.Errorf("standalone.node.server_port: got %d", cfg.Standalone.Node.ServerPort)
	}
	if len(cfg.Standalone.Users) != 1 {
		t.Fatalf("standalone.users: got %d", len(cfg.Standalone.Users))
	}
	if cfg.Standalone.Users[0].UUID != "11111111-1111-1111-1111-111111111111" {
		t.Errorf("standalone.users[0].uuid: got %q", cfg.Standalone.Users[0].UUID)
	}
}

func TestLoad_StandaloneRequiresUsers(t *testing.T) {
	path := writeTemp(t, `
standalone:
  enabled: true
  node:
    protocol: "trojan"
    server_port: 443
kernel:
  type: singbox
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for standalone mode without users")
	}
}

func TestLoadRoot_LegacyConfigNormalizesToSingleInstance(t *testing.T) {
	path := writeTemp(t, `
panel:
  url: "https://panel.example.com"
  token: "tok"
  node_id: 1
`)
	root, err := LoadRoot(path)
	if err != nil {
		t.Fatalf("LoadRoot: %v", err)
	}
	instances, err := root.NormalizeInstances()
	if err != nil {
		t.Fatalf("NormalizeInstances: %v", err)
	}
	if len(instances) != 1 {
		t.Fatalf("instances: got %d, want 1", len(instances))
	}
	if instances[0].Panel.URL != "https://panel.example.com" {
		t.Errorf("instance url: got %q", instances[0].Panel.URL)
	}
	if instances[0].InstanceID == "" {
		t.Fatal("expected auto instance id")
	}
}

func TestLoadRoot_InstancesConfig(t *testing.T) {
	path := writeTemp(t, `
instances:
  - panel:
      url: "https://panel-a.example.com"
      token_env: "PANEL_A_TOKEN"
      node_id: 1
    kernel:
      type: singbox
  - panel:
      url: "https://panel-b.example.com"
    machine:
      machine_id: 2
      token_env: "PANEL_B_MACHINE_TOKEN"
    kernel:
      type: singbox
`)
	t.Setenv("PANEL_A_TOKEN", "token-a")
	t.Setenv("PANEL_B_MACHINE_TOKEN", "token-b")
	root, err := LoadRoot(path)
	if err != nil {
		t.Fatalf("LoadRoot: %v", err)
	}
	instances, err := root.NormalizeInstances()
	if err != nil {
		t.Fatalf("NormalizeInstances: %v", err)
	}
	if len(instances) != 2 {
		t.Fatalf("instances: got %d, want 2", len(instances))
	}
	if instances[0].Panel.Token != "token-a" {
		t.Errorf("panel a token: got %q", instances[0].Panel.Token)
	}
	if instances[1].Machine == nil || instances[1].Machine.Token != "token-b" {
		t.Fatalf("machine token: got %+v", instances[1].Machine)
	}
	if instances[0].InstanceID == instances[1].InstanceID {
		t.Fatal("expected unique instance ids")
	}
}

func TestConfig_AutoInstanceIDStable(t *testing.T) {
	cfg := &Config{Panel: PanelConfig{URL: "https://Panel.Example.com/", NodeID: 1, Token: "tok"}}
	cfg.setDefaultsFrom("/etc/xboard-node")
	id1, err := cfg.AutoInstanceID()
	if err != nil {
		t.Fatalf("AutoInstanceID: %v", err)
	}
	id2, err := cfg.AutoInstanceID()
	if err != nil {
		t.Fatalf("AutoInstanceID second: %v", err)
	}
	if id1 != id2 {
		t.Fatalf("ids differ: %q vs %q", id1, id2)
	}
}

func TestLoadRoot_InheritanceFromTopLevel(t *testing.T) {
	path := writeTemp(t, `
log:
  level: "debug"
  output: "stderr"
kernel:
  type: xray
  log_level: "error"
node:
  push_interval: 42
  pull_interval: 99
instances:
  - panel:
      url: "https://panel.example.com"
      token: "tok-a"
      node_id: 1
  - panel:
      url: "https://panel.example.com"
      token: "tok-b"
      node_id: 2
    log:
      level: "warn"
`)
	root, err := LoadRoot(path)
	if err != nil {
		t.Fatalf("LoadRoot: %v", err)
	}
	if len(root.Instances) != 2 {
		t.Fatalf("instances: got %d, want 2", len(root.Instances))
	}
	// Instance 0: inherits everything from top-level.
	inst0 := root.Instances[0]
	if inst0.Log.Level != "debug" {
		t.Errorf("inst0 log.level: got %q, want %q", inst0.Log.Level, "debug")
	}
	if inst0.Log.Output != "stderr" {
		t.Errorf("inst0 log.output: got %q, want %q", inst0.Log.Output, "stderr")
	}
	if inst0.Kernel.Type != "xray" {
		t.Errorf("inst0 kernel.type: got %q, want %q", inst0.Kernel.Type, "xray")
	}
	if inst0.Kernel.LogLevel != "error" {
		t.Errorf("inst0 kernel.log_level: got %q, want %q", inst0.Kernel.LogLevel, "error")
	}
	if inst0.Node.PushInterval != 42 {
		t.Errorf("inst0 push_interval: got %d, want 42", inst0.Node.PushInterval)
	}
	if inst0.Node.PullInterval != 99 {
		t.Errorf("inst0 pull_interval: got %d, want 99", inst0.Node.PullInterval)
	}
	// Instance 1: overrides log.level, inherits the rest.
	inst1 := root.Instances[1]
	if inst1.Log.Level != "warn" {
		t.Errorf("inst1 log.level: got %q, want %q", inst1.Log.Level, "warn")
	}
	if inst1.Log.Output != "stderr" {
		t.Errorf("inst1 log.output: got %q, want %q", inst1.Log.Output, "stderr")
	}
	if inst1.Kernel.Type != "xray" {
		t.Errorf("inst1 kernel.type: got %q, want %q", inst1.Kernel.Type, "xray")
	}
}

func TestLoadRoot_InstanceOrderDoesNotAffectConfigDir(t *testing.T) {
	yamlTmpl := func(first, second int) string {
		return fmt.Sprintf(`
instances:
  - panel:
      url: "https://panel.example.com"
      token: "tok-a"
      node_id: %d
  - panel:
      url: "https://panel.example.com"
      token: "tok-b"
      node_id: %d
`, first, second)
	}

	path1 := writeTemp(t, yamlTmpl(1, 2))
	path2 := writeTemp(t, yamlTmpl(2, 1))
	root1, err := LoadRoot(path1)
	if err != nil {
		t.Fatalf("LoadRoot order1: %v", err)
	}
	root2, err := LoadRoot(path2)
	if err != nil {
		t.Fatalf("LoadRoot order2: %v", err)
	}

	// Collect the config_dir suffix (relative to base) by InstanceID.
	suffix := func(instances []Config) map[string]string {
		m := map[string]string{}
		for _, inst := range instances {
			// The config_dir ends with .../{instanceID}, extract last component.
			m[inst.InstanceID] = filepath.Base(inst.Kernel.ConfigDir)
		}
		return m
	}
	dirs1 := suffix(root1.Instances)
	dirs2 := suffix(root2.Instances)

	for id, s1 := range dirs1 {
		s2, ok := dirs2[id]
		if !ok {
			t.Fatalf("instance %q missing in reversed order", id)
		}
		if s1 != s2 {
			t.Errorf("instance %q config_dir suffix differs: %q vs %q", id, s1, s2)
		}
	}
}

func TestInheritFrom_AutoTLSNotForcedWhenChildHasCertMode(t *testing.T) {
	parent := &Config{Cert: CertConfig{AutoTLS: true, Domain: "example.com"}}
	child := &Config{Cert: CertConfig{CertMode: "file", CertFile: "/etc/cert.pem"}}
	child.inheritFrom(parent)
	if child.Cert.AutoTLS {
		t.Error("auto_tls should NOT be inherited when child has explicit cert_mode")
	}
	if child.Cert.Domain != "example.com" {
		t.Errorf("domain: got %q, want %q", child.Cert.Domain, "example.com")
	}
}

func TestInheritFrom_AutoTLSInheritedWhenChildHasNoCertConfig(t *testing.T) {
	parent := &Config{Cert: CertConfig{AutoTLS: true, Domain: "example.com"}}
	child := &Config{}
	child.inheritFrom(parent)
	if !child.Cert.AutoTLS {
		t.Error("auto_tls should be inherited when child has no cert config")
	}
}

func TestLoad_NegativeIntervalsRejected(t *testing.T) {
	// Regression test for B8: negative intervals used to pass
	// validation and panic at time.NewTicker during startup.
	cases := []struct {
		name string
		yaml string
	}{
		{"track_interval", "node:\n  track_interval: -1\n"},
		{"device_report_interval", "node:\n  device_report_interval: -5\n"},
		{"status_interval", "ws:\n  status_interval: -1\n"},
		{"backoff_initial", "ws:\n  backoff_initial: -1\n"},
		{"backoff_max", "ws:\n  backoff_max: -1\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTemp(t, "\npanel:\n  url: \"https://panel.example.com\"\n  token: \"x\"\n  node_id: 5\n  node_type: \"vmess\"\nkernel:\n  type: singbox\n"+tc.yaml)
			if _, err := Load(path); err == nil {
				t.Fatalf("Load should reject negative %s", tc.name)
			}
		})
	}
}

// TestLoad_WarnsOnInsecurePanelURL (B7): loading a config whose panel
// URL is plain http must log a cleartext-token warning, while an https
// panel URL must not. This is a warning only — http configs still load.
func TestLoad_WarnsOnInsecurePanelURL(t *testing.T) {
	var buf bytes.Buffer
	nlog.Init(&buf, slog.LevelDebug, false)
	defer nlog.Init(os.Stdout, slog.LevelInfo, false)

	httpPath := writeTemp(t, `
panel:
  url: "http://panel.example.com"
  token: "secret-token"
  node_id: 5
kernel:
  type: singbox
`)
	if _, err := Load(httpPath); err != nil {
		t.Fatalf("Load http config: %v", err)
	}
	if !strings.Contains(buf.String(), "plain http") {
		t.Errorf("expected a plain-http warning in log output, got:\n%s", buf.String())
	}
	if strings.Contains(buf.String(), "secret-token") {
		t.Errorf("warning output must not contain the token:\n%s", buf.String())
	}

	buf.Reset()
	httpsPath := writeTemp(t, `
panel:
  url: "https://panel.example.com"
  token: "secret-token"
  node_id: 5
kernel:
  type: singbox
`)
	if _, err := Load(httpsPath); err != nil {
		t.Fatalf("Load https config: %v", err)
	}
	if strings.Contains(buf.String(), "plain http") {
		t.Errorf("https config must not warn about plain http, got:\n%s", buf.String())
	}
}

// --- Regression tests for the config-layer fix batch (dc97373 baseline) ---

func TestLoad_NegativeWSIntervalsRejected(t *testing.T) {
	// Item 1: same B8 rule as the other intervals — negative rejects,
	// zero falls through to the defaults (300 / 15), not an error.
	for _, tc := range []struct {
		name string
		yaml string
	}{
		{"discovery_interval", "ws:\n  discovery_interval: -1\n"},
		{"handshake_timeout", "ws:\n  handshake_timeout: -5\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTemp(t, "\npanel:\n  url: \"https://panel.example.com\"\n  token: \"x\"\n  node_id: 5\nkernel:\n  type: singbox\n"+tc.yaml)
			if _, err := Load(path); err == nil {
				t.Fatalf("Load should reject negative %s", tc.name)
			}
		})
	}

	zeroPath := writeTemp(t, `
panel:
  url: "https://panel.example.com"
  token: "x"
  node_id: 5
kernel:
  type: singbox
ws:
  discovery_interval: 0
  handshake_timeout: 0
`)
	cfg, err := Load(zeroPath)
	if err != nil {
		t.Fatalf("Load with zero ws intervals: %v", err)
	}
	if cfg.WS.DiscoveryInterval != 300 {
		t.Errorf("discovery_interval default: got %d, want 300", cfg.WS.DiscoveryInterval)
	}
	if cfg.WS.HandshakeTimeout != 15 {
		t.Errorf("handshake_timeout default: got %d, want 15", cfg.WS.HandshakeTimeout)
	}
}

func TestExpandNodes_CertFieldMerge(t *testing.T) {
	// Item 2: a node cert block that only sets domain must keep the
	// global email / dns_provider / dns_env / http_port, and merging
	// must not mutate the global config's DNSEnv map.
	global := CertConfig{
		Domain:      "global.example.com",
		Email:       "admin@example.com",
		DNSProvider: "cloudflare",
		DNSEnv:      map[string]string{"CF_API_TOKEN": "global-token", "OTHER": "kept"},
		HTTPPort:    80,
		CertMode:    "dns",
		CertDir:     "/base/certs",
	}
	cfg := &Config{
		Panel:  PanelConfig{URL: "https://panel.example.com", Token: "tok"},
		Kernel: KernelConfig{ConfigDir: "/base", GeoDataDir: "/base"},
		Cert:   global,
		Nodes: []NodeEntry{
			{NodeID: 1, Cert: &CertConfig{
				Domain: "node1.example.com",
				DNSEnv: map[string]string{"CF_API_TOKEN": "node-token"},
			}},
			{NodeID: 2},
		},
	}
	nodes := cfg.ExpandNodes()
	if len(nodes) != 2 {
		t.Fatalf("expanded: got %d, want 2", len(nodes))
	}
	n1 := nodes[0].Cert
	if n1.Domain != "node1.example.com" {
		t.Errorf("node1 domain: got %q", n1.Domain)
	}
	if n1.Email != "admin@example.com" {
		t.Errorf("node1 email should inherit global, got %q", n1.Email)
	}
	if n1.DNSProvider != "cloudflare" {
		t.Errorf("node1 dns_provider should inherit global, got %q", n1.DNSProvider)
	}
	if n1.DNSEnv["CF_API_TOKEN"] != "node-token" {
		t.Errorf("node1 dns_env CF_API_TOKEN: got %q, want node-token", n1.DNSEnv["CF_API_TOKEN"])
	}
	if n1.DNSEnv["OTHER"] != "kept" {
		t.Errorf("node1 dns_env OTHER should survive merge, got %q", n1.DNSEnv["OTHER"])
	}
	if n1.HTTPPort != 80 || n1.CertMode != "dns" {
		t.Errorf("node1 http_port/cert_mode should inherit global, got %d/%q", n1.HTTPPort, n1.CertMode)
	}
	if cfg.Cert.DNSEnv["CF_API_TOKEN"] != "global-token" {
		t.Errorf("global dns_env mutated by merge: got %q", cfg.Cert.DNSEnv["CF_API_TOKEN"])
	}
	if cfg.Cert.Domain != "global.example.com" {
		t.Errorf("global domain mutated: got %q", cfg.Cert.Domain)
	}
	// Node 2 inherits everything except CertDir, which is derived (item 3).
	if nodes[1].Cert.Domain != "global.example.com" {
		t.Errorf("node2 domain: got %q", nodes[1].Cert.Domain)
	}
}

func TestExpandNodes_CertDirPerNode(t *testing.T) {
	// Item 3: the pre-fix "only when empty" branch never fired because
	// setDefaultsFrom had already filled CertDir with base/certs.
	// Even with a filled global CertDir, each node must get its own dir.
	cfg := &Config{
		Panel:  PanelConfig{URL: "https://panel.example.com", Token: "tok"},
		Kernel: KernelConfig{ConfigDir: "/base", GeoDataDir: "/base"},
		Cert:   CertConfig{CertDir: "/base/certs"},
		Nodes: []NodeEntry{
			{NodeID: 1},
			{NodeID: 2},
			{NodeID: 3, Kernel: &KernelOverride{ConfigDir: "/elsewhere"}},
			{NodeID: 4, Cert: &CertConfig{CertDir: "/explicit/certs"}},
		},
	}
	nodes := cfg.ExpandNodes()
	want := []string{
		"/base/node-1/certs",
		"/base/node-2/certs",
		"/elsewhere/certs",
		"/explicit/certs",
	}
	for i, w := range want {
		if nodes[i].Cert.CertDir != w {
			t.Errorf("node %d cert_dir: got %q, want %q", i+1, nodes[i].Cert.CertDir, w)
		}
	}
	if nodes[0].Cert.CertDir == nodes[1].Cert.CertDir {
		t.Error("nodes 1 and 2 must not share a cert dir")
	}
}

func TestValidateStartupLayout_ExpandedDirsAndBinding(t *testing.T) {
	// Item 4a: same instance-level dir, distinct expanded dirs — must pass.
	a := &Config{InstanceID: "inst-a",
		Panel:  PanelConfig{URL: "https://panel.example.com", Token: "tok"},
		Kernel: KernelConfig{ConfigDir: "/base"},
		Nodes:  []NodeEntry{{NodeID: 1}},
	}
	b := &Config{InstanceID: "inst-b",
		Panel:  PanelConfig{URL: "https://panel.example.com", Token: "tok"},
		Kernel: KernelConfig{ConfigDir: "/base"},
		Nodes:  []NodeEntry{{NodeID: 2}},
	}
	if err := ValidateStartupLayout([]*Config{a, b}); err != nil {
		t.Errorf("distinct expanded dirs must not conflict: %v", err)
	}

	// Item 4b: distinct instance dirs, colliding expanded dirs — must fail.
	c := &Config{InstanceID: "inst-c",
		Panel:  PanelConfig{URL: "https://panel.example.com", Token: "tok"},
		Kernel: KernelConfig{ConfigDir: "/base-c"},
		Nodes:  []NodeEntry{{NodeID: 1, Kernel: &KernelOverride{ConfigDir: "/collide"}}},
	}
	d := &Config{InstanceID: "inst-d",
		Panel:  PanelConfig{URL: "https://panel.example.com", Token: "tok"},
		Kernel: KernelConfig{ConfigDir: "/base-d"},
		Nodes:  []NodeEntry{{NodeID: 2, Kernel: &KernelOverride{ConfigDir: "/collide"}}},
	}
	if err := ValidateStartupLayout([]*Config{c, d}); err == nil {
		t.Error("colliding expanded config dirs must be rejected")
	}

	// Item 4c: trailing slash / scheme+host case must not create a
	// second binding for the same panel + node.
	e := &Config{InstanceID: "inst-e",
		Panel:  PanelConfig{URL: "https://panel.example.com/", Token: "tok", NodeID: 7},
		Kernel: KernelConfig{ConfigDir: "/dir-e"},
	}
	f := &Config{InstanceID: "inst-f",
		Panel:  PanelConfig{URL: "HTTPS://Panel.Example.COM", Token: "tok", NodeID: 7},
		Kernel: KernelConfig{ConfigDir: "/dir-f"},
	}
	if err := ValidateStartupLayout([]*Config{e, f}); err == nil {
		t.Error("https://panel.example.com/ and HTTPS://Panel.Example.COM with the same node_id must be one binding")
	}

	// Standalone instances have no panel binding and must not collide
	// on the empty URL / node 0 pair.
	s1 := &Config{InstanceID: "sa-1",
		Kernel:     KernelConfig{ConfigDir: "/sa-1"},
		Standalone: &StandaloneConfig{Enabled: true, Node: StandaloneNodeConfig{Protocol: "vless", ServerPort: 8443}},
	}
	s2 := &Config{InstanceID: "sa-2",
		Kernel:     KernelConfig{ConfigDir: "/sa-2"},
		Standalone: &StandaloneConfig{Enabled: true, Node: StandaloneNodeConfig{Protocol: "vless", ServerPort: 9443}},
	}
	if err := ValidateStartupLayout([]*Config{s1, s2}); err != nil {
		t.Errorf("standalone instances must not collide on panel binding: %v", err)
	}
}

func TestAutoInstanceID_StandaloneDistinct(t *testing.T) {
	// Item 5: distinct standalone inbounds get distinct IDs; an
	// identical inbound is a genuine duplicate.
	mk := func(protocol string, port int) *Config {
		return &Config{Standalone: &StandaloneConfig{
			Enabled: true,
			Node:    StandaloneNodeConfig{Protocol: protocol, ServerPort: port},
			Users:   []StandaloneUser{{ID: 1, UUID: "u1"}},
		}}
	}
	id1, err := mk("vless", 8443).AutoInstanceID()
	if err != nil {
		t.Fatalf("AutoInstanceID: %v", err)
	}
	id2, err := mk("vless", 9443).AutoInstanceID()
	if err != nil {
		t.Fatalf("AutoInstanceID: %v", err)
	}
	id3, err := mk("trojan", 8443).AutoInstanceID()
	if err != nil {
		t.Fatalf("AutoInstanceID: %v", err)
	}
	if id1 == id2 {
		t.Errorf("different ports must give different standalone ids, both %q", id1)
	}
	if id1 == id3 {
		t.Errorf("different protocols must give different standalone ids, both %q", id1)
	}
	dup, err := mk("vless", 8443).AutoInstanceID()
	if err != nil {
		t.Fatalf("AutoInstanceID: %v", err)
	}
	if dup != id1 {
		t.Errorf("identical standalone inbound must give the same id: %q vs %q", dup, id1)
	}
	// Wildcard listen addresses are the same bind.
	w1, _ := (&Config{Standalone: &StandaloneConfig{Enabled: true, Node: StandaloneNodeConfig{Protocol: "vless", ServerPort: 8443}}}).AutoInstanceID()
	w2, _ := (&Config{Standalone: &StandaloneConfig{Enabled: true, Node: StandaloneNodeConfig{Protocol: "vless", ListenIP: "::", ServerPort: 8443}}}).AutoInstanceID()
	if w1 != w2 {
		t.Errorf("empty and :: listen must give the same id: %q vs %q", w1, w2)
	}
}

func TestLoadRoot_MultipleStandaloneInstances(t *testing.T) {
	// Item 5 end-to-end: two standalone instances with different
	// inbounds must load (previously both got the same ID and
	// validateRoot rejected the second as a duplicate).
	path := writeTemp(t, `
instances:
  - standalone:
      enabled: true
      node:
        protocol: "vless"
        server_port: 8443
      users:
        - id: 1
          uuid: "11111111-1111-1111-1111-111111111111"
  - standalone:
      enabled: true
      node:
        protocol: "vless"
        server_port: 9443
      users:
        - id: 1
          uuid: "22222222-2222-2222-2222-222222222222"
`)
	root, err := LoadRoot(path)
	if err != nil {
		t.Fatalf("LoadRoot with two standalone instances: %v", err)
	}
	if root.Instances[0].InstanceID == root.Instances[1].InstanceID {
		t.Errorf("standalone instance ids must differ, both %q", root.Instances[0].InstanceID)
	}
	// ...but an identical inbound is still a duplicate.
	dupPath := writeTemp(t, `
instances:
  - standalone:
      enabled: true
      node:
        protocol: "vless"
        server_port: 8443
      users:
        - id: 1
          uuid: "11111111-1111-1111-1111-111111111111"
  - standalone:
      enabled: true
      node:
        protocol: "vless"
        server_port: 8443
      users:
        - id: 2
          uuid: "22222222-2222-2222-2222-222222222222"
`)
	if _, err := LoadRoot(dupPath); err == nil {
		t.Error("two standalone instances with the same inbound must be rejected as duplicates")
	}
}

func TestLoad_InvalidNodeTypeRejected(t *testing.T) {
	// Item 6: unknown strings fail at config load; every type in the
	// allowlist (and an empty node_type) loads.
	for _, bad := range []string{"v2ray", "ss", "wireguard", "unknown-proto", "V2RAY"} {
		path := writeTemp(t, "\npanel:\n  url: \"https://panel.example.com\"\n  token: \"x\"\n  node_id: 5\n  node_type: \""+bad+"\"\nkernel:\n  type: singbox\n")
		if _, err := Load(path); err == nil {
			t.Errorf("Load should reject node_type %q", bad)
		}
	}
	for _, good := range validNodeTypes {
		path := writeTemp(t, "\npanel:\n  url: \"https://panel.example.com\"\n  token: \"x\"\n  node_id: 5\n  node_type: \""+good+"\"\nkernel:\n  type: singbox\n")
		if _, err := Load(path); err != nil {
			t.Errorf("Load should accept node_type %q: %v", good, err)
		}
	}
	// Per-node entry type is checked too.
	badNodePath := writeTemp(t, `
panel:
  url: "https://panel.example.com"
  token: "x"
nodes:
  - node_id: 1
    node_type: "bogus"
kernel:
  type: singbox
`)
	if _, err := Load(badNodePath); err == nil {
		t.Error("Load should reject nodes[0].node_type \"bogus\"")
	}
	// Empty node_type still loads (panel resolves it from node_id).
	emptyPath := writeTemp(t, `
panel:
  url: "https://panel.example.com"
  token: "x"
  node_id: 5
kernel:
  type: singbox
`)
	if _, err := Load(emptyPath); err != nil {
		t.Errorf("Load with empty node_type should succeed: %v", err)
	}
}
