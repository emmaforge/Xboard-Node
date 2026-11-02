# xboard-node

[Xboard](https://github.com/cedar2025/Xboard) 面板的节点后端，支持 `sing-box` / `xray-core` 双内核。

> **声明**：本项目仅供学习交流使用。

## 功能

- 协议：V2Ray 全家桶、Trojan、Shadowsocks、Hysteria2、TUIC、AnyTLS
- 同步：WebSocket 推送 + REST 轮询双通道
- 用户控制：限速、设备数限制、在线 IP 统计、热更新
- 部署模式：节点模式、机器模式、单机模式
- 多实例：单个进程绑定多个面板 / 节点

## 安装

### 安装脚本（Linux + systemd）

仅支持主流系统：Ubuntu、Debian、CentOS、RHEL、Rocky、AlmaLinux、Fedora，amd64 / arm64 架构，且必须使用 systemd。

```bash
# 先装后配（推荐，像 V2bX 一样）：不带绑定参数直接安装，装完用交互菜单绑定
curl -fsSL https://raw.githubusercontent.com/zzx327317-creator/Xboard-Node/dev/install.sh | sudo bash
xbnode
# 在中文交互菜单里选 1 绑定节点或选 2 绑定机器，按提示填写面板地址、密钥和节点/机器 ID

# 安装时直接绑定（备选）：节点模式
curl -fsSL https://raw.githubusercontent.com/zzx327317-creator/Xboard-Node/dev/install.sh | \
  sudo bash -s -- --mode node --panel https://panel.example.com --token TOKEN --node-id 1

# 安装时直接绑定（备选）：机器模式
curl -fsSL https://raw.githubusercontent.com/zzx327317-creator/Xboard-Node/dev/install.sh | \
  sudo bash -s -- --mode machine --panel https://panel.example.com --token TOKEN --machine-id 1
```

只安装不绑定时，安装脚本只装二进制、xbnode 和 systemd 单元（设为开机自启、不启动服务），不写也不覆盖已有配置和凭据；绑定完成后面板服务会自动重启。

## xbnode

安装完成后直接运行 `xbnode`（不带参数）即可打开中文交互菜单，菜单里有安装/绑定、启停、状态、日志、版本、切换内核、更新、卸载等全部管理功能；也可以直接用子命令：

```bash
xbnode menu                        # 打开中文交互菜单
xbnode list                        # 列出全部实例
xbnode status                      # 查看运行状态
xbnode bind add-node --panel URL --token TOKEN --node-id 1
xbnode bind add-machine --panel URL --token TOKEN --machine-id 1
xbnode bind remove-node --panel URL --node-id 1
xbnode service restart
xbnode kernel                      # 查看各实例内核
xbnode kernel <id> singbox|xray    # 切换实例内核
```

## 配置

旧版单面板配置完全兼容。追加绑定后会自动迁移为 `instances` 格式。示例见 `config.yml.example`。

## 扩展文档

- 自定义路由：[docs-custom-routes.md](docs-custom-routes.md)
- 自定义出站：[docs-custom-outbounds.md](docs-custom-outbounds.md)
- DNS 服务商（ACME DNS-01）：[docs-dns-providers.md](docs-dns-providers.md)

## 开源协议

MPL-2.0。
