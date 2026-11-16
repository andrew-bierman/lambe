package pi_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andrew-bierman/lambe/internal/harness"
	"github.com/andrew-bierman/lambe/internal/harness/pi"
	"github.com/andrew-bierman/lambe/internal/proc"
)

// recordingSink captures per-turn stats records.
type recordingSink struct {
	mu      sync.Mutex
	session []string
	stats   []harness.SessionStats
}

// RecordTurnStats implements harness.TurnStatsRecorder.
func (r *recordingSink) RecordTurnStats(sessionID string, stats harness.SessionStats) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.session = append(r.session, sessionID)
	r.stats = append(r.stats, stats)
}

// len returns the number of recorded turns.
func (r *recordingSink) len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.stats)
}

// persistCapture records the proc Persist seam invocation.
type persistCapture struct {
	mu      sync.Mutex
	called  bool
	pgid    int
	session string
}

// capture implements the Persist seam.
func (p *persistCapture) capture(pgid int, session string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.called, p.pgid, p.session = true, pgid, session
}

// snapshot returns the captured values.
func (p *persistCapture) snapshot() (bool, int, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.called, p.pgid, p.session
}

// testEnv builds the driver options for one fake script and returns the
// log path the fake writes to.
func testEnv(t *testing.T, script string, mutate func(*pi.Options)) (pi.Options, string) {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), "fake.log")
	opts := pi.Options{
		Path:        os.Args[0],
		Dir:         t.TempDir(),
		Env:         append(os.Environ(), fakeEnv+"="+script, fakeLogEnv+"="+logPath),
		Stderr:      os.Stderr,
		Logger:      nil,
		CallTimeout: 2 * time.Second,
	}
	if mutate != nil {
		mutate(&opts)
	}
	return opts, logPath
}

// readLog returns the fake's accumulated log.
func readLog(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // G304: test-owned log path.
	if err != nil {
		t.Fatalf("read fake log: %v", err)
	}
	return string(b)
}

// drainEvents collects typed events into a slice. collect waits for the
// stream to end, so Close must run before calling it.
func drainEvents(d harness.Driver) func() []harness.Event {
	var mu sync.Mutex
	var events []harness.Event
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev := range d.Events() {
			mu.Lock()
			events = append(events, ev)
			mu.Unlock()
		}
	}()
	return func() []harness.Event {
		<-done
		mu.Lock()
		defer mu.Unlock()
		return events
	}
}

// closeThenCollect closes the driver and returns the collected events.
func closeThenCollect(t *testing.T, d *pi.Driver, collect func() []harness.Event) []harness.Event {
	t.Helper()
	if err := d.Close(t.Context()); err != nil {
		t.Fatalf("close: %v", err)
	}
	return collect()
}

// waitGroupGone polls until the process group no longer exists.
func waitGroupGone(t *testing.T, pgid int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if !proc.GroupAlive(pgid) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("pgid %d still alive after %s", pgid, within)
}

// TestHappyPathSettledWithStats covers prompt → events → agent_settled →
// settled outcome with per-turn stats recorded through the sink.
func TestHappyPathSettledWithStats(t *testing.T) {
	sink := &recordingSink{}
	persist := &persistCapture{}
	opts, _ := testEnv(t, scriptHappy, func(o *pi.Options) {
		o.SessionID = "sess-happy"
		o.Stats = sink
		o.Persist = persist.capture
	})
	d := pi.New(opts)
	getEvents := drainEvents(d)

	out, err := d.Run(t.Context(), "do the thing")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.Kind != harness.OutcomeSettled {
		t.Fatalf("outcome kind = %v, want settled", out.Kind)
	}
	if out.Aborted {
		t.Fatal("outcome aborted, want normal settle")
	}
	if !out.AttemptConsumed {
		t.Fatal("settled turn must consume an attempt")
	}
	if out.Stats == nil {
		t.Fatal("settled outcome carries no stats")
	}
	if out.Stats.TotalTokens != 154 || out.Stats.CostUSD != 0.012 {
		t.Fatalf("stats tokens/cost = %d/%v, want 154/0.012", out.Stats.TotalTokens, out.Stats.CostUSD)
	}
	if out.Stats.ContextPercent == nil || *out.Stats.ContextPercent != 25 {
		t.Fatalf("context percent = %v, want 25", out.Stats.ContextPercent)
	}
	if got := sink.len(); got != 1 {
		t.Fatalf("sink recorded %d turns, want 1", got)
	}
	if called, pgid, session := persist.snapshot(); !called || pgid <= 0 || session != "sess-happy" {
		t.Fatalf("persist seam: called=%v pgid=%d session=%q", called, pgid, session)
	}
	if err := d.Close(t.Context()); err != nil {
		t.Fatalf("close: %v", err)
	}
	for _, ev := range getEvents() {
		if _, ok := ev.(harness.AgentSettledEvent); ok {
			return
		}
	}
	t.Fatal("event stream never delivered agent_settled")
}

