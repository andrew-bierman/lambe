package tracker

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"
)

// TestSlowHubClaimToAttempt measures the daemon's "claim -> attempt recorded
// -> agent may be prompted" path against a real (cloned) hub while
// background workers hammer it the way a busy daemon does. It is a
// measurement, not a regression gate: it only runs when LAMBE_BENCH_HUB
// names an isolated hub directory (never production) and uses only API that
// also exists in v0.10.1, so the same file can be copied into an older
// checkout for a before/after comparison.
//
//	LAMBE_BENCH_HUB=<clone> LAMBE_BENCH_BD=<bd or logging wrapper> \
//	LAMBE_BENCH_ITER=6 LAMBE_BENCH_BG=6 go test ./internal/tracker -run SlowHubClaim -v
func TestSlowHubClaimToAttempt(t *testing.T) {
	hub := os.Getenv("LAMBE_BENCH_HUB")
	if hub == "" {
		t.Skip("set LAMBE_BENCH_HUB to an isolated hub clone to run this measurement")
	}
	bin := os.Getenv("LAMBE_BENCH_BD")
	iters, bg := envInt("LAMBE_BENCH_ITER", 6), envInt("LAMBE_BENCH_BG", 6)
	if os.Getenv("LAMBE_BENCH_BG") == "0" {
		bg = 0
	}
	t.Setenv("BEADS_DIR", hub+"/.beads")
	actor := "lambe@bench"
	l, err := NewLocal(bin, hub, actor)
	if err != nil {
		t.Fatal(err)
	}

	// sequence is what the loop does for a freshly dispatched issue, plus the
	// daemon's persist seam (a separate tracker instance, as in newAttemptPersister).
	sequence := func(n int) time.Duration {
		issue, err := l.CreateTask(t.Context(), fmt.Sprintf("bench task %d %d", time.Now().UnixNano(), n), []string{"bench"})
		if err != nil {
			t.Fatalf("create task: %v", err)
		}
		start := time.Now()
		if _, err := l.Claim(t.Context(), issue.ID); err != nil {
			t.Fatalf("claim: %v", err)
		}
		if _, err := l.Attempts(t.Context(), issue.ID); err != nil { // stopRecordedAgent
			t.Fatalf("attempts: %v", err)
		}
		if err := l.PutAttempt(t.Context(), Attempt{SessionID: issue.ID, Number: 1}); err != nil {
			t.Fatalf("put attempt: %v", err)
		}
		p, err := NewLocal(bin, hub, actor)
		if err != nil {
			t.Fatal(err)
		}
		attempts, err := p.Attempts(t.Context(), issue.ID)
		if err != nil || len(attempts) != 1 {
			t.Fatalf("persist read: %v %+v", err, attempts)
		}
		if err := p.PutAttempt(t.Context(), Attempt{SessionID: issue.ID, Number: 1, PGID: 4242, ProcessIdentity: "bench"}); err != nil {
			t.Fatalf("persist pgid: %v", err)
		}
		return time.Since(start)
	}

	solo := sequence(0)
	t.Logf("solo (no background load): %s", solo.Round(10*time.Millisecond))

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := range bg {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if i%2 == 0 {
					_ = l.Heartbeat(t.Context(), "ac-92ca")
				} else {
					_, _ = l.Dispatchable(t.Context(), DispatchQuery{Statuses: []Status{StatusInProgress}})
				}
			}
		}()
	}
	time.Sleep(2 * time.Second)
	var ds []time.Duration
	for n := 1; n <= iters; n++ {
		ds = append(ds, sequence(n))
	}
	close(stop)
	wg.Wait()
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	t.Logf("under %d background workers, %d runs: median %s, max %s (all: %v)", bg, iters,
		ds[len(ds)/2].Round(10*time.Millisecond), ds[len(ds)-1].Round(10*time.Millisecond), ds)
}

func envInt(key string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(key)); err == nil && v > 0 {
		return v
	}
	return def
}
