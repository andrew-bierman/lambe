package proc_test

import (
	"bytes"
	"context"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/andrew-bierman/lambe/internal/proc"
	"github.com/andrew-bierman/lambe/internal/testchild"
)

func TestMain(m *testing.M) {
	testchild.Main(m)
}

// start launches the fake agent with the given behavior.
func start(t *testing.T, behavior string, mutate func(*proc.Options)) *proc.Child {
	t.Helper()
	opts := proc.Options{
		Path:    testBin,
		Env:     append(os.Environ(), testchild.EnvVar+"="+behavior),
		Stderr:  &bytes.Buffer{},
		Session: "sess-test",
	}
	if mutate != nil {
		mutate(&opts)
	}
	child, err := proc.Start(opts)
	if err != nil {
		t.Fatalf("start %s: %v", behavior, err)
	}
	return child
}

// testBin is this test binary, re-exec'd as the fake agent via EnvVar.
var testBin = os.Args[0] //nolint:gochecknoglobals // re-exec target is inherently a package-level value.

// collectFrames drains the frames channel until it closes.
func collectFrames(t *testing.T, frames <-chan proc.Frame) []proc.Frame {
	t.Helper()
	var got []proc.Frame
	for frame := range frames {
		got = append(got, frame)
	}
	return got
}

// assertGone asserts the process-group id and pid no longer exist. The direct
// child must have been reaped (Wait returned) before calling this.
func assertGone(t *testing.T, child *proc.Child) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		groupAlive := proc.GroupAlive(child.Pgid())
		procState, _ := os.FindProcess(int(child.Pgid()))
		pidAlive := procState.Signal(syscall.Signal(0)) == nil
		if !groupAlive && !pidAlive {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("pgid %d still alive or unreaped after wait", child.Pgid())
}

// exitSignal returns the terminating signal of a signal-killed child.
func exitSignal(t *testing.T, child *proc.Child) syscall.Signal {
	t.Helper()
	state := child.State()
	if state == nil {
		t.Fatal("child has no process state; Wait not completed")
	}
	ws, ok := state.Sys().(syscall.WaitStatus)
	if !ok {
		t.Fatalf("process state is %T, want syscall.WaitStatus", state.Sys())
	}
	if !ws.Signaled() {
		t.Fatalf("child exited with code %d, want signal death", state.ExitCode())
	}
	return ws.Signal()
}

// TestFinalFrameSurvivesConcurrentWait is the pipe/Wait regression anchor: a
// final frame larger than the pipe buffer, written just before exit, must
// survive a Wait that completes while the frame pump is still draining.
func TestFinalFrameSurvivesConcurrentWait(t *testing.T) {
	child := start(t, testchild.BehaviorStreamingThenSettled, nil)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := child.Wait(); err != nil {
			t.Errorf("wait: %v", err)
		}
	}()

	frames := collectFrames(t, child.Frames())
	wg.Wait()

	if len(frames) == 0 {
		t.Fatal("no frames received")
	}
	last := frames[len(frames)-1]
	if last.Type != "trailing" {
		t.Fatalf("last frame type = %q, want %q (final frame lost?)", last.Type, "trailing")
	}
	settle := frames[len(frames)-2]
	if settle.Type != "result" {
		t.Fatalf("settle frame type = %q, want %q", settle.Type, "result")
	}
	wantPayload := 256 * 1024
	if payload := string(settle.Data); len(payload) < wantPayload {
		t.Fatalf("settle frame truncated: got %d bytes of payload data, want >= %d", len(payload), wantPayload)
	}
	if len(frames) != 22 { // 20 stream frames + settle + trailing
		t.Fatalf("got %d frames, want 22", len(frames))
	}
	assertGone(t, child)
}

