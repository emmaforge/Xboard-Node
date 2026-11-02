package panel

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cedar2025/xboard-node/internal/nlog"
	"github.com/gorilla/websocket"
)

// fakeWSServer simulates a minimal Workerman-style WS server for testing.
// It authenticates via token/node_id query params, sends auth.success, then
// delivers the provided events and handles pongs until the client disconnects.
func fakeWSServer(t *testing.T, events []wsMessage) *httptest.Server {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Validate query params
		q := r.URL.Query()
		if q.Get("token") == "" || q.Get("node_id") == "" {
			http.Error(w, "missing auth params", http.StatusUnauthorized)
			return
		}

		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Logf("upgrade error: %v", err)
			return
		}
		defer conn.Close()

		// Send auth.success
		if err := conn.WriteJSON(wsMessage{Event: "auth.success"}); err != nil {
			return
		}

		// Send test events
		for _, evt := range events {
			time.Sleep(50 * time.Millisecond)
			if err := conn.WriteJSON(evt); err != nil {
				return
			}
		}

		// Handle pongs/messages until connection closes
		for {
			var msg wsMessage
			if err := conn.ReadJSON(&msg); err != nil {
				return
			}
			if msg.Event == "pong" {
				// Client pong — acknowledged
			}
		}
	}))
}

func TestWSClient_ConnectAndReceiveDataEvents(t *testing.T) {
	configPayload := syncConfigPayload{
		Config:    NodeConfig{Protocol: "vless", ServerPort: 443},
		Timestamp: 1234,
	}
	usersPayload := syncUsersPayload{
		Users:     []User{{ID: 1, UUID: "abc", SpeedLimit: 100, DeviceLimit: 3}},
		Timestamp: 1235,
	}

	configData, _ := json.Marshal(configPayload)
	usersData, _ := json.Marshal(usersPayload)

	events := []wsMessage{
		{Event: WSEventSyncConfig, Data: configData},
		{Event: WSEventSyncUsers, Data: usersData},
	}
	server := fakeWSServer(t, events)
	defer server.Close()

	host := strings.TrimPrefix(server.URL, "http://")

	var mu sync.Mutex
	var received []WSEvent

	ws := NewWSClient("ws://"+host, "test-token", 1, WSClientConfig{}, func(event WSEvent) {
		mu.Lock()
		received = append(received, event)
		mu.Unlock()
	}, nil, func() map[string]interface{} { return nil })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go ws.Run(ctx)

	// Wait for events
	deadline := time.After(1500 * time.Millisecond)
	for {
		mu.Lock()
		n := len(received)
		mu.Unlock()
		if n >= 2 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for events, got %d", n)
		case <-time.After(50 * time.Millisecond):
		}
	}

	mu.Lock()
	defer mu.Unlock()

	if len(received) != 2 {
		t.Fatalf("expected 2 events, got %d", len(received))
	}

	// Verify config event
	if received[0].Type != WSEventSyncConfig {
		t.Errorf("event[0].Type = %q, want %q", received[0].Type, WSEventSyncConfig)
	}
	if received[0].Config == nil {
		t.Fatal("event[0].Config is nil")
	}
	if received[0].Config.Protocol != "vless" {
		t.Errorf("config.Protocol = %q, want %q", received[0].Config.Protocol, "vless")
	}
	if received[0].Config.ServerPort != 443 {
		t.Errorf("config.ServerPort = %d, want 443", received[0].Config.ServerPort)
	}

	// Verify users event
	if received[1].Type != WSEventSyncUsers {
		t.Errorf("event[1].Type = %q, want %q", received[1].Type, WSEventSyncUsers)
	}
	if len(received[1].Users) != 1 {
		t.Fatalf("expected 1 user, got %d", len(received[1].Users))
	}
	if received[1].Users[0].UUID != "abc" {
		t.Errorf("user.UUID = %q, want %q", received[1].Users[0].UUID, "abc")
	}

	if !ws.IsConnected() {
		t.Error("expected IsConnected() = true while server is running")
	}
}

