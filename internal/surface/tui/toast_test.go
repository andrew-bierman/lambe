package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/andrew-bierman/lambe/internal/daemon"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// fakeClock returns a clock a test installs on its model (m.now) and a function that advances it.
func fakeClock(t *testing.T) (now func() time.Time, advance func(time.Duration)) {
	t.Helper()
	cur := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	return func() time.Time { return cur }, func(d time.Duration) { cur = cur.Add(d) }
}

func firstAndLastLine(view string) (string, string) {
	lines := strings.Split(ansi.Strip(view), "\n")
	return lines[0], lines[len(lines)-1]
}

func TestToastLivesInFooterAndKeepsHeaderCounts(t *testing.T) {
	now, _ := fakeClock(t)
	m := demoRichModel(now())
	m.now = now
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	before, _ := firstAndLastLine(m.View())
	m.submitTask("Write the docs", nil)
	header, footer := firstAndLastLine(m.View())
	if strings.Contains(header, "Queued") || !strings.Contains(header, "active") {
		t.Fatalf("header was replaced by the toast: %q", header)
	}
	if !strings.Contains(footer, "Queued demo-") {
		t.Fatalf("footer has no toast: %q", footer)
	}
	if !strings.Contains(header, "stalled") && strings.Contains(before, "stalled") {
		t.Fatalf("stall warning disappeared: %q", header)
	}
}

func TestToastStallWarningSurvivesErrors(t *testing.T) {
	now, advance := fakeClock(t)
	m := demoRichModel(now())
	m.now = now
	advance(10 * time.Minute) // every running row is now silent
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m.Update(commandResult{err: errors.New("daemon\nnot reachable")})
	header, footer := firstAndLastLine(m.View())
	if !strings.Contains(header, "stalled") {
		t.Fatalf("stall warning hidden by an error toast: %q", header)
	}
	if !strings.Contains(footer, "daemon not reachable") {
		t.Fatalf("error toast missing or multi-line: %q", footer)
	}
}

func TestToastExpiresAfterTTL(t *testing.T) {
	now, advance := fakeClock(t)
	m := demoRichModel(now())
	m.now = now
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m.showToast("Copied demo-42", false, false)
	seq := m.toast.seq
	cases := []struct {
		name    string
		advance time.Duration
		visible bool
	}{
		{"fresh", 0, true},
		{"3s", 3 * time.Second, true},
		{"4s", time.Second, false},
	}
	for _, tc := range cases {
		advance(tc.advance)
		if _, ok := m.activeToast(); ok != tc.visible {
			t.Errorf("%s: visible=%v, want %v", tc.name, ok, tc.visible)
		}
	}
	if _, footer := firstAndLastLine(m.View()); strings.Contains(footer, "Copied") {
		t.Fatalf("expired toast still drawn: %q", footer)
	}
	// A stale expiry must not clear a newer toast; the matching one clears.
	m.showToast("Second", false, false)
	m.Update(toastExpiredMsg{seq})
	if m.toast.text != "Second" {
		t.Fatal("stale expiry cleared the newer toast")
	}
	m.Update(toastExpiredMsg{m.toast.seq})
	if m.toast.text != "" {
		t.Fatal("expiry did not clear the toast")
	}
}

func TestPendingToastDoesNotExpire(t *testing.T) {
	now, advance := fakeClock(t)
	m := demoRichModel(now())
	m.now = now
	if cmd := m.showToast("Queuing…", false, true); cmd != nil {
		t.Fatal("pending toast scheduled an expiry")
	}
	advance(time.Hour)
	if _, ok := m.activeToast(); !ok {
		t.Fatal("pending toast expired")
	}
}

func TestQueuedDemoIDsNeverCollide(t *testing.T) {
	now, _ := fakeClock(t)
	m := demoRichModel(now())
	m.now = now
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	seen := map[string]bool{}
	for _, r := range append(append([]string{}, ids(m.snapshot.Runs)...), ids(m.snapshot.History)...) {
		seen[r] = true
	}
	for range 3 {
		id := m.nextDemoID()
		if seen[id] {
			t.Fatalf("%s already exists", id)
		}
		seen[id] = true
		m.submitTask("task", nil)
	}
}

func TestDemoDoesNotDecayIntoStalls(t *testing.T) {
	now, advance := fakeClock(t)
	m := demoRichModel(now())
	m.now = now
	m.demo, m.demoAt = true, now()
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	stalled := func() int {
		n := 0
		for _, r := range m.snapshot.Runs {
			if stalledFor(r, true, now()) > 0 {
				n++
			}
		}
		return n
	}
	start := stalled()
	for range 600 { // ten minutes of one-second ticks
		advance(time.Second)
		m.Update(demoTickMsg(now()))
	}
	if got := stalled(); got != start {
		t.Fatalf("stalled rows drifted from %d to %d", start, got)
	}
	if !m.snapshot.UpdatedAt.Equal(now()) {
		t.Fatal("demo clock did not advance")
	}
}

func ids(runs []daemon.LiveRun) []string {
	out := make([]string, len(runs))
	for i, r := range runs {
		out[i] = r.IssueID
	}
	return out
}
