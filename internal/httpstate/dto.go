// Package httpstate exposes the Symphony spec §13.7 optional read-only HTTP
// state surface: GET /api/v1/state, GET /api/v1/{issue_identifier}, and
// POST /api/v1/refresh, plus the additive queue and history reads the
// operator views need (GET /api/v1/queue, GET /api/v1/history). It is
// stdlib-only and intended to be wired to the daemon's state source.
package httpstate

// SchemaVersion is the wire schema version carried by every response DTO.
const SchemaVersion = 1

// maxTailBytes bounds the output tail served with an issue: full output
// lives with the run's artifacts, never in the response body.
const maxTailBytes = 4096

// DaemonDTO identifies the daemon behind a response: so a client can always
// tell which daemon it is attached to. Empty optional fields mean the
// daemon could not determine them; they are never fabricated.
type DaemonDTO struct {
	// PID is the serving process id.
	PID int `json:"pid"`
	// Identity identifies this process incarnation (pid alone is
	// recyclable); empty when the platform cannot provide one.
	Identity string `json:"identity,omitempty"`
	// Machine is the daemon's configured dispatch identity.
	Machine string `json:"machine,omitempty"`
	// Version is the daemon build version.
	Version string `json:"version,omitempty"`
	// ConfigPath is the configuration file the daemon loaded; empty when
	// it ran without one.
	ConfigPath string `json:"config_path,omitempty"`
}

// SnapshotResponse is the DTO for GET /api/v1/state.
type SnapshotResponse struct {
	// SchemaVersion is the version of this wire schema.
	SchemaVersion int `json:"schema_version"`
	// Daemon identifies the serving daemon.
	Daemon DaemonDTO `json:"daemon"`
	// Runs are the supervised runs, in report order.
	Runs []RunDTO `json:"runs"`
	// DeferredUntil is the time the supervisor deferred its next action, if
	// any, in RFC 3339 format; empty when not deferred.
	DeferredUntil string `json:"deferred_until,omitempty"`
	// WakeAt is the time the supervisor will wake to re-evaluate state, if
	// known, in RFC 3339 format; empty when unset.
	WakeAt string `json:"wake_at,omitempty"`
	// NeedsHuman lists items awaiting human attention.
	NeedsHuman []NeedsHumanDTO `json:"needs_human"`
	// GateRejections is the recent gate rejection history, oldest first.
	GateRejections []GateRejectionDTO `json:"gate_rejections"`
	// RunsAsOf is when the runs section was read, RFC 3339; the in-memory
	// registry is read per request, so it is the serve time.
	RunsAsOf string `json:"runs_as_of,omitempty"`
	// NeedsHumanAsOf is when the needs-human section was read from the
	// tracker, RFC 3339; empty before the daemon's first read.
	NeedsHumanAsOf string `json:"needs_human_as_of,omitempty"`
	// GateRejectionsAsOf is when the gate-rejection section was read, RFC
	// 3339; empty before the daemon's first read.
	GateRejectionsAsOf string `json:"gate_rejections_as_of,omitempty"`
}

// RunDTO describes one supervised run in the snapshot.
type RunDTO struct {
	// Issue is the issue identifier the run is working on.
	Issue string `json:"issue"`
	// Session identifies the harness session, if any.
	Session string `json:"session,omitempty"`
	// Attempt is the attempt number for this issue, starting at 1.
	Attempt int `json:"attempt"`
	// State is the run state name (e.g. "running", "waiting", "stopped").
	State string `json:"state"`
	// LastEventAge is the human-readable age of the last observed event
	// (e.g. "5s"); "n/a" when no event has been observed.
	LastEventAge string `json:"last_event_age"`
	// LastEventAt is when the last event for this run was observed, RFC
	// 3339; the machine-readable counterpart of last_event_age, empty when
	// no event has been observed.
	LastEventAt string `json:"last_event_at,omitempty"`
	// Phase is the operator-facing supervision phase (e.g. "running",
	// "gating", "stopped: gate red"); richer than state and displayed
	// alongside it. Empty when the projection has not observed the run.
	Phase string `json:"phase,omitempty"`
}

// NeedsHumanDTO describes one item awaiting human attention.
type NeedsHumanDTO struct {
	// Issue is the issue identifier the item concerns.
	Issue string `json:"issue"`
	// Reason is why the item needs a human: the reason the escalation
	// recorded in the tracker, or the last gate output summary when no
	// recorded reason exists. Never a placeholder; empty only when no
	// record carries a reason at all.
	Reason string `json:"reason"`
	// Detail is a human-readable explanation.
	Detail string `json:"detail,omitempty"`
	// Since is when the item was raised, in RFC 3339 format; empty when
	// the record does not carry a time.
	Since string `json:"since,omitempty"`
}

// GateRejectionDTO records one verified-completion gate rejection.
type GateRejectionDTO struct {
	// Issue is the issue identifier the rejection concerns.
	Issue string `json:"issue"`
	// Gate names the gate that rejected (e.g. "gh:pr"); empty when the
	// source record does not carry the gate kind — it is never guessed.
	Gate string `json:"gate,omitempty"`
	// Reason is the rejection reason.
	Reason string `json:"reason"`
	// At is when the rejection occurred, in RFC 3339 format; empty when
	// the source record does not carry a time — it is never guessed.
	At string `json:"at,omitempty"`
}

