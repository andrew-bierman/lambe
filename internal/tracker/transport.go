package tracker

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
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

// BatchVerb is one of the four write verbs bd's batch grammar accepts.
// Everything else (claims, defers, heartbeats, reads) must go single-op.
type BatchVerb string

// The verbs of bd's batch file grammar: close|update|create|dep add|dep
// remove (dep add/remove count as one verb family in bd's docs).
const (
	BatchVerbClose     BatchVerb = "close"
	BatchVerbUpdate    BatchVerb = "update"
	BatchVerbCreate    BatchVerb = "create"
	BatchVerbDepAdd    BatchVerb = "dep add"
	BatchVerbDepRemove BatchVerb = "dep remove"
)

// BatchFields carries the k=v pairs a batched update may set. bd's batch
// grammar is deliberately narrow: metadata is not settable here, so
// metadata writes (attempt re-puts) stay single-op Update calls.
type BatchFields struct {
	Status   string
	Priority *int
	Title    string
	Assignee string
	Force    bool // override the open-children refusal on closing updates
}

// Empty reports whether no field is set, which renders no update at all.
func (f BatchFields) Empty() bool {
	return f.Status == "" && f.Priority == nil && f.Title == "" && f.Assignee == "" && !f.Force
}

// BatchOp is one line of a bd batch file: a single-transaction write.
// Construct it with the BatchClose/BatchUpdate/BatchCreate/BatchDepAdd/
// BatchDepRemove helpers; Render validates and emits the grammar line.
type BatchOp struct {
	Verb     BatchVerb
	ID       string // close/update target
	Reason   string // close reason; free text, becomes close_reason
	Fields   BatchFields
	Type     string // create: issue type
	Priority int    // create: priority
	Title    string // create: title
	FromID   string // dep: source issue
	ToID     string // dep: target issue
	DepType  string // dep add: type; empty is bd's default (blocks)
}

// BatchClose returns a terminal close; reason is recorded as the close
// reason, carrying the closing comment in the same write.
func BatchClose(id, reason string) BatchOp {
	return BatchOp{Verb: BatchVerbClose, ID: id, Reason: reason}
}

// BatchUpdate returns a field update. At least one field must be set.
func BatchUpdate(id string, f BatchFields) BatchOp {
	return BatchOp{Verb: BatchVerbUpdate, ID: id, Fields: f}
}

// BatchCreate returns a create; typ is the issue type (e.g. "task").
func BatchCreate(typ string, priority int, title string) BatchOp {
	return BatchOp{Verb: BatchVerbCreate, Type: typ, Priority: priority, Title: title}
}

// BatchDepAdd returns a dependency add; depType empty means bd's default.
func BatchDepAdd(fromID, toID, depType string) BatchOp {
	return BatchOp{Verb: BatchVerbDepAdd, FromID: fromID, ToID: toID, DepType: depType}
}

// BatchDepRemove returns a dependency removal.
func BatchDepRemove(fromID, toID string) BatchOp {
	return BatchOp{Verb: BatchVerbDepRemove, FromID: fromID, ToID: toID}
}

// Render emits the batch grammar line for the op, or an error when the op
// is outside the grammar (wrong verb, missing required tokens).
func (o BatchOp) Render() (string, error) {
	switch o.Verb {
	case BatchVerbClose:
		if o.ID == "" {
			return "", fmt.Errorf("batch close: id required")
		}
		if o.Reason == "" {
			return "close " + o.ID, nil
		}
		return "close " + o.ID + " " + quoteBatchToken(o.Reason), nil
	case BatchVerbUpdate:
		if o.ID == "" {
			return "", fmt.Errorf("batch update: id required")
		}
		if o.Fields.Empty() {
			return "", fmt.Errorf("batch update %s: at least one field required", o.ID)
		}
		var sb strings.Builder
		sb.WriteString("update " + o.ID)
		if o.Fields.Status != "" {
			sb.WriteString(" status=" + o.Fields.Status)
		}
		if o.Fields.Priority != nil {
			sb.WriteString(" priority=" + strconv.Itoa(*o.Fields.Priority))
		}
		if o.Fields.Title != "" {
			sb.WriteString(" title=" + quoteBatchToken(o.Fields.Title))
		}
		if o.Fields.Assignee != "" {
			sb.WriteString(" assignee=" + o.Fields.Assignee)
		}
		if o.Fields.Force {
			sb.WriteString(" force=true")
		}
		return sb.String(), nil
	case BatchVerbCreate:
		if o.Type == "" || o.Title == "" {
			return "", fmt.Errorf("batch create: type and title required")
		}
		return "create " + o.Type + " " + strconv.Itoa(o.Priority) + " " + quoteBatchToken(o.Title), nil
	case BatchVerbDepAdd:
		if o.FromID == "" || o.ToID == "" {
			return "", fmt.Errorf("batch dep add: from and to ids required")
		}
		line := "dep add " + o.FromID + " " + o.ToID
		if o.DepType != "" {
			line += " " + o.DepType
		}
		return line, nil
	case BatchVerbDepRemove:
		if o.FromID == "" || o.ToID == "" {
			return "", fmt.Errorf("batch dep remove: from and to ids required")
		}
		return "dep remove " + o.FromID + " " + o.ToID, nil
	default:
		return "", fmt.Errorf("batch verb %q is outside the batch grammar", o.Verb)
	}
}

// quoteBatchToken wraps a token in double quotes when it contains
// whitespace, quotes, or backslashes, escaping the latter two.
func quoteBatchToken(tok string) string {
	if !strings.ContainsAny(tok, " \t\"\\") {
		return tok
	}
	escaped := strings.ReplaceAll(tok, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `"`, `\"`)
	return `"` + escaped + `"`
}

// DefaultCommandTimeout bounds one bd subprocess invocation when the
// transport does not configure its own limit. Without it a wedged bd (a
// hung dolt engine, a stuck lock) would wedge every caller — including the
// daemon's tick loop — forever.
const DefaultCommandTimeout = 60 * time.Second

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
	// Batch applies write operations in one bd batch transaction: one
	// engine start, one DOLT_COMMIT, all-or-nothing. Ops are limited to
	// the batch grammar verbs (close/update/create/dep); render errors
	// surface before any tracker contact.
	Batch(ctx context.Context, ops []BatchOp) (Result, error)

	Version(ctx context.Context) (string, error)
	ConfigGet(ctx context.Context, key string) (string, error)
}
