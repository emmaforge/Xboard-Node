package xray

import (
	"sync/atomic"
	"time"
	"testing"

	"github.com/cedar2025/xboard-node/internal/model"
	"github.com/xtls/xray-core/common/buf"
	"github.com/xtls/xray-core/transport"
)

func newTestDispatcher() *LimitDispatcher {
	return &LimitDispatcher{
		limitedIPs: make(map[string]map[string]int),
	}
}

type nopReader struct{}

func (nopReader) ReadMultiBuffer() (buf.MultiBuffer, error) { return nil, nil }

func TestLimitDispatcher_DeviceLimitCheck(t *testing.T) {
	ld := newTestDispatcher()

	users := []model.UserSpec{
		{ID: 1, UUID: "uuid-1", DeviceLimit: 2, SpeedLimit: 0},
		{ID: 2, UUID: "uuid-2", DeviceLimit: 0, SpeedLimit: 10},
	}

	emailToUID := make(map[string]int)
	deviceLimits := make(map[string]int)
	speedLimits := make(map[string]int)
	for _, u := range users {
		email := userEmail(u.ID)
		emailToUID[email] = u.ID
		if u.DeviceLimit > 0 {
			deviceLimits[email] = u.DeviceLimit
		}
		if u.SpeedLimit > 0 {
			speedLimits[email] = u.SpeedLimit
		}
	}
	ld.UpdateLimits(emailToUID, deviceLimits, speedLimits)

	email1 := userEmail(1)

	// First IP should be allowed
	if ld.checkDeviceLimit(email1, "1.1.1.1") {
		t.Error("first IP should be allowed")
	}

	// Second IP should be allowed (limit=2)
	if ld.checkDeviceLimit(email1, "2.2.2.2") {
		t.Error("second IP should be allowed")
	}

	// Third unique IP should be rejected
	if !ld.checkDeviceLimit(email1, "3.3.3.3") {
		t.Error("third IP should be rejected (limit=2)")
	}

	// Same IP as first should be allowed (already connected)
	if ld.checkDeviceLimit(email1, "1.1.1.1") {
		t.Error("same IP should always be allowed")
	}

	// User 2 has no device limit — should always be allowed
	email2 := userEmail(2)
	for i := 0; i < 10; i++ {
		ip := "10.0.0." + string(rune('0'+i))
		if ld.checkDeviceLimit(email2, ip) {
			t.Errorf("user with no device limit should always be allowed (ip=%s)", ip)
		}
	}
}

func TestLimitDispatcher_DelConn(t *testing.T) {
	ld := newTestDispatcher()

	email := userEmail(1)
	deviceLimits := map[string]int{email: 2}
	ld.UpdateLimits(map[string]int{email: 1}, deviceLimits, nil)

	// Add 2 IPs
	ld.checkDeviceLimit(email, "1.1.1.1")
	ld.checkDeviceLimit(email, "2.2.2.2")

	// Third should be rejected
	if !ld.checkDeviceLimit(email, "3.3.3.3") {
		t.Error("third IP should be rejected")
	}

	// Remove first IP
	ld.delConn(email, "1.1.1.1")

	// Now third IP should be allowed
	if ld.checkDeviceLimit(email, "3.3.3.3") {
		t.Error("after deleting one IP, new IP should be allowed")
	}
}

func TestLimitDispatcher_GetConnectionState(t *testing.T) {
	ld := newTestDispatcher()

	email1 := userEmail(1)
	email2 := userEmail(2)
	ld.UpdateLimits(map[string]int{email1: 1, email2: 2}, nil, nil)

	ic1 := &ipCounter{}
	r1 := &atomic.Int64{}
	r1.Store(1)
	ic1.ips.Store("1.1.1.1", r1)
	r2 := &atomic.Int64{}
	r2.Store(1)
	ic1.ips.Store("2.2.2.2", r2)
	ld.unlimitedIPs.Store(email1, ic1)

	ic2 := &ipCounter{}
	r3 := &atomic.Int64{}
	r3.Store(1)
	ic2.ips.Store("3.3.3.3", r3)
	ld.unlimitedIPs.Store(email2, ic2)

	ld.connCount.Store(5)

	aliveIPs, connCount := ld.GetConnectionState()

	if connCount != 5 {
		t.Errorf("expected connCount=5, got %d", connCount)
	}
	if len(aliveIPs[1]) != 2 {
		t.Errorf("user 1 IPs: got %d, want 2", len(aliveIPs[1]))
	}
	if len(aliveIPs[2]) != 1 {
		t.Errorf("user 2 IPs: got %d, want 1", len(aliveIPs[2]))
	}
}

