// Package httpstate exposes the Symphony spec §13.7 optional read-only HTTP
// state surface. See dto.go for the package overview.
package httpstate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

// Run is one supervised run as reported by the state source.
type Run struct {
	// Issue is the issue identifier the run is working on.
	Issue string
	// Session identifies the harness session, if any.
	Session string
	// Attempt is the attempt number for this issue, starting at 1.
	Attempt int
	// State is the run state name (e.g. "running", "waiting", "stopped").
	State string
	// LastEvent is when the last event for this run was observed; zero when
	// no event has been observed.
	LastEvent time.Time
	// Phase is the operator-facing supervision phase (e.g. "running",
	// "gating", "stopped: gate red"); empty when not observed.
	Phase string
	// Workflow is the named workflow the run executes; empty when unknown.
	Workflow string
	// Machine and Actor are the daemon identity that owns the run.
	Machine string
	Actor   string
	// LastActivity is the last agent event of any kind; zero when none.
	LastActivity time.Time
	// Stalled is the source's verdict that a running run has had no
	// activity for the configured stall threshold.
	Stalled bool
}

// NeedsHuman is one item awaiting human attention.
type NeedsHuman struct {
	// Issue is the issue identifier the item concerns.
	Issue string
	// Reason is why the item needs a human: the recorded escalation reason
	// or its record-derived fallback. Never a placeholder.
	Reason string
	// Detail is a human-readable explanation.
	Detail string
	// Since is when the item was raised; zero when unknown.
	Since time.Time
}

// GateRejection records one verified-completion gate rejection. Gate and At
// reflect only what the source record actually carries: zero/empty means
// unknown, and sources must not guess.
type GateRejection struct {
	// Issue is the issue identifier the rejection concerns.
	Issue string
	// Gate names the gate that rejected (e.g. "gh:pr"); empty when the
	// record does not carry the gate kind.
	Gate string
	// Reason is the rejection reason.
	Reason string
	// At is when the rejection occurred; zero when the record carries no
	// time.
	At time.Time
}

// Daemon identifies the serving daemon. Sources fill what they know.
type Daemon struct {
	// PID is the serving process id.
	PID int
	// Identity identifies this process incarnation; empty when the
	// platform cannot provide one.
	Identity string
	// Machine is the daemon's configured dispatch identity.
	Machine string
	// Version is the daemon build version.
	Version string
	// ConfigPath is the configuration file the daemon loaded.
	ConfigPath string
	// Actor is the daemon's tracker identity for claims.
	Actor string
}

// QueueRow is one readiness row in the queue read: a task that is
// dispatchable, or blocked from dispatch with the reason and blockers.
type QueueRow struct {
	ID       string
	Title    string
	Reason   string
	Blocked  bool
	Blockers []QueueBlocker
}

// QueueBlocker names one blocking issue.
type QueueBlocker struct {
	ID    string
	Title string
}

// Queue is the cached readiness view served at /api/v1/queue. AsOf is when
// the source read the tracker; zero before the first read.
type Queue struct {
	AsOf time.Time
	Rows []QueueRow
}

// HistoryRun is one terminal or archived run in the history read.
type HistoryRun struct {
	Issue     string
	Title     string
	Phase     string
	State     string
	Attempt   int
	Outcome   string
	UpdatedAt time.Time
	// Attempts lists every known attempt of the issue, newest first,
	// including this headline row as its first entry. Empty from sources
	// that do not group.
	Attempts []HistoryAttempt
}

// HistoryAttempt is one attempt inside a grouped history row.
type HistoryAttempt struct {
	Attempt   int
	Phase     string
	State     string
	Outcome   string
	UpdatedAt time.Time
}

// History is the merged terminal-and-archived view served at
// /api/v1/history: newest first, bounded at historyBound entries. AsOf is
// when the source built the merge.
type History struct {
	AsOf time.Time
	Runs []HistoryRun
}

// Snapshot is the full supervised state as reported by the state source.
type Snapshot struct {
	// Runs are the supervised runs, in report order.
	Runs []Run
	// DeferredUntil is the time the supervisor deferred its next action;
	// zero when not deferred.
	DeferredUntil time.Time
	// WakeAt is the time the supervisor will wake to re-evaluate state;
	// zero when unset.
	WakeAt time.Time
	// NeedsHuman lists items awaiting human attention.
	NeedsHuman []NeedsHuman
	// GateRejections is the recent gate rejection history, oldest first.
	GateRejections []GateRejection
	// RunsAsOf is when the runs section was read; zero when unknown.
	RunsAsOf time.Time
	// TickAt is the daemon loop heartbeat; zero when unknown.
	TickAt time.Time
	// StallAfter is the stall threshold behind every Run.Stalled; zero when
	// unknown.
	StallAfter time.Duration
	// NeedsHumanAsOf is when the needs-human section was read from the
	// tracker; zero before the first read.
	NeedsHumanAsOf time.Time
	// GateRejectionsAsOf is when the gate-rejection section was read; zero
	// before the first read.
	GateRejectionsAsOf time.Time
}

