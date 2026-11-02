// 中文交互菜单：不带参数运行 xbnode（或运行 xbnode menu）时打开。
//
// 设计原则：
//   - 菜单只做“收集输入 + 转交已有实现”，不另起一套重复逻辑。状态、
//     列表、服务启停、日志、绑定、更新、卸载全部直接调用 main.go 里
//     已有的 runStatus / runList / runService / runBindAdd /
//     runUpgrade / runUninstall；唯一例外是切换内核，原来没有对应的
//     子命令，所以这里新加了 switchKernelAt，并同时注册为
//     `xbnode kernel` 子命令，供菜单和命令行共同复用。
//   - 非交互环境（stdin 不是终端，或读到 EOF）绝不死等输入：
//     run() 在入口处判断，菜单循环读到 EOF 也直接退出。
//   - 面板 token 只做输入校验，确认页和日志里一律不回显原文。

package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// menuRestartService 是切换内核后重启服务的操作，抽成变量只为让
// 测试可以替换它；正常运行时就是 systemctl restart。
var menuRestartService = func() error {
	return runCommand("systemctl", "restart", serviceName)
}

// stdinIsTerminal 判断标准输入是否为终端。管道、重定向和测试中的
// bytes.Reader 都不是终端，此时菜单不应等待交互输入。
func stdinIsTerminal() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// runMenu 是 bare xbnode / xbnode menu 的入口：非终端直接打印中文
// 提示后退出，终端则进入交互循环。
func runMenu() error {
	if !stdinIsTerminal() {
		printMenuNonInteractiveHint()
		return nil
	}
	return runMenuLoop(bufio.NewReader(os.Stdin), os.Stdout)
}

// printMenuNonInteractiveHint 用于非交互环境：不读输入，只告诉用户
// 怎么用交互菜单和各子命令。
func printMenuNonInteractiveHint() {
	fmt.Println("xbnode 交互菜单需要终端。")
	fmt.Println("请在终端里直接运行 xbnode（不带参数）打开中文交互菜单，")
	fmt.Println("或使用子命令，例如：xbnode status、xbnode list、xbnode service restart、xbnode bind-node、xbnode upgrade。")
	fmt.Println("运行 xbnode help 查看全部子命令。")
}

func printMenu(out io.Writer) {
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "==================== Xboard-Node 管理菜单 ====================")
	fmt.Fprintln(out, "  首次安装请使用 install.sh；本菜单用于已安装后的日常管理。")
	fmt.Fprintln(out, "  支持系统：Ubuntu、Debian、CentOS、RHEL、Rocky、AlmaLinux、Fedora")
	fmt.Fprintln(out, "  支持架构：amd64、arm64（需要 systemd）")
	fmt.Fprintln(out, "------------------------------------------------------------")
	// 序号右对齐（%2d）+ 标题按显示宽度补空格：中文算 2 列，手工数空格
	// 会在两位数序号处错位（10/11/12 的标题右移一列），第 12 项的描述
	// 列也曾差一列。这里统一算列宽，标题列固定 19 列，描述列固定 25 列。
	menuItems := []struct {
		num   int
		title string
		desc  string
	}{
		{1, "安装/绑定节点", "填写面板地址、节点密钥和节点 ID，完成首次绑定"},
		{2, "绑定机器", "填写面板地址、机器密钥和机器 ID，绑定一台机器"},
		{3, "启动服务", "启动 xboard-node systemd 服务"},
		{4, "停止服务", "停止 xboard-node systemd 服务（需确认）"},
		{5, "重启服务", "重启 xboard-node systemd 服务（需确认）"},
		{6, "查看服务状态", "服务、健康检查与实例一览"},
		{7, "查看实例列表", "列出已绑定的节点/机器实例"},
		{8, "查看实时日志", "跟随输出服务日志，按 Ctrl+C 退出（将结束菜单程序）"},
		{9, "查看版本", "显示 xbnode 版本"},
		{10, "切换内核", "在 sing-box 与 xray 之间切换（需确认并重启）"},
		{11, "更新到最新版", "从发布页下载最新版并替换二进制（需确认）"},
		{12, "卸载", "停止并删除服务与二进制，可选清除配置（需确认）"},
		{0, "退出菜单", ""},
	}
	for _, it := range menuItems {
		if it.desc == "" {
			fmt.Fprintf(out, "  %2d. %s\n", it.num, it.title)
			continue
		}
		fmt.Fprintf(out, "  %2d. %s%s\n", it.num, padDisplay(it.title, 19), it.desc)
	}
	fmt.Fprintln(out, "=============================================================")
	status, autostart := serviceStatusLine()
	color := menuOutputIsTerminal(out)
	fmt.Fprintf(out, "xboard-node 状态：%s\n", colorizeStatusValue(status, color))
	fmt.Fprintf(out, "是否开机自启：%s\n", colorizeAutostartValue(autostart, color))
	fmt.Fprint(out, "请输入选项编号 [0-12]：")
}

