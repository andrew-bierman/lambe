package rex

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
)

func TestEmitStatus(t *testing.T) {
	tests := []struct {
		name   string
		status Status
		text   string
		want   string
	}{
		{
			name:   "idle with text",
			status: StatusIdle,
			text:   "no work",
			want:   "\x1b]7501;state=idle:app=lambe:msg=" + base64.StdEncoding.EncodeToString([]byte("no work")) + "\x1b\\",
		},
		{
			name:   "working",
			status: StatusWorking,
			text:   "agent turn",
			want:   "\x1b]7501;state=working:app=lambe:msg=" + base64.StdEncoding.EncodeToString([]byte("agent turn")) + "\x1b\\",
		},
		{
			name:   "waiting-on-user",
			status: StatusWaitingOnUser,
			text:   "gate question",
			want:   "\x1b]7501;state=blocked:app=lambe:msg=" + base64.StdEncoding.EncodeToString([]byte("gate question")) + "\x1b\\",
		},
		{
			name:   "finished without text",
			status: StatusFinished,
			want:   "\x1b]7501;state=done:app=lambe:msg=" + base64.StdEncoding.EncodeToString([]byte("")) + "\x1b\\",
		},
		{
			name:   "failed",
			status: StatusFailed,
			text:   "watchdog killed",
			want:   "\x1b]7501;state=error:app=lambe:msg=" + base64.StdEncoding.EncodeToString([]byte("watchdog killed")) + "\x1b\\",
		},
		{
			name:   "text with semicolon is dropped to keep payload single-field",
			status: StatusIdle,
			text:   "a;b",
			want:   "\x1b]7501;state=idle:app=lambe:msg=" + base64.StdEncoding.EncodeToString([]byte("a;b")) + "\x1b\\",
		},
		{
			name:   "text with bell byte is stripped",
			status: StatusIdle,
			text:   "a\x07b",
			want:   "\x1b]7501;state=idle:app=lambe:msg=" + base64.StdEncoding.EncodeToString([]byte("ab")) + "\x1b\\",
		},
		{
			name:   "text with escape byte is stripped",
			status: StatusIdle,
			text:   "a\x1bb",
			want:   "\x1b]7501;state=idle:app=lambe:msg=" + base64.StdEncoding.EncodeToString([]byte("ab")) + "\x1b\\",
		},
		{
			name:   "newline becomes space",
			status: StatusIdle,
			text:   "a\nb",
			want:   "\x1b]7501;state=idle:app=lambe:msg=" + base64.StdEncoding.EncodeToString([]byte("a b")) + "\x1b\\",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := EmitStatus(&buf, tt.status, tt.text); err != nil {
				t.Fatalf("EmitStatus: %v", err)
			}
			if buf.String() != tt.want {
				t.Errorf("emitted %q, want %q", buf.String(), tt.want)
			}
		})
	}
}

func TestMapRunState(t *testing.T) {
	tests := []struct {
		state RunState
		want  Status
	}{
		{RunStateIdle, StatusIdle},
		{RunStateRunning, StatusWorking},
		{RunStateDeferred, StatusIdle},
		{RunStateSettled, StatusFinished},
		{RunStateWatchdogKilled, StatusFailed},
	}
	for _, tt := range tests {
		t.Run(tt.state.String(), func(t *testing.T) {
			got := MapRunState(tt.state)
			if got != tt.want {
				t.Errorf("MapRunState(%s) = %v, want %v", tt.state, got, tt.want)
			}
		})
	}
}

func TestStatusString(t *testing.T) {
	if !strings.HasPrefix(StatusWorking.String(), "working") {
		t.Errorf("unexpected status string %q", StatusWorking)
	}
}

func TestStatusMessageLimitsAndControls(t *testing.T) {
	var buf bytes.Buffer
	if err := EmitStatus(&buf, StatusWorking, "\u009b\x00"+strings.Repeat("🦖", 1000)); err != nil {
		t.Fatal(err)
	}
	seq := strings.TrimSuffix(strings.TrimPrefix(buf.String(), "\x1b]7501;state=working:app=lambe:msg="), "\x1b\\")
	text, err := base64.StdEncoding.DecodeString(seq)
	if err != nil || len(text) > 2048 || strings.ContainsAny(string(text), "\u009b\x00") {
		t.Fatalf("invalid status payload: size=%d err=%v", len(text), err)
	}
}

func TestEmitPerRunRecordAndKind(t *testing.T) {
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	tests := []struct {
		name string
		r    Report
		want string
	}{
		{
			name: "blocked gate escalation has permission kind and a run id",
			r:    Report{Status: StatusWaitingOnUser, Block: BlockGateEscalated, ID: "lambe-ac1", Title: "lambe-ac1", Msg: "gate escalation"},
			want: "\x1b]7501;state=blocked:kind=permission:id=lambe/lambe-ac1:app=lambe:msg=" + b64("gate escalation") + ":title=" + b64("lambe-ac1") + "\x1b\\",
		},
		{
			name: "needs-human and stalled both ask a question",
			r:    Report{Status: StatusWaitingOnUser, Block: BlockStalled, ID: "x", Msg: "m"},
			want: "\x1b]7501;state=blocked:kind=question:id=lambe/x:app=lambe:msg=" + b64("m") + "\x1b\\",
		},
		{
			name: "kind is only for blocked",
			r:    Report{Status: StatusWorking, Block: BlockNeedsHuman, ID: "x", Msg: "m"},
			want: "\x1b]7501;state=working:id=lambe/x:app=lambe:msg=" + b64("m") + "\x1b\\",
		},
		{
			name: "illegal id bytes are replaced and the segment is capped at 32",
			r:    Report{Status: StatusIdle, ID: "a b/c:d;" + strings.Repeat("z", 40), Msg: ""},
			want: "\x1b]7501;state=idle:id=lambe/a-b-c-d-" + strings.Repeat("z", 24) + ":app=lambe:msg=\x1b\\",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := Emit(&buf, tt.r); err != nil {
				t.Fatal(err)
			}
			if buf.String() != tt.want {
				t.Errorf("emitted %q\nwant    %q", buf.String(), tt.want)
			}
		})
	}
}

func TestClear(t *testing.T) {
	var buf bytes.Buffer
	if err := Clear(&buf, "ac-1"); err != nil {
		t.Fatal(err)
	}
	if want := "\x1b]7501;state=clear:id=lambe/ac-1\x1b\\"; buf.String() != want {
		t.Errorf("run clear = %q, want %q", buf.String(), want)
	}
	buf.Reset()
	if err := Clear(&buf, ""); err != nil {
		t.Fatal(err)
	}
	if want := "\x1b]7501;state=clear:id=lambe\x1b\\"; buf.String() != want {
		t.Errorf("clear all = %q, want %q", buf.String(), want)
	}
}

func TestMapRunStateBlockedAndFailed(t *testing.T) {
	if got := MapRunState(RunStateBlocked); got != StatusWaitingOnUser {
		t.Errorf("blocked = %v", got)
	}
	if got := MapRunState(RunStateFailed); got != StatusFailed {
		t.Errorf("failed = %v", got)
	}
}