func TestWSClient_ReconnectOnDisconnect(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}

	var mu sync.Mutex
	connectCount := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}

		mu.Lock()
		connectCount++
		count := connectCount
		mu.Unlock()

		// Send auth.success
		conn.WriteJSON(wsMessage{Event: "auth.success"})

		// Close immediately on first connection to trigger reconnect
		if count == 1 {
			conn.Close()
			return
		}

		// Keep second connection alive
		for {
			var msg wsMessage
			if err := conn.ReadJSON(&msg); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	host := strings.TrimPrefix(server.URL, "http://")
	ws := NewWSClient("ws://"+host, "reconnect-token", 1, WSClientConfig{}, func(WSEvent) {}, nil, func() map[string]interface{} { return nil })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go ws.Run(ctx)

	// Wait for reconnection
	deadline := time.After(4 * time.Second)
	for {
		mu.Lock()
		n := connectCount
		mu.Unlock()
		if n >= 2 {
			break
		}
		select {
		case <-deadline:
			mu.Lock()
			t.Fatalf("expected at least 2 connections, got %d", connectCount)
			mu.Unlock()
		case <-time.After(100 * time.Millisecond):
		}
	}

	if !ws.IsConnected() {
		t.Error("expected IsConnected() = true after reconnect")
	}
}

func TestWSClient_FallbackWhenNoServer(t *testing.T) {
	ws := NewWSClient("ws://127.0.0.1:19999", "fallback-token", 1, WSClientConfig{}, func(WSEvent) {}, nil, func() map[string]interface{} { return nil })

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	go ws.Run(ctx)

	time.Sleep(500 * time.Millisecond)
	if ws.IsConnected() {
		t.Error("expected IsConnected() = false when no server")
	}
}

func TestWSClient_UserDeltaEvent(t *testing.T) {
	deltaPayload := syncUserDeltaPayload{
		Action:    "add",
		Users:     []User{{ID: 42, UUID: "delta-uuid"}},
		Timestamp: 1236,
	}
	deltaData, _ := json.Marshal(deltaPayload)

	events := []wsMessage{
		{Event: WSEventSyncUserDelta, Data: deltaData},
	}
	server := fakeWSServer(t, events)
	defer server.Close()

	host := strings.TrimPrefix(server.URL, "http://")

	var mu sync.Mutex
	var received []WSEvent

	ws := NewWSClient("ws://"+host, "delta-token", 1, WSClientConfig{}, func(event WSEvent) {
		mu.Lock()
		received = append(received, event)
		mu.Unlock()
	}, nil, func() map[string]interface{} { return nil })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go ws.Run(ctx)

	deadline := time.After(1500 * time.Millisecond)
	for {
		mu.Lock()
		n := len(received)
		mu.Unlock()
		if n >= 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for delta event")
		case <-time.After(50 * time.Millisecond):
		}
	}

	mu.Lock()
	defer mu.Unlock()

	if received[0].Type != WSEventSyncUserDelta {
		t.Errorf("event.Type = %q, want %q", received[0].Type, WSEventSyncUserDelta)
	}
	if received[0].DeltaAction != "add" {
		t.Errorf("event.DeltaAction = %q, want %q", received[0].DeltaAction, "add")
	}
	if len(received[0].DeltaUsers) != 1 || received[0].DeltaUsers[0].ID != 42 {
		t.Errorf("unexpected DeltaUsers: %+v", received[0].DeltaUsers)
	}
}

