// Package mcp exposes lambe's supervised runs over the Model Context
// Protocol so any MCP-capable agent can spawn, observe, and cancel runs as
// tool calls.
//
// The server is a deliberate non-owner of run state. stdio MCP servers are
// killed within roughly half a second of their client session exiting, so
// this process must never hold live runs: every tool call is a thin proxy
// into the daemon through the [Control] interface (wired by
// internal/daemon, which owns run state and supervision).
//
// Two portability constraints shape the tool contracts in tools.go:
//
//   - Tool calls must return fast (clients cap calls at ~25k tokens of
//     result and some at ~300s of wall time, and subagent MCP calls never
//     auto-background). lambe_spawn registers a run and returns a handle
//     immediately; nothing blocks on run progress.
//   - Heavy output lives in artifact files on disk. Tools return artifact
//     paths plus at most a 4KB tail, never full transcripts.
//
// Trust note: an MCP server that spawns autonomous agents amplifies prompt
// injection. lambe bounds recursion with a depth limit (see trust.go): a
// lambe-supervised agent inherits a LAMBE_DEPTH environment variable and,
// by default (max depth 1), cannot spawn further runs.
package mcp

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// RunState is the typed lifecycle state of one supervised run. Tools
// surface its String form; callers never parse free-form status text.
type RunState int

// Run states, in rough lifecycle order.
const (
	// StatePending means the run is registered but not yet started.
	StatePending RunState = iota
	// StateRunning means an agent attempt is in flight.
	StateRunning
	// StateDeferred means the run is parked awaiting a wake time, e.g. a
	// suspected provider limit.
	StateDeferred
	// StateSettled means the run finished normally.
	StateSettled
	// StateFailed means the run exhausted its attempts or errored out.
	StateFailed
	// StateCanceled means the run was canceled by a caller.
	StateCanceled
)

// String returns the wire name of the state.
func (s RunState) String() string {
	switch s {
	case StatePending:
		return "pending"
	case StateRunning:
		return "running"
	case StateDeferred:
		return "deferred"
	case StateSettled:
		return "settled"
	case StateFailed:
		return "failed"
	case StateCanceled:
		return "canceled"
	default:
		return "unknown"
	}
}

// RunHandle is an opaque, typed reference to one supervised run. It is
// created by [Control] implementations, crosses the MCP boundary as a
// string, and is parsed back with [ParseRunHandle]. Client code must not
// construct handles from arbitrary input except through ParseRunHandle.
type RunHandle struct{ id string }

// NewRunHandle wraps a daemon-issued run identifier into a typed handle.
// Only Control implementations (the daemon and test fakes) call this.
func NewRunHandle(id string) RunHandle { return RunHandle{id: id} }

// String returns the wire form of the handle.
func (h RunHandle) String() string { return h.id }

// ParseRunHandle validates a handle string received from an MCP client and
// returns its typed form. Empty strings, and strings containing whitespace
// or control characters, are rejected.
func ParseRunHandle(s string) (RunHandle, error) {
	if s == "" {
		return RunHandle{}, fmt.Errorf("empty run handle")
	}
	if strings.ContainsAny(s, " \t\r\n") {
		return RunHandle{}, fmt.Errorf("malformed run handle")
	}
	return RunHandle{s}, nil
}

// RunSpec describes one supervised run the daemon should register. Depth
// is the supervision depth the spawned agent will run at; the daemon
// exports it to the child as LAMBE_DEPTH.
type RunSpec struct {
	// IssueID identifies the tracker issue the run works on.
	IssueID string
	// Prompt is the initial prompt for the spawned agent.
	Prompt string
	// Workspace is the working directory for the run.
	Workspace string
	// Depth is the supervision depth of the spawned agent (1 for a run
	// spawned by a top-level agent).
	Depth int
}

// RunStatus is a typed snapshot of one run.
type RunStatus struct {
	// State is the run's lifecycle state.
	State RunState
	// Attempt is the current attempt number, starting at 1.
	Attempt int
	// LastEventAt is when the run last produced an event; zero when
	// unknown.
	LastEventAt time.Time
	// WakeAt is when a deferred run will wake; zero unless State is
	// StateDeferred.
	WakeAt time.Time
	// Outcome describes how a finished run ended; empty while it is in
	// flight.
	Outcome string
}

