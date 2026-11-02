package xray

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	_ "unsafe"

	xrayDispatcher "github.com/xtls/xray-core/app/dispatcher"
	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/buf"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/session"
	"github.com/xtls/xray-core/features/routing"
	"github.com/xtls/xray-core/transport"

	"github.com/cedar2025/xboard-node/internal/kernel"
	"github.com/cedar2025/xboard-node/internal/nlog"
)

// Access xray's internal config creator registry so we can replace the
// default dispatcher factory with ours. This runs AFTER xray's init()
// functions because our package imports xray (dependency order guarantee).
//
//go:linkname typeCreatorRegistry github.com/xtls/xray-core/common.typeCreatorRegistry
var typeCreatorRegistry map[reflect.Type]common.ConfigCreator

var origDispatcherFactory common.ConfigCreator

// globalLimitDispatcher is set when the factory creates a LimitDispatcher.
// The Xray kernel reads it to configure limits and get connections.
var globalLimitDispatcher atomic.Pointer[LimitDispatcher]

func init() {
	configType := reflect.TypeOf((*xrayDispatcher.Config)(nil))
	origDispatcherFactory = typeCreatorRegistry[configType]
	typeCreatorRegistry[configType] = limitDispatcherFactory
}

func limitDispatcherFactory(ctx context.Context, config interface{}) (interface{}, error) {
	orig, err := origDispatcherFactory(ctx, config)
	if err != nil {
		return nil, err
	}
	inner, ok := orig.(routing.Dispatcher)
	if !ok {
		return orig, nil
	}
	ld := &LimitDispatcher{
		inner:      orig,
		innerDisp:  inner,
		limitedIPs: make(map[string]map[string]int),
		links:      make(map[string]map[*closeTrackingWriter]linkInfo),
	}
	globalLimitDispatcher.Store(ld)
	nlog.Core().Debug("xray: limit dispatcher installed")
	return ld, nil
}

// LimitDispatcher wraps xray's DefaultDispatcher to enforce per-user
// admission checks before a request is dispatched into xray-core.
//
// It intentionally does NOT mutate transport.Link.Reader/Writer. Xray's
// mux/XUDP close path requires the original concrete *pipe.Reader to remain
// intact, so the dispatcher is limited to gate-keeping and safe connection
// lifecycle bookkeeping.
type LimitDispatcher struct {
	inner     interface{}        // original DefaultDispatcher (Feature + Dispatcher)
	innerDisp routing.Dispatcher // same object, typed as Dispatcher

	// limitedUsers: users with device limit > 0, protected by mu.
	// Needs deterministic IP ordering for kick decisions.
	mu           sync.RWMutex
	limitedIPs   map[string]map[string]int // email → sourceIP → refcount
	deviceLimits map[string]int            // email → max devices
	emailToUID   map[string]int            // email → panel user ID

	// deviceLimitRejectedFunc, when set, is called once per connection
	// rejected by checkDeviceLimit. It lets the service layer count
	// rejections (e.g. limiter.RecordDeviceLimitEvent) without the
	// dispatcher depending on the service layer. It is invoked outside
	// mu, and must itself be cheap and non-blocking.
	deviceLimitRejectedFunc atomic.Pointer[func()]

	// unlimitedIPs: users without device limit — sync.Map for lock-free access.
	// Each entry is *ipCounter{ips sync.Map}.
	unlimitedIPs sync.Map // email → *ipCounter

	connCount atomic.Int64 // total active connections tracked by dispatcher

	// globalDevices is the panel-aggregated cross-node device state
	// (email -> IPs), refreshed via UpdateGlobalDevices from WS
	// sync.devices events. It mirrors the sing-box tracker's global state;
	// without it, xray nodes enforced device limits using only local
	// connections and cross-node limits silently did not apply.
	globalMu         sync.RWMutex
	globalDevices    map[string]map[string]bool
	globalLastUpdate time.Time

	// links indexes live connections by session email so a removed user can
	// be force-disconnected (CloseUserConnections). It is guarded by its own
	// mutex: closing a writer runs its onClose callback, which deregisters
	// the writer, so callers must snapshot under linkMu and close after
	// releasing it, or they deadlock on re-entry.
	linkMu sync.Mutex
	links  map[string]map[*closeTrackingWriter]linkInfo // email → live writers
}

