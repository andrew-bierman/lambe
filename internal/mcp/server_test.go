package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakeControl is a scriptable Control double. Every call is recorded so
// tests can assert propagation, and behavior is driven by the per-run
// script fields.
type fakeControl struct {
	mu sync.Mutex

	// spawnDelay is how long SpawnRun takes to register a run.
	spawnDelay time.Duration
	// spawnErr, when non-nil, is returned by SpawnRun.
	spawnErr error

	runs     map[string]*fakeRun
	nextID   int
	spawned  []RunSpec
	canceled []RunHandle
}

// fakeRun is one scripted run inside fakeControl.
type fakeRun struct {
	handle   RunHandle
	spec     RunSpec
	status   RunStatus
	output   RunOutput
	cancelSt RunStatus
}

func newFakeControl() *fakeControl {
	return &fakeControl{runs: map[string]*fakeRun{}}
}

func (f *fakeControl) SpawnRun(_ context.Context, spec RunSpec) (RunHandle, error) {
	if f.spawnDelay > 0 {
		time.Sleep(f.spawnDelay)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.spawnErr != nil {
		return RunHandle{}, f.spawnErr
	}
	f.nextID++
	h := NewRunHandle(fmt.Sprintf("run-%d", f.nextID))
	f.spawned = append(f.spawned, spec)
	f.runs[h.String()] = &fakeRun{
		handle: h,
		spec:   spec,
		status: RunStatus{State: StateRunning, Attempt: 1, LastEventAt: time.Now()},
	}
	return h, nil
}

func (f *fakeControl) RunStatus(_ context.Context, handle RunHandle) (RunStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.runs[handle.String()]
	if !ok {
		return RunStatus{}, fmt.Errorf("unknown run %q", handle)
	}
	return r.status, nil
}

func (f *fakeControl) RunOutput(_ context.Context, handle RunHandle) (RunOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.runs[handle.String()]
	if !ok {
		return RunOutput{}, fmt.Errorf("unknown run %q", handle)
	}
	return r.output, nil
}

func (f *fakeControl) CancelRun(_ context.Context, handle RunHandle) (CancelResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.runs[handle.String()]
	if !ok {
		return CancelResult{}, fmt.Errorf("unknown run %q", handle)
	}
	f.canceled = append(f.canceled, handle)
	r.status = r.cancelSt
	return CancelResult{State: r.cancelSt.State, Outcome: r.cancelSt.Outcome}, nil
}

func (f *fakeControl) ListRuns(_ context.Context) ([]RunInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []RunInfo
	for _, r := range f.runs {
		out = append(out, RunInfo{Handle: r.handle, IssueID: r.spec.IssueID, Status: r.status})
	}
	return out, nil
}

func (f *fakeControl) setOutput(t *testing.T, handle RunHandle, out RunOutput) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs[handle.String()].output = out
}

func (f *fakeControl) setCancel(t *testing.T, handle RunHandle, st RunStatus) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs[handle.String()].cancelSt = st
}

func (f *fakeControl) setStatus(t *testing.T, handle RunHandle, st RunStatus) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs[handle.String()].status = st
}

// harness wires a Server to an in-memory MCP client.
type harness struct {
	t  *testing.T
	cs *mcp.ClientSession
	fc *fakeControl
}

func newHarness(t *testing.T, opts Options) *harness {
	t.Helper()
	ctx := t.Context()
	st, ct := mcp.NewInMemoryTransports()
	fc := newFakeControl()
	srv := New(fc, opts)
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	return &harness{t: t, cs: cs, fc: fc}
}

