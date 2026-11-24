package tui

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/andrew-bierman/lambe/internal/daemon"
	"github.com/andrew-bierman/lambe/internal/symphony"
)

func filterItem(id, title, phase, wf string) runItem {
	return runItem{run: daemon.LiveRun{Progress: symphony.Progress{IssueID: id, Title: title, Phase: phase, Workflow: wf}}}
}

func filterIDs(items []runItem, query string) []string {
	targets := make([]string, len(items))
	for i, it := range items {
		targets[i] = it.FilterValue()
	}
	var ids []string
	for _, r := range filterRuns(query, targets) {
		ids = append(ids, items[r.Index].run.IssueID)
	}
	return ids
}

func TestFilterRuns(t *testing.T) {
	frozenClock(t)
	items := []runItem{
		filterItem("lambe-173", "Tidy the docs", "running", "docs"),
		filterItem("lambe-17", "Fix the flaky test", "stopped: gate green", "default"),
		filterItem("lambe-9", "Escalated passed gate review", "stopped: gate escalation", "default"),
		filterItem("lambe-4", "Upgrade deps", "retry", "deps"),
		filterItem("lambe-5", "Unblock release", "blocked", ""),
	}
	tests := []struct {
		query string
		want  []string
	}{
		{"", []string{"lambe-173", "lambe-17", "lambe-9", "lambe-4", "lambe-5"}},
		{"passed", []string{"lambe-17", "lambe-9"}}, // phase word first, then the title
		{"phase:passed", []string{"lambe-17"}},
		{"escalated", []string{"lambe-9"}},
		{"phase:escalated", []string{"lambe-9"}},
		{"retry", []string{"lambe-4"}},
		{"blocked", []string{"lambe-5"}},
		{"lambe-17", []string{"lambe-17", "lambe-173"}}, // exact id beats prefix
		{"id:lambe-17", []string{"lambe-17", "lambe-173"}},
		{"wf:docs", []string{"lambe-173"}},
		{"workflow:deps", []string{"lambe-4"}},
		{"wf:default phase:passed", []string{"lambe-17"}},
		{"FLAKY", []string{"lambe-17"}},
		{"flaky running", nil}, // all terms must match
		{"phase:", []string{"lambe-173", "lambe-17", "lambe-9", "lambe-4", "lambe-5"}},
		{"nothing-like-this", nil},
		{"unknown:field", nil},
	}
	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			if got := filterIDs(items, tt.query); !slices.Equal(got, tt.want) {
				t.Fatalf("filter %q = %v, want %v", tt.query, got, tt.want)
			}
		})
	}
}

func TestFilterMatchesStalledWord(t *testing.T) {
	now := frozenClock(t)
	stalled := runItem{live: true, run: daemon.LiveRun{
		Progress:     symphony.Progress{IssueID: "lambe-1", Phase: "running"},
		LastOutputAt: now.Add(-stallAfter - 5*60e9),
	}}
	if got := filterIDs([]runItem{stalled}, "stalled"); len(got) != 1 {
		t.Fatalf("/stalled should find the stalled row, got %v", got)
	}
}

func FuzzFilter(f *testing.F) {
	for _, seed := range []string{"", "phase:", ":", "wf:x id:", "PASSED", "é:ß", "a b c", "\x1f", "title:\xff"} {
		f.Add(seed, "lambe-1\x1fTitle\x1fpassed\x1fwf")
	}
	f.Fuzz(func(t *testing.T, query, target string) {
		targets := []string{target, "lambe-2\x1fOther\x1fstalled\x1f", ""}
		seen := map[int]bool{}
		for _, r := range filterRuns(query, targets) {
			if r.Index < 0 || r.Index >= len(targets) || seen[r.Index] {
				t.Fatalf("bad rank %v for %q", r, query)
			}
			seen[r.Index] = true
		}
	})
}

func typeFilter(m *Model, text string) {
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	for _, r := range text {
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
}

func TestFilterUIAndViewSwitch(t *testing.T) {
	now := frozenClock(t)
	m := demoModel(now)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'3'}})
	typeFilter(m, "passed")
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	view := ansi.Strip(m.View())
	if !strings.Contains(view, "/ passed") || !strings.Contains(view, "(2 of ") {
		t.Fatalf("missing query or count:\n%s", view)
	}
	if strings.Contains(view, "escalated") {
		t.Fatalf("/passed matched an escalated row:\n%s", view)
	}
	// The filter survives switching views.
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'1'}})
	if got := m.list.FilterValue(); got != "passed" {
		t.Fatalf("filter lost on view switch: %q", got)
	}
	for _, it := range m.list.VisibleItems() {
		if w := phaseWord(it.(runItem).run.Phase, false); w != "passed" {
			t.Fatalf("queue view shows %q under /passed", w)
		}
	}
	// esc clears it.
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m.list.FilterValue() != "" || len(m.list.VisibleItems()) == 0 {
		t.Fatalf("esc did not clear the filter")
	}
}

func TestFilterEmptyAndGolden(t *testing.T) {
	now := frozenClock(t)
	for _, size := range [][2]int{{40, 24}, {100, 30}} {
		w, h := size[0], size[1]
		m := demoModel(now)
		m.Update(tea.WindowSizeMsg{Width: w, Height: h})
		typeFilter(m, "stalled")
		m.Update(tea.KeyMsg{Type: tea.KeyEnter})
		fits(t, m.View(), w, h)
		checkGolden(t, fmt.Sprintf("filter-phase-%dx%d", w, h), plain(m.View()))

		m = demoModel(now)
		m.Update(tea.WindowSizeMsg{Width: w, Height: h})
		typeFilter(m, "zzzz") // enter on zero matches cancels the filter, so the empty state shows while typing
		fits(t, m.View(), w, h)
		if !strings.Contains(ansi.Strip(m.View()), "No runs match /zzzz") {
			t.Fatalf("missing empty state:\n%s", ansi.Strip(m.View()))
		}
		checkGolden(t, fmt.Sprintf("filter-empty-%dx%d", w, h), plain(m.View()))
	}
}
