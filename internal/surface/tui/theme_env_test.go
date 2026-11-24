package tui

import (
	"io"
	"os"
	"testing"
	"time"
)

func TestApplyTheme(t *testing.T) {
	tests := []struct {
		name     string
		env      map[string]string
		decided  bool
		wantDark bool
	}{
		{"unset queries the terminal", nil, false, false},
		{"auto queries the terminal", map[string]string{"LAMBE_THEME": "auto"}, false, false},
		{"light", map[string]string{"LAMBE_THEME": "light"}, true, false},
		{"dark, any case", map[string]string{"LAMBE_THEME": " Dark "}, true, true},
		{"theme beats COLORFGBG", map[string]string{"LAMBE_THEME": "light", "COLORFGBG": "15;0"}, true, false},
		{"NO_COLOR", map[string]string{"NO_COLOR": "1"}, true, true},
		{"COLORFGBG dark", map[string]string{"COLORFGBG": "15;0"}, true, true},
		{"COLORFGBG light", map[string]string{"COLORFGBG": "0;15"}, true, false},
		{"COLORFGBG light 7", map[string]string{"COLORFGBG": "0;7"}, true, false},
		{"COLORFGBG three fields", map[string]string{"COLORFGBG": "15;default;0"}, true, true},
		{"COLORFGBG dark 8", map[string]string{"COLORFGBG": "7;8"}, true, true},
		{"COLORFGBG garbage", map[string]string{"COLORFGBG": "15;default"}, false, false},
		{"COLORFGBG no separator", map[string]string{"COLORFGBG": "0"}, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got, called bool
			decided := applyTheme(func(k string) string { return tt.env[k] }, func(d bool) { got, called = d, true })
			if decided != tt.decided || called != tt.decided || got != tt.wantDark {
				t.Fatalf("decided=%v called=%v dark=%v, want decided=%v dark=%v", decided, called, got, tt.decided, tt.wantDark)
			}
		})
	}
}

func TestParseOSC11(t *testing.T) {
	tests := []struct {
		reply    string
		dark, ok bool
	}{
		{"\x1b]11;rgb:0000/0000/0000\a", true, true},
		{"\x1b]11;rgb:ffff/ffff/ffff\x1b\\", false, true},
		{"\x1b]11;rgb:ff/ff/ff\a", false, true},
		{"\x1b]11;rgb:1e1e/1e1e/2e2e\a", true, true},
		{"\x1b]11;rgb:zz/00/00\a", false, false},
		{"garbage", false, false},
		{"\x1b]11;rgb:ff\a", false, false},
	}
	for _, tt := range tests {
		if dark, ok := parseOSC11(tt.reply); dark != tt.dark || ok != tt.ok {
			t.Errorf("parseOSC11(%q) = %v, %v; want %v, %v", tt.reply, dark, ok, tt.dark, tt.ok)
		}
	}
}

// TestSilentTerminalDoesNotBlock stands a pipe in for a terminal that never
// answers OSC 11: the query must give up near the budget, not after 5 s.
func TestSilentTerminalDoesNotBlock(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close(); _ = w.Close() }()
	start := time.Now()
	if _, ok := queryOSC11(io.Discard, int(r.Fd()), oscBudget); ok {
		t.Fatal("a silent terminal produced a colour")
	}
	// Generous slack for a loaded machine; the old behaviour was 5 s.
	if elapsed := time.Since(start); elapsed < oscBudget/2 || elapsed > 2*time.Second {
		t.Fatalf("query took %v, want about %v", elapsed, oscBudget)
	}
}

func TestAnsweringTerminal(t *testing.T) {
	for reply, wantDark := range map[string]bool{
		"\x1b]11;rgb:0000/0000/0000\a":     true,
		"\x1b]11;rgb:ffff/ffff/ffff\x1b\\": false,
	} {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.WriteString(reply)
		dark, ok := queryOSC11(io.Discard, int(r.Fd()), time.Second)
		_ = r.Close()
		_ = w.Close()
		if !ok || dark != wantDark {
			t.Errorf("reply %q: dark=%v ok=%v, want dark=%v", reply, dark, ok, wantDark)
		}
	}
}