func TestLimitDispatcher_ResetConns(t *testing.T) {
	ld := newTestDispatcher()

	ld.mu.Lock()
	ld.limitedIPs["user@1"] = map[string]int{"1.1.1.1": 1}
	ld.mu.Unlock()
	ld.connCount.Store(3)

	ld.ResetConns()

	ld.mu.RLock()
	ipCount := len(ld.limitedIPs)
	ld.mu.RUnlock()
	if ipCount != 0 {
		t.Error("limitedIPs should be empty after reset")
	}

	if ld.connCount.Load() != 0 {
		t.Error("connCount should be 0 after reset")
	}
}

func TestLimitDispatcher_UnlimitedUserFastPath(t *testing.T) {
	ld := newTestDispatcher()

	email := userEmail(1)
	// No device limit set for this user
	ld.UpdateLimits(map[string]int{email: 1}, nil, nil)

	// Should use fast path (sync.Map), no lock needed
	for i := 0; i < 100; i++ {
		ip := "10.0.0." + string(rune('0'+i%10))
		if ld.checkDeviceLimit(email, ip) {
			t.Errorf("unlimited user should always be allowed (ip=%s)", ip)
		}
	}

	// Verify IPs are tracked in unlimitedIPs
	v, ok := ld.unlimitedIPs.Load(email)
	if !ok {
		t.Error("unlimited user should have entry in unlimitedIPs")
	}
	ic := v.(*ipCounter)
	ips := ic.aliveIPs()
	if len(ips) == 0 {
		t.Error("should have tracked some IPs")
	}
}


func TestLimitDispatcher_TrackLinkPreservesReader(t *testing.T) {
	ld := newTestDispatcher()
	email := userEmail(1)
	ld.UpdateLimits(map[string]int{email: 1}, map[string]int{email: 1}, nil)

	origReader := nopReader{}
	origWriter := &closeTrackingWriter{Writer: buf.Discard, onClose: func() {}}
	link := &transport.Link{Reader: origReader, Writer: origWriter}

	ld.trackLink(link, email, "1.1.1.1")

	if link.Reader != origReader {
		t.Fatal("trackLink must not replace link.Reader")
	}
	if link.Writer == origWriter {
		t.Fatal("trackLink should wrap link.Writer for lifecycle callbacks")
	}
}

func TestLimitDispatcher_CloseTrackingWriterReleasesConn(t *testing.T) {
	ld := newTestDispatcher()
	email := userEmail(1)
	ld.UpdateLimits(map[string]int{email: 1}, map[string]int{email: 1}, nil)
	if ld.checkDeviceLimit(email, "1.1.1.1") {
		t.Fatal("first connection should be allowed")
	}

	link := &transport.Link{Reader: nopReader{}, Writer: buf.Discard}
	ld.trackLink(link, email, "1.1.1.1")

	if got := ld.connCount.Load(); got != 1 {
		t.Fatalf("expected connCount=1 after tracking, got %d", got)
	}
	cw, ok := link.Writer.(*closeTrackingWriter)
	if !ok {
		t.Fatal("expected closeTrackingWriter wrapper")
	}
	if err := cw.Close(); err != nil {
		t.Fatalf("closeTrackingWriter.Close() error = %v", err)
	}
	if got := ld.connCount.Load(); got != 0 {
		t.Fatalf("expected connCount=0 after close, got %d", got)
	}
	if ld.checkDeviceLimit(email, "2.2.2.2") {
		t.Fatal("device slot should be released after writer close")
	}
}

