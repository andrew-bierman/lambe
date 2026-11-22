package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/andrew-bierman/lambe/internal/config"
	"github.com/andrew-bierman/lambe/internal/proc"
	"github.com/andrew-bierman/lambe/internal/symphony"
	"github.com/andrew-bierman/lambe/internal/tracker"
)

// orderTracker records heartbeat order and concurrency. With hang set every
// beat blocks until its context ends, like a bd call stuck on the hub.
type orderTracker struct {
	*fakeTracker
	hang bool

	mu       sync.Mutex
	order    []string
	inflight int
	peak     int
}

func (o *orderTracker) Heartbeat(ctx context.Context, id string) error {
	o.mu.Lock()
	o.order = append(o.order, id)
	o.inflight++
	o.peak = max(o.peak, o.inflight)
	o.mu.Unlock()
	defer func() { o.mu.Lock(); o.inflight--; o.mu.Unlock() }()
	if o.hang {
		<-ctx.Done()
		return ctx.Err()
	}
	time.Sleep(10 * time.Millisecond)
	return nil
}

type countingReads struct {
	*orderTracker
	reads atomic.Int64
}

func (c *countingReads) Dispatchable(ctx context.Context, q tracker.DispatchQuery) ([]tracker.Issue, error) {
	c.reads.Add(1)
	return c.orderTracker.Dispatchable(ctx, q)
}

func orphanHub(live, parked, lapsed int, hang bool) (*orderTracker, *symphony.OrphanRegistry) {
	tr := &orderTracker{fakeTracker: newFakeTracker(), hang: hang}
	reg := &symphony.OrphanRegistry{}
	add := func(prefix string, n int, orphan, lapse bool) {
		for i := range n {
			id := fmt.Sprintf("%s-%02d", prefix, i)
			tr.issues[id] = tracker.Issue{ID: id, Status: tracker.StatusInProgress, Assignee: "lambe@test"}
			if orphan {
				reg.Set(symphony.OrphanClaim{Issue: id, Since: time.Now()})
			}
			if lapse {
				reg.Lapse(id)
			}
		}
	}
	// Names sort live after orphans, so ordering is the loop's doing.
	add("z-live", live, false, false)
	add("a-parked", parked, true, false)
	add("a-lapsed", lapsed, true, true)
	return tr, reg
}

func TestBeatRoundOrphanPolicy(t *testing.T) {
	tr, reg := orphanHub(4, 40, 6, false)
	slow := beatRound(t.Context(), tr, "lambe@test", nil, reg, slog.New(slog.DiscardHandler), 30*time.Second)
	if slow {
		t.Fatal("a healthy round reported slow")
	}
	if tr.peak > heartbeatConcurrency {
		t.Errorf("peak concurrent beats = %d, want at most %d", tr.peak, heartbeatConcurrency)
	}
	if len(tr.order) != 44 {
		t.Fatalf("beat %d claims, want 4 live + 40 parked", len(tr.order))
	}
	for _, id := range tr.order {
		if strings.HasPrefix(id, "a-lapsed") {
			t.Fatalf("lapsed orphan %s was heartbeated", id)
		}
	}
	for _, id := range tr.order[:4] {
		if !strings.HasPrefix(id, "z-live") {
			t.Fatalf("orphan beaten before live work: %v", tr.order[:8])
		}
	}
}

func TestBeatRoundBudgetStopsWhenHubIsSlow(t *testing.T) {
	tr, reg := orphanHub(2, 48, 0, true)
	start := time.Now()
	slow := beatRound(t.Context(), tr, "lambe@test", nil, reg, slog.New(slog.DiscardHandler), 100*time.Millisecond)
	if !slow {
		t.Fatal("a round that spent its budget must report slow")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("round took %s past its bound", time.Since(start))
	}
	// Only the in-flight window starts; the other claims wait for next round.
	if len(tr.order) > heartbeatConcurrency+2 {
		t.Fatalf("started %d beats against a hung hub, want about %d", len(tr.order), heartbeatConcurrency)
	}
}

