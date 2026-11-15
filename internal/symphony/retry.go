package symphony

import (
	"sync"
	"time"
)

// Retry timing constants from the Symphony specification: failure retries
// back off exponentially from 10s; continuation retries are a fixed 1s
// after a normal exit. The default backoff cap is 10 minutes.
const (
	FailureRetryBase    = 10 * time.Second
	ContinuationRetry   = 1 * time.Second
	DefaultRetryBackoff = 10 * time.Minute
)

// DefaultDeferHorizon is the parking duration used for TurnDeferred turns
// whose TurnResult.WakeAt is zero.
const DefaultDeferHorizon = 5 * time.Minute

// FailureBackoff returns the failure-retry delay for the given attempt
// number: min(10000ms · 2^(attempt-1), cap). Attempt numbers below 1 are
// treated as 1.
func FailureBackoff(attempt int, maxDelay time.Duration) time.Duration {
	if maxDelay <= 0 {
		maxDelay = DefaultRetryBackoff
	}
	if attempt < 1 {
		attempt = 1
	}
	delay := FailureRetryBase
	for i := 1; i < attempt; i++ {
		delay *= 2
		if delay <= 0 || delay >= maxDelay {
			return maxDelay
		}
	}
	if delay > maxDelay {
		return maxDelay
	}
	return delay
}

// RetryKind distinguishes the two retry paths: a failed attempt backs off
// exponentially and consumes the next attempt number; a continuation retry
// fires 1s after a normal exit and reuses the attempt.
type RetryKind int

// The retry kinds.
const (
	RetryFailure RetryKind = iota
	RetryContinuation
)

// Retry is one scheduled re-entry of a supervised issue.
type Retry struct {
	IssueID string
	Kind    RetryKind
	At      time.Time
}

// Queue is the in-memory retry schedule. Ticks drain entries whose wake
// time has passed.
type Queue struct {
	mu    sync.Mutex
	items []Retry
}

// Add schedules a retry. A retry already queued for the issue is replaced.
func (q *Queue) Add(r Retry) {
	q.mu.Lock()
	defer q.mu.Unlock()
	replaced := false
	for i := range q.items {
		if q.items[i].IssueID == r.IssueID {
			q.items[i] = r
			replaced = true
			break
		}
	}
	if !replaced {
		q.items = append(q.items, r)
	}
}

// Due removes and returns every retry whose wake time is at or before now,
// in schedule order.
func (q *Queue) Due(now time.Time) []Retry {
	q.mu.Lock()
	defer q.mu.Unlock()
	var due []Retry
	kept := q.items[:0]
	for _, r := range q.items {
		if !r.At.After(now) {
			due = append(due, r)
			continue
		}
		kept = append(kept, r)
	}
	q.items = kept
	sortRetries(due)
	return due
}

// Len reports the number of scheduled retries.
func (q *Queue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}

// sortRetries orders due retries by wake time.
func sortRetries(rs []Retry) {
	for i := 1; i < len(rs); i++ {
		for j := i; j > 0 && rs[j].At.Before(rs[j-1].At); j-- {
			rs[j], rs[j-1] = rs[j-1], rs[j]
		}
	}
}
