package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cedar2025/xboard-node/internal/cert"
	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/controlplane"
	"github.com/cedar2025/xboard-node/internal/kernel"
	"github.com/cedar2025/xboard-node/internal/limiter"
	"github.com/cedar2025/xboard-node/internal/model"
	"github.com/cedar2025/xboard-node/internal/tracker"
	"golang.org/x/time/rate"
)

type fakeCertManager struct {
	cert        kernel.TLSCert
	renewed     bool
	fileChanged bool

	reconfigureCalls   int
	lastReconfigure    config.CertConfig
	reconfigureErr     error
	reconfigureChanged bool
}

func (f *fakeCertManager) Start(context.Context) error { return nil }
func (f *fakeCertManager) Stop()                       {}
func (f *fakeCertManager) Reconfigure(_ context.Context, cfg config.CertConfig) (bool, error) {
	f.reconfigureCalls++
	f.lastReconfigure = cfg
	return f.reconfigureChanged, f.reconfigureErr
}
func (f *fakeCertManager) TLSCert() kernel.TLSCert { return f.cert }
func (f *fakeCertManager) CertRenewed() bool {
	r := f.renewed
	f.renewed = false
	return r
}
func (f *fakeCertManager) HasCert() bool { return len(f.cert.CertPEM) > 0 }
func (f *fakeCertManager) CheckFileChanged() bool {
	c := f.fileChanged
	f.fileChanged = false
	return c
}

type fakeKernel struct {
	running bool

	reloadCalls   int
	lastReloadTLS kernel.TLSCert

	startErr  error
	updateErr error
	addErr    error
	removeErr error

	// failFirstUpdates makes the first N UpdateUsers calls fail, so a
	// test can fail the fallback but let a later rollback succeed.
	failFirstUpdates int

	startCalls  int
	updateCalls int
	addCalls    int
	removeCalls int
	closeCalls  int

	closedUUIDs  []string
	trafficAlive map[int]map[string]bool
	traffic      map[int][2]int64

	onUpdateUsers func([]model.UserSpec)
	onAddUsers    func([]model.UserSpec)
	onRemoveUsers func([]model.UserSpec)

	speedLimitFunc  func(string) *rate.Limiter
	deviceLimitFunc func(string) (int, bool)
}

func (f *fakeKernel) Name() string                      { return "fake" }
func (f *fakeKernel) Protocols() []string               { return []string{"vless"} }
func (f *fakeKernel) Capabilities() kernel.Capabilities { return kernel.Capabilities{} }
func (f *fakeKernel) Start(nodeConfig *model.NodeSpec, users []model.UserSpec, tls kernel.TLSCert) error {
	_, _, _ = nodeConfig, users, tls
	f.startCalls++
	if f.startErr != nil {
		return f.startErr
	}
	f.running = true
	return nil
}
func (f *fakeKernel) Stop()           { f.running = false }
func (f *fakeKernel) IsRunning() bool { return f.running }
func (f *fakeKernel) Reload(nodeConfig *model.NodeSpec, users []model.UserSpec, tls kernel.TLSCert) error {
	f.reloadCalls++
	f.lastReloadTLS = tls
	_, _ = nodeConfig, users
	return nil
}
func (f *fakeKernel) AddUsers(users []model.UserSpec) (int, error) {
	f.addCalls++
	if f.onAddUsers != nil {
		f.onAddUsers(users)
	}
	if f.addErr != nil {
		return 0, f.addErr
	}
	return len(users), nil
}
func (f *fakeKernel) RemoveUsers(users []model.UserSpec) (int, error) {
	f.removeCalls++
	if f.onRemoveUsers != nil {
		f.onRemoveUsers(users)
	}
	if f.removeErr != nil {
		return 0, f.removeErr
	}
	return len(users), nil
}
func (f *fakeKernel) UpdateUsers(users []model.UserSpec) (int, int, error) {
	f.updateCalls++
	if f.onUpdateUsers != nil {
		f.onUpdateUsers(users)
	}
	if f.updateErr != nil {
		return 0, 0, f.updateErr
	}
	if f.failFirstUpdates > 0 {
		f.failFirstUpdates--
		return 0, 0, errors.New("update failed")
	}
	return len(users), 0, nil
}
func (f *fakeKernel) GetUserTraffic(ctx context.Context) (map[int][2]int64, map[int]map[string]bool, int, error) {
	_ = ctx
	return f.traffic, f.trafficAlive, len(f.trafficAlive), nil
}
func (f *fakeKernel) CloseConnection(ctx context.Context, connID string) error {
	_, _ = ctx, connID
	return nil
}
func (f *fakeKernel) CloseUserConnections(ctx context.Context, uuid string) error {
	_, _ = ctx, uuid
	f.closeCalls++
	f.closedUUIDs = append(f.closedUUIDs, uuid)
	return nil
}
func (f *fakeKernel) SetSpeedLimitFunc(fn func(uuid string) *rate.Limiter) { f.speedLimitFunc = fn }
func (f *fakeKernel) SetDeviceLimitFunc(fn func(uuid string) (int, bool))  { f.deviceLimitFunc = fn }
func (f *fakeKernel) UpdateGlobalDevices(users map[int][]string)           { _ = users }
func (f *fakeKernel) ClearGlobalDevices()                                  {}

func newTestService(k *fakeKernel) *Service {
	sharedLimiter := limiter.New()
	s := &Service{
		kernel:       k,
		tracker:      tracker.New(),
		limiter:      sharedLimiter,
		speedTracker: limiter.NewSpeedTracker(sharedLimiter),
		cert:         cert.NewManager(config.CertConfig{}),
	}
	k.SetSpeedLimitFunc(s.speedTracker.GetLimiter)
	k.SetDeviceLimitFunc(s.limiter.GetDeviceLimitByUUID)
	return s
}

