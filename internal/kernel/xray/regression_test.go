package xray

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	xrayCore "github.com/xtls/xray-core/core"
	featurebandwidth "github.com/xtls/xray-core/features/bandwidth"
	"golang.org/x/time/rate"

	"github.com/cedar2025/xboard-node/internal/kernel"
	"github.com/cedar2025/xboard-node/internal/limiter"
	"github.com/cedar2025/xboard-node/internal/model"
	"github.com/cedar2025/xboard-node/internal/panel"
)

func TestLimitDispatcher_GetConnectionStateConcurrent(t *testing.T) {
	// Regression test for the concurrent map iteration crash:
	// GetConnectionState used to iterate limitedIPs after releasing
	// RLock while checkDeviceLimit/delConn wrote under Lock. Run all
	// three concurrently under -race; the test fails on a race or a
	// "concurrent map iteration and map write" panic.
	ld := newTestDispatcher()
	email := userEmail(1)
	ld.UpdateLimits(map[string]int{email: 1}, map[string]int{email: 2}, nil)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_, _ = ld.GetConnectionState()
			}
		}
	}()
	go func() {
		defer wg.Done()
		ips := []string{"1.1.1.1", "2.2.2.2", "3.3.3.3"}
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
				ld.checkDeviceLimit(email, ips[i%len(ips)])
			}
		}
	}()
	go func() {
		defer wg.Done()
		ips := []string{"1.1.1.1", "2.2.2.2", "3.3.3.3"}
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
				ld.delConn(email, ips[i%len(ips)])
			}
		}
	}()
	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()
}

func TestLimitDispatcher_UpdateLimitsMigratesBuckets(t *testing.T) {
	ld := newTestDispatcher()
	email := userEmail(1)
	ld.UpdateLimits(map[string]int{email: 1}, map[string]int{email: 2}, nil)

	// Two live limited connections.
	if ld.checkDeviceLimit(email, "1.1.1.1") || ld.checkDeviceLimit(email, "2.2.2.2") {
		t.Fatal("first two devices should be admitted")
	}

	// Flip to unlimited: counts must move to the unlimited bucket, not
	// leak in limitedIPs.
	ld.UpdateLimits(map[string]int{email: 1}, nil, nil)
	ld.mu.RLock()
	if len(ld.limitedIPs[email]) != 0 {
		t.Fatalf("limitedIPs after switch to unlimited = %v, want empty", ld.limitedIPs[email])
	}
	ld.mu.RUnlock()
	alive, _ := ld.GetConnectionState()
	if len(alive[1]) != 2 {
		t.Fatalf("alive IPs after switch to unlimited = %v, want 2", alive[1])
	}

	// Closing one connection must release its slot in the new bucket.
	ld.delConn(email, "1.1.1.1")
	alive, _ = ld.GetConnectionState()
	if len(alive[1]) != 1 || !alive[1]["2.2.2.2"] {
		t.Fatalf("alive IPs after delConn = %v, want only 2.2.2.2", alive[1])
	}

	// Flip back to limited: the surviving count must move back, and its
	// slot must be releasable from the limited bucket.
	ld.UpdateLimits(map[string]int{email: 1}, map[string]int{email: 2}, nil)
	if v, ok := ld.unlimitedIPs.Load(email); ok {
		ic := v.(*ipCounter)
		if len(ic.aliveIPs()) != 0 {
			t.Fatalf("unlimited bucket after switch back to limited = %v, want empty", ic.aliveIPs())
		}
	}
	ld.delConn(email, "2.2.2.2")
	// With the slot freed, a new device under the limit is admitted.
	if ld.checkDeviceLimit(email, "3.3.3.3") {
		t.Fatal("device slot should be free after delConn in the limited bucket")
	}
}

