#!/usr/bin/env bash
set -Eeuo pipefail

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
BOLD='\033[1m'
NC='\033[0m'

APP_NAME="xboard-node"
INSTALL_ROOT="/etc/xboard-node"
BACKUP_DIR="${INSTALL_ROOT}/backups"
INSTALL_META="${INSTALL_ROOT}/install-meta.json"
CONFIG_FILE="${INSTALL_ROOT}/config.yml"
CREDENTIALS_FILE="${INSTALL_ROOT}/credentials.env"
BINARY_PATH="/usr/local/bin/xboard-node"
SERVICE_NAME="xboard-node.service"
SERVICE_PATH="/etc/systemd/system/${SERVICE_NAME}"
CLI_PATH="/usr/local/bin/xbnode"
INSTALLER_COPY_PATH="${INSTALL_ROOT}/install.sh"
CLI_BINARY_SOURCE=""
DEFAULT_HEALTH_PORT=65530
DEFAULT_KERNEL="singbox"
DEFAULT_MODE="node"
DEFAULT_ACTION="install"
DEFAULT_RELEASE_VERSION="latest"
DEFAULT_LOG_LEVEL="info"
DEFAULT_KERNEL_LOG_LEVEL="warn"
DEFAULT_DOWNLOAD_BASE="https://github.com/emmaforge/Xboard-Node/releases"

ACTION="${DEFAULT_ACTION}"
MODE=""
PANEL_URL=""
TOKEN=""
NODE_ID=""
NODE_TYPE=""
MACHINE_ID=""
KERNEL_TYPE="${DEFAULT_KERNEL}"
RELEASE_VERSION="${DEFAULT_RELEASE_VERSION}"
HEALTH_PORT="${DEFAULT_HEALTH_PORT}"
HEALTH_ENABLED=1
RUNTIME_GOMEMLIMIT=""
RUNTIME_GOGC=""
BINARY_SOURCE=""
CLI_BINARY_SOURCE=""
FORCE_RECONFIGURE=0
PURGE=0
YES=0
INSTALL_ONLY=0
ARCH=""
OS=""
DOWNLOAD_URL=""
CURRENT_STATE="fresh"
TMP_DIR=""
BACKUP_PATH=""
SERVICE_EXISTED=0
CLEANUP_DONE=0

log_info()  { echo -e "${GREEN}[INFO]${NC} $1"; }
log_warn()  { echo -e "${YELLOW}[WARN]${NC} $1"; }
log_error() { echo -e "${RED}[ERROR]${NC} $1"; }
log_step()  { echo -e "${CYAN}[STEP]${NC} ${BOLD}$1${NC}"; }

cleanup_tmp() {
    if [ "$CLEANUP_DONE" -eq 1 ]; then
        return
    fi
    CLEANUP_DONE=1
    if [ -n "$TMP_DIR" ] && [ -d "$TMP_DIR" ]; then
        rm -rf "$TMP_DIR"
    fi
}

load_health_port_from_config() {
    local cfg_path="$1"
    if [ ! -f "$cfg_path" ]; then
        return
    fi
    local parsed
    if [ -x "$CLI_PATH" ]; then
        parsed=$("$CLI_PATH" config health-port --config "$cfg_path" 2>/dev/null)
    else
        parsed=$(grep -m1 'health_port:' "$cfg_path" 2>/dev/null | sed 's/.*health_port:[[:space:]]*//' | tr -cd '0-9')
    fi
    if [ -n "$parsed" ] && [ "$parsed" -ge 0 ] 2>/dev/null; then
        HEALTH_PORT="$parsed"
        if [ "$HEALTH_PORT" -eq 0 ]; then
            HEALTH_ENABLED=0
        else
            HEALTH_ENABLED=1
        fi
    else
        # The config exists but declares no health port: the running
        # service has no health endpoint, so checking the installer's
        # default port would fail spuriously. Treat it as disabled
        # rather than probing a port nothing listens on. (A missing
        # config file leaves the defaults untouched, per the early
        # return above.)
        HEALTH_PORT=0
        HEALTH_ENABLED=0
    fi
}

rollback_install() {
    log_warn "正在回滚安装"
    if [ -n "$BACKUP_PATH" ] && [ -d "$BACKUP_PATH" ]; then
        if [ -f "$BACKUP_PATH/xboard-node" ]; then
            install -m 755 "$BACKUP_PATH/xboard-node" "$BINARY_PATH"
        else
            rm -f "$BINARY_PATH"
        fi
        if [ -f "$BACKUP_PATH/config.yml" ]; then
            install -m 600 "$BACKUP_PATH/config.yml" "$CONFIG_FILE"
        else
            rm -f "$CONFIG_FILE"
        fi
        if [ -f "$BACKUP_PATH/credentials.env" ]; then
            install -m 600 "$BACKUP_PATH/credentials.env" "$CREDENTIALS_FILE"
        else
            rm -f "$CREDENTIALS_FILE"
        fi
        if [ -f "$BACKUP_PATH/install-meta.json" ]; then
            install -m 644 "$BACKUP_PATH/install-meta.json" "$INSTALL_META"
        else
            rm -f "$INSTALL_META"
        fi
        if [ -f "$BACKUP_PATH/xbnode" ]; then
            install -m 755 "$BACKUP_PATH/xbnode" "$CLI_PATH"
        else
            rm -f "$CLI_PATH"
        fi
        if [ -f "$BACKUP_PATH/${SERVICE_NAME}" ]; then
            install -m 644 "$BACKUP_PATH/${SERVICE_NAME}" "$SERVICE_PATH"
        else
            rm -f "$SERVICE_PATH"
        fi
    fi
    load_health_port_from_config "$CONFIG_FILE"
    systemctl daemon-reload || true
    if [ "$SERVICE_EXISTED" -eq 1 ] || [ -f "$SERVICE_PATH" ]; then
        systemctl reset-failed "$SERVICE_NAME" >/dev/null 2>&1 || true
        systemctl restart "$SERVICE_NAME" >/dev/null 2>&1 || true
        if ! wait_for_health; then
            log_error "回滚已完成，但恢复后的服务未通过健康检查"
            show_recent_logs
            return 1
        fi
    else
        systemctl disable "$SERVICE_NAME" >/dev/null 2>&1 || true
    fi
    log_warn "回滚完成"
}

