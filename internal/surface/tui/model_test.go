package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/andrew-bierman/lambe/internal/daemon"
	"github.com/andrew-bierman/lambe/internal/symphony"
)

func testSnapshot() daemon.LiveSnapshot {
	return daemon.LiveSnapshot{
		Identity:  "lambe@test",
		UpdatedAt: time.Now(),
		Runs: []daemon.LiveRun{
			{Progress: symphony.Progress{IssueID: "ac-1", Title: "first issue", Phase: "running", Attempt: 1}},
			{Progress: symphony.Progress{IssueID: "ac-2", Title: "second issue", Phase: "stopped: gate red"}},
			{Progress: symphony.Progress{IssueID: "ac-3", Title: "third issue", Phase: "retry"}},
		},
	}
}

func newSized(t *testing.T) *Model {
	t.Helper()
	m := New("/tmp/live.json")
	model, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	return model.(*Model)
}

func withSnapshot(t *testing.T, m *Model) *Model {
	t.Helper()
	model, _ := m.Update(snapshotMsg{snapshot: testSnapshot(), alive: true})
	return model.(*Model)
}

func TestNew(t *testing.T) {
	m := New("/tmp/live.json")
	if m.path != "/tmp/live.json" {
		t.Fatalf("path = %q", m.path)
	}
	if !m.loading {
		t.Fatal("new model should be loading")
	}
}

func TestWindowSizeSetsLayout(t *testing.T) {
	m := newSized(t)
	if m.width != 120 || m.height != 30 {
		t.Fatalf("dimensions = %dx%d", m.width, m.height)
	}
	if m.detail.Width <= 0 || m.detail.Height <= 0 {
		t.Fatalf("detail viewport not sized: %dx%d", m.detail.Width, m.detail.Height)
	}
}

func TestSnapshotPopulatesList(t *testing.T) {
	m := withSnapshot(t, newSized(t))
	if len(m.list.Items()) != 3 {
		t.Fatalf("items = %d, want 3", len(m.list.Items()))
	}
	if m.selectedID != "ac-1" {
		t.Fatalf("selectedID = %q, want ac-1", m.selectedID)
	}
	if !m.alive {
		t.Fatal("alive should be true")
	}
	if m.loading {
		t.Fatal("loading should be false")
	}
}

func TestNavigationMovesSelection(t *testing.T) {
	m := withSnapshot(t, newSized(t))

	model, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = model.(*Model)
	if m.selectedID != "ac-2" {
		t.Fatalf("selectedID = %q, want ac-2 after down", m.selectedID)
	}

	model, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = model.(*Model)
	if m.selectedID != "ac-3" {
		t.Fatalf("selectedID = %q, want ac-3", m.selectedID)
	}

	model, _ = m.Update(tea.KeyMsg{Type: tea.KeyUp})
	m = model.(*Model)
	if m.selectedID != "ac-2" {
		t.Fatalf("selectedID = %q, want ac-2 after up", m.selectedID)
	}
}

func TestSelectionPreservedAcrossSnapshots(t *testing.T) {
	m := withSnapshot(t, newSized(t))
	model, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = model.(*Model)
	if m.selectedID != "ac-2" {
		t.Fatalf("selectedID = %q", m.selectedID)
	}

	model, _ = m.Update(snapshotMsg{snapshot: testSnapshot(), alive: true})
	m = model.(*Model)
	if m.selectedID != "ac-2" {
		t.Fatalf("selectedID = %q after refresh, want ac-2", m.selectedID)
	}
}

func TestTabTogglesFocus(t *testing.T) {
	m := withSnapshot(t, newSized(t))
	if m.focus != focusList {
		t.Fatal("default focus should be list")
	}
	model, _ := m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = model.(*Model)
	if m.focus != focusDetail {
		t.Fatal("tab should focus detail")
	}
	model, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = model.(*Model)
	if m.focus != focusList {
		t.Fatal("tab should focus list again")
	}
}

func TestQuitKey(t *testing.T) {
	m := newSized(t)
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	if cmd == nil {
		t.Fatal("q should return a quit command")
	}
}

func TestViewContainsExpectedContent(t *testing.T) {
	m := withSnapshot(t, newSized(t))
	view := m.View()
	for _, want := range []string{"lambe", "running", "ac-1", "first issue", "quit"} {
		if !strings.Contains(view, want) {
			t.Fatalf("view missing %q:\n%s", want, view)
		}
	}
}

func TestDetailContent(t *testing.T) {
	run := daemon.LiveRun{
		Progress: symphony.Progress{
			IssueID:   "ac-1",
			Title:     "test issue",
			Phase:     "running",
			Attempt:   2,
			Turn:      3,
			Thread:    "thread-1",
			Workspace: "/tmp/ws",
		},
		Tail: "agent output\nmore output",
	}
	content := detailContent(run, 80)
	for _, want := range []string{"ac-1", "test issue", "running", "2", "3", "thread-1", "/tmp/ws", "agent output"} {
		if !strings.Contains(content, want) {
			t.Fatalf("detail content missing %q:\n%s", want, content)
		}
	}
}

func TestPhasePresentation(t *testing.T) {
	cases := []struct {
		phase    string
		wantIcon string
	}{
		{"running", "▶"},
		{"gating", "◉"},
		{"stopped: gate green", "✔"},
		{"stopped: gate red", "✘"},
		{"retry", "↻"},
		{"unknown", "○"},
	}
	for _, tc := range cases {
		icon, _ := phasePresentation(tc.phase)
		if icon != tc.wantIcon {
			t.Errorf("phase %q icon = %q, want %q", tc.phase, icon, tc.wantIcon)
		}
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("hello", 10); got != "hello" {
		t.Fatalf("truncate = %q", got)
	}
	if got := truncate("hello world", 6); got != "hello…" {
		t.Fatalf("truncate = %q", got)
	}
	if got := truncate("x", 0); got != "" {
		t.Fatalf("truncate = %q", got)
	}
}

func TestFilterLifecycle(t *testing.T) {
	m := withSnapshot(t, newSized(t))
	model, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	m = model.(*Model)
	for _, r := range "ac-2" {
		model, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = model.(*Model)
	}
	if m.list.FilterState() != list.Filtering {
		t.Fatalf("filter state = %v, want Filtering", m.list.FilterState())
	}
	if got := m.list.FilterInput.Value(); got != "ac-2" {
		t.Fatalf("filter input = %q, want ac-2", got)
	}
	model, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = model.(*Model)
	if m.list.FilterState() != list.FilterApplied {
		t.Fatalf("filter state = %v, want FilterApplied", m.list.FilterState())
	}
	model, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = model.(*Model)
	if m.list.FilterState() != list.Unfiltered {
		t.Fatalf("filter state = %v, want Unfiltered", m.list.FilterState())
	}
}

func TestEmptySnapshotShowsPlaceholder(t *testing.T) {
	m := New("/tmp/live.json")
	model, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	model, _ = model.Update(snapshotMsg{snapshot: daemon.LiveSnapshot{UpdatedAt: time.Now()}, alive: false})
	m = model.(*Model)
	if got := len(m.list.Items()); got != 0 {
		t.Fatalf("items = %d, want 0", got)
	}
	view := m.View()
	if !strings.Contains(view, "No active run") {
		t.Fatalf("empty state missing placeholder:\n%s", view)
	}
	if !strings.Contains(view, "stopped") {
		t.Fatalf("header should show stopped state:\n%s", view)
	}
}
