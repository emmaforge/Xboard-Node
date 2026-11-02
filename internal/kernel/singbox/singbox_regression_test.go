package singbox

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sagernet/sing/common/buf"
	"github.com/sagernet/sing/common/bufio"
	singM "github.com/sagernet/sing/common/metadata"

	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/model"
)

// fakePacketConn is a minimal N.PacketConn with scripted reads, used to
// drive trackedPacketConn through both its direct and CopyPacket paths.
type fakePacketConn struct {
	reads      [][]byte
	writes     [][]byte
	writeDests []singM.Socksaddr
	closed     bool
}

func (c *fakePacketConn) ReadPacket(buffer *buf.Buffer) (singM.Socksaddr, error) {
	if len(c.reads) == 0 {
		return singM.Socksaddr{}, io.EOF
	}
	chunk := c.reads[0]
	c.reads = c.reads[1:]
	if _, err := buffer.Write(chunk); err != nil {
		return singM.Socksaddr{}, err
	}
	return singM.Socksaddr{}, nil
}

func (c *fakePacketConn) WritePacket(buffer *buf.Buffer, dest singM.Socksaddr) error {
	c.writes = append(c.writes, append([]byte(nil), buffer.Bytes()...))
	c.writeDests = append(c.writeDests, dest)
	return nil
}

func (c *fakePacketConn) Close() error                     { c.closed = true; return nil }
func (c *fakePacketConn) LocalAddr() net.Addr              { return &net.UDPAddr{} }
func (c *fakePacketConn) SetDeadline(time.Time) error      { return nil }
func (c *fakePacketConn) SetReadDeadline(time.Time) error  { return nil }
func (c *fakePacketConn) SetWriteDeadline(time.Time) error { return nil }

func TestTrackedPacketConnCopyPacketDirection(t *testing.T) {
	// Regression test for the UDP packet-relay direction swap: the
	// CopyPacket unwrap path used to count reads as download and writes
	// as upload, opposite to the direct ReadPacket/WritePacket path.
	// Upload direction: tracked conn as the CopyPacket source (read side).
	tracker := NewConnTracker(0)
	tracker.SetUserMap(map[string]int{"uuid-1": 1})
	ctx := context.Background()

	upBase := &fakePacketConn{reads: [][]byte{[]byte("hello")}} // 5 bytes
	upWrapped := tracker.RoutedPacketConnection(ctx, upBase, testInboundContext("uuid-1", "1.1.1.1"), nil, nil)
	sink := &fakePacketConn{}
	if _, err := bufio.CopyPacket(sink, upWrapped); err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("CopyPacket upload direction error = %v", err)
	}
	if len(sink.writes) != 1 || string(sink.writes[0]) != "hello" {
		t.Fatalf("sink writes = %q, want [hello]", sink.writes)
	}
	traffic, _, _ := tracker.GetUserTraffic()
	if got := traffic[1]; got != [2]int64{5, 0} {
		t.Fatalf("traffic after CopyPacket read = %v, want [5 0] (upload)", got)
	}

	// Download direction: tracked conn as the CopyPacket destination
	// (write side). trackedPacketConn must report WriterReplaceable()
	// == false (see its doc): sing's UnwrapCountPacketWriter peels
	// replaceable wrappers BEFORE checking for counters, so a
	// replaceable counting wrapper's writer CountFunc was never
	// collected and CopyPacket download traffic went uncounted. With
	// the fix, the CopyPacket run itself must count the 4 bytes as
	// download — no manual CountFunc invocation.
	downBase := &fakePacketConn{}
	downWrapped := tracker.RoutedPacketConnection(ctx, downBase, testInboundContext("uuid-1", "1.1.1.1"), nil, nil)
	src := &fakePacketConn{reads: [][]byte{[]byte("bye!")}} // 4 bytes
	if _, err := bufio.CopyPacket(downWrapped, src); err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("CopyPacket download direction error = %v", err)
	}
	if len(downBase.writes) != 1 || string(downBase.writes[0]) != "bye!" {
		t.Fatalf("downstream writes = %q, want [bye!]", downBase.writes)
	}
	traffic, _, _ = tracker.GetUserTraffic()
	if got := traffic[1]; got != [2]int64{5, 4} {
		t.Fatalf("traffic after CopyPacket write = %v, want [5 4] (download counted once)", got)
	}

	// Direct path direction must be unchanged: ReadPacket = upload,
	// WritePacket = download.
	directBase := &fakePacketConn{reads: [][]byte{[]byte("abc")}} // 3 bytes
	direct := tracker.RoutedPacketConnection(ctx, directBase, testInboundContext("uuid-1", "1.1.1.1"), nil, nil)
	readBuf := buf.New()
	if _, err := direct.ReadPacket(readBuf); err != nil {
		t.Fatalf("direct ReadPacket error = %v", err)
	}
	readBuf.Release()
	writeBuf := buf.New()
	if _, err := writeBuf.Write([]byte("xy")); err != nil { // 2 bytes
		t.Fatalf("writeBuf.Write error = %v", err)
	}
	if err := direct.WritePacket(writeBuf, singM.Socksaddr{}); err != nil {
		t.Fatalf("direct WritePacket error = %v", err)
	}
	writeBuf.Release()
	traffic, _, _ = tracker.GetUserTraffic()
	if got := traffic[1]; got != [2]int64{8, 6} {
		t.Fatalf("traffic after direct path = %v, want [8 6]", got)
	}

	// Stream (TCP) path must be untouched by the packet fix.
	streamTracker := NewConnTracker(0)
	streamTracker.SetUserMap(map[string]int{"uuid-1": 1})
	streamBase := &testConn{reads: [][]byte{[]byte("hello")}}
	streamWrapped := streamTracker.RoutedConnection(ctx, streamBase, testInboundContext("uuid-1", "1.1.1.1"), nil, nil)
	streamBuf := make([]byte, 16)
	if n, err := streamWrapped.Read(streamBuf); err != nil || n != 5 {
		t.Fatalf("stream Read() = (%d, %v), want (5, nil)", n, err)
	}
	if _, err := streamWrapped.Write([]byte("bye")); err != nil {
		t.Fatalf("stream Write() error = %v", err)
	}
	streamTraffic, _, _ := streamTracker.GetUserTraffic()
	if got := streamTraffic[1]; got != [2]int64{5, 3} {
		t.Fatalf("stream traffic = %v, want [5 3]", got)
	}
}

