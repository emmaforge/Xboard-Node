package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/cedar2025/xboard-node/internal/config"
	versionpkg "github.com/cedar2025/xboard-node/internal/version"
	"gopkg.in/yaml.v3"
)

const (
	defaultConfigPath      = "/etc/xboard-node/config.yml"
	defaultMetaPath        = "/etc/xboard-node/install-meta.json"
	defaultCredentialsPath = "/etc/xboard-node/credentials.env"
	defaultBinaryPath      = "/usr/local/bin/xboard-node"
	defaultCLIPath         = "/usr/local/bin/xbnode"
	serviceName            = "xboard-node.service"
	serviceFilePath        = "/etc/systemd/system/xboard-node.service"
	defaultInstallRoot     = "/etc/xboard-node"
	downloadBase           = "https://github.com/zzx327317-creator/Xboard-Node/releases"
)

var (
	// version is the release version from internal/version; it is no
	// longer overwritten with a commit hash at build time.
	version = versionpkg.Version
)

type instanceRow struct {
	ID      string `json:"id"`
	Mode    string `json:"mode"`
	Panel   string `json:"panel"`
	Target  string `json:"target"`
	Service string `json:"service"`
	Health  string `json:"health"`
}

type fileRootConfig struct {
	Log        *fileLogConfig     `yaml:"log,omitempty"`
	Kernel     *fileKernelConfig  `yaml:"kernel,omitempty"`
	Node       *fileNodeConfig    `yaml:"node,omitempty"`
	WS         *config.WSConfig   `yaml:"ws,omitempty"`
	Runtime    *fileRuntimeConfig `yaml:"runtime,omitempty"`
	Cert       *config.CertConfig `yaml:"cert,omitempty"`
	HealthPort int                `yaml:"health_port,omitempty"`
	Instances  []fileInstance     `yaml:"instances,omitempty"`
}

type fileInstance struct {
	ID         string                   `yaml:"id,omitempty"`
	Panel      filePanelConfig          `yaml:"panel"`
	Node       *fileNodeConfig          `yaml:"node,omitempty"`
	Kernel     fileKernelConfig         `yaml:"kernel"`
	Log        fileLogConfig            `yaml:"log"`
	Runtime    *fileRuntimeConfig       `yaml:"runtime,omitempty"`
	HealthPort int                      `yaml:"health_port,omitempty"`
	Machine    *fileMachineConfig       `yaml:"machine,omitempty"`
	Standalone *config.StandaloneConfig `yaml:"standalone,omitempty"`
	Cert       *config.CertConfig       `yaml:"cert,omitempty"`
	WS         *config.WSConfig         `yaml:"ws,omitempty"`
	Nodes      []config.NodeEntry       `yaml:"nodes,omitempty"`
}

type filePanelConfig struct {
	URL      string `yaml:"url"`
	TokenEnv string `yaml:"token_env,omitempty"`
	NodeID   int    `yaml:"node_id,omitempty"`
	NodeType string `yaml:"node_type,omitempty"`
}

type fileMachineConfig struct {
	MachineID int    `yaml:"machine_id"`
	TokenEnv  string `yaml:"token_env,omitempty"`
}

type fileNodeConfig struct {
	PushInterval         int `yaml:"push_interval,omitempty"`
	PullInterval         int `yaml:"pull_interval,omitempty"`
	TrackInterval        int `yaml:"track_interval,omitempty"`
	DeviceReportInterval int `yaml:"device_report_interval,omitempty"`
}

type fileKernelConfig struct {
	Type         string           `yaml:"type"`
	ConfigDir    string           `yaml:"config_dir"`
	LogLevel     string           `yaml:"log_level,omitempty"`
	GeoDataDir   string           `yaml:"geo_data_dir,omitempty"`
	CustomConfig string           `yaml:"custom_config,omitempty"`
	CustomRoute  []map[string]any `yaml:"custom_route,omitempty"`
	CustomOut    []map[string]any `yaml:"custom_outbound,omitempty"`
}

type fileLogConfig struct {
	Level  string `yaml:"level,omitempty"`
	Output string `yaml:"output,omitempty"`
}

type fileRuntimeConfig struct {
	GoMemLimit  string `yaml:"gomemlimit,omitempty"`
	GoGCPercent int    `yaml:"gogc,omitempty"`
}

type instanceSummary struct {
	ID         string `json:"id"`
	PanelURL   string `json:"panel_url"`
	Mode       string `json:"mode"`
	NodeID     *int   `json:"node_id"`
	MachineID  *int   `json:"machine_id"`
	HealthPort int    `json:"health_port"`
}

type installMeta struct {
	ConfigMode       string            `json:"config_mode"`
	Version          string            `json:"version"`
	LatestInstanceID string            `json:"latest_instance_id"`
	InstanceCount    int               `json:"instance_count"`
	Instances        []instanceSummary `json:"instances"`
	UpdatedAt        string            `json:"updated_at"`
}

func loadInstallMeta(path string) (*installMeta, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	meta := &installMeta{}
	if err := json.Unmarshal(data, meta); err != nil {
		return nil, fmt.Errorf("parse install meta: %w", err)
	}
	return meta, nil
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		// 不带参数：终端里打开中文交互菜单；非终端（管道/重定向）
		// 只打印提示后退出，绝不等待输入。
		return runMenu()
	}
	switch args[0] {
	case "menu":
		return runMenu()
	case "kernel":
		return runKernel(args[1:])
	case "status":
		return runStatus()
	case "list":
		return runList(args[1:])
	case "instance":
		return runInstance(args[1:])
	case "service":
		return runService(args[1:])
	case "logs", "log":
		return runService([]string{"logs"})
	case "health":
		return runHealth()
	case "bind":
		return runBind(args[1:])
	case "bind-node":
		return runBind(append([]string{"add-node"}, args[1:]...))
	case "bind-machine":
		return runBind(append([]string{"add-machine"}, args[1:]...))
	case "unbind-node":
		return runBind(append([]string{"remove-node"}, args[1:]...))
	case "unbind-machine":
		return runBind(append([]string{"remove-machine"}, args[1:]...))
	case "start", "stop", "restart", "enable", "disable":
		return runService(args)
	case "upgrade":
		return runUpgrade(args[1:])
	case "uninstall":
		return runUninstall(args[1:])
	case "version", "-v", "--version":
		fmt.Printf("xbnode %s\n", version)
		return nil
	case "config":
		return runConfig(args[1:])
	case "help", "-h", "--help":
		printUsage()
		return nil
	default:
		return fmt.Errorf("unknown command: %s", args[0])
	}
}

