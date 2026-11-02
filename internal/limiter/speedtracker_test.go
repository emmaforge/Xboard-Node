package limiter

import (
	"testing"
	"time"

	"github.com/cedar2025/xboard-node/internal/model"
	"golang.org/x/time/rate"
)

func TestSpeedTracker_UpdateBuckets(t *testing.T) {
	l := New()
	st := NewSpeedTracker(l)

	users := []model.UserSpec{
		{ID: 1, UUID: "u1", SpeedLimit: 3, DeviceLimit: 0},  // 3 Mbps
		{ID: 2, UUID: "u2", SpeedLimit: 0, DeviceLimit: 0},  // unlimited
		{ID: 3, UUID: "u3", SpeedLimit: 10, DeviceLimit: 2}, // 10 Mbps
	}
	l.UpdateUsers(users)
	st.UpdateBuckets()

	// User 1: 3 Mbps = 375000 bytes/sec
	b1 := st.GetLimiter("u1")
	if b1 == nil {
		t.Fatal("expected bucket for user 1")
	}
	expectedRate := float64(3 * 1000000 / 8)
	if float64(b1.Limit()) != expectedRate {
		t.Errorf("user 1 rate: got %v, want %v", float64(b1.Limit()), expectedRate)
	}
	// Burst should be max(bytesPerSec, 64KB) = max(375000, 65536) = 375000
	expectedBurst := 3 * 1000000 / 8
	if b1.Burst() != expectedBurst {
		t.Errorf("user 1 burst: got %v, want %v", b1.Burst(), expectedBurst)
	}

	// User 2: no speed limit, no bucket
	b2 := st.GetLimiter("u2")
	if b2 != nil {
		t.Fatal("expected no bucket for user 2")
	}

	// User 3: 10 Mbps = 1250000 bytes/sec
	b3 := st.GetLimiter("u3")
	if b3 == nil {
		t.Fatal("expected bucket for user 3")
	}
	expectedRate3 := float64(10 * 1000000 / 8)
	if float64(b3.Limit()) != expectedRate3 {
		t.Errorf("user 3 rate: got %v, want %v", float64(b3.Limit()), expectedRate3)
	}
	// Burst should be max(1250000, 64KB) = 1250000
	expectedBurst3 := 10 * 1000000 / 8
	if b3.Burst() != expectedBurst3 {
		t.Errorf("user 3 burst: got %v, want %v", b3.Burst(), expectedBurst3)
	}
}

func TestSpeedTracker_UpdateBuckets_PreservesExisting(t *testing.T) {
	l := New()
	st := NewSpeedTracker(l)

	users := []model.UserSpec{
		{ID: 1, UUID: "u1", SpeedLimit: 3, DeviceLimit: 0},
	}
	l.UpdateUsers(users)
	st.UpdateBuckets()

	b1 := st.GetLimiter("u1")
	if b1 == nil {
		t.Fatal("expected bucket for user 1")
	}

	// Update same user with different speed — should reuse same *rate.Limiter pointer
	users2 := []model.UserSpec{
		{ID: 1, UUID: "u1", SpeedLimit: 5, DeviceLimit: 0},
	}
	l.UpdateUsers(users2)
	st.UpdateBuckets()

	b1After := st.GetLimiter("u1")
	if b1After == nil {
		t.Fatal("expected bucket for user 1 after update")
	}
	if b1 != b1After {
		t.Error("expected same rate.Limiter instance to be reused")
	}

	// Rate should be updated
	expectedRate := float64(5 * 1000000 / 8)
	if float64(b1After.Limit()) != expectedRate {
		t.Errorf("user 1 updated rate: got %v, want %v", float64(b1After.Limit()), expectedRate)
	}
}

func TestSpeedTracker_UpdateBuckets_RemovesUsers(t *testing.T) {
	l := New()
	st := NewSpeedTracker(l)

	users := []model.UserSpec{
		{ID: 1, UUID: "u1", SpeedLimit: 3, DeviceLimit: 0},
		{ID: 2, UUID: "u2", SpeedLimit: 5, DeviceLimit: 0},
	}
	l.UpdateUsers(users)
	st.UpdateBuckets()

	if st.GetLimiter("u1") == nil || st.GetLimiter("u2") == nil {
		t.Fatal("expected buckets for both users")
	}

	// Remove user 2
	users2 := []model.UserSpec{
		{ID: 1, UUID: "u1", SpeedLimit: 3, DeviceLimit: 0},
	}
	l.UpdateUsers(users2)
	st.UpdateBuckets()

	if st.GetLimiter("u1") == nil {
		t.Fatal("expected bucket for user 1")
	}
	if st.GetLimiter("u2") != nil {
		t.Error("expected no bucket for removed user 2")
	}
}