func TestLimitDispatcher_DelConnBothBuckets(t *testing.T) {
	// A count stranded in the limited bucket (e.g. admitted just before
	// a limit switch) must still be released even when an unlimited
	// entry for the same user exists.
	ld := newTestDispatcher()
	email := userEmail(1)
	ld.UpdateLimits(map[string]int{email: 1}, map[string]int{email: 2}, nil)
	if ld.checkDeviceLimit(email, "1.1.1.1") {
		t.Fatal("first device should be admitted")
	}
	// Simulate a stranded state: unlimited entry exists, limited count too.
	ld.unlimitedIPs.Store(email, &ipCounter{})
	ld.delConn(email, "1.1.1.1")
	ld.mu.RLock()
	remaining := len(ld.limitedIPs[email])
	ld.mu.RUnlock()
	if remaining != 0 {
		t.Fatalf("limited bucket after delConn = %d entries, want 0", remaining)
	}
	if ld.checkDeviceLimit(email, "2.2.2.2") {
		t.Fatal("device slot should be free after delConn")
	}
}

func TestLimitDispatcher_CheckDeviceLimitConcurrentSameIP(t *testing.T) {
	// Concurrent limited-user admissions of one IP must refcount exactly
	// (write-locked recheck, no -race findings) and drain to zero.
	ld := newTestDispatcher()
	email := userEmail(1)
	ld.UpdateLimits(map[string]int{email: 1}, map[string]int{email: 1}, nil)

	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ld.checkDeviceLimit(email, "1.1.1.1") {
				t.Error("same-IP admission rejected concurrently")
			}
		}()
	}
	wg.Wait()
	ld.mu.RLock()
	count := ld.limitedIPs[email]["1.1.1.1"]
	ld.mu.RUnlock()
	if count != n {
		t.Fatalf("limited refcount = %d, want %d", count, n)
	}
	for i := 0; i < n; i++ {
		ld.delConn(email, "1.1.1.1")
	}
	ld.mu.RLock()
	_, stillThere := ld.limitedIPs[email]
	ld.mu.RUnlock()
	if stillThere {
		t.Fatal("limited entry should be gone after all connections close")
	}
}

func TestLedgerAfterUserOps(t *testing.T) {
	users := []model.UserSpec{{ID: 1, UUID: "uuid-1"}, {ID: 2, UUID: "uuid-2"}, {ID: 3, UUID: "uuid-3"}}
	toRemove := []model.UserSpec{{ID: 4, UUID: "uuid-4"}}

	// Failed add excluded, failed remove retained.
	ledger := ledgerAfterUserOps(users, toRemove, map[int]bool{2: true}, map[int]bool{4: true})
	ids := map[int]bool{}
	for _, u := range ledger {
		ids[u.ID] = true
	}
	if ids[2] {
		t.Fatal("failed add (ID 2) must not enter the ledger")
	}
	if !ids[1] || !ids[3] || !ids[4] {
		t.Fatalf("ledger IDs = %v, want 1, 3, 4", ids)
	}

	// No failures: ledger equals the intended set.
	ledger = ledgerAfterUserOps(users, nil, nil, nil)
	if len(ledger) != 3 {
		t.Fatalf("ledger length = %d, want 3", len(ledger))
	}
}

func TestBuildTrojanDoesNotMutateNodeSpec(t *testing.T) {
	nc := &panel.NodeConfig{Protocol: "trojan", ServerPort: 443, TLS: 0}
	spec := testNodeSpec(nc)
	inbound := buildTrojan(M{
		"tag": "trojan-in", "listen": "::", "port": 443, "protocol": "trojan",
		"streamSettings": M{"sockopt": M{"reusePort": true}},
	}, spec, testUsers, kernel.TLSCert{CertPEM: []byte("CERT"), KeyPEM: []byte("KEY")})
	if spec.TLS != 0 {
		t.Fatalf("NodeSpec.TLS mutated to %d, want 0 (forced TLS must use a copy)", spec.TLS)
	}
	ss, ok := inbound["streamSettings"].(M)
	if !ok || ss["security"] != "tls" {
		t.Fatalf("trojan streamSettings = %v, want security=tls from the copy", inbound["streamSettings"])
	}
}