func TestRunHeartbeatsBacksOffWhenSlow(t *testing.T) {
	tr, reg := orphanHub(1, 5, 0, true)
	counting := &countingReads{orderTracker: tr}
	ctx, cancel := context.WithTimeout(t.Context(), 700*time.Millisecond)
	defer cancel()
	RunHeartbeats(ctx, HeartbeatConfig{
		Tracker: counting, Actor: "lambe@test", Orphans: reg,
		Interval:   func() time.Duration { return 5 * time.Millisecond },
		RoundBound: 20 * time.Millisecond,
	})
	rounds := counting.reads.Load()
	// 20ms rounds at a 5ms interval would run ~25 times without backoff.
	if rounds < 2 || rounds > 14 {
		t.Fatalf("ran %d rounds in 700ms, want backoff to hold it between 2 and 14", rounds)
	}
}

func writeOrphanCheckpoint(t *testing.T, dir, id, workspace string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"version":1,"phase":"legacy-blocked","issue":{"ID":%q,"Assignee":"lambe@test"},"workflow":{"MaxTurns":1},"key":%q,"workspace":%q,"blocked_since":"2026-11-01T00:00:00Z"}`, id, id, workspace)
	if err := os.WriteFile(filepath.Join(dir, symphony.SanitizeKey(id)+".json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestReleaseOrphanClaim(t *testing.T) {
	child := exec.Command("sleep", "30")
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = syscall.Kill(-child.Process.Pid, syscall.SIGKILL); _ = child.Wait() }()
	identity, err := proc.ProcessIdentity(child.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name      string
		record    bool
		workspace bool
		attempts  []tracker.Attempt
		force     bool
		wantErr   string
		released  bool
	}{
		{name: "unrecorded claim is refused", wantErr: "not a recorded orphaned claim"},
		{name: "unrecorded claim with force", force: true, released: true},
		{name: "recorded orphan is released", record: true, released: true},
		{name: "workspace on disk is refused", record: true, workspace: true, wantErr: "still has a workspace"},
		{name: "workspace on disk with force", record: true, workspace: true, force: true, released: true},
		{name: "live agent is refused", record: true, attempts: []tracker.Attempt{{Number: 1, PGID: child.Process.Pid, ProcessIdentity: identity}}, wantErr: "stop that agent first"},
		{name: "live agent with force", record: true, force: true, attempts: []tracker.Attempt{{Number: 1, PGID: child.Process.Pid, ProcessIdentity: identity}}, released: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			cfg := &config.Config{Trackers: []config.Tracker{{Name: "t", Path: root}}}
			state := filepath.Join(root, "state")
			dir := CheckpointDir(state, cfg)
			ws := filepath.Join(root, "ws")
			if tt.workspace {
				_ = os.MkdirAll(ws, 0o700)
			}
			tr := newFakeTracker()
			tr.issues["old-1"] = tracker.Issue{ID: "old-1", Status: tracker.StatusInProgress, Assignee: "lambe@test"}
			tr.attempts["old-1"] = tt.attempts
			if tt.record {
				writeOrphanCheckpoint(t, dir, "old-1", ws)
			}
			err := ReleaseOrphanClaim(t.Context(), tr, state, cfg, "old-1", tt.force)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			got := tr.issues["old-1"]
			if released := got.Assignee == ""; released != tt.released {
				t.Fatalf("released=%v, want %v (%+v)", released, tt.released, got)
			}
			if tt.released && tt.record {
				if left, _ := symphony.ScanOrphanClaims(dir); len(left) != 0 {
					t.Fatalf("checkpoint survived release: %+v", left)
				}
			}
		})
	}
}

func TestListOrphanClaimsOnlyShowsHeldClaims(t *testing.T) {
	root := t.TempDir()
	cfg := &config.Config{Trackers: []config.Tracker{{Name: "t", Path: root}}}
	state := filepath.Join(root, "state")
	tr := newFakeTracker()
	tr.issues["held"] = tracker.Issue{ID: "held", Status: tracker.StatusInProgress, Assignee: "lambe@test"}
	tr.issues["gone"] = tracker.Issue{ID: "gone", Status: tracker.StatusClosed}
	for _, id := range []string{"held", "gone"} {
		writeOrphanCheckpoint(t, CheckpointDir(state, cfg), id, filepath.Join(root, "absent"))
	}
	claims, err := ListOrphanClaims(t.Context(), tr, state, cfg)
	if err != nil || len(claims) != 1 || claims[0].Issue != "held" || claims[0].WorkspacePresent {
		t.Fatalf("claims = %+v, err %v", claims, err)
	}
}
