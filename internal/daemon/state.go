package daemon

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/andrew-bierman/lambe/internal/httpstate"
)

// ReportCache holds the morning report as of the daemon's last tick, so the
// read-only HTTP state surface can serve tracker-derived sections
// (needs-human, gate rejections) without a tracker read per request. It is
// safe for concurrent use.
type ReportCache struct {
	mu  sync.RWMutex
	rep Report
	at  time.Time
}

// Store replaces the cached report and stamps the read time.
func (c *ReportCache) Store(rep Report) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rep = rep
	c.at = time.Now()
}

// Load returns the cached report and when it was taken. The zero Report is
// returned before the first tick.
func (c *ReportCache) Load() (Report, time.Time) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.rep, c.at
}

// StateSourceAdapter implements httpstate.StateSource over daemon state:
// live runs come from the run registry, tracker-derived sections from the
// report cache, and Refresh pokes the daemon for a prompt re-evaluation.
// It is safe for concurrent use.
type StateSourceAdapter struct {
	registry *RunRegistry
	reports  *ReportCache
	poke     func()
	inflight atomic.Bool
}

// Compile-time check that StateSourceAdapter satisfies the HTTP state seam.
var _ httpstate.StateSource = (*StateSourceAdapter)(nil)

// NewStateSource builds the adapter. poke, when non-nil, is the advisory
// refresh hook the daemon wires to an immediate background tick.
func NewStateSource(registry *RunRegistry, reports *ReportCache, poke func()) *StateSourceAdapter {
	return &StateSourceAdapter{registry: registry, reports: reports, poke: poke}
}

// Snapshot returns the full supervised state: live runs in registration
// order, the earliest armed wake as the supervisor wake time, and the
// cached needs-human and gate-rejection sections.
func (a *StateSourceAdapter) Snapshot() httpstate.Snapshot {
	snap := httpstate.Snapshot{}
	var wake time.Time
	for _, run := range a.registry.List() {
		snap.Runs = append(snap.Runs, httpstate.Run{
			Issue:     run.IssueID,
			Session:   run.Session,
			Attempt:   run.Attempt,
			State:     stateName(run.State),
			LastEvent: run.LastEventAt,
		})
		if run.WakeAt.IsZero() {
			continue
		}
		if wake.IsZero() || run.WakeAt.Before(wake) {
			wake = run.WakeAt
		}
	}
	snap.WakeAt = wake

	if a.reports != nil {
		rep, _ := a.reports.Load()
		for _, nh := range rep.NeedsHuman {
			snap.NeedsHuman = append(snap.NeedsHuman, httpstate.NeedsHuman{
				Issue:  nh.Issue,
				Reason: "gate_rejected",
				Detail: nh.LastGateOutput,
			})
		}
		for _, gr := range rep.GateRejections {
			snap.GateRejections = append(snap.GateRejections, httpstate.GateRejection{
				Issue:  gr.Issue,
				Gate:   "formula",
				Reason: gr.Output,
			})
		}
	}
	return snap
}

// Issue returns one issue's state from the live registry; issues only known
// to the cached report report their tracker status. The second return is
// false when the issue is unknown to both.
func (a *StateSourceAdapter) Issue(id string) (httpstate.IssueState, bool) {
	if run, ok := a.registry.Lookup(id); ok {
		return httpstate.IssueState{
			State:     stateName(run.State),
			Attempt:   run.Attempt,
			Session:   run.Session,
			LastEvent: run.LastEventAt,
			Summary:   run.Outcome,
		}, true
	}
	if a.reports != nil {
		rep, _ := a.reports.Load()
		for _, active := range rep.Active {
			if active.Issue == id {
				return httpstate.IssueState{
					State:     "running",
					Attempt:   active.Attempt,
					Session:   active.Session,
					LastEvent: time.Now().Add(-active.LastFrameAge),
					Summary:   "in progress (pre-tick view)",
				}, true
			}
		}
		for _, d := range rep.Deferred {
			if d.Issue == id {
				return httpstate.IssueState{
					State:     "deferred",
					Attempt:   0,
					LastEvent: time.Now(),
					Summary:   d.Reason,
				}, true
			}
		}
	}
	return httpstate.IssueState{}, false
}

// Refresh advises the daemon to re-evaluate promptly. It returns
// immediately; overlapping refreshes collapse into one in-flight poke.
func (a *StateSourceAdapter) Refresh() {
	if a.poke == nil || !a.inflight.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer a.inflight.Store(false)
		a.poke()
	}()
}
