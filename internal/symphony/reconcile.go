package symphony

import (
	"context"
	"fmt"

	"github.com/andrew-bierman/lambe/internal/tracker"
)

// StatusActive reports whether a tracker status keeps a run alive. Anything
// else (closed, done, removed, canceled, …) is terminal: reconciliation
// stops the run and triggers terminal workspace cleanup.
func StatusActive(status string) bool {
	switch status {
	case "", "open", "ready", "in_progress":
		return true
	default:
		return false
	}
}

// Reconcile performs the ID-refresh read for the given issue ids and
// returns the subset whose runs must stop: issues that no longer appear in
// the dispatch answer or whose status is not active.
func Reconcile(ctx context.Context, tr tracker.Tracker, ids []string) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	issues, err := tr.Dispatchable(ctx, tracker.DispatchQuery{IDs: ids})
	if err != nil {
		return nil, fmt.Errorf("reconcile read: %w", err)
	}
	present := make(map[string]tracker.Issue, len(issues))
	for _, issue := range issues {
		present[issue.ID] = issue
	}
	var stop []string
	for _, id := range ids {
		issue, ok := present[id]
		if !ok || !StatusActive(issue.Status) {
			stop = append(stop, id)
			continue
		}
	}
	return stop, nil
}