func TestWSClient_EmptyUsersListIsForwardedNotDropped(t *testing.T) {
	// Two variants of "no users": an explicit empty list and a payload
	// with the users field missing entirely. Both must reach the callback
	// as a non-nil empty list, so the service can replace the user set
	// with the empty set (and close those users' connections) instead of
	// silently keeping the old users forever.
	emptyData, _ := json.Marshal(syncUsersPayload{Users: []User{}, Timestamp: 1234})
	missingData, _ := json.Marshal(syncUsersPayload{Timestamp: 1235})

	events := []wsMessage{
		{Event: WSEventSyncUsers, Data: emptyData},
		{Event: WSEventSyncUsers, Data: missingData},
	}
	server := fakeWSServer(t, events)
	defer server.Close()

	host := strings.TrimPrefix(server.URL, "http://")
	var mu sync.Mutex
	var received []WSEvent
	ws := NewWSClient("ws://"+host, "test-token", 1, WSClientConfig{}, func(event WSEvent) {
		mu.Lock()
		received = append(received, event)
		mu.Unlock()
	}, nil, func() map[string]interface{} { return nil })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go ws.Run(ctx)

	deadline := time.After(1500 * time.Millisecond)
	for {
		mu.Lock()
		n := len(received)
		mu.Unlock()
		if n >= 2 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for events, got %d", n)
		case <-time.After(50 * time.Millisecond):
		}
	}
	mu.Lock()
	defer mu.Unlock()
	for i, ev := range received {
		if ev.Type != WSEventSyncUsers {
			t.Errorf("event[%d].Type = %q, want %q", i, ev.Type, WSEventSyncUsers)
		}
		if ev.Users == nil {
			t.Errorf("event[%d].Users is nil, want non-nil empty list", i)
		}
		if len(ev.Users) != 0 {
			t.Errorf("event[%d].Users = %v, want empty", i, ev.Users)
		}
	}
}