// displayWidth 返回字符串在终端里占的列数：中文及全角字符算 2 列，
// 其余算 1 列。按字节或 rune 数补空格都会在中文标题处错位。
func displayWidth(s string) int {
	w := 0
	for _, r := range s {
		if isWideRune(r) {
			w += 2
		} else {
			w++
		}
	}
	return w
}

// isWideRune 判断一个 rune 是否为宽字符（东亚宽字符与全角字符）。
func isWideRune(r rune) bool {
	return (r >= 0x1100 && r <= 0x115f) || r == 0x2329 || r == 0x232a ||
		(r >= 0x2e80 && r <= 0xa4cf) || (r >= 0xac00 && r <= 0xd7a3) ||
		(r >= 0xf900 && r <= 0xfaff) || (r >= 0xfe10 && r <= 0xfe19) ||
		(r >= 0xfe30 && r <= 0xfe6f) || (r >= 0xff00 && r <= 0xff60) ||
		(r >= 0xffe0 && r <= 0xffe6) || (r >= 0x20000 && r <= 0x2fffd) ||
		(r >= 0x30000 && r <= 0x3fffd)
}

// padDisplay 在字符串右侧补空格，使其显示宽度达到 width；
// 已达到或超过时原样返回。
func padDisplay(s string, width int) string {
	if d := width - displayWidth(s); d > 0 {
		return s + strings.Repeat(" ", d)
	}
	return s
}

// formatServiceStatus 把 systemctl is-active 的原始状态翻译成菜单里
// 显示的中文状态。
func formatServiceStatus(state string) string {
	switch state {
	case "active":
		return "已运行"
	case "failed":
		return "异常"
	case "activating":
		return "启动中"
	case "deactivating":
		return "停止中"
	case "unknown", "":
		return "未知"
	default:
		return "未运行"
	}
}

