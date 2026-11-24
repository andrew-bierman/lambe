package daemon

import (
	"strings"
	"testing"

	"github.com/andrew-bierman/lambe/internal/surface/rex"
)

func TestRexEventForPhase(t *testing.T) {
	t.Parallel()
	tests := []struct {
		phase     string
		wantOK    bool
		wantState rex.RunState
		wantBlock rex.Block
	}{
		{"", false, 0, ""},
		{"running", true, rex.RunStateRunning, ""},
		{"gating", true, rex.RunStateRunning, ""},
		{"retry", true, rex.RunStateDeferred, ""},
		{"deferred", true, rex.RunStateDeferred, ""},
		{"interrupted", true, rex.RunStateBlocked, rex.BlockNeedsHuman},
		{"stopped: gate escalation", true, rex.RunStateBlocked, rex.BlockGateEscalated},
		{"stopped: gate red", true, rex.RunStateFailed, ""},
		{"stopped: gate green", true, rex.RunStateSettled, ""},
		{"stopped: done", true, rex.RunStateSettled, ""},
	}
	for _, tt := range tests {
		ev, ok := rexEventForPhase("ac-1", tt.phase)
		if ok != tt.wantOK {
			t.Errorf("%q: ok = %v, want %v", tt.phase, ok, tt.wantOK)
			continue
		}
		if !ok {
			continue
		}
		if ev.State != tt.wantState || ev.Block != tt.wantBlock || ev.RunID != "ac-1" || !strings.HasPrefix(ev.Msg, "ac-1: ") {
			t.Errorf("%q: event = %+v", tt.phase, ev)
		}
	}
}

func TestObservePhaseBadgesPerRunOnceAndClears(t *testing.T) {
	t.Parallel()
	var out syncBuffer
	adapter := rex.NewAdapter(rex.Availability{Available: true}, &fakeRexClient{}, &out)
	reg := NewRunRegistry(adapter, nil)

	reg.ObservePhase("ac-1", "running")
	reg.ObservePhase("ac-1", "running") // repeated phase: no second badge
	reg.ObservePhase("ac-2", "stopped: gate escalation")
	reg.ClearSurface()

	got := out.String()
	if n := strings.Count(got, "state=working:id=lambe/ac-1:"); n != 1 {
		t.Errorf("ac-1 working badges = %d, want 1; output %q", n, got)
	}
	if !strings.Contains(got, "state=blocked:kind=permission:id=lambe/ac-2:") {
		t.Errorf("no blocked badge for ac-2; output %q", got)
	}
	if !strings.HasSuffix(got, "state=clear:id=lambe\x1b\\") {
		t.Errorf("clear is not the last sequence; output %q", got)
	}
}