on_error() {
    local exit_code=$?
    local line_no=${1:-unknown}
    if [ "$exit_code" -ne 0 ]; then
        log_error "安装失败，位置：第 ${line_no} 行（退出码 ${exit_code}）"
        if [ -n "$BACKUP_PATH" ]; then
            rollback_install || true
        fi
    fi
    cleanup_tmp
    exit "$exit_code"
}
trap 'on_error $LINENO' ERR
trap cleanup_tmp EXIT

usage() {
    cat <<'HELP'

  xboard-node 安装脚本

  操作：
    install      安装或校准已有部署（默认）
    upgrade      升级二进制文件并重启服务
    uninstall    删除已安装的服务和二进制文件（除非加 --purge，否则保留配置）
    status       显示当前安装状态
    help         显示本帮助

  两种用法（二选一）：
    只安装不绑定（先装后配，像 V2bX 一样）：
      sudo bash install.sh
      装完运行不带参数的 xbnode 打开中文交互菜单，选 1 绑定节点
      或选 2 绑定机器，按提示填写面板地址、密钥和节点/机器 ID，
      绑定后服务会自动重启。
    安装时直接绑定：绑定信息必须一次给全，见下方必填项。

  模式（不指定时按 --node-id 或 --machine-id 自动判断）：
    --mode node      面板单节点模式（默认）
    --mode machine   面板机器模式

  直接绑定时，节点模式必填：
    --panel, -a      面板地址
    --token, -t      面板服务端密钥
    --node-id, -n    节点 ID

  直接绑定时，机器模式必填：
    --panel, -a       面板地址
    --token, -t       机器密钥
    --machine-id      机器 ID

  可选：
    --node-type, -T     节点模式下显式指定节点类型
    --kernel, -k        singbox 或 xray（默认：singbox）
    --version           发布版本或 latest（默认：latest）
    --binary            使用本地 xboard-node 二进制文件路径，不从网上下载
    --xbnode-binary      使用本地 xbnode 二进制文件路径，不从网上下载
    --health-port       本地健康检查端口（默认：65530，0 表示禁用）
    --gomemlimit        运行时 GOMEMLIMIT，例如 256MiB
    --gogc              运行时 GOGC，例如 50
    --force-reconfigure 覆盖已有安装：仅保留新绑定，旧配置先备份再替换（不加则合并）
    --purge             配合 uninstall，同时删除 /etc/xboard-node
    --yes, -y           对破坏性操作做非交互确认

  示例：
    sudo bash install.sh
    xbnode
    sudo bash install.sh --panel https://panel.example.com --token TOKEN --node-id 1
    sudo bash install.sh --panel https://panel.example.com --token TOKEN --machine-id 1
    sudo bash install.sh upgrade
    sudo bash install.sh uninstall --purge --yes

  注意：绑定信息只给一部分（例如只给了面板地址、缺密钥或 ID）
  会直接报错退出，不会按一半信息安装；要么补齐绑定参数，
  要么不带绑定参数只安装、装完用 xbnode 菜单绑定。

HELP
}

# need_arg <flag>: every valued option must be followed by a value.
# With `set -u`, reading a missing $2 used to die with a cryptic
# "unbound variable" error; now the user gets a friendly message.
need_arg() {
    if [ "$#" -lt 2 ]; then
        log_error "参数 $1 缺少值：请写成 $1 <值>"
        usage
        exit 1
    fi
}

