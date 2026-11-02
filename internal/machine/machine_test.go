package machine

import (
	"testing"
	"time"

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
