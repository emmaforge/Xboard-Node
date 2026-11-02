// Package machine implements the machine-mode orchestrator that dynamically
// discovers nodes from the panel's machine API and manages their lifecycles.
package machine

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/controlplane"
	"github.com/cedar2025/xboard-node/internal/model"
	"github.com/cedar2025/xboard-node/internal/monitor"
	"github.com/cedar2025/xboard-node/internal/nlog"
	"github.com/cedar2025/xboard-node/internal/panel"
	"github.com/cedar2025/xboard-node/internal/service"
)

// nodeHandle tracks a running node service.
type nodeHandle struct {
	cancel  context.CancelFunc
	done    chan struct{}
	mailbox *controlplane.NodeMailbox
}

// Orchestrator manages all nodes bound to a panel machine. It:
//   - discovers nodes via GET /machine/nodes
//   - starts / stops Service instances as nodes are added / removed
//   - maintains a shared WS connection that demuxes events by node_id
//   - reports machine-level load via POST /machine/status
type Orchestrator struct {
	cfg    *config.Config
	client *panel.Client // machine-level client (no node_id)

	mu    sync.Mutex
	nodes map[int]*nodeHandle // node_id → handle

	// Per-node mailbox keyed by node_id. Shared WS events are aggregated here
	// and each node service drains the latest state when ready.
	eventsMu  sync.RWMutex
	mailboxes map[int]*controlplane.NodeMailbox
	statuses  map[int]chan<- controlplane.StatusChange

	// Shared WS client (nil when WS is disabled or has never started).
	// ws, wsCancel and wsDisabled are guarded by wsMu: startNode reads
	// ws while tryStartWS (Run loop / WS-retry ticker) writes it, and
	// stopAll reads wsCancel while tryStartWS writes it. Reading them
	// without the mutex was a data race.
	wsMu       sync.RWMutex
	ws         *panel.WSClient
	wsCancel   context.CancelFunc
	wsDisabled bool

	// runCtx is stored from Run() so that onWSEvent can trigger rediscover
	// for sync.nodes events without blocking the main loop.
	runCtx context.Context

	pullInterval time.Duration
	pushInterval time.Duration
}

// wsClient returns the shared WS client under wsMu (may be nil).
func (o *Orchestrator) wsClient() *panel.WSClient {
	o.wsMu.RLock()
	defer o.wsMu.RUnlock()
	return o.ws
}

// wsDisabledFlag reports whether the panel disabled WS, under wsMu.
func (o *Orchestrator) wsDisabledFlag() bool {
	o.wsMu.RLock()
	defer o.wsMu.RUnlock()
	return o.wsDisabled
}

// setWSClient stores the shared WS client and its cancel func under wsMu.
func (o *Orchestrator) setWSClient(ws *panel.WSClient, cancel context.CancelFunc) {
	o.wsMu.Lock()
	o.ws = ws
	o.wsCancel = cancel
	o.wsMu.Unlock()
}

// setWSDisabled marks WS as panel-disabled under wsMu.
func (o *Orchestrator) setWSDisabled() {
	o.wsMu.Lock()
	o.wsDisabled = true
	o.wsMu.Unlock()
}

// wsCancelFunc returns the shared WS cancel func under wsMu (may be nil).
func (o *Orchestrator) wsCancelFunc() context.CancelFunc {
	o.wsMu.RLock()
	defer o.wsMu.RUnlock()
	return o.wsCancel
}

// nodeRestartBackoff is the delay before an unexpectedly-exited node is
// restarted (see startNode). It keeps a crash-looping node from being
// relaunched in a tight loop; the rediscover ticker can still relaunch
// it sooner, in which case the backoff restart is a no-op.
const nodeRestartBackoff = 5 * time.Second

// New creates a machine orchestrator from the given config.
func New(cfg *config.Config) *Orchestrator {
	panelCfg := config.PanelConfig{
		URL:       cfg.Panel.URL,
		Token:     cfg.Machine.Token,
		MachineID: cfg.Machine.MachineID,
	}
	return &Orchestrator{
		cfg:       cfg,
		client:    panel.NewClient(panelCfg),
		nodes:     make(map[int]*nodeHandle),
		mailboxes: make(map[int]*controlplane.NodeMailbox),
		statuses:  make(map[int]chan<- controlplane.StatusChange),
	}
}

