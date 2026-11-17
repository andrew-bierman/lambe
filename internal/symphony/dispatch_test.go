package symphony

import (
	"testing"
	"time"

	"github.com/andrew-bierman/lambe/internal/tracker"
)

func TestSortIssuesForDispatchOrdersByPriorityThenAge(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	issues := []tracker.Issue{
		{ID: "low", Identifier: "low", Title: "low", Priority: 3, CreatedAt: base},
		{ID: "high", Identifier: "high", Title: "high", Priority: 1, CreatedAt: base.Add(time.Hour)},
		{ID: "old-medium", Identifier: "old-medium", Title: "old-medium", Priority: 2, CreatedAt: base},
		{ID: "new-medium", Identifier: "new-medium", Title: "new-medium", Priority: 2, CreatedAt: base.Add(time.Hour)},
	}
	sortIssuesForDispatch(issues, nil)
	want := []string{"high", "old-medium", "new-medium", "low"}
	for i, issue := range issues {
		if issue.ID != want[i] {
			t.Errorf("position %d: got %q, want %q; order = %v", i, issue.ID, want[i], idsOf(issues))
		}
	}
}

func TestSortIssuesForDispatchFiltersRequiredLabels(t *testing.T) {
	t.Parallel()
	issues := []tracker.Issue{
		{ID: "a", Identifier: "a", Title: "a", Labels: []string{"lambe:babysit", "priority:high"}},
		{ID: "b", Identifier: "b", Title: "b", Labels: []string{"lambe:babysit"}},
		{ID: "c", Identifier: "c", Title: "c", Labels: []string{"other"}},
	}
	got := sortIssuesForDispatch(issues, []string{"lambe:babysit", "priority:high"})
	if len(got) != 1 || got[0].ID != "a" {
		t.Errorf("got %v, want only [a]", idsOf(got))
	}
}

func TestSortIssuesForDispatchSkipsBlockedIssues(t *testing.T) {
	t.Parallel()
	issues := []tracker.Issue{
		{ID: "blocked", Identifier: "blocked", Title: "blocked", BlockedBy: []tracker.BlockerRef{{Identifier: "other"}}},
		{ID: "free", Identifier: "free", Title: "free"},
	}
	got := sortIssuesForDispatch(issues, nil)
	if len(got) != 1 || got[0].ID != "free" {
		t.Errorf("got %v, want only [free]", idsOf(got))
	}
}

func idsOf(issues []tracker.Issue) []string {
	out := make([]string, len(issues))
	for i, issue := range issues {
		out[i] = issue.ID
	}
	return out
}