func TestApplyUserUpdatePreparesLimiterBeforeKernelUpdate(t *testing.T) {
	k := &fakeKernel{running: true}
	s := newTestService(k)
	s.lastConfig = &model.NodeSpec{Protocol: "vless"}
	oldUsers := []model.UserSpec{{ID: 1, UUID: "uuid-old", SpeedLimit: 4}}
	s.updateUserState(oldUsers)

	newUsers := []model.UserSpec{{ID: 2, UUID: "uuid-new", SpeedLimit: 8}}
	k.onUpdateUsers = func(users []model.UserSpec) {
		if len(users) != 1 || users[0].UUID != "uuid-new" {
			t.Fatalf("unexpected users passed to UpdateUsers: %#v", users)
		}
		if got := k.speedLimitFunc("uuid-new"); got == nil {
			t.Fatal("expected new user's limiter to be visible before kernel UpdateUsers")
		}
	}

	s.applyUserUpdate(context.Background(), newUsers, computeUserHash(newUsers))

	if got := k.updateCalls; got != 1 {
		t.Fatalf("UpdateUsers call count = %d, want 1", got)
	}
	if len(s.lastUsers) != 1 || s.lastUsers[0].UUID != "uuid-new" {
		t.Fatalf("lastUsers = %#v, want new users", s.lastUsers)
	}
	if s.speedTracker.GetLimiter("uuid-new") == nil {
		t.Fatal("expected limiter for new user after successful update")
	}
}

func TestApplyUserUpdateRestoresStateWhenKernelAndRestartFail(t *testing.T) {
	k := &fakeKernel{
		running:   true,
		updateErr: errors.New("update failed"),
		startErr:  errors.New("restart failed"),
	}
	s := newTestService(k)
	s.lastConfig = &model.NodeSpec{Protocol: "vless"}
	oldUsers := []model.UserSpec{{ID: 1, UUID: "uuid-old", SpeedLimit: 4}}
	s.updateUserState(oldUsers)
	oldHash := s.lastUserHash

	newUsers := []model.UserSpec{{ID: 2, UUID: "uuid-new", SpeedLimit: 8}}
	s.applyUserUpdate(context.Background(), newUsers, computeUserHash(newUsers))

	if got := k.startCalls; got != 1 {
		t.Fatalf("Start call count = %d, want 1", got)
	}
	if len(s.lastUsers) != 1 || s.lastUsers[0].UUID != "uuid-old" {
		t.Fatalf("lastUsers = %#v, want restored old users", s.lastUsers)
	}
	if s.lastUserHash != oldHash {
		t.Fatalf("lastUserHash = %q, want %q", s.lastUserHash, oldHash)
	}
	if s.speedTracker.GetLimiter("uuid-old") == nil {
		t.Fatal("expected old limiter to be restored after rollback")
	}
	if s.speedTracker.GetLimiter("uuid-new") != nil {
		t.Fatal("expected new limiter to be removed after rollback")
	}
}

func TestApplyUserDeltaAddPreparesLimiterBeforeKernelUpdate(t *testing.T) {
	k := &fakeKernel{running: true}
	s := newTestService(k)
	s.lastConfig = &model.NodeSpec{Protocol: "vless"}
	oldUsers := []model.UserSpec{{ID: 1, UUID: "uuid-old", SpeedLimit: 4}}
	s.updateUserState(oldUsers)

	delta := []model.UserSpec{{ID: 2, UUID: "uuid-new", SpeedLimit: 8}}
	k.onAddUsers = func(users []model.UserSpec) {
		if len(users) != 1 || users[0].UUID != "uuid-new" {
			t.Fatalf("unexpected users passed to AddUsers: %#v", users)
		}
		if got := k.speedLimitFunc("uuid-new"); got == nil {
			t.Fatal("expected delta user's limiter to be visible before kernel AddUsers")
		}
	}

	s.applyUserDelta(context.Background(), "add", delta)

	if got := k.addCalls; got != 1 {
		t.Fatalf("AddUsers call count = %d, want 1", got)
	}
	if s.speedTracker.GetLimiter("uuid-new") == nil {
		t.Fatal("expected limiter for delta-added user after successful update")
	}
}

func TestValidateNodeRuntimeRejectsUnsupportedDNSProvider(t *testing.T) {
	cfg := &config.Config{Kernel: config.KernelConfig{Type: "singbox"}}
	err := validateNodeRuntime(cfg, []string{"http"}, &model.NodeSpec{
		Protocol: "http",
		CertConfig: &config.CertConfig{
			CertMode:    "dns",
			DNSProvider: "3123123",
			Domain:      "example.com",
		},
	}, kernel.TLSCert{CertPEM: []byte("CERT"), KeyPEM: []byte("KEY")})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if err.Error() == "" {
		t.Fatal("expected non-empty error")
	}
	if got := err.Error(); !strings.HasPrefix(got, `unsupported cert_config.dns_provider "3123123" (supported: `) {
		t.Fatalf("unexpected error: %v", got)
	}
}