// Run is the main loop. It blocks until ctx is cancelled.
func (o *Orchestrator) Run(ctx context.Context) error {
	o.runCtx = ctx
	nodesResp, err := o.client.GetMachineNodes()
	if err != nil {
		return fmt.Errorf("initial node discovery: %w", err)
	}

	o.applyIntervals(nodesResp.BaseConfig)
	nlog.Core().Info(fmt.Sprintf("machine %d: discovered %d nodes",
		o.cfg.Machine.MachineID, len(nodesResp.Nodes)))

	// Start machine-level WS as early as possible so sync.nodes can reach an
	// empty machine before the first node is attached.
	o.tryStartWS(ctx)

	// Start initial nodes.
	for _, n := range nodesResp.Nodes {
		o.startNode(ctx, n)
	}

	discoveryTicker := time.NewTicker(o.pullInterval)
	statusTicker := time.NewTicker(o.pushInterval)
	defer discoveryTicker.Stop()
	defer statusTicker.Stop()

	// B12: if the initial WS handshake failed, retry it on this ticker
	// instead of staying REST-only forever. WSClient.Run reconnects on
	// its own once started, so this only covers the never-started case.
	wsRetryTicker := time.NewTicker(wsRetryInterval)
	defer wsRetryTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			o.stopAll()
			return nil

		case <-discoveryTicker.C:
			o.rediscover(ctx)

		case <-statusTicker.C:
			o.reportMachineStatus()

		case <-wsRetryTicker.C:
			if o.wsClient() == nil && !o.wsDisabledFlag() {
				o.tryStartWS(ctx)
				if o.wsClient() != nil || o.wsDisabledFlag() {
					wsRetryTicker.Stop()
				}
			}
		}
	}
}

// ─── Node lifecycle ──────────────────────────────────────────────────────