func TestTrackedPacketConnCopyPacketProductionChains(t *testing.T) {
	// Production wrapping orders from sing-box routePacketConnection:
	// raw → Cached → tracked, and additionally FakeIP NAT on the
	// outside (NAT(NewNetPacketConn(tracked))). In both chains the
	// CopyPacket download leg must deliver the bytes AND count them
	// exactly once as download; direct WritePacket through the NAT
	// chain must still apply NAT address translation and count once
	// (direct writers are not unwrapped by CopyPacket).
	ctx := context.Background()

	// Chain 1: Cached inside tracked (the default chain).
	tracker := NewConnTracker(0)
	tracker.SetUserMap(map[string]int{"uuid-1": 1})
	cachedBase := &fakePacketConn{}
	// NewCachedPacketConn requires a non-nil buffer (it IncRefs it);
	// the buffer only matters on the read side, this chain is
	// exercised as a CopyPacket destination.
	cachedBuf := buf.New()
	defer cachedBuf.Release()
	cached := bufio.NewCachedPacketConn(cachedBase, cachedBuf, singM.Socksaddr{})
	// RoutedPacketConnection wraps whatever conn it is given, so wrap
	// the cached conn the same way the router wraps the raw conn.
	trackedCached := &trackedPacketConn{
		PacketConn: cached,
		tracker:    tracker,
		us:         tracker.users[1],
		userID:     1,
		connID:     "cached-chain",
		sourceIP:   "1.1.1.1",
		ctx:        ctx,
	}
	src := &fakePacketConn{reads: [][]byte{[]byte("data!")}} // 5 bytes
	if _, err := bufio.CopyPacket(trackedCached, src); err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("CopyPacket cached chain error = %v", err)
	}
	if len(cachedBase.writes) != 1 || string(cachedBase.writes[0]) != "data!" {
		t.Fatalf("cached chain writes = %q, want [data!]", cachedBase.writes)
	}
	if traffic, _, _ := tracker.GetUserTraffic(); traffic[1] != [2]int64{0, 5} {
		t.Fatalf("cached chain traffic = %v, want [0 5]", traffic[1])
	}

	// Chain 2: FakeIP NAT outside tracked.
	natTracker := NewConnTracker(0)
	natTracker.SetUserMap(map[string]int{"uuid-1": 1})
	natBase := &fakePacketConn{}
	natTracked := natTracker.RoutedPacketConnection(ctx, natBase, testInboundContext("uuid-1", "1.1.1.1"), nil, nil)
	origin := singM.SocksaddrFromNet(&net.UDPAddr{IP: net.ParseIP("203.0.113.10")})
	fakeAddr := singM.SocksaddrFromNet(&net.UDPAddr{IP: net.ParseIP("198.18.0.5")})
	natConn := bufio.NewNATPacketConn(bufio.NewNetPacketConn(natTracked), origin, fakeAddr)
	natSrc := &fakePacketConn{reads: [][]byte{[]byte("nat-data")}} // 8 bytes
	if _, err := bufio.CopyPacket(natConn, natSrc); err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("CopyPacket NAT chain error = %v", err)
	}
	if len(natBase.writes) != 1 || string(natBase.writes[0]) != "nat-data" {
		t.Fatalf("NAT chain writes = %q, want [nat-data]", natBase.writes)
	}
	if traffic, _, _ := natTracker.GetUserTraffic(); traffic[1] != [2]int64{0, 8} {
		t.Fatalf("NAT chain traffic = %v, want [0 8] (download counted once)", traffic[1])
	}

	// Direct write through the NAT chain: NAT translation must still
	// apply (fake destination rewritten to origin) and the bytes must
	// be counted exactly once via tracked.WritePacket, not twice.
	directBuf := buf.New()
	if _, err := directBuf.Write([]byte("hi")); err != nil {
		t.Fatalf("directBuf.Write error = %v", err)
	}
	if err := natConn.WritePacket(directBuf, fakeAddr); err != nil {
		t.Fatalf("NAT direct WritePacket error = %v", err)
	}
	directBuf.Release()
	if len(natBase.writeDests) != 2 {
		t.Fatalf("NAT direct write dests = %d, want 2 (CopyPacket + direct)", len(natBase.writeDests))
	}
	// The direct write's destination must have been rewritten from the
	// fake address to the origin address by NAT.
	got := natBase.writeDests[1]
	if got.Addr.String() != origin.Addr.String() {
		t.Fatalf("NAT direct write dest = %v, want origin %v", got, origin)
	}
	if traffic, _, _ := natTracker.GetUserTraffic(); traffic[1] != [2]int64{0, 10} {
		t.Fatalf("NAT chain traffic after direct write = %v, want [0 10] (no double count)", traffic[1])
	}
}

