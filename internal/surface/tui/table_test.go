package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/andrew-bierman/lambe/internal/daemon"
	"github.com/andrew-bierman/lambe/internal/symphony"
)

func TestRowAgeArchivedAttempts(t *testing.T) {
	now := frozenClock(t)
	run := daemon.LiveRun{
		Progress:  symphony.Progress{IssueID: "x-1", Phase: "retry", WakeAt: now.Add(time.Minute)},
		UpdatedAt: now.Add(-15 * time.Minute),
	}
	tests := []struct {
		name string
		item runItem
		want string
	}{
		{"live retry counts down", runItem{run: run, live: true}, "in 1m"},
		{"archived retry shows time since update", runItem{run: run, live: true, attempts: 1}, "15m"},
		{"archived interrupted", runItem{run: daemon.LiveRun{Progress: symphony.Progress{Phase: "interrupted"}, UpdatedAt: now.Add(-3 * time.Hour)}, live: true, attempts: 2}, "3h"},
	}
	for _, tt := range tests {
		if got := tt.item.rowAge(now); got != tt.want {
			t.Errorf("%s: age %q, want %q", tt.name, got, tt.want)
		}
	}
}

// TestTableColumnsAlign checks every row (including the ×N history rows and an
// interrupted one) ends in the same AGE column and shows the full phase word.
func TestTableColumnsAlign(t *testing.T) {
	now := frozenClock(t)
	snap := demoSnapshot(now)
	snap.History = append(snap.History, daemon.LiveRun{
		Progress:  symphony.Progress{IssueID: "demo-60", Title: "Restart-interrupted job", Phase: "interrupted", Attempt: 1},
		UpdatedAt: now.Add(-7 * time.Minute),
	})
	for _, width := range []int{60, 100, 140} {
		m := New("")
		m.Update(snapshotMsg{snapshot: snap, alive: true})
		m.Update(tea.WindowSizeMsg{Width: width, Height: 30})
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'3'}})
		cols := resolveCols(m.tableWidth, m.idWidth())
		var sawSuffix, sawInterrupted bool
		for _, line := range strings.Split(ansi.Strip(m.renderTable(m.tableWidth, 12)), "\n") {
			if !strings.Contains(line, "demo-") {
				continue
			}
			if w := lipgloss.Width(line); w != m.tableWidth {
				t.Fatalf("width %d: row is %d cols: %q", width, w, line)
			}
			age := strings.TrimSpace(ansi.Cut(line, m.tableWidth-ageColWidth, m.tableWidth))
			if strings.Contains(line, "retry") && strings.Contains(line, "demo-44") && age != "15m" {
				t.Errorf("width %d: archived retry age %q: %q", width, age, line)
			}
			if strings.Contains(line, "×3") {
				sawSuffix = true
				if age != "20m" {
					t.Errorf("width %d: ×3 row age shifted: %q", width, line)
				}
			}
			if strings.Contains(line, "interrupted") {
				sawInterrupted = true
				if age != "7m" {
					t.Errorf("width %d: interrupted age %q: %q", width, age, line)
				}
			}
		}
		if !sawSuffix || !sawInterrupted {
			t.Fatalf("width %d: suffix=%v interrupted=%v (cols %+v)", width, sawSuffix, sawInterrupted, cols)
		}
	}
}
