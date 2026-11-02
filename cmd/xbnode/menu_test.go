package main

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cedar2025/xboard-node/internal/config"
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

func TestFormatServiceStatus(t *testing.T) {
	cases := map[string]string{
		"active":       "已运行",
		"failed":       "异常",
		"activating":   "启动中",
		"deactivating": "停止中",
		"inactive":     "未运行",
		"unknown":      "未知",
		"":             "未知",
	}
	for in, want := range cases {
		if got := formatServiceStatus(in); got != want {
			t.Errorf("formatServiceStatus(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestColorizeStatusValue(t *testing.T) {
	cases := map[string]string{
		"已运行": "\x1b[32m已运行\x1b[0m",
		"运行中": "\x1b[32m运行中\x1b[0m",
		"启动中": "\x1b[32m启动中\x1b[0m",
		"未运行": "\x1b[31m未运行\x1b[0m",
		"未启动": "\x1b[31m未启动\x1b[0m",
		"停止":  "\x1b[31m停止\x1b[0m",
		"停止中": "\x1b[31m停止中\x1b[0m",
		"异常":  "\x1b[31m异常\x1b[0m",
		"未知":  "未知",
	}
	for in, want := range cases {
		if got := colorizeStatusValue(in, true); got != want {
			t.Errorf("colorizeStatusValue(%q, true) = %q, want %q", in, got, want)
		}
		if got := colorizeStatusValue(in, false); got != in {
			t.Errorf("colorizeStatusValue(%q, false) = %q, want plain %q", in, got, in)
		}
	}
	if got := colorizeAutostartValue("是", true); got != "\x1b[32m是\x1b[0m" {
		t.Errorf("colorizeAutostartValue(是) = %q", got)
	}
	if got := colorizeAutostartValue("否", true); got != "\x1b[31m否\x1b[0m" {
		t.Errorf("colorizeAutostartValue(否) = %q", got)
	}
}

func TestLoadCredentialsEnvFrom(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials.env")
	content := "# comment\n\nTEST_XBNODE_TOKEN_A=secret-a\nBADLINE\nTEST_XBNODE_TOKEN_B=secret=b\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_XBNODE_TOKEN_A", "keep-me")
	oldB, hadB := os.LookupEnv("TEST_XBNODE_TOKEN_B")
	os.Unsetenv("TEST_XBNODE_TOKEN_B")
	defer func() {
		if hadB {
			os.Setenv("TEST_XBNODE_TOKEN_B", oldB)
		} else {
			os.Unsetenv("TEST_XBNODE_TOKEN_B")
		}
	}()
	loadCredentialsEnvFrom(path)
	if got := os.Getenv("TEST_XBNODE_TOKEN_A"); got != "keep-me" {
		t.Errorf("existing env should win, got %q", got)
	}
	if got := os.Getenv("TEST_XBNODE_TOKEN_B"); got != "secret=b" {
		t.Errorf("missing key should be filled from file, got %q", got)
	}
	// Missing file is a silent no-op, not an error.
	loadCredentialsEnvFrom(filepath.Join(dir, "nope.env"))
}

// 复现用户服务器上的情况：机器模式配置里 token 为空、只有 token_env，
// 真 token 在 credentials.env 里。不读凭据文件时 LoadRoot 校验失败，
// 读了之后才通过——这正是底部误报未启动而服务本身正常的原因。
func TestMachineConfigTokenFromCredentialsEnv(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yml")
	cfg := "panel:\n  url: https://panel.example.org\nmachine:\n  machine_id: 1\n  token_env: TEST_XBNODE_MACHINE_TOKEN\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	credPath := filepath.Join(dir, "credentials.env")
	if err := os.WriteFile(credPath, []byte("TEST_XBNODE_MACHINE_TOKEN=secret-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old, had := os.LookupEnv("TEST_XBNODE_MACHINE_TOKEN")
	os.Unsetenv("TEST_XBNODE_MACHINE_TOKEN")
	defer func() {
		if had {
			os.Setenv("TEST_XBNODE_MACHINE_TOKEN", old)
		} else {
			os.Unsetenv("TEST_XBNODE_MACHINE_TOKEN")
		}
	}()
	if _, err := config.LoadRoot(cfgPath); err == nil {
		t.Fatalf("LoadRoot without credentials env should fail, got success")
	}
	loadCredentialsEnvFrom(credPath)
	root, err := config.LoadRoot(cfgPath)
	if err != nil {
		t.Fatalf("LoadRoot with credentials env: %v", err)
	}
	instances, err := root.NormalizeInstances()
	if err != nil || len(instances) != 1 {
		t.Fatalf("NormalizeInstances = %d instances, err %v", len(instances), err)
	}
	if instances[0].Machine == nil || instances[0].Machine.Token != "secret-token" {
		t.Errorf("machine token was not resolved from credentials env")
	}
}

func TestFooterStatusText(t *testing.T) {
	cases := []struct {
		valid bool
		state string
		want  string
	}{
		{true, "active", "运行中"},
		{true, "inactive", "停止"},
		{true, "failed", "停止"},
		{true, "activating", "停止"},
		{true, "deactivating", "停止"},
		{true, "unknown", "未知"},
		{true, "", "未知"},
		{false, "active", "未启动"},
		{false, "activating", "未启动"},
		{false, "unknown", "未启动"},
	}
	for _, c := range cases {
		if got := footerStatusText(c.valid, c.state); got != c.want {
			t.Errorf("footerStatusText(%v, %q) = %q, want %q", c.valid, c.state, got, c.want)
		}
	}
}

// 提示范围与菜单项数量挂钩：加了 13/14/15 后提示必须是 [0-15]。
func TestMenuPromptRange(t *testing.T) {
	in := bufio.NewReader(strings.NewReader("0\n"))
	var out bytes.Buffer
	if err := runMenuLoop(in, &out); err != nil {
		t.Fatalf("runMenuLoop: %v", err)
	}
	if !strings.Contains(out.String(), "请输入选项编号 [0-15]：") {
		t.Errorf("prompt range not updated to [0-15]:\n%s", out.String())
	}
	for _, want := range []string{"13. 开启开机自启", "14. 关闭开机自启", "15. 删除实例"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("menu output missing %q:\n%s", want, out.String())
		}
	}
}

func TestPrintMenuShowsServiceStatus(t *testing.T) {
	var out bytes.Buffer
	printMenu(&out)
	if !strings.Contains(out.String(), "xboard-node 状态：") {
		t.Fatalf("menu output missing service status line:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "是否开机自启：") {
		t.Fatalf("menu output missing autostart line:\n%s", out.String())
	}
}

// testDisplayWidth 与实现无关的列宽算法：中文/全角算 2 列，其余 1 列，
// 用于断言菜单各行的标题列与描述列真正对齐，而不是只断言字面空格。
func testDisplayWidth(s string) int {
	w := 0
	for _, r := range s {
		if (r >= 0x2e80 && r <= 0x9fff) || (r >= 0xff01 && r <= 0xff60) ||
			(r >= 0xffe0 && r <= 0xffe6) || r == 0x3001 || r == 0xff08 || r == 0xff09 {
			w += 2
		} else {
			w++
		}
	}
	return w
}

func TestPrintMenuAlignment(t *testing.T) {
	var out bytes.Buffer
	printMenu(&out)
	titles := []string{"安装/绑定节点", "绑定机器", "启动服务", "停止服务", "重启服务", "查看服务状态", "查看实例列表", "查看实时日志", "查看版本", "切换内核", "更新到最新版", "卸载", "开启开机自启", "关闭开机自启", "删除实例"}
	descs := []string{"填写面板地址、节点密钥", "填写面板地址、机器密钥", "启动 xboard-node", "停止 xboard-node", "重启 xboard-node", "服务、健康检查", "列出已绑定的节点", "跟随输出服务日志", "显示 xbnode 版本", "在 sing-box 与 xray 之间切换", "从发布页下载最新版", "停止并删除服务", "设置 xboard-node 开机自启", "取消 xboard-node 开机自启", "删除一个已绑定的节点/机器实例"}
	var titleCol, descCol = -1, -1
	for _, line := range strings.Split(out.String(), "\n") {
		for i, title := range titles {
			idx := strings.Index(line, title)
			if idx < 0 {
				continue
			}
			col := testDisplayWidth(line[:idx])
			if titleCol < 0 {
				titleCol = col
			} else if col != titleCol {
				t.Errorf("title %q starts at col %d, want %d (line %q)", title, col, titleCol, line)
			}
			didx := strings.Index(line, descs[i])
			if didx < 0 {
				t.Errorf("line for %q missing desc %q: %q", title, descs[i], line)
				continue
			}
			dcol := testDisplayWidth(line[:didx])
			if descCol < 0 {
				descCol = dcol
			} else if dcol != descCol {
				t.Errorf("desc for %q starts at col %d, want %d (line %q)", title, dcol, descCol, line)
			}
		}
	}
	if titleCol < 0 || descCol < 0 {
		t.Fatalf("did not find menu item lines in output:\n%s", out.String())
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