parse_args() {
    while [ $# -gt 0 ]; do
        case "$1" in
            install|upgrade|uninstall|status|help)
                ACTION="$1"
                shift
                ;;
            --mode)
                need_arg "$@"
                MODE="$2"
                shift 2
                ;;
            --panel|-a|--api)
                need_arg "$@"
                PANEL_URL="$2"
                shift 2
                ;;
            --token|-t)
                need_arg "$@"
                TOKEN="$2"
                shift 2
                ;;
            --node-id|-n)
                need_arg "$@"
                NODE_ID="$2"
                shift 2
                ;;
            --node-type|-T)
                need_arg "$@"
                NODE_TYPE="$2"
                shift 2
                ;;
            --machine-id)
                need_arg "$@"
                MACHINE_ID="$2"
                shift 2
                ;;
            --kernel|-k)
                need_arg "$@"
                KERNEL_TYPE="$2"
                shift 2
                ;;
            --version)
                need_arg "$@"
                RELEASE_VERSION="$2"
                shift 2
                ;;
            --binary)
                need_arg "$@"
                BINARY_SOURCE="$2"
                shift 2
                ;;
            --xbnode-binary)
                need_arg "$@"
                CLI_BINARY_SOURCE="$2"
                shift 2
                ;;
            --health-port)
                need_arg "$@"
                HEALTH_PORT="$2"
                shift 2
                ;;
            --gomemlimit)
                need_arg "$@"
                RUNTIME_GOMEMLIMIT="$2"
                shift 2
                ;;
            --gogc)
                need_arg "$@"
                RUNTIME_GOGC="$2"
                shift 2
                ;;
            --force-reconfigure)
                FORCE_RECONFIGURE=1
                shift
                ;;
            --purge)
                PURGE=1
                shift
                ;;
            --yes|-y)
                YES=1
                shift
                ;;
            --help|-h)
                ACTION="help"
                shift
                ;;
            *)
                # Unknown arguments used to be silently ignored (or,
                # for a lone positional, quietly reinterpreted as the
                # action). Fail loudly instead so a typo can't pass
                # unnoticed.
                log_error "未知参数：$1"
                usage
                exit 1
                ;;
        esac
    done

    case "$KERNEL_TYPE" in
        singbox|SingBox|SINGBOX) KERNEL_TYPE="singbox" ;;
        xray|Xray|XRAY) KERNEL_TYPE="xray" ;;
        *) ;;
    esac

    # Auto-detect mode from arguments when --mode is not specified.
    if [ -z "$MODE" ]; then
        if [ -n "$MACHINE_ID" ]; then
            MODE="machine"
        else
            MODE="node"
        fi
    fi

    case "$MODE" in
        node|machine) ;;
        *)
            log_error "不支持的模式：$MODE"
            usage
            exit 1
            ;;
    esac
}

check_root() {
    if [ "$(id -u)" -ne 0 ]; then
        log_error "请以 root 身份或使用 sudo 运行"
        exit 1
    fi
}

detect_arch() {
    local raw
    raw=$(uname -m)
    case "$raw" in
        x86_64|amd64) ARCH="amd64" ;;
        aarch64|arm64) ARCH="arm64" ;;
        *)
            log_error "不支持的架构：$raw"
            exit 1
            ;;
    esac
}

detect_os() {
    if [ -f /etc/os-release ]; then
        . /etc/os-release
        OS="$ID"
    else
        OS="unknown"
    fi
}

ensure_systemd() {
    if ! command -v systemctl >/dev/null 2>&1; then
        log_error "本安装脚本需要 systemd"
        exit 1
    fi
    if [ ! -d /run/systemd/system ]; then
        log_error "本机似乎未运行 systemd"
        exit 1
    fi
}

run_with_retry() {
    local attempts="$1"
    local delay="$2"
    shift 2
    local i=1
    while [ "$i" -le "$attempts" ]; do
        if "$@"; then
            return 0
        fi
        if [ "$i" -lt "$attempts" ]; then
            log_warn "命令失败，${delay} 秒后重试：$*"
            sleep "$delay"
        fi
        i=$((i + 1))
    done
    return 1
}

# required_tools_present: the tools the installer actually needs
# (curl/wget for downloads). ca-certificates has no binary to probe,
# so it is not part of this check.
required_tools_present() {
    command -v curl >/dev/null 2>&1 && command -v wget >/dev/null 2>&1
}

install_dependencies() {
    case "$OS" in
        ubuntu|debian)
            if ! { DEBIAN_FRONTEND=noninteractive run_with_retry 10 3 apt-get update -qq &&
                   DEBIAN_FRONTEND=noninteractive run_with_retry 10 3 apt-get install -y -qq curl wget ca-certificates >/dev/null 2>&1; }; then
                # Previously any apt failure was fatal (set -e), even
                # when curl/wget were already installed and nothing
                # else was needed. Now apt failure is only a warning
                # when the required tools are present; it is fatal
                # only when a required tool is actually missing.
                if required_tools_present; then
                    log_warn "安装依赖失败（apt），但 curl/wget 已齐全，继续安装"
                else
                    log_error "安装依赖失败（apt），且缺少必需工具 curl/wget，无法继续"
                    exit 1
                fi
            fi
            ;;
        centos|rhel|rocky|almalinux|fedora)
            if command -v dnf >/dev/null 2>&1; then
                install_ok=$(run_with_retry 5 3 dnf install -y -q curl wget ca-certificates >/dev/null 2>&1 && echo yes || echo no)
            else
                install_ok=$(run_with_retry 5 3 yum install -y -q curl wget ca-certificates >/dev/null 2>&1 && echo yes || echo no)
            fi
            if [ "$install_ok" != "yes" ]; then
                if required_tools_present; then
                    log_warn "安装依赖失败（包管理器），但 curl/wget 已齐全，继续安装"
                else
                    log_error "安装依赖失败（包管理器），且缺少必需工具 curl/wget，无法继续"
                    exit 1
                fi
            fi
            ;;
        *)
            log_warn "系统 ${OS} 不在官方支持列表中，将尽力继续安装"
            ;;
    esac
}

ensure_dirs() {
    mkdir -p "$INSTALL_ROOT" "$BACKUP_DIR"
    chmod 700 "$INSTALL_ROOT"
}