func TestValidateNodeRuntimeAllowsSelfManagedTLSBeforeFilesExist(t *testing.T) {
	cfg := &config.Config{Kernel: config.KernelConfig{Type: "singbox"}}
	err := validateNodeRuntime(cfg, []string{"anytls", "hysteria"}, &model.NodeSpec{
		Protocol: "anytls",
		CertConfig: &config.CertConfig{
			CertMode: "self",
			Domain:   "example.com",
		},
	}, kernel.TLSCert{})
	if err != nil {
		t.Fatalf("expected self-managed TLS config to pass validation, got %v", err)
	}
}

func TestValidateNodeRuntimeAllowsSingboxRealityWithRequiredFields(t *testing.T) {
	cfg := &config.Config{Kernel: config.KernelConfig{Type: "singbox"}}
	err := validateNodeRuntime(cfg, []string{"vless"}, &model.NodeSpec{
		Protocol: "vless",
		TLS:      2,
		TLSSettings: map[string]any{
			"private_key": "test-key",
			"server_name": "example.com",
		},
	}, kernel.TLSCert{CertPEM: []byte("CERT"), KeyPEM: []byte("KEY")})
	if err != nil {
		t.Fatalf("expected sing-box reality validation to pass, got %v", err)
	}
}

func TestValidateNodeRuntimeRejectsRealityWithoutTLSSettings(t *testing.T) {
	cfg := &config.Config{Kernel: config.KernelConfig{Type: "singbox"}}
	err := validateNodeRuntime(cfg, []string{"vless"}, &model.NodeSpec{
		Protocol: "vless",
		TLS:      2,
	}, kernel.TLSCert{CertPEM: []byte("CERT"), KeyPEM: []byte("KEY")})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if got := err.Error(); got != "reality tls requires tls_settings" {
		t.Fatalf("unexpected error: %v", got)
	}
}

func TestValidateNodeRuntimeRejectsRealityWithoutPrivateKey(t *testing.T) {
	cfg := &config.Config{Kernel: config.KernelConfig{Type: "singbox"}}
	err := validateNodeRuntime(cfg, []string{"vless"}, &model.NodeSpec{
		Protocol: "vless",
		TLS:      2,
		TLSSettings: map[string]any{
			"server_name": "example.com",
		},
	}, kernel.TLSCert{CertPEM: []byte("CERT"), KeyPEM: []byte("KEY")})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if got := err.Error(); got != "reality tls requires tls_settings.private_key" {
		t.Fatalf("unexpected error: %v", got)
	}
}

func TestValidateNodeRuntimeRejectsRealityWithoutServerNameOrDest(t *testing.T) {
	cfg := &config.Config{Kernel: config.KernelConfig{Type: "singbox"}}
	err := validateNodeRuntime(cfg, []string{"vless"}, &model.NodeSpec{
		Protocol: "vless",
		TLS:      2,
		TLSSettings: map[string]any{
			"private_key": "test-key",
		},
	}, kernel.TLSCert{CertPEM: []byte("CERT"), KeyPEM: []byte("KEY")})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if got := err.Error(); got != "reality tls requires tls_settings.server_name or tls_settings.dest" {
		t.Fatalf("unexpected error: %v", got)
	}
}

func TestApplyUserUpdateClosesRemovedUsersConnections(t *testing.T) {
	k := &fakeKernel{running: true}
	s := newTestService(k)
	s.lastConfig = &model.NodeSpec{Protocol: "vless"}
	oldUsers := []model.UserSpec{{ID: 1, UUID: "uuid-old", SpeedLimit: 4}, {ID: 2, UUID: "uuid-kept", SpeedLimit: 4}}
	s.updateUserState(oldUsers)

	newUsers := []model.UserSpec{{ID: 2, UUID: "uuid-kept", SpeedLimit: 4}}
	s.applyUserUpdate(context.Background(), newUsers, computeUserHash(newUsers))

	if k.closeCalls != 1 || len(k.closedUUIDs) != 1 || k.closedUUIDs[0] != "uuid-old" {
		t.Fatalf("CloseUserConnections calls = %v, want [uuid-old]", k.closedUUIDs)
	}
}

func TestApplyUserDeltaRemoveClosesRemovedUsersConnections(t *testing.T) {
	k := &fakeKernel{running: true}
	s := newTestService(k)
	s.lastConfig = &model.NodeSpec{Protocol: "vless"}
	oldUsers := []model.UserSpec{{ID: 1, UUID: "uuid-old", SpeedLimit: 4}, {ID: 2, UUID: "uuid-kept", SpeedLimit: 4}}
	s.updateUserState(oldUsers)

	s.applyUserDelta(context.Background(), "remove", []model.UserSpec{{ID: 1, UUID: "uuid-old"}})

	if k.closeCalls != 1 || len(k.closedUUIDs) != 1 || k.closedUUIDs[0] != "uuid-old" {
		t.Fatalf("CloseUserConnections calls = %v, want [uuid-old]", k.closedUUIDs)
	}
	if len(s.lastUsers) != 1 || s.lastUsers[0].ID != 2 {
		t.Fatalf("lastUsers = %#v, want only user 2", s.lastUsers)
	}
}

