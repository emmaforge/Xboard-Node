package singbox

import (
	"testing"

	"github.com/cedar2025/xboard-node/internal/kernel"
	"github.com/cedar2025/xboard-node/internal/model"
	"github.com/cedar2025/xboard-node/internal/panel"
)

func TestCompilePanelRouteRule_DNS(t *testing.T) {
	// A dns action without a target must NOT reference the never-defined
	// "dns-out" outbound; it falls back to direct (mirroring xray).
	rules := compilePanelRouteRule(model.RouteRule{ID: 1, Match: []string{"example.com"}, Action: "dns"})
	if len(rules) != 1 {
		t.Fatalf("rules = %d, want 1", len(rules))
	}
	if rules[0]["outbound"] != "direct" {
		t.Fatalf("dns without target outbound = %v, want direct", rules[0]["outbound"])
	}

	// With an explicit target, the target is used.
	rules = compilePanelRouteRule(model.RouteRule{ID: 2, Match: []string{"example.com"}, Action: "dns", ActionValue: "dns-server"})
	if rules[0]["outbound"] != "dns-server" {
		t.Fatalf("dns with target outbound = %v, want dns-server", rules[0]["outbound"])
	}
}

func TestBuildInbound_Hysteria2ProtocolString(t *testing.T) {
	// Protocols() advertises "hysteria2", so the literal string must
	// build a hysteria2 inbound even when Version is unset — not nil
	// (which buildConfig would silently drop, starting a node with no
	// inbound at all).
	nc := &panel.NodeConfig{Protocol: "hysteria2", ServerPort: 443}
	inbound := buildInbound(testNodeSpec(nc), testUsers, kernel.TLSCert{})
	if inbound == nil {
		t.Fatal("buildInbound(hysteria2) = nil, want a hysteria2 inbound")
	}
	if inbound["type"] != "hysteria2" {
		t.Fatalf("inbound type = %v, want hysteria2", inbound["type"])
	}

	// The plain "hysteria" protocol still selects v2 when Version == 2.
	nc2 := &panel.NodeConfig{Protocol: "hysteria", ServerPort: 443, Version: 2}
	inbound = buildInbound(testNodeSpec(nc2), testUsers, kernel.TLSCert{})
	if inbound == nil || inbound["type"] != "hysteria2" {
		t.Fatalf("buildInbound(hysteria, Version=2) type = %v, want hysteria2", inbound["type"])
	}
}

func TestApplyTransport_WS_HostPriority(t *testing.T) {
	// Unified rule with xray: an explicit Host header wins over the host
	// field; the host field only fills in a missing Host.
	t.Run("explicit Host header wins", func(t *testing.T) {
		base := M{}
		nc := &panel.NodeConfig{
			Network: "ws",
			NetworkSettings: map[string]interface{}{
				"path":    "/ws",
				"host":    "field.example",
				"headers": map[string]interface{}{"Host": "header.example"},
			},
		}
		applyTransport(base, testNodeSpec(nc))
		headers := base["transport"].(M)["headers"].(M)
		if headers["Host"] != "header.example" {
			t.Fatalf("Host = %v, want header.example", headers["Host"])
		}
	})

	t.Run("host fills missing Host", func(t *testing.T) {
		base := M{}
		nc := &panel.NodeConfig{
			Network: "ws",
			NetworkSettings: map[string]interface{}{
				"path":    "/ws",
				"host":    "field.example",
				"headers": map[string]interface{}{"X-Custom": "1"},
			},
		}
		applyTransport(base, testNodeSpec(nc))
		headers := base["transport"].(M)["headers"].(M)
		if headers["Host"] != "field.example" || headers["X-Custom"] != "1" {
			t.Fatalf("headers = %v, want Host=field.example plus X-Custom", headers)
		}
	})

	t.Run("host alone becomes Host", func(t *testing.T) {
		base := M{}
		nc := &panel.NodeConfig{
			Network: "ws",
			NetworkSettings: map[string]interface{}{
				"path": "/ws",
				"host": "field.example",
			},
		}
		applyTransport(base, testNodeSpec(nc))
		headers := base["transport"].(M)["headers"].(M)
		if headers["Host"] != "field.example" {
			t.Fatalf("Host = %v, want field.example", headers["Host"])
		}
	})
}
