package rex

import (
	"bytes"
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
			want:   "\x1b]7501;idle;no work\x07",
		},
		{
			name:   "working",
			status: StatusWorking,
			text:   "agent turn",
			want:   "\x1b]7501;working;agent turn\x07",
		},
		{
			name:   "waiting-on-user",
			status: StatusWaitingOnUser,
			text:   "gate question",
			want:   "\x1b]7501;waiting-on-user;gate question\x07",
		},
		{
			name:   "finished without text",
			status: StatusFinished,
			want:   "\x1b]7501;finished;\x07",
		},
		{
			name:   "failed",
			status: StatusFailed,
			text:   "watchdog killed",
			want:   "\x1b]7501;failed;watchdog killed\x07",
		},
		{
			name:   "text with semicolon is dropped to keep payload single-field",
			status: StatusIdle,
			text:   "a;b",
			want:   "\x1b]7501;idle;a;b\x07",
		},
		{
			name:   "text with bell byte is stripped",
			status: StatusIdle,
			text:   "a\x07b",
			want:   "\x1b]7501;idle;ab\x07",
		},
		{
			name:   "text with escape byte is stripped",
			status: StatusIdle,
			text:   "a\x1bb",
			want:   "\x1b]7501;idle;ab\x07",
		},
		{
			name:   "newline becomes space",
			status: StatusIdle,
			text:   "a\nb",
			want:   "\x1b]7501;idle;a b\x07",
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
		{RunStateDeferred, StatusWaitingOnUser},
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