func TestGateDeviceAndRegisterSequential(t *testing.T) {
	tracker := NewConnTracker(0)
	tracker.SetUserMap(map[string]int{"uuid-1": 1})
	us := tracker.users[1]

	// Limit 2: first two distinct IPs admitted, third (lexicographically
	// largest) rejected and NOT registered.
	if !tracker.gateDeviceAndRegister(us, 1, "1.1.1.1", 2) {
		t.Fatal("first IP should be admitted")
	}
	if !tracker.gateDeviceAndRegister(us, 1, "2.2.2.2", 2) {
		t.Fatal("second IP should be admitted")
	}
	if tracker.gateDeviceAndRegister(us, 1, "3.3.3.3", 2) {
		t.Fatal("third IP should be rejected")
	}
	if ips := us.aliveIPList(); len(ips) != 2 || ips["3.3.3.3"] {
		t.Fatalf("rejected IP must not be registered, alive = %v", ips)
	}
	// Already-known IP still admitted and refcounted.
	if !tracker.gateDeviceAndRegister(us, 1, "1.1.1.1", 2) {
		t.Fatal("known IP should be admitted")
	}
	if got := us.connCount; got != 3 {
		t.Fatalf("connCount = %d, want 3", got)
	}
}

func TestGateDeviceAndRegisterConcurrentSameIP(t *testing.T) {
	// Concurrent registrations of one new IP must all be admitted with
	// an exact refcount, and the write-locked gate must not race under
	// -race (run the full suite with -race to check).
	tracker := NewConnTracker(0)
	tracker.SetUserMap(map[string]int{"uuid-1": 1})
	us := tracker.users[1]

	const n = 50
	var wg sync.WaitGroup
	results := make([]bool, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = tracker.gateDeviceAndRegister(us, 1, "1.1.1.1", 1)
		}(i)
	}
	wg.Wait()
	for i, ok := range results {
		if !ok {
			t.Fatalf("goroutine %d: same-IP registration rejected", i)
		}
	}
	if got := us.connCount; got != n {
		t.Fatalf("connCount = %d, want %d", got, n)
	}
	if ips := us.aliveIPList(); len(ips) != 1 {
		t.Fatalf("alive IPs = %v, want exactly one", ips)
	}
}