// IssueState is the per-issue detail returned by Issue. Every field beyond
// the §13.7 core is additive: a source that cannot observe it leaves the
// zero value, and the server omits it.
type IssueState struct {
	// State is the issue's current state name.
	State string
	// Attempt is the current attempt number for the issue.
	Attempt int
	// Session identifies the active harness session, if any.
	Session string
	// LastEvent is when the last event was observed; zero when none.
	LastEvent time.Time
	// Summary is a short human-readable status summary.
	Summary string
	// Phase is the operator-facing supervision phase; empty when not
	// observed.
	Phase string
	// Turn is the current conversation turn within the attempt.
	Turn int
	// Thread is the harness conversation the run continues.
	Thread string
	// Workspace is the run's working directory.
	Workspace string
	// Workflow is the named workflow the run executes.
	Workflow string
	// WakeAt is when a deferred run wakes; zero unless deferred.
	WakeAt time.Time
	// LastGate is a bounded preview of the last gate output.
	LastGate string
	// Tail is the run's bounded recent output tail.
	Tail string
	// Machine and Actor are the daemon identity that owns the run.
	Machine string
	Actor   string
	// LastActivity is the last agent event of any kind; zero when none.
	LastActivity time.Time
	// Stalled and StallAfter mirror the snapshot's server-side verdict.
	Stalled    bool
	StallAfter time.Duration
}

// StateSource is the narrow interface httpstate reads from. The daemon is the
// intended implementor: it wires its live supervision state into these
// methods. Implementations must be safe for concurrent use; httpstate serves
// each request on its own goroutine.
type StateSource interface {
	// Snapshot returns the full supervised state.
	Snapshot() Snapshot
	// Issue returns the state of a single issue, identified by its
	// issue identifier. The second return is false when the issue is unknown.
	Issue(id string) (IssueState, bool)
	// Queue returns the cached readiness projection for the configured
	// labels.
	Queue() Queue
	// History returns the merged terminal-and-archived runs.
	History() History
	// Identity returns the serving daemon's identity facts.
	Identity() Daemon
	// Refresh asks the supervisor to re-evaluate its state promptly. It is
	// advisory and must not block on the re-evaluation completing.
	Refresh()
}

// Server serves the read-only state HTTP surface on a listener the caller
// owns. Use New to construct one with defaults; the daemon wires the
// StateSource and owns the lifecycle via Start/Shutdown.
type Server struct {
	source StateSource
	http   *http.Server
	ln     net.Listener
	log    func(format string, args ...any)
}

// New creates a Server serving source over the given listener. The listener
// must already be bound; loopback binding is the caller's choice (see
// ListenLoopback). The server is not started until Start is called.
func New(source StateSource, ln net.Listener) *Server {
	s := &Server{source: source, ln: ln}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/state", s.handleState)
	mux.HandleFunc("GET /api/v1/queue", s.handleQueue)
	mux.HandleFunc("GET /api/v1/history", s.handleHistory)
	mux.HandleFunc("GET /api/v1/{issue}", s.handleIssue)
	mux.HandleFunc("POST /api/v1/refresh", s.handleRefresh)
	mux.HandleFunc("GET /api/v1/refresh", s.methodNotAllowed)
	mux.HandleFunc("POST /api/v1/state", s.methodNotAllowed)
	mux.HandleFunc("POST /api/v1/queue", s.methodNotAllowed)
	mux.HandleFunc("POST /api/v1/history", s.methodNotAllowed)
	mux.HandleFunc("/", s.jsonNotFound)
	s.http = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	return s
}

// Listen binds the given TCP host:port address for the state surface. It
// is ListenLoopback with an explicit address; port 0 selects an ephemeral
// port. The daemon's configuration validation is responsible for
// restricting the address to loopback before this is called.
func Listen(addr string) (net.Listener, error) {
	return net.Listen("tcp", addr)
}

// ListenLoopback binds a loopback listener on an ephemeral port and returns
// it. It is the recommended default binding for the state surface.
func ListenLoopback() (net.Listener, error) {
	return Listen("127.0.0.1:0")
}

// Addr returns the bound address of the server's listener.
func (s *Server) Addr() net.Addr { return s.ln.Addr() }

// SetLogger sets an optional printf-style error logger for the underlying
// http.Server. It must be called before Start.
func (s *Server) SetLogger(log func(format string, args ...any)) { s.log = log }