// menuOutputIsTerminal 判断菜单输出是否写到终端：只有真终端才上色，
// 重定向到文件或测试里的 buffer 不带转义码，免得污染输出和对齐计算。
func menuOutputIsTerminal(out io.Writer) bool {
	f, ok := out.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// colorizeStatusValue 给运行状态值上色：开（已运行）绿，关（未运行、
// 异常）红，启动/停止中黄，未知不着色。color 为 false 时原样返回。
func colorizeStatusValue(status string, color bool) string {
	if !color {
		return status
	}
	switch status {
	case "已运行":
		return "\x1b[32m" + status + "\x1b[0m"
	case "未运行", "异常":
		return "\x1b[31m" + status + "\x1b[0m"
	case "启动中", "停止中":
		return "\x1b[33m" + status + "\x1b[0m"
	default:
		return status
	}
}

// colorizeAutostartValue 给开机自启值上色：是绿，否红。
func colorizeAutostartValue(autostart string, color bool) string {
	if !color {
		return autostart
	}
	if autostart == "是" {
		return "\x1b[32m是\x1b[0m"
	}
	return "\x1b[31m否\x1b[0m"
}

// serviceStatusLine 查询服务的运行状态与开机自启状态，供菜单底部展示。
// systemctl 不可用或查询失败时分别显示未知与否，不阻塞菜单渲染。
func serviceStatusLine() (status, autostart string) {
	status = formatServiceStatus(systemctlState())
	if err := exec.Command("systemctl", "is-enabled", serviceName).Run(); err == nil {
		autostart = "是"
	} else {
		autostart = "否"
	}
	return status, autostart
}

// runMenuLoop 是可测试的菜单循环：输入输出均可注入，读到 EOF 即退出。
func runMenuLoop(in *bufio.Reader, out io.Writer) error {
	// systemd 是硬要求：没有 systemctl 时先警告，菜单仍可打开查看，
	// 但启停/重启/日志等操作会失败。
	if _, err := exec.LookPath("systemctl"); err != nil {
		fmt.Fprintln(out, "警告：未找到 systemctl，本程序需要 systemd，服务管理类操作将不可用。")
	}
	for {
		printMenu(out)
		choice, err := readLine(in)
		if err != nil {
			// EOF（Ctrl+D 或输入流结束）：安静退出，不报错、不死循环。
			if errors.Is(err, io.EOF) {
				fmt.Fprintln(out, "已退出菜单。")
				return nil
			}
			return err
		}
		switch choice {
		case "0", "q", "Q", "退出", "exit":
			fmt.Fprintln(out, "已退出菜单。")
			return nil
		case "1":
			if err := menuBindNode(in, out); err != nil {
				fmt.Fprintf(out, "操作失败：%v\n", err)
			}
		case "2":
			if err := menuBindMachine(in, out); err != nil {
				fmt.Fprintf(out, "操作失败：%v\n", err)
			}
		case "3":
			if err := runService([]string{"start"}); err != nil {
				fmt.Fprintf(out, "操作失败：%v\n", err)
			} else {
				fmt.Fprintln(out, "服务已启动。")
			}
		case "4":
			if ok, err := confirm(in, out, "确认停止 xboard-node 服务吗？停止后所有节点将离线"); err != nil {
				return err
			} else if !ok {
				fmt.Fprintln(out, "已取消。")
			} else if err := runService([]string{"stop"}); err != nil {
				fmt.Fprintf(out, "操作失败：%v\n", err)
			} else {
				fmt.Fprintln(out, "服务已停止。")
			}
		case "5":
			if ok, err := confirm(in, out, "确认重启 xboard-node 服务吗？重启期间现有连接会中断"); err != nil {
				return err
			} else if !ok {
				fmt.Fprintln(out, "已取消。")
			} else if err := runService([]string{"restart"}); err != nil {
				fmt.Fprintf(out, "操作失败：%v\n", err)
			} else {
				fmt.Fprintln(out, "服务已重启。")
			}
		case "6":
			if err := runStatus(); err != nil {
				fmt.Fprintf(out, "操作失败：%v\n", err)
			}
		case "7":
			if err := runList(nil); err != nil {
				fmt.Fprintf(out, "操作失败：%v\n", err)
			}
		case "8":
			// Honest wording: this process installs no SIGINT
			// handler, so Ctrl+C kills xbnode itself instead of
			// returning to the menu. The old "回到菜单" text
			// promised behaviour the program never had.
			fmt.Fprintln(out, "正在跟随服务日志，按 Ctrl+C 退出日志查看（将结束菜单程序）……")
			if err := runService([]string{"logs"}); err != nil {
				fmt.Fprintf(out, "查看日志结束：%v\n", err)
			}
		case "9":
			if err := run([]string{"version"}); err != nil {
				fmt.Fprintf(out, "操作失败：%v\n", err)
			}
		case "10":
			if err := menuSwitchKernel(in, out); err != nil {
				fmt.Fprintf(out, "操作失败：%v\n", err)
			}
		case "11":
			if ok, err := confirm(in, out, "确认更新到最新版吗？更新会替换二进制并重启服务，失败时自动回滚"); err != nil {
				return err
			} else if !ok {
				fmt.Fprintln(out, "已取消。")
			} else if err := runUpgrade(nil); err != nil {
				fmt.Fprintf(out, "操作失败：%v\n", err)
			}
		case "12":
			if err := menuUninstall(in, out); err != nil {
				fmt.Fprintf(out, "操作失败：%v\n", err)
			}
		default:
			fmt.Fprintln(out, "无效的选择，请输入 0-12 的编号。")
			continue
		}
		if err := pauseForMenu(in, out); err != nil {
			// pauseForMenu uses io.EOF as its "exit the menu"
			// sentinel (EOF on stdin, or q at the pause prompt).
			// Treat it as a clean exit, like the option-read EOF
			// path above: returning it would print "EOF" on
			// stderr and exit 1, while the option path exits 0.
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

// pauseForMenu 在每次操作后等一次回车再回到菜单；读到 EOF 则退出。
func pauseForMenu(in *bufio.Reader, out io.Writer) error {
	fmt.Fprint(out, "按回车返回主菜单（直接输入 q 退出）：")
	line, err := readLine(in)
	if err != nil {
		if errors.Is(err, io.EOF) {
			fmt.Fprintln(out, "已退出菜单。")
			return io.EOF
		}
		return err
	}
	if line == "q" || line == "Q" || line == "退出" {
		fmt.Fprintln(out, "已退出菜单。")
		return io.EOF
	}
	return nil
}

// readLine 读一行并去掉首尾空白；空行返回 ""，输入流结束返回 EOF。
func readLine(in *bufio.Reader) (string, error) {
	line, err := in.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	line = strings.TrimSpace(line)
	if errors.Is(err, io.EOF) && line == "" {
		return "", io.EOF
	}
	return line, nil
}

// promptString 循环提示直到拿到合法输入；required 为 false 时空行
// 表示接受默认值 def（def 为 "" 则表示跳过该项）。
func promptString(in *bufio.Reader, out io.Writer, prompt string, required bool, def string, validate func(string) error) (string, error) {
	for {
		if def != "" {
			fmt.Fprintf(out, "%s（默认：%s，直接回车使用默认）：", prompt, def)
		} else {
			fmt.Fprintf(out, "%s：", prompt)
		}
		v, err := readLine(in)
		if err != nil {
			return "", err
		}
		if v == "" {
			if def != "" {
				return def, nil
			}
			if !required {
				return "", nil
			}
			fmt.Fprintln(out, "该项不能为空，请重新输入。")
			continue
		}
		if validate != nil {
			if err := validate(v); err != nil {
				fmt.Fprintf(out, "输入无效：%v，请重新输入。\n", err)
				continue
			}
		}
		return v, nil
	}
}

// promptPositiveInt 提示输入一个正整数。
func promptPositiveInt(in *bufio.Reader, out io.Writer, prompt string) (int, error) {
	for {
		v, err := promptString(in, out, prompt, true, "", nil)
		if err != nil {
			return 0, err
		}
		n, err := parsePositiveInt(v, prompt)
		if err != nil {
			fmt.Fprintf(out, "输入无效：%v，请重新输入。\n", err)
			continue
		}
		return n, nil
	}
}

// confirm 请用户确认，y/Y/yes/是 表示确认，其他一律视为取消。
func confirm(in *bufio.Reader, out io.Writer, msg string) (bool, error) {
	fmt.Fprintf(out, "%s [y/N]：", msg)
	v, err := readLine(in)
	if err != nil {
		return false, err
	}
	switch strings.ToLower(v) {
	case "y", "yes", "是":
		return true, nil
	default:
		return false, nil
	}
}

// validatePanelURL 校验面板地址：必须是 http/https 且带 host；
// http 只警告不明文风险，不拒绝（与 B7 的警告策略一致，现有 http
// 面板用户不受影响）。
func validatePanelURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return errors.New("面板地址格式不正确，例如 https://panel.example.com")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("面板地址必须以 http:// 或 https:// 开头")
	}
	return nil
}

// normalizeKernel 接受 singbox、sing-box、xray，统一为配置里使用的
// singbox / xray；其他值报错。
func normalizeKernel(raw string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "singbox", "sing-box":
		return "singbox", nil
	case "xray":
		return "xray", nil
	default:
		return "", fmt.Errorf("内核只能是 singbox 或 xray，得到 %q", raw)
	}
}

// promptKernel 让用户二选一，默认 singbox。
func promptKernel(in *bufio.Reader, out io.Writer) (string, error) {
	fmt.Fprintln(out, "请选择内核：1) sing-box（默认）  2) xray")
	v, err := promptString(in, out, "请输入 1 或 2", false, "1", nil)
	if err != nil {
		return "", err
	}
	switch v {
	case "1", "singbox", "sing-box":
		return "singbox", nil
	case "2", "xray":
		return "xray", nil
	default:
		return "", fmt.Errorf("无效的内核选择 %q", v)
	}
}

// promptPanelAndToken 收集面板地址和密钥；密钥只校验非空，
// 从不在任何输出里回显原文。tokenLabel 区分密钥种类：节点绑定用
// “节点密钥”，机器绑定用“机器密钥”，避免与面板通信密钥混淆。
func promptPanelAndToken(in *bufio.Reader, out io.Writer, tokenLabel string) (string, string, error) {
	panelURL, err := promptString(in, out, "请输入面板地址（例如 https://panel.example.com）", true, "", validatePanelURL)
	if err != nil {
		return "", "", err
	}
	if u, err := url.Parse(panelURL); err == nil && u.Scheme == "http" {
		fmt.Fprintf(out, "提示：面板地址使用了明文 http，%s在传输中可见，建议改用 https。\n", tokenLabel)
	}
	fmt.Fprintf(out, "请输入%s（输入时会在屏幕上回显，输完请注意清屏）：\n", tokenLabel)
	token, err := promptString(in, out, tokenLabel, true, "", nil)
	if err != nil {
		return "", "", err
	}
	return panelURL, token, nil
}

// menuBindNode：安装/绑定节点，复用 runBindAdd（即 xbnode bind-node）。
func menuBindNode(in *bufio.Reader, out io.Writer) error {
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		return fmt.Errorf("不支持的架构 %s，仅支持 amd64/arm64", runtime.GOARCH)
	}
	panelURL, token, err := promptPanelAndToken(in, out, "节点密钥")
	if err != nil {
		return err
	}
	nodeID, err := promptPositiveInt(in, out, "请输入节点 ID（面板里的节点编号）")
	if err != nil {
		return err
	}
	nodeType, err := promptString(in, out, "请输入节点类型（可空，如 vless/vmess/trojan/shadowsocks，直接回车跳过）", false, "", nil)
	if err != nil {
		return err
	}
	kernel, err := promptKernel(in, out)
	if err != nil {
		return err
	}
	fmt.Fprintln(out, "即将绑定节点，信息如下（密钥不回显）：")
	fmt.Fprintf(out, "  面板地址：%s\n  节点 ID：%d\n  节点类型：%s\n  内核：%s\n",
		panelURL, nodeID, ifEmpty(nodeType, "（面板默认）"), kernel)
	ok, err := confirm(in, out, "确认绑定吗？绑定后服务会自动重启")
	if err != nil {
		return err
	}
	if !ok {
		fmt.Fprintln(out, "已取消。")
		return nil
	}
	args := []string{"--panel-url", panelURL, "--token", token, "--node-id", fmt.Sprint(nodeID), "--kernel", kernel}
	if nodeType != "" {
		args = append(args, "--node-type", nodeType)
	}
	return runBindAdd("node", args)
}