// TestWSConnectLogRedactsToken (B7) proves the "ws connecting" Debug line
// never carries the real panel token, while still showing that a
// connection was attempted (with the token masked).
func TestWSConnectLogRedactsToken(t *testing.T) {
	const secret = "b7-secret-token-xyz"

	var buf bytes.Buffer
	nlog.Init(&buf, slog.LevelDebug, false)
	defer nlog.Init(os.Stdout, slog.LevelInfo, false)

	// Server: verify the real token still arrives over the wire (the
	// transport is unchanged), send auth.success, then wait for the
	// client to close.
	sawRequest := make(chan struct{}, 1)
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("token") != secret {
			http.Error(w, "missing auth params", http.StatusUnauthorized)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.WriteJSON(wsMessage{Event: "auth.success"})
		select {
		case sawRequest <- struct{}{}:
		default:
		}
		// Block until the client closes the connection.
		for {
			var msg wsMessage
			if err := conn.ReadJSON(&msg); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	host := strings.TrimPrefix(server.URL, "http://")
	ws := NewWSClient("ws://"+host, secret, 1, WSClientConfig{}, nil, nil, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ws.connect(ctx) }()

	select {
	case <-sawRequest:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("timed out waiting for the server to see the WS handshake")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for connect() to return after cancel")
	}

	out := buf.String()
	if strings.Contains(out, secret) {
		t.Errorf("connect log contains the real token:\n%s", out)
	}
	if !strings.Contains(out, "ws connecting") {
		t.Errorf("expected a 'ws connecting' log line, got:\n%s", out)
	}
	if !strings.Contains(out, "token=REDACTED") {
		t.Errorf("expected the masked token in the log line, got:\n%s", out)
	}
}

// TestWSClient_FirstMessageReadTimeout proves connect() does not block
// forever when the panel accepts the handshake but never sends the
// first (auth) message: the read must fail within HandshakeTimeout
// instead of hanging until the caller's context is cancelled.
func TestWSClient_FirstMessageReadTimeout(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		// Never send auth.success (or anything else); block until
		// the client gives up and closes the connection.
		for {
			var msg wsMessage
			if err := conn.ReadJSON(&msg); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	host := strings.TrimPrefix(server.URL, "http://")
	ws := NewWSClient("ws://"+host, "timeout-token", 1,
		WSClientConfig{HandshakeTimeout: 200 * time.Millisecond}, nil, nil, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	err := ws.connect(ctx)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected connect() to fail on first-message timeout, got nil")
	}
	if !strings.Contains(err.Error(), "read auth response") {
		t.Errorf("error = %v, want it to mention the auth-response read", err)
	}
	if elapsed > 3*time.Second {
		t.Errorf("connect() took %v, want it to fail near HandshakeTimeout, not hang", elapsed)
	}
	if ws.IsConnected() {
		t.Error("expected IsConnected() = false after first-message timeout")
	}
}

// TestWSClient_SilentPeerDetectedByReadDeadline proves a connection to
// a peer that stops speaking (and never answers control pings, i.e. a
// half-open / dead peer) is detected and reported disconnected, so
// Run() can reconnect, instead of staying "connected" forever.
func TestWSClient_SilentPeerDetectedByReadDeadline(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		// Swallow control pings without ponging: this is the
		// half-open peer. The default gorilla ping handler would
		// pong and keep the connection alive, which is not the
		// case under test.
		conn.SetPingHandler(func(string) error { return nil })
		if err := conn.WriteJSON(wsMessage{Event: "auth.success"}); err != nil {
			return
		}
		for {
			var msg wsMessage
			if err := conn.ReadJSON(&msg); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	host := strings.TrimPrefix(server.URL, "http://")
	var mu sync.Mutex
	var sawConnected, sawDisconnected bool
	ws := NewWSClient("ws://"+host, "silent-token", 1,
		WSClientConfig{StatusInterval: 100 * time.Millisecond, BackoffInitial: 100 * time.Millisecond, BackoffMax: 200 * time.Millisecond},
		func(WSEvent) {},
		func(s WSStatusChange) {
			mu.Lock()
			if s.Connected {
				sawConnected = true
			} else if sawConnected {
				sawDisconnected = true
			}
			mu.Unlock()
		},
		nil, // no node.status traffic: only control pings probe the peer
	)

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	go ws.Run(ctx)

	deadline := time.After(5 * time.Second)
	for {
		mu.Lock()
		done := sawConnected && sawDisconnected
		mu.Unlock()
		if done {
			return
		}
		select {
		case <-deadline:
			mu.Lock()
			t.Fatalf("silent peer was not reported disconnected (connected=%v disconnected=%v)", sawConnected, sawDisconnected)
			mu.Unlock()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// TestWSClient_PongKeepsConnectionAlive is the positive control for the
// read deadline: a peer that answers control pings (the default
// gorilla behaviour, and what a live panel does) must NOT be dropped
// even though it sends no data messages for well over pongWait.
func TestWSClient_PongKeepsConnectionAlive(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if err := conn.WriteJSON(wsMessage{Event: "auth.success"}); err != nil {
			return
		}
		// Default ping handler pongs; no data messages are sent.
		for {
			var msg wsMessage
			if err := conn.ReadJSON(&msg); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	host := strings.TrimPrefix(server.URL, "http://")
	var mu sync.Mutex
	var sawConnected, sawDisconnected bool
	ws := NewWSClient("ws://"+host, "pong-token", 1,
		WSClientConfig{StatusInterval: 100 * time.Millisecond},
		func(WSEvent) {},
		func(s WSStatusChange) {
			mu.Lock()
			if s.Connected {
				sawConnected = true
			} else {
				sawDisconnected = true
			}
			mu.Unlock()
		},
		nil,
	)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go ws.Run(ctx)

	// pongWait is 3 x StatusInterval = 300ms; observing for 1.5s
	// covers several missed-tick windows if pongs were ignored.
	time.Sleep(1500 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if !sawConnected {
		t.Fatal("expected the client to connect")
	}
	if sawDisconnected {
		t.Error("ponging peer was reported disconnected, want it to stay connected")
	}
	if !ws.IsConnected() {
		t.Error("expected IsConnected() = true for a ponging peer")
	}
}

// TestWSClient_SendDeviceReportErrors covers the SendDeviceReport /
// SendDeviceReportForNode error contract: not connected, no write
// channel, and a full channel all return a non-nil error, and a
// queued report returns nil with the expected event and payload.
func TestWSClient_SendDeviceReportErrors(t *testing.T) {
	devices := map[int][]string{1: {"10.0.0.1", "10.0.0.2"}}

	t.Run("not connected", func(t *testing.T) {
		ws := NewWSClient("ws://127.0.0.1:19999", "x", 1, WSClientConfig{}, nil, nil, nil)
		if err := ws.SendDeviceReport(devices); err == nil {
			t.Error("SendDeviceReport on a disconnected client = nil, want non-nil error")
		}
		if err := ws.SendDeviceReportForNode(7, devices); err == nil {
			t.Error("SendDeviceReportForNode on a disconnected client = nil, want non-nil error")
		}
	})

	t.Run("connected but no channel", func(t *testing.T) {
		ws := NewWSClient("ws://127.0.0.1:19999", "x", 1, WSClientConfig{}, nil, nil, nil)
		ws.connected.Store(true) // channel deliberately left nil
		if err := ws.SendDeviceReport(devices); err == nil {
			t.Error("SendDeviceReport with nil writeCh = nil, want non-nil error")
		}
	})

	t.Run("channel full", func(t *testing.T) {
		ws := NewWSClient("ws://127.0.0.1:19999", "x", 1, WSClientConfig{}, nil, nil, nil)
		ws.connected.Store(true)
		ch := make(chan wsMessage, 1)
		ws.mu.Lock()
		ws.writeCh = ch
		ws.mu.Unlock()
		ch <- wsMessage{Event: "fill"} // occupy the only slot
		if err := ws.SendDeviceReport(devices); err == nil {
			t.Error("SendDeviceReport on a full channel = nil, want non-nil error")
		} else if !strings.Contains(err.Error(), "full") {
			t.Errorf("full-channel error = %v, want it to mention the full channel", err)
		}
	})

	t.Run("queued with node id", func(t *testing.T) {
		ws := NewWSClient("ws://127.0.0.1:19999", "x", 1, WSClientConfig{}, nil, nil, nil)
		ws.connected.Store(true)
		ch := make(chan wsMessage, 16)
		ws.mu.Lock()
		ws.writeCh = ch
		ws.mu.Unlock()
		if err := ws.SendDeviceReportForNode(7, devices); err != nil {
			t.Fatalf("SendDeviceReportForNode queued = %v, want nil", err)
		}
		select {
		case msg := <-ch:
			if msg.Event != WSEventReportDevices {
				t.Errorf("queued event = %q, want %q", msg.Event, WSEventReportDevices)
			}
			var payload map[string]interface{}
			if err := json.Unmarshal(msg.Data, &payload); err != nil {
				t.Fatalf("cannot decode queued payload: %v", err)
			}
			if payload["node_id"] != float64(7) {
				t.Errorf("payload node_id = %v, want 7", payload["node_id"])
			}
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for the queued device report")
		}
	})

	t.Run("queued legacy mode", func(t *testing.T) {
		ws := NewWSClient("ws://127.0.0.1:19999", "x", 1, WSClientConfig{}, nil, nil, nil)
		ws.connected.Store(true)
		ch := make(chan wsMessage, 16)
		ws.mu.Lock()
		ws.writeCh = ch
		ws.mu.Unlock()
		if err := ws.SendDeviceReport(devices); err != nil {
			t.Fatalf("SendDeviceReport queued = %v, want nil", err)
		}
		select {
		case msg := <-ch:
			if msg.Event != WSEventReportDevices {
				t.Errorf("queued event = %q, want %q", msg.Event, WSEventReportDevices)
			}
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for the queued device report")
		}
	})
}

// TestWSClient_ConcurrentSendNoRace drives all three send paths from
// many goroutines while a live connection runs and then disconnects.
// It asserts nothing about delivery by itself: run it with -race and
// any unsynchronised access to writeCh / connected fails the test.
func TestWSClient_ConcurrentSendNoRace(t *testing.T) {
	server := fakeWSServer(t, nil)
	defer server.Close()

	host := strings.TrimPrefix(server.URL, "http://")
	ws := NewWSClient("ws://"+host, "race-token", 1,
		WSClientConfig{StatusInterval: 100 * time.Millisecond},
		func(WSEvent) {}, nil,
		func() map[string]interface{} { return map[string]interface{}{"online": 1} })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ws.Run(ctx)

	deadline := time.After(3 * time.Second)
	for !ws.IsConnected() {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for connection")
		case <-time.After(20 * time.Millisecond):
		}
	}

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				_ = ws.SendDeviceReport(map[int][]string{id: {"10.0.0.1"}})
				ws.SendNodeStatus(id, map[string]interface{}{"online": 1})
				ws.SendRaw("test.event", json.RawMessage(`{}`))
			}
		}(g)
	}
	wg.Wait()

	// Disconnect while senders are still in flight: a final burst
	// must neither panic nor race, whatever it returns.
	cancel()
	for i := 0; i < 50; i++ {
		_ = ws.SendDeviceReport(devicesForRaceTest)
		ws.SendRaw("test.event", json.RawMessage(`{}`))
	}
}

var devicesForRaceTest = map[int][]string{1: {"10.0.0.1"}}