func TestGateDeviceAndRegisterConcurrentDistinctIPsIntegrity(t *testing.T) {
	// Concurrent distinct-IP registrations under the gate must keep the
	// refcounts consistent: every admitted registration appears in the
	// alive set, and closing all of them drains the set to zero. (The
	// lexicographic admission rule itself can admit beyond the limit in
	// some arrival orders — pre-existing semantics — so this test
	// asserts bookkeeping integrity, not a hard cap.)
	tracker := NewConnTracker(0)
	tracker.SetUserMap(map[string]int{"uuid-1": 1})
	us := tracker.users[1]

	ips := []string{"1.1.1.1", "2.2.2.2", "3.3.3.3", "4.4.4.4", "5.5.5.5"}
	var wg sync.WaitGroup
	admitted := make([]bool, len(ips))
	for i, ip := range ips {
		wg.Add(1)
		go func(i int, ip string) {
			defer wg.Done()
			admitted[i] = tracker.gateDeviceAndRegister(us, 1, ip, 3)
		}(i, ip)
	}
	wg.Wait()
	alive := us.aliveIPList()
	admittedCount := 0
	for i, ip := range ips {
		if admitted[i] {
			admittedCount++
			if !alive[ip] {
				t.Fatalf("admitted IP %s missing from alive set %v", ip, alive)
			}
		}
	}
	if got := us.connCount; got != admittedCount {
		t.Fatalf("connCount = %d, want admitted count %d", got, admittedCount)
	}
}

func TestMergeUsers(t *testing.T) {
	current := []model.UserSpec{
		{ID: 1, UUID: "uuid-old", SpeedLimit: 10, DeviceLimit: 1},
		{ID: 2, UUID: "uuid-2", SpeedLimit: 0, DeviceLimit: 0},
	}

	// UUID rotation on an existing ID updates in place; the old UUID
	// must not survive in the merged list.
	merged, changed := mergeUsers(current, []model.UserSpec{
		{ID: 1, UUID: "uuid-new", SpeedLimit: 20, DeviceLimit: 2},
	})
	if changed != 1 {
		t.Fatalf("changed = %d, want 1", changed)
	}
	if len(merged) != 2 || merged[0].UUID != "uuid-new" || merged[0].SpeedLimit != 20 {
		t.Fatalf("merged = %+v, want ID 1 rotated to uuid-new", merged)
	}
	for _, u := range merged {
		if u.UUID == "uuid-old" {
			t.Fatal("old UUID must not survive a rotation")
		}
	}

	// Unchanged input changes nothing.
	if _, changed := mergeUsers(current, []model.UserSpec{{ID: 1, UUID: "uuid-old", SpeedLimit: 10, DeviceLimit: 1}}); changed != 0 {
		t.Fatalf("changed = %d, want 0 for identical input", changed)
	}

	// New ID appended, rotation + addition counted together.
	merged, changed = mergeUsers(current, []model.UserSpec{
		{ID: 1, UUID: "uuid-new"},
		{ID: 3, UUID: "uuid-3"},
	})
	if changed != 2 || len(merged) != 3 || merged[2].ID != 3 {
		t.Fatalf("merged = %+v, changed = %d, want 3 users and changed 2", merged, changed)
	}
}