func TestBuildConfig_HysteriaVersion(t *testing.T) {
	// Hysteria v1 must be a hard error, not a Warn + nil inbound that
	// "starts successfully" with no listener.
	nc := &panel.NodeConfig{Protocol: "hysteria", ServerPort: 443, Version: 1}
	if _, err := buildConfig(testKernelCfg, testNodeSpec(nc), testUsers, kernel.TLSCert{}); err == nil {
		t.Fatal("buildConfig(hysteria v1) should error")
	}

	// Hysteria v2 builds fine.
	nc.Version = 2
	cfg, err := buildConfig(testKernelCfg, testNodeSpec(nc), testUsers, kernel.TLSCert{})
	if err != nil {
		t.Fatalf("buildConfig(hysteria v2) error = %v", err)
	}
	if cfg["inbounds"] == nil {
		t.Fatal("buildConfig(hysteria v2) should include an inbound")
	}

	// The literal "hysteria2" protocol string also builds (mirrors sing-box).
	nc2 := &panel.NodeConfig{Protocol: "hysteria2", ServerPort: 443}
	cfg, err = buildConfig(testKernelCfg, testNodeSpec(nc2), testUsers, kernel.TLSCert{})
	if err != nil {
		t.Fatalf("buildConfig(hysteria2) error = %v", err)
	}
	if cfg["inbounds"] == nil {
		t.Fatal("buildConfig(hysteria2) should include an inbound")
	}
}

func TestCompilePanelRouteRule_DNS(t *testing.T) {
	// dns must be handled explicitly, not silently defaulted to block.
	rules := compilePanelRouteRule(model.RouteRule{ID: 1, Match: []string{"example.com"}, Action: "dns"})
	if len(rules) != 1 || rules[0]["outboundTag"] != "direct" {
		t.Fatalf("dns without target rules = %v, want direct", rules)
	}
	rules = compilePanelRouteRule(model.RouteRule{ID: 2, Match: []string{"example.com"}, Action: "dns", ActionValue: "dns-server"})
	if len(rules) != 1 || rules[0]["outboundTag"] != "dns-server" {
		t.Fatalf("dns with target rules = %v, want dns-server", rules)
	}
}

func TestApplyStreamSettings_WS_HostPriority(t *testing.T) {
	// Unified rule with sing-box: an explicit Host header wins over the
	// host field; the host field only fills in a missing Host.
	wsHeaders := func(nc *model.NodeSpec) M {
		base := M{}
		applyStreamSettings(base, nc, kernel.TLSCert{})
		ss := base["streamSettings"].(M)
		return ss["wsSettings"].(M)["headers"].(M)
	}

	nc := &panel.NodeConfig{
		Network: "ws",
		NetworkSettings: map[string]interface{}{
			"path":    "/ws",
			"host":    "field.example",
			"headers": map[string]interface{}{"Host": "header.example"},
		},
	}
	if got := wsHeaders(testNodeSpec(nc)); got["Host"] != "header.example" {
		t.Fatalf("Host = %v, want header.example", got["Host"])
	}

	nc.NetworkSettings = map[string]interface{}{
		"path":    "/ws",
		"host":    "field.example",
		"headers": map[string]interface{}{"X-Custom": "1"},
	}
	if got := wsHeaders(testNodeSpec(nc)); got["Host"] != "field.example" || got["X-Custom"] != "1" {
		t.Fatalf("headers = %v, want Host=field.example plus X-Custom", got)
	}

	nc.NetworkSettings = map[string]interface{}{"path": "/ws", "host": "field.example"}
	if got := wsHeaders(testNodeSpec(nc)); got["Host"] != "field.example" {
		t.Fatalf("Host = %v, want field.example", got["Host"])
	}
}

