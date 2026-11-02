package panel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cedar2025/xboard-node/internal/nlog"
	"github.com/gorilla/websocket"
)

// WSEvent types
const (
	WSEventSyncConfig    = "sync.config"
	WSEventSyncUsers     = "sync.users"
	WSEventSyncUserDelta = "sync.user.delta"
	WSEventSyncDevices   = "sync.devices"   // panel → node: global device state
	WSEventSyncNodes     = "sync.nodes"     // panel → machine: node list changed
	WSEventReportDevices = "report.devices" // node → panel: report device snapshot
)

// WSEvent is a parsed data event delivered to the service layer.
type WSEvent struct {
	Type        string
	Config      *NodeConfig
	Users       []User
	DeltaAction string // "add" or "remove" (only for sync.user.delta)
	DeltaUsers  []User // users affected by the delta

	// Device sync fields
	DeviceUsers map[int][]string // userID -> IPs (for sync.devices)
	NodeID      int

	// Machine node discovery fields (for sync.nodes)
	Nodes []MachineNode
}

// WSStatusChange notifies the service when WS connectivity changes.
type WSStatusChange struct {
	Connected bool
}

// wsMessage is the JSON envelope for all WS messages.
type wsMessage struct {
	Event     string          `json:"event"`
	Data      json.RawMessage `json:"data,omitempty"`
	Timestamp int64           `json:"timestamp,omitempty"`
}

// Payload structures for data events.
type syncConfigPayload struct {
	Config    NodeConfig `json:"config"`
	Timestamp int64      `json:"timestamp"`
	NodeID    int        `json:"node_id"`
}

type syncUsersPayload struct {
	Users     []User `json:"users"`
	Timestamp int64  `json:"timestamp"`
	NodeID    int    `json:"node_id"`
}

type syncUserDeltaPayload struct {
	Action    string `json:"action"`
	Users     []User `json:"users"`
	Timestamp int64  `json:"timestamp"`
	NodeID    int    `json:"node_id"`
}

// syncDevicesPayload carries global device state from panel.
type syncDevicesPayload struct {
	Users     map[int][]string `json:"users"`
	Timestamp int64            `json:"timestamp"`
	NodeID    int              `json:"node_id"`
}

// syncNodesPayload carries the updated node list for a machine.
type syncNodesPayload struct {
	Nodes []MachineNode `json:"nodes"`
}

// WSClientConfig holds WebSocket client tuning options.
type WSClientConfig struct {
	StatusInterval   time.Duration
	HandshakeTimeout time.Duration
	BackoffInitial   time.Duration
	BackoffMax       time.Duration
	MachineID        int
}

// WSClient connects to the panel's Workerman WS server using native WebSocket.
// Authentication is done via query parameters (token + node_id) during the
// WebSocket handshake — no separate auth step needed.
//
// B7: the token must stay in the query. The Xboard panel's Server /
// ServerV2 middleware validates $request->input('token'), which Laravel
// fills from the query string, form fields and JSON body — never from
// request headers — and a WebSocket handshake (a GET upgrade) has no
// practical body channel. An Authorization header would therefore be
// rejected by the panel. This fix only redacts the token from log output
// (see RedactURL); it does not change how the token is transmitted.
type WSClient struct {
	wsURL    string // base WS URL, e.g. ws://panel.example.com:8076
	token    string
	nodeID   int
	onEvent  func(WSEvent)
	onStatus func(WSStatusChange)
	onPing   func() map[string]interface{}

	cfg WSClientConfig

	connected atomic.Bool

	// mu guards writeCh. Senders must hold mu (read lock) for the
	// whole check-and-enqueue step, and connect()'s disconnect
	// cleanup must hold mu (write lock) while clearing writeCh, so
	// a sender can never observe a half-updated channel, send to a
	// channel that has just been orphaned, or race with the clear.
	// writeCh itself is only ever drained by connect()'s main loop.
	mu sync.RWMutex
	// writeCh allows sending messages from outside the connect loop.
	// Ordering (startup): the channel is created and stored here
	// BEFORE connected is set to true, so a sender that sees
	// connected==true always finds a usable channel (or a nil
	// channel during the brief disconnect window, which trySend
	// reports as an error instead of silently dropping).
	// Ordering (disconnect): connect()'s deferred cleanup clears
	// writeCh while connected is still true; senders in that window
	// get errWSNoWriteChannel, and Run() then flips connected to
	// false. A message enqueued just before the read loop exits
	// may still be left undrained in the old channel — the loop is
	// the only drainer, so exactly-once delivery across a
	// disconnect is not guaranteed; callers must treat a nil
	// return as "queued", not "delivered", and retry on the next
	// connection (see the service-layer device-report recovery).
	writeCh chan wsMessage
}