// Regression: panel delta-remove payloads carry only the user ID
// (CheckTrafficExceeded / notifyUserChanged send [{'id': id}]), so the
// backend must resolve the UUID from its pre-delta user set. Closing by
// the empty UUID is a silent no-op in both kernels and also leaves no
// tombstone for the trackAndEnforce safety net, which is exactly how an
// out-of-quota user's existing connection survived indefinitely while
// reconnects were already blocked.
func TestApplyUserDeltaRemoveWithIDOnlyClosesConnections(t *testing.T) {
	k := &fakeKernel{running: true}
	s := newTestService(k)
	s.lastConfig = &model.NodeSpec{Protocol: "vless"}
	oldUsers := []model.UserSpec{{ID: 1, UUID: "uuid-old", SpeedLimit: 4}, {ID: 2, UUID: "uuid-kept", SpeedLimit: 4}}
	s.updateUserState(oldUsers)

	s.applyUserDelta(context.Background(), "remove", []model.UserSpec{{ID: 1}})

	if k.closeCalls != 1 || len(k.closedUUIDs) != 1 || k.closedUUIDs[0] != "uuid-old" {
		t.Fatalf("CloseUserConnections calls = %v, want [uuid-old]", k.closedUUIDs)
	}
	if got := s.removedUserUUIDs[1]; got != "uuid-old" {
		t.Fatalf("removedUserUUIDs[1] = %q, want uuid-old (safety-net tombstone)", got)
	}
	if len(s.lastUsers) != 1 || s.lastUsers[0].ID != 2 {
		t.Fatalf("lastUsers = %#v, want only user 2", s.lastUsers)
	}
}

func TestTrackAndEnforceClosesStrayRemovedUserConnections(t *testing.T) {
	k := &fakeKernel{
		running:      true,
		trafficAlive: map[int]map[string]bool{1: {"1.1.1.1": true}, 2: {"2.2.2.2": true}},
	}
	s := newTestService(k)
	s.lastConfig = &model.NodeSpec{Protocol: "vless"}
	// User 2 is current; user 1 was removed earlier (tombstone recorded).
	s.updateUserState([]model.UserSpec{{ID: 2, UUID: "uuid-kept", SpeedLimit: 4}})
	s.removedUserUUIDs = map[int]string{1: "uuid-old"}

	s.trackAndEnforce(context.Background())

	if len(k.closedUUIDs) != 1 || k.closedUUIDs[0] != "uuid-old" {
		t.Fatalf("CloseUserConnections calls = %v, want [uuid-old] only", k.closedUUIDs)
	}
}

// TestTrackAndEnforce_CertRenewalReloadsKernel covers the WS-connected
// renewal gap: a renewed certificate (or a changed file-mode pair) must
// reload the kernel on the track tick, a stale flag for already-applied
// material must not cause a second reload, and no reload happens when
// the kernel is not running.
func TestTrackAndEnforce_CertRenewalReloadsKernel(t *testing.T) {
	k := &fakeKernel{running: true}
	s := newTestService(k)
	s.lastConfig = &model.NodeSpec{Protocol: "vless"}
	s.updateUserState([]model.UserSpec{{ID: 1, UUID: "uuid-1", SpeedLimit: 4}})

	oldTLS := kernel.TLSCert{CertPEM: []byte("old-cert"), KeyPEM: []byte("old-key")}
	newTLS := kernel.TLSCert{CertPEM: []byte("new-cert"), KeyPEM: []byte("new-key")}
	fc := &fakeCertManager{cert: newTLS, renewed: true}
	s.cert = fc
	s.appliedState.TLS = oldTLS

	s.trackAndEnforce(context.Background())
	if k.reloadCalls != 1 || string(k.lastReloadTLS.CertPEM) != "new-cert" {
		t.Fatalf("reloadCalls=%d lastCert=%q, want 1 / new-cert", k.reloadCalls, k.lastReloadTLS.CertPEM)
	}
	if string(s.appliedState.TLS.CertPEM) != "new-cert" {
		t.Fatalf("applied TLS not updated: %q", s.appliedState.TLS.CertPEM)
	}

	// Stale flag, same material: no second reload.
	fc.renewed = true
	s.trackAndEnforce(context.Background())
	if k.reloadCalls != 1 {
		t.Fatalf("stale renewal flag caused a second reload: %d calls", k.reloadCalls)
	}

	// File-mode change with new material: reloads again.
	newerTLS := kernel.TLSCert{CertPEM: []byte("newer-cert"), KeyPEM: []byte("newer-key")}
	fc.cert = newerTLS
	fc.fileChanged = true
	s.trackAndEnforce(context.Background())
	if k.reloadCalls != 2 || string(k.lastReloadTLS.CertPEM) != "newer-cert" {
		t.Fatalf("file change: reloadCalls=%d lastCert=%q, want 2 / newer-cert", k.reloadCalls, k.lastReloadTLS.CertPEM)
	}

	// Kernel stopped: flags are consumed but no reload is attempted.
	k.running = false
	fc.renewed = true
	s.trackAndEnforce(context.Background())
	if k.reloadCalls != 2 {
		t.Fatalf("reload attempted while kernel stopped: %d calls", k.reloadCalls)
	}
}

// ─── Regression tests for the service/limiter fix batch ────────────────

type fakePushClient struct {
	name      string
	connected bool
	runCalls  int

	deviceReportCalls int
	lastDevices       map[int][]string
	sendErr           error
}

func (f *fakePushClient) Run(context.Context) { f.runCalls++ }
func (f *fakePushClient) IsConnected() bool   { return f.connected }
func (f *fakePushClient) SendDeviceReport(devices map[int][]string) error {
	f.deviceReportCalls++
	f.lastDevices = devices
	return f.sendErr
}

type fakeSource struct {
	bootstrap         controlplane.Bootstrap
	bootstrapErr      error
	discoverClient    controlplane.PushClient
	discoverErr       error
	supportsDiscovery bool
	supportsPolling   bool
}

