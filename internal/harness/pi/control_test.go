package pi_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andrew-bierman/lambe/internal/harness"
	"github.com/andrew-bierman/lambe/internal/harness/pi"
)

// lockedBuffer is a goroutine-safe log sink.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// waitFor polls cond until it holds or a generous deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func startedDriver(t *testing.T, script string, mutate func(*pi.Options)) (*pi.Driver, string) {
	t.Helper()
	opts, logPath := testEnv(t, script, mutate)
	d := pi.New(opts)
	t.Cleanup(func() { _ = d.Close(context.WithoutCancel(t.Context())) })
	if err := d.Start(t.Context()); err != nil {
		t.Fatalf("start: %v", err)
	}
	return d, logPath
}

func TestDriverCapabilitiesAdvertiseLimitSignal(t *testing.T) {
	if !pi.New(pi.Options{}).Capabilities().LimitSignal {
		t.Fatal("pi driver must advertise LimitSignal")
	}
}

// TestDriverControlCommandsReachTheAgent pins the wire form of the three
// control-plane calls.
func TestDriverControlCommandsReachTheAgent(t *testing.T) {
	tests := []struct {
		name string
		call func(*pi.Driver, context.Context) error
		want []string
	}{
		{"prompt", func(d *pi.Driver, ctx context.Context) error { return d.Prompt(ctx, "do the thing") }, []string{`"type":"prompt"`, `"message":"do the thing"`}},
		{"steer", func(d *pi.Driver, ctx context.Context) error { return d.Steer(ctx, "go left") }, []string{`"type":"steer"`, `"message":"go left"`}},
		{"abort", func(d *pi.Driver, ctx context.Context) error { return d.Abort(ctx) }, []string{`"type":"abort"`}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d, logPath := startedDriver(t, scriptHappy, nil)
			if err := tc.call(d, t.Context()); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			log := readLog(t, logPath)
			for _, want := range tc.want {
				if !strings.Contains(log, want) {
					t.Fatalf("agent never received %s:\n%s", want, log)
				}
			}
		})
	}
}

// TestDriverPromptDuringStreamingSteers checks the direct Prompt
// path (not Run) also converts to steer while the agent streams.
func TestDriverPromptDuringStreamingSteers(t *testing.T) {
	d, logPath := startedDriver(t, scriptStreaming, nil)
	if err := d.Prompt(t.Context(), "redirect"); err != nil {
		t.Fatal(err)
	}
	log := readLog(t, logPath)
	if strings.Contains(log, "PROTOCOL-VIOLATION") || strings.Contains(log, `"type":"prompt"`) || !strings.Contains(log, `"type":"steer"`) {
		t.Fatalf("Prompt while streaming must send steer only:\n%s", log)
	}
}

// TestDriverRefusedCommandsReturnCommandError pins that an agent refusal
// is a typed error carrying the agent's message, not a silent success.
func TestDriverRefusedCommandsReturnCommandError(t *testing.T) {
	d, _ := startedDriver(t, scriptRefuse, nil)
	for name, call := range map[string]func() error{
		"prompt": func() error { return d.Prompt(t.Context(), "x") },
		"steer":  func() error { return d.Steer(t.Context(), "x") },
		"abort":  func() error { return d.Abort(t.Context()) },
	} {
		t.Run(name, func(t *testing.T) {
			err := call()
			var ce *pi.CommandError
			if !errors.As(err, &ce) {
				t.Fatalf("err = %v, want *pi.CommandError", err)
			}
			if string(ce.Command) != name || ce.Message != "refused: "+name {
				t.Fatalf("CommandError = %+v", ce)
			}
		})
	}
}

// TestDriverCallHonorsCallTimeout pins that a prompt whose ack never comes
// returns on the configured CallTimeout rather than hanging.
func TestDriverCallHonorsCallTimeout(t *testing.T) {
	d, _ := startedDriver(t, scriptSlowAck, nil) // startup runs under the default call timeout
	d.SetCallTimeout(50 * time.Millisecond)
	err := d.Prompt(t.Context(), "x") // slow-ack holds the response for 700ms
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
}

