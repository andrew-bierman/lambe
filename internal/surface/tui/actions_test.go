package tui

import (
	"bytes"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/andrew-bierman/lambe/internal/daemon"
	"github.com/andrew-bierman/lambe/internal/symphony"
)

// fakeActions records calls instead of touching the clipboard or starting processes.
type fakeActions struct {
	copied, opened, edited []string
	copyErr                error
}

func (f *fakeActions) Copy(text string) error {
	f.copied = append(f.copied, text)
	return f.copyErr
}

func (f *fakeActions) Open(dir string) error { f.opened = append(f.opened, dir); return nil }

func (f *fakeActions) Edit(dir string) tea.Cmd {
	f.edited = append(f.edited, dir)
	return func() tea.Msg { return commandResult{status: "edited"} }
}

func actionModel(t *testing.T, workspace string) (*Model, *fakeActions) {
	t.Helper()
	m := newSized(t)
	fake := &fakeActions{}
	m.acts = fake
	m.Update(snapshotMsg{alive: true, snapshot: daemon.LiveSnapshot{
		UpdatedAt: time.Now(),
		Runs: []daemon.LiveRun{{
			Progress: symphony.Progress{IssueID: "ac-1", Title: "first", Phase: "running", Workspace: workspace},
			Tail:     "line one\x1b[31m red\nline two",
		}},
	}})
	return m, fake
}

// press sends keys and delivers any command result back to the model.
func press(m *Model, keys ...string) {
	for _, k := range keys {
		msg := runes(k)
		switch k {
		case "esc":
			msg = tea.KeyMsg{Type: tea.KeyEsc}
		case "enter":
			msg = tea.KeyMsg{Type: tea.KeyEnter}
		}
		_, cmd := m.Update(msg)
		if cmd == nil {
			continue
		}
		// Toast expiry commands sleep for seconds; only wait for quick results.
		done := make(chan tea.Msg, 1)
		go func() { done <- cmd() }()
		select {
		case res := <-done:
			if res, ok := res.(commandResult); ok {
				m.Update(res)
			}
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func footerText(m *Model) string { _, f := firstAndLastLine(m.View()); return f }

func TestCopyKeys(t *testing.T) {
	ws := t.TempDir()
	tests := []struct {
		name     string
		keys     []string
		wantCopy string
		toast    string
	}{
		{"yy copies the issue id", []string{"y", "y"}, "ac-1", "Copied ac-1"},
		{"yi copies the issue id", []string{"y", "i"}, "ac-1", "Copied ac-1"},
		{"yw copies the workspace path", []string{"y", "w"}, ws, "Copied workspace path"},
		{"yo copies the cleaned output", []string{"y", "o"}, "line one red\nline two", "Copied output of ac-1"},
		{"esc cancels", []string{"y", "esc"}, "", ""},
		{"unknown sub-key cancels", []string{"y", "z"}, "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, fake := actionModel(t, ws)
			press(m, tc.keys...)
			if tc.wantCopy == "" {
				if len(fake.copied) != 0 || m.yank {
					t.Fatalf("copied %q, yank=%v", fake.copied, m.yank)
				}
				return
			}
			if len(fake.copied) != 1 || fake.copied[0] != tc.wantCopy {
				t.Fatalf("copied %q, want %q", fake.copied, tc.wantCopy)
			}
			if !strings.Contains(footerText(m), tc.toast) {
				t.Fatalf("footer %q lacks %q", footerText(m), tc.toast)
			}
		})
	}
}

func TestCopyReportsProblemsAsToasts(t *testing.T) {
	m, fake := actionModel(t, "")
	press(m, "y", "w")
	if len(fake.copied) != 0 || !strings.Contains(footerText(m), "no workspace") {
		t.Fatalf("copied %q, footer %q", fake.copied, footerText(m))
	}
	fake.copyErr = errors.New("terminal refused")
	press(m, "y", "y")
	if !strings.Contains(footerText(m), "copy failed: terminal refused") {
		t.Fatalf("footer %q", footerText(m))
	}
}

func TestPendingYankHintsAtSubKeys(t *testing.T) {
	m, _ := actionModel(t, "")
	press(m, "y")
	if f := footerText(m); !strings.Contains(f, "w workspace") {
		t.Fatalf("no sub-key hint: %q", f)
	}
}

func TestOpenWorkspace(t *testing.T) {
	ws := t.TempDir()
	m, fake := actionModel(t, ws)
	press(m, "o")
	if len(fake.opened) != 1 || fake.opened[0] != ws {
		t.Fatalf("opened %q", fake.opened)
	}
	press(m, "O")
	if len(fake.edited) != 1 || fake.edited[0] != ws {
		t.Fatalf("edited %q", fake.edited)
	}
}

func TestOpenRefusesBadWorkspaces(t *testing.T) {
	for name, ws := range map[string]string{"empty": "", "relative": "rel/dir", "missing": "/nonexistent/lambe-test"} {
		t.Run(name, func(t *testing.T) {
			m, fake := actionModel(t, ws)
			press(m, "o", "O")
			if len(fake.opened)+len(fake.edited) != 0 {
				t.Fatalf("opened %q edited %q", fake.opened, fake.edited)
			}
			if _, ok := m.activeToast(); !ok || !m.toast.err {
				t.Fatal("expected an error toast")
			}
		})
	}
}

func TestPaletteOffersCopyAndOpen(t *testing.T) {
	ws := t.TempDir()
	m, fake := actionModel(t, ws)
	press(m, ":")
	m.palette.input.SetValue("copy path")
	matches := m.palette.matches()
	if len(matches) == 0 || matches[0].label != "Copy workspace path" || matches[0].hint != "yw" {
		t.Fatalf("matches %v", labelsOf(matches))
	}
	press(m, "enter")
	if len(fake.copied) != 1 || fake.copied[0] != ws {
		t.Fatalf("copied %q", fake.copied)
	}
	for _, label := range []string{"Open workspace in file manager", "Open workspace in $EDITOR", "Copy issue ID", "Copy last output"} {
		found := false
		for _, c := range m.paletteCommands(&commandPalette{}) {
			found = found || c.label == label
		}
		if !found {
			t.Errorf("palette lacks %q", label)
		}
	}
}

func TestSystemCopyWritesOSC52AndCapsPayload(t *testing.T) {
	var out bytes.Buffer
	a := systemActions{out: &out, getenv: noEnv}
	if err := a.Copy("hello"); err != nil {
		t.Fatal(err)
	}
	want := "\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte("hello")) + "\a"
	if out.String() != want {
		t.Fatalf("got %q, want %q", out.String(), want)
	}
	out.Reset()
	long := strings.Repeat("a", maxCopyBytes) + "TAIL"
	if err := a.Copy(long); err != nil {
		t.Fatal(err)
	}
	payload := strings.TrimSuffix(strings.TrimPrefix(out.String(), "\x1b]52;c;"), "\a")
	raw, err := base64.StdEncoding.DecodeString(payload)
	if err != nil || len(raw) != maxCopyBytes || !strings.HasSuffix(string(raw), "TAIL") {
		t.Fatalf("payload not capped to the tail: len=%d err=%v", len(raw), err)
	}
}

func noEnv(string) string { return "" }

// TestOSC52InsideTmuxUsesDCSPassthrough checks the wrapper tmux needs: the
// sequence inside ESC P tmux ; ... ESC \ with every ESC doubled.
func TestOSC52InsideTmuxUsesDCSPassthrough(t *testing.T) {
	payload := base64.StdEncoding.EncodeToString([]byte("ac-1"))
	plainSeq := "\x1b]52;c;" + payload + "\a"
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"no multiplexer", nil, plainSeq},
		{"tmux", map[string]string{"TMUX": "/tmp/tmux-501/default,123,0"}, "\x1bPtmux;\x1b\x1b]52;c;" + payload + "\a\x1b\\"},
		{"empty TMUX is not tmux", map[string]string{"TMUX": ""}, plainSeq},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			a := systemActions{out: &out, getenv: func(k string) string { return tc.env[k] }}
			if err := a.Copy("ac-1"); err != nil {
				t.Fatal(err)
			}
			if out.String() != tc.want {
				t.Fatalf("got %q, want %q", out.String(), tc.want)
			}
		})
	}
}