// linkInfo is one live connection in the links index.
type linkInfo struct {
	writer   *closeTrackingWriter
	sourceIP string
}

// ipCounter tracks IPs for unlimited users without any lock.
type ipCounter struct {
	ips sync.Map // sourceIP → *atomic.Int64 (refcount)
}

// aliveIPs returns a snapshot of distinct IPs.
func (ic *ipCounter) aliveIPs() map[string]bool {
	result := make(map[string]bool)
	ic.ips.Range(func(key, _ interface{}) bool {
		if rv, ok := ic.ips.Load(key); ok && rv.(*atomic.Int64).Load() > 0 {
			result[key.(string)] = true
		}
		return true
	})
	return result
}

// ─── routing.Dispatcher ──────────────────────────────────────────────────────

func (d *LimitDispatcher) Dispatch(ctx context.Context, dest net.Destination) (*transport.Link, error) {
	email, sourceIP, err := d.identifyAndCheck(ctx, dest)
	if err != nil {
		return nil, err
	}

	link, err := d.innerDisp.Dispatch(ctx, dest)
	if err != nil {
		if email != "" {
			d.delConn(email, sourceIP)
		}
		return nil, err
	}

	if email != "" {
		d.trackLink(link, email, sourceIP)
	}
	return link, nil
}

func (d *LimitDispatcher) DispatchLink(ctx context.Context, dest net.Destination, link *transport.Link) error {
	email, sourceIP, err := d.identifyAndCheck(ctx, dest)
	if err != nil {
		return err
	}

	if email != "" {
		w := d.trackLink(link, email, sourceIP)
		if err := d.innerDisp.DispatchLink(ctx, dest, link); err != nil {
			// Roll back the tracking added above: the link never got
			// dispatched, so its writer must not stay registered or keep
			// inflating the device/IP counts.
			if w != nil {
				w.Close()
			}
			return err
		}
		return nil
	}
	return d.innerDisp.DispatchLink(ctx, dest, link)
}

// identifyAndCheck extracts user identity from the session context and
// enforces device limits. Returns a non-nil error only when the connection
// should be rejected. dest is accepted for future transport-aware checks;
// device counting itself is transport-agnostic (see checkDeviceLimit).
func (d *LimitDispatcher) identifyAndCheck(ctx context.Context, dest net.Destination) (email, sourceIP string, err error) {
	_ = dest
	si := session.InboundFromContext(ctx)
	if si == nil || si.User == nil || len(si.User.Email) == 0 {
		return "", "", nil
	}
	email = si.User.Email
	sourceIP = kernel.NormalizeDeviceIP(si.Source.Address.IP().String())

	if d.checkDeviceLimit(email, sourceIP) {
		nlog.Core().Debug("xray: device limit exceeded", "email", email, "ip", sourceIP)
		return "", "", errors.New("device limit exceeded for " + email)
	}
	return email, sourceIP, nil
}

// trackLink records connection lifecycle without mutating xray-core owned
// transport primitives. This keeps mux/XUDP compatible while still allowing
// the dispatcher to release device-limit state when the link closes. It
// returns the tracking writer so callers can roll tracking back on a
// failed dispatch, and registers it in links so CloseUserConnections can
// force-close every live connection of a removed user.
func (d *LimitDispatcher) trackLink(link *transport.Link, email, sourceIP string) *closeTrackingWriter {
	d.connCount.Add(1)

	var w *closeTrackingWriter
	onClose := func() {
		// Always release the device/IP count, for TCP and UDP alike (see
		// checkDeviceLimit: a dispatched Link is a session, not a packet).
		d.delConn(email, sourceIP)
		d.connCount.Add(-1)
		if w != nil {
			d.unregisterLink(email, w)
		}
	}

	w = &closeTrackingWriter{
		Writer:  link.Writer,
		onClose: onClose,
	}
	link.Writer = w

	d.linkMu.Lock()
	if d.links == nil {
		d.links = make(map[string]map[*closeTrackingWriter]linkInfo)
	}
	set, ok := d.links[email]
	if !ok {
		set = make(map[*closeTrackingWriter]linkInfo)
		d.links[email] = set
	}
	set[w] = linkInfo{writer: w, sourceIP: sourceIP}
	d.linkMu.Unlock()
	return w
}

