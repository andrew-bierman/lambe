package tracker

import (
	"encoding/json"
	"testing"
)

func TestParseStatus(t *testing.T) {
	t.Parallel()
	tests := []struct {
		raw  string
		want Status
	}{
		{"open", StatusOpen},
		{"ready", StatusReady},
		{"in_progress", StatusInProgress},
		{"blocked", StatusBlocked},
		{"deferred", StatusDeferred},
		{"closed", StatusClosed},
		{"needs-human", StatusNeedsHuman},
		{"", ""},
		{"done", StatusUnknown},
		{"Open", StatusUnknown}, // case matters: bd emits lowercase
		{"unknown", StatusUnknown},
	}
	for _, tt := range tests {
		if got := ParseStatus(tt.raw); got != tt.want {
			t.Errorf("ParseStatus(%q) = %q, want %q", tt.raw, got, tt.want)
		}
	}
}

func TestStatusKnown(t *testing.T) {
	t.Parallel()
	for _, s := range []Status{StatusReady, StatusOpen, StatusInProgress, StatusBlocked, StatusDeferred, StatusClosed, StatusNeedsHuman} {
		if !s.Known() {
			t.Errorf("%q.Known() = false", s)
		}
	}
	for _, s := range []Status{"", StatusUnknown, "done"} {
		if s.Known() {
			t.Errorf("%q.Known() = true", s)
		}
	}
}

// TestStatusJSONWire pins the wire form: a Status marshals as the bare
// string it replaced, and decoding validates through ParseStatus.
func TestStatusJSONWire(t *testing.T) {
	t.Parallel()
	b, err := json.Marshal(struct{ S Status }{StatusInProgress})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"S":"in_progress"}` {
		t.Fatalf("marshal = %s", b)
	}
	var got struct{ S Status }
	if err := json.Unmarshal([]byte(`{"S":"removed"}`), &got); err != nil {
		t.Fatal(err)
	}
	if got.S != StatusUnknown {
		t.Errorf("unknown wire status decoded to %q, want %q", got.S, StatusUnknown)
	}
	if err := json.Unmarshal([]byte(`{"S":3}`), &got); err == nil {
		t.Error("non-string status decoded without error")
	}
}

// TestToIssueKeepsRawState pins that an unrecognized bd status is typed
// Unknown while State keeps bd's text.
func TestToIssueKeepsRawState(t *testing.T) {
	t.Parallel()
	issue := issueJSON{ID: "x-1", Status: "pinned"}.toIssue()
	if issue.Status != StatusUnknown || issue.State != "pinned" || issue.State.Status() != StatusUnknown {
		t.Fatalf("status=%q state=%q", issue.Status, issue.State)
	}
	if got := (issueJSON{ID: "x-2", Status: "closed"}).toIssue(); got.Status != StatusClosed || got.State.String() != "closed" {
		t.Fatalf("closed issue: status=%q state=%q", got.Status, got.State)
	}
}