// Regression: log callback must not run while UpdateBuckets holds t.mu.Lock,
// otherwise callbacks that call LimitedUserCount() self-deadlock on RWMutex.
func TestSpeedTracker_UpdateBuckets_LogCallbackMayCallLimitedUserCount(t *testing.T) {
	l := New()
	st := NewSpeedTracker(l)
	l.UpdateUsers([]model.UserSpec{{ID: 1, UUID: "u1", SpeedLimit: 1}})
	st.SetLogCallback(func(msg string) {
		_ = st.LimitedUserCount()
	})

	done := make(chan struct{})
	go func() {
		st.UpdateBuckets()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("deadlock: UpdateBuckets did not finish (log callback vs RWMutex)")
	}
}

func TestBurstForUnifiedAcrossPaths(t *testing.T) {
	// Regression test for B10: burst must be computed identically by
	// UpdateBuckets and GetLimiter, so a limiter's burst does not
	// depend on which path created it first.
	if got := burstFor(12_500_000); got != 12_500_000 {
		t.Errorf("burstFor(12.5MB/s) = %d, want 12500000", got)
	}
	if got := burstFor(1000); got != 64*1024 {
		t.Errorf("burstFor(1000) = %d, want 65536 floor", got)
	}
	if got := burstFor(20_000); got != 64*1024 {
		t.Errorf("burstFor(20000) = %d, want 65536 floor", got)
	}
}

// Item 10: the device-limit rejection counter moves only via the explicit
// counting entry point (kernel gate-keeping must call it on rejection).
func TestRecordDeviceLimitEvent(t *testing.T) {
	l := New()
	if got := l.SnapshotMetrics().DeviceLimitEvents; got != 0 {
		t.Fatalf("initial DeviceLimitEvents = %d, want 0", got)
	}
	l.RecordDeviceLimitEvent()
	l.RecordDeviceLimitEvent()
	if got := l.SnapshotMetrics().DeviceLimitEvents; got != 2 {
		t.Fatalf("DeviceLimitEvents = %d, want 2", got)
	}
}

// Item 11: GetLimiter's double-check must re-verify the UUID mapping, not
// just bucket existence. Here the UUID moved to another user while a
// stale bucket for the old user ID still sits in the map: the lookup
// must return a bucket for the NEW user, never the stale one.
func TestSpeedTracker_GetLimiterDoubleCheckRemap(t *testing.T) {
	l := New()
	st := NewSpeedTracker(l)
	l.UpdateUsers([]model.UserSpec{
		{ID: 1, UUID: "u1", SpeedLimit: 3},
		{ID: 2, UUID: "u2", SpeedLimit: 5},
	})
	st.UpdateBuckets()
	oldBucket := st.GetLimiter("u1")
	if oldBucket == nil {
		t.Fatal("expected bucket for user 1")
	}

	// UUID u1 moves from user 1 to user 2 (UpdateBuckets rebuilds the
	// mapping), but a stale bucket for user 1 lingers in the map.
	l.UpdateUsers([]model.UserSpec{
		{ID: 1, UUID: "uX", SpeedLimit: 3},
		{ID: 2, UUID: "u1", SpeedLimit: 5},
	})
	st.UpdateBuckets()
	st.mu.Lock()
	st.buckets[1] = oldBucket
	delete(st.buckets, 2)
	st.mu.Unlock()

	got := st.GetLimiter("u1")
	if got == nil {
		t.Fatal("expected bucket for remapped UUID u1")
	}
	if got == oldBucket {
		t.Fatal("GetLimiter returned the stale bucket of the previous owner")
	}
	expectedRate := float64(5 * 1000000 / 8)
	if float64(got.Limit()) != expectedRate {
		t.Errorf("remapped bucket rate: got %v, want %v (user 2's limit)", float64(got.Limit()), expectedRate)
	}
}