func printUsage() {
	fmt.Println(`xbnode 命令：
  xbnode（不带参数）             打开中文交互菜单（仅限终端）
  xbnode menu                   打开中文交互菜单
  xbnode kernel                 列出各实例的内核
  xbnode kernel <id> <singbox|xray>  切换指定实例的内核
  xbnode help                   显示本帮助
  xbnode status                 查看运行状态
  xbnode list [--output text|json]  列出全部实例
  xbnode instance list [--output text|json]  列出实例
  xbnode instance get <id> [--output text|json]  查看单个实例
  xbnode config init --mode node|machine --panel-url URL --token TOKEN [flags]  初始化配置
  xbnode config health-port [--config PATH]  查看健康检查端口
  xbnode service status|start|stop|restart|enable|disable|logs  服务管理
  xbnode health                 健康检查
  xbnode bind add-node --panel-url URL --token TOKEN --node-id ID [--node-type TYPE] [--kernel singbox|xray]  绑定节点
  xbnode bind add-machine --panel-url URL --token TOKEN --machine-id ID [--kernel singbox|xray]  绑定机器
  xbnode bind remove <instance-id>  解绑实例
  xbnode bind remove-node --panel URL --node-id ID  解绑节点
  xbnode bind remove-machine --panel URL --machine-id ID  解绑机器
  xbnode upgrade [--version VERSION]  升级
  xbnode uninstall [--purge] [--yes]  卸载
  xbnode version                查看版本

快捷方式：
  xbnode start|stop|restart        = xbnode service start|stop|restart
  xbnode log|logs                  = xbnode service logs
  xbnode bind-node ...             = xbnode bind add-node ...
  xbnode bind-machine ...          = xbnode bind add-machine ...
  xbnode unbind-node ...           = xbnode bind remove-node ...
  xbnode unbind-machine ...        = xbnode bind remove-machine ...`)
}

func runStatus() error {
	fmt.Println("xboard-node status")
	fmt.Println()

	// Version from install-meta.json
	ver := "unknown"
	if meta, err := loadInstallMeta(defaultMetaPath); err == nil {
		ver = meta.Version
	}
	fmt.Printf("  version:  %s\n", ver)

	// Service status
	svc := systemctlState()
	fmt.Printf("  service:  %s\n", svc)

	// Health
	health := instanceAwareHealth()
	fmt.Printf("  health:   %s\n", health)
	fmt.Println()

	// Instance list
	rows, err := collectInstanceRows()
	if err != nil {
		fmt.Printf("  (no instances found: %v)\n", err)
		return nil
	}
	return printRows(rows, "text")
}

func runList(args []string) error {
	output := parseOutput(args)
	rows, err := collectInstanceRows()
	if err != nil {
		return err
	}
	return printRows(rows, output)
}

func runInstance(args []string) error {
	if len(args) == 0 {
		return runList(nil)
	}
	if args[0] == "list" {
		return runList(args[1:])
	}
	if args[0] != "get" {
		return fmt.Errorf("unknown instance command: %s", args[0])
	}
	if len(args) < 2 {
		return errors.New("usage: xbnode instance get <id> [--output text|json]")
	}
	id := args[1]
	output := parseOutput(args[2:])
	rows, err := collectInstanceRows()
	if err != nil {
		return err
	}
	for _, row := range rows {
		if row.ID == id {
			return printRows([]instanceRow{row}, output)
		}
	}
	return fmt.Errorf("instance not found: %s", id)
}

func runService(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: xbnode service <status|start|stop|restart|enable|disable|logs>")
	}
	sub := args[0]
	rest := args[1:]
	switch sub {
	case "status":
		return runCommand("sudo", append([]string{"systemctl", "status", serviceName, "--no-pager"}, rest...)...)
	case "start", "stop", "restart", "enable", "disable":
		return runCommand("sudo", append([]string{"systemctl", sub, serviceName}, rest...)...)
	case "logs":
		if len(rest) == 0 {
			rest = []string{"-f"}
		}
		return runCommand("sudo", append([]string{"journalctl", "-u", serviceName}, rest...)...)
	default:
		return fmt.Errorf("unknown service command: %s", sub)
	}
}

func runHealth() error {
	h := instanceAwareHealth()
	fmt.Println(h)
	if h == "down" {
		return errors.New("health check failed")
	}
	return nil
}

func runBind(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: xbnode bind <add-node|add-machine|remove-node|remove-machine> ...")
	}
	if err := ensureRoot("bind"); err != nil {
		return err
	}
	sub := args[0]
	rest := args[1:]
	switch sub {
	case "add-node":
		return runBindAdd("node", rest)
	case "add-machine":
		return runBindAdd("machine", rest)
	case "remove-node":
		panel, nodeID, err := parseRemoveNodeArgs(rest)
		if err != nil {
			return err
		}
		return removeBinding(panel, nodeID, 0, "")
	case "remove-machine":
		panel, machineID, err := parseRemoveMachineArgs(rest)
		if err != nil {
			return err
		}
		return removeBinding(panel, 0, machineID, "")
	case "remove":
		if len(rest) == 0 {
			return errors.New("usage: xbnode bind remove <instance-id>")
		}
		return removeBinding("", 0, 0, rest[0])
	default:
		return fmt.Errorf("unknown bind command: %s", sub)
	}
}

// validateBindArgs checks a bind's arguments before anything is
// written or restarted, so a missing/illegal value fails loudly here
// instead of printing "Binding added successfully" afterwards. It
// validates: token non-empty, mode node/machine, kernel singbox/xray
// when given, and the required ID for the mode. Panel URL presence is
// also checked here (runConfigInit re-checks it as well).
func validateBindArgs(mode string, args []string) error {
	if mode != "node" && mode != "machine" {
		return fmt.Errorf("invalid bind mode: %s (want node or machine)", mode)
	}
	var token, panelURL, kernel string
	var nodeID, machineID int
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--panel-url":
			if i+1 >= len(args) {
				return errors.New("missing value for --panel-url")
			}
			panelURL = args[i+1]
			i++
		case "--token":
			if i+1 >= len(args) {
				return errors.New("missing value for --token")
			}
			token = args[i+1]
			i++
		case "--node-id":
			if i+1 >= len(args) {
				return errors.New("missing value for --node-id")
			}
			v, err := parsePositiveInt(args[i+1], "node-id")
			if err != nil {
				return err
			}
			nodeID = v
			i++
		case "--machine-id":
			if i+1 >= len(args) {
				return errors.New("missing value for --machine-id")
			}
			v, err := parsePositiveInt(args[i+1], "machine-id")
			if err != nil {
				return err
			}
			machineID = v
			i++
		case "--node-type", "--kernel", "--health-port", "--gomemlimit", "--gogc":
			if i+1 >= len(args) {
				return fmt.Errorf("missing value for %s", args[i])
			}
			if args[i] == "--kernel" {
				kernel = args[i+1]
			}
			i++
		default:
			return fmt.Errorf("unknown bind flag: %s", args[i])
		}
	}
	if strings.TrimSpace(panelURL) == "" {
		return errors.New("bind requires --panel-url")
	}
	if strings.TrimSpace(token) == "" {
		return errors.New("bind requires --token (panel/machine key must not be empty)")
	}
	if kernel != "" {
		if _, err := normalizeKernel(kernel); err != nil {
			return fmt.Errorf("invalid --kernel: %w", err)
		}
	}
	if mode == "node" && nodeID <= 0 {
		return errors.New("bind requires --node-id for node mode")
	}
	if mode == "machine" && machineID <= 0 {
		return errors.New("bind requires --machine-id for machine mode")
	}
	return nil
}

