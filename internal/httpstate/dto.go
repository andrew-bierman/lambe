// Package httpstate exposes the Symphony spec §13.7 optional read-only HTTP
// state surface: GET /api/v1/state, GET /api/v1/{issue_identifier}, and
// POST /api/v1/refresh. It is stdlib-only and intended to be wired to the
// daemon's state source.
package httpstate

// SchemaVersion is the wire schema version carried by every response DTO.
const SchemaVersion = 1

// SnapshotResponse is the DTO for GET /api/v1/state.
type SnapshotResponse struct {
	// SchemaVersion is the version of this wire schema.
	SchemaVersion int `json:"schema_version"`
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
}

// NeedsHumanDTO describes one item awaiting human attention.
type NeedsHumanDTO struct {
	// Issue is the issue identifier the item concerns.
	Issue string `json:"issue"`
	// Reason is a short machine-stable reason code.
	Reason string `json:"reason"`
	// Detail is a human-readable explanation.
	Detail string `json:"detail,omitempty"`
	// Since is when the item was raised, in RFC 3339 format.
	Since string `json:"since,omitempty"`
}

// GateRejectionDTO records one verified-completion gate rejection.
type GateRejectionDTO struct {
	// Issue is the issue identifier the rejection concerns.
	Issue string `json:"issue"`
	// Gate names the gate that rejected (e.g. "gh:pr").
	Gate string `json:"gate"`
	// Reason is the rejection reason.
	Reason string `json:"reason"`
	// At is when the rejection occurred, in RFC 3339 format.
	At string `json:"at"`
}

// IssueResponse is the DTO for GET /api/v1/{issue_identifier}.
type IssueResponse struct {
	// SchemaVersion is the version of this wire schema.
	SchemaVersion int `json:"schema_version"`
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
	// Summary is a short human-readable status summary.
	Summary string `json:"summary,omitempty"`
}

// RefreshResponse is the DTO for POST /api/v1/refresh.
type RefreshResponse struct {
	// SchemaVersion is the version of this wire schema.
	SchemaVersion int `json:"schema_version"`
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