validate_positive_int() {
    local label="$1"
    local value="$2"
    if ! [[ "$value" =~ ^[0-9]+$ ]] || [ "$value" -le 0 ]; then
        log_error "${label} 必须是正整数，实际值：${value}"
        exit 1
    fi
}

# refuse_partial_binding <detail>: binding info must arrive either complete
# or not at all. A half-configured install (panel without token, token
# without ID, ...) is refused outright instead of being installed in a
# broken state; the user picks one of the two supported usages.
refuse_partial_binding() {
    log_error "绑定信息不完整：$1"
    log_error "正确用法（二选一）："
    log_error "  1) 补齐绑定参数后安装，例如：sudo bash install.sh --panel <面板地址> --token <密钥> --node-id <节点 ID>"
    log_error "  2) 不带绑定参数只安装二进制，装完运行 xbnode 打开中文交互菜单绑定：sudo bash install.sh"
    exit 1
}

# validate_install_request classifies the install request into three cases
# and sets INSTALL_ONLY accordingly:
#   - no binding info at all (panel, token, node ID, machine ID all empty):
#     install binaries only; binding happens later via the xbnode menu.
#   - complete binding info for the selected mode: full install as before.
#   - anything in between: refused via refuse_partial_binding.
# Kernel and health-port are validated in all three cases.
validate_install_request() {
    if ! [[ "$HEALTH_PORT" =~ ^[0-9]+$ ]]; then
        log_error "health-port 必须是非负整数"
        exit 1
    fi
    if [ "$HEALTH_PORT" -eq 0 ]; then
        HEALTH_ENABLED=0
    fi
    case "$KERNEL_TYPE" in
        singbox|xray) ;;
        *)
            log_error "内核必须是 singbox 或 xray"
            exit 1
            ;;
    esac
    if [ -z "$PANEL_URL" ] && [ -z "$TOKEN" ] && [ -z "$NODE_ID" ] && [ -z "$MACHINE_ID" ]; then
        INSTALL_ONLY=1
        return
    fi
    INSTALL_ONLY=0
    if [ -z "$PANEL_URL" ] || [ -z "$TOKEN" ]; then
        refuse_partial_binding "面板地址和密钥必须一起提供"
    fi
    case "$MODE" in
        node)
            if [ -z "$NODE_ID" ]; then
                refuse_partial_binding "节点模式需要 --node-id"
            fi
            validate_positive_int "Node ID" "$NODE_ID"
            ;;
        machine)
            if [ -z "$MACHINE_ID" ]; then
                refuse_partial_binding "机器模式需要 --machine-id"
            fi
            validate_positive_int "Machine ID" "$MACHINE_ID"
            ;;
    esac
}

detect_current_state() {
    local has_binary=0 has_config=0 has_service=0
    [ -x "$BINARY_PATH" ] && has_binary=1
    [ -f "$CONFIG_FILE" ] && has_config=1
    [ -f "$SERVICE_PATH" ] && has_service=1

    if [ "$has_binary" -eq 0 ] && [ "$has_config" -eq 0 ] && [ "$has_service" -eq 0 ]; then
        CURRENT_STATE="fresh"
    elif [ "$has_binary" -eq 1 ] && [ "$has_config" -eq 1 ] && [ "$has_service" -eq 1 ]; then
        CURRENT_STATE="installed"
    else
        CURRENT_STATE="partial"
    fi
}

select_binary_source() {
    # Only an explicitly requested local binary (--binary) is ever used.
    # Previously, a same-named file in the current working directory
    # (./xboard-node, ./xboard-node-linux-<arch>) was silently preferred
    # over the release download, so a planted file in the installer's
    # working directory would be installed without any check.
    if [ -n "$BINARY_SOURCE" ]; then
        if [ ! -f "$BINARY_SOURCE" ]; then
            log_error "未找到二进制文件来源：$BINARY_SOURCE"
            exit 1
        fi
        echo "$BINARY_SOURCE"
        return
    fi
    echo ""
}

# verify_download <url> <file>: verify a downloaded file against the
# release's published "<artifact>.sha256" sidecar, if one exists. A
# mismatch is fatal. If no sidecar is published (older releases), warn
# clearly and continue: failing hard would break installing those
# releases, but new releases (see .github/workflows/ci.yml, which now
# publishes .sha256 files) are always verified.
verify_download() {
    local url="$1" file="$2"
    local sum_file="$TMP_DIR/checksum.txt"
    if ! command -v sha256sum >/dev/null 2>&1; then
        log_warn "本机没有 sha256sum，跳过 ${url} 的校验和验证"
        return 0
    fi
    if ! curl -fsSL "${url}.sha256" -o "$sum_file" 2>/dev/null; then
        log_warn "${url} 未发布 SHA-256 校验和，跳过校验和验证"
        return 0
    fi
    local expected actual
    expected=$(awk '{print $1}' "$sum_file")
    actual=$(sha256sum "$file" | awk '{print $1}')
    if [ -z "$expected" ] || [ "$expected" != "$actual" ]; then
        log_error "${url} 的 SHA-256 校验和不匹配：期望 ${expected}，实际 ${actual}"
        rm -f "$file"
        exit 1
    fi
    log_step "SHA-256 校验通过：${file}"
}

resolve_download_url() {
    local artifact="$1"
    if [ "$RELEASE_VERSION" = "latest" ]; then
        DOWNLOAD_URL="${DEFAULT_DOWNLOAD_BASE}/latest/download/${artifact}"
    else
        DOWNLOAD_URL="${DEFAULT_DOWNLOAD_BASE}/download/${RELEASE_VERSION}/${artifact}"
    fi
}

