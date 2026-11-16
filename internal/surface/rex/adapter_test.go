package rex

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

func newTestAdapter(avail bool, fc *fakeClient, out *bytes.Buffer) *Adapter {
	return NewAdapter(Availability{Available: avail}, fc, out)
}

func TestAdapterLifecycle(t *testing.T) {
	fc := &fakeClient{createResp: `{"id":"ses-1"}`}
	var out bytes.Buffer
	a := newTestAdapter(true, fc, &out)

	if err := a.Handle(context.Background(), RunStartedEvent{Agent: "issues", Prompt: "fix bug"}); err != nil {
		t.Fatalf("RunStarted: %v", err)
	}
	if len(fc.created) != 1 {
		t.Fatalf("sessions created = %d, want 1", len(fc.created))
	}
	req := fc.created[0]
	if req.Name != "issues" {
		t.Errorf("session name = %q, want issues", req.Name)
	}
	if req.Labels["surface"] != "lambe" {
		t.Errorf("labels missing surface=lambe: %v", req.Labels)
	}

	for _, ev := range []RunEvent{
		RunStateEvent{State: RunStateRunning},
		RunStateEvent{State: RunStateDeferred},
		RunStateEvent{State: RunStateSettled},
	} {
		if err := a.Handle(context.Background(), ev); err != nil {
			t.Fatalf("Handle(%v): %v", ev, err)
		}
	}
	got := out.String()
	for _, want := range []string{
		"]7501;state=working:", "]7501;state=idle:", "]7501;state=done:",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output %q missing %q", got, want)
		}
	}
}

func TestAdapterWatchdogKilledEmitsFailed(t *testing.T) {
	fc := &fakeClient{createResp: `{"id":"ses-1"}`}
	var out bytes.Buffer
	a := newTestAdapter(true, fc, &out)
	_ = a.Handle(context.Background(), RunStartedEvent{Agent: "a"})
	if err := a.Handle(context.Background(), RunStateEvent{State: RunStateWatchdogKilled}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !strings.Contains(out.String(), "]7501;state=error:") {
		t.Errorf("output %q missing failed badge", out.String())
	}
}

func TestAdapterStateBeforeStartStillEmits(t *testing.T) {
	var out bytes.Buffer
	a := newTestAdapter(true, &fakeClient{}, &out)
	if err := a.Handle(context.Background(), RunStateEvent{State: RunStateIdle}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !strings.Contains(out.String(), "]7501;state=idle:") {
		t.Errorf("output %q missing idle badge", out.String())
	}
	if len(a.Sessions()) != 0 {
		t.Errorf("sessions = %v, want none", a.Sessions())
	}
}

func TestAdapterDegradesToNoOpWhenUnavailable(t *testing.T) {
	fc := &fakeClient{createResp: `{"id":"ses-1"}`}
	var out bytes.Buffer
	a := newTestAdapter(false, fc, &out)
	for _, ev := range []RunEvent{
		RunStartedEvent{Agent: "issues"},
		RunStateEvent{State: RunStateRunning},
		RunStateEvent{State: RunStateSettled},
	} {
		if err := a.Handle(context.Background(), ev); err != nil {
			t.Fatalf("Handle(%v): %v", ev, err)
		}
	}
	if len(fc.created) != 0 {
		t.Errorf("created sessions = %d, want 0", len(fc.created))
	}
	if out.Len() != 0 {
		t.Errorf("output = %q, want empty", out.String())
	}
}

func TestAdapterCreateSessionErrorPropagates(t *testing.T) {
	fc := &fakeClient{createErr: errors.New("rex down")}
	a := newTestAdapter(true, fc, &bytes.Buffer{})
	err := a.Handle(context.Background(), RunStartedEvent{Agent: "x"})
	if err == nil {
		t.Fatal("Handle should propagate create error")
	}
}

type testIgnoredEvent struct{}

func (testIgnoredEvent) runEvent() {}

func TestAdapterUnknownEventIgnored(t *testing.T) {
	a := newTestAdapter(true, &fakeClient{}, &bytes.Buffer{})
	if err := a.Handle(context.Background(), testIgnoredEvent{}); err != nil {
		t.Fatalf("unknown event should be ignored, got %v", err)
	}
}