func runBindAdd(mode string, args []string) error {
	if err := validateBindArgs(mode, args); err != nil {
		return fmt.Errorf("bind failed: %w", err)
	}
	// Build configInit args from bind args
	initArgs := []string{
		"--mode", mode,
		"--config", defaultConfigPath,
		"--output", defaultConfigPath,
		"--credentials-in", defaultCredentialsPath,
		"--credentials-out", defaultCredentialsPath,
		"--meta", defaultMetaPath,
		"--install-root", defaultInstallRoot,
	}
	// Pass through remaining args (--panel-url, --token, --node-id, --machine-id, --kernel, etc.)
	initArgs = append(initArgs, args...)

	if err := runConfigInit(initArgs); err != nil {
		return fmt.Errorf("bind failed: %w", err)
	}
	// Restart service to pick up new config
	fmt.Println("Restarting service...")
	if err := runCommand("systemctl", "restart", serviceName); err != nil {
		return fmt.Errorf("service restart failed: %w", err)
	}
	fmt.Println("Binding added successfully")
	return nil
}

func runUpgrade(args []string) error {
	if err := ensureRoot("upgrade"); err != nil {
		return err
	}

	version := "latest"
	for i := 0; i < len(args); i++ {
		if args[i] == "--version" && i+1 < len(args) {
			version = args[i+1]
			i++
		}
	}

	arch := runtime.GOARCH
	if arch != "amd64" && arch != "arm64" {
		return fmt.Errorf("unsupported architecture: %s", arch)
	}

	fmt.Println("Starting upgrade...")

	binaryDir := filepath.Dir(defaultBinaryPath)
	cliDir := filepath.Dir(defaultCLIPath)
	newBinary := filepath.Join(binaryDir, ".xboard-node.new")
	newCLI := filepath.Join(cliDir, ".xbnode.new")

	// Download with the same latest → dev fallback as install.sh's
	// download_artifact: only a 404 on the latest release falls back
	// to the dev release; a network failure is reported as-is instead
	// of silently installing dev.
	downloadUpgradeArtifact := func(artifact, dest string) error {
		url := resolveDownloadURL(artifact, version)
		fmt.Printf("Downloading %s...\n", url)
		if err := downloadFileWithChecksum(url, dest); err == nil {
			return nil
		} else if version == "latest" && downloadHTTPStatus(err) == 404 {
			devURL := resolveDownloadURL(artifact, "dev")
			fmt.Printf("Latest release not found (404), falling back to dev release: %s\n", devURL)
			if err := downloadFileWithChecksum(devURL, dest); err != nil {
				return fmt.Errorf("download %s from dev release: %w", artifact, err)
			}
			return nil
		} else {
			return err
		}
	}

	if err := downloadUpgradeArtifact(fmt.Sprintf("xboard-node-linux-%s", arch), newBinary); err != nil {
		return fmt.Errorf("download binary: %w", err)
	}

	if err := downloadUpgradeArtifact(fmt.Sprintf("xbnode-linux-%s", arch), newCLI); err != nil {
		os.Remove(newBinary)
		return fmt.Errorf("download xbnode: %w", err)
	}

	if err := os.Chmod(newBinary, 0o755); err != nil {
		return cleanupFiles(newBinary, newCLI, fmt.Errorf("chmod binary: %w", err))
	}
	if err := os.Chmod(newCLI, 0o755); err != nil {
		return cleanupFiles(newBinary, newCLI, fmt.Errorf("chmod xbnode: %w", err))
	}

	// Validate downloaded binaries
	if out, err := exec.Command(newBinary, "-v").CombinedOutput(); err != nil {
		return cleanupFiles(newBinary, newCLI, fmt.Errorf("binary version check failed: %s", string(out)))
	}
	if out, err := exec.Command(newCLI, "version").CombinedOutput(); err != nil {
		return cleanupFiles(newBinary, newCLI, fmt.Errorf("xbnode version check failed: %s", string(out)))
	}

	// Backup existing binaries
	backupBinary := defaultBinaryPath + ".bak"
	backupCLI := defaultCLIPath + ".bak"
	// Backup existing binaries
	if fileExists(defaultBinaryPath) {
		if err := copyFile(defaultBinaryPath, backupBinary); err != nil {
			return cleanupFiles(newBinary, newCLI, fmt.Errorf("backup binary: %w", err))
		}
	}
	if fileExists(defaultCLIPath) {
		if err := copyFile(defaultCLIPath, backupCLI); err != nil {
			return cleanupFiles(newBinary, newCLI, fmt.Errorf("backup xbnode: %w", err))
		}
	}

	// Atomic rename
	if err := os.Rename(newBinary, defaultBinaryPath); err != nil {
		return cleanupFiles(newBinary, newCLI, fmt.Errorf("replace binary: %w", err))
	}
	if err := os.Rename(newCLI, defaultCLIPath); err != nil {
		if fileExists(backupBinary) {
			os.Rename(backupBinary, defaultBinaryPath)
		}
		os.Remove(newCLI)
		return fmt.Errorf("replace xbnode: %w", err)
	}

	// Recreate /usr/bin/xbnode symlink
	os.Remove("/usr/bin/xbnode")
	os.Symlink(defaultCLIPath, "/usr/bin/xbnode")

	// Restart service
	fmt.Println("Restarting service...")
	runCommand("systemctl", "daemon-reload")
	if err := runCommand("systemctl", "restart", serviceName); err != nil {
		fmt.Println("Restart failed, rolling back...")
		rollbackOK := true
		if fileExists(backupBinary) {
			if e := os.Rename(backupBinary, defaultBinaryPath); e != nil {
				fmt.Printf("Warning: rollback binary failed: %v\n", e)
				rollbackOK = false
			}
		}
		if fileExists(backupCLI) {
			if e := os.Rename(backupCLI, defaultCLIPath); e != nil {
				fmt.Printf("Warning: rollback xbnode failed: %v\n", e)
				rollbackOK = false
			}
		}
		runCommand("systemctl", "daemon-reload")
		if e := runCommand("systemctl", "restart", serviceName); e != nil {
			return fmt.Errorf("upgrade and rollback restart both failed: %w", e)
		}
		if rollbackOK {
			return errors.New("upgrade failed: service restart failed, rolled back successfully")
		}
		return errors.New("upgrade failed: partial rollback, check binary state manually")
	}

	// Clean up backups
	os.Remove(backupBinary)
	os.Remove(backupCLI)

	// Update install-meta.json
	newVer := "unknown"
	if out, err := exec.Command(defaultBinaryPath, "-v").CombinedOutput(); err == nil {
		newVer = strings.TrimSpace(string(out))
	}
	if root, err := loadWritableRootConfig(defaultConfigPath); err == nil {
		instances, _ := root.NormalizeInstances()
		writeInstallMetaVersioned(defaultMetaPath, root, newVer, latestInstanceID(instances))
	}

	fmt.Printf("Upgrade complete (version: %s)\n", newVer)
	return nil
}