# download_artifact <url> <out> <artifact>: download a release artifact.
# When the requested version is "latest" but the fork has no version tag
# yet, GitHub's /latest endpoint 404s (CI only marks tag releases as
# latest; dev pushes publish under the "dev" tag with make_latest=false).
# Only that 404 falls back to the dev release, with an explicit warning.
# Any other failure (network outage, 5xx, ...) must NOT silently install
# dev: previously every curl failure fell back, so a transient network
# blip installed an unintended dev binary. The HTTP status is probed
# with a second lightweight request; a probe that itself fails (000)
# is treated as a network failure, not a 404.
download_artifact() {
    local url="$1" out="$2" artifact="$3"
    if curl -fsSL "$url" -o "$out"; then
        return 0
    fi
    if [ "$RELEASE_VERSION" = "latest" ]; then
        local http_code
        http_code=$(curl -sL -o /dev/null -w "%{http_code}" "$url" 2>/dev/null || echo "000")
        if [ "$http_code" = "404" ]; then
            local dev_url="${DEFAULT_DOWNLOAD_BASE}/download/dev/${artifact}"
            log_warn "未找到最新正式版（404），回退到 dev 版：${dev_url}"
            if curl -fsSL "$dev_url" -o "$out"; then
                DOWNLOAD_URL="$dev_url"
                return 0
            fi
        else
            log_error "下载失败（HTTP ${http_code}），不是 404，不回退到 dev 版"
        fi
    fi
    DOWNLOAD_URL="$url"
    return 1
}

stage_binary() {
    local staged="$TMP_DIR/xboard-node"
    local local_src
    local_src=$(select_binary_source)
    if [ -n "$local_src" ]; then
        log_step "使用本地二进制文件：${local_src}"
        cp "$local_src" "$staged"
    else
        resolve_download_url "xboard-node-linux-${ARCH}"
        log_step "正在下载二进制文件：${DOWNLOAD_URL}"
        if ! download_artifact "$DOWNLOAD_URL" "$staged" "xboard-node-linux-${ARCH}"; then
            log_error "从 ${DOWNLOAD_URL} 下载二进制文件失败"
            exit 1
        fi
        verify_download "$DOWNLOAD_URL" "$staged"
    fi
    chmod +x "$staged"
    if ! "$staged" -v >/dev/null 2>&1; then
        log_error "下载的二进制文件未通过版本检查"
        exit 1
    fi
}

stage_xbnode() {
    local staged="$TMP_DIR/xbnode"
    # Only an explicitly requested local binary (--xbnode-binary) is ever
    # used; see select_binary_source for why implicit ./xbnode fallbacks
    # were removed.
    local local_src=""
    if [ -n "$CLI_BINARY_SOURCE" ]; then
        if [ ! -f "$CLI_BINARY_SOURCE" ]; then
            log_error "未找到 xbnode 二进制文件来源：$CLI_BINARY_SOURCE"
            exit 1
        fi
        local_src="$CLI_BINARY_SOURCE"
    fi
    if [ -n "$local_src" ]; then
        log_step "使用本地 xbnode 二进制文件：${local_src}"
        cp "$local_src" "$staged"
    else
        resolve_download_url "xbnode-linux-${ARCH}"
        log_step "正在下载 xbnode：${DOWNLOAD_URL}"
        if ! download_artifact "$DOWNLOAD_URL" "$staged" "xbnode-linux-${ARCH}"; then
            log_error "从 ${DOWNLOAD_URL} 下载 xbnode 失败"
            exit 1
        fi
        verify_download "$DOWNLOAD_URL" "$staged"
    fi
    chmod +x "$staged"
    if ! "$staged" version > /dev/null 2>&1; then
        log_error "下载的 xbnode 未通过版本检查"
        exit 1
    fi
}

render_config() {
    local init_args=(
        config init
        --mode "$MODE"
        --panel-url "$PANEL_URL"
        --kernel "${KERNEL_TYPE:-singbox}"
        --health-port "${HEALTH_PORT:-0}"
        --token "$TOKEN"
        --version "$RELEASE_VERSION"
        --output "$TMP_DIR/config.yml"
        --credentials-out "$TMP_DIR/credentials.env"
        --meta "$TMP_DIR/install-meta.json"
        --install-root "$INSTALL_ROOT"
    )
    # --force-reconfigure is real overwrite semantics, not a no-op:
    # the new binding replaces the whole previous config (and its
    # credentials) instead of merging into it. The previous state is
    # still backed up by backup_existing_state before it is replaced.
    # Without the flag, render_config merges (same-ID rebind preserves
    # customisation, other instances are kept).
    if [ "$FORCE_RECONFIGURE" -eq 1 ]; then
        log_warn "已启用 --force-reconfigure：已有配置将被新绑定完全替换（旧配置已备份）"
    else
        if [ -f "$CONFIG_FILE" ]; then
            init_args+=(--config "$CONFIG_FILE")
        fi
        if [ -f "$CREDENTIALS_FILE" ]; then
            init_args+=(--credentials-in "$CREDENTIALS_FILE")
        fi
    fi
    if [ "$MODE" = "machine" ]; then
        init_args+=(--machine-id "$MACHINE_ID")
    else
        init_args+=(--node-id "$NODE_ID")
        if [ -n "$NODE_TYPE" ]; then
            init_args+=(--node-type "$NODE_TYPE")
        fi
    fi
    if [ -n "$RUNTIME_GOMEMLIMIT" ]; then
        init_args+=(--gomemlimit "$RUNTIME_GOMEMLIMIT")
    fi
    if [ -n "$RUNTIME_GOGC" ] && [ "$RUNTIME_GOGC" -gt 0 ] 2>/dev/null; then
        init_args+=(--gogc "$RUNTIME_GOGC")
    fi

    local output
    output=$("$TMP_DIR/xbnode" "${init_args[@]}") || {
        log_error "xbnode config init 执行失败"
        exit 1
    }

    INSTANCE_ID=$(echo "$output" | grep '^INSTANCE_ID=' | cut -d= -f2-)
    chmod 600 "$TMP_DIR/credentials.env"
}