// Send failures reported by trySend / SendDeviceReport*.
var (
	errWSNotConnected     = errors.New("ws: not connected")
	errWSNoWriteChannel   = errors.New("ws: write channel unavailable")
	errWSWriteChannelFull = errors.New("ws: write channel full, message dropped")
)

// trySend enqueues msg for connect()'s main loop, or reports why it
// could not be queued. It never blocks and never panics on a nil or
// concurrently-cleared channel: the connected flag and the channel
// are re-checked while holding mu, and the non-blocking send happens
// under the same read lock that disconnect cleanup must take as a
// write lock before clearing writeCh.
func (w *WSClient) trySend(msg wsMessage) error {
	if !w.connected.Load() {
		return errWSNotConnected
	}
	w.mu.RLock()
	defer w.mu.RUnlock()
	if !w.connected.Load() {
		return errWSNotConnected
	}
	ch := w.writeCh
	if ch == nil {
		return errWSNoWriteChannel
	}
	select {
	case ch <- msg:
		return nil
	default:
		return errWSWriteChannelFull
	}
}

// NewWSClient creates a new WebSocket client.
// wsURL is the base WebSocket URL (e.g. "ws://panel.example.com:8076").
// token and nodeID are used for authentication via query parameters.
func NewWSClient(wsURL string, token string, nodeID int, cfg WSClientConfig, onEvent func(WSEvent), onStatus func(WSStatusChange), onPing func() map[string]interface{}) *WSClient {
	// Apply defaults
	if cfg.StatusInterval == 0 {
		cfg.StatusInterval = 10 * time.Second
	}
	if cfg.HandshakeTimeout == 0 {
		cfg.HandshakeTimeout = 15 * time.Second
	}
	if cfg.BackoffInitial == 0 {
		cfg.BackoffInitial = time.Second
	}
	if cfg.BackoffMax == 0 {
		cfg.BackoffMax = 60 * time.Second
	}
	return &WSClient{
		wsURL:    wsURL,
		token:    token,
		nodeID:   nodeID,
		cfg:      cfg,
		onEvent:  onEvent,
		onStatus: onStatus,
		onPing:   onPing,
	}
}

func (w *WSClient) IsConnected() bool { return w.connected.Load() }

func (w *WSClient) notifyStatus(connected bool) {
	if w.onStatus != nil {
		w.onStatus(WSStatusChange{Connected: connected})
	}
}

// Run connects and reconnects until ctx is cancelled.
func (w *WSClient) Run(ctx context.Context) {
	backoff := w.cfg.BackoffInitial
	for {
		start := time.Now()
		err := w.connect(ctx)

		wasConnected := w.connected.Swap(false)
		if wasConnected {
			w.notifyStatus(false)
		}

		if err != nil {
			nlog.Core().Warn("ws disconnected", "error", err)
			if !wasConnected {
				w.notifyStatus(false)
			}
		}

		select {
		case <-ctx.Done():
			return
		default:
		}

		// Reset backoff if the connection was up for a meaningful duration.
		if time.Since(start) > 2*time.Minute {
			backoff = w.cfg.BackoffInitial
		}

		// Apply exponential backoff with jitter to prevent thundering herd.
		jitter := time.Duration(rand.Int63n(int64(backoff / 5)))
		wait := backoff + jitter

		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if backoff < w.cfg.BackoffMax {
			backoff = min(backoff*2, w.cfg.BackoffMax)
		}
	}
}