// TestSilentChildWatchdogLadder: no frames arrive, the silence timer fires,
// the ladder runs TERM → grace → KILL, and the child ends reaped with no
// zombie and no surviving group member.
func TestSilentChildWatchdogLadder(t *testing.T) {
	child := start(t, testchild.BehaviorSilent, nil)
	ctx := context.Background()

	reasonc := make(chan proc.ExpiryReason, 1)
	termErrc := make(chan error, 1)
	wd := proc.NewWatchdog(30*time.Second, 200*time.Millisecond, func(r proc.ExpiryReason) {
		reasonc <- r
		termErrc <- child.Terminate(ctx, 300*time.Millisecond)
	})
	defer wd.Stop()

	// Drain frames, resetting the silence timer on each valid frame.
	go func() {
		for range child.Frames() {
			wd.FrameSeen()
		}
	}()

	waitErr := child.Wait()
	wd.Stop()
	select {
	case r := <-reasonc:
		if r != proc.ReasonSilence {
			t.Fatalf("watchdog expiry = %v, want silence", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("watchdog reason never delivered")
	}
	if err := <-termErrc; err != nil {
		t.Fatalf("terminate: %v", err)
	}
	if waitErr == nil {
		t.Fatal("silent child should have died by signal, got clean exit")
	}
	sig := exitSignal(t, child)
	if sig != syscall.SIGTERM && sig != syscall.SIGKILL {
		t.Fatalf("exit signal = %v, want TERM or KILL", sig)
	}
	assertGone(t, child)
}

// TestSIGTERMIgnoringChildEscalates: a child that traps SIGTERM must die by
// group SIGKILL within the grace bound, and be reaped.
func TestSIGTERMIgnoringChildEscalates(t *testing.T) {
	child := start(t, testchild.BehaviorIgnoreSIGTERM, nil)
	go func() { _ = collectFrames(t, child.Frames()) }()

	// Give the child time to install its SIGTERM handler before signaling;
	// a TERM that wins the race with runtime startup is a default-action death.
	time.Sleep(300 * time.Millisecond)
	const grace = 300 * time.Millisecond
	begin := time.Now()
	if err := child.Terminate(context.Background(), grace); err != nil {
		t.Fatalf("terminate: %v", err)
	}
	elapsed := time.Since(begin)

	if elapsed < grace {
		t.Fatalf("ladder escalated after %v, before grace %v elapsed", elapsed, grace)
	}
	if sig := exitSignal(t, child); sig != syscall.SIGKILL {
		t.Fatalf("exit signal = %v, want KILL", sig)
	}
	assertGone(t, child)
}

// TestGrandchildHoldsPipeWaitDelay: a clean child exit with a grandchild
// holding the pipes must not hang the wait; the WaitDelay bound applies and
// the exec.ErrWaitDelay path counts as success.
func TestGrandchildHoldsPipeWaitDelay(t *testing.T) {
	const waitDelay = 400 * time.Millisecond
	child := start(t, testchild.BehaviorGrandchildHoldsPipe, func(o *proc.Options) {
		o.WaitDelay = waitDelay
	})
	go func() { _ = collectFrames(t, child.Frames()) }()

	begin := time.Now()
	if err := child.Wait(); err != nil {
		t.Fatalf("wait: %v (ErrWaitDelay on clean exit must be success)", err)
	}
	elapsed := time.Since(begin)

	// Two bounds apply (the exec-managed stderr pipe, then the stdout read
	// end) plus race-instrumented child startup; anything within this window
	// proves the wait did not hang on the hour-sleeping grandchild.
	if bound := 3*waitDelay + 2*time.Second; elapsed > bound {
		t.Fatalf("wait took %v, want <= %v", elapsed, bound)
	}
	// The child itself exited cleanly and was reaped.
	if code := child.State().ExitCode(); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
}

// TestSpamFrames verifies ordering and completeness under a fast producer.
func TestSpamFrames(t *testing.T) {
	child := start(t, testchild.BehaviorSpam, nil)
	frames := collectFrames(t, child.Frames())
	if err := child.Wait(); err != nil {
		t.Fatalf("wait: %v", err)
	}
	if len(frames) != 501 {
		t.Fatalf("got %d frames, want 501", len(frames))
	}
	if frames[0].Type != "event" || frames[500].Type != "result" {
		t.Fatalf("frame order wrong: first=%q last=%q", frames[0].Type, frames[500].Type)
	}
}

// TestLimitErrorFrames verifies the provider-error stream shape then settle.
func TestLimitErrorFrames(t *testing.T) {
	child := start(t, testchild.BehaviorLimitError, nil)

	// Collect until the settle frame arrives so the child is provably up
	// before the ladder starts.
	var settled bool
	var frames []proc.Frame
	for frame := range child.Frames() {
		frames = append(frames, frame)
		if frame.Type == "result" {
			settled = true
			break
		}
	}
	if !settled {
		t.Fatal("no settle frame arrived before channel drained")
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := child.Terminate(context.Background(), 200*time.Millisecond); err != nil {
			t.Errorf("terminate: %v", err)
		}
	}()
	defer wg.Wait()
	for frame := range child.Frames() {
		frames = append(frames, frame)
	}
	_ = child.Wait()

	var errorFrames, resultFrames int
	for _, f := range frames {
		switch f.Type {
		case "error":
			errorFrames++
		case "result":
			resultFrames++
		}
	}
	if errorFrames != 3 || resultFrames != 1 {
		t.Fatalf("got %d error and %d result frames, want 3 and 1", errorFrames, resultFrames)
	}
	assertGone(t, child)
}

// TestHangChildFramesThenTerminate: one frame arrives, then silence until the
// ladder reaps the child.
func TestHangChildFramesThenTerminate(t *testing.T) {
	child := start(t, testchild.BehaviorHang, nil)

	// Wait for the emitted frame so the child is provably up before the ladder.
	framesc := child.Frames()
	first, ok := <-framesc
	if !ok || first.Type != "progress" {
		t.Fatalf("first frame = %+v (ok=%v), want progress", first, ok)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := child.Terminate(context.Background(), 200*time.Millisecond); err != nil {
			t.Errorf("terminate: %v", err)
		}
	}()
	defer wg.Wait()
	frames := []proc.Frame{first}
	frames = append(frames, collectFrames(t, framesc)...)
	if len(frames) != 1 || frames[0].Type != "progress" {
		t.Fatalf("got %v frames, want one progress frame", frames)
	}
	_ = child.Wait()
	assertGone(t, child)
}

// TestExit0 verifies a clean immediate exit yields success and no frames.
func TestExit0(t *testing.T) {
	child := start(t, testchild.BehaviorExit0, nil)
	frames := collectFrames(t, child.Frames())
	if err := child.Wait(); err != nil {
		t.Fatalf("wait: %v", err)
	}
	if len(frames) != 0 {
		t.Fatalf("got %d frames, want 0", len(frames))
	}
	assertGone(t, child)
}

// TestPersistSeesPgidAndSession verifies the crash-recovery seam: the persist
// callback runs exactly once, before Start returns, with the real pgid.
func TestPersistSeesPgidAndSession(t *testing.T) {
	var mu sync.Mutex
	var calls []procCall
	child := start(t, testchild.BehaviorExit0, func(o *proc.Options) {
		o.Persist = func(pgid int, session string) {
			mu.Lock()
			defer mu.Unlock()
			calls = append(calls, procCall{pgid: pgid, session: session})
		}
	})
	_ = collectFrames(t, child.Frames())
	if err := child.Wait(); err != nil {
		t.Fatalf("wait: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 1 {
		t.Fatalf("persist called %d times, want 1", len(calls))
	}
	if calls[0].pgid != child.Pgid() {
		t.Fatalf("persist pgid = %d, want %d", calls[0].pgid, child.Pgid())
	}
	if calls[0].session != "sess-test" {
		t.Fatalf("persist session = %q, want %q", calls[0].session, "sess-test")
	}
}

// procCall records one persist-callback invocation.
type procCall struct {
	pgid    int
	session string
}
