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
}

// NeedsHuman is one item awaiting human attention.
type NeedsHuman struct {
	// Issue is the issue identifier the item concerns.
	Issue string
	// Reason is a short machine-stable reason code.
	Reason string
	// Detail is a human-readable explanation.
	Detail string
	// Since is when the item was raised; zero when unknown.
	Since time.Time
}

// GateRejection records one verified-completion gate rejection.
type GateRejection struct {
	// Issue is the issue identifier the rejection concerns.
	Issue string
	// Gate names the gate that rejected (e.g. "gh:pr").
	Gate string
	// Reason is the rejection reason.
	Reason string
	// At is when the rejection occurred.
	At time.Time
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
}

// IssueState is the per-issue detail returned by Issue.
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
}

// StateSource is the narrow interface httpstate reads from. The daemon is the
// intended implementor: it wires its live supervision state into these three
// methods. Implementations must be safe for concurrent use; httpstate serves
// each request on its own goroutine.
type StateSource interface {
	// Snapshot returns the full supervised state.
	Snapshot() Snapshot
	// Issue returns the state of a single issue, identified by its
	// issue identifier. The second return is false when the issue is unknown.
	Issue(id string) (IssueState, bool)
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
	mux.HandleFunc("GET /api/v1/{issue}", s.handleIssue)
	mux.HandleFunc("POST /api/v1/refresh", s.handleRefresh)
	mux.HandleFunc("GET /api/v1/refresh", s.methodNotAllowed)
	mux.HandleFunc("POST /api/v1/state", s.methodNotAllowed)
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
	snap := s.source.Snapshot()
	resp := SnapshotResponse{
		SchemaVersion: SchemaVersion,
		DeferredUntil: formatTime(snap.DeferredUntil),
		WakeAt:        formatTime(snap.WakeAt),
	}
	for _, run := range snap.Runs {
		resp.Runs = append(resp.Runs, RunDTO{
			Issue:        run.Issue,
			Session:      run.Session,
			Attempt:      run.Attempt,
			State:        run.State,
			LastEventAge: formatAge(run.LastEvent, time.Now()),
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
	writeJSON(w, http.StatusOK, IssueResponse{
		SchemaVersion: SchemaVersion,
		Issue:         id,
		State:         st.State,
		Attempt:       st.Attempt,
		Session:       st.Session,
		LastEventAge:  formatAge(st.LastEvent, time.Now()),
		Summary:       st.Summary,
	})
}

func (s *Server) handleRefresh(w http.ResponseWriter, _ *http.Request) {
	s.source.Refresh()
	writeJSON(w, http.StatusAccepted, RefreshResponse{
		SchemaVersion: SchemaVersion,
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
