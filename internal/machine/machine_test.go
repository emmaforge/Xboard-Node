package machine

import (
	"context"
	"testing"
	"time"

	"github.com/cedar2025/xboard-node/internal/controlplane"
	"github.com/cedar2025/xboard-node/internal/panel"
)

func TestApplyIntervalsClampsToFloorNot60s(t *testing.T) {
	// Regression test for B12: intervals below the floor used to be
	// silently rewritten to 60s. The floors are rate protections
	// (discovery at most every 30s, status at most every 10s), so a
	// below-floor panel value must clamp to the floor itself.
	o := &Orchestrator{}
	o.applyIntervals(panel.MachineBaseConfig{PullInterval: 20, PushInterval: 5})
	if o.pullInterval != 30*time.Second {
		t.Errorf("pullInterval = %v, want 30s", o.pullInterval)
	}
	if o.pushInterval != 10*time.Second {
		t.Errorf("pushInterval = %v, want 10s", o.pushInterval)
	}
	// Values at or above the floor pass through unchanged.
	o.applyIntervals(panel.MachineBaseConfig{PullInterval: 45, PushInterval: 30})
	if o.pullInterval != 45*time.Second {
		t.Errorf("pullInterval = %v, want 45s", o.pullInterval)
	}
	if o.pushInterval != 30*time.Second {
		t.Errorf("pushInterval = %v, want 30s", o.pushInterval)
	}
	// Missing (zero) panel values fall back to the floors, not to a
	// zero ticker (which would panic) or to 60s.
	o.applyIntervals(panel.MachineBaseConfig{})
	if o.pullInterval != 30*time.Second || o.pushInterval != 10*time.Second {
		t.Errorf("zero intervals = %v/%v, want 30s/10s", o.pullInterval, o.pushInterval)
	}
}

func TestRemoveNodeHandlePointerComparison(t *testing.T) {
	o := &Orchestrator{nodes: make(map[int]*nodeHandle)}
	h1 := &nodeHandle{done: make(chan struct{})}
	h2 := &nodeHandle{done: make(chan struct{})}
	o.nodes[7] = h1
	// A stale handle must not remove the current one.
	if o.removeNodeHandle(7, h2) {
		t.Fatal("removeNodeHandle with a stale handle removed the current handle")
	}
	if o.nodes[7] != h1 {
		t.Fatal("current handle changed after stale removal attempt")
	}
	if !o.removeNodeHandle(7, h1) {
		t.Fatal("removeNodeHandle with the current handle should remove it")
	}
	if _, ok := o.nodes[7]; ok {
		t.Fatal("handle still present after removal")
	}
	if o.removeNodeHandle(7, h1) {
		t.Fatal("second removal should report nothing removed")
	}
}

func TestUnregisterNodePointerComparison(t *testing.T) {
	o := &Orchestrator{
		nodes:     make(map[int]*nodeHandle),
		mailboxes: make(map[int]*controlplane.NodeMailbox),
		statuses:  make(map[int]chan<- controlplane.StatusChange),
	}
	oldMB := controlplane.NewNodeMailbox()
	newMB := controlplane.NewNodeMailbox()
	oldSt := make(chan controlplane.StatusChange, 1)
	newSt := make(chan controlplane.StatusChange, 1)
	oldHandle := &nodeHandle{done: make(chan struct{}), mailbox: oldMB}
	newHandle := &nodeHandle{done: make(chan struct{}), mailbox: newMB}

	// Stale exit after a replacement node started: nothing belonging
	// to the new node may be deleted.
	o.nodes[7] = newHandle
	o.mailboxes[7] = newMB
	o.statuses[7] = newSt
	o.unregisterNode(7, oldMB)
	if o.mailboxes[7] != newMB {
		t.Fatal("stale unregister deleted the replacement mailbox")
	}
	if o.statuses[7] != newSt {
		t.Fatal("stale unregister deleted the replacement status channel")
	}

	// Own exit: its mailbox is removed. Its status channel is
	// deliberately left for stopNode/registration (see
	// unregisterNode): the exiting goroutine cannot prove ownership
	// of the channel entry, and deleting by current-handle comparison
	// would reintroduce the stale-delete bug this test guards.
	o.nodes[7] = oldHandle
	o.mailboxes[7] = oldMB
	o.statuses[7] = oldSt
	o.unregisterNode(7, oldMB)
	if _, ok := o.mailboxes[7]; ok {
		t.Fatal("own mailbox not removed")
	}
	if o.statuses[7] != oldSt {
		t.Fatal("unregister must not touch the status channel")
	}
}

func TestStopNodeDeletesMailboxAndStatus(t *testing.T) {
	o := &Orchestrator{
		nodes:     make(map[int]*nodeHandle),
		mailboxes: make(map[int]*controlplane.NodeMailbox),
		statuses:  make(map[int]chan<- controlplane.StatusChange),
	}
	nodeCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		<-nodeCtx.Done()
		close(done)
	}()
	mb := controlplane.NewNodeMailbox()
	st := make(chan controlplane.StatusChange, 1)
	o.nodes[7] = &nodeHandle{cancel: cancel, done: done, mailbox: mb}
	o.mailboxes[7] = mb
	o.statuses[7] = st
	o.stopNode(7)
	if _, ok := o.nodes[7]; ok {
		t.Fatal("stopNode did not remove the handle")
	}
	if _, ok := o.mailboxes[7]; ok {
		t.Fatal("stopNode did not remove the mailbox")
	}
	if _, ok := o.statuses[7]; ok {
		t.Fatal("stopNode did not remove the status channel")
	}
}

func TestWSClientAccessors(t *testing.T) {
	o := &Orchestrator{}
	if o.wsClient() != nil || o.wsDisabledFlag() {
		t.Fatal("fresh orchestrator should have no WS client and not be WS-disabled")
	}
	o.setWSDisabled()
	if !o.wsDisabledFlag() {
		t.Fatal("setWSDisabled did not stick")
	}
	if o.wsCancelFunc() != nil {
		t.Fatal("ws cancel should still be nil")
	}
}
