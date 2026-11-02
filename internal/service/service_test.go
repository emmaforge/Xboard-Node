package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cedar2025/xboard-node/internal/cert"
	"github.com/cedar2025/xboard-node/internal/config"
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
}

func (f *fakeCertManager) Start(context.Context) error { return nil }
func (f *fakeCertManager) Stop()                       {}
func (f *fakeCertManager) Reconfigure(context.Context, config.CertConfig) (bool, error) {
	return false, nil
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

	startCalls  int
	updateCalls int
	addCalls    int
	removeCalls int
	closeCalls  int

	closedUUIDs  []string
	trafficAlive map[int]map[string]bool

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
	return len(users), 0, nil
}
func (f *fakeKernel) GetUserTraffic(ctx context.Context) (map[int][2]int64, map[int]map[string]bool, int, error) {
	_ = ctx
	return nil, f.trafficAlive, len(f.trafficAlive), nil
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
