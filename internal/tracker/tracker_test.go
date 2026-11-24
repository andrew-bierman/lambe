package tracker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"
)

// fakeTransport records every call so tests can prove the empty-query
// short-circuit makes no contact at all.
type fakeTransport struct {
	calls []string
	lists []DispatchQuery
}

func (f *fakeTransport) List(_ context.Context, q DispatchQuery) (Result, error) {
	f.calls = append(f.calls, "list")
	f.lists = append(f.lists, q)
	rows := []issueJSON{{ID: "x-1", Title: "t", Status: "open"}}
	data, _ := json.Marshal(rows)
	return Result{Data: data}, nil
}

func (f *fakeTransport) Show(_ context.Context, id string) (Result, error) {
	f.calls = append(f.calls, "show")
	data, _ := json.Marshal([]issueJSON{{ID: id, Status: "open"}})
	return Result{Data: data}, nil
}

func (f *fakeTransport) Create(_ context.Context, r CreateRequest) (Result, error) {
	f.calls = append(f.calls, "create")
	data, _ := json.Marshal([]issueJSON{{ID: r.ID, Status: "open"}})
	return Result{Data: data}, nil
}

func (f *fakeTransport) Update(_ context.Context, r UpdateRequest) (Result, error) {
	f.calls = append(f.calls, "update")
	data, _ := json.Marshal([]issueJSON{{ID: r.ID, Status: "in_progress", Assignee: "me"}})
	return Result{Data: data}, nil
}

func (f *fakeTransport) Heartbeat(_ context.Context, _ string) (Result, error) {
	f.calls = append(f.calls, "heartbeat")
	return Result{}, nil
}

func (f *fakeTransport) Unclaim(_ context.Context, _ string, _ string) (Result, error) {
	f.calls = append(f.calls, "unclaim")
	return Result{}, nil
}

func (f *fakeTransport) Reclaim(_ context.Context, _ time.Duration, _ []string) (Result, error) {
	f.calls = append(f.calls, "reclaim")
	return Result{}, nil
}

func (f *fakeTransport) Defer(_ context.Context, _ string, _ *time.Time, _ string) (Result, error) {
	f.calls = append(f.calls, "defer")
	return Result{}, nil
}

func (f *fakeTransport) Events(_ context.Context, since int64) (Result, error) {
	f.calls = append(f.calls, "events")
	line := fmt.Sprintf(`{"seq":%d,"ts":"2026-01-01T00:00:00Z","op":"create","issue_id":"x-1","actor":"me"}`, since+1)
	return Result{Raw: []byte(line + "\n")}, nil
}

func (f *fakeTransport) Version(_ context.Context) (string, error) {
	return "bd version 1.3.4 (7ef2fd05e)", nil
}

func (f *fakeTransport) ConfigGet(_ context.Context, _ string) (string, error) {
	return "x", nil
}

func (f *fakeTransport) Batch(_ context.Context, _ []BatchOp) (Result, error) {
	f.calls = append(f.calls, "batch")
	return Result{}, nil
}

func TestDispatchableEmptyQueryMakesNoCall(t *testing.T) {
	ft := &fakeTransport{}
	b := NewBeads(ft, "lambe@test")
	issues, err := b.Dispatchable(t.Context(), DispatchQuery{})
	if err != nil {
		t.Fatalf("Dispatchable: %v", err)
	}
	if issues != nil {
		t.Errorf("issues: got %v, want nil", issues)
	}
	if len(ft.calls) != 0 {
		t.Errorf("transport contacted %d time(s) for an empty query: %v", len(ft.calls), ft.calls)
	}
}

func TestDispatchableByStatusesAndByIDsAreDistinctReads(t *testing.T) {
	ft := &fakeTransport{}
	b := NewBeads(ft, "lambe@test")
	if _, err := b.Dispatchable(t.Context(), DispatchQuery{Statuses: []Status{"open", "ready"}}); err != nil {
		t.Fatalf("by statuses: %v", err)
	}
	if _, err := b.Dispatchable(t.Context(), DispatchQuery{IDs: []string{"x-1"}}); err != nil {
		t.Fatalf("by ids: %v", err)
	}
	if len(ft.lists) != 2 {
		t.Fatalf("list calls: got %d, want 2", len(ft.lists))
	}
	if got := ft.lists[0].Statuses; len(got) != 2 || got[0] != "open" || got[1] != "ready" {
		t.Errorf("first read statuses: got %v", got)
	}
	if got := ft.lists[1].IDs; len(got) != 1 || got[0] != "x-1" {
		t.Errorf("second read ids: got %v", got)
	}
}

func TestClassifyExitMapsTypedOutcomes(t *testing.T) {
	cases := []struct {
		code int
		want error
	}{
		{13, ErrLostRace},
		{14, ErrFrozen},
		{1, nil},
		{2, nil},
	}
	for _, tc := range cases {
		err := classifyExit(&ExitError{Code: tc.code})
		if tc.want == nil {
			if err != nil {
				t.Errorf("code %d: got %v, want nil", tc.code, err)
			}
			continue
		}
		if !errors.Is(err, tc.want) {
			t.Errorf("code %d: got %v, want wrapping %v", tc.code, err, tc.want)
		}
	}
}

