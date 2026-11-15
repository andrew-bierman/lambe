package daemon

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/andrew-bierman/lambe/internal/proc"
	"github.com/andrew-bierman/lambe/internal/tracker"
)

// Typed error categories for graceful degradation: when a tracker is
// unreachable the daemon keeps running its agents on last-known state and
// logs the category instead of a raw message.
const (
	// CategoryUnavailable marks transport and backend failures: retry next
	// tick, keep supervising on last-known state.
	CategoryUnavailable = "unavailable"
	// CategoryRace marks a lost guarded write; stand down, do not retry.
	CategoryRace = "lost_race"
	// CategoryLease marks a claim no longer held; stop supervising.
	CategoryLease = "lease_lost"
	// CategoryUnknown marks unclassified failures.
	CategoryUnknown = "unknown"
)

// ErrorCategory maps a tracker error to its degradation category.
func ErrorCategory(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, tracker.ErrLostRace):
		return CategoryRace
	case errors.Is(err, tracker.ErrNotOwner):
		return CategoryLease
	case errors.Is(err, tracker.ErrFrozen), errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return CategoryUnavailable
	default:
		return CategoryUnknown
	}
}

// MachineLabelPrefix is the fixed label namespace of the dispatch affinity
// rule: an issue labeled "lambe/machine:<name>" runs only on the machine
// whose configured identity is <name>. No other affinity exists.
const MachineLabelPrefix = "lambe/machine:"

// SkipsMachine reports whether the fixed label-skip rule excludes an issue
// from this machine: some label pins the issue to a different machine's
// identity. An issue with no machine labels is dispatchable everywhere.
func SkipsMachine(labels []string, machine string) bool {
	for _, label := range labels {
		name, ok := strings.CutPrefix(label, MachineLabelPrefix)
		if ok && name != machine {
			return true
		}
	}
	return false
}

// LabelSource resolves an issue's labels. It is a narrow seam over the
// tracker backend's label read, which the committed tracker interface does
// not yet surface; the assembly point wires the real source.
type LabelSource interface {
	Labels(ctx context.Context, issueID string) ([]string, error)
}

// MetadataLabelSource reads labels from an issue's metadata under the
// "labels" key (comma-separated), the representation the tracker layer
// stamps today. An issue without the key has no labels.
type MetadataLabelSource struct {
	Tracker tracker.Tracker
}

// Labels implements LabelSource.
func (m MetadataLabelSource) Labels(ctx context.Context, issueID string) ([]string, error) {
	issue, ok, err := lookupIssue(ctx, m.Tracker, issueID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	raw := issue.Metadata["labels"]
	if raw == "" {
		return nil, nil
	}
	return strings.Split(raw, ","), nil
}

// FilteredTracker wraps a tracker and enforces the fixed label-skip rule on
// every dispatch read: issues labeled for another machine are never
// dispatched here. A label read failure is logged and the issue is kept —
// an unreadable affinity never silently drops work.
type FilteredTracker struct {
	tracker.Tracker
	Machine string
	Labels  LabelSource
	Log     *slog.Logger
}

// Dispatchable implements tracker.Tracker with the machine filter applied.
func (f *FilteredTracker) Dispatchable(ctx context.Context, q tracker.DispatchQuery) ([]tracker.Issue, error) {
	issues, err := f.Tracker.Dispatchable(ctx, q)
	if err != nil {
		return nil, err
	}
	out := issues[:0]
	for _, issue := range issues {
		labels, lerr := f.Labels.Labels(ctx, issue.ID)
		if lerr != nil {
			if f.Log != nil {
				f.Log.Warn("label read failed, keeping issue dispatchable",
					"issue", issue.ID, "category", ErrorCategory(lerr), "error", lerr)
			}
			out = append(out, issue)
			continue
		}
		if SkipsMachine(labels, f.Machine) {
			if f.Log != nil {
				f.Log.Info("issue labeled for another machine, skipped", "issue", issue.ID, "machine", f.Machine)
			}
			continue
		}
		out = append(out, issue)
	}
	return out, nil
}

// Ticker is one tick of the symphony loop; *symphony.Loop satisfies it.
type Ticker interface {
	Tick(ctx context.Context) error
}

// nopTicker keeps the daemon's tick cadence when no loop is wired.
type nopTicker struct{}

// Tick implements Ticker as a no-op.
func (nopTicker) Tick(context.Context) error { return nil }

// TickOptions configures the tick loop.
type TickOptions struct {
	// Interval is the cadence between ticks.
	Interval time.Duration
	// Sync runs the hub pull/push cycle: pull before the tick, push after.
	Sync Syncer
	// Loop is the symphony tick.
	Loop Ticker
	// Flush applies the tracker's queued write batch after the tick and
	// before the push; nil skips flushing. The primary tracker qualifies
	// when it implements FlushWrites.
	Flush Flusher
	// AfterTick runs after every tick's writes are flushed (success or
	// not); the daemon refreshes its report cache here. Errors are the
	// callback's business; the loop only invokes it.
	AfterTick func(ctx context.Context)
	// Log receives tick outcomes; nil discards them.
	Log *slog.Logger
}

// Flusher applies a tracker's queued batched writes in one transaction.
// *tracker.Local satisfies it through its embedded Beads.
type Flusher interface {
	FlushWrites(ctx context.Context) error
}

// RunTicks drives the daemon's cadence until ctx is done: pull, tick, push.
// A failing sync or tick is logged with its category and never stops the
// daemon — degradation keeps agents running on last-known state.
func RunTicks(ctx context.Context, opts TickOptions) {
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	loop := opts.Loop
	if loop == nil {
		loop = nopTicker{}
	}
	ticker := time.NewTicker(opts.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := opts.Sync.Pull(ctx); err != nil {
				log.Warn("hub pull failed, continuing on local state",
					"category", ErrorCategory(err), "error", err)
			}
			if err := loop.Tick(ctx); err != nil {
				log.Warn("tick failed, continuing",
					"category", ErrorCategory(err), "error", err)
			}
			if opts.Flush != nil {
				if err := opts.Flush.FlushWrites(ctx); err != nil {
					log.Warn("write batch flush failed, writes stay queued",
						"category", ErrorCategory(err), "error", err)
				}
			}
			if err := PushAfterWrites(ctx, opts.Sync); err != nil {
				log.Warn("hub push failed, local writes stay ahead",
					"category", ErrorCategory(err), "error", err)
			}
			if opts.AfterTick != nil {
				opts.AfterTick(ctx)
			}
		}
	}
}

