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
	// RunStateBlocked means the run cannot continue until a human acts;
	// RunStateEvent.Block says why.
	RunStateBlocked
	// RunStateFailed means the run stopped and failed (for example a red
	// gate or exhausted attempts).
	RunStateFailed
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
	case RunStateBlocked:
		return "blocked"
	case RunStateFailed:
		return "failed"
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
	case RunStateWatchdogKilled, RunStateFailed:
		return StatusFailed
	case RunStateBlocked:
		return StatusWaitingOnUser
	default:
		return StatusIdle
	}
}

// Block says why a blocked run needs a human. Rex's protocol has a fixed
// set of kinds (permission, question, auth) and ignores any other value, so
// the lambe reason travels in the message and maps to the nearest kind.
type Block string

const (
	// BlockNeedsHuman means the run is parked and only a person can resume
	// it (for example after an interrupted attempt).
	BlockNeedsHuman Block = "needs-human"
	// BlockGateEscalated means the verification gate failed repeatedly and
	// the run was handed to a person for approval or repair.
	BlockGateEscalated Block = "gate-escalated"
	// BlockStalled means the run is alive but has produced no activity for
	// too long; a person should look.
	BlockStalled Block = "stalled"
)

// kind maps the reason to the protocol's `kind` value.
func (b Block) kind() string {
	switch b {
	case BlockGateEscalated:
		return "permission"
	case BlockNeedsHuman, BlockStalled:
		return "question"
	default:
		return ""
	}
}

// Report is one OSC 7501 report. The zero ID addresses the root record; a
// non-empty ID addresses the per-run record "lambe/<ID>", so several runs
// show their own badge and none overwrites another.
type Report struct {
	// Status is the badge state.
	Status Status
	// Block, with StatusWaitingOnUser only, sets the protocol `kind`.
	Block Block
	// ID names the run; it is reduced to a legal record segment.
	ID string
	// Title labels the record (the run's issue id); omitted when empty.
	Title string
	// Msg is one line saying what the run is doing or waiting for.
	Msg string
}

const (
	maxMsgBytes   = 2048
	maxTitleBytes = 192
	maxSegment    = 32
)

// recordID returns the protocol id for a run: "lambe/<segment>", with every
// byte outside [A-Za-z0-9_.+-] replaced and the segment capped at 32 bytes.
func recordID(run string) string {
	if run == "" {
		return ""
	}
	seg := []byte(run)
	for i, c := range seg {
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '.' || c == '+' || c == '-'
		if !ok {
			seg[i] = '-'
		}
	}
	if len(seg) > maxSegment {
		seg = seg[:maxSegment]
	}
	return "lambe/" + string(seg)
}

// capText trims text on a rune boundary to at most max bytes.
func capText(text string, limit int) string {
	for len(text) > limit {
		_, size := utf8.DecodeLastRuneInString(text)
		text = text[:len(text)-size]
	}
	return text
}

// Emit writes one OSC 7501 report. Control bytes that would terminate or
// corrupt the sequence (ESC, BEL, CR, newline) are stripped or flattened so
// every free-text field stays a single line.
func Emit(w io.Writer, r Report) error {
	var b strings.Builder
	b.WriteString("\x1b]7501;state=" + r.Status.String())
	if r.Status == StatusWaitingOnUser && r.Block.kind() != "" {
		b.WriteString(":kind=" + r.Block.kind())
	}
	if id := recordID(r.ID); id != "" {
		b.WriteString(":id=" + id)
	}
	b.WriteString(":app=lambe:msg=" + base64.StdEncoding.EncodeToString([]byte(capText(sanitizeText(r.Msg), maxMsgBytes))))
	if r.Title != "" {
		b.WriteString(":title=" + base64.StdEncoding.EncodeToString([]byte(capText(sanitizeText(r.Title), maxTitleBytes))))
	}
	b.WriteString("\x1b\\")
	_, err := io.WriteString(w, b.String())
	return err
}

// EmitStatus writes a root-record report: the badge and one line of text.
func EmitStatus(w io.Writer, status Status, text string) error {
	return Emit(w, Report{Status: status, Msg: text})
}

// Clear removes a run's record, or every lambe record when run is empty
// ("lambe" is the parent of every per-run id). The root record is left alone.
func Clear(w io.Writer, run string) error {
	id := recordID(run)
	if id == "" {
		id = "lambe"
	}
	_, err := io.WriteString(w, "\x1b]7501;state=clear:id="+id+"\x1b\\")
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