// Wrap wraps the server's route handler with middleware. It must be called
// before Start. The daemon installs the bearer-auth middleware here, so
// every route — including the refresh POST — is guarded while the route
// handlers and DTOs stay untouched behind the middleware.
func (s *Server) Wrap(mw func(http.Handler) http.Handler) {
	s.http.Handler = mw(s.http.Handler)
}

// Start serves requests on the bound listener, blocking until the server is
// shut down or the listener fails. Start always returns a non-nil error.
func (s *Server) Start() error {
	if s.log != nil {
		s.http.ErrorLog = log.New(stdWriter(s.log), "", 0)
	}
	err := s.http.Serve(s.ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Shutdown gracefully stops the server, waiting for in-flight requests or
// the context to complete, whichever happens first.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.http.Shutdown(ctx)
}

func (s *Server) handleState(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, NewSnapshotResponse(s.source.Snapshot(), s.source.Identity(), time.Now()))
}

// NewSnapshotResponse converts a state snapshot and the serving identity to
// the GET /api/v1/state wire DTO. It is the one conversion: the daemon's
// route and `lambe status -json` both use it, so the two shapes cannot
// drift. now anchors the human age strings.
func NewSnapshotResponse(snap Snapshot, id Daemon, now time.Time) SnapshotResponse {
	resp := SnapshotResponse{
		SchemaVersion:      SchemaVersion,
		Daemon:             daemonDTO(id),
		Runs:               []RunDTO{}, // empty lists encode as [], never null
		NeedsHuman:         []NeedsHumanDTO{},
		GateRejections:     []GateRejectionDTO{},
		DeferredUntil:      formatTime(snap.DeferredUntil),
		WakeAt:             formatTime(snap.WakeAt),
		RunsAsOf:           formatTime(snap.RunsAsOf),
		NeedsHumanAsOf:     formatTime(snap.NeedsHumanAsOf),
		GateRejectionsAsOf: formatTime(snap.GateRejectionsAsOf),
		TickAt:             formatTime(snap.TickAt),
		StallAfterSeconds:  int(snap.StallAfter.Seconds()),
	}
	for _, run := range snap.Runs {
		resp.Runs = append(resp.Runs, RunDTO{
			Issue:          run.Issue,
			Session:        run.Session,
			Attempt:        run.Attempt,
			State:          run.State,
			LastEventAge:   formatAge(run.LastEvent, now),
			LastEventAt:    formatTime(run.LastEvent),
			Phase:          run.Phase,
			Workflow:       run.Workflow,
			Machine:        run.Machine,
			Actor:          run.Actor,
			LastActivityAt: formatTime(run.LastActivity),
			Stalled:        run.Stalled,
		})
	}
	for _, nh := range snap.NeedsHuman {
		resp.NeedsHuman = append(resp.NeedsHuman, NeedsHumanDTO{
			Issue:  nh.Issue,
			Reason: nh.Reason,
			Detail: nh.Detail,
			Since:  formatTime(nh.Since),
		})
	}
	for _, gr := range snap.GateRejections {
		resp.GateRejections = append(resp.GateRejections, GateRejectionDTO{
			Issue:  gr.Issue,
			Gate:   gr.Gate,
			Reason: gr.Reason,
			At:     formatTime(gr.At),
		})
	}
	return resp
}

