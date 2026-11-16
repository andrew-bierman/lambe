package symphony

import (
	"testing"
	"time"
)

func TestFailureBackoff(t *testing.T) {
	t.Parallel()
	maxDelay := 10 * time.Minute
	tests := []struct {
		attempt int
		want    time.Duration
	}{
		{attempt: 1, want: 10 * time.Second},
		{attempt: 2, want: 20 * time.Second},
		{attempt: 3, want: 40 * time.Second},
		{attempt: 4, want: 80 * time.Second},
		{attempt: 7, want: 10 * time.Minute}, // 10s<<6 = 640s, capped
		{attempt: 0, want: 10 * time.Second}, // clamped to attempt 1
	}
	for _, tt := range tests {
		if got := FailureBackoff(tt.attempt, maxDelay); got != tt.want {
			t.Errorf("FailureBackoff(%d) = %s, want %s", tt.attempt, got, tt.want)
		}
	}
}

func TestFailureBackoffCaps(t *testing.T) {
	t.Parallel()
	tests := []struct {
		cap     time.Duration
		attempt int
		want    time.Duration
	}{
		{cap: 30 * time.Second, attempt: 2, want: 20 * time.Second},
		{cap: 30 * time.Second, attempt: 3, want: 30 * time.Second},
		{cap: 30 * time.Second, attempt: 50, want: 30 * time.Second},
		{cap: 0, attempt: 50, want: DefaultRetryBackoff}, // zero cap falls back to the default
	}
	for _, tt := range tests {
		if got := FailureBackoff(tt.attempt, tt.cap); got != tt.want {
			t.Errorf("FailureBackoff(%d, %s) = %s, want %s", tt.attempt, tt.cap, got, tt.want)
		}
	}
}

func TestRetryQueue(t *testing.T) {
	t.Parallel()
	now := time.Now()
	q := &Queue{}
	q.Add(Retry{IssueID: "b", Kind: RetryFailure, At: now.Add(2 * time.Second)})
	q.Add(Retry{IssueID: "a", Kind: RetryContinuation, At: now.Add(1 * time.Second)})

	if due := q.Due(now); len(due) != 0 {
		t.Fatalf("nothing due yet, got %v", due)
	}
	// A re-add replaces the pending entry for the same issue.
	q.Add(Retry{IssueID: "a", Kind: RetryFailure, At: now.Add(3 * time.Second)})

	due := q.Due(now.Add(2 * time.Second))
	if len(due) != 1 || due[0].IssueID != "b" {
		t.Fatalf("due at +2s = %v, want only b (a was replaced)", due)
	}
	due = q.Due(now.Add(3 * time.Second))
	if len(due) != 1 || due[0].IssueID != "a" {
		t.Errorf("due = %v, want the replaced a entry", due)
	}
	if q.Len() != 0 {
		t.Errorf("queue not drained, len = %d", q.Len())
	}
}
