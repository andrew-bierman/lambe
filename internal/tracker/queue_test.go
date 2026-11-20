package tracker

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// stubTracker is a bare Tracker whose only used method is Dispatchable; it
// carries no readiness source, the shape the queue degrades to.
type stubTracker struct {
	Tracker
}

func (stubTracker) Dispatchable(_ context.Context, _ DispatchQuery) ([]Issue, error) {
	return nil, nil
}

// fakeReadiness is a readiness source that mirrors the observed bd bug: it
// ignores the label filters entirely and returns every ready row it holds.
type fakeReadiness struct {
	Tracker
	rows     []ReadinessRow
	issues   map[string]Issue
	err      error
	reads    int
	dispatch []DispatchQuery
}

func (f *fakeReadiness) Readiness(context.Context, []string, []string) ([]ReadinessRow, error) {
	f.reads++
	if f.err != nil {
		return nil, f.err
	}
	return f.rows, nil
}

func (f *fakeReadiness) Dispatchable(_ context.Context, q DispatchQuery) ([]Issue, error) {
	f.dispatch = append(f.dispatch, q)
	var out []Issue
	for _, id := range q.IDs {
		if issue, ok := f.issues[id]; ok {
			out = append(out, issue)
		}
	}
	return out, nil
}

func TestQueueReadinessPostFiltersLabelIgnorantBackend(t *testing.T) {
	t.Parallel()
	fake := &fakeReadiness{
		Tracker: stubTracker{},
		rows: []ReadinessRow{
			{ID: "route-a", Title: "single route"},
			{ID: "ambiguous", Title: "two routes"},
			{ID: "unlabeled", Title: "no labels"},
			{ID: "other-lane", Title: "unrelated label"},
		},
		issues: map[string]Issue{
			"route-a":    {ID: "route-a", Labels: []string{"lambe:general"}},
			"ambiguous":  {ID: "ambiguous", Labels: []string{"lambe:general", "lambe:fleet"}},
			"unlabeled":  {ID: "unlabeled"},
			"other-lane": {ID: "other-lane", Labels: []string{"unrelated"}},
		},
	}
	rows, err := QueueReadiness(context.Background(), fake, nil, []string{"lambe:general", "lambe:fleet"})
	if err != nil {
		t.Fatalf("QueueReadiness: %v", err)
	}
	byID := map[string]ReadinessRow{}
	for _, row := range rows {
		byID[row.ID] = row
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want the two route-labeled issues only", rows)
	}
	got, ok := byID["route-a"]
	if !ok || got.Blocked {
		t.Errorf("route-a row = %+v, want ready", got)
	}
	got, ok = byID["ambiguous"]
	if !ok {
		t.Fatalf("ambiguous row missing: %+v", rows)
	}
	// An issue tagged for two workflows is not dispatchable: the dispatcher
	// skips it, so the queue must never present it as ready.
	if !got.Blocked {
		t.Errorf("ambiguous row = %+v, want blocked", got)
	}
	if got.Reason == "" || !strings.Contains(got.Reason, "lambe:general") || !strings.Contains(got.Reason, "lambe:fleet") {
		t.Errorf("ambiguous reason = %q, want it to name both labels", got.Reason)
	}
	for _, absent := range []string{"unlabeled", "other-lane"} {
		if _, ok := byID[absent]; ok {
			t.Errorf("row %q is outside every configured lane and must be dropped", absent)
		}
	}
	// One readiness read plus one grouped label read, never a read per row.
	if fake.reads != 1 || len(fake.dispatch) != 1 {
		t.Errorf("reads = (readiness %d, dispatch %d), want (1, 1)", fake.reads, len(fake.dispatch))
	}
	if len(fake.dispatch[0].IDs) != 4 {
		t.Errorf("label read ids = %v, want one grouped read for every row", fake.dispatch[0].IDs)
	}
}