// menuBindMachine：绑定机器，复用 runBindAdd（即 xbnode bind-machine）。
func menuBindMachine(in *bufio.Reader, out io.Writer) error {
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		return fmt.Errorf("不支持的架构 %s，仅支持 amd64/arm64", runtime.GOARCH)
	}
	panelURL, token, err := promptPanelAndToken(in, out, "机器密钥")
	if err != nil {
		return err
	}
	machineID, err := promptPositiveInt(in, out, "请输入机器 ID（面板里的机器编号）")
	if err != nil {
		return err
	}
	kernel, err := promptKernel(in, out)
	if err != nil {
		return err
	}
	fmt.Fprintln(out, "即将绑定机器，信息如下（密钥不回显）：")
	fmt.Fprintf(out, "  面板地址：%s\n  机器 ID：%d\n  内核：%s\n", panelURL, machineID, kernel)
	ok, err := confirm(in, out, "确认绑定吗？绑定后服务会自动重启")
	if err != nil {
		return err
	}
	if !ok {
		fmt.Fprintln(out, "已取消。")
		return nil
	}
	return runBindAdd("machine", []string{"--panel-url", panelURL, "--token", token, "--machine-id", fmt.Sprint(machineID), "--kernel", kernel})
}

// menuSwitchKernel：切换内核，复用 switchKernelAt（即 xbnode kernel）。
func menuSwitchKernel(in *bufio.Reader, out io.Writer) error {
	root, err := loadWritableRootConfig(defaultConfigPath)
	if err != nil {
		return fmt.Errorf("读取配置文件失败（尚未绑定实例请先用菜单 1/2 绑定）：%w", err)
	}
	instances := normalizeRootInstances(root)
	if len(instances) == 0 {
		return errors.New("尚未配置任何实例，请先用菜单 1/2 绑定节点或机器")
	}
	fmt.Fprintln(out, "当前实例：")
	for _, inst := range instances {
		id, _ := inst.AutoInstanceID()
		if id == "" {
			id = inst.InstanceID
		}
		fmt.Fprintf(out, "  实例：%s  模式：%s  面板：%s  当前内核：%s\n",
			id, instanceMode(inst), inst.Panel.URL, inst.Kernel.Type)
	}
	var instanceID string
	if len(instances) == 1 {
		id, _ := instances[0].AutoInstanceID()
		if id == "" {
			id = instances[0].InstanceID
		}
		instanceID, err = promptString(in, out, "请输入实例 ID", false, id, nil)
	} else {
		instanceID, err = promptString(in, out, "请输入要切换的实例 ID", true, "", nil)
	}
	if err != nil {
		return err
	}
	kernel, err := promptKernel(in, out)
	if err != nil {
		return err
	}
	ok, err := confirm(in, out, fmt.Sprintf("确认把实例 %s 的内核切换为 %s 吗？切换后服务会自动重启", instanceID, kernel))
	if err != nil {
		return err
	}
	if !ok {
		fmt.Fprintln(out, "已取消。")
		return nil
	}
	changed, err := switchKernelAt(defaultConfigPath, instanceID, kernel, menuRestartService)
	if err != nil {
		return err
	}
	if !changed {
		fmt.Fprintf(out, "实例 %s 已经是 %s 内核，无需切换。\n", instanceID, kernel)
		return nil
	}
	fmt.Fprintf(out, "实例 %s 已切换为 %s 内核，服务已重启。\n", instanceID, kernel)
	return nil
}