func (o *Orchestrator) startNode(ctx context.Context, mn panel.MachineNode) {
	o.mu.Lock()
	if _, exists := o.nodes[mn.ID]; exists {
		o.mu.Unlock()
		return
	}

	nodeCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	mb := controlplane.NewNodeMailbox()
	handle := &nodeHandle{cancel: cancel, done: done, mailbox: mb}
	o.nodes[mn.ID] = handle
	o.mu.Unlock()

	o.eventsMu.Lock()
	o.mailboxes[mn.ID] = mb
	o.eventsMu.Unlock()

	nodeCfg := o.cfg.ExpandMachineNode(mn.ID, mn.Type)

	perNodeClient := o.client.ForNode(mn.ID)

	// Pre-fetch node config to detect transport-based kernel requirements.
	// If the transport (e.g. xhttp) is incompatible with the configured kernel
	// (e.g. singbox), auto-switch to the required kernel for this node.
	// A prefetch failure used to be swallowed silently (empty err branch),
	// so a node could start on the wrong kernel with no trace. Now it is
	// logged, and the kernel choice is re-checked by the service's
	// Initial() fetch (the ETag reset below forces a full response there),
	// so a transient prefetch failure only loses the early auto-switch.
	if cfgSnapshot, err := perNodeClient.GetConfig(); err != nil {
		nlog.Core().Warn("machine: prefetch node config failed, kernel auto-switch skipped; service Initial will retry",
			"node_id", mn.ID, "error", err)
	} else if cfgSnapshot == nil {
		nlog.Core().Warn("machine: prefetch node config returned empty snapshot, kernel auto-switch skipped",
			"node_id", mn.ID)
	} else if resolved := model.ResolveKernelForTransport(cfgSnapshot.Network, nodeCfg.Kernel.Type); resolved != nodeCfg.Kernel.Type {
		nlog.Core().Info(fmt.Sprintf("machine: auto-switching kernel for node %d (%s→%s, transport=%s)",
			mn.ID, nodeCfg.Kernel.Type, resolved, cfgSnapshot.Network))
		nodeCfg.Kernel.Type = resolved
	}
	// Reset cached ETag so the subsequent GetConfig in Initial() gets a full response.
	perNodeClient.ResetConfigETag()

	var push controlplane.PushClient
	if ws := o.wsClient(); ws != nil {
		push = &machineNodePush{
			nodeID: mn.ID,
			ws:     ws,
		}
	}

	// The registerFn is called by MachinePanelControlPlane.Initial() to expose
	// the node mailbox + status channel to the Service.
	nodeID := mn.ID
	registerFn := func(st chan<- controlplane.StatusChange) *controlplane.NodeMailbox {
		o.registerNode(nodeID, st)
		return mb
	}

	cp := controlplane.NewMachinePanelControlPlane(perNodeClient, nodeCfg.Kernel, push, registerFn)
	svc := service.NewWithControlPlane(nodeCfg, cp)

	nlog.Core().Info(fmt.Sprintf("machine: starting node %d (%s/%s)",
		mn.ID, mn.Type, mn.Name))

	go func() {
		defer close(done)
		defer o.unregisterNode(mn.ID, mb)
		// Detach this handle on exit and, for an unexpected exit only,
		// schedule a backoff restart. Previously the exit path only
		// unregistered the mailbox and left the o.nodes handle in
		// place, so startNode/rediscover saw the dead node as still
		// running and it was never relaunched. This defer runs before
		// close(done) (LIFO), so stopNode's <-done wait is unaffected.
		// No restart when stopNode already detached the handle (a
		// deliberate stop) or when the parent context is done (process
		// shutdown / reload), which keeps stop semantics intact.
		defer func() {
			if !o.removeNodeHandle(mn.ID, handle) {
				return
			}
			if ctx.Err() != nil {
				return
			}
			nlog.Core().Warn("machine: node exited unexpectedly, scheduling restart",
				"node_id", mn.ID, "backoff", nodeRestartBackoff)
			go func() {
				timer := time.NewTimer(nodeRestartBackoff)
				defer timer.Stop()
				select {
				case <-ctx.Done():
					return
				case <-timer.C:
					o.startNode(ctx, mn)
				}
			}()
		}()
		if err := svc.Run(nodeCtx); err != nil {
			nlog.Core().Error("machine node exited with error",
				"node_id", mn.ID, "error", err)
		}
	}()
}

// removeNodeHandle deletes the o.nodes entry for nodeID, but only if it
// is still this handle (pointer comparison). A stopNode-detached entry
// or a replacement node's handle must never be removed by a stale exit.
func (o *Orchestrator) removeNodeHandle(nodeID int, handle *nodeHandle) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if cur, ok := o.nodes[nodeID]; ok && cur == handle {
		delete(o.nodes, nodeID)
		return true
	}
	return false
}

func (o *Orchestrator) stopNode(nodeID int) {
	o.mu.Lock()
	h, ok := o.nodes[nodeID]
	if !ok {
		o.mu.Unlock()
		return
	}
	delete(o.nodes, nodeID)
	o.mu.Unlock()

	o.eventsMu.Lock()
	delete(o.mailboxes, nodeID)
	// Statuses are deleted here too, synchronously, because the
	// exiting goroutine must not delete them (see unregisterNode:
	// it cannot prove ownership of the channel entry).
	delete(o.statuses, nodeID)
	o.eventsMu.Unlock()

	nlog.Core().Info(fmt.Sprintf("machine: stopping node %d", nodeID))
	h.cancel()
	<-h.done
}

func (o *Orchestrator) stopAll() {
	o.mu.Lock()
	handles := make(map[int]*nodeHandle, len(o.nodes))
	for id, h := range o.nodes {
		handles[id] = h
	}
	o.mu.Unlock()

	for id, h := range handles {
		nlog.Core().Info(fmt.Sprintf("machine: stopping node %d", id))
		h.cancel()
	}
	for _, h := range handles {
		<-h.done
	}

	if wsCancel := o.wsCancelFunc(); wsCancel != nil {
		wsCancel()
	}
}

// ─── Node discovery ──────────────────────────────────────────────────────