func TestXrayRemoveUsersClearsLedgerAndHash(t *testing.T) {
	x := New(testKernelCfg)
	x.instance = new(xrayCore.Instance)
	ld := newTestDispatcher()
	x.limitDispatcher = ld
	x.users = []model.UserSpec{{ID: 1, UUID: "uuid-1"}}
	x.lastKernelHash = "somehash"
	x.running.Store(true)

	removed, err := x.RemoveUsers([]model.UserSpec{{ID: 1, UUID: "uuid-1"}})
	if err != nil {
		t.Fatalf("RemoveUsers() error = %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	if len(x.users) != 0 {
		t.Fatalf("x.users = %v, want empty after last user removed", x.users)
	}
	if x.lastKernelHash != "" {
		t.Fatalf("lastKernelHash = %q, want empty so a later Reload restarts", x.lastKernelHash)
	}
	if x.IsRunning() {
		t.Fatal("expected kernel stopped after last user removed")
	}
}

func TestDrainConnsBounded(t *testing.T) {
	// drainConns must return once its timeout elapses even when
	// connections never drain, so a stuck drain cannot block Stop or a
	// background recycle forever (hard close follows in both paths).
	ld := newTestDispatcher()
	ld.connCount.Store(1)
	start := time.Now()
	drainConns(ld, 50*time.Millisecond)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("drainConns took %v, want ~50ms", elapsed)
	}
	if got := ld.connCount.Load(); got != 1 {
		t.Fatalf("drainConns must not clear counts itself, connCount = %d", got)
	}
}

func TestLimitDispatcherDeviceLimitRejectedCallback(t *testing.T) {
	ld := newTestDispatcher()
	email := userEmail(1)
	ld.UpdateLimits(map[string]int{email: 1}, map[string]int{email: 1}, nil)
	var rejected atomic.Int32
	ld.SetDeviceLimitRejectedFunc(func() { rejected.Add(1) })

	// Admitted: no callback.
	if ld.checkDeviceLimit(email, "1.1.1.1") {
		t.Fatal("first device should be admitted")
	}
	if got := rejected.Load(); got != 0 {
		t.Fatalf("rejected = %d after admission, want 0", got)
	}
	// Rejected: callback fires exactly once.
	if !ld.checkDeviceLimit(email, "2.2.2.2") {
		t.Fatal("second device should be rejected")
	}
	if got := rejected.Load(); got != 1 {
		t.Fatalf("rejected = %d after rejection, want 1", got)
	}
	// Already-known IP admitted: no callback.
	if ld.checkDeviceLimit(email, "1.1.1.1") {
		t.Fatal("known IP should be admitted")
	}
	if got := rejected.Load(); got != 1 {
		t.Fatalf("rejected = %d after known-IP admission, want 1", got)
	}
	// Clearing the callback with nil must not panic on later rejections.
	ld.SetDeviceLimitRejectedFunc(nil)
	if !ld.checkDeviceLimit(email, "3.3.3.3") {
		t.Fatal("third device should still be rejected without a callback")
	}
	if got := rejected.Load(); got != 1 {
		t.Fatalf("rejected = %d after clearing callback, want 1", got)
	}
}

func TestXraySetDeviceLimitRejectedFuncForwardsAndReapplies(t *testing.T) {
	email := userEmail(1)
	var rejected atomic.Int32
	cb := func() { rejected.Add(1) }

	// Setter before any dispatcher exists: stored, then applied by
	// updateDispatcherLimits when a (new) dispatcher appears — the
	// restart path.
	x := New(testKernelCfg)
	x.SetDeviceLimitRejectedFunc(cb)
	ld := newTestDispatcher()
	x.limitDispatcher = ld
	x.updateDispatcherLimits([]model.UserSpec{{ID: 1, UUID: "uuid-1", DeviceLimit: 1}})
	if ld.checkDeviceLimit(email, "1.1.1.1") {
		t.Fatal("first device should be admitted")
	}
	if !ld.checkDeviceLimit(email, "2.2.2.2") {
		t.Fatal("second device should be rejected")
	}
	if got := rejected.Load(); got != 1 {
		t.Fatalf("rejected after reapply = %d, want 1", got)
	}

	// Setter with a dispatcher already present: forwarded immediately.
	x2 := New(testKernelCfg)
	ld2 := newTestDispatcher()
	x2.limitDispatcher = ld2
	ld2.UpdateLimits(map[string]int{email: 1}, map[string]int{email: 1}, nil)
	x2.SetDeviceLimitRejectedFunc(cb)
	if ld2.checkDeviceLimit(email, "1.1.1.1") {
		t.Fatal("first device should be admitted")
	}
	if !ld2.checkDeviceLimit(email, "2.2.2.2") {
		t.Fatal("second device should be rejected")
	}
	if got := rejected.Load(); got != 2 {
		t.Fatalf("rejected after forward = %d, want 2", got)
	}
}

