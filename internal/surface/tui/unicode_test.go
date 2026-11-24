package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/andrew-bierman/lambe/internal/daemon"
	"github.com/andrew-bierman/lambe/internal/symphony"
)

// unicodeSnapshot holds the titles, ids and workflows that stress width
// arithmetic: wide CJK, ZWJ emoji and flags, combining marks, right-to-left
// scripts, and values far longer than any window.
func unicodeSnapshot(now time.Time) daemon.LiveSnapshot {
	long := strings.Repeat("0123456789", 20) // 200 columns
	run := func(id, title, phase, workflow string, ago time.Duration) daemon.LiveRun {
		return daemon.LiveRun{
			Progress:     symphony.Progress{IssueID: id, Title: title, Phase: phase, Workflow: workflow, Attempt: 1},
			UpdatedAt:    now.Add(-ago),
			LastOutputAt: now.Add(-ago),
			Tail:         title + "\n日本語の出力 🚀 עברית مرحبا\n" + long + "\n",
		}
	}
	return daemon.LiveSnapshot{
		UpdatedAt: now,
		Runs: []daemon.LiveRun{
			run("cjk-001", "日本語の長いタイトルで幅を確認する漢字仮名交じり文", "running", "ワークフロー", 5*time.Second),
			run("emoji-002", "👩‍👩‍👧‍👦 family 🚀🔥 release 🇯🇵 flags ❤️ done", "running", "emoji-flow", 20*time.Second),
			run("combining-003", "Café résumé naïve à́̂̃̄ zalgo", "retry", "marks", 2*time.Minute),
			run("rtl-004", "שלום עולם — בדיקת כיוון, مرحبا بالعالم اختبار", "stopped: gate escalation", "rtl", 3*time.Minute),
			run("long-title-005", long, "running", "long", 9*time.Second),
			run(long, "long id", "stopped: gate red", "long-id", 4*time.Minute),
			run("long-wf-007", "long workflow", "running", strings.Repeat("workflow-", 22), time.Second),
		},
		History: []daemon.LiveRun{
			run("hist-008", "履歴の行 🎉 with a long tail "+long, "stopped: gate green", "ワークフロー", time.Hour),
		},
	}
}

func unicodeModel(now time.Time, w, h int) *Model {
	m := at(New(""), now)
	m.Update(snapshotMsg{snapshot: unicodeSnapshot(now), alive: true})
	m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	return m
}

var unicodeSizes = [][2]int{{28, 14}, {40, 24}, {60, 24}, {100, 30}, {140, 40}}

// TestUnicodeFixtures renders the stress snapshot at every target width in all
// three views and holds each frame to the window; the queue is also a golden.
func TestUnicodeFixtures(t *testing.T) {
	now := frozenClock(t)
	for _, size := range unicodeSizes {
		w, h := size[0], size[1]
		t.Run(fmt.Sprintf("%dx%d", w, h), func(t *testing.T) {
			for key, view := range map[rune]string{'1': "queue", '2': "attention", '3': "history"} {
				m := unicodeModel(now, w, h)
				m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{key}})
				fits(t, m.View(), w, h)
				if view == "queue" {
					checkGolden(t, fmt.Sprintf("unicode-queue-%dx%d", w, h), plain(m.View()))
				}
			}
			m := unicodeModel(now, w, h)
			m.Update(tea.KeyMsg{Type: tea.KeyEnter})
			fits(t, m.View(), w, h)
		})
	}
}

// TestEverySmallWindowFits resizes one model through every size up to 40x40
// in each view and with the preview focused. A row wider than the table used to wrap onto extra lines in
// windows narrower than 20 columns.
func TestEverySmallWindowFits(t *testing.T) {
	now := frozenClock(t)
	keys := []tea.KeyMsg{
		{Type: tea.KeyRunes, Runes: []rune{'1'}},
		{Type: tea.KeyRunes, Runes: []rune{'2'}},
		{Type: tea.KeyRunes, Runes: []rune{'3'}},
		{Type: tea.KeyRunes, Runes: []rune{'?'}},
		{Type: tea.KeyTab},
		{Type: tea.KeyRunes, Runes: []rune{'/'}},
	}
	for _, key := range keys {
		m := unicodeModel(now, 100, 30)
		m.Update(key)
		for w := 1; w <= 40; w++ {
			for h := 1; h <= 40; h++ {
				m.Update(tea.WindowSizeMsg{Width: w, Height: h})
				if vw, vh := lipgloss.Size(m.View()); vw > w || vh > h {
					t.Fatalf("key %v: %dx%d frame in a %dx%d window", key, vw, vh, w, h)
				}
			}
		}
	}
}