render_service() {
    cat >"$TMP_DIR/${SERVICE_NAME}" <<EOF_UNIT
[Unit]
Description=Xboard Node Backend
Documentation=https://github.com/emmaforge/Xboard-Node
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=${INSTALL_ROOT}
EnvironmentFile=-${CREDENTIALS_FILE}
ExecStart=${BINARY_PATH} -c ${CONFIG_FILE}
Restart=always
RestartSec=5
LimitNOFILE=1048576
NoNewPrivileges=true
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
EOF_UNIT
}

backup_existing_state() {
    BACKUP_PATH="${BACKUP_DIR}/$(date +%Y%m%d-%H%M%S)"
    mkdir -p "$BACKUP_PATH"
    if [ -x "$BINARY_PATH" ]; then
        cp "$BINARY_PATH" "$BACKUP_PATH/xboard-node"
    fi
    if [ -x "$CLI_PATH" ]; then
        cp "$CLI_PATH" "$BACKUP_PATH/xbnode"
    fi
    if [ -f "$CONFIG_FILE" ]; then
        cp "$CONFIG_FILE" "$BACKUP_PATH/config.yml"
    fi
    if [ -f "$CREDENTIALS_FILE" ]; then
        cp "$CREDENTIALS_FILE" "$BACKUP_PATH/credentials.env"
    fi
    if [ -f "$INSTALL_META" ]; then
        cp "$INSTALL_META" "$BACKUP_PATH/install-meta.json"
    fi
    if [ -f "$SERVICE_PATH" ]; then
        cp "$SERVICE_PATH" "$BACKUP_PATH/${SERVICE_NAME}"
        SERVICE_EXISTED=1
    else
        SERVICE_EXISTED=0
    fi
}

stop_existing_service() {
    if [ -f "$SERVICE_PATH" ] || systemctl is-active "$SERVICE_NAME" >/dev/null 2>&1; then
        systemctl stop "$SERVICE_NAME" >/dev/null 2>&1 || true
    fi
}

install_staged_files() {
    stop_existing_service
    install -m 755 "$TMP_DIR/xboard-node" "$BINARY_PATH"
    install -m 600 "$TMP_DIR/config.yml" "$CONFIG_FILE"
    install -m 600 "$TMP_DIR/credentials.env" "$CREDENTIALS_FILE"
    install -m 644 "$TMP_DIR/install-meta.json" "$INSTALL_META"
    if [ -f "$0" ] && [ "$(realpath "$0")" != "$(realpath "$INSTALLER_COPY_PATH" 2>/dev/null || echo "$INSTALLER_COPY_PATH")" ]; then
        install -m 755 "$0" "$INSTALLER_COPY_PATH"
    fi
    install -m 755 "$TMP_DIR/xbnode" "$CLI_PATH"
    ln -sf "$CLI_PATH" /usr/bin/xbnode 2>/dev/null || true
    # 旧版命令行工具 xbctl 已改名为 xbnode，清理残留，避免新旧命令并存。
    rm -f /usr/local/bin/xbctl /usr/bin/xbctl 2>/dev/null || true
    install -m 644 "$TMP_DIR/${SERVICE_NAME}" "$SERVICE_PATH"
    systemctl daemon-reload
    systemctl enable "$SERVICE_NAME" > /dev/null 2>&1
}

wait_for_health() {
    if ! systemctl is-active "$SERVICE_NAME" >/dev/null 2>&1; then
        return 1
    fi
    if [ "$HEALTH_ENABLED" -eq 0 ]; then
        return 0
    fi
    local attempt=0
    local max_attempts=30
    while [ "$attempt" -lt "$max_attempts" ]; do
        if ! systemctl is-active "$SERVICE_NAME" >/dev/null 2>&1; then
            return 1
        fi
        if curl -fsS "http://127.0.0.1:${HEALTH_PORT}/healthz" >/dev/null 2>&1; then
            return 0
        fi
        sleep 1
        attempt=$((attempt + 1))
    done
    return 1
}

show_recent_logs() {
    if command -v journalctl >/dev/null 2>&1; then
        journalctl -u "$SERVICE_NAME" -n 30 --no-pager || true
    fi
}

start_service() {
    if systemctl is-enabled "$SERVICE_NAME" >/dev/null 2>&1; then
        systemctl restart "$SERVICE_NAME"
    else
        systemctl start "$SERVICE_NAME"
    fi
    if ! wait_for_health; then
        log_error "服务未通过健康检查"
        show_recent_logs
        return 1
    fi
}

