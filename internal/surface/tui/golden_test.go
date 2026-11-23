package tui

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

var update = flag.Bool("update", false, "rewrite golden files")

// frozenClock pins rendered ages so snapshots are stable.
func frozenClock(t *testing.T) time.Time {
	t.Helper()
	fixed := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	clock = func() time.Time { return fixed }
	t.Cleanup(func() { clock = time.Now })
	return fixed
}

func plain(view string) string {
	lines := strings.Split(ansi.Strip(view), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " ")
	}
	return strings.Join(lines, "\n") + "\n"
}

func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if *update {
		if err := os.MkdirAll("testdata", 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path) //nolint:gosec // fixed testdata path
	if err != nil {
		t.Fatalf("%v (run with -update)", err)
	}
	if string(want) != got {
		t.Fatalf("%s differs from golden (run with -update to accept):\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}

func fits(t *testing.T, view string, w, h int) {
	t.Helper()
	if vw, vh := lipgloss.Size(view); vw > w || vh > h {
		t.Fatalf("view is %dx%d, window is %dx%d:\n%s", vw, vh, w, h, ansi.Strip(view))
	}
}

// TestGoldenLayouts snapshots the dashboard at the widths the redesign targets:
// 40 (a Rex split), 60 (stacked), 100 (side by side) and 140 (desktop).
func TestGoldenLayouts(t *testing.T) {
	now := frozenClock(t)
	for _, size := range [][2]int{{40, 24}, {60, 24}, {100, 30}, {140, 40}} {
		w, h := size[0], size[1]
		t.Run(fmt.Sprintf("queue-%dx%d", w, h), func(t *testing.T) {
			m := demoModel(now)
			m.Update(tea.WindowSizeMsg{Width: w, Height: h})
			fits(t, m.View(), w, h)
			checkGolden(t, fmt.Sprintf("queue-%dx%d", w, h), plain(m.View()))
		})
	}
	t.Run("drill-in-40x12", func(t *testing.T) {
		m := demoModel(now)
		m.Update(tea.WindowSizeMsg{Width: 40, Height: 12})
		m.Update(tea.KeyMsg{Type: tea.KeyEnter})
		fits(t, m.View(), 40, 12)
		checkGolden(t, "drill-in-40x12", plain(m.View()))
	})
	t.Run("attention-40x24", func(t *testing.T) {
		m := demoModel(now)
		m.Update(tea.WindowSizeMsg{Width: 40, Height: 24})
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'2'}})
		checkGolden(t, "attention-40x24", plain(m.View()))
	})
	for _, size := range [][2]int{{40, 24}, {100, 30}} {
		w, h := size[0], size[1]
		t.Run(fmt.Sprintf("history-%dx%d", w, h), func(t *testing.T) {
			m := demoModel(now)
			m.Update(tea.WindowSizeMsg{Width: w, Height: h})
			m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'3'}})
			fits(t, m.View(), w, h)
			checkGolden(t, fmt.Sprintf("history-%dx%d", w, h), plain(m.View()))
		})
	}
	t.Run("history-140x40", func(t *testing.T) {
		m := demoModel(now)
		m.Update(tea.WindowSizeMsg{Width: 140, Height: 40})
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'3'}})
		checkGolden(t, "history-140x40", plain(m.View()))
	})
}

// TestResizeSequenceKeepsSelectionAndFits drags the window through every
// layout (side by side, stacked, drill-in) and back.
func TestResizeSequenceKeepsSelectionAndFits(t *testing.T) {
	now := frozenClock(t)
	m := demoModel(now)
	m.Update(tea.WindowSizeMsg{Width: 140, Height: 40})
	m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m.Update(tea.KeyMsg{Type: tea.KeyDown})
	if m.selectedID != "demo-44" {
		t.Fatalf("selected %q", m.selectedID)
	}
	for _, size := range [][2]int{{100, 30}, {60, 24}, {40, 24}, {40, 8}, {28, 6}, {40, 24}, {140, 40}} {
		m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		view := m.View()
		fits(t, view, size[0], size[1])
		if m.selectedID != "demo-44" {
			t.Fatalf("%v: resize lost selection, now %q", size, m.selectedID)
		}
		if size[1] >= 7 && !strings.Contains(ansi.Strip(view), "demo-44") {
			t.Fatalf("%v: selected run not on screen:\n%s", size, ansi.Strip(view))
		}
	}
	if !m.split || m.stacked {
		t.Fatal("expected side-by-side layout at 140x40")
	}
	m.Update(tea.WindowSizeMsg{Width: 60, Height: 24})
	if !m.stacked || m.split {
		t.Fatal("expected stacked layout at 60x24")
	}
	m.Update(tea.WindowSizeMsg{Width: 40, Height: 12})
	if m.stacked || m.split {
		t.Fatal("expected drill-in layout at 40x12")
	}
}

