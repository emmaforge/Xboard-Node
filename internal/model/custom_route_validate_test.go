package model

import (
	"strings"
	"testing"
)

func TestValidateCustomRouteRules(t *testing.T) {
	available := map[string]struct{}{"direct": {}, "block": {}, "warp": {}, "proxy": {}}
	tests := []struct {
		name    string
		rules   []CustomRouteRule
		kernel  string
		wantErr string
	}{
		{
			name: "valid route target",
			rules: []CustomRouteRule{{
				Match:  RouteMatch{DomainSuffixes: []string{"example.com"}, Ports: []string{"443"}, Networks: []string{"tcp"}},
				Action: RouteAction{Type: "route", Target: "warp"},
			}},
			kernel: "singbox",
		},
		{
			name:    "missing match",
			rules:   []CustomRouteRule{{Action: RouteAction{Type: "direct"}}},
			kernel:  "singbox",
			wantErr: "custom_route_rules[0].match is required",
		},
		{
			name:    "unknown target",
			rules:   []CustomRouteRule{{Match: RouteMatch{DomainSuffixes: []string{"example.com"}}, Action: RouteAction{Type: "route", Target: "missing"}}},
			kernel:  "singbox",
			wantErr: `custom_route_rules[0].action.target references unknown outbound "missing"`,
		},
		{
			name:    "route target required",
			rules:   []CustomRouteRule{{Match: RouteMatch{DomainSuffixes: []string{"example.com"}}, Action: RouteAction{Type: "route"}}},
			kernel:  "singbox",
			wantErr: "custom_route_rules[0].action.target is required when action.type is route",
		},
		{
			name:    "direct target forbidden",
			rules:   []CustomRouteRule{{Match: RouteMatch{DomainSuffixes: []string{"example.com"}}, Action: RouteAction{Type: "direct", Target: "warp"}}},
			kernel:  "singbox",
			wantErr: "custom_route_rules[0].action.target is only allowed when action.type is route",
		},
		{
			name:    "unsupported action",
			rules:   []CustomRouteRule{{Match: RouteMatch{DomainSuffixes: []string{"example.com"}}, Action: RouteAction{Type: "dns"}}},
			kernel:  "singbox",
			wantErr: `custom_route_rules[0].action.type "dns" is not supported by kernel "singbox"`,
		},
		{
			name:    "invalid port range",
			rules:   []CustomRouteRule{{Match: RouteMatch{Ports: []string{"2000-1000"}}, Action: RouteAction{Type: "direct"}}},
			kernel:  "singbox",
			wantErr: `custom_route_rules[0].match.ports contains invalid port range "2000-1000"`,
		},
		{
			name:    "invalid network",
			rules:   []CustomRouteRule{{Match: RouteMatch{Networks: []string{"icmp"}}, Action: RouteAction{Type: "direct"}}},
			kernel:  "singbox",
			wantErr: `custom_route_rules[0].match.networks contains unsupported network "icmp"`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateCustomRouteRules(tc.rules, tc.kernel, available)
			if tc.wantErr == "" && err != nil {
				t.Fatalf("expected no error, got %v", err)
			}
			if tc.wantErr != "" {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("unexpected error: %v", err)
				}
			}
		})
	}
}

func TestValidateCustomRouteRules_Formats(t *testing.T) {
	// Regression test for B11: IP/CIDR and domain matchers used to
	// skip format validation entirely, so typos silently matched
	// nothing at runtime.
	valid := func(match RouteMatch) []CustomRouteRule {
		return []CustomRouteRule{{Match: match, Action: RouteAction{Type: "block"}}}
	}
	validCases := []struct {
		kernel string
		match  RouteMatch
	}{
		{"singbox", RouteMatch{IPCIDRs: []string{"10.0.0.0/8", "2001:db8::/32", "192.168.1.1"}}},
		{"singbox", RouteMatch{SourceCIDRs: []string{"0.0.0.0/0"}}},
		{"singbox", RouteMatch{Domains: []string{"example.com", "sub.example.co.uk"}}},
		{"xray", RouteMatch{Domains: []string{"domain:example.com", "regexp:^.*\\.example\\.com$"}}},
		{"singbox", RouteMatch{DomainSuffixes: []string{"example.com"}}},
	}
	for i, vc := range validCases {
		if err := ValidateCustomRouteRules(valid(vc.match), vc.kernel, nil); err != nil {
			t.Errorf("valid case %d should pass, got %v", i, err)
		}
	}
	invalidCases := []RouteMatch{
		{IPCIDRs: []string{"10.0.0.256/8"}},
		{IPCIDRs: []string{"not-an-ip"}},
		{SourceCIDRs: []string{"192.168.1.0/33"}},
		{Domains: []string{"exa mple.com"}},
		{Domains: []string{"*.example.com"}},
		{Domains: []string{"example..com"}},
		{Domains: []string{"regexp:([invalid"}},
		{Domains: []string{"domain:example.com"}},
		{DomainSuffixes: []string{"-bad.example.com"}},
	}
	for i, m := range invalidCases {
		if err := ValidateCustomRouteRules(valid(m), "singbox", nil); err == nil {
			t.Errorf("invalid case %d should fail: %+v", i, m)
		}
	}
}