// TestRunIgnoresMalformedFrames pins that garbage on stdout is dropped (and
// logged) and does not poison the turn: the real settle record still ends
// the run.
func TestRunIgnoresMalformedFrames(t *testing.T) {
	logs := &lockedBuffer{}
	opts, _ := testEnv(t, scriptMalformed, func(o *pi.Options) {
		o.Logger = slog.New(slog.NewTextHandler(logs, nil))
	})
	d := pi.New(opts)
	collect := drainEvents(d)
	out, err := d.Run(t.Context(), "go")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.Kind != harness.OutcomeSettled {
		t.Fatalf("outcome = %v, want settled despite malformed frames", out.Kind)
	}
	events := closeThenCollect(t, d, collect)
	var settled int
	for _, ev := range events {
		if _, ok := ev.(harness.AgentSettledEvent); ok {
			settled++
		}
	}
	if settled != 1 {
		t.Fatalf("settled events = %d, want exactly 1 in %v", settled, events)
	}
	if got := logs.String(); !strings.Contains(got, "unparseable record") || !strings.Contains(got, "this is not json") {
		t.Fatalf("malformed frame not reported in logs:\n%s", got)
	}
}

// TestClientCloseWhileEmitting pins that closing a driver mid-stream
// terminates the process group, closes the event channel, and neither
// deadlocks nor panics on the racing emitter.
func TestClientCloseWhileEmitting(t *testing.T) {
	opts, _ := testEnv(t, scriptChatter, func(o *pi.Options) { o.TerminateGrace = 200 * time.Millisecond })
	var pgid int
	opts.Persist = func(p int, _ string) error { pgid = p; return nil }
	d := pi.New(opts)
	if err := d.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := d.Prompt(t.Context(), "go"); err != nil {
		t.Fatal(err)
	}
	// Do not consume yet: wait until events are actually flowing.
	waitFor(t, "first event", func() bool { return len(d.Events()) > 0 })

	closed := make(chan error, 1)
	go func() { closed <- d.Close(context.WithoutCancel(t.Context())) }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("close: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("Close deadlocked against an active emitter")
	}
	waitGroupGone(t, pgid, 30*time.Second)
	// The stream must end: drain until closed.
	for range d.Events() { //nolint:revive // drain to channel close
	}
}

// TestClientCallAfterProcessExit pins the error shape of a command sent to
// a dead agent: an error, never a hang.
func TestClientCallAfterProcessExit(t *testing.T) {
	logPath := t.TempDir() + "/fake.log"
	c, err := pi.StartClient(pi.ClientOptions{
		Path: os.Args[0],
		Env:  append(os.Environ(), fakeEnv+"="+scriptSettleExit, fakeLogEnv+"="+logPath),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close(context.WithoutCancel(t.Context()), 100*time.Millisecond) })
	if _, err := c.Call(t.Context(), pi.Command{Type: pi.CmdPrompt, Message: "go"}); err != nil {
		// settle-exit answers nothing to a prompt-less get_state; either the
		// response arrived or the stream ended: both are acceptable here.
		t.Logf("first call: %v", err)
	}
	<-c.Done()
	callCtx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if _, err := c.Call(callCtx, pi.Command{Type: pi.CmdGetState}); err == nil {
		t.Fatal("Call on a dead agent succeeded")
	}
	if err := c.Notify(pi.Command{Type: pi.CmdAbort}); err == nil {
		// Writing to a reaped child's closed stdin must fail.
		t.Fatal("Notify on a dead agent succeeded")
	}
}

// TestClientLifecycleAccessors covers Pgid, CloseStdin, Wait and WaitErr on
// a healthy child: closing stdin makes the fake exit cleanly.
func TestClientLifecycleAccessors(t *testing.T) {
	c, err := pi.StartClient(pi.ClientOptions{
		Path: os.Args[0],
		Env:  append(os.Environ(), fakeEnv+"="+scriptHappy),
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.Pgid() <= 0 {
		t.Fatalf("Pgid = %d", c.Pgid())
	}
	if _, err := c.Call(t.Context(), pi.Command{Type: pi.CmdGetState}); err != nil {
		t.Fatalf("get_state: %v", err)
	}
	c.CloseStdin()
	waited := make(chan [2]error, 1)
	go func() { waited <- [2]error{c.Wait(), c.WaitErr()} }()
	select {
	case errs := <-waited:
		if errs[0] != nil || errs[1] != nil {
			t.Fatalf("Wait=%v WaitErr=%v, want clean exit after stdin close", errs[0], errs[1])
		}
	case <-time.After(60 * time.Second):
		t.Fatal("child did not exit after CloseStdin")
	}
	<-c.Done()
}