// TestAgentEndIsNotCompletion is the completion-semantics anchor: an
// agent_end followed by an automatic retry must NOT complete the turn;
// only agent_settled does.
func TestAgentEndIsNotCompletion(t *testing.T) {
	opts, _ := testEnv(t, scriptRetried, nil)
	d := pi.New(opts)
	getEvents := drainEvents(d)

	start := time.Now()
	out, err := d.Run(t.Context(), "flaky work")
	if err != nil {
		_ = d.Close(t.Context())
		t.Fatalf("run: %v", err)
	}
	events := closeThenCollect(t, d, getEvents)
	if out.Kind != harness.OutcomeSettled {
		t.Fatalf("outcome kind = %v, want settled", out.Kind)
	}
	if elapsed := time.Since(start); elapsed < 250*time.Millisecond {
		t.Fatalf("run returned after %s; agent_end was mistaken for completion", elapsed)
	}
	var ends, settles, retries int
	for _, ev := range events {
		switch ev.(type) {
		case harness.AgentEndedEvent:
			ends++
		case harness.AgentSettledEvent:
			settles++
		case harness.AutoRetryScheduledEvent:
			retries++
		}
	}
	if ends < 2 {
		t.Fatalf("got %d agent_end events, want >= 2", ends)
	}
	if retries < 1 {
		t.Fatalf("got %d retry events, want >= 1", retries)
	}
	if settles != 1 {
		t.Fatalf("got %d agent_settled events, want exactly 1", settles)
	}
}

// TestPromptDuringStreamingUsesSteer verifies the prompt-vs-steer race
// guard: when the agent reports isStreaming, Prompt must send a steer
// command, never a bare prompt.
func TestPromptDuringStreamingUsesSteer(t *testing.T) {
	opts, logPath := testEnv(t, scriptStreaming, nil)
	d := pi.New(opts)
	defer func() { _ = d.Close(t.Context()) }()

	out, err := d.Run(t.Context(), "change of plans")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.Kind != harness.OutcomeSettled {
		t.Fatalf("outcome kind = %v, want settled", out.Kind)
	}
	log := readLog(t, logPath)
	if strings.Contains(log, "PROTOCOL-VIOLATION") {
		t.Fatal("driver sent a bare prompt during streaming")
	}
	if !strings.Contains(log, `"type":"steer"`) || !strings.Contains(log, `"message":"change of plans"`) {
		t.Fatalf("fake log lacks steer command:\n%s", log)
	}
	if strings.Contains(log, `"type":"prompt"`) {
		t.Fatalf("driver sent prompt while streaming:\n%s", log)
	}
}

