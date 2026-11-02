# 自定义路由

## 快速示例

```json
{
  "custom_route_rules": [
    {
      "name": "direct-example",
      "match": {"domain_suffixes": ["example.com"]},
      "action": {"type": "direct"}
    },
    {
      "name": "block-ads",
      "match": {"domains": ["ads.example.com"]},
      "action": {"type": "block"}
    },
    {
      "name": "route-warp",
      "match": {"ip_cidrs": ["1.1.1.0/24"], "ports": ["80", "443"]},
      "action": {"type": "route", "target": "warp-out"}
    }
  ]
}
```

## 匹配条件

| 条件 | 说明 | 示例 |
|-----------|-------------|---------|
| `domains` | 精确匹配域名 | `["api.example.com"]` |
| `domain_suffixes` | 后缀匹配 | `["example.com"]` |
| `ip_cidrs` | IP CIDR 段 | `["10.0.0.0/8"]` |
| `ports` | 端口（单个或范围） | `["443", "8000-9000"]` |
| `networks` | 协议 | `["tcp"]` 或 `["udp"]` |
| `source_cidrs` | 来源 IP CIDR | `["192.168.1.0/24"]` |
| `source_ports` | 来源端口 | `["1024-65535"]` |

## 动作类型

| 动作 | 说明 |
|--------|-------------|
| `{"type": "direct"}` | 直连，不走代理 |
| `{"type": "block"}` | 拦截连接 |
| `{"type": "route", "target": "tag"}` | 路由到指定出站（填出站的 tag） |

## 生效顺序

1. 结构化 `custom_route_rules`（优先级最高）
2. 原生 `custom_routes`
3. 内置拦截规则
4. 面板路由

## 内核兼容性

| 功能 | Xray | Sing-box |
|---------|------|----------|
| 全部匹配条件 | ✅ | ✅ |
| direct / block / route | ✅ | ✅ |

## 最佳实践

- **优先用** `custom_route_rules`：双内核通用，由面板管理
- `custom_routes` **只在**需要原生功能时用（例如负载均衡）