// call invokes one tool and decodes its structured output into out.
func (h *harness) call(tool string, args any, out any) *mcp.CallToolResult {
	h.t.Helper()
	res, err := h.cs.CallTool(h.t.Context(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		h.t.Fatalf("call %s: %v", tool, err)
	}
	if out != nil && res.StructuredContent != nil {
		b, err := json.Marshal(res.StructuredContent)
		if err != nil {
			h.t.Fatalf("marshal structured content: %v", err)
		}
		if err := json.Unmarshal(b, out); err != nil {
			h.t.Fatalf("unmarshal structured content: %v", err)
		}
	}
	return res
}

// TestSpawnReturnsFast is the return-fast anchor: even when registering a
// run takes a quarter second and the run itself is long-lived, the tool
// call must return well inside a second with a usable handle.
func TestSpawnReturnsFast(t *testing.T) {
	h := newHarness(t, Options{})
	h.fc.spawnDelay = 250 * time.Millisecond

	start := time.Now()
	var out spawnResult
	res := h.call("lambe_spawn", spawnArgs{IssueID: "iss-1", Prompt: "do the thing", Workspace: "/tmp/w"}, &out)
	elapsed := time.Since(start)

	if res.IsError {
		t.Fatalf("lambe_spawn errored: %v", res.Content)
	}
	if elapsed >= time.Second {
		t.Errorf("lambe_spawn took %v; want well under 1s (never blocks on the run)", elapsed)
	}
	if out.Handle == "" || out.IssueID != "iss-1" || out.Status != StateRunning.String() {
		t.Errorf("spawn result = %+v; want handle, issue id, and initial status", out)
	}
}

// TestStatusReflectsTransitions checks lambe_status across state changes,
// including attempt numbers and the deferred wake time.
func TestStatusReflectsTransitions(t *testing.T) {
	h := newHarness(t, Options{})
	var out spawnResult
	h.call("lambe_spawn", spawnArgs{IssueID: "iss-2", Prompt: "p"}, &out)

	wake := time.Now().Add(5 * time.Minute).UTC().Truncate(time.Second)
	h.fc.setStatus(t, mustHandle(t, out.Handle), RunStatus{
		State:       StateDeferred,
		Attempt:     2,
		LastEventAt: time.Now().Add(-90 * time.Second),
		WakeAt:      wake,
		Outcome:     "",
	})

	var st statusResult
	res := h.call("lambe_status", statusArgs{Handle: out.Handle}, &st)
	if res.IsError {
		t.Fatalf("lambe_status errored: %v", res.Content)
	}
	if st.State != StateDeferred.String() || st.Attempt != 2 {
		t.Errorf("status = %+v; want deferred attempt 2", st)
	}
	if st.WakeAt != wake.Format(time.RFC3339) {
		t.Errorf("wake_at = %q; want %q", st.WakeAt, wake.Format(time.RFC3339))
	}
	if st.LastEventAgeSeconds < 89 || st.LastEventAgeSeconds > 91 {
		t.Errorf("last_event_age_seconds = %d; want ~90", st.LastEventAgeSeconds)
	}
}

// TestOutputTailBound checks lambe_output returns artifact paths and never
// more than the 4KB tail, even when the control misbehaves.
func TestOutputTailBound(t *testing.T) {
	h := newHarness(t, Options{})
	var out spawnResult
	h.call("lambe_spawn", spawnArgs{IssueID: "iss-3", Prompt: "p"}, &out)
	handle := mustHandle(t, out.Handle)

	h.fc.setOutput(t, handle, RunOutput{
		Artifacts: []Artifact{
			{Path: "/tmp/artifacts/iss-3/transcript.jsonl", Bytes: 1 << 20, Kind: "transcript"},
			{Path: "/tmp/artifacts/iss-3/diff.patch", Bytes: 4096, Kind: "diff"},
		},
		Tail: strings.Repeat("x", 10*1024), // control violating the bound
	})

	var resOut outputResult
	res := h.call("lambe_output", outputArgs{Handle: out.Handle}, &resOut)
	if res.IsError {
		t.Fatalf("lambe_output errored: %v", res.Content)
	}
	if len(resOut.Tail) > maxTailBytes {
		t.Errorf("tail length = %d; want <= %d", len(resOut.Tail), maxTailBytes)
	}
	if !resOut.Truncated {
		t.Error("truncated = false; want true for an oversized tail")
	}
	if len(resOut.Artifacts) != 2 || resOut.Artifacts[0].Path != "/tmp/artifacts/iss-3/transcript.jsonl" {
		t.Errorf("artifacts = %+v; want both artifact paths", resOut.Artifacts)
	}
}

// TestCancelPropagates checks lambe_cancel reaches the control and reports
// the resulting state.
func TestCancelPropagates(t *testing.T) {
	h := newHarness(t, Options{})
	var out spawnResult
	h.call("lambe_spawn", spawnArgs{IssueID: "iss-4", Prompt: "p"}, &out)
	handle := mustHandle(t, out.Handle)
	h.fc.setCancel(t, handle, RunStatus{State: StateCanceled, Attempt: 1, LastEventAt: time.Now(), Outcome: "canceled by caller"})

	var cres cancelResult
	res := h.call("lambe_cancel", cancelArgs{Handle: out.Handle}, &cres)
	if res.IsError {
		t.Fatalf("lambe_cancel errored: %v", res.Content)
	}
	if len(h.fc.canceled) != 1 || h.fc.canceled[0] != handle {
		t.Errorf("control cancels = %v; want [%v]", h.fc.canceled, handle)
	}
	if cres.State != StateCanceled.String() || cres.Outcome != "canceled by caller" {
		t.Errorf("cancel result = %+v; want canceled state with outcome", cres)
	}
}

// TestListSnapshot checks lambe_list returns one entry per run with handle
// and state.
func TestListSnapshot(t *testing.T) {
	h := newHarness(t, Options{})
	var a, b spawnResult
	h.call("lambe_spawn", spawnArgs{IssueID: "iss-5", Prompt: "p"}, &a)
	h.call("lambe_spawn", spawnArgs{IssueID: "iss-6", Prompt: "p"}, &b)

	var lres listResult
	res := h.call("lambe_list", struct{}{}, &lres)
	if res.IsError {
		t.Fatalf("lambe_list errored: %v", res.Content)
	}
	if len(lres.Runs) != 2 {
		t.Fatalf("runs = %d; want 2", len(lres.Runs))
	}
	got := map[string]statusResult{}
	for _, r := range lres.Runs {
		got[r.IssueID] = r
	}
	for _, issue := range []string{"iss-5", "iss-6"} {
		r, ok := got[issue]
		if !ok {
			t.Fatalf("issue %s missing from snapshot %+v", issue, lres.Runs)
		}
		if r.Handle == "" || r.State != StateRunning.String() {
			t.Errorf("run for %s = %+v; want handle and running state", issue, r)
		}
	}
}

// TestDepthRefusal is the trust anchor: an agent already supervised at
// depth 1 (the default max) may not spawn further runs, and the refusal
// names the limit instead of silently failing.
func TestDepthRefusal(t *testing.T) {
	h := newHarness(t, Options{Env: []string{"LAMBE_DEPTH=1"}})

	res := h.call("lambe_spawn", spawnArgs{IssueID: "iss-7", Prompt: "p"}, nil)
	if !res.IsError {
		t.Fatalf("lambe_spawn at depth 1 succeeded; want refusal")
	}
	if len(h.fc.spawned) != 0 {
		t.Errorf("control saw %d spawns; want 0 after refusal", len(h.fc.spawned))
	}
	var text string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			text = tc.Text
		}
	}
	if !strings.Contains(text, "depth") || !strings.Contains(text, "1") {
		t.Errorf("error text %q; want it to name the depth limit", text)
	}
}

