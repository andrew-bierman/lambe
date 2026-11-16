package proc

import (
	"sync"
	"sync/atomic"
	"time"
)

// ExpiryReason distinguishes which watchdog timer fired.
type ExpiryReason int

const (
	// ReasonSilence means the silence timeout elapsed with no valid frame.
	ReasonSilence ExpiryReason = iota
	// ReasonDeadline means the maximum run duration elapsed.
	ReasonDeadline
)

// String returns the reason's name for logs and test messages.
func (r ExpiryReason) String() string {
	switch r {
	case ReasonSilence:
		return "silence"
	case ReasonDeadline:
		return "deadline"
	default:
		return "unknown"
	}
}

// Watchdog carries the two independent timers each supervised child needs: a
// maximum-run deadline and a silence timeout that every valid frame resets.
// Whichever fires first invokes the expiry callback exactly once; the
// supervisor's callback runs the TerminateGroup ladder.
type Watchdog struct {
	silenceAfter time.Duration
	maxRun       time.Duration
	onExpire     func(ExpiryReason)
	silence      *time.Timer
	deadline     *time.Timer
	expireOnce   sync.Once
	fired        atomic.Bool
}

// NewWatchdog arms both timers. maxRun <= 0 leaves only the silence timer
// armed; silenceAfter must be positive.
func NewWatchdog(maxRun, silenceAfter time.Duration, onExpire func(ExpiryReason)) *Watchdog {
	w := &Watchdog{
		silenceAfter: silenceAfter,
		maxRun:       maxRun,
		onExpire:     onExpire,
	}
	w.silence = time.AfterFunc(silenceAfter, func() { w.expire(ReasonSilence) })
	if maxRun > 0 {
		w.deadline = time.AfterFunc(maxRun, func() { w.expire(ReasonDeadline) })
	}
	return w
}

// FrameSeen resets the silence timer after a valid frame arrived.
func (w *Watchdog) FrameSeen() {
	w.silence.Reset(w.silenceAfter)
}

// Expired reports whether either timer has already fired.
func (w *Watchdog) Expired() bool {
	return w.fired.Load()
}

// Stop disarms both timers. It is idempotent and must be called on every path
// that ends a supervised child so a late expiry cannot fire the ladder.
func (w *Watchdog) Stop() {
	w.silence.Stop()
	if w.deadline != nil {
		w.deadline.Stop()
	}
}

func (w *Watchdog) expire(reason ExpiryReason) {
	w.expireOnce.Do(func() {
		w.fired.Store(true)
		if w.onExpire != nil {
			w.onExpire(reason)
		}
	})
}
