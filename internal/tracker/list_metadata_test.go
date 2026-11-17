package tracker

import (
	"context"
	"testing"
	"time"
)

// TestListByMetadataQueryShape pins the single-query read: one uncapped list
// spanning the record statuses, filtered by the metadata pair.
func TestListByMetadataQueryShape(t *testing.T) {
	t.Parallel()
	ft := &fakeTransport{}
	b := NewBeads(ft, "lambe@test")

	issues, err := b.ListByMetadata(context.Background(), MetaKind, KindAttempt)
	if err != nil {
		t.Fatalf("list by metadata: %v", err)
	}
	if len(issues) != 1 || issues[0].ID != "x-1" {
		t.Errorf("issues = %+v, want the one fake row decoded", issues)
	}
	if len(ft.lists) != 1 {
		t.Fatalf("list calls = %d, want exactly 1", len(ft.lists))
	}
	q := ft.lists[0]
	if !q.Unlimited {
		t.Error("query is capped; the record-set read must be unlimited")
	}
	for _, want := range recordStatuses() {
		found := false
		for _, got := range q.Statuses {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Errorf("query statuses %v missing %q", q.Statuses, want)
		}
	}
	if q.Metadata[MetaKind] != KindAttempt {
		t.Errorf("query metadata = %v, want %s=%s", q.Metadata, MetaKind, KindAttempt)
	}
}

// TestGroupAttempts sorts each session's records by number and skips records
// missing the fields the decode needs.
func TestGroupAttempts(t *testing.T) {
	t.Parallel()
	wake := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	records := []Issue{
		{ID: "a3", Metadata: map[string]string{metaSession: "a", metaAttempt: "3", metaPGID: "99"}},
		{ID: "a1", Metadata: map[string]string{metaSession: "a", metaAttempt: "1"}},
		{ID: "b1", Metadata: map[string]string{metaSession: "b", metaAttempt: "1", metaWakeAt: wake.Format(time.RFC3339)}},
		{ID: "no-number", Metadata: map[string]string{metaSession: "a"}},
		{ID: "no-session", Metadata: map[string]string{metaAttempt: "4"}},
	}

	grouped := GroupAttempts(records)
	if len(grouped) != 2 {
		t.Fatalf("grouped sessions = %d, want 2", len(grouped))
	}
	a := grouped["a"]
	if len(a) != 2 || a[0].Number != 1 || a[1].Number != 3 || a[1].PGID != 99 {
		t.Errorf("session a = %+v, want attempts 1 and 3 in order", a)
	}
	b := grouped["b"]
	if len(b) != 1 || !b[0].WakeAt.Equal(wake) {
		t.Errorf("session b = %+v, want one attempt waking at %s", b, wake)
	}
}