// Artifact describes one heavy output file produced by a run. Tools
// reference artifacts by path; contents are never inlined.
type Artifact struct {
	// Path is the absolute path of the artifact file.
	Path string
	// Bytes is the artifact's size in bytes.
	Bytes int64
	// Kind names the artifact kind, e.g. "transcript" or "diff".
	Kind string
}

// RunOutput is the output view of one run: artifact paths plus a bounded
// tail of the run's recent output.
type RunOutput struct {
	// Artifacts are the run's heavy output files.
	Artifacts []Artifact
	// Tail is the trailing slice of the run's combined output. Control
	// implementations should bound it to [maxTailBytes]; the server
	// enforces the bound regardless.
	Tail string
}

// CancelResult reports what a cancel did.
type CancelResult struct {
	// State is the run's state after the cancel was applied.
	State RunState
	// Outcome describes the cancel outcome, e.g. why the run ended.
	Outcome string
}

// RunInfo pairs a handle with the status of one run, for snapshots.
type RunInfo struct {
	// Handle identifies the run.
	Handle RunHandle
	// IssueID is the tracker issue the run works on.
	IssueID string
	// Status is the run's current snapshot.
	Status RunStatus
}

// Control is the daemon seam: everything the MCP server can do to runs.
// The daemon (internal/daemon) provides the implementation; tests use a
// fake. Implementations must make SpawnRun a fast registration call — it
// must never block on run progress — and may assume every method is called
// with a live context from a tool call.
type Control interface {
	// SpawnRun registers a new supervised run and returns its handle
	// immediately.
	SpawnRun(ctx context.Context, spec RunSpec) (RunHandle, error)
	// RunStatus returns the run's current typed snapshot.
	RunStatus(ctx context.Context, handle RunHandle) (RunStatus, error)
	// RunOutput returns the run's artifact paths and a bounded tail.
	RunOutput(ctx context.Context, handle RunHandle) (RunOutput, error)
	// CancelRun requests a best-effort cancel of the run.
	CancelRun(ctx context.Context, handle RunHandle) (CancelResult, error)
	// ListRuns returns a snapshot of all runs.
	ListRuns(ctx context.Context) ([]RunInfo, error)
}

// Options configures a Server. The zero value is usable: max depth
// defaults to [DefaultMaxDepth] and the environment to os.Environ.
type Options struct {
	// MaxDepth is the highest supervision depth a spawned run may run
	// at. Zero means [DefaultMaxDepth].
	MaxDepth int
	// Env is the server's environment, read for LAMBE_DEPTH to detect
	// supervised callers. Nil means os.Environ.
	Env []string
}

// Server is the lambe MCP server: five tools over a [Control], plus a
// stdio serving loop. It holds no run state.
type Server struct {
	control Control
	guard   depthGuard
	depth   int
	sdk     *mcp.Server
}

// New builds a Server over the given control. The control must be the
// daemon's implementation; nil is rejected.
func New(control Control, opts Options) *Server {
	if control == nil {
		panic("mcp: nil Control")
	}
	env := opts.Env
	if env == nil {
		env = os.Environ()
	}
	s := &Server{
		control: control,
		guard:   newDepthGuard(opts.MaxDepth),
		depth:   depthFromEnv(env),
	}
	s.sdk = mcp.NewServer(&mcp.Implementation{Name: "lambe", Version: "0.1.0"}, nil)
	s.registerTools()
	return s
}

// Connect attaches the server to a transport and returns the MCP session.
// It exists for tests; production use is [ServeStdio].
func (s *Server) Connect(ctx context.Context, t mcp.Transport, opts *mcp.ServerSessionOptions) (*mcp.ServerSession, error) {
	return s.sdk.Connect(ctx, t, opts)
}

// ServeStdio runs the JSON-RPC serving loop over the given reader and
// writer until the context is canceled or the transport closes. It blocks;
// callers typically pass os.Stdin and os.Stdout.
func (s *Server) ServeStdio(ctx context.Context, in io.Reader, out io.Writer) error {
	t := &mcp.IOTransport{Reader: io.NopCloser(in), Writer: nopWriteCloser{w: out}}
	return s.sdk.Run(ctx, t)
}

// nopWriteCloser adapts an io.Writer to the io.WriteCloser the SDK's
// IOTransport requires.
type nopWriteCloser struct{ w io.Writer }

// Write forwards to the wrapped writer.
func (c nopWriteCloser) Write(p []byte) (int, error) { return c.w.Write(p) }

// Close is a no-op; stdio lifetimes are owned by the process.
func (c nopWriteCloser) Close() error { return nil }