func (w *WSClient) connect(ctx context.Context) error {
	u, err := url.Parse(w.wsURL)
	if err != nil {
		return fmt.Errorf("parse ws url: %w", err)
	}
	q := u.Query()
	q.Set("token", w.token)
	if w.cfg.MachineID > 0 {
		q.Set("machine_id", strconv.Itoa(w.cfg.MachineID))
	} else {
		q.Set("node_id", strconv.Itoa(w.nodeID))
	}
	u.RawQuery = q.Encode()

	// B7: u.String() carries the panel token in its query string, so it
	// must never reach the logs verbatim (Debug output included).
	nlog.Core().Debug("ws connecting", "url", RedactURL(u.String()))

	dialer := websocket.Dialer{
		HandshakeTimeout: w.cfg.HandshakeTimeout,
	}
	conn, _, err := dialer.DialContext(ctx, u.String(), nil)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	conn.SetReadLimit(10 << 20) // 10MB max message size

	// Half-open / dead-peer detection, part 1: the first message
	// must arrive within the handshake timeout. Without a read
	// deadline, a peer that accepts the TCP/WS handshake and then
	// never speaks would block connect() forever and Run() would
	// never back off or reconnect. Reuses HandshakeTimeout so the
	// first read has the same budget as the handshake itself.
	handshakeTimeout := w.cfg.HandshakeTimeout
	if handshakeTimeout <= 0 {
		handshakeTimeout = 15 * time.Second
	}
	if err := conn.SetReadDeadline(time.Now().Add(handshakeTimeout)); err != nil {
		return fmt.Errorf("set first read deadline: %w", err)
	}
	// Read first message — expect auth.success or error
	var firstMsg wsMessage
	if err := conn.ReadJSON(&firstMsg); err != nil {
		return fmt.Errorf("read auth response: %w", err)
	}
	// First message arrived; the loop below installs its own
	// heartbeat deadline, so clear the one-shot deadline here.
	_ = conn.SetReadDeadline(time.Time{})
	nlog.Core().Debug("ws recv", "event", firstMsg.Event, "data", string(firstMsg.Data))

	if firstMsg.Event == "error" {
		var errData struct {
			Message string `json:"message"`
		}
		if err := json.Unmarshal(firstMsg.Data, &errData); err != nil {
			return fmt.Errorf("auth failed (unable to parse error: %v)", err)
		}
		return fmt.Errorf("auth failed: %s", errData.Message)
	}

	// writeCh decouples data collection from network I/O.
	// It MUST be stored before connected is set to true: the old
	// order (connected first, writeCh second, both unsynchronised)
	// let a concurrent sender see connected==true with writeCh
	// still nil and silently drop its message via the default case
	// of a send on a nil channel, besides racing with the
	// disconnect clear under -race.
	writeCh := make(chan wsMessage, 16)
	w.mu.Lock()
	w.writeCh = writeCh
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		w.writeCh = nil
		w.mu.Unlock()
	}()

	w.connected.Store(true)
	w.notifyStatus(true)
	if firstMsg.Event != "auth.success" {
		// It might be a data event already (server pushed sync before auth.success)
		// Process it and continue
		w.handleMessage(firstMsg)
	}

	// Half-open / dead-peer detection, part 2: heartbeat read
	// deadline. The loop refreshes the deadline on every inbound
	// frame (control pong via the handler below, and any data /
	// app-level ping message after a successful read), so a peer
	// that stops speaking — including a half-open TCP connection
	// where writes still appear to succeed — makes ReadJSON fail
	// with a timeout after pongWait, which the main loop turns
	// into a disconnect and Run() turns into a reconnect. The
	// tolerance is a few heartbeat periods, not one, so one missed
	// tick does not kill a healthy connection.
	pongWait := 3 * w.cfg.StatusInterval
	if pongWait <= 0 {
		pongWait = 30 * time.Second
	}
	conn.SetPongHandler(func(string) error {
		// Pong arrived: the peer is alive; extend the deadline.
		// Errors from SetReadDeadline are ignored here because
		// the in-flight ReadJSON will surface a failure anyway.
		_ = conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})
	if err := conn.SetReadDeadline(time.Now().Add(pongWait)); err != nil {
		return fmt.Errorf("set read deadline: %w", err)
	}

	// Ping interval: send pong responses to server pings.
	// We also use this timer to trigger periodic status pushes.
	reportTicker := time.NewTicker(w.cfg.StatusInterval)
	defer reportTicker.Stop()

	errCh := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			var msg wsMessage
			if err := conn.ReadJSON(&msg); err != nil {
				select {
				case errCh <- err:
				default:
				}
				return
			}
			// Any inbound message proves the peer is alive, even
			// if it never sends a control pong: extend the
			// deadline so a chatty-but-pongless peer is not
			// mistaken for a dead one.
			_ = conn.SetReadDeadline(time.Now().Add(pongWait))
			nlog.Core().Debug("ws recv", "event", msg.Event, "data", string(msg.Data))
			w.handleMessage(msg)
			if msg.Event == "ping" {
				select {
				case writeCh <- wsMessage{Event: "pong"}:
				default:
					nlog.Core().Warn("ws write channel full, skipping pong")
				}
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			conn.WriteMessage(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
			<-done
			return nil

		case err := <-errCh:
			return fmt.Errorf("read: %w", err)

		case <-reportTicker.C:
			// Heartbeat probe: a control ping elicits a pong from
			// a live peer, which refreshes the read deadline
			// above. A dead peer never pongs, so the deadline
			// expires and the connection is re-established by
			// Run(). A ping write failure is fatal for this
			// connection (unlike a merely-full status queue).
			// The main loop is the only goroutine writing to
			// conn, so this cannot interleave with WriteJSON.
			if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second)); err != nil {
				return fmt.Errorf("ping: %w", err)
			}
			// Send periodic node.status via WebSocket
			if w.onPing != nil {
				msg := wsMessage{Event: "node.status"}
				if stats := w.onPing(); stats != nil {
					data, _ := json.Marshal(stats)
					msg.Data = data
					msg.Timestamp = time.Now().Unix()
				}
				select {
				case writeCh <- msg:
				default:
					nlog.Core().Warn("ws write channel full, skipping status push (network slow?)")
				}
			}

		case msg := <-writeCh:
			// Perform the actual network write asynchronously in this loop.
			nlog.Core().Debug("ws send", "event", msg.Event, "data", string(msg.Data))
			conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := conn.WriteJSON(msg); err != nil {
				return fmt.Errorf("write: %w", err)
			}
		}
	}
}