// unregisterLink removes a writer from the per-email live-connection index.
// Called from the writer's onClose callback, so it must not be called while
// linkMu is held.
func (d *LimitDispatcher) unregisterLink(email string, w *closeTrackingWriter) {
	d.linkMu.Lock()
	defer d.linkMu.Unlock()
	if set, ok := d.links[email]; ok {
		delete(set, w)
		if len(set) == 0 {
			delete(d.links, email)
		}
	}
}

// CloseUserConnections force-closes every live connection registered under
// the given session email. Writers are snapshotted under linkMu and closed
// after releasing it, because closing a writer runs its onClose callback,
// which re-enters unregisterLink. Returns the number of connections closed.
func (d *LimitDispatcher) CloseUserConnections(email string) int {
	if email == "" {
		return 0
	}
	d.linkMu.Lock()
	set := d.links[email]
	writers := make([]*closeTrackingWriter, 0, len(set))
	for w := range set {
		writers = append(writers, w)
	}
	d.linkMu.Unlock()
	for _, w := range writers {
		w.Close()
	}
	return len(writers)
}

// ─── features.Feature (delegated) ───────────────────────────────────────────

func (d *LimitDispatcher) Type() interface{} { return routing.DispatcherType() }

func (d *LimitDispatcher) Start() error {
	if s, ok := d.inner.(interface{ Start() error }); ok {
		return s.Start()
	}
	return nil
}

func (d *LimitDispatcher) Close() error {
	if c, ok := d.inner.(interface{ Close() error }); ok {
		return c.Close()
	}
	return nil
}

// ─── Limit management (called by Xray kernel) ──────────────────────────────

func (d *LimitDispatcher) UpdateLimits(emailToUID map[string]int, deviceLimits, _ map[string]int) {
	d.mu.Lock()
	oldLimits := d.deviceLimits
	d.emailToUID = emailToUID
	d.deviceLimits = deviceLimits
	// Migrate live counts across the limited/unlimited buckets when a
	// user's limit status flips. Without this, a limited -> unlimited
	// switch stranded counts in limitedIPs (and vice versa): delConn
	// looked in only one bucket, so the device slot leaked permanently
	// and the user could be blocked forever despite connections having
	// closed.
	for email := range unionKeys(oldLimits, deviceLimits) {
		wasLimited := oldLimits[email] > 0
		nowLimited := deviceLimits[email] > 0
		if wasLimited == nowLimited {
			continue
		}
		if nowLimited {
			// unlimited -> limited: fold the lock-free counters into
			// the limited map and drop the unlimited entry.
			if v, ok := d.unlimitedIPs.Load(email); ok {
				ic := v.(*ipCounter)
				moved := make(map[string]int)
				ic.ips.Range(func(key, value interface{}) bool {
					if n := value.(*atomic.Int64).Load(); n > 0 {
						moved[key.(string)] = int(n)
					}
					return true
				})
				if len(moved) > 0 {
					if d.limitedIPs == nil {
						d.limitedIPs = make(map[string]map[string]int)
					}
					dst := d.limitedIPs[email]
					if dst == nil {
						dst = make(map[string]int)
						d.limitedIPs[email] = dst
					}
					for ip, n := range moved {
						dst[ip] += n
					}
				}
				d.unlimitedIPs.Delete(email)
			}
		} else {
			// limited -> unlimited: move the limited counts into the
			// lock-free counters and drop the limited entry.
			if ips, ok := d.limitedIPs[email]; ok {
				v, _ := d.unlimitedIPs.LoadOrStore(email, &ipCounter{})
				ic := v.(*ipCounter)
				for ip, n := range ips {
					if n <= 0 {
						continue
					}
					rv, _ := ic.ips.LoadOrStore(ip, &atomic.Int64{})
					rv.(*atomic.Int64).Add(int64(n))
				}
				delete(d.limitedIPs, email)
			}
		}
	}
	d.mu.Unlock()
}

