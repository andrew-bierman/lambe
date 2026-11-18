package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

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

func TestFilteredSelectionSurvivesSnapshots(t *testing.T) {
	snapshot := testSnapshot()
	snapshot.Runs[1].Title = "matching second issue"
	snapshot.Runs[2].Title = "matching third issue"
	m := newSized(t)
	m.Update(snapshotMsg{snapshot: snapshot, alive: true})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("matching")})
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m.Update(tea.KeyMsg{Type: tea.KeyDown})
	if m.selectedID != "ac-3" {
		m.Update(tea.KeyMsg{Type: tea.KeyUp})
	}
	if run, ok := m.selectedRun(); !ok || run.IssueID != "ac-3" {
		t.Fatalf("expected ac-3 before refresh, got %q (%v)", run.IssueID, ok)
	}

	snapshot.Runs[2].Tail = "fresh output"
	for range 2 {
		_, refresh := m.Update(snapshotMsg{snapshot: snapshot, alive: true})
		if refresh != nil {
			m.Update(refresh())
		}
		if run, ok := m.selectedRun(); !ok || run.IssueID != "ac-3" || run.Tail != "fresh output" {
			t.Fatalf("expected refreshed ac-3, got %q with output %q (%v)", run.IssueID, run.Tail, ok)
		}
	}
	snapshot.Runs = snapshot.Runs[:2]
	m.Update(snapshotMsg{snapshot: snapshot, alive: true})
	if run, ok := m.selectedRun(); !ok || run.IssueID != "ac-2" || m.selectedID != "ac-2" {
		t.Fatalf("expected remaining match ac-2 after removal, got %q (%v)", run.IssueID, ok)
	}
}

func TestFilterIgnoresOutOfOrderResults(t *testing.T) {
	m := newSized(t)
	snapshot := testSnapshot()
	m.Update(snapshotMsg{snapshot: snapshot, alive: true})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	_, older := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("second")})
	m.Update(tea.KeyMsg{Type: tea.KeyHome})
	m.Update(tea.KeyMsg{Type: tea.KeyDelete})
	if value, pos := m.list.FilterInput.Value(), m.list.FilterInput.Position(); value != "econd" || pos != 0 {
		t.Fatalf("middle edit left filter %q at cursor %d, want econd at cursor 0", value, pos)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyEnd})
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlU})
	_, newer := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("third")})
	if run, ok := m.selectedRun(); !ok || run.IssueID != "ac-3" {
		t.Fatalf("expected current query to match immediately, got %q (%v)", run.IssueID, ok)
	}

	snapshot.Runs[2].Tail = "fresh output"
	m.Update(snapshotMsg{snapshot: snapshot, alive: true})
	for i, pending := range []tea.Cmd{newer, older} {
		result := pending()
		if batch, ok := result.(tea.BatchMsg); ok {
			for _, cmd := range batch {
				if matches, ok := cmd().(list.FilterMatchesMsg); ok {
					result = matches
					break
				}
			}
		}
		matches, ok := result.(list.FilterMatchesMsg)
		if !ok {
			t.Fatalf("expected an asynchronous filter result, got %T", result)
		}
		probe, _ := m.list.Update(matches)
		want := []string{"ac-3", "ac-2"}[i]
		if visible := probe.VisibleItems(); len(visible) != 1 || visible[0].(runItem).run.IssueID != want {
			t.Fatalf("queued filter lost its original query, want %q, got %v", want, visible)
		}
		m.Update(result)
	}

	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if run, ok := m.selectedRun(); !ok || run.IssueID != "ac-3" || run.Tail != "fresh output" {
		t.Fatalf("expected current query and fresh output, got %q with output %q (%v)", run.IssueID, run.Tail, ok)
	}
}

