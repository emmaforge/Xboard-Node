package limiter

import (
	"sync"

	"github.com/cedar2025/xboard-node/internal/model"
	"golang.org/x/time/rate"
)

// SpeedTrackerLogCallback is called when bucket updates occur.
type SpeedTrackerLogCallback func(msg string)

// SpeedTracker manages per-user token-bucket rate limiters.
// It does NOT wrap connections itself — instead, ConnTracker consults it
// via GetLimiter to embed rate limiting in the same tracked connection
// wrapper that does byte counting.
type SpeedTracker struct {
	limiter *Limiter
	mu      sync.RWMutex
	buckets map[int]*rate.Limiter // userID → shared rate limiter
	uuidMap map[string]int        // UUID → userID

	// Optional callback for logging
	logFunc SpeedTrackerLogCallback
}

// unlimitedBurst is the burst of a retired bucket (see retireBucket). It
// only defeats the per-call truncation in kernel connection wrappers;
// the rate itself is rate.Inf, so no token wait is ever imposed.
const unlimitedBurst = 1 << 30

// retireBucket converts a shared bucket to unlimited IN PLACE. Connections
// that already hold the *rate.Limiter pointer keep sharing it, so flipping
// the limit here unlimits them immediately — deleting the map entry alone
// would leave existing holders throttled until they disconnect, because
// they never re-read the map. The retired bucket stays in the map (for
// still-active users) so later GetLimiter calls return the same unlimited
// bucket; it is deleted by the removed-user cleanup once the user leaves,
// and re-armed by UpdateBuckets/GetLimiter if a limit is configured again.
func retireBucket(lim *rate.Limiter) {
	if lim == nil {
		return
	}
	lim.SetLimit(rate.Inf)
	lim.SetBurst(unlimitedBurst)
}

// burstFor computes the token-bucket burst for a bytes-per-second rate:
// one second of traffic, floored at 64KiB, and capped at 4 seconds of
// traffic. Both UpdateBuckets and GetLimiter must use it: they used to
// compute burst inline, with GetLimiter carrying a 4s cap UpdateBuckets
// lacked, so the effective burst depended on which path created the
// limiter first (last caller wins). Note the cap is unreachable when
// burst starts at bytesPerSec (bytesPerSec <= 4*bytesPerSec), so
// unification preserves current behaviour while removing the
// divergence risk for future edits.
func burstFor(bytesPerSec int) int {
	burst := bytesPerSec
	if burst < 64*1024 {
		burst = 64 * 1024
	}
	if cap4s := bytesPerSec * 4; cap4s > 64*1024 && burst > cap4s {
		burst = cap4s
	}
	return burst
}

// NewSpeedTracker creates a bucket manager for per-user bandwidth throttling.
func NewSpeedTracker(l *Limiter) *SpeedTracker {
	return &SpeedTracker{
		limiter: l,
		buckets: make(map[int]*rate.Limiter),
		uuidMap: make(map[string]int),
	}
}

// SetLogCallback sets the logging callback.
func (t *SpeedTracker) SetLogCallback(f SpeedTrackerLogCallback) {
	t.logFunc = f
}

// UpdateBuckets updates the UUID→userID mapping and syncs existing limiters.
func (t *SpeedTracker) UpdateBuckets() {
	currentUsers := make([]model.UserSpec, 0, 32)
	t.limiter.mu.RLock()
	for _, u := range t.limiter.users {
		currentUsers = append(currentUsers, u)
	}
	t.limiter.mu.RUnlock()

	func() {
		t.mu.Lock()
		defer t.mu.Unlock()

		newUUIDMap := make(map[string]int, len(currentUsers))
		activeIDs := make(map[int]struct{}, len(currentUsers))

		for _, user := range currentUsers {
			activeIDs[user.ID] = struct{}{}
			if user.UUID != "" {
				newUUIDMap[user.UUID] = user.ID
			}

			// Update existing limiter if speed changed. A cancelled
			// limit (SpeedLimit <= 0) retires the bucket in place
			// instead of deleting it, so connections that already
			// hold the shared pointer stop being throttled at once
			// (see retireBucket); only fully removed users lose
			// their map entry, in the cleanup loop below.
			if lim, ok := t.buckets[user.ID]; ok {
				if user.SpeedLimit > 0 {
					bytesPerSec := int(user.SpeedLimit) * 1_000_000 / 8
					lim.SetLimit(rate.Limit(bytesPerSec))
					lim.SetBurst(burstFor(bytesPerSec))
				} else {
					retireBucket(lim)
				}
			}
		}

		// Clean up buckets for removed users (retired buckets of
		// still-active users are kept deliberately, see above).
		for id := range t.buckets {
			if _, ok := activeIDs[id]; !ok {
				delete(t.buckets, id)
			}
		}

		t.uuidMap = newUUIDMap
	}()

	if t.logFunc != nil {
		t.logFunc("buckets updated")
	}
}