// unionKeys returns the set of keys present in either map.
func unionKeys(a, b map[string]int) map[string]bool {
	out := make(map[string]bool, len(a)+len(b))
	for k := range a {
		out[k] = true
	}
	for k := range b {
		out[k] = true
	}
	return out
}

func (d *LimitDispatcher) ResetConns() {
	d.mu.Lock()
	d.limitedIPs = make(map[string]map[string]int)
	d.mu.Unlock()

	d.linkMu.Lock()
	d.links = make(map[string]map[*closeTrackingWriter]linkInfo)
	d.linkMu.Unlock()

	// Clear unlimited IPs
	d.unlimitedIPs.Range(func(key, _ interface{}) bool {
		d.unlimitedIPs.Delete(key)
		return true
	})

	d.connCount.Store(0)
}

// GetConnectionState returns dispatcher-tracked alive IPs and connection count.
// Traffic bytes are intentionally left to xray's built-in stats pipeline.
func (d *LimitDispatcher) GetConnectionState() (aliveIPs map[int]map[string]bool, connCount int) {
	// Deep-copy both maps under RLock, then iterate the copies without
	// holding the lock. The previous code only copied the map headers
	// and iterated limitedIPs after unlocking, racing with writers
	// (checkDeviceLimit/delConn take mu.Lock) and crashing with
	// "concurrent map iteration and map write".
	d.mu.RLock()
	emailToUID := make(map[string]int, len(d.emailToUID))
	for email, uid := range d.emailToUID {
		emailToUID[email] = uid
	}
	limitedIPs := make(map[string]map[string]int, len(d.limitedIPs))
	for email, ipsMap := range d.limitedIPs {
		cp := make(map[string]int, len(ipsMap))
		for ip, count := range ipsMap {
			cp[ip] = count
		}
		limitedIPs[email] = cp
	}
	d.mu.RUnlock()

	aliveIPs = make(map[int]map[string]bool)

	// Collect IPs from limited users (from the deep copy above).
	for email, ipsMap := range limitedIPs {
		uid := emailToUID[email]
		if uid == 0 {
			continue
		}
		ipSet := make(map[string]bool, len(ipsMap))
		for ip := range ipsMap {
			ipSet[ip] = true
		}
		if len(ipSet) > 0 {
			aliveIPs[uid] = ipSet
		}
	}

	// Collect IPs from the live-link index too. This matters for removed
	// users: UpdateLimits drops them from emailToUID, so the limited/
	// unlimited collections above would hide their surviving connections
	// from the service's trackAndEnforce safety net. Session emails are
	// "user@<id>", which parses back to a user ID; UUID-keyed emails
	// cannot be mapped here and are handled by CloseUserConnections.
	d.linkMu.Lock()
	linkIPs := make(map[int]map[string]bool)
	for email, set := range d.links {
		if !strings.HasPrefix(email, "user@") {
			continue
		}
		uid, err := strconv.Atoi(strings.TrimPrefix(email, "user@"))
		if err != nil || uid <= 0 {
			continue
		}
		for _, info := range set {
			if info.sourceIP == "" {
				continue
			}
			if linkIPs[uid] == nil {
				linkIPs[uid] = make(map[string]bool)
			}
			linkIPs[uid][info.sourceIP] = true
		}
	}
	d.linkMu.Unlock()
	for uid, ips := range linkIPs {
		if existing, ok := aliveIPs[uid]; ok {
			for ip := range ips {
				existing[ip] = true
			}
		} else {
			aliveIPs[uid] = ips
		}
	}

	// Collect IPs from unlimited users (lock-free).
	d.unlimitedIPs.Range(func(key, value interface{}) bool {
		email := key.(string)
		uid := emailToUID[email]
		if uid == 0 {
			return true
		}
		ic := value.(*ipCounter)
		if ips := ic.aliveIPs(); len(ips) > 0 {
			// Merge with limited IPs if any
			if existing, ok := aliveIPs[uid]; ok {
				for ip := range ips {
					existing[ip] = true
				}
			} else {
				aliveIPs[uid] = ips
			}
		}
		return true
	})

	connCount = int(d.connCount.Load())
	return
}

