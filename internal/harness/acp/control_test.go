package acp_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	acpsdk "github.com/coder/acp-go-sdk"

	"github.com/andrew-bierman/lambe/internal/harness"
	"github.com/andrew-bierman/lambe/internal/harness/acp"
	"github.com/andrew-bierman/lambe/internal/proc"
)

// pollUntil retries cond until it holds or a generous deadline passes.
func pollUntil(t *testing.T, what string, cond func() bool) {
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

// TestPromptSettlesOnEventStream pins the non-Run path: Prompt returns at
// submission and the stop response surfaces as a clean settle event.
func TestPromptSettlesOnEventStream(t *testing.T) {
	drv, events := newDriver(t, "work", nil)
	if err := drv.Prompt(t.Context(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	ev := waitEvent(t, events, 30*time.Second, func(e harness.Event) bool {
		_, ok := e.(harness.AgentSettledEvent)
		return ok
	})
	if ev.(harness.AgentSettledEvent).Aborted {
		t.Fatal("clean end_turn reported as aborted")
	}
}

// TestPromptFailureEndsWithoutSettling pins that an agent error response
// ends the run on the stream but is never reported as a settle.
func TestPromptFailureEndsWithoutSettling(t *testing.T) {
	drv, events := newDriver(t, "error", nil)
	if err := drv.Prompt(t.Context(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	waitEvent(t, events, 30*time.Second, func(e harness.Event) bool {
		if _, settled := e.(harness.AgentSettledEvent); settled {
			t.Fatal("failed prompt reported as settled")
		}
		_, ended := e.(harness.AgentEndedEvent)
		return ended
	})
}

func TestSteerSendsSecondPromptAndSurfacesAgentErrors(t *testing.T) {
	t.Run("accepted", func(t *testing.T) {
		drv, _ := newDriver(t, "work", nil)
		if err := drv.Steer(t.Context(), "also do x"); err != nil {
			t.Fatalf("Steer: %v", err)
		}
	})
	t.Run("agent error is returned", func(t *testing.T) {
		drv, _ := newDriver(t, "error", nil)
		err := drv.Steer(t.Context(), "also do x")
		var rpc *acpsdk.RequestError
		if !errors.As(err, &rpc) || rpc.Code != -32603 {
			t.Fatalf("err = %v, want JSON-RPC internal error from the agent", err)
		}
	})
	t.Run("unanswered steer honors CallTimeout", func(t *testing.T) {
		drv, _ := newDriver(t, "silent", func(o *acp.Options) { o.CallTimeout = 100 * time.Millisecond })
		err := drv.Steer(t.Context(), "hello?")
		if err == nil {
			t.Fatal("Steer to a silent agent returned nil")
		}
		if !errors.Is(err, context.DeadlineExceeded) && !strings.Contains(err.Error(), "deadline") && !strings.Contains(err.Error(), "context") {
			t.Fatalf("err = %v, want a timeout error", err)
		}
	})
	t.Run("canceled context never spawns", func(t *testing.T) {
		drv, _ := newDriver(t, "work", nil)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if err := drv.Steer(ctx, "x"); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	})
}

func TestAbortBeforeStartIsAnError(t *testing.T) {
	drv, _ := newDriver(t, "work", nil)
	if err := drv.Abort(t.Context()); err == nil || !strings.Contains(err.Error(), "abort before start") {
		t.Fatalf("err = %v, want abort-before-start error", err)
	}
}

func TestAbortOnIdleStartedAgentSucceeds(t *testing.T) {
	drv, _ := newDriver(t, "work", nil)
	if err := drv.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := drv.Abort(t.Context()); err != nil {
		t.Fatalf("Abort with no turn in flight: %v", err)
	}
}

// TestStartFailsFastOnMalformedAgentOutput pins that an agent whose stdout is
// garbage then EOF yields a Start error rather than a hang.
func TestStartFailsFastOnMalformedAgentOutput(t *testing.T) {
	drv, _ := newDriver(t, "work", func(o *acp.Options) {
		o.Argv = []string{"sh", "-c", `echo 'not json'; echo '{"jsonrpc":"2.0","id":'; echo '[]'; exit 0`}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	err := drv.Start(ctx)
	if err == nil {
		t.Fatal("Start succeeded against an agent that never spoke ACP")
	}
	if ctx.Err() != nil {
		t.Fatalf("Start hung until the test deadline: %v", err)
	}
}

// TestCloseWhileStreaming pins that Close during an active message stream
// finishes, kills the group, and ends the event stream without panicking on
// the emitter that is mid-send.
func TestCloseWhileStreaming(t *testing.T) {
	pgid := 0
	drv, events := newDriver(t, "stream", func(o *acp.Options) {
		o.Persist = func(p int, _ string) error { pgid = p; return nil }
		o.TerminateGrace = 200 * time.Millisecond
	})
	if err := drv.Prompt(t.Context(), "stream please"); err != nil {
		t.Fatal(err)
	}
	waitEvent(t, events, 30*time.Second, func(e harness.Event) bool {
		_, ok := e.(harness.ToolExecutionStartedEvent)
		return ok
	})
	closed := make(chan error, 1)
	go func() { closed <- drv.Close(context.WithoutCancel(t.Context())) }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("Close deadlocked against a streaming agent")
	}
	pollUntil(t, "agent process group to exit", func() bool { return !proc.GroupAlive(pgid) })
	pollUntil(t, "event stream to close", func() bool {
		for {
			select {
			case _, ok := <-events:
				if !ok {
					return true
				}
			default:
				return false
			}
		}
	})
}
