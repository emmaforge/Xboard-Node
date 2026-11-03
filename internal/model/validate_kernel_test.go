package model

import "testing"

func TestResolveKernelForNode(t *testing.T) {
	cases := []struct {
		name       string
		network    string
		decryption string
		configured string
		want       string
		wantReason string
	}{
		{"singbox xhttp switches", "xhttp", "", "singbox", "xray", "transport"},
		{"singbox splithttp switches", "splithttp", "", "singbox", "xray", "transport"},
		{"singbox tcp stays", "tcp", "", "singbox", "singbox", ""},
		{"singbox encryption switches", "tcp", "mlkem768x25519plus.native.600s.abc", "singbox", "xray", "encryption"},
		{"singbox encryption none stays", "tcp", "none", "singbox", "singbox", ""},
		{"singbox encryption empty stays", "tcp", "", "singbox", "singbox", ""},
		{"singbox transport reason wins", "xhttp", "mlkem768x25519plus.native.600s.abc", "singbox", "xray", "transport"},
		{"xray xhttp stays", "xhttp", "", "xray", "xray", ""},
		{"xray encryption stays", "tcp", "mlkem768x25519plus.native.600s.abc", "xray", "xray", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, reason := ResolveKernelForNode(c.network, c.decryption, c.configured)
			if got != c.want || reason != c.wantReason {
				t.Errorf("ResolveKernelForNode(%q, %q, %q) = (%q, %q), want (%q, %q)",
					c.network, c.decryption, c.configured, got, reason, c.want, c.wantReason)
			}
		})
	}
}

func TestResolveKernelForTransport(t *testing.T) {
	if got := ResolveKernelForTransport("xhttp", "singbox"); got != "xray" {
		t.Errorf("ResolveKernelForTransport(xhttp, singbox) = %q, want xray", got)
	}
	if got := ResolveKernelForTransport("tcp", "singbox"); got != "singbox" {
		t.Errorf("ResolveKernelForTransport(tcp, singbox) = %q, want singbox", got)
	}
}
