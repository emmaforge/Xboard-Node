package singbox

import (
	"net"
	"testing"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing/service"

	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/kernel"
	"github.com/cedar2025/xboard-node/internal/model"
	"github.com/cedar2025/xboard-node/internal/panel"
)

func freeTestPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for free port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}

func TestSingBoxStartOnRunningKernel(t *testing.T) {
	// The kernel contract says Start on a running kernel stops the old
	// instance first. Starting twice on the same fixed port must
	// succeed; building the new instance before stopping the old one
	// failed the second bind with EADDRINUSE.
	s := New(config.KernelConfig{Type: "sing-box"})
	defer s.Stop()
	nc := testNodeSpec(&panel.NodeConfig{Protocol: "vmess", ServerPort: freeTestPort(t)})
	if err := s.Start(nc, testUsers, kernel.TLSCert{}); err != nil {
		t.Fatalf("first Start() error = %v", err)
	}
	if err := s.Start(nc, testUsers, kernel.TLSCert{}); err != nil {
		t.Fatalf("second Start() on a running kernel error = %v", err)
	}
	if !s.IsRunning() {
		t.Fatal("expected kernel running after second Start")
	}
}

func TestSingBoxAddUsersRecreatesNonUpdatableInbound(t *testing.T) {
	// Mieru inbounds implement no UpdatableInbound, so AddUsers must
	// fall back to inbound reconstruction instead of silently skipping
	// the update (err left nil -> continue). Reconstruction is proven
	// by the inbound object identity changing across AddUsers.
	s := New(config.KernelConfig{Type: "sing-box"})
	defer s.Stop()
	nc := testNodeSpec(&panel.NodeConfig{Protocol: "mieru", ServerPort: freeTestPort(t), Transport: "TCP"})
	if err := s.Start(nc, testUsers, kernel.TLSCert{}); err != nil {
		t.Fatalf("Start(mieru) error = %v", err)
	}
	im := service.FromContext[adapter.InboundManager](s.ctx)
	if im == nil {
		t.Fatal("inbound manager not available")
	}
	before, ok := im.Get("mieru-in")
	if !ok {
		t.Fatal("mieru-in inbound not found after Start")
	}
	added, err := s.AddUsers([]model.UserSpec{{ID: 3, UUID: "cccccccc-9999-8888-7777-666666666666"}})
	if err != nil {
		t.Fatalf("AddUsers() error = %v", err)
	}
	if added != 1 {
		t.Fatalf("AddUsers() added = %d, want 1", added)
	}
	after, ok := im.Get("mieru-in")
	if !ok {
		t.Fatal("mieru-in inbound not found after AddUsers")
	}
	if after == before {
		t.Fatal("expected mieru inbound to be recreated by AddUsers, but the same inbound object is still registered (user change silently skipped)")
	}
	if !s.IsRunning() {
		t.Fatal("expected kernel still running after inbound recreation")
	}
}
