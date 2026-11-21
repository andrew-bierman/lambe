package daemon

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

type reclaimCounter struct {
	*fakeTracker
	calls  atomic.Int32
	grace  atomic.Int64
	failed bool
}

func (r *reclaimCounter) Reclaim(_ context.Context, grace time.Duration, _ []string) error {
	r.calls.Add(1)
	r.grace.Store(int64(grace))
	if r.failed {
		return context.DeadlineExceeded
	}
	return nil
}

func TestReclaimLoopReapsWithConfiguredGraceAndSurvivesErrors(t *testing.T) {
	for _, failed := range []bool{false, true} {
		tr := &reclaimCounter{fakeTracker: &fakeTracker{}, failed: failed}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() { ReclaimLoop(ctx, tr, 5*time.Millisecond, 7*time.Minute, nil); close(done) }()
		deadline := time.After(5 * time.Second)
		for tr.calls.Load() < 2 { // a failing round must not stop the loop
			select {
			case <-deadline:
				t.Fatalf("failed=%v: only %d reclaim rounds", failed, tr.calls.Load())
			case <-time.After(5 * time.Millisecond):
			}
		}
		cancel()
		<-done
		if got := time.Duration(tr.grace.Load()); got != 7*time.Minute {
			t.Errorf("failed=%v: grace = %s, want 7m", failed, got)
		}
	}
}