func TestQueueReadinessEveryLabelLane(t *testing.T) {
	t.Parallel()
	fake := &fakeReadiness{
		Tracker: stubTracker{},
		rows: []ReadinessRow{
			{ID: "all", Title: "carries every label"},
			{ID: "some", Title: "carries one"},
		},
		issues: map[string]Issue{
			"all":  {ID: "all", Labels: []string{"supervised", "extra"}},
			"some": {ID: "some", Labels: []string{"extra"}},
		},
	}
	rows, err := QueueReadiness(context.Background(), fake, []string{"supervised"}, nil)
	if err != nil {
		t.Fatalf("QueueReadiness: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != "all" {
		t.Fatalf("rows = %+v, want only the issue carrying every required label", rows)
	}
}

func TestQueueReadinessStampedMetadataLabels(t *testing.T) {
	t.Parallel()
	fake := &fakeReadiness{
		Tracker: stubTracker{},
		rows:    []ReadinessRow{{ID: "stamped"}},
		issues: map[string]Issue{
			// The dispatch filter reads stamped "labels" metadata when
			// present; the queue filter must agree with it.
			"stamped": {ID: "stamped", Labels: []string{"stale"}, Metadata: map[string]string{"labels": "lambe:general"}},
		},
	}
	rows, err := QueueReadiness(context.Background(), fake, nil, []string{"lambe:general"})
	if err != nil {
		t.Fatalf("QueueReadiness: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != "stamped" || rows[0].Blocked {
		t.Fatalf("rows = %+v, want the stamped-label issue ready", rows)
	}
}

func TestQueueReadinessEmptyAndDegraded(t *testing.T) {
	t.Parallel()
	t.Run("empty tracker yields an empty list, not an error", func(t *testing.T) {
		t.Parallel()
		rows, err := QueueReadiness(context.Background(), &fakeReadiness{Tracker: stubTracker{}}, []string{"x"}, nil)
		if err != nil {
			t.Fatalf("QueueReadiness: %v", err)
		}
		if len(rows) != 0 {
			t.Errorf("rows = %+v, want empty", rows)
		}
	})
	t.Run("readiness failure propagates for the caller to degrade", func(t *testing.T) {
		t.Parallel()
		want := errors.New("bd unreachable")
		fake := &fakeReadiness{Tracker: stubTracker{}, err: want}
		if _, err := QueueReadiness(context.Background(), fake, nil, nil); !errors.Is(err, want) {
			t.Fatalf("err = %v, want %v", err, want)
		}
	})
	t.Run("tracker without readiness support yields no rows", func(t *testing.T) {
		t.Parallel()
		rows, err := QueueReadiness(context.Background(), stubTracker{}, []string{"x"}, nil)
		if err != nil {
			t.Fatalf("QueueReadiness: %v", err)
		}
		if rows != nil {
			t.Errorf("rows = %+v, want nil", rows)
		}
	})
}

func TestQueueLanes(t *testing.T) {
	t.Parallel()
	every, anyLabels := QueueLanes([]string{"supervised"}, []string{"lambe:a", "lambe:b"})
	if every != nil || len(anyLabels) != 2 {
		t.Errorf("routes supersede the required-label lane: got every=%v anyLabels=%v", every, anyLabels)
	}
	every, anyLabels = QueueLanes([]string{"supervised"}, nil)
	if len(every) != 1 || every[0] != "supervised" || anyLabels != nil {
		t.Errorf("no routes keeps the required-label lane: got every=%v anyLabels=%v", every, anyLabels)
	}
	every, anyLabels = QueueLanes(nil, nil)
	if every != nil || anyLabels != nil {
		t.Errorf("empty configuration has no lanes: got every=%v anyLabels=%v", every, anyLabels)
	}
}

// TestQueueReadinessPostFiltersAgainstBdTracker pins the post-filter against
// the real CLI in a disposable project: bd ready --explain has been observed
// to ignore label filters, so rows outside every configured lane and rows
// tagged for two workflows must be corrected by the filter, not served raw.
func TestQueueReadinessPostFiltersAgainstBdTracker(t *testing.T) {
	bin := bdBin()
	if bin == "" {
		t.Skip("bd binary not found; set LAMBE_TEST_BD=1 with bd on PATH to run")
	}
	ctx := t.Context()
	local, err := NewLocal(bin, t.TempDir(), "lambe@testbox")
	if err != nil {
		t.Fatal(err)
	}
	if err := local.Init(ctx); err != nil {
		t.Fatal(err)
	}
	const (
		routeA = "lambe:general"
		routeB = "lambe:fleet"
	)
	seed := func(title string, labels []string) string {
		t.Helper()
		issue, err := local.EnsureIssue(ctx, title, IssueSpec{Title: title, Priority: 2, Labels: labels})
		if err != nil {
			t.Fatalf("ensure %s: %v", title, err)
		}
		return issue.ID
	}
	single := seed("queue-single-route", []string{routeA})
	ambiguous := seed("queue-ambiguous-route", []string{routeA, routeB})
	unlabeled := seed("queue-unlabeled", nil)

	rows, err := QueueReadiness(ctx, local, nil, []string{routeA, routeB})
	if err != nil {
		t.Fatalf("QueueReadiness: %v", err)
	}
	byID := map[string]ReadinessRow{}
	for _, row := range rows {
		byID[row.ID] = row
	}
	got, ok := byID[single]
	if !ok || got.Blocked {
		t.Errorf("single-route row = %+v (present %t), want ready", got, ok)
	}
	got, ok = byID[ambiguous]
	if ok && !got.Blocked {
		// Present only while bd ignores the label filter — but whenever it
		// appears it must not be dispatchable.
		t.Errorf("ambiguous row = %+v, want blocked", got)
	}
	if _, ok := byID[unlabeled]; ok {
		t.Errorf("unlabeled row served although no configured lane matches it")
	}
	for _, row := range rows {
		if row.Blocked && row.Reason == "" {
			t.Errorf("blocked row %s carries no reason", row.ID)
		}
	}
}