// GetLimiter returns the rate limiter for the given user UUID, or nil if
// no limit applies. Creates limiter on-demand if not exists.
// Thread-safe.
func (t *SpeedTracker) GetLimiter(user string) *rate.Limiter {
	t.mu.RLock()
	uid, exists := t.uuidMap[user]
	if !exists {
		t.mu.RUnlock()
		return nil
	}
	if lim, ok := t.buckets[uid]; ok {
		t.mu.RUnlock()
		return lim
	}
	t.mu.RUnlock()

	// Get user info from limiter (first check; revalidated below).
	t.limiter.mu.RLock()
	u, userExists := t.limiter.users[uid]
	t.limiter.mu.RUnlock()

	if !userExists || u.SpeedLimit <= 0 {
		return nil
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	// Double-check after acquiring the write lock: the UUID→userID
	// mapping and the user's configured limit may both have changed
	// while we waited (UUID reassigned to another user, user removed,
	// or limit cancelled). Checking only "bucket exists" here used to
	// return a stale bucket belonging to the wrong user or to a
	// limit that no longer exists. Lock-order note: Limiter.mu is a
	// leaf lock — no code holds it while acquiring SpeedTracker.mu
	// (UpdateBuckets releases it before taking t.mu) — so taking it
	// under t.mu here cannot deadlock against UpdateBuckets.
	currentUID, mapped := t.uuidMap[user]
	if !mapped {
		return nil
	}
	if currentUID != uid {
		uid = currentUID
	}
	t.limiter.mu.RLock()
	current, currentExists := t.limiter.users[uid]
	t.limiter.mu.RUnlock()
	if !currentExists || current.UUID != user {
		// UUID now belongs to nobody, or to a different record than
		// the mapping claims: never hand out a bucket for it. (The
		// bucket itself is left alone — it is keyed by user ID and
		// is unreachable via this UUID; UpdateBuckets deletes
		// buckets of users that are actually gone.)
		return nil
	}
	if current.SpeedLimit <= 0 {
		// Limit cancelled while we waited: retire any existing
		// bucket in place (existing holders stop being throttled)
		// and report unlimited for this lookup.
		if existing, ok := t.buckets[uid]; ok {
			retireBucket(existing)
			return existing
		}
		return nil
	}

	// Re-sync an existing bucket to the current config (covers a
	// retired bucket whose limit was re-imposed, and a bucket created
	// concurrently by another goroutine), or create it on demand
	// with the same burst rule as UpdateBuckets.
	bytesPerSec := int(current.SpeedLimit) * 1_000_000 / 8
	if existing, ok := t.buckets[uid]; ok {
		existing.SetLimit(rate.Limit(bytesPerSec))
		existing.SetBurst(burstFor(bytesPerSec))
		return existing
	}
	lim := rate.NewLimiter(rate.Limit(bytesPerSec), burstFor(bytesPerSec))
	t.buckets[uid] = lim
	return lim
}

// HasLimits returns true if any user currently has a speed limit configured.
func (t *SpeedTracker) HasLimits() bool {
	return t.LimitedUserCount() > 0
}

// LimitedUserCount returns the number of users CONFIGURED with a speed
// limit (SpeedLimit > 0 in the limiter's user set), whether or not a
// token bucket has been materialised for them yet. It used to return
// len(buckets), which only counted users that had already opened a
// connection (buckets are created on demand), so the panel-facing
// "limited_users" / "speed_limited_users" fields under-reported
// configured limits. Retired (unlimited) buckets of still-active users
// are not counted, because their users no longer have a limit.
func (t *SpeedTracker) LimitedUserCount() int {
	if t.limiter == nil {
		t.mu.RLock()
		defer t.mu.RUnlock()
		return len(t.buckets)
	}
	t.limiter.mu.RLock()
	defer t.limiter.mu.RUnlock()
	n := 0
	for _, u := range t.limiter.users {
		if u.SpeedLimit > 0 {
			n++
		}
	}
	return n
}