# install_staged_binaries_only: like install_staged_files but without
# touching config, credentials or install metadata, and without stopping
# or starting the service. Replacing a running executable via install(1)
# is safe (the running process keeps the old inode); the new binary takes
# effect on the next service restart (e.g. after menu binding).
# Enable semantics: only enable the service when it was already enabled
# or when this is a fresh install (no unit file existed). A pre-existing
# service that was deliberately left disabled must NOT be silently
# enabled by a bare reinstall overwriting it.
install_staged_binaries_only() {
    local had_service=0 was_enabled=0 was_active=0
    [ -f "$SERVICE_PATH" ] && had_service=1
    systemctl is-enabled "$SERVICE_NAME" >/dev/null 2>&1 && was_enabled=1
    systemctl is-active "$SERVICE_NAME" >/dev/null 2>&1 && was_active=1
    install -m 755 "$TMP_DIR/xboard-node" "$BINARY_PATH"
    if [ -f "$0" ] && [ "$(realpath "$0")" != "$(realpath "$INSTALLER_COPY_PATH" 2>/dev/null || echo "$INSTALLER_COPY_PATH")" ]; then
        install -m 755 "$0" "$INSTALLER_COPY_PATH"
    fi
    install -m 755 "$TMP_DIR/xbnode" "$CLI_PATH"
    ln -sf "$CLI_PATH" /usr/bin/xbnode 2>/dev/null || true
    # 旧版命令行工具 xbctl 已改名为 xbnode，清理残留，避免新旧命令并存。
    rm -f /usr/local/bin/xbctl /usr/bin/xbctl 2>/dev/null || true
    install -m 644 "$TMP_DIR/${SERVICE_NAME}" "$SERVICE_PATH"
    systemctl daemon-reload
    if [ "$was_enabled" -eq 1 ] || [ "$had_service" -eq 0 ]; then
        systemctl enable "$SERVICE_NAME" > /dev/null 2>&1
        BARE_INSTALL_ENABLED=1
    else
        log_warn "服务原本未设为开机自启，本次裸安装不擅自 enable，仍保持 disabled"
        BARE_INSTALL_ENABLED=0
    fi
    if [ "$was_active" -eq 1 ]; then
        log_warn "服务正在运行：新二进制将在下次重启服务后生效，本次裸安装不重启服务"
    fi
}

# perform_install_only: install binaries + xbnode + service unit only, no
# binding. Used when install runs without any panel/token/ID arguments
# (V2bX-style "install first, bind later" flow). Existing config and
# credentials are never touched, and the service is not started: with no
# binding there is nothing valid for it to run, and starting it would
# fail. Binding afterwards happens via the xbnode interactive menu
# (option 1 node, option 2 machine), which restarts the service.
perform_install_only() {
    TMP_DIR=$(mktemp -d)
    ensure_dirs
    stage_binary
    stage_xbnode
    render_service
    backup_existing_state
    install_staged_binaries_only

    log_info "安装完成（仅安装二进制，未绑定面板）"
    if [ -f "$CONFIG_FILE" ]; then
        log_info "已有配置已保留：${CONFIG_FILE}"
    fi
    if [ "${BARE_INSTALL_ENABLED:-1}" -eq 1 ]; then
        log_info "服务单元已安装并设为开机自启：${SERVICE_NAME}（本次仅安装，不启动服务）"
    else
        log_info "服务单元已安装（开机自启状态保持不变：仍为 disabled，本次仅安装，不启动服务）"
    fi
    log_info "请运行不带参数的 xbnode 打开中文交互菜单：xbnode"
    log_info "在菜单里选 1 绑定节点，或选 2 绑定机器，按提示填写面板地址、密钥和节点/机器 ID"
    log_info "绑定完成后服务会自动重启"
    log_info "命令行工具：${CLI_PATH}（若 xbnode 不在 PATH 中，可运行 '${CLI_PATH} list'）"
}

perform_install() {
    validate_install_request
    if [ "$INSTALL_ONLY" -eq 1 ]; then
        perform_install_only
        return
    fi
    detect_current_state
    TMP_DIR=$(mktemp -d)
    ensure_dirs
    stage_binary
    stage_xbnode
    render_config
    render_service
    backup_existing_state
    install_staged_files
    start_service

    log_info "安装成功"
    log_info "服务：${SERVICE_NAME}"
    log_info "配置：${CONFIG_FILE}"
    log_info "凭据：${CREDENTIALS_FILE}"
    if [ "$HEALTH_ENABLED" -eq 1 ]; then
        log_info "健康检查：http://127.0.0.1:${HEALTH_PORT}/healthz"
    fi
    log_info "命令行工具：${CLI_PATH}（若 xbnode 不在 PATH 中，可运行 '${CLI_PATH} list'）"
}

