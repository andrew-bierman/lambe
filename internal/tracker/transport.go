package tracker

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// Result is a successful transport reply: the payload inside bd's JSON
// envelope (already unwrapped), plus raw bytes for line-oriented replies
// such as the events journal.
type Result struct {
	Data json.RawMessage
	Raw  []byte
}

// ExitError is a failed bd invocation. Code is the process exit code; the
// tracker maps codes to typed outcomes (13 lost race, 14 freeze) and leaves
// everything else as a plain error.
type ExitError struct {
	Code   int
	Stdout string
	Stderr string
}

// Error implements error.
func (e *ExitError) Error() string {
	msg := e.Stderr
	if msg == "" {
		msg = e.Stdout
	}
	if msg == "" {
		msg = "bd failed"
	}
	return fmt.Sprintf("bd exit %d: %s", e.Code, msg)
}

// CreateRequest is a transport-level create. ID is the pinned id, empty for
// an auto-assigned one.
type CreateRequest struct {
	ID        string
	Title     string
	Type      string
	Priority  int
	Labels    []string
	Metadata  map[string]string
	Ephemeral bool
}

// UpdateRequest is a transport-level update. The zero request is invalid;
// at least one write must be requested.
type UpdateRequest struct {
	ID          string
	Claim       bool    // atomic claim (ignores IfAssignee)
	IfAssignee  *string // guarded write; mismatch is bd exit 13
	Assignee    string
	Status      string
	Metadata    map[string]string
	DeferUntil  *time.Time // set with DeferReason to snooze; the time is the wake timer
	DeferClear  bool       // remove an existing defer
	DeferReason string
}

// Transport carries tracker operations to a Beads backend. Two
// implementations exist: ShellTransport forks a bd subprocess per call
// (embedded local mode), and HTTPTransport talks to a supervised bd serve
// (the daemon's hub transport). Every method returns an *ExitError for a
// command-level failure so callers can map exit codes.
type Transport interface {
	List(ctx context.Context, q DispatchQuery) (Result, error)
	Show(ctx context.Context, id string) (Result, error)
	Create(ctx context.Context, r CreateRequest) (Result, error)
	Update(ctx context.Context, r UpdateRequest) (Result, error)
	Heartbeat(ctx context.Context, id string) (Result, error)
	Unclaim(ctx context.Context, id string, ifAssignee string) (Result, error)
	Reclaim(ctx context.Context, olderThan time.Duration, ids []string) (Result, error)
	Defer(ctx context.Context, id string, until *time.Time, reason string) (Result, error)
	// Events returns raw newline-delimited JSON records with seq >
	// since, exactly as bd emits them.
	Events(ctx context.Context, since int64) (Result, error)
	Version(ctx context.Context) (string, error)
	ConfigGet(ctx context.Context, key string) (string, error)
}