// TestYankPrefixTimesOut: y then silence cancels the copy prefix after
// yankTimeout and says so; a later key must not copy, and a stale timeout
// from an earlier y must not cancel a newer one.
func TestYankPrefixTimesOut(t *testing.T) {
	m, fake := actionModel(t, "")
	now, advance := fakeClock(t)
	m.now = now
	_, cmd := m.Update(runes("y"))
	if !m.yank || cmd == nil {
		t.Fatalf("y must arm the prefix and a timeout: yank=%v cmd=%v", m.yank, cmd != nil)
	}
	first := m.yankSeq
	m.Update(yankExpiredMsg{first})
	if m.yank {
		t.Fatal("the prefix survived its timeout")
	}
	if toast, ok := m.activeToast(); !ok || toast.until.IsZero() {
		t.Fatalf("timeout must replace the pending toast with a notice: %+v ok=%v", toast, ok)
	}
	advance(time.Second)
	m.Update(runes("y")) // a key after the timeout is a fresh y, not a copy choice
	if len(fake.copied) != 0 || !m.yank {
		t.Fatalf("copied %q, yank=%v", fake.copied, m.yank)
	}
	m.Update(yankExpiredMsg{first}) // the first press's timer fires late
	if !m.yank {
		t.Fatal("a stale timeout cancelled a newer prefix")
	}
	press(m, "y")
	if m.yank || len(fake.copied) != 1 {
		t.Fatalf("second key must complete the copy: yank=%v copied=%q", m.yank, fake.copied)
	}
	m.Update(yankExpiredMsg{m.yankSeq}) // after completion a timeout is ignored
	if _, ok := m.activeToast(); ok && strings.Contains(m.toast.text, "cancelled") {
		t.Fatal("a completed copy was reported as cancelled")
	}
}