// TestSelectionMovesWithKeysAndMarksRow checks the selected-row marker tracks
// the cursor and the preview follows it.
func TestSelectionMovesWithKeysAndMarksRow(t *testing.T) {
	m := demoModel(frozenClock(t))
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	marked := func() string {
		for _, line := range strings.Split(ansi.Strip(m.View()), "\n") {
			if strings.HasPrefix(line, "▌") {
				return line
			}
		}
		return ""
	}
	if got := marked(); !strings.Contains(got, "demo-42") {
		t.Fatalf("first row not marked: %q", got)
	}
	for i, want := range []string{"demo-43", "demo-44", "demo-45"} {
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
		if got := marked(); !strings.Contains(got, want) {
			t.Fatalf("step %d: marked row %q, want %s", i, got, want)
		}
		if !strings.Contains(ansi.Strip(m.View()), want+"  ") {
			t.Fatalf("preview did not follow selection to %s", want)
		}
	}
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
	if got := marked(); !strings.Contains(got, "demo-44") {
		t.Fatalf("k did not move up: %q", got)
	}
}

// TestListScrollsAndPreviewScrolls covers paging through a long queue and
// scrolling the preview's scrollback independently.
func TestListScrollsAndPreviewScrolls(t *testing.T) {
	frozenClock(t)
	m := New("/tmp/live.json")
	m.Update(tea.WindowSizeMsg{Width: 40, Height: 14})
	snap := manyRunsSnapshot(30)
	snap.Runs[0].Tail = strings.Repeat("line of output\n", 80)
	m.Update(snapshotMsg{snapshot: snap, alive: true})
	if m.list.Paginator.TotalPages < 2 {
		t.Fatal("expected the queue to need more than one page")
	}
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'g'}})
	top := m.selectedID
	for range 25 {
		m.Update(tea.KeyMsg{Type: tea.KeyDown})
	}
	if m.selectedID == top || !strings.Contains(ansi.Strip(m.View()), m.selectedID) {
		t.Fatalf("scrolling lost the selection %q:\n%s", m.selectedID, ansi.Strip(m.View()))
	}
	m.Update(tea.MouseMsg{X: 5, Y: 1 + m.tabsH + 2, Button: tea.MouseButtonWheelUp})
	if m.selectedID == "" {
		t.Fatal("wheel dropped the selection")
	}

	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'g'}})
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m.Update(tea.KeyMsg{Type: tea.KeyPgUp})
	if view := ansi.Strip(m.View()); !strings.Contains(view, "scroll ") || strings.Contains(view, "following") {
		t.Fatalf("scrollback not reflected in footer:\n%s", view)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'G'}})
	if view := ansi.Strip(m.View()); !strings.Contains(view, "following") {
		t.Fatalf("G did not return to live output:\n%s", view)
	}
}

func TestRowColumnsAlign(t *testing.T) {
	m := demoModel(frozenClock(t))
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	var cols []int
	for _, line := range strings.Split(ansi.Strip(m.View()), "\n") {
		if i := strings.Index(line, "PHASE"); i >= 0 {
			cols = append(cols, lipgloss.Width(line[:i]))
		}
		table, _, _ := strings.Cut(line, "│") // keep the preview pane out of it
		for _, word := range []string{" running ", " stalled ", " retry ", " escalated ", " passed "} {
			if i := strings.Index(table, word); i >= 0 && strings.Contains(table, "demo-") {
				cols = append(cols, lipgloss.Width(table[:i])+1)
			}
		}
	}
	if len(cols) < 6 {
		t.Fatalf("found only %d aligned cells", len(cols))
	}
	for _, c := range cols {
		if c != cols[0] {
			t.Fatalf("phase column misaligned: %v", cols)
		}
	}
}