func TestResizeKeepsFollowingOutput(t *testing.T) {
	snapshot := testSnapshot()
	snapshot.Runs[0].Tail = strings.Repeat("a line of output with several words written by the agent\n", 30) + "latest output"
	m := newSized(t)
	m.Update(snapshotMsg{snapshot: snapshot, alive: true})
	m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m.Update(tea.KeyMsg{Type: tea.KeyEnd})
	m.Update(tea.WindowSizeMsg{Width: 34, Height: 14})
	if view := m.View(); !strings.Contains(view, "latest output") {
		t.Fatalf("resize lost the end of the output:\n%s", view)
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

func TestNarrowViewSwitchesPanesAndShowsFilter(t *testing.T) {
	m := withSnapshot(t, newSized(t))
	m.Update(tea.WindowSizeMsg{Width: 36, Height: 14})
	if view := m.View(); !strings.Contains(view, "enter details") || strings.Contains(view, "--- output ---") {
		t.Fatalf("expected run picker and details shortcut:\n%s", view)
	}

	m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m.Update(tea.KeyMsg{Type: tea.KeyTab})
	if view := m.View(); !strings.Contains(view, "second issue") ||
		!strings.Contains(view, "--- output ---") || !strings.Contains(view, "esc runs") ||
		strings.Contains(view, "first issue") {
		t.Fatalf("expected details for selected run:\n%s", view)
	}

	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	if view := m.View(); !strings.Contains(view, "Filter: a") || strings.Contains(view, "--- output ---") {
		t.Fatalf("expected visible filter input:\n%s", view)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})

	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	if view := m.View(); !strings.Contains(view, "│") || !strings.Contains(view, "--- output ---") {
		t.Fatalf("expected split layout after resizing:\n%s", view)
	}
}

func TestNarrowEnterAndEscapeSwitchPanes(t *testing.T) {
	m := withSnapshot(t, newSized(t))
	m.Update(tea.WindowSizeMsg{Width: 36, Height: 14})
	m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if view := m.View(); !strings.Contains(view, "esc runs") ||
		!strings.Contains(view, "second issue") || !strings.Contains(view, "--- output ---") {
		t.Fatalf("expected selected run details after enter:\n%s", view)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if view := m.View(); !strings.Contains(view, "enter details") || strings.Contains(view, "--- output ---") {
		t.Fatalf("expected run picker after escape:\n%s", view)
	}
}

func TestNarrowDetailKeysLeaveHiddenListSelectionAlone(t *testing.T) {
	m := withSnapshot(t, newSized(t))
	m.Update(tea.WindowSizeMsg{Width: 36, Height: 9})
	if m.list.Paginator.TotalPages < 2 {
		t.Fatal("test needs multiple pages of runs")
	}
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m.Update(tea.KeyMsg{Type: tea.KeyRight})
	if m.focus != focusDetail || m.selectedID != "ac-1" {
		t.Fatalf("detail navigation moved focus to %v and selection to %q", m.focus, m.selectedID)
	}
}

func TestResizeRevealsFilterWhileDetailHasFocus(t *testing.T) {
	m := withSnapshot(t, newSized(t))
	m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	m.Update(tea.WindowSizeMsg{Width: 36, Height: 14})

	if view := m.View(); !strings.Contains(view, "Filter:") || strings.Contains(view, "--- output ---") {
		t.Fatalf("expected filter to stay visible after resizing:\n%s", view)
	}
}

func TestNarrowFocusChangesFitWindow(t *testing.T) {
	m := withSnapshot(t, newSized(t))
	const height = 14
	for width := 33; width <= 35; width++ {
		m.Update(tea.KeyMsg{Type: tea.KeyTab})
		m.Update(tea.WindowSizeMsg{Width: width, Height: height})
		for _, input := range []tea.KeyMsg{
			{Type: tea.KeyTab},
			{Type: tea.KeyTab},
			{Type: tea.KeyRunes, Runes: []rune{'/'}},
		} {
			m.Update(input)
			if w, h := lipgloss.Size(m.View()); w > width || h > height {
				t.Fatalf("view after %q measures %dx%d, window is %dx%d", input, w, h, width, height)
			}
		}
		m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	}
}

func TestViewFitsWindow(t *testing.T) {
	cases := []struct {
		name string
		w, h int
	}{
		{"wide", 120, 30},
		{"narrow", 36, 14},
		{"short", 24, 5},
		{"tiny", 1, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := New("/tmp/live.json")
			m.Update(tea.WindowSizeMsg{Width: tc.w, Height: tc.h})
			assertFits := func() {
				t.Helper()
				if w, h := lipgloss.Size(m.View()); w > tc.w || h > tc.h {
					t.Fatalf("view measures %dx%d, window is %dx%d:\n%s", w, h, tc.w, tc.h, m.View())
				}
			}
			assertFits()
			withSnapshot(t, m)
			assertFits()
			m.Update(tea.KeyMsg{Type: tea.KeyTab})
			assertFits()
			m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'?'}})
			assertFits()
		})
	}
}