func runUninstall(args []string) error {
	if err := ensureRoot("uninstall"); err != nil {
		return err
	}

	purge := false
	yes := false
	for _, a := range args {
		switch a {
		case "--purge":
			purge = true
		case "--yes", "-y":
			yes = true
		}
	}

	if !yes {
		fmt.Print("Proceed with uninstall? [y/N]: ")
		var answer string
		fmt.Scanln(&answer)
		if answer != "y" && answer != "Y" {
			fmt.Println("Uninstall cancelled")
			return nil
		}
	}

	var warnings []string

	// Stop and disable service
	if fileExists(serviceFilePath) {
		if err := runCommand("systemctl", "stop", serviceName); err != nil {
			warnings = append(warnings, fmt.Sprintf("stop service: %v", err))
		}
		// Verify the service really stopped before deleting any
		// binary: a stop that reported success (or a stop error
		// swallowed into warnings) must not lead to binaries being
		// removed from under a still-running service.
		if systemctlState() == "active" {
			return fmt.Errorf("uninstall aborted: service %s is still active after stop; binaries not removed", serviceName)
		}
		if err := runCommand("systemctl", "disable", serviceName); err != nil {
			warnings = append(warnings, fmt.Sprintf("disable service: %v", err))
		}
		if err := os.Remove(serviceFilePath); err != nil {
			warnings = append(warnings, fmt.Sprintf("remove service file: %v", err))
		}
		runCommand("systemctl", "daemon-reload")
	}

	// Remove binaries
	// Remove binaries and symlinks
	for _, p := range []string{defaultBinaryPath, defaultCLIPath, "/usr/bin/xbnode"} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			warnings = append(warnings, fmt.Sprintf("remove %s: %v", p, err))
		}
	}

	if purge {
		if err := os.RemoveAll(defaultInstallRoot); err != nil {
			warnings = append(warnings, fmt.Sprintf("remove %s: %v", defaultInstallRoot, err))
		} else {
			fmt.Printf("Removed %s\n", defaultInstallRoot)
		}
	} else {
		os.Remove(defaultMetaPath)
		fmt.Printf("Config preserved under %s\n", defaultInstallRoot)
	}

	if len(warnings) > 0 {
		fmt.Println("Uninstall completed with warnings:")
		for _, w := range warnings {
			fmt.Printf("  - %s\n", w)
		}
		return nil
	}

	fmt.Println("Uninstall complete")
	return nil
}

func ensureRoot(cmd string) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("%s requires root privileges; run with sudo", cmd)
	}
	return nil
}

func resolveDownloadURL(artifact, version string) string {
	if version == "latest" {
		return downloadBase + "/latest/download/" + artifact
	}
	return downloadBase + "/download/" + version + "/" + artifact
}

// downloadHTTPStatus extracts the HTTP status code from a downloadFile
// error ("HTTP <code> from <url>"), or 0 when the error is not an HTTP
// error (e.g. a network failure). It is what lets the upgrade fallback
// distinguish a missing latest release (404 → try dev) from a network
// outage (no fallback, report the failure).
func downloadHTTPStatus(err error) int {
	if err == nil {
		return 0
	}
	var status int
	if _, scanErr := fmt.Sscanf(err.Error(), "HTTP %d", &status); scanErr != nil {
		return 0
	}
	return status
}

func downloadFile(url, dest string) error {
	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, resp.Body)
	return err
}

// downloadFileWithChecksum downloads a file and verifies it against the
// release's published "<artifact>.sha256" sidecar, if one exists. A
// mismatch deletes the downloaded file and is fatal. If no sidecar is
// published (older releases), it warns and continues: failing hard would
// break upgrading from those releases, but new releases (see CI, which now
// publishes .sha256 files) are always verified.
func downloadFileWithChecksum(url, dest string) error {
	if err := downloadFile(url, dest); err != nil {
		return err
	}
	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Get(url + ".sha256")
	if err != nil {
		fmt.Printf("Warning: could not fetch checksum for %s (%v); skipping checksum verification\n", url, err)
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		fmt.Printf("Warning: no SHA-256 checksum published for %s; skipping checksum verification\n", url)
		return nil
	}
	sumData, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		os.Remove(dest)
		return fmt.Errorf("read checksum: %w", err)
	}
	expected := strings.Fields(string(sumData))
	if len(expected) == 0 {
		os.Remove(dest)
		return fmt.Errorf("empty checksum file for %s", url)
	}
	// Stream the hash so large binaries are not loaded into memory.
	f, err := os.Open(dest)
	if err != nil {
		return fmt.Errorf("read downloaded file: %w", err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		os.Remove(dest)
		return fmt.Errorf("hash downloaded file: %w", err)
	}
	actual := hex.EncodeToString(h.Sum(nil))
	if actual != expected[0] {
		os.Remove(dest)
		return fmt.Errorf("SHA-256 mismatch for %s: expected %s, got %s", url, expected[0], actual)
	}
	return nil
}

func cleanupFiles(a, b string, err error) error {
	os.Remove(a)
	os.Remove(b)
	return err
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o755)
}

func runCommand(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	return cmd.Run()
}

func parseOutput(args []string) string {
	for i := 0; i < len(args); i++ {
		if args[i] == "--output" && i+1 < len(args) {
			return args[i+1]
		}
		if strings.HasPrefix(args[i], "--output=") {
			return strings.TrimPrefix(args[i], "--output=")
		}
	}
	return "text"
}

func parseRemoveNodeArgs(args []string) (string, int, error) {
	var panel string
	var nodeID int
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--panel", "-a", "--api":
			if i+1 >= len(args) {
				return "", 0, errors.New("missing value for --panel")
			}
			panel = args[i+1]
			i++
		case "--node-id", "-n":
			if i+1 >= len(args) {
				return "", 0, errors.New("missing value for --node-id")
			}
			var err error
			nodeID, err = parsePositiveInt(args[i+1], "node-id")
			if err != nil {
				return "", 0, err
			}
			i++
		default:
			return "", 0, fmt.Errorf("unknown remove-node arg: %s", args[i])
		}
	}
	if strings.TrimSpace(panel) == "" || nodeID <= 0 {
		return "", 0, errors.New("usage: xbnode bind remove-node --panel URL --node-id ID")
	}
	return strings.TrimSpace(panel), nodeID, nil
}

func parseRemoveMachineArgs(args []string) (string, int, error) {
	var panel string
	var machineID int
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--panel", "-a", "--api":
			if i+1 >= len(args) {
				return "", 0, errors.New("missing value for --panel")
			}
			panel = args[i+1]
			i++
		case "--machine-id":
			if i+1 >= len(args) {
				return "", 0, errors.New("missing value for --machine-id")
			}
			var err error
			machineID, err = parsePositiveInt(args[i+1], "machine-id")
			if err != nil {
				return "", 0, err
			}
			i++
		default:
			return "", 0, fmt.Errorf("unknown remove-machine arg: %s", args[i])
		}
	}
	if strings.TrimSpace(panel) == "" || machineID <= 0 {
		return "", 0, errors.New("usage: xbnode bind remove-machine --panel URL --machine-id ID")
	}
	return strings.TrimSpace(panel), machineID, nil
}

func parsePositiveInt(raw string, field string) (int, error) {
	// Strict whole-string parse, same rule as parseMemLimit (B15 in
	// cmd/xboard-node): fmt.Sscanf("%d") used to accept "12abc" as 12
	// and "1.5" as 1, silently using the wrong ID.
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("invalid %s: %s", field, raw)
	}
	return value, nil
}