func (f *fakeSource) Initial(context.Context, func() map[string]interface{}, chan<- controlplane.Event, chan<- controlplane.StatusChange) (controlplane.Bootstrap, error) {
	return f.bootstrap, f.bootstrapErr
}
func (f *fakeSource) Poll(context.Context) (controlplane.Snapshot, error) {
	return controlplane.Snapshot{}, nil
}
func (f *fakeSource) Discover(context.Context, func() map[string]interface{}, chan<- controlplane.Event, chan<- controlplane.StatusChange) (controlplane.PushClient, error) {
	return f.discoverClient, f.discoverErr
}
func (f *fakeSource) Metrics() controlplane.APIMetrics { return controlplane.APIMetrics{} }
func (f *fakeSource) SupportsPolling() bool            { return f.supportsPolling }
func (f *fakeSource) SupportsDiscovery() bool          { return f.supportsDiscovery }

type fakeSink struct {
	reportDevicesCalls int
	lastDevices        map[int][]string
}

func (f *fakeSink) Report(controlplane.ReportPayload) error { return nil }
func (f *fakeSink) ReportDevices(_ controlplane.PushClient, devices map[int][]string) {
	f.reportDevicesCalls++
	f.lastDevices = devices
}
func (f *fakeSink) SupportsReporting() bool     { return true }
func (f *fakeSink) SupportsDeviceReports() bool { return true }

type fakeDeviceSender struct {
	err error
	got map[int][]string
}

func (f *fakeDeviceSender) SendDeviceReport(devices map[int][]string) error {
	f.got = devices
	return f.err
}

// Item 1: vless/vmess with TLS == 1 (plain TLS) require a certificate;
// TLS == 0 needs none, TLS == 2 (Reality) is validated separately.
func TestValidateTLSRequirementsVlessVmessTLS1NeedsCert(t *testing.T) {
	cases := []struct {
		name    string
		proto   string
		tlsMode int
		tls     kernel.TLSCert
		certCfg *config.CertConfig
		wantErr bool
	}{
		{"vless tls1 no cert", "vless", 1, kernel.TLSCert{}, nil, true},
		{"vmess tls1 no cert", "vmess", 1, kernel.TLSCert{}, nil, true},
		{"vless tls1 with cert", "vless", 1, kernel.TLSCert{CertPEM: []byte("C"), KeyPEM: []byte("K")}, nil, false},
		{"vmess tls1 self certconfig", "vmess", 1, kernel.TLSCert{}, &config.CertConfig{CertMode: "self", Domain: "example.com"}, false},
		{"vless tls0 no cert", "vless", 0, kernel.TLSCert{}, nil, false},
		{"vmess tls0 no cert", "vmess", 0, kernel.TLSCert{}, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := &model.NodeSpec{Protocol: tc.proto, TLS: tc.tlsMode, CertConfig: tc.certCfg}
			err := validateTLSRequirements(spec, tc.tls, "singbox")
			if tc.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected nil error, got %v", err)
			}
		})
	}
}

// Item 2 (full update): a stopped kernel must not drop the update. With
// no config yet the state lands, the removal tombstone is recorded, and
// a pending sync is marked; once config arrives, ensureRunning builds
// the kernel from the new state.
func TestApplyUserUpdateKernelStoppedLandsState(t *testing.T) {
	k := &fakeKernel{running: false}
	s := newTestService(k)
	s.lastConfig = nil
	oldUsers := []model.UserSpec{{ID: 1, UUID: "uuid-old", SpeedLimit: 4}}
	s.updateUserState(oldUsers)

	newUsers := []model.UserSpec{{ID: 2, UUID: "uuid-new", SpeedLimit: 8}}
	s.applyUserUpdate(context.Background(), newUsers, computeUserHash(newUsers))

	if len(s.lastUsers) != 1 || s.lastUsers[0].UUID != "uuid-new" {
		t.Fatalf("lastUsers = %#v, want new users (state must land while kernel stopped)", s.lastUsers)
	}
	if !s.pendingKernelSync {
		t.Fatal("expected pendingKernelSync after update with no config/kernel")
	}
	if got := s.removedUserUUIDs[1]; got != "uuid-old" {
		t.Fatalf("removedUserUUIDs[1] = %q, want tombstone for removed user", got)
	}
	if s.speedTracker.GetLimiter("uuid-new") == nil {
		t.Fatal("expected limiter for new user after stopped-kernel update")
	}

	// Config arrives later: the kernel must start from the NEW state.
	s.lastConfig = &model.NodeSpec{Protocol: "vless"}
	if !s.ensureRunning() {
		t.Fatal("expected kernel to start once config arrives")
	}
	if s.pendingKernelSync {
		t.Fatal("expected pendingKernelSync cleared by kernel start")
	}
	if k.startCalls != 1 {
		t.Fatalf("startCalls = %d, want 1", k.startCalls)
	}
}

// Item 2 (delta add / delta remove): same landing rule for deltas.
func TestApplyUserDeltaKernelStoppedLandsState(t *testing.T) {
	k := &fakeKernel{running: false}
	s := newTestService(k)
	s.lastConfig = nil
	s.updateUserState([]model.UserSpec{
		{ID: 1, UUID: "uuid-old", SpeedLimit: 4},
		{ID: 2, UUID: "uuid-kept", SpeedLimit: 4},
	})

	// ID-only remove (panel exhaustion payload) while stopped.
	s.applyUserDelta(context.Background(), "remove", []model.UserSpec{{ID: 1}})
	if len(s.lastUsers) != 1 || s.lastUsers[0].ID != 2 {
		t.Fatalf("lastUsers = %#v, want only user 2 after stopped-kernel remove", s.lastUsers)
	}
	if got := s.removedUserUUIDs[1]; got != "uuid-old" {
		t.Fatalf("removedUserUUIDs[1] = %q, want tombstone (removal must not be lost)", got)
	}
	if !s.pendingKernelSync {
		t.Fatal("expected pendingKernelSync after stopped-kernel remove")
	}
	if k.closeCalls != 0 {
		t.Fatalf("closeCalls = %d, want 0 (no running kernel to close on)", k.closeCalls)
	}

	// Add while still stopped: merged state lands as well.
	s.applyUserDelta(context.Background(), "add", []model.UserSpec{{ID: 3, UUID: "uuid-new", SpeedLimit: 8}})
	if len(s.lastUsers) != 2 {
		t.Fatalf("lastUsers = %#v, want users 2 and 3 after stopped-kernel add", s.lastUsers)
	}
	if !s.pendingKernelSync {
		t.Fatal("expected pendingKernelSync to stay set after stopped-kernel add")
	}
}