// TestDepthAllowedAtTop checks a depth-0 caller can spawn and the run is
// recorded at depth 1 so the daemon can export LAMBE_DEPTH to the child.
func TestDepthAllowedAtTop(t *testing.T) {
	h := newHarness(t, Options{Env: []string{"LAMBE_DEPTH=0"}})
	var out spawnResult
	res := h.call("lambe_spawn", spawnArgs{IssueID: "iss-8", Prompt: "p"}, &out)
	if res.IsError {
		t.Fatalf("lambe_spawn at depth 0 errored: %v", res.Content)
	}
	if len(h.fc.spawned) != 1 || h.fc.spawned[0].Depth != 1 {
		t.Errorf("spawn specs = %+v; want one spawn at depth 1", h.fc.spawned)
	}
}

// TestDepthConfigurable checks raising MaxDepth lets a supervised agent
// spawn exactly one more level.
func TestDepthConfigurable(t *testing.T) {
	h := newHarness(t, Options{MaxDepth: 2, Env: []string{"LAMBE_DEPTH=1"}})
	res := h.call("lambe_spawn", spawnArgs{IssueID: "iss-9", Prompt: "p"}, nil)
	if res.IsError {
		t.Fatalf("lambe_spawn at depth 1 with max 2 errored: %v", res.Content)
	}
	if len(h.fc.spawned) != 1 || h.fc.spawned[0].Depth != 2 {
		t.Errorf("spawn specs = %+v; want one spawn at depth 2", h.fc.spawned)
	}
}

// TestArgsRoundTrip checks the typed tool arguments survive a JSON
// round trip, which is what every MCP client does to them.
func TestArgsRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		arg  any
	}{
		{"spawn", spawnArgs{IssueID: "iss-1", Prompt: "prompt text", Workspace: "/w"}},
		{"status", statusArgs{Handle: "run-1"}},
		{"output", outputArgs{Handle: "run-1"}},
		{"cancel", cancelArgs{Handle: "run-1"}},
		{"list", struct{}{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := json.Marshal(tt.arg)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			got := tt.arg
			if err := json.Unmarshal(b, &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			want, _ := json.Marshal(tt.arg)
			gotB, _ := json.Marshal(got)
			if string(want) != string(gotB) {
				t.Errorf("round trip changed args: got %s, want %s", gotB, want)
			}
		})
	}
}

// TestToolSchemaAdvertised checks the client sees all five tools with
// object input schemas.
func TestToolSchemaAdvertised(t *testing.T) {
	h := newHarness(t, Options{})
	tools, err := h.cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	want := map[string]bool{
		"lambe_spawn": false, "lambe_status": false, "lambe_output": false,
		"lambe_cancel": false, "lambe_list": false,
	}
	for _, tool := range tools.Tools {
		if _, ok := want[tool.Name]; ok {
			want[tool.Name] = true
		}
		if tool.InputSchema == nil {
			t.Errorf("tool %s has no input schema", tool.Name)
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("tool %s not advertised", name)
		}
	}
}

// TestDepthFromEnvTable covers depth parsing from inherited environments.
func TestDepthFromEnvTable(t *testing.T) {
	tests := []struct {
		name string
		env  []string
		want int
	}{
		{"absent means top level", nil, 0},
		{"zero", []string{"A=1", "LAMBE_DEPTH=0"}, 0},
		{"one", []string{"LAMBE_DEPTH=1"}, 1},
		{"garbage means zero", []string{"LAMBE_DEPTH=deep"}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := depthFromEnv(tt.env); got != tt.want {
				t.Errorf("depthFromEnv(%v) = %d; want %d", tt.env, got, tt.want)
			}
		})
	}
}

// mustHandle converts a tool-visible handle string back to the typed
// handle for fake scripting.
func mustHandle(t *testing.T, s string) RunHandle {
	t.Helper()
	h, err := ParseRunHandle(s)
	if err != nil {
		t.Fatalf("parse handle %q: %v", s, err)
	}
	return h
}