func removeBinding(panelURL string, nodeID int, machineID int, instanceID string) error {
	root, err := loadWritableRootConfig(defaultConfigPath)
	if err != nil {
		return err
	}
	instances := normalizeRootInstances(root)
	if len(instances) == 0 {
		return errors.New("no instances configured")
	}

	kept := make([]config.Config, 0, len(instances))
	removed := make([]config.Config, 0, 1)
	for _, inst := range instances {
		var matched bool
		if instanceID != "" {
			// Match by instance ID
			id, _ := inst.AutoInstanceID()
			matched = id == instanceID || inst.InstanceID == instanceID
		} else {
			matched = strings.TrimSpace(inst.Panel.URL) == strings.TrimSpace(panelURL)
			if nodeID > 0 {
				matched = matched && !inst.IsMachineMode() && inst.Panel.NodeID == nodeID
			}
			if machineID > 0 {
				matched = matched && inst.IsMachineMode() && inst.Machine != nil && inst.Machine.MachineID == machineID
			}
		}
		if matched {
			removed = append(removed, inst)
			continue
		}
		kept = append(kept, inst)
	}
	if len(removed) == 0 {
		if instanceID != "" {
			return fmt.Errorf("binding not found: id=%s", instanceID)
		}
		if nodeID > 0 {
			return fmt.Errorf("binding not found: panel=%s node_id=%d", panelURL, nodeID)
		}
		return fmt.Errorf("binding not found: panel=%s machine_id=%d", panelURL, machineID)
	}
	if len(kept) == 0 {
		// Last binding removed — stop the service to prevent crash-loop.
		// The written config must genuinely contain no binding:
		// writeRootConfig normally resurrects a legacy single config
		// from root.Config.Panel, so a copy with the legacy binding
		// fields cleared is written instead (top-level shared settings
		// such as log/kernel are preserved; only the binding is gone).
		// Previously the legacy config was written straight back, so
		// removing the last binding silently kept it.
		lastRoot := *root
		lastRoot.Instances = nil
		lastRoot.Config.Panel = config.PanelConfig{}
		lastRoot.Config.Machine = nil
		lastRoot.Config.Nodes = nil
		lastRoot.Config.Standalone = nil
		if err := writeRootConfig(defaultConfigPath, &lastRoot); err != nil {
			return err
		}
		if err := pruneCredentialKeys(defaultCredentialsPath, removed); err != nil {
			return err
		}
		if err := writeInstallMeta(defaultMetaPath, &lastRoot); err != nil {
			return err
		}
		runCommand("systemctl", "stop", serviceName)
		fmt.Printf("removed %d binding(s)\n", len(removed))
		fmt.Println("All bindings removed. Service stopped.")
		fmt.Println("Use 'xbnode bind add-node/add-machine' to add a new binding, or 'xbnode uninstall' to fully uninstall.")
		return nil
	}

	root.Instances = kept
	// Preserve top-level shared settings (log, kernel, etc.) — instances inherit from these.
	if err := writeRootConfig(defaultConfigPath, root); err != nil {
		return err
	}
	if err := pruneCredentialKeys(defaultCredentialsPath, removed); err != nil {
		return err
	}
	if err := writeInstallMeta(defaultMetaPath, root); err != nil {
		return err
	}
	if err := runCommand("systemctl", "restart", serviceName); err != nil {
		return err
	}
	fmt.Printf("removed %d binding(s)\n", len(removed))
	return nil
}

func loadWritableRootConfig(path string) (*config.RootConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config file: %w", err)
	}
	rc := &config.RootConfig{}
	if err := yaml.Unmarshal(data, rc); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if len(rc.Instances) == 0 {
		legacy := &config.Config{}
		if err := yaml.Unmarshal(data, legacy); err != nil {
			return nil, fmt.Errorf("parse legacy config: %w", err)
		}
		rc.Config = *legacy
	}
	return rc, nil
}

func normalizeRootInstances(root *config.RootConfig) []config.Config {
	if len(root.Instances) > 0 {
		return append([]config.Config(nil), root.Instances...)
	}
	// Only treat legacy single-config mode if the embedded Config is valid
	if root.Config.Panel.URL != "" {
		return []config.Config{root.Config}
	}
	return nil
}

// certNonEmpty reports whether any cert field is set. The old write
// gate only looked at mode/domain/cert_file/auto_tls, so a cert block
// carrying only an email, DNS provider, http_port, cert_dir or inline
// content was dropped on every rewrite.
func certNonEmpty(c config.CertConfig) bool {
	return c.AutoTLS || c.Domain != "" || c.Email != "" || c.CertFile != "" ||
		c.KeyFile != "" || c.CertDir != "" || c.HTTPPort != 0 ||
		c.CertMode != "" || c.DNSProvider != "" || len(c.DNSEnv) > 0 ||
		c.CertContent != "" || c.KeyContent != ""
}

// wsNonEmpty reports whether any WS tuning field is set. The old write
// gate missed backoff_max and discovery_interval, dropping them.
func wsNonEmpty(w config.WSConfig) bool {
	return w.StatusInterval != 0 || w.HandshakeTimeout != 0 ||
		w.BackoffInitial != 0 || w.BackoffMax != 0 || w.DiscoveryInterval != 0
}

// kernelNonEmpty reports whether any top-level kernel field is set.
func kernelNonEmpty(k config.KernelConfig) bool {
	return k.Type != "" || k.ConfigDir != "" || k.LogLevel != "" ||
		k.GeoDataDir != "" || k.CustomConfig != "" ||
		len(k.CustomRoute) > 0 || len(k.CustomOutbound) > 0
}

