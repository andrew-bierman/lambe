package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// benchModel is a 120x40 dashboard over 2.5k rows, the size the audit
// measured poll cost at.
func benchModel(b *testing.B) *Model {
	b.Helper()
	m := New("")
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m.Update(snapshotMsg{snapshot: manyRunsSnapshot(2500), alive: true})
	return m
}

func BenchmarkView2500(b *testing.B) {
	m := benchModel(b)
	b.ReportAllocs()
	for b.Loop() {
		_ = m.View()
	}
}

// BenchmarkUpdateKey2500 is one cursor move (and the re-render the program
// would do after it) over 2.5k rows.
func BenchmarkUpdateKey2500(b *testing.B) {
	m := benchModel(b)
	down, up := tea.KeyMsg{Type: tea.KeyDown}, tea.KeyMsg{Type: tea.KeyUp}
	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		if i%2 == 0 {
			m.Update(down)
		} else {
			m.Update(up)
		}
		_ = m.View()
	}
}

// BenchmarkUpdateSnapshot2500 is a changed snapshot landing, the 4 Hz poll
// path: rebuild the rows, reselect, refresh the preview.
func BenchmarkUpdateSnapshot2500(b *testing.B) {
	m := benchModel(b)
	snap := manyRunsSnapshot(2500)
	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		snap.Runs[i%len(snap.Runs)].Tail += "x"
		m.Update(snapshotMsg{snapshot: snap, alive: true})
	}
}