// Item 11 (stale config): a bucket whose user's limit was cancelled must
// not be handed out at the old rate: after the cancel, the shared bucket
// itself is retired to Inf, so any holder or fresh lookup observes
// unlimited rather than the stale 3 Mbps rate.
func TestSpeedTracker_GetLimiterDoubleCheckStaleLimit(t *testing.T) {
	l := New()
	st := NewSpeedTracker(l)
	l.UpdateUsers([]model.UserSpec{{ID: 1, UUID: "u1", SpeedLimit: 3}})
	st.UpdateBuckets()
	stale := st.GetLimiter("u1")
	if stale == nil {
		t.Fatal("expected bucket for user 1")
	}

	l.UpdateUsers([]model.UserSpec{{ID: 1, UUID: "u1", SpeedLimit: 0}})
	st.UpdateBuckets()
	if stale.Limit() != rate.Inf {
		t.Fatalf("retired bucket limit = %v, want +Inf", stale.Limit())
	}
	if got := st.GetLimiter("u1"); got != stale || got.Limit() != rate.Inf {
		t.Fatal("expected fresh lookup to return the retired unlimited bucket")
	}
}

// Item 12: LimitedUserCount counts CONFIGURED limits, not materialised
// buckets — users that never opened a connection still count.
func TestLimitedUserCountCountsConfiguredNotBuckets(t *testing.T) {
	l := New()
	st := NewSpeedTracker(l)
	l.UpdateUsers([]model.UserSpec{
		{ID: 1, UUID: "u1", SpeedLimit: 3},
		{ID: 2, UUID: "u2", SpeedLimit: 5},
		{ID: 3, UUID: "u3", SpeedLimit: 0},
	})
	if got := st.LimitedUserCount(); got != 2 {
		t.Fatalf("LimitedUserCount = %d, want 2 (configured, no buckets yet)", got)
	}
	if !st.HasLimits() {
		t.Fatal("HasLimits = false, want true with configured limits")
	}
	l.UpdateUsers([]model.UserSpec{
		{ID: 1, UUID: "u1", SpeedLimit: 3},
		{ID: 2, UUID: "u2", SpeedLimit: 0},
		{ID: 3, UUID: "u3", SpeedLimit: 0},
	})
	if got := st.LimitedUserCount(); got != 1 {
		t.Fatalf("LimitedUserCount = %d, want 1 after cancelling user 2's limit", got)
	}
}

// Item 13: cancelling a limit retires the shared bucket in place, so a
// connection that already holds the pointer stops being throttled at
// once; re-imposing a limit re-arms the same pointer.
func TestSpeedTracker_UpdateBucketsRetiresBucketOnLimitCancel(t *testing.T) {
	l := New()
	st := NewSpeedTracker(l)
	l.UpdateUsers([]model.UserSpec{{ID: 1, UUID: "u1", SpeedLimit: 3}})
	st.UpdateBuckets()
	held := st.GetLimiter("u1")
	if held == nil {
		t.Fatal("expected bucket for user 1")
	}

	l.UpdateUsers([]model.UserSpec{{ID: 1, UUID: "u1", SpeedLimit: 0}})
	st.UpdateBuckets()

	if held.Limit() != rate.Inf {
		t.Fatalf("held bucket limit = %v, want +Inf after limit cancel", held.Limit())
	}
	if got := st.GetLimiter("u1"); got != held {
		t.Fatal("expected the same (retired) bucket to be returned while the user stays active")
	}
	if got := st.LimitedUserCount(); got != 0 {
		t.Fatalf("LimitedUserCount = %d, want 0 after cancel", got)
	}
	if st.HasLimits() {
		t.Fatal("HasLimits = true, want false after cancel")
	}

	// Re-impose: the same pointer is re-armed, so holders and new
	// lookups alike see the new rate.
	l.UpdateUsers([]model.UserSpec{{ID: 1, UUID: "u1", SpeedLimit: 5}})
	st.UpdateBuckets()
	expectedRate := float64(5 * 1000000 / 8)
	if float64(held.Limit()) != expectedRate {
		t.Errorf("held bucket limit after re-impose = %v, want %v", float64(held.Limit()), expectedRate)
	}
	if got := st.GetLimiter("u1"); got != held {
		t.Fatal("expected the same bucket pointer after re-impose")
	}
	if got := st.LimitedUserCount(); got != 1 {
		t.Fatalf("LimitedUserCount = %d, want 1 after re-impose", got)
	}
}
