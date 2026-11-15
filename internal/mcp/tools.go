package mcp

import (
	"context"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// maxTailBytes is the hard cap on transcript tail returned by lambe_output.
// Full transcripts live in artifact files, referenced by path.
const maxTailBytes = 4096

// Tool names, exported so daemon and CLI wiring can reference them.
const (
	ToolSpawn  = "lambe_spawn"
	ToolStatus = "lambe_status"
	ToolOutput = "lambe_output"
	ToolCancel = "lambe_cancel"
	ToolList   = "lambe_list"
)

// spawnArgs is the input contract of lambe_spawn.
type spawnArgs struct {
	// IssueID is the tracker issue the run works on.
	IssueID string `json:"issue_id" jsonschema:"the tracker issue id the supervised run works on"`
	// Prompt is the initial prompt handed to the spawned agent.
	Prompt string `json:"prompt" jsonschema:"the initial prompt for the supervised agent"`
	// Workspace is the working directory for the run.
	Workspace string `json:"workspace,omitempty" jsonschema:"working directory for the run"`
}

// spawnResult is the immediate reply of lambe_spawn.
type spawnResult struct {
	// Handle identifies the run in later lambe_status/output/cancel calls.
	Handle string `json:"handle"`
	// IssueID echoes the issue the run works on.
	IssueID string `json:"issue_id"`
	// Status is the run's initial state name.
	Status string `json:"status"`
}

// statusArgs is the input contract of lambe_status.
type statusArgs struct {
	// Handle is the run handle from lambe_spawn.
	Handle string `json:"handle" jsonschema:"run handle returned by lambe_spawn"`
}

// statusResult is the reply of lambe_status.
type statusResult struct {
	// Handle echoes the queried handle.
	Handle string `json:"handle"`
	// IssueID is the issue the run works on, when known.
	IssueID string `json:"issue_id,omitempty"`
	// State is the run state name: pending, running, deferred, settled,
	// failed, or canceled.
	State string `json:"state"`
	// Attempt is the current attempt number, starting at 1.
	Attempt int `json:"attempt"`
	// LastEventAgeSeconds is how long ago the run last produced an event.
	LastEventAgeSeconds int64 `json:"last_event_age_seconds,omitempty"`
	// WakeAt is the RFC3339 wake time, present only for deferred runs.
	WakeAt string `json:"wake_at,omitempty"`
	// Outcome describes how a finished run ended; empty while running.
	Outcome string `json:"outcome,omitempty"`
}

// outputArgs is the input contract of lambe_output.
type outputArgs struct {
	// Handle is the run handle from lambe_spawn.
	Handle string `json:"handle" jsonschema:"run handle returned by lambe_spawn"`
}

// artifactInfo is one artifact file reference in an outputResult.
type artifactInfo struct {
	// Path is the absolute file path to read for the full output.
	Path string `json:"path"`
	// Bytes is the file size.
	Bytes int64 `json:"bytes"`
	// Kind names the artifact, e.g. transcript or diff.
	Kind string `json:"kind"`
}

// outputResult is the reply of lambe_output.
type outputResult struct {
	// Handle echoes the queried handle.
	Handle string `json:"handle"`
	// Artifacts are the run's output files; read them for full content.
	Artifacts []artifactInfo `json:"artifacts"`
	// Tail is at most 4KB of the run's most recent output.
	Tail string `json:"tail,omitempty"`
	// Truncated reports whether the tail was cut at the 4KB bound.
	Truncated bool `json:"truncated"`
}

// cancelArgs is the input contract of lambe_cancel.
type cancelArgs struct {
	// Handle is the run handle from lambe_spawn.
	Handle string `json:"handle" jsonschema:"run handle returned by lambe_spawn"`
}

// cancelResult is the reply of lambe_cancel.
type cancelResult struct {
	// Handle echoes the canceled handle.
	Handle string `json:"handle"`
	// State is the run state after the cancel attempt.
	State string `json:"state"`
	// Outcome describes what the cancel did.
	Outcome string `json:"outcome,omitempty"`
}

// listResult is the reply of lambe_list.
type listResult struct {
	// Runs is the current snapshot of all supervised runs.
	Runs []statusResult `json:"runs"`
}

// registerTools wires the five lambe tools onto the SDK server. Tool
// descriptions are the agent-facing contract and say the same things the
// package doc does: spawn returns fast, output never inlines transcripts.
func (s *Server) registerTools() {
	mcp.AddTool(s.sdk, &mcp.Tool{
		Name:        ToolSpawn,
		Description: "Spawn a new SUPERVISED coding-agent run and return immediately. Returns {handle, issue_id, status} without waiting for the run: poll lambe_status with the handle. Runs are supervised by the lambe daemon (attempt caps, watchdogs, verified-completion gates); this server only proxies. Trust note: an agent that is itself lambe-supervised (LAMBE_DEPTH >= 1) cannot spawn further runs unless depth is configured higher — this bounds prompt-injection recursion.",
	}, s.spawn)

	mcp.AddTool(s.sdk, &mcp.Tool{
		Name:        ToolStatus,
		Description: "Get the current status of a supervised run: state (pending/running/deferred/settled/failed/canceled), attempt number, seconds since the run's last event, and the RFC3339 wake time when the run is deferred (e.g. waiting out a provider limit).",
	}, s.status)

	mcp.AddTool(s.sdk, &mcp.Tool{
		Name:        ToolOutput,
		Description: "Get a supervised run's output: paths to full artifact files (transcripts, diffs) plus at most a 4KB tail of recent output. Never returns full transcripts inline — read the artifact paths for complete content.",
	}, s.output)

	mcp.AddTool(s.sdk, &mcp.Tool{
		Name:        ToolCancel,
		Description: "Best-effort cancel of a supervised run. Returns the run's state and outcome after the cancel was applied; a run may still report a non-canceled state if it finished before the cancel landed.",
	}, s.cancel)

	mcp.AddTool(s.sdk, &mcp.Tool{
		Name:        ToolList,
		Description: "Snapshot of all supervised runs: handle, issue, state, and attempt for each.",
	}, s.list)
}

// spawn implements lambe_spawn: depth-check, register, return the handle.
func (s *Server) spawn(ctx context.Context, _ *mcp.CallToolRequest, args spawnArgs) (*mcp.CallToolResult, spawnResult, error) {
	if args.IssueID == "" {
		return nil, spawnResult{}, fmt.Errorf("issue_id is required")
	}
	runDepth, err := s.guard.check(s.depth)
	if err != nil {
		return nil, spawnResult{}, err
	}
	handle, err := s.control.SpawnRun(ctx, RunSpec{
		IssueID:   args.IssueID,
		Prompt:    args.Prompt,
		Workspace: args.Workspace,
		Depth:     runDepth,
	})
	if err != nil {
		return nil, spawnResult{}, fmt.Errorf("daemon refused spawn: %w", err)
	}
	st, err := s.control.RunStatus(ctx, handle)
	if err != nil {
		// The run exists; report it pending rather than failing the call.
		st = RunStatus{State: StatePending}
	}
	out := spawnResult{Handle: handle.String(), IssueID: args.IssueID, Status: st.State.String()}
	if out.Status == "" {
		out.Status = StatePending.String()
	}
	return nil, out, nil
}

// parseHandle converts a tool-supplied handle string into its typed form.
func parseHandle(raw string) (RunHandle, error) {
	h, err := ParseRunHandle(raw)
	if err != nil {
		return RunHandle{}, fmt.Errorf("invalid handle: %w", err)
	}
	return h, nil
}

// status implements lambe_status.
func (s *Server) status(ctx context.Context, _ *mcp.CallToolRequest, args statusArgs) (*mcp.CallToolResult, statusResult, error) {
	h, err := parseHandle(args.Handle)
	if err != nil {
		return nil, statusResult{}, err
	}
	st, err := s.control.RunStatus(ctx, h)
	if err != nil {
		return nil, statusResult{}, err
	}
	return nil, statusFrom(h, st), nil
}

// statusFrom converts a typed snapshot into the wire form.
func statusFrom(h RunHandle, st RunStatus) statusResult {
	res := statusResult{
		Handle:  h.String(),
		State:   st.State.String(),
		Attempt: st.Attempt,
		Outcome: st.Outcome,
	}
	if !st.LastEventAt.IsZero() {
		res.LastEventAgeSeconds = int64(time.Since(st.LastEventAt).Round(time.Second) / time.Second)
	}
	if !st.WakeAt.IsZero() {
		res.WakeAt = st.WakeAt.UTC().Format(time.RFC3339)
	}
	return res
}

// output implements lambe_output with the tail bound enforced locally even
// if a control misbehaves.
func (s *Server) output(ctx context.Context, _ *mcp.CallToolRequest, args outputArgs) (*mcp.CallToolResult, outputResult, error) {
	h, err := parseHandle(args.Handle)
	if err != nil {
		return nil, outputResult{}, err
	}
	out, err := s.control.RunOutput(ctx, h)
	if err != nil {
		return nil, outputResult{}, err
	}
	res := outputResult{Handle: h.String(), Artifacts: make([]artifactInfo, 0, len(out.Artifacts))}
	for _, a := range out.Artifacts {
		res.Artifacts = append(res.Artifacts, artifactInfo(a))
	}
	res.Tail = out.Tail
	if len(res.Tail) > maxTailBytes {
		res.Tail = res.Tail[len(res.Tail)-maxTailBytes:]
		res.Truncated = true
	}
	return nil, res, nil
}

// cancel implements lambe_cancel.
func (s *Server) cancel(ctx context.Context, _ *mcp.CallToolRequest, args cancelArgs) (*mcp.CallToolResult, cancelResult, error) {
	h, err := parseHandle(args.Handle)
	if err != nil {
		return nil, cancelResult{}, err
	}
	res, err := s.control.CancelRun(ctx, h)
	if err != nil {
		return nil, cancelResult{}, err
	}
	return nil, cancelResult{Handle: h.String(), State: res.State.String(), Outcome: res.Outcome}, nil
}

// list implements lambe_list.
func (s *Server) list(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, listResult, error) {
	runs, err := s.control.ListRuns(ctx)
	if err != nil {
		return nil, listResult{}, err
	}
	res := listResult{Runs: make([]statusResult, 0, len(runs))}
	for _, r := range runs {
		sr := statusFrom(r.Handle, r.Status)
		sr.IssueID = r.IssueID
		res.Runs = append(res.Runs, sr)
	}
	return nil, res, nil
}
