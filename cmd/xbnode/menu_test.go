package main

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runMenuWithInput(t *testing.T, input string) string {
	t.Helper()
	var out bytes.Buffer
	if err := runMenuLoop(bufio.NewReader(strings.NewReader(input)), &out); err != nil {
		t.Fatalf("runMenuLoop(%q) error: %v", input, err)
	}
	return out.String()
}

func TestMenuExitOnZero(t *testing.T) {
	out := runMenuWithInput(t, "0\n")
	if !strings.Contains(out, "管理菜单") || !strings.Contains(out, "已退出菜单") {
		t.Fatalf("menu exit output missing header/exit line:\n%s", out)
	}
}

func TestMenuInvalidChoiceThenExit(t *testing.T) {
	out := runMenuWithInput(t, "99\n0\n")
	if !strings.Contains(out, "无效的选择") {
		t.Fatalf("expected invalid-choice message, got:\n%s", out)
	}
	if !strings.Contains(out, "已退出菜单") {
		t.Fatalf("expected clean exit after invalid choice, got:\n%s", out)
	}
}

func TestMenuEOFExitsWithoutHanging(t *testing.T) {
	out := runMenuWithInput(t, "")
	if !strings.Contains(out, "已退出菜单") {
		t.Fatalf("EOF should exit the menu cleanly, got:\n%s", out)
	}
}

func TestConfirmAcceptsYOnly(t *testing.T) {
	for in, want := range map[string]bool{"y\n": true, "Y\n": true, "是\n": true, "n\n": false, "\n": false, "no\n": false} {
		var out bytes.Buffer
		got, err := confirm(bufio.NewReader(strings.NewReader(in)), &out, "确认吗")
		if err != nil {
			t.Fatalf("confirm(%q) error: %v", in, err)
		}
		if got != want {
			t.Fatalf("confirm(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestValidatePanelURL(t *testing.T) {
	if err := validatePanelURL("https://panel.example.com"); err != nil {
		t.Fatalf("https URL should pass: %v", err)
	}
	if err := validatePanelURL("http://panel.example.com"); err != nil {
		t.Fatalf("http URL must warn, not fail: %v", err)
	}
	for _, bad := range []string{"panel.example.com", "ftp://panel.example.com", "https://", ""} {
		if err := validatePanelURL(bad); err == nil {
			t.Fatalf("validatePanelURL(%q) should fail", bad)
		}
	}
}

func TestNormalizeKernel(t *testing.T) {
	for in, want := range map[string]string{"singbox": "singbox", "sing-box": "singbox", "XRAY": "xray"} {
		got, err := normalizeKernel(in)
		if err != nil || got != want {
			t.Fatalf("normalizeKernel(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := normalizeKernel("v2ray"); err == nil {
		t.Fatal("normalizeKernel(v2ray) should fail")
	}
}

func TestSwitchKernelAt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	cfg := `instances:
  - panel:
      url: https://panel.example.com
      node_id: 7
    kernel:
      type: singbox
      config_dir: /tmp/x
    log:
      level: info
`
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	restarts := 0
	restart := func() error { restarts++; return nil }

	// Single instance: empty ID selects it.
	changed, err := switchKernelAt(path, "", "xray", restart)
	if err != nil || !changed {
		t.Fatalf("switch to xray: changed=%v err=%v", changed, err)
	}
	if restarts != 1 {
		t.Fatalf("expected 1 restart, got %d", restarts)
	}
	root, err := loadWritableRootConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(root.Instances) != 1 || root.Instances[0].Kernel.Type != "xray" {
		t.Fatalf("kernel after switch = %v", root.Instances)
	}

	// Switching to the current kernel is a no-op (no rewrite, no restart).
	changed, err = switchKernelAt(path, "", "xray", restart)
	if err != nil || changed {
		t.Fatalf("same-kernel switch: changed=%v err=%v", changed, err)
	}
	if restarts != 1 {
		t.Fatalf("no-op switch must not restart, restarts=%d", restarts)
	}

	// Unknown instance and invalid kernel both fail.
	if _, err := switchKernelAt(path, "no-such-instance", "singbox", restart); err == nil {
		t.Fatal("unknown instance should fail")
	}
	if _, err := switchKernelAt(path, "", "v2ray", restart); err == nil {
		t.Fatal("invalid kernel should fail")
	}
}

func TestMenuPauseEOFIsCleanExit(t *testing.T) {
	// pauseForMenu returns io.EOF as its exit sentinel; runMenuLoop
	// must convert it to a clean nil (exit 0), matching the
	// option-read EOF path, instead of printing "EOF" and exiting 1.
	// Input "6" runs status (no instances in test env: prints a
	// notice, returns nil) and then hits EOF at the pause prompt.
	var out bytes.Buffer
	if err := runMenuLoop(bufio.NewReader(strings.NewReader("6\n")), &out); err != nil {
		t.Fatalf("runMenuLoop after pause EOF should exit cleanly, got error: %v\noutput:\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "已退出菜单") {
		t.Fatalf("expected clean exit line, got:\n%s", out.String())
	}
	// Direct pause check: the sentinel itself is still io.EOF.
	var out2 bytes.Buffer
	if err := pauseForMenu(bufio.NewReader(strings.NewReader("")), &out2); err == nil {
		t.Fatal("pauseForMenu on EOF should return the EOF sentinel")
	}
}

func TestMenuLogItemHonestCtrlCText(t *testing.T) {
	var out bytes.Buffer
	printMenu(&out)
	text := out.String()
	if strings.Contains(text, "按 Ctrl+C 回到菜单") {
		t.Fatalf("log menu item still promises returning to the menu:\n%s", text)
	}
	if !strings.Contains(text, "按 Ctrl+C 退出") {
		t.Fatalf("log menu item should state Ctrl+C exits:\n%s", text)
	}
}
