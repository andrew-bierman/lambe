package symphony

import (
	"context"
	"errors"
	"sync/atomic"

	"github.com/andrew-bierman/lambe/internal/logging"
	"github.com/andrew-bierman/lambe/internal/tracker"
)

type workerStop int

const (
	workerRunning workerStop = iota
	workerTerminal
	workerClaimLost
)

// A worker owns its session copy until done closes. Only the coordinator
// changes active, sessions, the retry queue, or stop; it reads results after done.
type worker struct {
	loop        *Loop
	session     *session
	cancel      context.CancelFunc
	done        chan struct{}
	retry       *Retry
	finished    bool
	stop        workerStop
	interrupted bool
	settling    atomic.Bool
}

func (l *Loop) capacity() int { return l.Loader.Current().MaxConcurrentAgents }

func (l *Loop) start(ctx context.Context, s *session, kind RetryKind) {
	ctx, cancel := context.WithCancel(ctx)
	copySession := *s
	w := &worker{loop: l, session: &copySession, cancel: cancel, done: make(chan struct{})}
	l.active[s.issue.ID] = w
	go func() {
		defer close(w.done)
		if kind == RetryGate {
			w.evaluateGate(ctx, w.session, w.session.gateOutput, logging.WithIssue(l.Log, s.issue.ID))
		} else {
			w.runAttempt(ctx, w.session, kind == RetryFailure)
		}
		if ctx.Err() != nil {
			l.closeRunnerSession(ctx, s.issue.ID)
		}
	}()
}

func (w *worker) terminal(ctx context.Context, s *session, reason string) {
	w.loop.cleanup(ctx, s, reason)
	w.loop.removeCheckpoint(s.issue.ID)
	w.finished = true
}

func (l *Loop) collect(ctx context.Context) {
	for id, w := range l.active {
		select {
		case <-w.done:
			w.cancel()
			delete(l.active, id)
			if w.stop != workerRunning {
				if w.stop == workerTerminal && !w.finished {
					l.cleanup(ctx, w.session, "issue left active state")
					l.removeCheckpoint(id)
				}
				if w.stop == workerClaimLost {
					l.removeCheckpoint(id)
				}
				delete(l.sessions, id)
			} else if w.finished {
				delete(l.sessions, id)
			} else {
				l.sessions[id] = w.session
				if w.retry != nil {
					l.retries.Add(*w.retry)
				}
			}
		default:
		}
	}
}

func (l *Loop) reconcileWorkers(ctx context.Context) {
	ids := make([]string, 0, len(l.active))
	for id, w := range l.active {
		if w.stop == workerRunning {
			ids = append(ids, id)
		}
	}
	stopped, err := Reconcile(ctx, l.Tracker, ids)
	if err != nil {
		l.Log.Warn("active reconciliation unavailable; keeping workers", "error", err)
		return
	}
	for _, id := range stopped {
		w := l.active[id]
		w.stop = workerTerminal
		if !w.settling.Load() {
			w.cancel()
		}
	}
	for _, id := range ids {
		w := l.active[id]
		if w.stop != workerRunning {
			continue
		}
		if err := l.Tracker.Heartbeat(ctx, id); errors.Is(err, tracker.ErrNotOwner) {
			// A gate can close its own issue after reconciliation but before heartbeat.
			if w.settling.Load() {
				issues, readErr := l.Tracker.Dispatchable(ctx, tracker.DispatchQuery{IDs: []string{id}})
				if readErr != nil {
					l.Log.Warn("settling ownership recheck unavailable; keeping gate", "issue", id, "error", readErr)
					continue
				}
				if len(issues) == 1 && issues[0].Status == tracker.StatusClosed {
					w.stop = workerTerminal
					continue
				}
			}
			w.stop = workerClaimLost
			w.cancel()
			l.Log.Info("active claim lost; stopping worker", "issue", id)
		} else if err != nil {
			l.Log.Warn("active ownership check unavailable; keeping worker", "issue", id, "error", err)
		}
	}
}

// Wait joins workers already launched by Tick and collects their results.
// Like Tick, it must not run concurrently with another coordinator operation.
// It does not dispatch retries or new work.
func (l *Loop) Wait(ctx context.Context) error {
	for _, w := range l.active {
		select {
		case <-w.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	l.collect(ctx)
	return nil
}

// Close cancels and joins all active workers and closes retained runner sessions.
// It leaves tracker claims and workspaces intact for restart recovery.
func (l *Loop) Close(ctx context.Context) error {
	for _, w := range l.active {
		w.cancel()
	}
	if err := l.Wait(ctx); err != nil {
		return err
	}
	for id := range l.sessions {
		l.closeRunnerSession(ctx, id)
	}
	return nil
}