func (o *Orchestrator) rediscover(ctx context.Context) {
	nodesResp, err := o.client.GetMachineNodes()
	if err != nil {
		nlog.Core().Warn("machine node discovery failed", "error", err)
		return
	}

	wanted := make(map[int]panel.MachineNode, len(nodesResp.Nodes))
	for _, n := range nodesResp.Nodes {
		wanted[n.ID] = n
	}

	o.mu.Lock()
	var toRemove []int
	for id := range o.nodes {
		if _, ok := wanted[id]; !ok {
			toRemove = append(toRemove, id)
		}
	}
	o.mu.Unlock()

	for _, id := range toRemove {
		o.stopNode(id)
	}

	for _, n := range nodesResp.Nodes {
		o.startNode(ctx, n) // no-op if already running
	}
}

// ─── Machine status reporting ────────────────────────────────────────────

func (o *Orchestrator) reportMachineStatus() {
	s := monitor.Collect()
	if err := o.client.ReportMachineStatus(
		s.CPU,
		[2]uint64{s.MemTotal, s.MemUsed},
		[2]uint64{s.SwapTotal, s.SwapUsed},
		[2]uint64{s.DiskTotal, s.DiskUsed},
		s.NetInSpeed, s.NetOutSpeed,
	); err != nil {
		nlog.Core().Warn("machine status report failed", "error", err)
	}
}

// ─── WS mux ─────────────────────────────────────────────────────────────

// wsRetryInterval is how often the Run loop retries a failed initial
// WS handshake. Short enough to recover promptly, long enough not to
// hammer a panel that is briefly down.
const wsRetryInterval = 30 * time.Second

func (o *Orchestrator) tryStartWS(ctx context.Context) {
	hs, err := o.client.Handshake()
	if err != nil {
		nlog.Core().Warn("machine ws handshake failed, will retry", "error", err)
		return
	}
	if !hs.WebSocket.Enabled || hs.WebSocket.WSURL == "" {
		nlog.Core().Info("machine: ws disabled by panel, REST only")
		o.setWSDisabled()
		return
	}

	wsCfg := panel.WSClientConfig{
		StatusInterval:   time.Duration(o.cfg.WS.StatusInterval) * time.Second,
		HandshakeTimeout: time.Duration(o.cfg.WS.HandshakeTimeout) * time.Second,
		BackoffInitial:   time.Duration(o.cfg.WS.BackoffInitial) * time.Second,
		BackoffMax:       time.Duration(o.cfg.WS.BackoffMax) * time.Second,
		MachineID:        o.cfg.Machine.MachineID,
	}

	ws := panel.NewWSClient(
		hs.WebSocket.WSURL,
		o.cfg.Machine.Token,
		0, // no single node_id
		wsCfg,
		o.onWSEvent,
		o.onWSStatus,
		nil, // per-node status is sent via machineNodePush
	)

	wsCtx, wsCancel := context.WithCancel(ctx)
	o.setWSClient(ws, wsCancel)
	go ws.Run(wsCtx)

	nlog.Core().Info("machine: ws mux started")
}

// onWSEvent routes a WS event to the correct node's channel.
// sync.nodes is a machine-level event that triggers immediate rediscovery.
func (o *Orchestrator) onWSEvent(event panel.WSEvent) {
	// sync.nodes is a machine-level event, not per-node
	if event.Type == panel.WSEventSyncNodes {
		nlog.Core().Info("machine received sync.nodes, triggering immediate rediscovery")
		go o.rediscover(o.runCtx)
		return
	}

	nodeID := event.NodeID
	if nodeID == 0 {
		nlog.Core().Debug("machine ws event missing node_id, dropping", "type", event.Type)
		return
	}

	translated, err := controlplane.TranslateWSEvent(event, o.cfg.Kernel)
	if err != nil {
		nlog.Core().Warn("machine ws event translation failed",
			"type", event.Type, "node_id", nodeID, "error", err)
		return
	}

	o.eventsMu.RLock()
	mailbox, ok := o.mailboxes[nodeID]
	o.eventsMu.RUnlock()
	if !ok {
		nlog.Core().Debug("machine ws event for unknown node", "node_id", nodeID, "type", event.Type)
		return
	}
	mailbox.Apply(translated)
}