// IssueResponse is the DTO for GET /api/v1/{issue_identifier}.
type IssueResponse struct {
	// SchemaVersion is the version of this wire schema.
	SchemaVersion int `json:"schema_version"`
	// Daemon identifies the serving daemon.
	Daemon DaemonDTO `json:"daemon"`
	// Issue is the issue identifier requested.
	Issue string `json:"issue"`
	// State is the issue's current state name.
	State string `json:"state"`
	// Attempt is the current attempt number for the issue.
	Attempt int `json:"attempt"`
	// Session identifies the active harness session, if any.
	Session string `json:"session,omitempty"`
	// LastEventAge is the human-readable age of the last observed event;
	// "n/a" when no event has been observed.
	LastEventAge string `json:"last_event_age"`
	// LastEventAt is when the last event was observed, RFC 3339; empty
	// when no event has been observed.
	LastEventAt string `json:"last_event_at,omitempty"`
	// Summary is a short human-readable status summary.
	Summary string `json:"summary,omitempty"`
	// Phase is the operator-facing supervision phase, richer than State;
	// empty when the projection has not observed the issue.
	Phase string `json:"phase,omitempty"`
	// Turn is the current conversation turn within the attempt.
	Turn int `json:"turn,omitempty"`
	// Thread is the harness conversation the run continues.
	Thread string `json:"thread,omitempty"`
	// Workspace is the run's working directory.
	Workspace string `json:"workspace,omitempty"`
	// Workflow is the named workflow the run executes.
	Workflow string `json:"workflow,omitempty"`
	// WakeAt is when a deferred run wakes, RFC 3339; empty unless
	// deferred.
	WakeAt string `json:"wake_at,omitempty"`
	// LastGate is a bounded preview of the last gate output.
	LastGate string `json:"last_gate,omitempty"`
	// Tail is at most 4KB of the run's most recent output; full output
	// lives in artifacts, never here.
	Tail string `json:"tail,omitempty"`
	// TailTruncated reports whether the tail was cut at the 4KB bound.
	TailTruncated bool `json:"tail_truncated,omitempty"`
}

// QueueResponse is the DTO for GET /api/v1/queue.
type QueueResponse struct {
	// SchemaVersion is the version of this wire schema.
	SchemaVersion int `json:"schema_version"`
	// Daemon identifies the serving daemon.
	Daemon DaemonDTO `json:"daemon"`
	// AsOf is when the readiness projection was read from the tracker, RFC
	// 3339; empty before the daemon's first read. Rows are served from
	// that cached canonical read, never re-read per request.
	AsOf string `json:"as_of,omitempty"`
	// Rows are the readiness rows for the configured labels, ready rows
	// first. Empty (not null) when nothing is ready or blocked.
	Rows []QueueRowDTO `json:"rows"`
}

// QueueRowDTO is one readiness row: a task that is dispatchable, or blocked
// from dispatch with the reason and blockers.
type QueueRowDTO struct {
	// ID is the issue identifier.
	ID string `json:"id"`
	// Title is the issue title.
	Title string `json:"title,omitempty"`
	// Reason is why the task is ready or blocked.
	Reason string `json:"reason,omitempty"`
	// Blocked reports that the task cannot be dispatched now.
	Blocked bool `json:"blocked"`
	// Blockers name the issues blocking this one.
	Blockers []QueueBlockerDTO `json:"blockers,omitempty"`
}

// QueueBlockerDTO names one blocking issue.
type QueueBlockerDTO struct {
	// ID is the blocking issue identifier.
	ID string `json:"id"`
	// Title is the blocking issue title, when known.
	Title string `json:"title,omitempty"`
}

// HistoryResponse is the DTO for GET /api/v1/history.
type HistoryResponse struct {
	// SchemaVersion is the version of this wire schema.
	SchemaVersion int `json:"schema_version"`
	// Daemon identifies the serving daemon.
	Daemon DaemonDTO `json:"daemon"`
	// AsOf is when the history projection was read, RFC 3339.
	AsOf string `json:"as_of,omitempty"`
	// Runs are the finished and archived attempts, newest first, bounded
	// at 100 entries: terminal live rows merged with the attempts archived
	// across daemon restarts. Empty (not null) when there is none.
	Runs []HistoryRunDTO `json:"runs"`
}

// HistoryRunDTO is one archived or terminal run in the history read.
type HistoryRunDTO struct {
	// Issue is the issue identifier the attempt worked on.
	Issue string `json:"issue"`
	// Title is the issue title, when the projection carried it.
	Title string `json:"title,omitempty"`
	// Phase is the operator-facing phase the attempt ended in (e.g.
	// "stopped: gate red", "interrupted" for a run cut off by a daemon
	// restart).
	Phase string `json:"phase,omitempty"`
	// State is the coarse run state name, when known.
	State string `json:"state,omitempty"`
	// Attempt is the attempt number.
	Attempt int `json:"attempt"`
	// Outcome is how the run ended, when the record carries one.
	Outcome string `json:"outcome,omitempty"`
	// UpdatedAt is when the attempt was last observed, RFC 3339; the
	// newest-first ordering key.
	UpdatedAt string `json:"updated_at,omitempty"`
}

// RefreshResponse is the DTO for POST /api/v1/refresh.
type RefreshResponse struct {
	// SchemaVersion is the version of this wire schema.
	SchemaVersion int `json:"schema_version"`
	// Daemon identifies the serving daemon.
	Daemon DaemonDTO `json:"daemon"`
	// Accepted reports that the refresh request was accepted for processing.
	Accepted bool `json:"accepted"`
}

// ErrorResponse is the DTO for all non-2xx responses.
type ErrorResponse struct {
	// SchemaVersion is the version of this wire schema.
	SchemaVersion int `json:"schema_version"`
	// Error is a short machine-stable error code (e.g. "not_found").
	Error string `json:"error"`
	// Message is a human-readable explanation.
	Message string `json:"message"`
}
