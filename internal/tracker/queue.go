package tracker

import (
	"context"
	"strings"
)

// ReadinessSource is the tracker readiness read behind the state surface's
// queue projection. *Local implements it; the transport-agnostic Tracker
// interface does not carry it, so consumers probe with a type assertion and
// degrade to no queue rows when the backend cannot answer.
type ReadinessSource interface {
	// Readiness returns the dispatch-front rows for the given label lanes:
	// labels is the every-label lane, anyLabels the any-label lane.
	Readiness(ctx context.Context, labels, anyLabels []string) ([]ReadinessRow, error)
}

// QueueLanes resolves the readiness read's label lanes for a configuration:
// named-workflow route labels form an any-label lane and supersede the
// required-label lane, matching the dispatcher's per-route reads. This is
// the one code source for that selection; the queue projection and the TUI
// must agree on it.
func QueueLanes(labels, routeLabels []string) (every, anyLabels []string) {
	if len(routeLabels) > 0 {
		return nil, routeLabels
	}
	return labels, nil
}

// IssueLabels resolves an issue's labels the way the dispatch filter does:
// a stamped "labels" metadata value (comma-separated) wins over native
// labels. Issues with neither have no labels.
func IssueLabels(issue Issue) []string {
	raw, stamped := issue.Metadata["labels"]
	if !stamped {
		return issue.Labels
	}
	if raw == "" {
		return nil
	}
	return strings.Split(raw, ",")
}

// QueueReadiness reads the dispatch-front readiness rows for the configured
// label lanes and post-filters them against the tracker's own records: bd
// ready --explain has been observed to ignore label filters, so every row
// is verified against the issue's stored labels here, and an issue carrying
// more than one named-workflow route label is ambiguous — the dispatcher
// skips it, so it is served as blocked, never dispatchable. A tracker
// without readiness support yields no rows; a failed readiness read is the
// caller's error to degrade on.
func QueueReadiness(ctx context.Context, tr Tracker, labels, routeLabels []string) ([]ReadinessRow, error) {
	source, ok := tr.(ReadinessSource)
	if !ok {
		return nil, nil
	}
	every, anyLabels := QueueLanes(labels, routeLabels)
	rows, err := source.Readiness(ctx, every, anyLabels)
	if err != nil {
		return nil, err
	}
	return FilterReadinessRows(ctx, tr, rows, every, anyLabels)
}

// FilterReadinessRows post-filters readiness rows against the tracker's
// own records, so a backend that ignores label filters still serves only
// lane-valid rows, and a multi-route issue is never dispatchable. It costs
// one grouped read for the rows' label records.
func FilterReadinessRows(ctx context.Context, tr Tracker, rows []ReadinessRow, every, anyLabels []string) ([]ReadinessRow, error) {
	if len(rows) == 0 {
		return nil, nil
	}
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		if row.ID != "" {
			ids = append(ids, row.ID)
		}
	}
	records, err := tr.Dispatchable(ctx, DispatchQuery{IDs: ids})
	if err != nil {
		return nil, err
	}
	byID := make(map[string]Issue, len(records))
	for _, issue := range records {
		byID[issue.ID] = issue
	}
	out := rows[:0]
	for _, row := range rows {
		issue, ok := byID[row.ID]
		if !ok {
			// The readiness front and the label records disagreed; an
			// unverifiable row is never served.
			continue
		}
		keep, blocked, reason := laneVerdict(IssueLabels(issue), every, anyLabels)
		if !keep {
			continue
		}
		if blocked {
			row.Blocked = true
			row.Reason = reason
		}
		out = append(out, row)
	}
	return out, nil
}

// laneVerdict applies the label lanes to one issue's labels. keep reports
// whether the issue belongs to any configured lane; blocked and reason
// carry the ambiguity verdict for an issue the dispatcher would refuse.
func laneVerdict(issueLabels, every, anyLabels []string) (keep, blocked bool, reason string) {
	if len(anyLabels) > 0 {
		var matched []string
		for _, label := range issueLabels {
			for _, route := range anyLabels {
				if label == route {
					matched = append(matched, label)
					break
				}
			}
		}
		switch len(matched) {
		case 0:
			return false, false, ""
		case 1:
			return true, false, ""
		default:
			return true, true, "ambiguous workflow labels: " + strings.Join(matched, ", ")
		}
	}
	if len(every) > 0 {
		seen := make(map[string]bool, len(issueLabels))
		for _, label := range issueLabels {
			seen[label] = true
		}
		for _, label := range every {
			if !seen[label] {
				return false, false, ""
			}
		}
	}
	return true, false, ""
}