// onWSStatus broadcasts WS connectivity changes to all registered nodes.
func (o *Orchestrator) onWSStatus(status panel.WSStatusChange) {
	change := controlplane.StatusChange{Connected: status.Connected}
	o.eventsMu.RLock()
	defer o.eventsMu.RUnlock()
	for _, ch := range o.statuses {
		select {
		case ch <- change:
		default:
		}
	}
}

func (o *Orchestrator) registerNode(nodeID int, st chan<- controlplane.StatusChange) {
	o.eventsMu.Lock()
	o.statuses[nodeID] = st
	o.eventsMu.Unlock()
}

// unregisterNode removes this node's mailbox, but only when it is
// still this node's own (pointer comparison). Previously it deleted
// unconditionally, so a stale exit running after stopNode + startNode
// of a replacement node deleted the new node's mailbox.
// The status channel is deliberately NOT deleted here: the exiting
// goroutine never saw its own channel (registration happens inside
// the service), so it cannot prove ownership of the map entry —
// comparing against the current handle's channel would be exactly
// the stale-delete bug again, because a replacement node's channel
// matches its own handle. Status cleanup belongs to stopNode (which
// deletes it synchronously before cancelling) and to registration
// itself (a restarted node overwrites the entry); a leftover entry
// for a dead service is harmless because onWSStatus sends to it
// non-blockingly and it is overwritten or removed on the next
// stop/start. The mailbox, by contrast, must be removed: events
// routed to a dead mailbox would otherwise accumulate unseen.
func (o *Orchestrator) unregisterNode(nodeID int, mb *controlplane.NodeMailbox) {
	o.eventsMu.Lock()
	defer o.eventsMu.Unlock()
	if cur, ok := o.mailboxes[nodeID]; ok && cur == mb {
		delete(o.mailboxes, nodeID)
	}
}

// applyIntervals adopts the panel's base intervals, clamped UP to the
// floors, not rewritten to 60s. Threshold semantics, confirmed from the
// call sites: pullInterval drives node discovery (at most one panel poll
// per 30s) and pushInterval drives machine status reports (at most one
// per 10s) — both are rate protections, so a panel value below the floor
// means "as fast as allowed", i.e. the floor itself. The old code
// rewrote e.g. pull=20s to 60s, halving discovery freshness for no
// reason, and did so silently.
func (o *Orchestrator) applyIntervals(bc panel.MachineBaseConfig) {
	o.pullInterval = time.Duration(bc.PullInterval) * time.Second
	if o.pullInterval < 30*time.Second {
		o.pullInterval = 30 * time.Second
	}
	o.pushInterval = time.Duration(bc.PushInterval) * time.Second
	if o.pushInterval < 10*time.Second {
		o.pushInterval = 10 * time.Second
	}
}

// ─── Virtual PushClient ─────────────────────────────────────────────────

// machineNodePush implements controlplane.PushClient for a single node
// backed by the shared machine WS connection. Events are routed by the
// WS mux directly to the Service's channels; this adapter only provides
// connectivity status and send capabilities.
type machineNodePush struct {
	nodeID int
	ws     *panel.WSClient
}

func (p *machineNodePush) Run(ctx context.Context) {
	// The shared WS mux pushes events into our channels; we just wait.
	<-ctx.Done()
}

func (p *machineNodePush) IsConnected() bool {
	return p.ws != nil && p.ws.IsConnected()
}

func (p *machineNodePush) SendDeviceReport(devices map[int][]string) error {
	if p.ws == nil {
		return fmt.Errorf("machine node push client: ws not connected")
	}
	payload := map[string]interface{}{
		"node_id": p.nodeID,
	}
	// Flatten into the standard format with node_id wrapper.
	strDevices := make(map[string][]string, len(devices))
	for uid, ips := range devices {
		strDevices[fmt.Sprintf("%d", uid)] = ips
	}
	payload["devices"] = strDevices
	data, _ := json.Marshal(payload)
	p.ws.SendRaw(panel.WSEventReportDevices, data)
	return nil
}