func (w *WSClient) handleMessage(msg wsMessage) {
	switch msg.Event {
	case "ping":
		// Server ping — handled by the pong timer reset above
		nlog.Core().Debug("ws received ping")

	case "auth.success":
		nlog.Core().Debug("ws auth confirmed")

	case WSEventSyncConfig:
		w.handleDataEvent(msg)

	case WSEventSyncUsers:
		w.handleDataEvent(msg)

	case WSEventSyncUserDelta:
		w.handleDataEvent(msg)

	case WSEventSyncDevices:
		w.handleDataEvent(msg)

	case WSEventSyncNodes:
		w.handleDataEvent(msg)

	default:
		nlog.Core().Debug("ws unknown event", "event", msg.Event)
	}
}

func (w *WSClient) handleDataEvent(msg wsMessage) {
	var event WSEvent
	event.Type = msg.Event

	// Helper to unmarshal and decode with weak Typing
	decodeData := func(data []byte, target interface{}) error {
		var raw map[string]interface{}
		if err := json.Unmarshal(data, &raw); err != nil {
			return err
		}
		return decodeWeakRaw(raw, target)
	}

	switch msg.Event {
	case WSEventSyncConfig:
		nlog.Core().Debug("ws sync config event received")
		var p syncConfigPayload
		if err := decodeData(msg.Data, &p); err != nil {
			nlog.Core().Warn("ws: cannot decode config payload", "error", err)
			return
		}
		if p.Config.Protocol == "" {
			nlog.Core().Warn("ws: config payload missing protocol")
			return
		}
		event.Config = &p.Config
		if p.NodeID > 0 {
			event.NodeID = p.NodeID
		} else if p.Config.NodeID > 0 {
			event.NodeID = p.Config.NodeID
		}

	case WSEventSyncUsers:
		nlog.Core().Debug("ws sync users event received")
		var p syncUsersPayload
		if err := decodeData(msg.Data, &p); err != nil {
			nlog.Core().Warn("ws: cannot decode users payload", "error", err)
			return
		}
		// An empty user list is a valid full sync, not an error: it means
		// the panel currently has no available users for this node (e.g.
		// every user ran out of quota). Dropping it here used to leave
		// the last user of a node connected forever. Forward it so the
		// service replaces the user set with the empty set and closes
		// those users' connections.
		if p.Users == nil {
			p.Users = []User{}
		}
		event.Users = p.Users
		event.NodeID = p.NodeID

	case WSEventSyncUserDelta:
		nlog.Core().Debug("ws sync user delta event received")
		var p syncUserDeltaPayload
		if err := decodeData(msg.Data, &p); err != nil {
			nlog.Core().Warn("ws: cannot decode user delta payload", "error", err)
			return
		}
		if p.Action == "" {
			nlog.Core().Warn("ws: user delta payload missing action")
			return
		}
		if len(p.Users) == 0 {
			nlog.Core().Warn("ws: user delta payload has no users")
			return
		}
		event.DeltaAction = p.Action
		event.DeltaUsers = p.Users
		event.NodeID = p.NodeID

	case WSEventSyncDevices:
		nlog.Core().Debug("ws sync devices event received")
		var p syncDevicesPayload
		if err := decodeData(msg.Data, &p); err != nil {
			nlog.Core().Warn("ws: cannot decode devices payload", "error", err)
			return
		}
		event.DeviceUsers = p.Users
		event.NodeID = p.NodeID

	case WSEventSyncNodes:
		nlog.Core().Info("ws sync nodes event received (machine node list changed)")
		var p syncNodesPayload
		if err := decodeData(msg.Data, &p); err != nil {
			nlog.Core().Warn("ws: cannot decode nodes payload", "error", err)
			return
		}
		event.Nodes = p.Nodes
	}

	w.onEvent(event)
}