// ─── Internal helpers ───────────────────────────────────────────────────────

// checkDeviceLimit enforces per-user device limits.
// Fast path: unlimited users use lock-free sync.Map.
// Slow path: limited users use RWMutex with deterministic IP ordering.
// checkDeviceLimit enforces per-user device limits. Every dispatched
// session counts, TCP and UDP alike: a dispatcher Link represents one
// session (not one packet), so excluding UDP let UDP/QUIC users open
// unlimited devices. The old TCP-only counting is the B2 bug.
func (d *LimitDispatcher) checkDeviceLimit(email, sourceIP string) bool {
	d.mu.RLock()
	limit, hasLimit := d.deviceLimits[email]
	d.mu.RUnlock()

	// Fast path: no device limit — use lock-free sync.Map.
	if !hasLimit || limit <= 0 {
		v, _ := d.unlimitedIPs.LoadOrStore(email, &ipCounter{})
		ic := v.(*ipCounter)

		// Increment IP refcount atomically.
		rv, _ := ic.ips.LoadOrStore(sourceIP, &atomic.Int64{})
		rv.(*atomic.Int64).Add(1)
		return false
	}

	// Slow path: user has device limit — the whole decision is made
	// under the write lock. The old code fast-allowed under RLock
	// (merged count < limit) and then incremented under Lock without
	// re-checking the merged count, so concurrent connections from
	// new devices could all observe "under limit" and all be admitted.
	// Taking the write lock for limited users only (unlimited users
	// keep the lock-free path above) closes that window. The rejection
	// callback fires outside mu, so a callback can never deadlock
	// against a dispatcher lock.
	rejected := d.checkDeviceLimitLocked(email, sourceIP, limit)
	if rejected {
		d.notifyDeviceLimitRejected()
	}
	return rejected
}

// SetDeviceLimitRejectedFunc configures the callback invoked once per
// connection rejected by checkDeviceLimit. A nil fn clears the callback.
func (d *LimitDispatcher) SetDeviceLimitRejectedFunc(fn func()) {
	d.deviceLimitRejectedFunc.Store(&fn)
}

// notifyDeviceLimitRejected invokes the rejection callback, if set.
// Callers must only invoke it on the rejection path, outside mu.
func (d *LimitDispatcher) notifyDeviceLimitRejected() {
	if p := d.deviceLimitRejectedFunc.Load(); p != nil && *p != nil {
		(*p)()
	}
}