func TestRetireCancelledLimiters(t *testing.T) {
	// Regression test for speed-limit cancel on xray connections that
	// do not hold the shared SpeedTracker bucket. xray-core wraps each
	// connection's writer in a RateLimitWriter holding the limiter
	// pointer fetched from the bandwidth manager at dispatch time, so
	// merely deleting/replacing the manager entry leaves live
	// connections throttled on the old bucket. updateBandwidthLimits
	// therefore retires the manager's current entry in place before
	// Reset(); this test covers that helper directly, including a
	// fallback-style bucket created by SetUserLimit (fn == nil path)
	// that SpeedTracker can never retire on its own.
	bm := featurebandwidth.New()
	// User 1: fallback-style private bucket, 1000 Bps.
	bm.SetUserLimit(userEmail(1), 1000)
	// User 2: shared-style bucket, still limited.
	shared := rate.NewLimiter(rate.Limit(2000), 2000)
	bm.SetUserLimiter(userEmail(2), shared)
	// User 3: no manager entry at all — must not panic.
	retireCancelledLimiters(bm, []model.UserSpec{
		{ID: 1, UUID: "uuid-1", SpeedLimit: 0},
		{ID: 2, UUID: "uuid-2", SpeedLimit: 100},
		{ID: 3, UUID: "uuid-3", SpeedLimit: 0},
	})
	if lim := bm.GetUserLimiter(userEmail(1)); lim == nil || lim.Limit() != rate.Inf {
		t.Fatalf("cancelled user 1 limiter = %v, want Inf (retired in place)", lim)
	}
	// A retired bucket must let a huge write through immediately —
	// this is the actual unthrottle behaviour live connections see.
	if err := bm.GetUserLimiter(userEmail(1)).WaitN(context.Background(), 1<<30); err != nil {
		t.Fatalf("retired bucket WaitN error = %v, want immediate success", err)
	}
	if lim := bm.GetUserLimiter(userEmail(2)); lim == nil || lim.Limit() != rate.Limit(2000) {
		t.Fatalf("still-limited user 2 limiter = %v, want 2000 Bps untouched", lim)
	}
	if lim := bm.GetUserLimiter(userEmail(2)); lim != shared {
		t.Fatalf("still-limited user 2 limiter pointer changed, want the same bucket")
	}
}

func TestSpeedTrackerCancelRetiresSharedBucket(t *testing.T) {
	// Guards the normal shared-pointer path: when the bandwidth
	// manager holds the SpeedTracker bucket itself, cancelling the
	// limit retires that bucket in place, so existing xray
	// connections holding the same pointer stop being throttled
	// without any kernel-side intervention.
	l := limiter.New()
	l.UpdateUsers([]model.UserSpec{{ID: 1, UUID: "uuid-1", SpeedLimit: 100}})
	st := limiter.NewSpeedTracker(l)
	st.UpdateBuckets()
	bucket := st.GetLimiter("uuid-1")
	if bucket == nil {
		t.Fatal("GetLimiter returned nil for a limited user")
	}
	bm := featurebandwidth.New()
	bm.SetUserLimiter(userEmail(1), bucket)

	l.UpdateUsers([]model.UserSpec{{ID: 1, UUID: "uuid-1", SpeedLimit: 0}})
	st.UpdateBuckets()
	if bucket.Limit() != rate.Inf {
		t.Fatalf("shared bucket limit after cancel = %v, want Inf", bucket.Limit())
	}
	if err := bm.GetUserLimiter(userEmail(1)).WaitN(context.Background(), 1<<30); err != nil {
		t.Fatalf("manager-held bucket WaitN error = %v, want immediate success", err)
	}
}
