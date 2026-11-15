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
// dispatched here. Labels are read from the "labels" metadata key already
// present on each dispatch result; the LabelSource fallback runs only for
// issues that carry no such key, and its failure is logged with the issue
// kept — an unreadable affinity never silently drops work.
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
		labels := labelsOf(issue)
		if _, stamped := issue.Metadata["labels"]; !stamped && f.Labels != nil {
			fallback, lerr := f.Labels.Labels(ctx, issue.ID)
			if lerr != nil {
				if f.Log != nil {
					f.Log.Warn("label read failed, keeping issue dispatchable",
						"issue", issue.ID, "category", ErrorCategory(lerr), "error", lerr)
				}
				out = append(out, issue)
				continue
			}
			labels = fallback
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

// labelsOf splits an issue's comma-separated "labels" metadata value; an
// absent key yields nil, an empty value yields no labels.
func labelsOf(issue tracker.Issue) []string {
	raw := issue.Metadata["labels"]
	if raw == "" {
		return nil
	}
	return strings.Split(raw, ",")
}

// Ticker is one tick of the symphony loop; *symphony.Loop satisfies it.
type Ticker interface {
	Tick(ctx context.Context) error
}

// Flusher applies a tracker's queued batched writes in one transaction.
// *tracker.Local satisfies it through its embedded Beads.
type Flusher interface {
	FlushWrites(ctx context.Context) error
}

// heartbeatConcurrency bounds how many lease beats are extended at once
// per beat round; the rest of a large claim set waits for the next beats.
const heartbeatConcurrency = 4

// WriteCounter reports how many writes are queued awaiting the next
// flush. *tracker.Local satisfies it through its embedded Beads; the tick
// cycle uses it to skip the hub push after a write-free tick.
type WriteCounter interface {
	BufferedWrites() int
}

// RunTicks drives the daemon's cadence until ctx is done: every interval it
// runs one cycle — pull, tick, flush, push, report — through the supplied
// body. A failing step is logged with its category inside the cycle and
// never stops the daemon — degradation keeps agents running on last-known
// state.
func RunTicks(ctx context.Context, interval time.Duration, cycle func(context.Context)) {
	if cycle == nil {
		cycle = func(context.Context) {}
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cycle(ctx)
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
			issues, err := tr.Dispatchable(ctx, tracker.DispatchQuery{Statuses: []string{tracker.StatusInProgress}})
			if err != nil {
				log.Warn("heartbeat dispatch read failed",
					"category", ErrorCategory(err), "error", err)
				continue
			}
			// Beats run concurrently under a bounded semaphore so a
			// large claim set does not stretch one round across many
			// serial shell-outs.
			sem := make(chan struct{}, heartbeatConcurrency)
			var wg sync.WaitGroup
			for _, issue := range issues {
				if issue.Assignee != actor {
					continue
				}
				wg.Add(1)
				sem <- struct{}{}
				go func(id string) {
					defer wg.Done()
					defer func() { <-sem }()
					if err := tr.Heartbeat(ctx, id); err != nil {
						if errors.Is(err, tracker.ErrNotOwner) {
							log.Info("heartbeat lost claim", "issue", id, "category", CategoryLease)
							return
						}
						log.Warn("heartbeat failed", "issue", id,
							"category", ErrorCategory(err), "error", err)
					}
				}(issue.ID)
			}
			wg.Wait()
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