// SendDeviceReport sends local device snapshot to panel via WS.
// It reports failure instead of silently dropping: a non-nil error
// means the snapshot was NOT queued (not connected, no write
// channel, channel full, or marshal failure) so the caller — the
// service layer's device-report recovery — can retry it later.
// A nil error means only that the message was queued for
// connect()'s loop, not that the panel received it.
func (w *WSClient) SendDeviceReport(devices map[int][]string) error {
	return w.SendDeviceReportForNode(0, devices)
}

// SendDeviceReportForNode sends a device report tagged with a specific node_id.
// nodeID == 0 omits the field (legacy single-node mode).
func (w *WSClient) SendDeviceReportForNode(nodeID int, devices map[int][]string) error {
	var payload interface{}
	if nodeID > 0 {
		payload = map[string]interface{}{"node_id": nodeID, "devices": devices}
	} else {
		payload = devices
	}

	d, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("ws: marshal device report: %w", err)
	}
	msg := wsMessage{
		Event:     WSEventReportDevices,
		Data:      d,
		Timestamp: time.Now().Unix(),
	}

	if err := w.trySend(msg); err != nil {
		// Keep the old log behaviour: only a full channel is
		// worth a warning; not-connected / no-channel is the
		// normal disconnected state and the returned error is
		// what the caller acts on.
		if errors.Is(err, errWSWriteChannelFull) {
			nlog.Core().Warn("ws write channel full, skipping device report")
		}
		return err
	}
	return nil
}

// SendNodeStatus sends a node.status event with the given node_id (machine mode).
// Signature intentionally unchanged; failures are only logged, as before,
// but the send itself now goes through the mutex-protected trySend.
func (w *WSClient) SendNodeStatus(nodeID int, stats map[string]interface{}) {
	if stats == nil {
		return
	}
	if nodeID > 0 {
		stats["node_id"] = nodeID
	}
	data, _ := json.Marshal(stats)
	msg := wsMessage{
		Event:     "node.status",
		Data:      data,
		Timestamp: time.Now().Unix(),
	}
	if err := w.trySend(msg); err != nil && errors.Is(err, errWSWriteChannelFull) {
		nlog.Core().Warn("ws write channel full, skipping node status")
	}
}

// SendRaw sends a raw message to the panel via WebSocket.
// Signature intentionally unchanged; failures are only logged, as before,
// but the send itself now goes through the mutex-protected trySend.
func (w *WSClient) SendRaw(event string, data json.RawMessage) {
	msg := wsMessage{
		Event:     event,
		Data:      data,
		Timestamp: time.Now().Unix(),
	}
	if err := w.trySend(msg); err != nil && errors.Is(err, errWSWriteChannelFull) {
		nlog.Core().Warn("ws write channel full, skipping raw message", "event", event)
	}
}