// menuUninstall：卸载，复用 runUninstall；是否清除配置先问清楚，
// 再做一次最终确认，之后 runUninstall 以 --yes 跳过它自己的提问。
func menuUninstall(in *bufio.Reader, out io.Writer) error {
	purge, err := confirm(in, out, "是否同时清除配置文件和数据（/etc/xboard-node）？选否仅删除服务与二进制，配置会保留")
	if err != nil {
		return err
	}
	if purge {
		fmt.Fprintln(out, "警告：清除后所有绑定配置和密钥都将丢失，如需重装要重新绑定。")
	} else {
		fmt.Fprintln(out, "配置将保留在 /etc/xboard-node，重装后可继续使用。")
	}
	ok, err := confirm(in, out, "确认卸载 xboard-node 吗？此操作不可撤销")
	if err != nil {
		return err
	}
	if !ok {
		fmt.Fprintln(out, "已取消。")
		return nil
	}
	args := []string{"--yes"}
	if purge {
		args = append(args, "--purge")
	}
	return runUninstall(args)
}

// runKernel 是 `xbnode kernel` 子命令：无参数列出各实例内核，
// 带参数切换指定实例的内核。
//
//	xbnode kernel
//	xbnode kernel <instance-id> <singbox|xray>
func runKernel(args []string) error {
	if len(args) == 0 {
		root, err := loadWritableRootConfig(defaultConfigPath)
		if err != nil {
			return err
		}
		instances := normalizeRootInstances(root)
		if len(instances) == 0 {
			return errors.New("no instances configured")
		}
		for _, inst := range instances {
			id, _ := inst.AutoInstanceID()
			if id == "" {
				id = inst.InstanceID
			}
			fmt.Printf("%s\t%s\t%s\n", id, instanceMode(inst), inst.Kernel.Type)
		}
		return nil
	}
	if len(args) != 2 {
		return errors.New("usage: xbnode kernel <instance-id> <singbox|xray>")
	}
	if err := ensureRoot("kernel"); err != nil {
		return err
	}
	kernel, err := normalizeKernel(args[1])
	if err != nil {
		return err
	}
	changed, err := switchKernelAt(defaultConfigPath, args[0], kernel, menuRestartService)
	if err != nil {
		return err
	}
	if !changed {
		fmt.Printf("instance %s already uses kernel %s\n", args[0], kernel)
		return nil
	}
	fmt.Printf("instance %s switched to kernel %s, service restarted\n", args[0], kernel)
	return nil
}