func TestExitThirteenFromGuardedWriteIsLostRace(t *testing.T) {
	b := NewBeads(&guardTransport{}, "lambe@test")
	_, err := b.Claim(t.Context(), "x-1")
	if !errors.Is(err, ErrLostRace) {
		t.Fatalf("claim: got %v, want ErrLostRace", err)
	}
}

// guardTransport fails every write with bd exit 13.
type guardTransport struct{ fakeTransport }

func (g *guardTransport) Update(_ context.Context, _ UpdateRequest) (Result, error) {
	return Result{}, &ExitError{Code: 13, Stderr: "guard precondition no longer held"}
}

func TestHeartbeatFailureAfterLossIsStopSignal(t *testing.T) {
	b := NewBeads(&lostHeartbeatTransport{}, "lambe@test")
	err := b.Heartbeat(t.Context(), "x-1")
	if !errors.Is(err, ErrNotOwner) {
		t.Fatalf("heartbeat: got %v, want ErrNotOwner", err)
	}
}

// lostHeartbeatTransport answers heartbeat with bd's reclaimed/released
// claim failure: plain exit 1 with a claim-related message.
type lostHeartbeatTransport struct{ fakeTransport }

func (l *lostHeartbeatTransport) Heartbeat(_ context.Context, _ string) (Result, error) {
	return Result{}, &ExitError{
		Code:   1,
		Stdout: `{"data":{"error":"heartbeat x-1: issue already claimed by other"}}`,
	}
}

func TestEventsTruncationCarriesRebaselineWindow(t *testing.T) {
	b := NewBeads(&truncatedTransport{}, "lambe@test")
	_, err := b.Events(t.Context(), 0)
	var trunc *TruncatedError
	if !errors.As(err, &trunc) {
		t.Fatalf("Events: got %v, want TruncatedError", err)
	}
	if !errors.Is(err, ErrJournalTruncated) {
		t.Errorf("Events: %v does not wrap ErrJournalTruncated", err)
	}
	if trunc.Floor != 5 || trunc.Head != 6 {
		t.Errorf("window: got [%d..%d], want [5..6]", trunc.Floor, trunc.Head)
	}
	// Re-baseline: read from Floor onward (tail is strictly greater-than,
	// so Floor itself is already pruned) and continue.
	events, err := b.Events(t.Context(), trunc.Floor)
	if err != nil {
		t.Fatalf("re-baseline read: %v", err)
	}
	if len(events) != 1 || events[0].Seq != trunc.Head {
		t.Errorf("re-baseline events: got %+v, want seq %d", events, trunc.Head)
	}
}

// truncatedTransport reports the structured journal-truncation failure.
type truncatedTransport struct{ fakeTransport }

func (tr *truncatedTransport) Events(_ context.Context, since int64) (Result, error) {
	if since < 5 {
		return Result{}, &ExitError{Code: 1, Stdout: `{"data":{"code":"events_journal_truncated","error":"events journal truncated: checkpoint ` + fmt.Sprint(since) + ` is below the retained window [5..6]; records 1..4 were pruned","floor":5,"head":6}}`}
	}
	return Result{Raw: []byte(`{"seq":6,"ts":"2026-01-01T00:00:00Z","op":"update","issue_id":"x-1","actor":"me"}` + "\n")}, nil
}

func TestParseBdVersion(t *testing.T) {
	cases := []struct {
		in  string
		maj int
		min int
		ok  bool
	}{
		{"bd version 1.3.4 (7ef2fd05e)", 1, 3, true},
		{"bd version 1.4.0 (abc)", 1, 4, true},
		{"bd version 2.0.1 (abc)", 2, 0, true},
		{"garbage", 0, 0, false},
	}
	for _, tc := range cases {
		maj, minor, ok := parseBdVersion(tc.in)
		if ok != tc.ok || maj != tc.maj || minor != tc.min {
			t.Errorf("parseBdVersion(%q): got %d,%d,%v want %d,%d,%v", tc.in, maj, minor, ok, tc.maj, tc.min, tc.ok)
		}
	}
}

func TestCapabilitiesByVersion(t *testing.T) {
	b := NewBeads(&fakeTransport{}, "lambe@test")
	caps, err := b.Capabilities(t.Context())
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if caps.SSEWatch || caps.RetainedTombstones || caps.HTTPHeartbeat {
		t.Errorf("bd 1.3.4 must not advertise 1.4 capabilities: %+v", caps)
	}
}

func TestHTTPTransportConstructor(t *testing.T) {
	if _, err := NewHTTPTransport("http://127.0.0.1:8398"); err != nil {
		t.Errorf("valid base url rejected: %v", err)
	}
	if _, err := NewHTTPTransport("ftp://example.com"); err == nil {
		t.Errorf("non-http scheme accepted")
	}
	tr, err := NewHTTPTransport("http://127.0.0.1:8398")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tr.List(t.Context(), DispatchQuery{Statuses: []Status{"open"}}); !errors.Is(err, ErrUnsupported) {
		t.Errorf("List before daemon wiring: got %v, want ErrUnsupported", err)
	}
}
