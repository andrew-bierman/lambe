package tracker

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Lane is a bd call's priority class in the shell transport's subprocess
// queue. Lower values are served first when a slot frees up.
type Lane int

// The lanes, highest priority first. Writes (claims, attempt records,
// closes) must never wait behind the periodic traffic: a daemon that cannot
// record an attempt refuses to prompt its agent.
const (
	// LaneWrite carries state-changing calls: claim, create, update, close,
	// unclaim, defer, reclaim, batch.
	LaneWrite Lane = iota
	// LaneRead carries polls and lookups: list, show, config, events.
	LaneRead
	// LaneBackground carries lease heartbeats, which tolerate delay (the
	// lease TTL is many ticks) and are the first to give way.
	LaneBackground
	laneCount
)

// Defaults for the shell transport's bd subprocess limits.
const (
	// DefaultMaxConcurrency is the number of bd subprocesses that may run at
	// once. Embedded Dolt serializes writers behind a lock and each process
	// pays seconds of engine startup, so more than a few only queues work
	// inside bd where it cannot be prioritized.
	DefaultMaxConcurrency = 3
	// DefaultQueueWait bounds how long a call may wait for a free slot
	// before it fails. It is separate from the run timeout, which only
	// starts once the process does.
	DefaultQueueWait = 10 * time.Minute
)

// ErrQueueWait marks a call that gave up waiting for a free bd subprocess
// slot. It also wraps context.DeadlineExceeded so callers classify it as an
// unavailable tracker.
var ErrQueueWait = errors.New("bd call waited too long for a free subprocess slot")

// Limits tunes every ShellTransport in the process. Zero fields keep the
// defaults.
type Limits struct {
	// MaxConcurrent caps simultaneous bd subprocesses.
	MaxConcurrent int
	// Timeout bounds one read or heartbeat subprocess run (not its queue
	// wait). Zero means DefaultCommandTimeout.
	Timeout time.Duration
	// WriteTimeout bounds one write subprocess run. Zero means Timeout.
	WriteTimeout time.Duration
	// QueueWait bounds the wait for a slot. Zero means DefaultQueueWait.
	QueueWait time.Duration
}

var (
	limitsMu sync.RWMutex
	limits   Limits
	bdGate   = newGate(DefaultMaxConcurrency)
	bdStats  = &callStats{counts: map[string]uint64{}}
)

// SetLimits applies process-wide bd limits. The daemon calls it once at
// startup from configuration; tests may call it to isolate behavior.
func SetLimits(l Limits) {
	limitsMu.Lock()
	limits = l
	limitsMu.Unlock()
	bdGate.setMax(l.maxConcurrent())
}

func currentLimits() Limits {
	limitsMu.RLock()
	defer limitsMu.RUnlock()
	return limits
}

func (l Limits) maxConcurrent() int {
	if l.MaxConcurrent > 0 {
		return l.MaxConcurrent
	}
	return DefaultMaxConcurrency
}

func (l Limits) queueWait() time.Duration {
	if l.QueueWait > 0 {
		return l.QueueWait
	}
	return DefaultQueueWait
}

// runTimeout resolves the per-run bound for a lane.
func (l Limits) runTimeout(lane Lane) time.Duration {
	base := l.Timeout
	if base <= 0 {
		base = DefaultCommandTimeout
	}
	if lane == LaneWrite && l.WriteTimeout > 0 {
		return l.WriteTimeout
	}
	return base
}

// WriteRunTimeout is the effective bound on one bd write run, so callers
// that wrap tracker calls in their own deadline can keep it above the bd
// bound instead of cutting a healthy but slow call short.
func WriteRunTimeout() time.Duration { return currentLimits().runTimeout(LaneWrite) }

// ReadRunTimeout is the effective bound on one bd read run.
func ReadRunTimeout() time.Duration { return currentLimits().runTimeout(LaneRead) }

// callStats counts started bd subprocesses by verb, for measuring calls per
// tick.
type callStats struct {
	mu     sync.Mutex
	counts map[string]uint64
}

func (s *callStats) add(verb string) {
	s.mu.Lock()
	s.counts[verb]++
	s.mu.Unlock()
}

// CallCounts returns a snapshot of bd subprocesses started by this process,
// keyed by bd verb (list, show, create, update, heartbeat, ...).
func CallCounts() map[string]uint64 {
	bdStats.mu.Lock()
	defer bdStats.mu.Unlock()
	out := make(map[string]uint64, len(bdStats.counts))
	for k, v := range bdStats.counts {
		out[k] = v
	}
	return out
}

// gate is a counting semaphore whose waiters are served by lane priority,
// first come first served within a lane.
type gate struct {
	mu     sync.Mutex
	max    int
	active int
	queues [laneCount][]*waiter
}

type waiter struct{ granted chan struct{} }

func newGate(slots int) *gate { return &gate{max: slots} }

func (g *gate) setMax(n int) {
	g.mu.Lock()
	g.max = n
	g.grantLocked()
	g.mu.Unlock()
}

// grantLocked hands free slots to the highest-priority waiters.
func (g *gate) grantLocked() {
	for g.active < g.max {
		var next *waiter
		for lane := range g.queues {
			if q := g.queues[lane]; len(q) > 0 {
				next, g.queues[lane] = q[0], q[1:]
				break
			}
		}
		if next == nil {
			return
		}
		g.active++
		close(next.granted)
	}
}

func (g *gate) release() {
	g.mu.Lock()
	g.active--
	g.grantLocked()
	g.mu.Unlock()
}

// acquire blocks for a slot in lane until ctx ends or maxWait elapses. The
// returned func releases the slot.
func (g *gate) acquire(ctx context.Context, lane Lane, maxWait time.Duration) (func(), error) {
	w := &waiter{granted: make(chan struct{})}
	g.mu.Lock()
	g.queues[lane] = append(g.queues[lane], w)
	g.grantLocked()
	g.mu.Unlock()

	timer := time.NewTimer(maxWait)
	defer timer.Stop()
	var cause error
	select {
	case <-w.granted:
		return g.release, nil
	case <-ctx.Done():
		cause = ctx.Err()
	case <-timer.C:
		cause = fmt.Errorf("%w: %w", ErrQueueWait, context.DeadlineExceeded)
	}
	g.mu.Lock()
	q := g.queues[lane]
	for i, cand := range q {
		if cand == w {
			g.queues[lane] = append(q[:i:i], q[i+1:]...)
			g.mu.Unlock()
			return nil, cause
		}
	}
	g.mu.Unlock()
	// Granted in the instant before we could dequeue: give the slot back.
	g.release()
	return nil, cause
}