// HeartbeatLoop extends the lease on every in-progress claim this actor
// holds, at the configured interval, independent of the tick loop. The
// interval is re-read through the getter each beat so a config reload
// applies live. Lease losses downgrade the claim to standing-down state;
// other errors are logged and retried on the next beat.
func HeartbeatLoop(ctx context.Context, tr tracker.Tracker, actor string, interval func() time.Duration, log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}
	if interval == nil {
		d := 150 * time.Second
		interval = func() time.Duration { return d }
	}
	tick := interval()
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			next := interval()
			if next != tick {
				tick = next
				ticker.Reset(tick)
			}
			issues, err := tr.Dispatchable(ctx, tracker.DispatchQuery{Statuses: []string{"in_progress"}})
			if err != nil {
				log.Warn("heartbeat dispatch read failed",
					"category", ErrorCategory(err), "error", err)
				continue
			}
			for _, issue := range issues {
				if issue.Assignee != actor {
					continue
				}
				if err := tr.Heartbeat(ctx, issue.ID); err != nil {
					if errors.Is(err, tracker.ErrNotOwner) {
						log.Info("heartbeat lost claim", "issue", issue.ID, "category", CategoryLease)
						continue
					}
					log.Warn("heartbeat failed", "issue", issue.ID,
						"category", ErrorCategory(err), "error", err)
				}
			}
		}
	}
}

// ChildSet tracks the live supervised children so shutdown can run the
// bounded kill ladder against every process group.
type ChildSet struct {
	mu       sync.Mutex
	children map[int]*proc.Child
}

// NewChildSet returns an empty ChildSet.
func NewChildSet() *ChildSet {
	return &ChildSet{children: map[int]*proc.Child{}}
}

// Add registers a live child; re-adding the same pgid replaces it.
func (s *ChildSet) Add(c *proc.Child) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.children[c.Pgid()] = c
}

// TerminateAll runs the SIGTERM → bounded grace → SIGKILL ladder against
// every tracked process group concurrently and waits for all of them.
func (s *ChildSet) TerminateAll(ctx context.Context) error {
	s.mu.Lock()
	children := make([]*proc.Child, 0, len(s.children))
	for _, c := range s.children {
		children = append(children, c)
	}
	s.children = map[int]*proc.Child{}
	s.mu.Unlock()

	var wg sync.WaitGroup
	errs := make([]error, len(children))
	for i, c := range children {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = c.Terminate(ctx, 0)
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}