// Item 7 + pending sync: a failed old-UUID removal on the UUID-change
// path must be logged (not silently dropped) and marked for a full sync,
// and the track tick's full sync must clear the flag.
func TestApplyUserDeltaAddUUIDChangeRemoveFailureMarksPending(t *testing.T) {
	k := &fakeKernel{running: true, removeErr: errors.New("remove failed")}
	s := newTestService(k)
	s.lastConfig = &model.NodeSpec{Protocol: "vless"}
	s.updateUserState([]model.UserSpec{{ID: 1, UUID: "uuid-old", SpeedLimit: 4}})

	s.applyUserDelta(context.Background(), "add", []model.UserSpec{{ID: 1, UUID: "uuid-new", SpeedLimit: 8}})

	if len(s.lastUsers) != 1 || s.lastUsers[0].UUID != "uuid-new" {
		t.Fatalf("lastUsers = %#v, want replaced user", s.lastUsers)
	}
	if got := s.removedUserUUIDs[1]; got != "uuid-old" {
		t.Fatalf("removedUserUUIDs[1] = %q, want old-UUID tombstone", got)
	}
	if !s.pendingKernelSync {
		t.Fatal("expected pendingKernelSync after failed old-UUID removal")
	}
	if k.closeCalls != 1 || k.closedUUIDs[0] != "uuid-old" {
		t.Fatalf("CloseUserConnections calls = %v, want [uuid-old]", k.closedUUIDs)
	}

	// Track tick reconciles with a full UpdateUsers and clears the flag.
	before := k.updateCalls
	s.trackAndEnforce(context.Background())
	if k.updateCalls != before+1 {
		t.Fatalf("updateCalls = %d, want %d (full-sync reconciliation)", k.updateCalls, before+1)
	}
	if s.pendingKernelSync {
		t.Fatal("expected pendingKernelSync cleared by successful full sync")
	}
}

// Item 9 (dual failure, kernel rollback fails): service rolls back to the
// pre-delta set, partially added users' connections are closed, and a
// pending full sync is marked — the service must never claim users the
// kernel does not have.
func TestApplyUserDeltaAddDualFailureRollsBackAndMarksPending(t *testing.T) {
	k := &fakeKernel{running: true, addErr: errors.New("add failed"), updateErr: errors.New("update failed")}
	s := newTestService(k)
	s.lastConfig = &model.NodeSpec{Protocol: "vless"}
	oldUsers := []model.UserSpec{{ID: 1, UUID: "uuid-old", SpeedLimit: 4}}
	s.updateUserState(oldUsers)

	s.applyUserDelta(context.Background(), "add", []model.UserSpec{{ID: 2, UUID: "uuid-new", SpeedLimit: 8}})

	if len(s.lastUsers) != 1 || s.lastUsers[0].UUID != "uuid-old" {
		t.Fatalf("lastUsers = %#v, want restored old users", s.lastUsers)
	}
	if s.speedTracker.GetLimiter("uuid-new") != nil {
		t.Fatal("expected no limiter for rolled-back new user")
	}
	if !s.pendingKernelSync {
		t.Fatal("expected pendingKernelSync when kernel rollback also fails")
	}
	if k.updateCalls != 2 {
		t.Fatalf("updateCalls = %d, want 2 (fallback + rollback attempt)", k.updateCalls)
	}
	if len(k.closedUUIDs) != 1 || k.closedUUIDs[0] != "uuid-new" {
		t.Fatalf("closedUUIDs = %v, want [uuid-new] (partially added user)", k.closedUUIDs)
	}
}

// Item 9 (dual failure, kernel rollback succeeds): no pending flag, and
// the service state is still the pre-delta set.
func TestApplyUserDeltaAddDualFailureKernelRollbackSucceeds(t *testing.T) {
	k := &fakeKernel{running: true, addErr: errors.New("add failed"), failFirstUpdates: 1}
	s := newTestService(k)
	s.lastConfig = &model.NodeSpec{Protocol: "vless"}
	s.updateUserState([]model.UserSpec{{ID: 1, UUID: "uuid-old", SpeedLimit: 4}})

	s.applyUserDelta(context.Background(), "add", []model.UserSpec{{ID: 2, UUID: "uuid-new", SpeedLimit: 8}})

	if len(s.lastUsers) != 1 || s.lastUsers[0].UUID != "uuid-old" {
		t.Fatalf("lastUsers = %#v, want restored old users", s.lastUsers)
	}
	if s.pendingKernelSync {
		t.Fatal("expected no pending sync when kernel rollback succeeds")
	}
	if k.updateCalls != 2 {
		t.Fatalf("updateCalls = %d, want 2 (failed fallback + successful rollback)", k.updateCalls)
	}
}

