package acp_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/andrew-bierman/lambe/internal/harness"
	"github.com/andrew-bierman/lambe/internal/harness/acp"
)

// fakeagentBin is built once per test run by TestMain; every test drives the
// real protocol through it.
var fakeagentBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "lambe-acp-fakeagent")
	if err != nil {
		panic(err)
	}
	bin := filepath.Join(dir, "fakeagent")
	out, err := exec.Command("go", "build", "-o", bin, "github.com/andrew-bierman/lambe/internal/harness/acp/fakeagent").CombinedOutput() //nolint:gosec // G204: builds the in-tree test double at a fixed repo-relative path.
	if err != nil {
		panic(string(out))
	}
	fakeagentBin = bin
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// newDriver builds a driver over the fake agent with the given behavior.
func newDriver(t *testing.T, behavior string, mutate func(*acp.Options)) (*acp.Driver, <-chan harness.Event) {
	t.Helper()
	opts := acp.Options{
		Argv:         []string{fakeagentBin, behavior},
		CallTimeout:  5 * time.Second,
		SilenceAfter: 2 * time.Second,
	}
	if mutate != nil {
		mutate(&opts)
	}
	drv, err := acp.New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	events := collect(t, drv)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = drv.Close(ctx)
	})
	return drv, events
}

// collect drains the driver's event stream into a channel for the whole test.
func collect(t *testing.T, drv *acp.Driver) <-chan harness.Event {
	t.Helper()
	out := make(chan harness.Event, 4096)
	go func() {
		defer close(out)
		for ev := range drv.Events() {
			out <- ev
		}
	}()
	return out
}

// waitEvent waits for one event satisfying want, discarding others.
func waitEvent(t *testing.T, events <-chan harness.Event, timeout time.Duration, want func(harness.Event) bool) harness.Event {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatal("event stream closed before match")
			}
			if want(ev) {
				return ev
			}
		case <-deadline:
			t.Fatal("timed out waiting for event")
			return nil
		}
	}
}

// TestCapabilitiesAndResume covers the per-process session posture: no limit
// signal, and a typed not-supported resume error.
func TestCapabilitiesAndResume(t *testing.T) {
	drv, _ := newDriver(t, "work", nil)
	if got := drv.Capabilities(); got.LimitSignal {
		t.Fatal("LimitSignal must be false for the ACP driver")
	}
	err := drv.Resume(context.Background(), "some-session")
	if !errors.Is(err, acp.ErrResumeNotSupported) {
		t.Fatalf("Resume error = %v, want ErrResumeNotSupported", err)
	}
}