func writeRootConfig(path string, root *config.RootConfig) error {
	instances := root.Instances
	if len(instances) == 0 && root.Config.Panel.URL != "" {
		instances = []config.Config{root.Config}
	}
	out := fileRootConfig{Instances: make([]fileInstance, 0, len(instances))}

	// Preserve top-level shared settings so instances can inherit them.
	p := &root.Config
	if p.Log.Level != "" || p.Log.Output != "" {
		out.Log = &fileLogConfig{Level: p.Log.Level, Output: p.Log.Output}
	}
	if kernelNonEmpty(p.Kernel) {
		// Write the full kernel block, not just type/log_level:
		// custom_config (and geo_data_dir / custom_route /
		// custom_outbound / config_dir) used to be dropped here.
		out.Kernel = &fileKernelConfig{
			Type:         p.Kernel.Type,
			ConfigDir:    p.Kernel.ConfigDir,
			LogLevel:     p.Kernel.LogLevel,
			GeoDataDir:   p.Kernel.GeoDataDir,
			CustomConfig: p.Kernel.CustomConfig,
			CustomRoute:  p.Kernel.CustomRoute,
			CustomOut:    p.Kernel.CustomOutbound,
		}
	}
	if p.Node.PushInterval != 0 || p.Node.PullInterval != 0 || p.Node.TrackInterval != 0 || p.Node.DeviceReportInterval != 0 {
		out.Node = &fileNodeConfig{
			PushInterval:         p.Node.PushInterval,
			PullInterval:         p.Node.PullInterval,
			TrackInterval:        p.Node.TrackInterval,
			DeviceReportInterval: p.Node.DeviceReportInterval,
		}
	}
	if p.Runtime.GoMemLimit != "" || p.Runtime.GoGCPercent != 0 {
		out.Runtime = &fileRuntimeConfig{GoMemLimit: p.Runtime.GoMemLimit, GoGCPercent: p.Runtime.GoGCPercent}
	}
	if wsNonEmpty(p.WS) {
		wsCopy := p.WS
		out.WS = &wsCopy
	}
	if certNonEmpty(p.Cert) {
		certCopy := p.Cert
		out.Cert = &certCopy
	}
	out.HealthPort = p.HealthPort

	for _, inst := range instances {
		fi := fileInstance{
			ID: inst.InstanceID,
			Panel: filePanelConfig{
				URL:      inst.Panel.URL,
				TokenEnv: inst.Panel.TokenEnv,
				NodeID:   inst.Panel.NodeID,
				NodeType: inst.Panel.NodeType,
			},
			Kernel: fileKernelConfig{
				Type:         inst.Kernel.Type,
				ConfigDir:    inst.Kernel.ConfigDir,
				LogLevel:     inst.Kernel.LogLevel,
				GeoDataDir:   inst.Kernel.GeoDataDir,
				CustomConfig: inst.Kernel.CustomConfig,
				CustomRoute:  inst.Kernel.CustomRoute,
				CustomOut:    inst.Kernel.CustomOutbound,
			},
			Log: fileLogConfig{
				Level:  inst.Log.Level,
				Output: inst.Log.Output,
			},
			HealthPort: inst.HealthPort,
		}
		if !inst.IsMachineMode() && (inst.Node.PushInterval != 0 || inst.Node.PullInterval != 0 || inst.Node.TrackInterval != 0 || inst.Node.DeviceReportInterval != 0) {
			fi.Node = &fileNodeConfig{
				PushInterval:         inst.Node.PushInterval,
				PullInterval:         inst.Node.PullInterval,
				TrackInterval:        inst.Node.TrackInterval,
				DeviceReportInterval: inst.Node.DeviceReportInterval,
			}
		}
		if inst.Runtime.GoMemLimit != "" || inst.Runtime.GoGCPercent != 0 {
			fi.Runtime = &fileRuntimeConfig{
				GoMemLimit:  inst.Runtime.GoMemLimit,
				GoGCPercent: inst.Runtime.GoGCPercent,
			}
		}
		if inst.IsMachineMode() && inst.Machine != nil {
			fi.Machine = &fileMachineConfig{
				MachineID: inst.Machine.MachineID,
				TokenEnv:  inst.Machine.TokenEnv,
			}
			fi.Panel.NodeID = 0
			fi.Panel.NodeType = ""
		}
		// Standalone round-trips verbatim: the field used to be
		// declared but never assigned here, so every rewrite silently
		// turned a standalone instance back into a panel instance.
		fi.Standalone = inst.Standalone
		// The instance cert block is written in full whenever any
		// cert field is set — not only when mode/domain/file/
		// auto_tls is set — so email, DNS provider/env, http_port,
		// cert_dir and inline content survive a rewrite.
		if certNonEmpty(inst.Cert) {
			certCopy := inst.Cert
			fi.Cert = &certCopy
		}
		if wsNonEmpty(inst.WS) {
			wsCopy := inst.WS
			fi.WS = &wsCopy
		}
		if len(inst.Nodes) > 0 {
			fi.Nodes = inst.Nodes
		}
		out.Instances = append(out.Instances, fi)
	}
	data, err := yaml.Marshal(&out)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	return os.WriteFile(path, data, 0o600)
}

func pruneCredentialKeys(path string, removed []config.Config) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read credentials: %w", err)
	}
	removeKeys := map[string]struct{}{}
	for _, inst := range removed {
		if env := strings.TrimSpace(inst.Panel.TokenEnv); env != "" {
			removeKeys[env] = struct{}{}
		}
		if inst.Machine != nil {
			if env := strings.TrimSpace(inst.Machine.TokenEnv); env != "" {
				removeKeys[env] = struct{}{}
			}
		}
	}
	lines := strings.Split(string(data), "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		key := trimmed
		if idx := strings.Index(trimmed, "="); idx >= 0 {
			key = trimmed[:idx]
		}
		if _, ok := removeKeys[key]; ok {
			continue
		}
		kept = append(kept, line)
	}
	output := strings.Join(kept, "\n")
	if output != "" {
		output += "\n"
	}
	return os.WriteFile(path, []byte(output), 0o600)
}

func writeInstallMeta(path string, root *config.RootConfig) error {
	ver := "unknown"
	if meta, err := loadInstallMeta(path); err == nil && strings.TrimSpace(meta.Version) != "" {
		ver = meta.Version
	}
	return writeInstallMetaVersioned(path, root, ver, "")
}

func instanceMode(inst config.Config) string {
	if inst.IsMachineMode() {
		return "machine"
	}
	return "node"
}

// collectInstanceRows reads the instance list from the config file
// first and only falls back to install-meta.json when the config is
// unreadable or empty. The config file is the source of truth: the old
// meta-first order showed stale bindings (e.g. after a manual config
// edit or a failed bind) until the meta file happened to be rewritten.
// Meta remains a fallback for installs whose config cannot be parsed,
// not a preferred source.
func collectInstanceRows() ([]instanceRow, error) {
	rows, err := collectRowsFromConfig()
	if err == nil && len(rows) > 0 {
		return rows, nil
	}
	metaRows, metaErr := collectRowsFromMeta()
	if metaErr == nil && len(metaRows) > 0 {
		return metaRows, nil
	}
	if err != nil {
		return nil, err
	}
	return nil, metaErr
}