// handleQueue serves the cached readiness projection; it never touches the
// tracker, so repeated calls cost one cache read.
func (s *Server) handleQueue(w http.ResponseWriter, _ *http.Request) {
	queue := s.source.Queue()
	resp := QueueResponse{
		SchemaVersion: SchemaVersion,
		Daemon:        daemonDTO(s.source.Identity()),
		AsOf:          formatTime(queue.AsOf),
		Rows:          make([]QueueRowDTO, 0, len(queue.Rows)),
	}
	for _, row := range queue.Rows {
		qr := QueueRowDTO{
			ID:      row.ID,
			Title:   row.Title,
			Reason:  row.Reason,
			Blocked: row.Blocked,
		}
		for _, b := range row.Blockers {
			qr.Blockers = append(qr.Blockers, QueueBlockerDTO(b))
		}
		resp.Rows = append(resp.Rows, qr)
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleHistory serves the merged terminal-and-archived runs.
func (s *Server) handleHistory(w http.ResponseWriter, _ *http.Request) {
	history := s.source.History()
	resp := HistoryResponse{
		SchemaVersion: SchemaVersion,
		Daemon:        daemonDTO(s.source.Identity()),
		AsOf:          formatTime(history.AsOf),
		Runs:          make([]HistoryRunDTO, 0, len(history.Runs)),
	}
	for _, run := range history.Runs {
		hr := HistoryRunDTO{
			Issue:     run.Issue,
			Title:     run.Title,
			Phase:     run.Phase,
			State:     run.State,
			Attempt:   run.Attempt,
			Outcome:   run.Outcome,
			UpdatedAt: formatTime(run.UpdatedAt),
		}
		hr.AttemptCount = max(1, len(run.Attempts))
		for _, a := range run.Attempts {
			hr.Attempts = append(hr.Attempts, HistoryAttemptDTO{
				Attempt:   a.Attempt,
				Phase:     a.Phase,
				State:     a.State,
				Outcome:   a.Outcome,
				UpdatedAt: formatTime(a.UpdatedAt),
			})
		}
		resp.Runs = append(resp.Runs, hr)
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleIssue(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("issue")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{
			SchemaVersion: SchemaVersion,
			Error:         "bad_request",
			Message:       "missing issue identifier",
		})
		return
	}
	st, ok := s.source.Issue(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, ErrorResponse{
			SchemaVersion: SchemaVersion,
			Error:         "not_found",
			Message:       fmt.Sprintf("unknown issue %q", id),
		})
		return
	}
	tail, truncated := boundTail(st.Tail)
	writeJSON(w, http.StatusOK, IssueResponse{
		SchemaVersion:     SchemaVersion,
		Daemon:            daemonDTO(s.source.Identity()),
		Issue:             id,
		State:             st.State,
		Attempt:           st.Attempt,
		Session:           st.Session,
		LastEventAge:      formatAge(st.LastEvent, time.Now()),
		LastEventAt:       formatTime(st.LastEvent),
		Summary:           st.Summary,
		Phase:             st.Phase,
		Turn:              st.Turn,
		Thread:            st.Thread,
		Workspace:         st.Workspace,
		Workflow:          st.Workflow,
		WakeAt:            formatTime(st.WakeAt),
		LastGate:          st.LastGate,
		Tail:              tail,
		TailTruncated:     truncated,
		Machine:           st.Machine,
		Actor:             st.Actor,
		LastActivityAt:    formatTime(st.LastActivity),
		Stalled:           st.Stalled,
		StallAfterSeconds: int(st.StallAfter.Seconds()),
	})
}

func (s *Server) handleRefresh(w http.ResponseWriter, _ *http.Request) {
	s.source.Refresh()
	writeJSON(w, http.StatusAccepted, RefreshResponse{
		SchemaVersion: SchemaVersion,
		Daemon:        daemonDTO(s.source.Identity()),
		Accepted:      true,
	})
}

// methodNotAllowed answers a known endpoint reached with the wrong method,
// so the error still uses the ErrorResponse DTO.
func (s *Server) methodNotAllowed(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusMethodNotAllowed, ErrorResponse{
		SchemaVersion: SchemaVersion,
		Error:         "method_not_allowed",
		Message:       "wrong method for this endpoint",
	})
}

// jsonNotFound is the catch-all: every unmatched route gets the
// ErrorResponse DTO rather than the net/http plaintext 404.
func (s *Server) jsonNotFound(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusNotFound, ErrorResponse{
		SchemaVersion: SchemaVersion,
		Error:         "not_found",
		Message:       "unknown route",
	})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(body)
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// daemonDTO converts the source's identity facts to the wire form.
func daemonDTO(d Daemon) DaemonDTO { return DaemonDTO(d) }

// boundTail caps a tail at maxTailBytes, cutting at the trailing bound and
// reporting whether anything was cut. The source is expected to bound the
// tail; this enforces the contract locally even if a source misbehaves.
func boundTail(tail string) (string, bool) {
	if len(tail) <= maxTailBytes {
		return tail, false
	}
	return trimToRuneStart(tail[len(tail)-maxTailBytes:]), true
}

// trimToRuneStart drops leading continuation bytes so a truncated tail
// begins on a UTF-8 rune boundary instead of surfacing mojibake.
func trimToRuneStart(s string) string {
	for len(s) > 0 {
		r, size := utf8.DecodeRuneInString(s)
		if r != utf8.RuneError || size != 1 {
			return s
		}
		s = s[size:]
	}
	return s
}

func formatAge(t, now time.Time) string {
	if t.IsZero() {
		return "n/a"
	}
	return strings.TrimSpace(roundDuration(now.Sub(t)).String())
}

// roundDuration clamps negative ages to zero and rounds to whole seconds.
func roundDuration(d time.Duration) time.Duration {
	if d < 0 {
		return 0
	}
	return d.Round(time.Second)
}

// stdWriter adapts a printf-style function to io.Writer for use as an
// http.Server error log sink.
type stdWriter func(format string, args ...any)

func (f stdWriter) Write(p []byte) (int, error) {
	f("%s", strings.TrimRight(string(p), "\n"))
	return len(p), nil
}