perform_upgrade() {
    detect_current_state
    if [ "$CURRENT_STATE" = "fresh" ]; then
        log_warn "未找到已有安装，回退到全新安装"
        perform_install
        return
    fi
    # Read the health port from the existing config BEFORE the
    # post-upgrade health check: previously the check always used the
    # installer's default port (65530), so an install bound with a
    # different port — or with health checks disabled — failed the
    # upgrade (and rolled back) even though the service was healthy.
    # wait_for_health already returns success when HEALTH_ENABLED=0,
    # so a disabled check no longer fails the upgrade.
    load_health_port_from_config "$CONFIG_FILE"
    TMP_DIR=$(mktemp -d)
    ensure_dirs
    stage_binary
    stage_xbnode
    render_service
    backup_existing_state
    install -m 755 "$TMP_DIR/xboard-node" "$BINARY_PATH"
    install -m 755 "$TMP_DIR/xbnode" "$CLI_PATH"
    ln -sf "$CLI_PATH" /usr/bin/xbnode 2>/dev/null || true
    # 旧版命令行工具 xbctl 已改名为 xbnode，清理残留，避免新旧命令并存。
    rm -f /usr/local/bin/xbctl /usr/bin/xbctl 2>/dev/null || true
    install -m 644 "$TMP_DIR/${SERVICE_NAME}" "$SERVICE_PATH"
    systemctl daemon-reload
    systemctl restart "$SERVICE_NAME"
    if ! wait_for_health; then
        log_error "升级后健康检查失败"
        show_recent_logs
        # B13: roll back explicitly instead of relying on the ERR trap.
        # Verified: with `set -Eeuo pipefail` and a bare call, the trap
        # does fire on this return — but only in that exact call shape.
        # If perform_upgrade were ever called in a condition (if/||/!)
        # or with `|| true`, bash suppresses the ERR trap and the new
        # binary would stay installed while unhealthy. Clearing
        # BACKUP_PATH afterwards also stops the trap from running the
        # same rollback a second time on the way out.
        if ! rollback_install; then
            log_error "升级失败后的回滚也失败了，需要手动处理"
        fi
        BACKUP_PATH=""
        return 1
    fi
    log_info "升级成功"
}

confirm_uninstall() {
    if [ "$YES" -eq 1 ]; then
        return
    fi
    echo
    read -r -p "Proceed with uninstall? [y/N]: " answer
    if ! [[ "$answer" =~ ^[Yy]$ ]]; then
        log_warn "已取消卸载"
        exit 0
    fi
}

# wait_for_service_stopped: poll systemctl until the service is no
# longer active (up to ~10s). A stop command returning success does
# not prove the process is gone; deleting binaries while it still runs
# would leave a half-removed deployment behind.
wait_for_service_stopped() {
    local i=0
    while [ "$i" -lt 10 ]; do
        if ! systemctl is-active "$SERVICE_NAME" >/dev/null 2>&1; then
            return 0
        fi
        sleep 1
        i=$((i + 1))
    done
    if systemctl is-active "$SERVICE_NAME" >/dev/null 2>&1; then
        return 1
    fi
    return 0
}

perform_uninstall() {
    confirm_uninstall
    if [ -f "$SERVICE_PATH" ]; then
        systemctl stop "$SERVICE_NAME" >/dev/null 2>&1 || true
        # Verify the service really stopped BEFORE deleting anything:
        # if it is still active, abort the uninstall rather than
        # removing binaries out from under a running service.
        if ! wait_for_service_stopped; then
            log_error "服务停止失败：${SERVICE_NAME} 仍在运行，卸载已中止，未删除任何文件"
            exit 1
        fi
        systemctl disable "$SERVICE_NAME" >/dev/null 2>&1 || true
        rm -f "$SERVICE_PATH"
        systemctl daemon-reload || true
    fi
    rm -f "$BINARY_PATH"
    rm -f "$CLI_PATH"
    rm -f /usr/bin/xbnode 2>/dev/null || true
    if [ "$PURGE" -eq 1 ]; then
        rm -rf "$INSTALL_ROOT"
        log_info "已删除 ${INSTALL_ROOT}"
    else
        rm -f "$INSTALL_META"
        log_info "配置保留在 ${INSTALL_ROOT} 下"
    fi
    log_info "卸载完成"
}

perform_status() {
    detect_current_state
    echo
    echo -e "${BOLD}xboard-node 安装状态${NC}"
    echo "  状态：  ${CURRENT_STATE}"
    if [ -f "$INSTALL_META" ]; then
        echo "  元数据：${INSTALL_META}"
        if [ -x "$CLI_PATH" ]; then
            "$CLI_PATH" list 2>/dev/null || true
        else
            # Simple key extraction from JSON (no Python needed)
            local val
            for key in config_mode version latest_instance_id instance_count updated_at; do
                val=$(sed -n "s/.*\"${key}\": *\"\{0,1\}\([^\"]*\)\"\{0,1\}.*/\1/p" "$INSTALL_META" | head -1)
                val="${val%,}"  # strip trailing comma from numeric JSON values
                [ -n "$val" ] && echo "  ${key}: ${val}"
            done
        fi
    fi
    if [ -f "$SERVICE_PATH" ]; then
        echo "  服务：  ${SERVICE_NAME}"
        systemctl status "$SERVICE_NAME" --no-pager || true
    fi
}

main() {
    parse_args "$@"
    case "$ACTION" in
        help)
            usage
            exit 0
            ;;
        status)
            ensure_systemd
            perform_status
            exit 0
            ;;
    esac

    check_root
    detect_arch
    detect_os
    ensure_systemd
    install_dependencies

    case "$ACTION" in
        install)
            perform_install
            ;;
        upgrade)
            perform_upgrade
            ;;
        uninstall)
            perform_uninstall
            ;;
        *)
            log_error "未知操作：$ACTION"
            usage
            exit 1
            ;;
    esac
}

main "$@"
