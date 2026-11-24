package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/andrew-bierman/lambe/internal/daemon"
	"github.com/andrew-bierman/lambe/internal/symphony"
)

// fuzzKeys is the key alphabet a fuzz input indexes into.
var fuzzKeys = []tea.KeyMsg{
	{Type: tea.KeyRunes, Runes: []rune{'1'}},
	{Type: tea.KeyRunes, Runes: []rune{'2'}},
	{Type: tea.KeyRunes, Runes: []rune{'3'}},
	{Type: tea.KeyDown},
	{Type: tea.KeyUp},
	{Type: tea.KeyEnter},
	{Type: tea.KeyEsc},
	{Type: tea.KeyTab},
	{Type: tea.KeyRunes, Runes: []rune{'?'}},
	{Type: tea.KeyRunes, Runes: []rune{'/'}},
	{Type: tea.KeyRunes, Runes: []rune{':'}},
	{Type: tea.KeyRunes, Runes: []rune{'G'}},
	{Type: tea.KeyRunes, Runes: []rune{'g'}},
	{Type: tea.KeyRunes, Runes: []rune{'y'}},
	{Type: tea.KeyRunes, Runes: []rune{'a'}},
	{Type: tea.KeyRunes, Runes: []rune{'x'}},
	{Type: tea.KeyPgDown},
	{Type: tea.KeyBackspace},
}

// FuzzViewFits feeds arbitrary run text, window sizes and key presses to the
// dashboard. It must never panic and no rendered line may be wider than the
// window or taller than it (measured with lipgloss.Width).
func FuzzViewFits(f *testing.F) {
	f.Add("Verify the release", "lambe-1", "running", "wf", "tail\nline", uint8(40), uint8(24), []byte{0, 3, 5})
	f.Add("日本語🚀 é", "id", "stopped: gate escalation", "ワーク", strings.Repeat("x", 300), uint8(28), uint8(6), []byte{8, 9, 4})
	f.Add("\x1b[31mred\x07bell\r\n", "\x00", "retry", "", "\x1b]52;c;AAAA\x07", uint8(1), uint8(1), []byte{2, 5, 6})
	f.Add(strings.Repeat("שלום ", 50), strings.Repeat("i", 200), "interrupted", strings.Repeat("w", 80), "", uint8(200), uint8(60), []byte{1, 10, 5})
	f.Fuzz(func(t *testing.T, title, id, phase, workflow, tail string, w, h uint8, keys []byte) {
		// live.json goes through encoding/json, which turns invalid UTF-8 into U+FFFD.
		clean := func(s string) string { return strings.ToValidUTF8(s, "\uFFFD") }
		title, id, phase, workflow, tail = clean(title), clean(id), clean(phase), clean(workflow), clean(tail)
		width, height := int(w), int(h)
		if width < 1 || height < 1 {
			t.Skip()
		}
		now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
		mk := func(i int, p string) daemon.LiveRun {
			return daemon.LiveRun{
				Progress:     symphony.Progress{IssueID: fmt.Sprintf("%s%d", id, i), Title: title, Phase: p, Workflow: workflow, Attempt: i, Thread: workflow, Workspace: id, LastGate: tail},
				UpdatedAt:    now.Add(-time.Duration(i) * time.Minute),
				LastOutputAt: now.Add(-time.Duration(i) * 10 * time.Minute),
				Tail:         tail,
			}
		}
		snap := daemon.LiveSnapshot{UpdatedAt: now, Runs: []daemon.LiveRun{mk(0, phase), mk(1, "running"), mk(2, "retry")}, History: []daemon.LiveRun{mk(3, phase), mk(4, "stopped: gate green")}}
		m := at(New(""), now)
		m.Update(snapshotMsg{snapshot: snap, alive: true})
		m.Update(tea.WindowSizeMsg{Width: width, Height: height})
		check := func(step string) {
			view := m.View()
			if !utf8.ValidString(ansi.Strip(view)) {
				t.Fatalf("%s: view is not valid UTF-8", step)
			}
			lines := strings.Split(view, "\n")
			if len(lines) > height {
				t.Fatalf("%s: %d lines in a %dx%d window", step, len(lines), width, height)
			}
			for i, l := range lines {
				if lw := lipgloss.Width(l); lw > width {
					t.Fatalf("%s: line %d is %d cols in a %dx%d window: %q", step, i, lw, width, height, ansi.Strip(l))
				}
			}
		}
		check("initial")
		for i, k := range keys {
			m.Update(fuzzKeys[int(k)%len(fuzzKeys)])
			check(fmt.Sprintf("key %d", i))
		}
	})
}
