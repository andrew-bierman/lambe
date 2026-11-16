package rex

import (
	"encoding/base64"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Status is one OSC 7501 program status badge Rex understands.
type Status int

const (
	// StatusIdle means the program has no active work.
	StatusIdle Status = iota
	// StatusWorking means the program is actively doing work.
	StatusWorking
	// StatusWaitingOnUser means the program needs human input to proceed.
	StatusWaitingOnUser
	// StatusFinished means the current unit of work completed.
	StatusFinished
	// StatusFailed means the current unit of work failed.
	StatusFailed
)

// String returns the wire name used in the OSC 7501 payload.
func (s Status) String() string {
	switch s {
	case StatusIdle:
		return "idle"
	case StatusWorking:
		return "working"
	case StatusWaitingOnUser:
		return "blocked"
	case StatusFinished:
		return "done"
	case StatusFailed:
		return "error"
	default:
		return "unknown"
	}
}

// RunState is lambe's typed supervised-run state, mirrored locally so this
// package stays independent of the supervisor loop.
type RunState int

const (
	// RunStateIdle means no run is in flight.
	RunStateIdle RunState = iota
	// RunStateRunning means an agent turn is in flight.
	RunStateRunning
	// RunStateDeferred means the supervisor deferred the run pending a
	// wake time or human decision.
	RunStateDeferred
	// RunStateSettled means the agent settled: no automatic work remains.
	RunStateSettled
	// RunStateWatchdogKilled means the silence/deadline watchdog killed
	// the agent process group.
	RunStateWatchdogKilled
)

// String returns the state's name for logs and test messages.
func (s RunState) String() string {
	switch s {
	case RunStateIdle:
		return "idle"
	case RunStateRunning:
		return "running"
	case RunStateDeferred:
		return "deferred"
	case RunStateSettled:
		return "settled"
	case RunStateWatchdogKilled:
		return "watchdog_killed"
	default:
		return "unknown"
	}
}

// MapRunState translates a lambe run state into its Rex status badge.
func MapRunState(s RunState) Status {
	switch s {
	case RunStateRunning:
		return StatusWorking
	case RunStateDeferred:
		return StatusIdle
	case RunStateSettled:
		return StatusFinished
	case RunStateWatchdogKilled:
		return StatusFailed
	default:
		return StatusIdle
	}
}

// EmitStatus writes one OSC 7501 escape sequence carrying the status badge
// and a human-readable text line. Control bytes that would terminate or
// corrupt the sequence (ESC, BEL, CR, newline) are stripped or flattened so
// the payload always stays a single field.
func EmitStatus(w io.Writer, status Status, text string) error {
	text = sanitizeText(text)
	for len(text) > 2048 {
		_, size := utf8.DecodeLastRuneInString(text)
		text = text[:len(text)-size]
	}
	seq := "\x1b]7501;state=" + status.String() + ":app=lambe:msg=" + base64.StdEncoding.EncodeToString([]byte(text)) + "\x1b\\"
	_, err := io.WriteString(w, seq)
	return err
}

// sanitizeText makes arbitrary text safe to embed in an OSC payload.
func sanitizeText(text string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' {
			return ' '
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, text)
}