func TestNarrowDetailWrapsUnicodeOutput(t *testing.T) {
	run := daemon.LiveRun{
		Progress: symphony.Progress{Title: "狭い画面で情報を確認する", Workspace: "/tmp/workspace"},
		Tail:     strings.Repeat("長", 24) + "-end",
	}
	const width = 20
	content := detailContent(run, width)
	if got := lipgloss.Width(content); got > width {
		t.Fatalf("content width = %d, want <= %d:\n%s", got, width, content)
	}
	for _, want := range []string{"Workspace:", "-end"} {
		if !strings.Contains(content, want) {
			t.Fatalf("detail content missing %q:\n%s", want, content)
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
	m.Update(tea.WindowSizeMsg{Width: 36, Height: 14})
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
	if m.list.FilterState() != list.FilterApplied || m.focus != focusList {
		t.Fatalf("enter should apply the filter and keep the run picker visible, got filter %v and focus %v", m.list.FilterState(), m.focus)
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

func TestDashboardShowsActivityAndResumeShortcut(t *testing.T) {
	m := withSnapshot(t, newSized(t))
	for _, want := range []string{"1 active", "1 waiting", "enter details"} {
		if view := m.View(); !strings.Contains(view, want) {
			t.Fatalf("dashboard missing %q:\n%s", want, view)
		}
	}

	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	for _, want := range []string{"Following output", "G latest", "esc runs"} {
		if view := m.View(); !strings.Contains(view, want) {
			t.Fatalf("detail missing %q:\n%s", want, view)
		}
	}

	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m.focus != focusList || m.selectedID != "ac-1" {
		t.Fatalf("back lost run selection: focus %v, run %q", m.focus, m.selectedID)
	}
}

func TestInspectorKeepsRunVisibleDuringScrollback(t *testing.T) {
	m := newSized(t)
	m.Update(tea.WindowSizeMsg{Width: 40, Height: 12})
	snapshot := testSnapshot()
	snapshot.Runs[0].Tail = strings.Repeat("agent output\n", 40)
	m.Update(snapshotMsg{snapshot: snapshot, alive: true})
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m.Update(tea.KeyMsg{Type: tea.KeyPgUp})

	view := m.View()
	lines := strings.Split(view, "\n")
	if len(lines) != 12 || !strings.Contains(lines[1], "╭") || !strings.Contains(lines[2], "ac-1") || !strings.Contains(lines[3], "agent output") {
		t.Fatalf("expected pinned run identity above transcript:\n%s", view)
	}
}

func TestHistorySelectionSurvivesNewerAttempts(t *testing.T) {
	m := newSized(t)
	now := time.Now()
	snapshot := daemon.LiveSnapshot{
		UpdatedAt: now,
		History: []daemon.LiveRun{
			{Progress: symphony.Progress{IssueID: "ac-1", Attempt: 2}, UpdatedAt: now, Tail: "second output"},
			{Progress: symphony.Progress{IssueID: "ac-1", Attempt: 1}, UpdatedAt: now.Add(-time.Minute), Tail: "first output"},
		},
	}
	m.Update(snapshotMsg{snapshot: snapshot, alive: true})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'h'}})
	m.Update(tea.KeyMsg{Type: tea.KeyDown})
	snapshot.History = append([]daemon.LiveRun{
		{Progress: symphony.Progress{IssueID: "ac-1", Attempt: 3}, UpdatedAt: now.Add(time.Minute)},
	}, snapshot.History...)
	m.Update(snapshotMsg{snapshot: snapshot, alive: true})
	if run, ok := m.selectedRun(); !ok || run.Attempt != 1 || !strings.Contains(m.View(), "first output") {
		t.Fatalf("history moved away from the first attempt: %+v\n%s", run, m.View())
	}
}

func TestCommandPaletteExecutesFilteredCommandAfterResize(t *testing.T) {
	m := withSnapshot(t, newSized(t))
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{':'}})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("history")})
	m.Update(tea.WindowSizeMsg{Width: 36, Height: 12})
	view := m.View()
	if !strings.Contains(view, "Show attempt history") || strings.Contains(view, "Show run queue") {
		t.Fatalf("unexpected palette matches:\n%s", view)
	}
	if w, h := lipgloss.Size(view); w > 36 || h > 12 {
		t.Fatalf("palette exceeds terminal: %dx%d", w, h)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m.palette != nil || !m.showHistory {
		t.Fatal("Enter did not open history and close the palette")
	}
}

func TestQueueCommandSubmitsOnlyAfterConfirmation(t *testing.T) {
	m := withSnapshot(t, newSized(t))
	var submitted string
	m.queueTask = func(title string) (string, error) {
		submitted = title
		return "ac-42", nil
	}
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{':'}})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("agent task")})
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("  Confirmed task  ")})
	if submitted != "" {
		t.Fatal("typing submitted a task")
	}
	_, submit := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if submit == nil {
		t.Fatal("confirmation did not return a submit command")
	}
	m.Update(submit())
	if submitted != "Confirmed task" || !strings.Contains(m.View(), "Queued ac-42") {
		t.Fatalf("missing task confirmation: %q\n%s", submitted, m.View())
	}
}

func TestActivityLongestBarStaysOnItsLabelRow(t *testing.T) {
	m := newSized(t)
	m.Update(tea.WindowSizeMsg{Width: 40, Height: 15})
	m.Update(snapshotMsg{snapshot: demoSnapshot(time.Now()), alive: true})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{':'}})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("activity")})
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	view := m.View()
	for _, line := range strings.Split(view, "\n") {
		if strings.Contains(line, "Active") && strings.Contains(line, "━") {
			return
		}
	}
	t.Fatalf("activity chart wrapped the active bar:\n%s", view)
}

func TestPhoneWidthKeepsCoreControlsVisible(t *testing.T) {
	for _, width := range []int{28, 32, 40} {
		m := newSized(t)
		m.Update(tea.WindowSizeMsg{Width: width, Height: 12})
		m.Update(snapshotMsg{snapshot: demoSnapshot(time.Now()), alive: true})
		view := m.View()
		for _, want := range []string{"2 active · 1 waiting", "demo-43", ": cmd", "h hist", "?"} {
			if !strings.Contains(view, want) {
				t.Fatalf("%d columns missing %q:\n%s", width, want, view)
			}
		}
		m.Update(tea.KeyMsg{Type: tea.KeyEnter})
		view = m.View()
		if !strings.Contains(view, "esc runs G end : cmd ?") || !strings.Contains(view, "demo-42") {
			t.Fatalf("%d columns hid inspector controls or run identity:\n%s", width, view)
		}
	}
}
