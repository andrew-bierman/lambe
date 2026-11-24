package tracker

import (
	"encoding/json"
	"strings"
)

// Status is an issue's bd status. It is a string on the wire: marshaling
// is the plain status text, byte-identical to the untyped form it replaced.
//
// The zero value is "no status" (an omitted bd field, or "leave unchanged"
// on a write). Any other text bd reports that lambe does not know decodes
// to [StatusUnknown], so a typo or a status from a newer bd can never alias
// a real one; the original text stays available as [Issue.State].
type Status string

// The issue statuses lambe reads and writes. bd's "ready" is a
// pseudo-status (the dispatchable front), not a stored value.
const (
	// StatusReady is bd's pseudo-status selecting the dispatchable front.
	StatusReady Status = "ready"
	// StatusOpen is the open pool a claim is released back to.
	StatusOpen Status = "open"
	// StatusInProgress marks a claimed issue under active work.
	StatusInProgress Status = "in_progress"
	// StatusBlocked hides an issue from the dispatch front while keeping it
	// readable through metadata queries.
	StatusBlocked Status = "blocked"
	// StatusDeferred is bd's snoozed state; it returns to open lazily
	// on the next dispatch-front read once the defer time passes.
	StatusDeferred Status = "deferred"
	// StatusClosed is bd's terminal status.
	StatusClosed Status = "closed"
	// StatusNeedsHuman parks an issue for human attention. It is a bd
	// custom status the daemon registers (status.custom) before use.
	StatusNeedsHuman Status = "needs-human"
	// StatusUnknown is any status text lambe does not recognize. It never
	// equals a known status, so callers treat it as neither claimable nor
	// closed.
	StatusUnknown Status = "unknown"
)

// String returns the wire text of the status.
func (s Status) String() string { return string(s) }

// Known reports whether s is one of the statuses lambe understands. The
// zero value is not known.
func (s Status) Known() bool {
	switch s {
	case StatusReady, StatusOpen, StatusInProgress, StatusBlocked, StatusDeferred, StatusClosed, StatusNeedsHuman:
		return true
	case StatusUnknown:
		return false
	}
	return false
}

// ParseStatus validates status text arriving from bd (the trust boundary).
// Empty text stays the zero Status; text that is not a known status becomes
// [StatusUnknown].
func ParseStatus(raw string) Status {
	if raw == "" {
		return ""
	}
	if s := Status(raw); s.Known() {
		return s
	}
	return StatusUnknown
}

// UnmarshalJSON parses through [ParseStatus] so every bd JSON decode
// validates once, at the boundary.
func (s *Status) UnmarshalJSON(data []byte) error {
	var raw string
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*s = ParseStatus(raw)
	return nil
}

// joinStatuses renders statuses as bd's comma-separated --status value.
func joinStatuses(ss []Status) string {
	parts := make([]string, len(ss))
	for i, s := range ss {
		parts[i] = string(s)
	}
	return strings.Join(parts, ",")
}

// State is the provider-native state name of an issue, kept verbatim from
// the tracker (spec §4.1.1): unlike [Status] it is open-ended, so it is a
// distinct type but carries no closed set. For bd it is the raw status text.
type State string

// StateUnknown is the zero State: the tracker supplied no state name.
const StateUnknown State = ""

// String returns the state name as the tracker reported it.
func (s State) String() string { return string(s) }

// Status classifies the provider-native name as one of lambe's statuses.
func (s State) Status() Status { return ParseStatus(string(s)) }
