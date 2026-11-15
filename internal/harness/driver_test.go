package harness_test

import (
	"errors"
	"testing"
	"time"

	"github.com/andrew-bierman/lambe/internal/harness"
)

// TestRunError covers the outcome-to-error mapping: only outright failure
// and an unclean exit are errors; every retryable supervision outcome is a
// value.
func TestRunError(t *testing.T) {
	failure := errors.New("boom")
	tests := []struct {
		name string
		out  harness.Outcome
		want error
	}{
		{
			name: "settled",
			out:  harness.Outcome{Kind: harness.OutcomeSettled},
		},
		{
			name: "handled",
			out:  harness.Outcome{Kind: harness.OutcomeHandled},
		},
		{
			name: "limit suspected",
			out:  harness.Outcome{Kind: harness.OutcomeLimitSuspected, Retryable: true},
		},
		{
			name: "watchdog killed",
			out:  harness.Outcome{Kind: harness.OutcomeWatchdogKilled, Err: failure},
		},
		{
			name: "process exited clean",
			out:  harness.Outcome{Kind: harness.OutcomeProcessExited, ExitClean: true},
		},
		{
			name: "process exited unclean",
			out:  harness.Outcome{Kind: harness.OutcomeProcessExited, ExitClean: false, Err: failure},
			want: failure,
		},
		{
			name: "failed",
			out:  harness.Outcome{Kind: harness.OutcomeFailed, Err: failure},
			want: failure,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.out.RunError()
			if !errors.Is(got, tt.want) {
				t.Fatalf("RunError() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestEmit covers event delivery: the event flows on an open stream, and a
// closing driver aborts the send instead of blocking.
func TestEmit(t *testing.T) {
	t.Run("delivers on open stream", func(t *testing.T) {
		ch := make(chan harness.Event, 1)
		harness.Emit(ch, harness.AgentStartedEvent{}, nil)
		select {
		case ev := <-ch:
			if _, ok := ev.(harness.AgentStartedEvent); !ok {
				t.Fatalf("got %T, want AgentStartedEvent", ev)
			}
		default:
			t.Fatal("event not delivered")
		}
	})

	t.Run("closed driver aborts send", func(t *testing.T) {
		ch := make(chan harness.Event) // unbuffered, no reader
		closed := make(chan struct{})
		close(closed)
		done := make(chan struct{})
		go func() {
			defer close(done)
			harness.Emit(ch, harness.AgentStartedEvent{}, closed)
		}()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("Emit blocked on an unconsumed stream after close")
		}
	})
}

// TestDeliverResult covers outcome handoff: delivered when a run is
// waiting, dropped when the channel is nil or already holds an outcome.
func TestDeliverResult(t *testing.T) {
	t.Run("nil channel is a no-op", func(*testing.T) {
		harness.DeliverResult(nil, harness.Outcome{Kind: harness.OutcomeSettled})
	})

	t.Run("delivers to waiting run", func(t *testing.T) {
		ch := make(chan harness.Outcome, 1)
		harness.DeliverResult(ch, harness.Outcome{Kind: harness.OutcomeSettled})
		if out := <-ch; out.Kind != harness.OutcomeSettled {
			t.Fatalf("got %s, want settled", out.Kind)
		}
	})

	t.Run("drops when an outcome already arrived", func(t *testing.T) {
		ch := make(chan harness.Outcome, 1)
		harness.DeliverResult(ch, harness.Outcome{Kind: harness.OutcomeSettled})
		done := make(chan struct{})
		go func() {
			defer close(done)
			harness.DeliverResult(ch, harness.Outcome{Kind: harness.OutcomeFailed})
		}()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("second DeliverResult blocked instead of dropping")
		}
		if out := <-ch; out.Kind != harness.OutcomeSettled {
			t.Fatalf("first outcome was displaced by the second: %s", out.Kind)
		}
	})
}
