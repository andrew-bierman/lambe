package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/andrew-bierman/lambe/internal/daemon"
	"github.com/andrew-bierman/lambe/internal/symphony"
)

func manyRunsSnapshot(n int) daemon.LiveSnapshot {
	now := time.Now()
	titles := []string{"Verify the release build", "日本語の長いタイトルで幅を確認する", "Emoji 🚀🔥 heading that runs long past the edge", "x"}
	s := daemon.LiveSnapshot{UpdatedAt: now}
	for i := range n {
		run := daemon.LiveRun{Progress: symphony.Progress{
			IssueID: fmt.Sprintf("wf%d-%03d", i%3, i), Title: titles[i%len(titles)],
			Phase: "running", Attempt: 1, Workflow: fmt.Sprintf("workflow-%d", i%3),
		}, LastOutputAt: now}
		if i%7 == 3 {
			run.LastOutputAt = now.Add(-9 * time.Minute)
		}
		s.Runs = append(s.Runs, run)
	}
	return s
}

// TestLayoutInvariants renders at phone and desktop sizes and asserts the
// frame never exceeds the window and the selection stays on screen.
func TestLayoutInvariants(t *testing.T) {
	for _, size := range [][2]int{{40, 24}, {40, 12}, {120, 40}, {200, 50}} {
		w, h := size[0], size[1]
		t.Run(fmt.Sprintf("%dx%d", w, h), func(t *testing.T) {
			m := New("/tmp/live.json")
			m.Update(tea.WindowSizeMsg{Width: w, Height: h})
			m.Update(snapshotMsg{snapshot: manyRunsSnapshot(40), alive: true})
			check := func(step string) {
				t.Helper()
				view := m.View()
				if vw, vh := lipgloss.Size(view); vw > w || vh > h {
					t.Fatalf("%s: view %dx%d exceeds %dx%d", step, vw, vh, w, h)
				}
				if run, ok := m.selectedRun(); ok &&
					!strings.Contains(ansi.Strip(view), run.IssueID) {
					t.Fatalf("%s: selected %s is off-screen:\n%s", step, run.IssueID, ansi.Strip(view))
				}
			}
			check("initial")
			for i := range 45 {
				m.Update(tea.KeyMsg{Type: tea.KeyUp})
				if i%11 == 0 {
					check(fmt.Sprintf("up %d", i))
				}
			}
			check("after up")
			m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'G'}})
			check("G")
			for range 50 {
				m.Update(tea.KeyMsg{Type: tea.KeyDown})
			}
			check("after down")
			if !strings.Contains(ansi.Strip(m.View()), "⚠") {
				// The stalled run may be scrolled out; the header must still flag it.
				t.Fatalf("stalled runs are not flagged anywhere:\n%s", ansi.Strip(m.View()))
			}
		})
	}
}

func TestStalledRunsAreFlagged(t *testing.T) {
	now := time.Now()
	run := daemon.LiveRun{Progress: symphony.Progress{IssueID: "a-1", Phase: "running"}, LastOutputAt: now.Add(-6 * time.Minute)}
	if got := stalledFor(run, true, now); got != 6*time.Minute {
		t.Fatalf("stalledFor = %v", got)
	}
	if stalledFor(run, false, now) != 0 {
		t.Fatal("archived runs must not be flagged as stalled")
	}
	run.LastOutputAt = now.Add(-10 * time.Second)
	if stalledFor(run, true, now) != 0 {
		t.Fatal("fresh output flagged as stalled")
	}
	run.Phase, run.LastOutputAt = "retry", now.Add(-time.Hour)
	if stalledFor(run, true, now) != 0 {
		t.Fatal("waiting runs are not stalls")
	}
	if got := shortAge(4 * time.Minute); got != "4m" {
		t.Fatalf("shortAge = %q", got)
	}
}

func TestTruncateIsRuneSafe(t *testing.T) {
	if got := truncate("日本語日本語", 5); lipgloss.Width(got) > 5 || strings.ContainsRune(got, '�') {
		t.Fatalf("truncate = %q", got)
	}
}
