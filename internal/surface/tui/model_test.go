package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/andrew-bierman/lambe/internal/daemon"
	"github.com/andrew-bierman/lambe/internal/symphony"
)

func TestNew(t *testing.T) {
	m := New("/tmp/live.json")
	if m.path != "/tmp/live.json" {
		t.Fatalf("path = %q, want %q", m.path, "/tmp/live.json")
	}
	if !m.loading {
		t.Fatal("new model should be loading")
	}
}

func TestWindowSizeSetsLayout(t *testing.T) {
	m := New("/tmp/live.json")
	model, cmd := m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	mm := model.(*Model)
	if cmd != nil {
		t.Fatal("window size should not return a command")
	}
	if mm.width != 120 || mm.height != 30 {
		t.Fatalf("dimensions = %dx%d, want 120x30", mm.width, mm.height)
	}
	if mm.listWidth < minListWidth {
		t.Fatalf("listWidth = %d, want at least %d", mm.listWidth, minListWidth)
	}
	if mm.vp.Width == 0 || mm.vp.Height == 0 {
		t.Fatalf("viewport not sized: %dx%d", mm.vp.Width, mm.vp.Height)
	}
}

func TestSnapshotUpdatesRuns(t *testing.T) {
	m := New("/tmp/live.json")
	model, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m = model.(*Model)

	snapshot := daemon.LiveSnapshot{
		Identity:  "lambe@test",
		UpdatedAt: time.Now(),
		Runs: []daemon.LiveRun{
			{Progress: symphony.Progress{IssueID: "ac-1", Title: "first", Phase: "running"}},
			{Progress: symphony.Progress{IssueID: "ac-2", Title: "second", Phase: "stopped: gate red"}},
		},
	}
	model, cmd := m.Update(snapshotMsg{snapshot: snapshot, alive: true})
	mm := model.(*Model)
	if cmd != nil {
		t.Fatal("snapshot update should not return a command")
	}
	if len(mm.visibleRuns()) != 2 {
		t.Fatalf("runs = %d, want 2", len(mm.visibleRuns()))
	}
	if !mm.alive {
		t.Fatal("alive should be true")
	}
	if mm.loading {
		t.Fatal("loading should be false after snapshot")
	}
}

func TestNavigation(t *testing.T) {
	m := New("/tmp/live.json")
	model, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m = model.(*Model)
	model, _ = m.Update(snapshotMsg{
		snapshot: daemon.LiveSnapshot{
			UpdatedAt: time.Now(),
			Runs: []daemon.LiveRun{
				{Progress: symphony.Progress{IssueID: "ac-1", Phase: "running"}},
				{Progress: symphony.Progress{IssueID: "ac-2", Phase: "running"}},
				{Progress: symphony.Progress{IssueID: "ac-3", Phase: "running"}},
			},
		},
	})
	mm := model.(*Model)

	if mm.selected != 0 {
		t.Fatalf("selected = %d, want 0", mm.selected)
	}

	model, _ = mm.Update(tea.KeyMsg{Type: tea.KeyDown})
	mm = model.(*Model)
	if mm.selected != 1 {
		t.Fatalf("selected = %d, want 1 after down", mm.selected)
	}

	model, _ = mm.Update(tea.KeyMsg{Type: tea.KeyDown})
	mm = model.(*Model)
	model, _ = mm.Update(tea.KeyMsg{Type: tea.KeyDown})
	mm = model.(*Model)
	if mm.selected != 2 {
		t.Fatalf("selected = %d, want 2 at bottom", mm.selected)
	}

	model, _ = mm.Update(tea.KeyMsg{Type: tea.KeyUp})
	mm = model.(*Model)
	if mm.selected != 1 {
		t.Fatalf("selected = %d, want 1 after up", mm.selected)
	}

	model, _ = mm.Update(tea.KeyMsg{Type: tea.KeyEnd})
	mm = model.(*Model)
	if mm.selected != 2 {
		t.Fatalf("selected = %d, want 2 after end", mm.selected)
	}

	model, _ = mm.Update(tea.KeyMsg{Type: tea.KeyHome})
	mm = model.(*Model)
	if mm.selected != 0 {
		t.Fatalf("selected = %d, want 0 after home", mm.selected)
	}
}

func TestQuitKey(t *testing.T) {
	m := New("/tmp/live.json")
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	if cmd == nil {
		t.Fatal("q should return a quit command")
	}
}

func TestViewContainsExpectedContent(t *testing.T) {
	m := New("/tmp/live.json")
	model, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m = model.(*Model)
	model, _ = m.Update(snapshotMsg{
		snapshot: daemon.LiveSnapshot{
			Identity:  "lambe@test",
			UpdatedAt: time.Now(),
			Runs: []daemon.LiveRun{
				{Progress: symphony.Progress{IssueID: "ac-1", Title: "test issue", Phase: "running"}},
			},
		},
		alive: true,
	})
	mm := model.(*Model)

	view := mm.View()
	for _, want := range []string{"lambe", "running", "ac-1", "test issue", "navigate", "quit"} {
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
		t.Fatalf("truncate = %q, want hello", got)
	}
	if got := truncate("hello world", 6); got != "hello…" {
		t.Fatalf("truncate = %q, want hello…", got)
	}
	if got := truncate("x", 0); got != "" {
		t.Fatalf("truncate = %q, want empty", got)
	}
}