// checkDeviceLimitLocked decides admission for a limited user while
// holding mu (write). It returns true when the connection must be
// rejected; the caller notifies outside the lock.
func (d *LimitDispatcher) checkDeviceLimitLocked(email, sourceIP string, limit int) bool {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.limitedIPs == nil {
		d.limitedIPs = make(map[string]map[string]int)
	}
	ips := d.limitedIPs[email]
	if ips == nil {
		ips = make(map[string]int)
		d.limitedIPs[email] = ips
	}

	if ips[sourceIP] > 0 {
		ips[sourceIP]++
		return false
	}

	// Merge cross-node state when fresh: a source IP already seen on
	// another node counts toward the limit, and an IP known globally
	// is admitted (counted locally) instead of being double-counted
	// as a new device. Stale or missing global state -> local-only.
	effective := make(map[string]bool, len(ips)+1)
	for ip := range ips {
		effective[ip] = true
	}
	if globalIPs := d.globalIPsFor(email); globalIPs != nil {
		if globalIPs[sourceIP] {
			ips[sourceIP]++
			return false
		}
		for ip := range globalIPs {
			effective[ip] = true
		}
	}

	if len(effective) < limit {
		ips[sourceIP]++
		return false
	}

	// Over limit — deterministic: allow lowest IPs lexicographically.
	ipList := make([]string, 0, len(effective)+1)
	for ip := range effective {
		ipList = append(ipList, ip)
	}
	ipList = append(ipList, sourceIP)
	sort.Strings(ipList)

	for i := 0; i < limit && i < len(ipList); i++ {
		if ipList[i] == sourceIP {
			ips[sourceIP]++
			return false
		}
	}
	return true
}

// UpdateGlobalDevices syncs panel-aggregated cross-node device state.
// IPs are canonicalized on ingest (IPv6 -> /64) so state reported by
// nodes on an older build still merges with local canonical keys.
func (d *LimitDispatcher) UpdateGlobalDevices(users map[string]map[string]bool) {
	d.globalMu.Lock()
	defer d.globalMu.Unlock()
	d.globalDevices = make(map[string]map[string]bool, len(users))
	for email, ips := range users {
		m := make(map[string]bool, len(ips))
		for ip := range ips {
			m[kernel.NormalizeDeviceIP(ip)] = true
		}
		d.globalDevices[email] = m
	}
	d.globalLastUpdate = time.Now()
}

// ClearGlobalDevices resets cross-node device state (on WS disconnect).
func (d *LimitDispatcher) ClearGlobalDevices() {
	d.globalMu.Lock()
	defer d.globalMu.Unlock()
	d.globalDevices = make(map[string]map[string]bool)
	d.globalLastUpdate = time.Time{}
}

// globalIPsFor snapshots the cross-node IP set for one user, or nil when
// the state is stale (>60s, same threshold as the sing-box tracker) or
// missing, in which case callers fall back to local-only counting.
func (d *LimitDispatcher) globalIPsFor(email string) map[string]bool {
	d.globalMu.RLock()
	defer d.globalMu.RUnlock()
	if time.Since(d.globalLastUpdate) > 60*time.Second {
		return nil
	}
	return d.globalDevices[email]
}

// delConn decrements the IP refcount when a connection closes. It
// releases from BOTH buckets: UpdateLimits migrates counts on a limit
// switch, but a connection admitted concurrently with a switch can still
// leave a count in the other bucket, and the old single-bucket lookup
// leaked that device slot permanently.
func (d *LimitDispatcher) delConn(email, sourceIP string) {
	// Unlimited bucket (lock-free).
	if v, ok := d.unlimitedIPs.Load(email); ok {
		ic := v.(*ipCounter)
		if rv, ok := ic.ips.Load(sourceIP); ok {
			counter := rv.(*atomic.Int64)
			if counter.Add(-1) <= 0 {
				ic.ips.Delete(sourceIP)
			}
		}
	}

	// Limited bucket — use write lock.
	d.mu.Lock()
	defer d.mu.Unlock()
	if ips, ok := d.limitedIPs[email]; ok {
		ips[sourceIP]--
		if ips[sourceIP] <= 0 {
			delete(ips, sourceIP)
		}
		if len(ips) == 0 {
			delete(d.limitedIPs, email)
		}
	}
}

type closeTrackingWriter struct {
	buf.Writer
	onClose func()
	closed  atomic.Bool
}

func (w *closeTrackingWriter) Close() error {
	if w.closed.CompareAndSwap(false, true) {
		w.onClose()
	}
	return common.Close(w.Writer)
}

func (w *closeTrackingWriter) Interrupt() {
	if w.closed.CompareAndSwap(false, true) {
		w.onClose()
	}
	common.Interrupt(w.Writer)
}