func TestLimitDispatcher_CloseUserConnections(t *testing.T) {
	ld := newTestDispatcher()
	email1 := userEmail(1)
	email2 := userEmail(2)
	ld.UpdateLimits(map[string]int{email1: 1, email2: 2}, nil, nil)

	link1 := &transport.Link{Reader: nopReader{}, Writer: buf.Discard}
	link2 := &transport.Link{Reader: nopReader{}, Writer: buf.Discard}
	link3 := &transport.Link{Reader: nopReader{}, Writer: buf.Discard}
	ld.trackLink(link1, email1, "1.1.1.1")
	ld.trackLink(link2, email1, "1.1.1.2")
	ld.trackLink(link3, email2, "2.2.2.2")
	if got := ld.connCount.Load(); got != 3 {
		t.Fatalf("connCount = %d, want 3", got)
	}

	if n := ld.CloseUserConnections(email1); n != 2 {
		t.Fatalf("CloseUserConnections(email1) = %d, want 2", n)
	}
	if got := ld.connCount.Load(); got != 1 {
		t.Fatalf("connCount after close = %d, want 1", got)
	}
	if n := ld.CloseUserConnections(email1); n != 0 {
		t.Fatalf("second CloseUserConnections(email1) = %d, want 0", n)
	}
	// Removed users must still surface in connection state so the
	// service safety net can see their surviving links.
	aliveIPs, _ := ld.GetConnectionState()
	if aliveIPs[2] == nil {
		t.Fatal("expected surviving user 2 to remain in connection state")
	}
}

func TestLimitDispatcher_DeviceLimitCountsUDPSessions(t *testing.T) {
	// Regression test for B2: device limits used to count only TCP
	// sessions, so UDP/QUIC users could open unlimited devices. A
	// dispatched Link is a session regardless of transport, so UDP
	// sessions must count exactly like TCP ones.
	ld := newTestDispatcher()
	email := userEmail(1)
	ld.UpdateLimits(map[string]int{email: 1}, map[string]int{email: 1}, nil)

	if ld.checkDeviceLimit(email, "1.1.1.1") {
		t.Fatal("first device should be allowed")
	}
	if !ld.checkDeviceLimit(email, "2.2.2.2") {
		t.Fatal("second distinct device should be rejected even for UDP sessions")
	}
	ld.delConn(email, "1.1.1.1")
	if ld.checkDeviceLimit(email, "2.2.2.2") {
		t.Fatal("device slot should be released after delConn")
	}
}

func TestLimitDispatcher_GlobalDevicesEnforced(t *testing.T) {
	ld := newTestDispatcher()
	email := userEmail(1)
	ld.UpdateLimits(map[string]int{email: 1}, map[string]int{email: 2}, nil)

	// One local device, one device on another node: the merged count
	// already hits the limit of 2, so a third device must be rejected
	// even though this node alone only sees one IP.
	if ld.checkDeviceLimit(email, "1.1.1.1") {
		t.Fatal("first local IP should be allowed")
	}
	ld.UpdateGlobalDevices(map[string]map[string]bool{email: {"2.2.2.2": true}})
	if !ld.checkDeviceLimit(email, "3.3.3.3") {
		t.Fatal("third device should be rejected once cross-node state fills the limit")
	}
	// An IP already known globally is admitted, not double-counted.
	if ld.checkDeviceLimit(email, "2.2.2.2") {
		t.Fatal("globally known IP should be admitted")
	}
	// Stale global state falls back to local-only counting: on a fresh
	// dispatcher with one local IP, a stale global entry must not push
	// the merged count to the limit.
	ld2 := newTestDispatcher()
	ld2.UpdateLimits(map[string]int{email: 1}, map[string]int{email: 2}, nil)
	if ld2.checkDeviceLimit(email, "1.1.1.1") {
		t.Fatal("first local IP should be allowed")
	}
	ld2.UpdateGlobalDevices(map[string]map[string]bool{email: {"2.2.2.2": true}})
	ld2.globalLastUpdate = time.Now().Add(-2 * time.Minute)
	if ld2.checkDeviceLimit(email, "3.3.3.3") {
		t.Fatal("with stale global state, local-only counting should admit a new IP under the local limit")
	}
}

func TestLimitDispatcher_GlobalDevicesIPv6Canonical(t *testing.T) {
	ld := newTestDispatcher()
	email := userEmail(1)
	ld.UpdateGlobalDevices(map[string]map[string]bool{email: {"2001:db8:abcd:1234:5678::1": true}})
	got := ld.globalIPsFor(email)
	if got == nil || !got["2001:db8:abcd:1234::"] {
		t.Fatalf("global IPs should be canonicalized to /64, got %v", got)
	}
}
