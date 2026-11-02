# 自定义出站

## 快速示例

```json
{
  "custom_outbounds": [
    {
      "tag": "warp",
      "protocol": "wireguard",
      "settings": {
        "peers": [{"address": "162.159.195.1"}],
        "private_key": "aMzA..."
      }
    },
    {
      "tag": "us-proxy",
      "protocol": "socks",
      "settings": {"servers": [{"address": "1.2.3.4", "port": 1080}]},
      "proxy_tag": "warp"
    }
  ]
}
```

## 支持的协议

| 协议 | Xray | Sing-box |
|----------|------|----------|
| vmess | ✅ | ✅ |
| vless | ✅ | ✅ |
| trojan | ✅ | ✅ |
| shadowsocks | ✅ | ✅ |
| socks | ✅ | ✅ |
| http | ✅ | ✅ |
| wireguard | ✅ | ✅ |
| tuic | ❌ | ✅ |
| hysteria2 | ❌ | ✅ |
| anytls | ❌ | ✅ |
| naive | ❌ | ✅ |
| mieru | ❌ | ✅ |

## 核心字段

| 字段 | 是否必填 | 说明 |
|-------|----------|-------------|
| `tag` | ✅ | 出站标识，在路由中引用它 |
| `protocol` | ✅ | 协议类型 |
| `settings` | ✅ | 该协议的专属配置 |
| `proxy_tag` | ❌ | 串联到另一个出站（填对方的 tag） |

## 最佳实践

- **优先用** `custom_outbounds`：由面板管理，双内核通用
- `kernel.custom_outbound` **只在**需要原生字段时用