func TestConnTrackerDeviceLimitRejectedCallback(t *testing.T) {
	tracker := NewConnTracker(0)
	tracker.SetUserMap(map[string]int{"uuid-1": 1})
	tracker.SetDeviceLimitFunc(func(uuid string) (int, bool) {
		if uuid != "uuid-1" {
			return 0, false
		}
		return 1, true
	})
	var rejected atomic.Int32
	tracker.SetDeviceLimitRejectedFunc(func() { rejected.Add(1) })
	ctx := context.Background()

	// TCP: first connection admitted, no callback.
	first := &testConn{}
	if wrapped := tracker.RoutedConnection(ctx, first, testInboundContext("uuid-1", "1.1.1.1"), nil, nil); wrapped == first {
		t.Fatal("first TCP connection should be admitted (wrapped)")
	}
	if got := rejected.Load(); got != 0 {
		t.Fatalf("rejected = %d after admission, want 0", got)
	}

	// TCP: second distinct IP rejected, callback fires exactly once.
	second := &testConn{}
	if wrapped := tracker.RoutedConnection(ctx, second, testInboundContext("uuid-1", "2.2.2.2"), nil, nil); wrapped != second {
		t.Fatal("rejected TCP connection should be returned unwrapped")
	}
	if !second.closed {
		t.Fatal("rejected TCP connection should be closed")
	}
	if got := rejected.Load(); got != 1 {
		t.Fatalf("rejected = %d after TCP rejection, want 1", got)
	}

	// Already-known IP admitted again, no callback.
	third := &testConn{}
	if wrapped := tracker.RoutedConnection(ctx, third, testInboundContext("uuid-1", "1.1.1.1"), nil, nil); wrapped == third {
		t.Fatal("known IP should be admitted")
	}
	if got := rejected.Load(); got != 1 {
		t.Fatalf("rejected = %d after known-IP admission, want 1", got)
	}

	// UDP: distinct IP rejected, callback fires again.
	udpBase := &testPacketConn{}
	if wrapped := tracker.RoutedPacketConnection(ctx, udpBase, testInboundContext("uuid-1", "3.3.3.3"), nil, nil); wrapped != udpBase {
		t.Fatal("rejected UDP connection should be returned unwrapped")
	}
	if !udpBase.closed {
		t.Fatal("rejected UDP connection should be closed")
	}
	if got := rejected.Load(); got != 2 {
		t.Fatalf("rejected = %d after UDP rejection, want 2", got)
	}

	// Clearing the callback with nil must not panic on later rejections.
	tracker.SetDeviceLimitRejectedFunc(nil)
	fourth := &testConn{}
	tracker.RoutedConnection(ctx, fourth, testInboundContext("uuid-1", "4.4.4.4"), nil, nil)
	if got := rejected.Load(); got != 2 {
		t.Fatalf("rejected = %d after clearing callback, want 2", got)
	}
}

func TestSingBoxSetDeviceLimitRejectedFuncForwardsToTracker(t *testing.T) {
	s := New(config.KernelConfig{Type: "sing-box"})
	// Normally created by Start; assigned directly here (same package).
	s.connTracker = NewConnTracker(0)
	s.connTracker.SetUserMap(map[string]int{"uuid-1": 1})
	s.connTracker.SetDeviceLimitFunc(func(uuid string) (int, bool) {
		if uuid != "uuid-1" {
			return 0, false
		}
		return 1, true
	})
	var rejected atomic.Int32
	s.SetDeviceLimitRejectedFunc(func() { rejected.Add(1) })

	ctx := context.Background()
	first := &testConn{}
	tracker := s.connTracker
	if wrapped := tracker.RoutedConnection(ctx, first, testInboundContext("uuid-1", "1.1.1.1"), nil, nil); wrapped == first {
		t.Fatal("first connection should be admitted")
	}
	second := &testConn{}
	tracker.RoutedConnection(ctx, second, testInboundContext("uuid-1", "2.2.2.2"), nil, nil)
	if got := rejected.Load(); got != 1 {
		t.Fatalf("rejected via SingBox passthrough = %d, want 1", got)
	}
	// The setter also stores the func for trackers created by later Starts.
	if s.deviceLimitRejectedFunc == nil {
		t.Fatal("SingBox should retain the rejection func for future trackers")
	}
}