// switchKernelAt 改写 path 指定的配置文件里某个实例的内核类型，
// 然后调用 restart 重启服务。返回 changed 表示是否真的改了值。
// path 与 restart 可注入，便于测试（生产路径为 defaultConfigPath，
// 重启为 systemctl restart xboard-node.service）。
func switchKernelAt(path, instanceID, kernel string, restart func() error) (bool, error) {
	kernel, err := normalizeKernel(kernel)
	if err != nil {
		return false, err
	}
	root, err := loadWritableRootConfig(path)
	if err != nil {
		return false, fmt.Errorf("read config file: %w", err)
	}
	if len(root.Instances) > 0 {
		matched := -1
		if instanceID == "" && len(root.Instances) == 1 {
			matched = 0
		} else {
			for i, inst := range root.Instances {
				autoID, _ := inst.AutoInstanceID()
				if inst.InstanceID == instanceID || autoID == instanceID {
					matched = i
					break
				}
			}
		}
		if matched < 0 {
			return false, fmt.Errorf("instance not found: %s", instanceID)
		}
		if root.Instances[matched].Kernel.Type == kernel {
			return false, nil
		}
		root.Instances[matched].Kernel.Type = kernel
	} else if root.Config.Panel.URL != "" {
		// 旧版单配置模式：内核在顶层配置里。
		if root.Config.Kernel.Type == kernel {
			return false, nil
		}
		root.Config.Kernel.Type = kernel
	} else {
		return false, errors.New("no instances configured")
	}
	if err := writeRootConfig(path, root); err != nil {
		return false, err
	}
	if restart != nil {
		if err := restart(); err != nil {
			return true, fmt.Errorf("config updated but service restart failed: %w", err)
		}
	}
	return true, nil
}

func ifEmpty(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}
