// Package tracker defines lambe's typed interface to a Beads tracker and the
// Beads-backed implementation. A tracker is the supervisor's entire state
// substrate: the dispatchable work front, claim leases, wake timers, the
// mutation journal, and lambe's own operational records (attempts, sessions).
//
// Implementations must be layout-agnostic: a hub clone, a repo-level tracker,
// and an in-repo .beads/ directory are all just configured paths behind the
// same interface.
package tracker

import (
	"context"
	"errors"
	"time"
)

// Sentinel outcomes. Callers branch on these with errors.Is, never on
// formatted message text.
var (
	// ErrLostRace reports a guarded write whose precondition no longer held
	// (bd exit 13, or a claim already held by another actor). Nothing was
	// written; the caller must stand down, not retry the same guard.
	ErrLostRace = errors.New("lost race")

	// ErrFrozen reports bd exit 14: a migration freeze briefly blocks
	// writes. The caller may retry the same operation later.
	ErrFrozen = errors.New("migration freeze")

	// ErrNotOwner reports a heartbeat or release on a claim the actor no
	// longer holds (reclaimed, released, or closed). For a supervisor this
	// is a stop signal: the work is someone else's now.
	ErrNotOwner = errors.New("claim no longer held")

	// ErrJournalTruncated reports an events read whose checkpoint precedes
	// the journal's retained floor. The consumer must re-baseline with a
	// full read of the retained window (see TruncatedError).
	ErrJournalTruncated = errors.New("events journal truncated")

	// ErrJournalDisabled reports an events journal that has never been
	// enabled; no records exist to consume.
	ErrJournalDisabled = errors.New("events journal disabled")

	// ErrUnsupported reports an operation the transport cannot perform.
	// The HTTP transport returns this for write operations until the
	// daemon's supervised bd serve wiring lands.
	ErrUnsupported = errors.New("operation unsupported by transport")
)

// TruncatedError carries the retained window returned by the journal when a
// consumer's checkpoint fell below the floor. Re-baseline by reading from
// Floor onward: events below Floor are gone for good.
type TruncatedError struct {
	Floor int64
	Head  int64
}

// Error implements error.
func (e *TruncatedError) Error() string {
	// Deliberately constant-message; the window rides on the struct.
	return "events journal truncated below checkpoint"
}

// Unwrap makes errors.Is(err, ErrJournalTruncated) true.
func (e *TruncatedError) Unwrap() error { return ErrJournalTruncated }

// The issue statuses lambe reads and writes. bd's "ready" is a
// pseudo-status (the dispatchable front), not a stored value.
const (
	// StatusReady is bd's pseudo-status selecting the dispatchable front.
	StatusReady = "ready"
	// StatusOpen is the open pool a claim is released back to.
	StatusOpen = "open"
	// StatusInProgress marks a claimed issue under active work.
	StatusInProgress = "in_progress"
	// StatusBlocked hides an issue from the dispatch front while keeping it
	// readable through metadata queries.
	StatusBlocked = "blocked"
	// StatusClosed is bd's terminal status.
	// StatusDeferred is bd's snoozed state; it returns to open lazily
	// on the next dispatch-front read once the defer time passes.
	StatusDeferred = "deferred"
	StatusClosed   = "closed"
)

// Issue is a single bead as lambe sees it.
type Issue struct {
	ID       string
	Title    string
	Status   string
	Assignee string
	Revision string
	Metadata map[string]string
}

// DispatchQuery selects the dispatchable work front. It expresses the
// two-read dispatch: read by status names, or read an explicit set of ids.
// Exactly one dimension is normally set; the zero query matches nothing and
// short-circuits without touching the tracker.
type DispatchQuery struct {
	Statuses []string
	IDs      []string
	Metadata map[string]string
}

// Empty reports whether the query can match nothing, in which case
// implementations must not contact the tracker at all.
func (q DispatchQuery) Empty() bool {
	return len(q.Statuses) == 0 && len(q.IDs) == 0
}

// IssueSpec describes an issue to create through EnsureIssue.
type IssueSpec struct {
	Title     string
	Type      string
	Priority  int
	Labels    []string
	Metadata  map[string]string
	Ephemeral bool
}

// Attempt is one supervised execution attempt of a session, recorded as a
// pinned-id bead so crash recovery can find it.
type Attempt struct {
	SessionID       string
	Number          int
	PGID            int
	ProcessIdentity string    // kernel identity of the recorded group leader
	WakeAt          time.Time // zero means no wake timer armed
}

// Event is one journaled mutation.
type Event struct {
	Seq     int64
	At      time.Time
	Op      string // create|update|close|delete|dep_add|dep_remove|comment
	IssueID string
	Actor   string
}

// Capabilities advertises optional tracker features so callers can degrade
// instead of guessing. All capabilities are absent in bd 1.3.x.
type Capabilities struct {
	// SSEWatch reports server-sent events:watch streaming.
	SSEWatch bool
	// RetainedTombstones reports --retain-ids tombstones and types.retained.
	RetainedTombstones bool
	// HTTPHeartbeat reports heartbeat and reclaim over the HTTP API.
	HTTPHeartbeat bool
}

// Tracker is lambe's typed view of one Beads tracker.
type Tracker interface {
	// Capabilities reports the features this tracker build supports.
	Capabilities(ctx context.Context) (Capabilities, error)

	// Dispatchable reads the dispatch front. A query with neither statuses
	// nor ids returns nil without contacting the tracker.
	Dispatchable(ctx context.Context, q DispatchQuery) ([]Issue, error)

	// Claim atomically takes the issue: assignee set, status in_progress,
	// lease armed. Losing the race returns an error wrapping ErrLostRace.
	Claim(ctx context.Context, id string) (Issue, error)

	// Release gives the claim back, restoring the issue to the open pool.
	Release(ctx context.Context, id string) error

	// Heartbeat extends the lease on a held claim. A failure wrapping
	// ErrNotOwner means the claim was taken away: stop supervising it.
	Heartbeat(ctx context.Context, id string) error

	// Reclaim reverts claims whose leases expired more than olderThan ago,
	// restricted to ids when non-empty. Run it on a timer with a window of
	// roughly twice the claim TTL.
	Reclaim(ctx context.Context, olderThan time.Duration, ids []string) error

	// Defer parks the issue until until, hiding it from the dispatch front.
	// Waking is lazy: once until passes, the next Dispatchable read returns
	// the issue to open.
	Defer(ctx context.Context, id string, until time.Time, reason string) error

	// Events returns journal records with seq greater than since, in order.
	// A checkpoint below the retained floor returns an error wrapping
	// ErrJournalTruncated carrying the re-baseline window.
	Events(ctx context.Context, since int64) ([]Event, error)

	// EnsureIssue idempotently creates the issue with pinned id
	// prefix-key, where prefix is the tracker's issue prefix. A duplicate
	// create is a no-op returning the existing issue.
	EnsureIssue(ctx context.Context, key string, spec IssueSpec) (Issue, error)

	// PutAttempt records an attempt as a pinned-id bead. Re-putting the
	// same session and number is an idempotent update.
	PutAttempt(ctx context.Context, a Attempt) error

	// Attempts returns the recorded attempts for a session, in order.
	Attempts(ctx context.Context, sessionID string) ([]Attempt, error)
}