// TestResumeWithPersistedSessionID verifies spawning with a persisted
// session id continues that session: the id reaches the child's argv and
// get_session_stats reports it back.
func TestResumeWithPersistedSessionID(t *testing.T) {
	const sessionID = "lambe-resume-42"
	opts, logPath := testEnv(t, scriptHappy, func(o *pi.Options) {
		o.SessionID = sessionID
	})
	d := pi.New(opts)

	stats, err := d.SessionStats(t.Context())
	if err != nil {
		t.Fatalf("session stats: %v", err)
	}
	if stats.SessionID != sessionID {
		t.Fatalf("stats session id = %q, want %q", stats.SessionID, sessionID)
	}
	if !strings.Contains(readLog(t, logPath), "--session-id "+sessionID) {
		t.Fatalf("child argv lacks --session-id %s:\n%s", sessionID, readLog(t, logPath))
	}

	// Resume tears down and respawns with the same id.
	if err := d.Resume(t.Context(), sessionID); err != nil {
		t.Fatalf("resume: %v", err)
	}
	out, err := d.Run(t.Context(), "continue the work")
	if err != nil {
		t.Fatalf("run after resume: %v", err)
	}
	if out.Kind != harness.OutcomeSettled {
		t.Fatalf("outcome kind after resume = %v, want settled", out.Kind)
	}
	if got := strings.Count(readLog(t, logPath), "--session-id "+sessionID); got != 2 {
		t.Fatalf("argv logged %d times, want 2 (spawn + respawn)", got)
	}
	if err := d.Close(t.Context()); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// TestSilentChildSteerProbeThenLadder covers the silence watchdog ladder:
// a child that acks then goes silent gets a steer probe first, then the
// kill ladder, and the outcome is a retryable watchdog kill.
func TestSilentChildSteerProbeThenLadder(t *testing.T) {
	persist := &persistCapture{}
	opts, logPath := testEnv(t, scriptSilentAck, func(o *pi.Options) {
		o.SilenceAfter = 150 * time.Millisecond
		o.ProbeWindow = 200 * time.Millisecond
		o.TerminateGrace = 300 * time.Millisecond
		o.Persist = persist.capture
	})
	d := pi.New(opts)
	defer func() { _ = d.Close(t.Context()) }()

	out, err := d.Run(t.Context(), "hello?")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.Kind != harness.OutcomeWatchdogKilled {
		t.Fatalf("outcome kind = %v, want watchdog_killed", out.Kind)
	}
	if !out.Retryable {
		t.Fatal("watchdog kill must be retryable")
	}
	if !out.AttemptConsumed {
		t.Fatal("watchdog kill consumed an attempt flag missing")
	}
	log := readLog(t, logPath)
	if !strings.Contains(log, `"type":"steer"`) || !strings.Contains(log, pi.DefaultSteerProbeMessage) {
		t.Fatalf("steer probe never sent:\n%s", log)
	}
	_, pgid, _ := persist.snapshot()
	waitGroupGone(t, pgid, 5*time.Second)
}

// TestLimitErrorStreamSuspected covers the heuristic limit classifier: a
// stream of rate-limit error events yields a LimitSuspected outcome with a
// wake-at time that does not consume an attempt.
func TestLimitErrorStreamSuspected(t *testing.T) {
	sink := &recordingSink{}
	persist := &persistCapture{}
	opts, _ := testEnv(t, scriptLimit, func(o *pi.Options) {
		o.SilenceAfter = 30 * time.Second // classifier must fire first
		o.LimitHorizon = time.Hour
		o.Stats = sink
		o.Persist = persist.capture
	})
	d := pi.New(opts)
	getEvents := drainEvents(d)

	now := time.Now()
	out, err := d.Run(t.Context(), "hit the limit")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.Kind != harness.OutcomeLimitSuspected {
		t.Fatalf("outcome kind = %v, want limit_suspected", out.Kind)
	}
	if !out.Retryable {
		t.Fatal("limit-suspected outcome must be retryable")
	}
	if out.AttemptConsumed {
		t.Fatal("limit-suspected turn must not consume an attempt")
	}
	if out.WakeAt.Before(now) || out.WakeAt.After(now.Add(time.Hour).Add(5*time.Second)) {
		t.Fatalf("wake-at %v outside (now, now+1h)", out.WakeAt)
	}
	if out.Confidence <= 0 || out.Confidence > 1 {
		t.Fatalf("confidence %v outside (0,1]", out.Confidence)
	}
	if sink.len() != 0 {
		t.Fatalf("sink recorded %d turns, want 0", sink.len())
	}
	_, pgid, _ := persist.snapshot()
	waitGroupGone(t, pgid, 5*time.Second)
	_ = d.Close(t.Context())
	var retries int
	for _, ev := range getEvents() {
		if _, ok := ev.(harness.AutoRetryScheduledEvent); ok {
			retries++
		}
	}
	if retries < 2 {
		t.Fatalf("got %d retry events before suspicion, want >= 2", retries)
	}
}

// TestGrandchildHoldsPipeCleanExit covers teardown when a descendant
// holds the stdout pipe: the direct child's clean exit is still a success
// after the bounded wait, and the ladder clears the group.
func TestGrandchildHoldsPipeCleanExit(t *testing.T) {
	persist := &persistCapture{}
	opts, _ := testEnv(t, scriptGrandkid, func(o *pi.Options) {
		o.WaitDelay = 400 * time.Millisecond
		o.TerminateGrace = 500 * time.Millisecond
		o.Persist = persist.capture
	})
	d := pi.New(opts)

	out, err := d.Run(t.Context(), "settle then orphan a grandchild")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.Kind != harness.OutcomeSettled {
		t.Fatalf("outcome kind = %v, want settled", out.Kind)
	}
	if err := d.Close(t.Context()); err != nil {
		t.Fatalf("close after grandchild-pipe exit: %v", err)
	}
	_, pgid, _ := persist.snapshot()
	waitGroupGone(t, pgid, 5*time.Second)
}

// TestWatchdogKillDuringPromptSubmission covers the race where the watchdog
// ladder kills the process while the prompt call is still in flight: the
// watchdog-kill outcome must win over the resulting transport error.
func TestWatchdogKillDuringPromptSubmission(t *testing.T) {
	opts, _ := testEnv(t, scriptSlowAck, func(o *pi.Options) {
		o.SilenceAfter = 100 * time.Millisecond
		o.ProbeWindow = 100 * time.Millisecond
		o.TerminateGrace = 300 * time.Millisecond
	})
	d := pi.New(opts)
	defer func() { _ = d.Close(t.Context()) }()

	out, err := d.Run(t.Context(), "slow to ack")
	if out.Kind != harness.OutcomeWatchdogKilled {
		t.Fatalf("outcome kind = %v (err %v), want watchdog_killed", out.Kind, err)
	}
	if !out.Retryable {
		t.Fatal("watchdog kill must be retryable")
	}
}

// TestRunCancelStillKillsGroup covers the cancelled-run path: Run returns
// on ctx cancellation, and the kill ladder — run on a background context —
// still takes down the child process group after the run context is dead.
func TestRunCancelStillKillsGroup(t *testing.T) {
	persist := &persistCapture{}
	opts, _ := testEnv(t, scriptSilentAck, func(o *pi.Options) {
		o.SilenceAfter = 30 * time.Second // only the cancel path may kill
		o.TerminateGrace = 300 * time.Millisecond
		o.Persist = persist.capture
	})
	d := pi.New(opts)
	defer func() { _ = d.Close(t.Context()) }()

	ctx, cancel := context.WithCancel(t.Context())
	runDone := make(chan harness.Outcome, 1)
	go func() {
		out, _ := d.Run(ctx, "will be cancelled")
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
	_, pgid, _ := persist.snapshot()
	waitGroupGone(t, pgid, 5*time.Second)
}

// TestSettleRacingExitStillObserved covers the exit race: a settle record
// written immediately before process exit must still produce a settled
// outcome inside the flush window, not an unsettled process-exited one.
func TestSettleRacingExitStillObserved(t *testing.T) {
	opts, _ := testEnv(t, scriptSettleExit, nil)
	d := pi.New(opts)
	defer func() { _ = d.Close(t.Context()) }()

	out, err := d.Run(t.Context(), "settle then die")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.Kind != harness.OutcomeSettled {
		t.Fatalf("outcome kind = %v, want settled", out.Kind)
	}
}

// TestLivePi spawns the real pi binary in a temp directory. It is skipped
// unless LAMBE_TEST_PI=1 is set.
func TestLivePi(t *testing.T) {
	if os.Getenv("LAMBE_TEST_PI") != "1" {
		t.Skip("set LAMBE_TEST_PI=1 to run the live pi test")
	}
	d := pi.New(pi.Options{
		Dir:          t.TempDir(),
		SessionID:    "lambe-live-test",
		Args:         []string{"--no-tools"},
		CallTimeout:  60 * time.Second,
		SilenceAfter: 2 * time.Minute,
		Logger:       nil,
	})
	defer func() { _ = d.Close(t.Context()) }()

	out, err := d.Run(t.Context(), "Reply with exactly: ready")
	if err != nil {
		t.Fatalf("live run: %v (is a provider configured? run `pi auth check --provider <name>`)", err)
	}
	if out.Kind != harness.OutcomeSettled {
		t.Fatalf("outcome kind = %v, want settled (outcome: %+v)", out.Kind, out)
	}
	if out.Stats == nil {
		t.Fatal("live settled outcome carries no stats")
	}
	t.Logf("live stats: %+v", *out.Stats)
}