// Item 6: tombstone pruning must not be gated on len(aliveIPs) > 0.
func TestTrackAndEnforcePrunesTombstonesWithNoAliveIPs(t *testing.T) {
	k := &fakeKernel{running: true}
	s := newTestService(k)
	s.lastConfig = &model.NodeSpec{Protocol: "vless"}
	s.updateUserState([]model.UserSpec{{ID: 2, UUID: "uuid-kept", SpeedLimit: 4}})
	s.removedUserUUIDs = map[int]string{1: "uuid-old"}

	s.trackAndEnforce(context.Background())

	if _, ok := s.removedUserUUIDs[1]; ok {
		t.Fatal("tombstone for user 1 was not pruned on a tick with no alive IPs")
	}
}

// Tombstones are kept while the user still has traffic, even with no alive IPs.
func TestTrackAndEnforceKeepsTombstoneWithTraffic(t *testing.T) {
	k := &fakeKernel{running: true, traffic: map[int][2]int64{1: {100, 200}}}
	s := newTestService(k)
	s.lastConfig = &model.NodeSpec{Protocol: "vless"}
	s.updateUserState([]model.UserSpec{{ID: 2, UUID: "uuid-kept", SpeedLimit: 4}})
	s.removedUserUUIDs = map[int]string{1: "uuid-old"}

	s.trackAndEnforce(context.Background())

	if got := s.removedUserUUIDs[1]; got != "uuid-old" {
		t.Fatal("tombstone was pruned while user still had traffic")
	}
}

// Item 5: after a >10min disconnect, discovery of a new client must adopt
// it (cancelling the old one), not discard it; a still-connected old
// client must be kept.
func TestWsDiscoveryAdoptsNewClientAfterLongDisconnect(t *testing.T) {
	oldClient := &fakePushClient{name: "old", connected: false}
	newClient := &fakePushClient{name: "new", connected: true}
	k := &fakeKernel{running: true}
	s := newTestService(k)
	s.wsClient = oldClient
	s.wsDisconnectAt = time.Now().Add(-11 * time.Minute)
	oldCancelled := false
	s.wsCancel = func() { oldCancelled = true }
	s.source = &fakeSource{
		discoverClient:    newClient,
		supportsDiscovery: true,
		supportsPolling:   false,
	}

	s.wsDiscovery(context.Background())

	if !oldCancelled {
		t.Fatal("expected old disconnected client to be cancelled")
	}
	got, ok := s.wsClient.(*fakePushClient)
	if !ok || got.name != "new" {
		t.Fatalf("wsClient = %#v, want new client", s.wsClient)
	}
	if !s.wsDisconnectAt.IsZero() {
		t.Fatal("expected wsDisconnectAt cleared after adopting new client")
	}
}

func TestWsDiscoveryKeepsConnectedClient(t *testing.T) {
	oldClient := &fakePushClient{name: "old", connected: true}
	newClient := &fakePushClient{name: "new", connected: true}
	k := &fakeKernel{running: true}
	s := newTestService(k)
	s.wsClient = oldClient
	s.wsDisconnectAt = time.Now().Add(-11 * time.Minute)
	oldCancelled := false
	s.wsCancel = func() { oldCancelled = true }
	s.source = &fakeSource{
		discoverClient:    newClient,
		supportsDiscovery: true,
		supportsPolling:   false,
	}

	s.wsDiscovery(context.Background())

	if oldCancelled {
		t.Fatal("connected old client must not be cancelled")
	}
	got, ok := s.wsClient.(*fakePushClient)
	if !ok || got.name != "old" {
		t.Fatalf("wsClient = %#v, want old client kept", s.wsClient)
	}
	if newClient.runCalls != 0 {
		t.Fatalf("new client runCalls = %d, want 0 (discarded client must not start)", newClient.runCalls)
	}
}

// Item 4: an empty initial user list must still apply panel cert settings.
func TestInitialSetupEmptyUsersStillAppliesRemoteOverrides(t *testing.T) {
	k := &fakeKernel{running: false}
	s := newTestService(k)
	fc := &fakeCertManager{}
	s.cert = fc
	s.cfg = &config.Config{}
	s.source = &fakeSource{
		bootstrap: controlplane.Bootstrap{
			Config: &model.NodeSpec{Protocol: "vless", AutoTLS: true, Domain: "example.com"},
			Users:  []model.UserSpec{},
		},
		supportsDiscovery: false,
		supportsPolling:   false,
	}
	s.sink = &fakeSink{}

	if err := s.initialSetup(context.Background()); err != nil {
		t.Fatalf("initialSetup failed: %v", err)
	}
	if fc.reconfigureCalls != 1 {
		t.Fatalf("reconfigureCalls = %d, want 1 (legacy cert fields must reach cert manager despite zero users)", fc.reconfigureCalls)
	}
	if !s.cfg.Cert.AutoTLS || s.cfg.Cert.Domain != "example.com" {
		t.Fatalf("cfg.Cert = %+v, want AutoTLS=true Domain=example.com", s.cfg.Cert)
	}
	if k.startCalls != 0 {
		t.Fatalf("startCalls = %d, want 0 (kernel must not start with zero users)", k.startCalls)
	}
	if s.lastConfig == nil || s.lastConfig.Protocol != "vless" {
		t.Fatalf("lastConfig = %#v, want vless config recorded", s.lastConfig)
	}
}