// TestPromptStopSettled is the anchor scenario: a prompt answered by the
// protocol's stop reason settles the run, with tool and message milestones
// observed on the way and none of them settling it early.
func TestPromptStopSettled(t *testing.T) {
	drv, events := newDriver(t, "work", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	start := time.Now()
	out, err := drv.Run(ctx, "do the work")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Kind != harness.OutcomeSettled {
		t.Fatalf("outcome kind = %s, want settled", out.Kind)
	}
	if out.Aborted {
		t.Fatal("outcome must not be aborted")
	}
	if !out.AttemptConsumed {
		t.Fatal("settled turn must consume an attempt")
	}
	if out.Stats == nil || out.Stats.InputTokens != 100 || out.Stats.TotalTokens != 120 {
		t.Fatalf("per-turn stats = %+v, want usage from the stop response", out.Stats)
	}
	if elapsed := time.Since(start); elapsed < 20*time.Millisecond {
		t.Fatal("run settled before the agent did any work")
	}

	waitEvent(t, events, time.Second, func(ev harness.Event) bool {
		_, ok := ev.(harness.ToolExecutionStartedEvent)
		return ok && ev.(harness.ToolExecutionStartedEvent).ToolName == "run tests"
	})
	waitEvent(t, events, time.Second, func(ev harness.Event) bool {
		ended, ok := ev.(harness.ToolExecutionEndedEvent)
		return ok && ended.ToolName == "run tests" && !ended.IsError
	})
	waitEvent(t, events, time.Second, func(ev harness.Event) bool {
		msg, ok := ev.(harness.MessageCompletedEvent)
		return ok && msg.Role == "assistant" && msg.Text == "work finished"
	})
	waitEvent(t, events, time.Second, func(ev harness.Event) bool {
		settled, ok := ev.(harness.AgentSettledEvent)
		return ok && !settled.Aborted
	})
}

// TestUpdateStreamDoesNotSettleEarly is the red anchor: an agent streaming
// update after update must not settle the run until the stop response
// arrives. The driver keeps waiting while chunks flow.
func TestUpdateStreamDoesNotSettleEarly(t *testing.T) {
	drv, events := newDriver(t, "stream", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	runDone := make(chan harness.Outcome, 1)
	go func() {
		out, err := drv.Run(ctx, "stream for a while")
		if err != nil {
			t.Errorf("Run: %v", err)
		}
		runDone <- out
	}()

	// Wait until at least three streamed steps have been observed live,
	// then assert the run is still in flight: updates are progress, not
	// completion.
	steps := 0
	texts := []string{}
	waitEvent(t, events, 5*time.Second, func(ev harness.Event) bool {
		if _, ok := ev.(harness.ToolExecutionStartedEvent); ok {
			steps++
		}
		if msg, ok := ev.(harness.MessageCompletedEvent); ok {
			texts = append(texts, msg.Text)
		}
		return steps >= 3
	})
	select {
	case out := <-runDone:
		t.Fatalf("run settled after streamed updates without a stop: %+v", out)
	case <-time.After(150 * time.Millisecond):
	}

	out := <-runDone
	if out.Kind != harness.OutcomeSettled {
		t.Fatalf("outcome kind = %s, want settled", out.Kind)
	}
	// The whole stream must have been delivered: one flushed message per
	// streamed step, plus the settle event. The final events may still be
	// in flight, so poll with a deadline.
	deadline := time.After(2 * time.Second)
	seenSettled := false
	for {
		if seenSettled {
			want := "chunk 0; chunk 1; chunk 2; chunk 3; chunk 4; chunk 5; chunk 6; chunk 7; chunk 8; chunk 9; "
			got := strings.Join(texts, "")
			if got == want {
				return
			}
			t.Fatalf("stream incomplete: text=%q", got)
		}
		select {
		case ev := <-events:
			switch e := ev.(type) {
			case harness.MessageCompletedEvent:
				texts = append(texts, e.Text)
			case harness.AgentSettledEvent:
				seenSettled = true
			}
		case <-deadline:
			got := strings.Join(texts, "")
			t.Fatalf("timed out waiting for settle: text=%q settled=%v", got, seenSettled)
		}
	}
}

// TestPermissionAutoDenied covers the unattended posture: a permission
// request is denied without interaction, surfaced as a diagnostic event, and
// the agent still completes its turn.
func TestPermissionAutoDenied(t *testing.T) {
	drv, events := newDriver(t, "permission", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	out, err := drv.Run(ctx, "needs permission")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Kind != harness.OutcomeSettled {
		t.Fatalf("outcome kind = %s, want settled", out.Kind)
	}
	waitEvent(t, events, time.Second, func(ev harness.Event) bool {
		diag, ok := ev.(harness.DiagnosticEvent)
		return ok && diag.Message == "permission auto-denied: run dangerous tool"
	})
}

// TestInterruptAborts covers Abort → session/cancel: an agent that honors
// the interrupt answers cancelled, which surfaces as a settled, aborted run.
func TestInterruptAborts(t *testing.T) {
	drv, events := newDriver(t, "stream", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	runDone := make(chan harness.Outcome, 1)
	go func() {
		out, err := drv.Run(ctx, "long turn")
		if err != nil {
			t.Errorf("Run: %v", err)
		}
		runDone <- out
	}()

	waitEvent(t, events, 5*time.Second, func(ev harness.Event) bool {
		_, ok := ev.(harness.ToolExecutionStartedEvent)
		return ok
	})
	if err := drv.Abort(ctx); err != nil {
		t.Fatalf("Abort: %v", err)
	}

	out := <-runDone
	if out.Kind != harness.OutcomeSettled || !out.Aborted {
		t.Fatalf("outcome = %+v, want settled+aborted", out)
	}
	waitEvent(t, events, time.Second, func(ev harness.Event) bool {
		settled, ok := ev.(harness.AgentSettledEvent)
		return ok && settled.Aborted
	})
}

// TestIgnoreInterruptStillSettles covers an agent that ignores session/cancel:
// Abort stays advisory, the turn keeps running, and the driver still reports
// the eventual stop as a clean settle.
func TestIgnoreInterruptStillSettles(t *testing.T) {
	drv, _ := newDriver(t, "ignore-interrupt", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	runDone := make(chan harness.Outcome, 1)
	go func() {
		out, err := drv.Run(ctx, "keep going")
		if err != nil {
			t.Errorf("Run: %v", err)
		}
		runDone <- out
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := drv.Abort(ctx); err == nil {
			break
		}
		if !time.Now().Before(deadline) {
			t.Fatal("Abort never succeeded")
		}
		time.Sleep(20 * time.Millisecond)
	}

	select {
	case out := <-runDone:
		if out.Kind != harness.OutcomeSettled || out.Aborted {
			t.Fatalf("outcome = %+v, want settled and not aborted", out)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("run never settled after ignored interrupt")
	}
}

// groupAlive reports whether any process still exists in the group.
func groupAlive(pgid int) bool {
	return syscall.Kill(-pgid, 0) == nil
}

// TestSilentAgentWatchdogKills covers the silence path: an agent that
// accepts the prompt and then goes silent is killed by the watchdog via the
// group kill ladder and reported as a retryable watchdog kill.
func TestSilentAgentWatchdogKills(t *testing.T) {
	var pgids sync.Map
	drv, _ := newDriver(t, "silent", func(o *acp.Options) {
		o.SilenceAfter = 400 * time.Millisecond
		o.Persist = func(pgid int, _ string) { pgids.Store(pgid, true) }
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	out, err := drv.Run(ctx, "say anything")
	if err == nil && out.Kind != harness.OutcomeWatchdogKilled {
		t.Fatalf("outcome = %+v err=%v, want watchdog_killed", out, err)
	}
	if out.Kind != harness.OutcomeWatchdogKilled {
		t.Fatalf("outcome kind = %s, want watchdog_killed", out.Kind)
	}
	if !out.Retryable {
		t.Fatal("watchdog kill must be retryable")
	}

	// The kill ladder must have reached the process group.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		alive := false
		pgids.Range(func(k, _ any) bool {
			if pid, ok := k.(int); ok && groupAlive(pid) {
				alive = true
			}
			return true
		})
		if !alive {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("process group survived the kill ladder")
}

// TestRunCancelStillKillsGroup covers the cancelled-run path: Run returns
// on ctx cancellation, and the kill ladder — run on a background context —
// still takes down the agent process group after the run context is dead.
func TestRunCancelStillKillsGroup(t *testing.T) {
	var pgidOf sync.Map
	drv, _ := newDriver(t, "silent", func(o *acp.Options) {
		o.SilenceAfter = 10 * time.Second // only the cancel path may kill
		o.Persist = func(pgid int, _ string) { pgidOf.Store(pgid, true) }
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runDone := make(chan harness.Outcome, 1)
	go func() {
		out, _ := drv.Run(ctx, "will be cancelled")
		runDone <- out
	}()
	time.Sleep(200 * time.Millisecond)
	cancel()

	select {
	case out := <-runDone:
		if out.Kind != harness.OutcomeFailed || !errors.Is(out.Err, context.Canceled) {
			t.Fatalf("outcome = %+v, want failed with context.Canceled", out)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run never returned after cancellation")
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		alive := false
		pgidOf.Range(func(k, _ any) bool {
			if pid, ok := k.(int); ok && groupAlive(pid) {
				alive = true
			}
			return true
		})
		if !alive {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("process group survived the kill ladder after run-context cancel")
}

// TestStopRacingExitStillSettles covers the exit race: a stop response
// written immediately before process exit must still settle the run inside
// the flush window, not conclude as an unsettled process exit.
func TestStopRacingExitStillSettles(t *testing.T) {
	drv, _ := newDriver(t, "stop-die", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	out, err := drv.Run(ctx, "answer then die")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Kind != harness.OutcomeSettled {
		t.Fatalf("outcome kind = %s, want settled", out.Kind)
	}
}

// TestMatrix drives every fake-agent behavior through Run and checks the
// typed outcome; it is the regression net over the mappings documented in
// the package comment.
func TestMatrix(t *testing.T) {
	tests := []struct {
		behavior  string
		wantKind  harness.OutcomeKind
		wantAbort bool
	}{
		{behavior: "work", wantKind: harness.OutcomeSettled},
		{behavior: "stream", wantKind: harness.OutcomeSettled},
		{behavior: "permission", wantKind: harness.OutcomeSettled},
		{behavior: "error", wantKind: harness.OutcomeFailed},
		{behavior: "ignore-interrupt", wantKind: harness.OutcomeSettled},
	}
	for _, tt := range tests {
		t.Run(tt.behavior, func(t *testing.T) {
			drv, _ := newDriver(t, tt.behavior, nil)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			out, err := drv.Run(ctx, "matrix turn")
			if out.Kind != tt.wantKind {
				t.Fatalf("outcome kind = %s (err %v), want %s", out.Kind, err, tt.wantKind)
			}
			if out.Aborted != tt.wantAbort {
				t.Fatalf("outcome aborted = %v, want %v", out.Aborted, tt.wantAbort)
			}
			stats, serr := drv.SessionStats(ctx)
			if serr != nil {
				t.Fatalf("SessionStats: %v", serr)
			}
			if stats.SessionID == "" {
				t.Fatal("SessionStats must carry the backend session id")
			}
		})
	}
}

// TestStartCreatesSessionAndStatsRecords covers the explicit Start path and
// per-turn stats recording through the recorder seam.
func TestStartCreatesSessionAndStatsRecords(t *testing.T) {
	recorder := &statsRecorder{}
	drv, _ := newDriver(t, "work", func(o *acp.Options) { o.Stats = recorder })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := drv.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	out, err := drv.Run(ctx, "record my stats")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Kind != harness.OutcomeSettled {
		t.Fatalf("outcome kind = %s, want settled", out.Kind)
	}
	if got := recorder.recorded(); got != 1 {
		t.Fatalf("RecordTurnStats calls = %d, want 1", got)
	}
	stats, err := drv.SessionStats(ctx)
	if err != nil {
		t.Fatalf("SessionStats: %v", err)
	}
	if stats.InputTokens != 100 {
		t.Fatalf("stats input tokens = %d, want 100", stats.InputTokens)
	}
}

type statsRecorder struct {
	mu   sync.Mutex
	seen int
}

func (s *statsRecorder) RecordTurnStats(string, harness.SessionStats) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen++
}

func (s *statsRecorder) recorded() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seen
}

// TestLookup covers the harness-name → argv registry.
func TestLookup(t *testing.T) {
	tests := []struct {
		name    string
		want    []string
		wantErr bool
	}{
		{name: "opencode", want: []string{"opencode", "acp"}},
		{name: "gemini", want: []string{"gemini", "acp"}},
		{name: "no-such-agent", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := acp.Lookup(tt.name)
			if tt.wantErr {
				if err == nil {
					t.Fatal("Lookup should fail for unknown agent")
				}
				return
			}
			if err != nil {
				t.Fatalf("Lookup: %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("Lookup(%q) = %v, want %v", tt.name, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("Lookup(%q) = %v, want %v", tt.name, got, tt.want)
				}
			}
		})
	}
}

// TestNewRequiresArgv covers configuration validation.
func TestNewRequiresArgv(t *testing.T) {
	if _, err := acp.New(acp.Options{}); err == nil {
		t.Fatal("New with no argv and no agent name must fail")
	}
	if _, err := acp.New(acp.Options{Agent: "no-such-agent"}); err == nil {
		t.Fatal("New with unknown agent name must fail")
	}
}

// TestCloseIsIdempotent covers teardown.
func TestCloseIsIdempotent(t *testing.T) {
	drv, _ := newDriver(t, "work", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := drv.Close(ctx); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := drv.Close(ctx); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}