func collectRowsFromMeta() ([]instanceRow, error) {
	meta, err := loadInstallMeta(defaultMetaPath)
	if err != nil {
		return nil, err
	}
	serviceStatus := systemctlState()
	healthStatus := healthStatus()
	rows := make([]instanceRow, 0, len(meta.Instances))
	for _, inst := range meta.Instances {
		rows = append(rows, instanceRow{
			ID:      inst.ID,
			Mode:    inst.Mode,
			Panel:   inst.PanelURL,
			Target:  formatTarget(inst.NodeID, inst.MachineID),
			Service: serviceStatus,
			Health:  healthStatus,
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	return rows, nil
}

func collectRowsFromConfig() ([]instanceRow, error) {
	root, err := config.LoadRoot(defaultConfigPath)
	if err != nil {
		return nil, err
	}
	instances, err := root.NormalizeInstances()
	if err != nil {
		return nil, err
	}
	serviceStatus := systemctlState()
	healthStatus := healthStatus()
	rows := make([]instanceRow, 0, len(instances))
	for _, inst := range instances {
		mode := "node"
		if inst.IsMachineMode() {
			mode = "machine"
		}
		rows = append(rows, instanceRow{
			ID:      inst.InstanceID,
			Mode:    mode,
			Panel:   inst.Panel.URL,
			Target:  formatTarget(intPtr(inst.Panel.NodeID), machineIDPtr(inst)),
			Service: serviceStatus,
			Health:  healthStatus,
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	return rows, nil
}

func printRows(rows []instanceRow, output string) error {
	if output == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rows)
	}
	var buf bytes.Buffer
	tw := tabwriter.NewWriter(&buf, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tMODE\tPANEL\tTARGET\tSERVICE\tHEALTH")
	for _, row := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", row.ID, row.Mode, row.Panel, row.Target, row.Service, row.Health)
	}
	tw.Flush()
	_, err := fmt.Print(buf.String())
	return err
}

func systemctlState() string {
	cmd := exec.Command("systemctl", "is-active", serviceName)
	out, err := cmd.CombinedOutput()
	state := strings.TrimSpace(string(out))
	if state != "" {
		return state
	}
	if err != nil {
		return "unknown"
	}
	return state
}

func healthStatus() string {
	return instanceAwareHealth()
}

func instanceAwareHealth() string {
	// Same source-of-truth rule as collectInstanceRows: the config
	// file wins, meta is only a fallback. The effective health port
	// is the first instance's (see warnFirstInstanceOnly in
	// cmd/xboard-node: only instances[0].HealthPort is ever served),
	// so a stale meta port must not shadow it.
	port := 0
	if root, err := config.LoadRoot(defaultConfigPath); err == nil {
		if instances, normErr := root.NormalizeInstances(); normErr == nil && len(instances) > 0 {
			port = instances[0].HealthPort
		}
		// Top-level health_port is part of the config too and must
		// be consulted before any meta fallback.
		if port == 0 {
			port = root.HealthPort
		}
	}
	if port == 0 {
		if meta, err := loadInstallMeta(defaultMetaPath); err == nil {
			for _, inst := range meta.Instances {
				if inst.HealthPort > 0 {
					port = inst.HealthPort
					break
				}
			}
		}
	}
	if port == 0 {
		if root, err := loadWritableRootConfig(defaultConfigPath); err == nil {
			// Check top-level health_port first (inherited by all instances)
			if root.HealthPort > 0 {
				port = root.HealthPort
			}
			if port == 0 {
				instances, _ := root.NormalizeInstances()
				for _, inst := range instances {
					if inst.HealthPort > 0 {
						port = inst.HealthPort
						break
					}
				}
			}
		}
	}
	if port == 0 {
		return "disabled"
	}
	url := fmt.Sprintf("http://127.0.0.1:%d/healthz", port)
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return "down"
	}
	resp.Body.Close()
	if resp.StatusCode == 200 {
		return "ok"
	}
	return "down"
}

func formatTarget(nodeID *int, machineID *int) string {
	if nodeID != nil && *nodeID > 0 {
		return fmt.Sprintf("node_id=%d", *nodeID)
	}
	if machineID != nil && *machineID > 0 {
		return fmt.Sprintf("machine_id=%d", *machineID)
	}
	return ""
}

func intPtr(v int) *int {
	if v <= 0 {
		return nil
	}
	vv := v
	return &vv
}

func latestInstanceID(instances []*config.Config) string {
	if len(instances) > 0 {
		id, _ := instances[len(instances)-1].AutoInstanceID()
		return id
	}
	return ""
}

func regenerateServiceFile() error {
	unit := fmt.Sprintf(`[Unit]
Description=Xboard Node Backend
Documentation=https://github.com/zzx327317-creator/Xboard-Node
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=%s
EnvironmentFile=-%s
ExecStart=%s -c %s
Restart=always
RestartSec=5
LimitNOFILE=1048576
NoNewPrivileges=true
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
`, defaultInstallRoot, defaultCredentialsPath, defaultBinaryPath, defaultConfigPath)
	return os.WriteFile(serviceFilePath, []byte(unit), 0o644)
}

func machineIDPtr(cfg *config.Config) *int {
	if cfg.Machine == nil || cfg.Machine.MachineID <= 0 {
		return nil
	}
	vv := cfg.Machine.MachineID
	return &vv
}

// ── config subcommand ─────────────────────────────────────────────────

func runConfig(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: xbnode config <init|health-port>")
	}
	switch args[0] {
	case "init":
		return runConfigInit(args[1:])
	case "health-port":
		return runConfigHealthPort(args[1:])
	default:
		return fmt.Errorf("unknown config command: %s", args[0])
	}
}

// runConfigInit generates/merges an instance into config.yml, writes
// credentials.env and install-meta.json. It replaces the Python PY_INST,
// PY_CFG, PY_ENV, and PY_META heredoc blocks in install.sh.
//
// Output (stdout, one per line):
//
//	INSTANCE_ID=<generated-id>
//	ENV_KEY=<credential-env-var-name>
func runConfigInit(args []string) error {
	var (
		configIn       string
		configOut      string
		credentialsIn  string
		credentialsOut string
		metaPath       string
		mode           string
		panelURL       string
		nodeID         int
		nodeType       string
		machineID      int
		kernelType     string
		healthPort     int
		gomemlimit     string
		gogc           int
		installRoot    string
		token          string
		releaseVersion string
	)

	for i := 0; i < len(args); i++ {
		// Every config-init flag takes a value: a trailing flag used
		// to be silently dropped (break), so e.g. a missing --token
		// value could pass validation and still "succeed".
		if i+1 >= len(args) {
			return fmt.Errorf("missing value for %s", args[i])
		}
		switch args[i] {
		case "--config":
			i++
			configIn = args[i]
		case "--output":
			i++
			configOut = args[i]
		case "--credentials-in":
			i++
			credentialsIn = args[i]
		case "--credentials-out":
			i++
			credentialsOut = args[i]
		case "--meta":
			i++
			metaPath = args[i]
		case "--mode":
			i++
			mode = args[i]
		case "--panel-url":
			i++
			panelURL = args[i]
		case "--node-id":
			i++
			v, err := strconv.Atoi(args[i])
			if err != nil {
				return fmt.Errorf("invalid --node-id: %w", err)
			}
			nodeID = v
		case "--node-type":
			i++
			nodeType = args[i]
		case "--machine-id":
			i++
			v, err := strconv.Atoi(args[i])
			if err != nil {
				return fmt.Errorf("invalid --machine-id: %w", err)
			}
			machineID = v
		case "--kernel":
			i++
			kernelType = args[i]
		case "--health-port":
			i++
			v, err := strconv.Atoi(args[i])
			if err != nil {
				return fmt.Errorf("invalid --health-port: %w", err)
			}
			healthPort = v
		case "--gomemlimit":
			i++
			gomemlimit = args[i]
		case "--gogc":
			i++
			v, err := strconv.Atoi(args[i])
			if err != nil {
				return fmt.Errorf("invalid --gogc: %w", err)
			}
			gogc = v
		case "--install-root":
			i++
			installRoot = args[i]
		case "--token":
			i++
			token = args[i]
		case "--version":
			i++
			releaseVersion = args[i]
		default:
			// Unknown flags used to be silently skipped (and their
			// value then misparsed as the next flag), so a typo'd
			// bind flag could look successful while doing nothing.
			return fmt.Errorf("unknown config init flag: %s", args[i])
		}
	}

	if mode == "" {
		return errors.New("--mode is required (node or machine)")
	}
	if panelURL == "" {
		return errors.New("--panel-url is required")
	}
	if mode == "node" && nodeID <= 0 {
		return errors.New("--node-id is required for node mode")
	}
	if mode == "machine" && machineID <= 0 {
		return errors.New("--machine-id is required for machine mode")
	}

	// Build the new instance.
	inst := config.Config{
		Panel: config.PanelConfig{URL: panelURL},
		Kernel: config.KernelConfig{
			Type:     kernelType,
			LogLevel: "warn",
		},
		Log:        config.LogConfig{Level: "info", Output: "stdout"},
		HealthPort: healthPort,
	}

	if mode == "machine" {
		inst.Machine = &config.MachineConfig{MachineID: machineID}
	} else {
		inst.Panel.NodeID = nodeID
		if nodeType != "" {
			inst.Panel.NodeType = nodeType
		}
	}

	// Generate deterministic instance ID.
	instanceID, err := inst.AutoInstanceID()
	if err != nil {
		return fmt.Errorf("generate instance ID: %w", err)
	}
	inst.InstanceID = instanceID

	if installRoot == "" {
		installRoot = "/etc/xboard-node"
	}
	inst.Kernel.ConfigDir = filepath.Join(installRoot, "instances", instanceID)

	// Build credential env key.
	envKey := "INSTANCE_" + strings.ToUpper(strings.ReplaceAll(instanceID, "-", "_"))
	if mode == "machine" {
		envKey += "_MACHINE_TOKEN"
		inst.Machine.TokenEnv = envKey
	} else {
		envKey += "_API_KEY"
		inst.Panel.TokenEnv = envKey
	}

	if gomemlimit != "" {
		inst.Runtime.GoMemLimit = gomemlimit
	}
	if gogc > 0 {
		inst.Runtime.GoGCPercent = gogc
	}

	// Load existing config (if any).
	root := &config.RootConfig{}
	hasExisting := false
	if configIn != "" {
		if loaded, loadErr := loadWritableRootConfig(configIn); loadErr == nil {
			root = loaded
			hasExisting = len(root.Instances) > 0 || root.Config.Panel.URL != "" || root.Config.Kernel.Type != ""
		}
	}

	// Normalise existing instance IDs so dedup works correctly.
	var instances []config.Config
	if hasExisting {
		instances = normalizeRootInstances(root)
		seen := make(map[string]bool)
		deduped := make([]config.Config, 0, len(instances))
		for _, existing := range instances {
			autoID, idErr := existing.AutoInstanceID()
			if idErr == nil && autoID != "" {
				existing.InstanceID = autoID
			}
			id := existing.InstanceID
			if id != "" && seen[id] {
				continue
			}
			if id != "" {
				seen[id] = true
			}
			deduped = append(deduped, existing)
		}
		instances = deduped
	}

	// Merge: replace if same ID exists, otherwise append.
	// A same-ID rebind used to replace the whole instance with a fresh
	// one, wiping operator customisation (health port, custom_config /
	// custom_route, cert, node/WS intervals, log level, config_dir).
	// Now a rebind starts from the existing instance and only updates
	// binding-related fields (panel URL, token env, node/machine ID,
	// node type when given, kernel when given, health port / runtime
	// tuning when explicitly given); everything else is preserved.
	replaced := false
	for i, existing := range instances {
		if existing.InstanceID == instanceID {
			merged := existing
			merged.Panel.URL = panelURL
			merged.Panel.Token = ""
			if mode == "machine" {
				if merged.Machine == nil {
					merged.Machine = &config.MachineConfig{}
				}
				merged.Machine.MachineID = machineID
				merged.Machine.Token = ""
				merged.Machine.TokenEnv = envKey
			} else {
				merged.Panel.NodeID = nodeID
				if nodeType != "" {
					merged.Panel.NodeType = nodeType
				}
				merged.Panel.TokenEnv = envKey
			}
			if kernelType != "" {
				merged.Kernel.Type = kernelType
			}
			if healthPort > 0 {
				merged.HealthPort = healthPort
			}
			if gomemlimit != "" {
				merged.Runtime.GoMemLimit = gomemlimit
			}
			if gogc > 0 {
				merged.Runtime.GoGCPercent = gogc
			}
			instances[i] = merged
			replaced = true
			break
		}
	}
	if !replaced {
		instances = append(instances, inst)
	}
	root.Instances = instances

	// Write config.
	if configOut == "" {
		configOut = configIn
	}
	if configOut == "" {
		return errors.New("--output (or --config) is required")
	}
	if err := writeRootConfig(configOut, root); err != nil {
		return err
	}

	// Write credentials.
	if token != "" && credentialsOut != "" {
		if err := mergeCredentials(credentialsIn, credentialsOut, envKey, token); err != nil {
			return err
		}
	}

	// Write install-meta.json.
	if metaPath != "" {
		if err := writeInstallMetaVersioned(metaPath, root, releaseVersion, instanceID); err != nil {
			return err
		}
	}

	// Output for bash capture.
	fmt.Printf("INSTANCE_ID=%s\n", instanceID)
	fmt.Printf("ENV_KEY=%s\n", envKey)
	return nil
}

// mergeCredentials reads existing key=value credentials, adds/replaces
// the given key, and writes the result preserving insertion order.
func mergeCredentials(srcPath, dstPath, key, value string) error {
	entries := make(map[string]string)
	var order []string
	if srcPath != "" {
		data, err := os.ReadFile(srcPath)
		if err == nil {
			for _, raw := range strings.Split(string(data), "\n") {
				line := strings.TrimSpace(raw)
				if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
					continue
				}
				k, v, _ := strings.Cut(line, "=")
				if _, exists := entries[k]; !exists {
					order = append(order, k)
				}
				entries[k] = v
			}
		}
	}
	if _, exists := entries[key]; !exists {
		order = append(order, key)
	}
	entries[key] = value

	var buf strings.Builder
	for _, k := range order {
		fmt.Fprintf(&buf, "%s=%s\n", k, entries[k])
	}
	return os.WriteFile(dstPath, []byte(buf.String()), 0o600)
}

// writeInstallMetaVersioned writes install-meta.json with an explicit
// version and latest-instance-ID. When latestID is empty, the last
// instance in the config is used (backward compat with writeInstallMeta).
// The old loop assigned latestID on the first iteration and never
// overwrote it, so it silently meant the FIRST instance despite this
// comment; it is now resolved after the loop to the last item.
func writeInstallMetaVersioned(path string, root *config.RootConfig, ver, latestID string) error {
	if ver == "" {
		ver = "unknown"
	}
	instances := normalizeRootInstances(root)
	items := make([]instanceSummary, 0, len(instances))
	for _, inst := range instances {
		id, err := inst.AutoInstanceID()
		if err != nil {
			return err
		}
		item := instanceSummary{
			ID:         id,
			PanelURL:   inst.Panel.URL,
			Mode:       instanceMode(inst),
			NodeID:     intPtr(inst.Panel.NodeID),
			MachineID:  machineIDPtr(&inst),
			HealthPort: inst.HealthPort,
		}
		if item.Mode == "machine" {
			item.NodeID = nil
		}
		items = append(items, item)
	}
	if latestID == "" && len(items) > 0 {
		latestID = items[len(items)-1].ID
	}
	meta := installMeta{
		ConfigMode:       "instances",
		Version:          ver,
		LatestInstanceID: latestID,
		InstanceCount:    len(items),
		Instances:        items,
		UpdatedAt:        time.Now().UTC().Format("2006-01-02T15:04:05Z"),
	}
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal install meta: %w", err)
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o644)
}

// runConfigHealthPort reads health_port from an existing config file and
// prints it to stdout. Exits silently if the file does not exist or has
// no health_port.
func runConfigHealthPort(args []string) error {
	cfgPath := defaultConfigPath
	for i := 0; i < len(args); i++ {
		if args[i] == "--config" && i+1 < len(args) {
			i++
			cfgPath = args[i]
		}
	}
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return nil // file not found → no health port
	}
	root := &config.RootConfig{}
	if err := yaml.Unmarshal(data, root); err != nil {
		return nil
	}
	// Check instances first, then legacy top-level.
	if len(root.Instances) > 0 {
		for _, inst := range root.Instances {
			if inst.HealthPort > 0 {
				fmt.Println(inst.HealthPort)
				return nil
			}
		}
	}
	if root.Config.HealthPort > 0 {
		fmt.Println(root.Config.HealthPort)
	}
	return nil
}