// Item 8: legacy domain/auto_tls changes reach the cert manager, and a
// Reconfigure failure leaves local config untouched.
func TestApplyRemoteOverridesLegacyTriggersReconfigure(t *testing.T) {
	newSvc := func() (*Service, *fakeCertManager) {
		k := &fakeKernel{running: true}
		s := newTestService(k)
		fc := &fakeCertManager{}
		s.cert = fc
		s.cfg = &config.Config{}
		return s, fc
	}

	t.Run("change triggers reconfigure", func(t *testing.T) {
		s, fc := newSvc()
		s.applyRemoteOverrides(context.Background(), &model.NodeSpec{AutoTLS: true, Domain: "example.com"})
		if fc.reconfigureCalls != 1 {
			t.Fatalf("reconfigureCalls = %d, want 1", fc.reconfigureCalls)
		}
		if !s.cfg.Cert.AutoTLS || s.cfg.Cert.Domain != "example.com" {
			t.Fatalf("cfg.Cert = %+v, want updated legacy fields", s.cfg.Cert)
		}
	})

	t.Run("no change triggers nothing", func(t *testing.T) {
		s, fc := newSvc()
		s.cfg.Cert = config.CertConfig{AutoTLS: true, Domain: "example.com"}
		s.applyRemoteOverrides(context.Background(), &model.NodeSpec{AutoTLS: true, Domain: "example.com"})
		if fc.reconfigureCalls != 0 {
			t.Fatalf("reconfigureCalls = %d, want 0", fc.reconfigureCalls)
		}
	})

	t.Run("reconfigure failure keeps local config", func(t *testing.T) {
		s, fc := newSvc()
		fc.reconfigureErr = errors.New("reconfigure failed")
		if changed := s.applyRemoteOverrides(context.Background(), &model.NodeSpec{AutoTLS: true, Domain: "example.com"}); changed {
			t.Fatal("expected changed=false on Reconfigure failure")
		}
		if s.cfg.Cert.AutoTLS || s.cfg.Cert.Domain != "" {
			t.Fatalf("cfg.Cert = %+v, want untouched after failure", s.cfg.Cert)
		}
	})
}

// Item 3: with the new error-returning SendDeviceReport signature, a
// failed send restores the flushed snapshot for the next flush; a
// successful send restores nothing.
func TestSendDevicesViaRestoresOnFailure(t *testing.T) {
	s := newTestService(&fakeKernel{running: true})
	devices := map[int][]string{1: {"1.1.1.1", "2.2.2.2"}}

	failing := &fakeDeviceSender{err: errors.New("ws write failed")}
	if err := s.sendDevicesVia(failing, devices); err == nil {
		t.Fatal("expected error from failing sender")
	}
	if got := failing.got; len(got) != 1 || len(got[1]) != 2 {
		t.Fatalf("sender got %#v, want the flushed snapshot", got)
	}
	flushed := s.tracker.FlushAliveIPs()
	if len(flushed) != 1 || len(flushed[1]) != 2 {
		t.Fatalf("FlushAliveIPs after failed send = %#v, want restored snapshot", flushed)
	}

	okSender := &fakeDeviceSender{}
	if err := s.sendDevicesVia(okSender, devices); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if flushed := s.tracker.FlushAliveIPs(); flushed != nil {
		t.Fatalf("FlushAliveIPs after successful send = %#v, want nil (nothing restored)", flushed)
	}
}

// Item 3 (direct path): sendDeviceBatch now calls the error-returning
// PushClient.SendDeviceReport directly (the reflection bridge was deleted
// once the interface itself carried the error signature). A failed send
// restores the flushed snapshot for the next flush.
func TestSendDeviceBatchRestoresOnFailure(t *testing.T) {
	k := &fakeKernel{running: true}
	s := newTestService(k)
	failing := &fakePushClient{name: "failing", connected: true, sendErr: errors.New("ws write failed")}
	s.wsClient = failing
	s.tracker.RestoreAliveIPs(map[int][]string{1: {"1.1.1.1"}})

	s.sendDeviceBatch()

	if failing.deviceReportCalls != 1 {
		t.Fatalf("deviceReportCalls = %d, want 1", failing.deviceReportCalls)
	}
	if flushed := s.tracker.FlushAliveIPs(); len(flushed) != 1 || len(flushed[1]) != 1 {
		t.Fatalf("FlushAliveIPs after failed send = %#v, want restored snapshot", flushed)
	}
}

// Item 3 (direct path): sendDeviceBatch sends the flushed snapshot
// directly through the error-returning PushClient without restoring it.
func TestSendDeviceBatchSendsDirectly(t *testing.T) {
	k := &fakeKernel{running: true}
	s := newTestService(k)
	oldClient := &fakePushClient{name: "old", connected: true}
	s.wsClient = oldClient
	sink := &fakeSink{}
	s.sink = sink
	s.tracker.RestoreAliveIPs(map[int][]string{1: {"1.1.1.1"}})

	s.sendDeviceBatch()

	if oldClient.deviceReportCalls != 1 {
		t.Fatalf("deviceReportCalls = %d, want 1", oldClient.deviceReportCalls)
	}
	if len(oldClient.lastDevices) != 1 || len(oldClient.lastDevices[1]) != 1 {
		t.Fatalf("client devices = %#v, want flushed snapshot", oldClient.lastDevices)
	}
	if sink.reportDevicesCalls != 0 {
		t.Fatalf("reportDevicesCalls = %d, want 0 (direct path handled it)", sink.reportDevicesCalls)
	}
	if flushed := s.tracker.FlushAliveIPs(); flushed != nil {
		t.Fatalf("FlushAliveIPs after successful send = %#v, want nil (nothing restored)", flushed)
	}
}
