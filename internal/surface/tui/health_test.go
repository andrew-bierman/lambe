package tui

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/andrew-bierman/lambe/internal/daemon"
	"github.com/andrew-bierman/lambe/internal/symphony"
)

// healthCase builds one connection state on top of the demo snapshot.
type healthCase struct {
	name    string
	kind    healthKind
	word    string   // the health word the header must carry
	banner  []string // fragments the banner must carry
	prepare func(m *Model, now time.Time)
}

var errCorrupt = errors.New("invalid character '}' looking for beginning of value")

func healthCases() []healthCase {
	live := func(tickAge time.Duration) func(*Model, time.Time) {
		return func(m *Model, now time.Time) {
			snap := demoSnapshot(now)
			snap.TickAt = now.Add(-tickAge)
			m.Update(snapshotMsg{snapshot: snap, alive: true})
		}
	}
	return []healthCase{
		{"running", healthRunning, "running", nil, live(5 * time.Second)},
		{"stale", healthStale, "stale 2m", []string{"▲ stale", "loop quiet for 2m"}, live(2 * time.Minute)},
		{"wedged", healthWedged, "wedged 30m", []string{"▲ wedged", "not ticking for 30m"}, live(30 * time.Minute)},
		{"stopped", healthStopped, "stopped 4m", []string{"■ stopped", "last update 4m ago", "rows show its last state"}, func(m *Model, now time.Time) {
			snap := demoSnapshot(now)
			snap.UpdatedAt = now.Add(-4 * time.Minute)
			m.Update(snapshotMsg{snapshot: snap, alive: false})
		}},
		{"missing", healthMissing, "no data", []string{"○ no data", "no live snapshot yet"}, func(m *Model, _ time.Time) {
			m.Update(snapshotMsg{err: fmt.Errorf("open live.json: %w", fs.ErrNotExist)})
		}},
		{"removed", healthMissing, "no data 3m", []string{"○ no data", "snapshot file removed", "last update 3m ago"}, func(m *Model, now time.Time) {
			snap := demoSnapshot(now)
			snap.UpdatedAt = now.Add(-3 * time.Minute)
			m.Update(snapshotMsg{snapshot: snap, alive: true})
			m.Update(snapshotMsg{err: fmt.Errorf("open live.json: %w", fs.ErrNotExist)})
		}},
		{"unreadable", healthUnreadable, "unreadable 3m", []string{"✖ unreadable", "invalid character", "3m ago"}, func(m *Model, now time.Time) {
			snap := demoSnapshot(now)
			snap.UpdatedAt = now.Add(-3 * time.Minute)
			m.Update(snapshotMsg{snapshot: snap, alive: true})
			m.Update(snapshotMsg{err: errCorrupt})
		}},
	}
}

func healthView(t *testing.T, now time.Time, c healthCase, w, h int) *Model {
	t.Helper()
	m := at(New(""), now) // the "demo" context keeps goldens independent of the host name
	m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	c.prepare(m, now)
	return m
}

func TestHealthStates(t *testing.T) {
	now := frozenClock(t)
	for _, c := range healthCases() {
		for _, size := range stateSizes {
			w, h := size[0], size[1]
			t.Run(fmt.Sprintf("%s-%dx%d", c.name, w, h), func(t *testing.T) {
				m := healthView(t, now, c, w, h)
				if got := m.health(now).kind; got != c.kind {
					t.Fatalf("kind = %v, want %v", got, c.kind)
				}
				fits(t, m.View(), w, h)
				view := plain(m.View())
				header := strings.SplitN(view, "\n", 2)[0]
				if want := c.word; w >= 36 || c.kind != healthRunning {
					if !strings.Contains(header, want) {
						t.Fatalf("header %q lacks health word %q", header, want)
					}
				}
				if c.kind == healthRunning {
					if m.bannerH != 0 {
						t.Fatal("a healthy daemon must not show a banner")
					}
					return
				}
				if m.bannerH != 1 {
					t.Fatalf("bannerH = %d, want 1", m.bannerH)
				}
				banner := strings.Split(view, "\n")[1]
				// Every banner opens with the glyph, the word and the age at any width.
				for _, want := range c.banner[:1] {
					if !strings.Contains(banner, want) {
						t.Fatalf("banner %q lacks %q", banner, want)
					}
				}
				if w >= 100 {
					for _, want := range c.banner {
						if !strings.Contains(banner, want) {
							t.Fatalf("banner %q lacks %q", banner, want)
						}
					}
				}
			})
		}
	}
}

func TestHealthShortWindowKeepsWordInHeader(t *testing.T) {
	now := frozenClock(t)
	for _, c := range healthCases()[1:] {
		m := healthView(t, now, c, 40, 6)
		if m.bannerH != 0 {
			t.Fatalf("%s: no room for a banner at height 6", c.name)
		}
		header := strings.SplitN(plain(m.View()), "\n", 2)[0]
		if !strings.Contains(header, c.word) {
			t.Errorf("%s: header %q lacks %q", c.name, header, c.word)
		}
	}
}

func TestStoppedDaemonRowsAreNotRunning(t *testing.T) {
	now := frozenClock(t)
	var stopped healthCase
	for _, c := range healthCases() {
		if c.name == "stopped" {
			stopped = c
		}
	}
	for _, w := range []int{40, 100} {
		m := healthView(t, now, stopped, w, 30)
		rows := strings.Join(strings.Split(plain(m.View()), "\n")[4:12], "\n")
		for _, bad := range []string{"▶", " running "} {
			if strings.Contains(rows, bad) {
				t.Errorf("width %d: stopped daemon row shows %q:\n%s", w, bad, rows)
			}
		}
		if w >= 100 && !strings.Contains(rows, "(running)") {
			t.Errorf("width %d: running row not relabelled:\n%s", w, rows)
		}
	}
	// The preview must not claim live output either.
	m := healthView(t, now, stopped, 100, 30)
	if view := plain(m.View()); strings.Contains(view, "output 8s ago") || strings.Contains(view, "▶ running") {
		t.Errorf("preview of a stopped daemon claims live output:\n%s", view)
	}
}

func TestHealthFollowsTimeWithoutNewSnapshots(t *testing.T) {
	now := frozenClock(t)
	m := at(New("/state/live.json"), now)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	snap := demoSnapshot(now)
	snap.TickAt = now
	m.Update(snapshotMsg{snapshot: snap, alive: true})
	if m.bannerH != 0 {
		t.Fatal("fresh heartbeat must not show a banner")
	}
	bodyBefore := m.bodyH
	later := now.Add(10 * time.Minute)
	m.now = func() time.Time { return later }
	m.Update(tickMsg(later))
	if m.bannerH != 1 || m.health(later).kind != healthWedged {
		t.Fatalf("a heartbeat 10m old must show the wedged banner, bannerH=%d kind=%v", m.bannerH, m.health(later).kind)
	}
	if m.bodyH != bodyBefore-1 {
		t.Fatalf("banner must take one body row: %d -> %d", bodyBefore, m.bodyH)
	}
	fits(t, m.View(), 100, 30)
	snap.TickAt = later
	m.Update(snapshotMsg{snapshot: snap, alive: true})
	if m.bannerH != 0 || m.bodyH != bodyBefore {
		t.Fatalf("recovery must remove the banner, bannerH=%d", m.bannerH)
	}
}

func TestOlderDaemonWithoutHeartbeatIsNotFlagged(t *testing.T) {
	now := frozenClock(t)
	m := at(New("/state/live.json"), now)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	snap := demoSnapshot(now)
	snap.UpdatedAt = now.Add(-time.Hour) // no TickAt: an idle older daemon
	m.Update(snapshotMsg{snapshot: snap, alive: true})
	if got := m.health(now).kind; got != healthRunning {
		t.Fatalf("kind = %v, want running", got)
	}
}

func TestStallUsesLastActivity(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	run := func(out, activity time.Duration) daemon.LiveRun {
		r := demoSnapshot(now).Runs[0]
		r.LastOutputAt = now.Add(-out)
		if activity > 0 {
			r.LastActivityAt = now.Add(-activity)
		}
		return r
	}
	for _, tc := range []struct {
		name          string
		out, activity time.Duration
		stalled       bool
	}{
		{"silent output only", 5 * time.Minute, 0, true},
		{"recent tool activity keeps it alive", 5 * time.Minute, 10 * time.Second, false},
		{"both old", 5 * time.Minute, 4 * time.Minute, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := stalledFor(run(tc.out, tc.activity), true, now) > 0; got != tc.stalled {
				t.Fatalf("stalled = %v, want %v", got, tc.stalled)
			}
		})
	}
	if stalledFor(run(5*time.Minute, 0), false, now) != 0 {
		t.Fatal("a stopped daemon's rows are never stalled")
	}
}

func TestRefreshKey(t *testing.T) {
	m := New("/state/live.json")
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	if cmd == nil {
		t.Fatal("r must force a snapshot read")
	}
	if _, ok := cmd().(snapshotMsg); !ok {
		t.Fatal("r must produce a snapshot read")
	}
	if _, cmd := New("").Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}}); cmd != nil {
		t.Fatal("the demo has nothing to refresh")
	}
}

// healthViews renders every unhealthy state at the targeted widths.
func healthViews(t *testing.T, now time.Time) map[string]string {
	t.Helper()
	views := map[string]string{}
	for _, c := range healthCases()[1:] {
		for _, size := range [][2]int{{40, 24}, {100, 30}} {
			views[fmt.Sprintf("health-%s-%dx%d", c.name, size[0], size[1])] = healthView(t, now, c, size[0], size[1]).View()
		}
	}
	return views
}

// stateSizes are every width the connection states are exercised at.
var stateSizes = [][2]int{{28, 12}, {40, 24}, {60, 24}, {100, 30}, {140, 40}}

// TestGoldenHealthAllWidths adds the widths the colour goldens skip (28, 60,
// 140) so each connection state has a plain-text snapshot at every width.
func TestGoldenHealthAllWidths(t *testing.T) {
	now := frozenClock(t)
	for _, c := range healthCases()[1:] {
		for _, size := range [][2]int{{28, 12}, {60, 24}, {140, 40}} {
			name := fmt.Sprintf("health-%s-%dx%d", c.name, size[0], size[1])
			t.Run(name, func(t *testing.T) {
				m := healthView(t, now, c, size[0], size[1])
				fits(t, m.View(), size[0], size[1])
				checkGolden(t, name, plain(m.View()))
			})
		}
	}
}

func TestGoldenHealth(t *testing.T) {
	now := frozenClock(t)
	for name, view := range healthViews(t, now) {
		t.Run(name, func(t *testing.T) { checkGolden(t, name, plain(view)) })
	}
}

func TestGoldenHealthLight(t *testing.T) {
	forceBackground(t, false)
	now := frozenClock(t)
	views := healthViews(t, now)
	for _, name := range slices.Sorted(maps.Keys(views)) {
		t.Run(name, func(t *testing.T) { checkGolden(t, "light-"+name, annotate(parseCells(views[name]))) })
	}
}

func TestGoldenHealthDark(t *testing.T) {
	forceBackground(t, true)
	now := frozenClock(t)
	views := healthViews(t, now)
	for _, name := range slices.Sorted(maps.Keys(views)) {
		t.Run(name, func(t *testing.T) { checkGolden(t, "dark-"+name, annotate(parseCells(views[name]))) })
	}
}

// TestDetailPhaseOfStoppedDaemonIsDimmed: the Phase row follows the list rows,
// so a stopped daemon's "running" reads "(running)" with the dim glyph.
func TestDetailPhaseOfStoppedDaemonIsDimmed(t *testing.T) {
	run := daemon.LiveRun{Progress: symphony.Progress{IssueID: "ac-1", Phase: "running"}}
	phaseRow := func(live bool) string {
		for _, line := range strings.Split(ansi.Strip(detailContent(run, live, 80)), "\n") {
			if strings.HasPrefix(line, "Phase:") {
				return line
			}
		}
		t.Fatal("no Phase row")
		return ""
	}
	if got := phaseRow(true); !strings.Contains(got, "▶ running") {
		t.Errorf("live row = %q", got)
	}
	if got := phaseRow(false); !strings.Contains(got, "◌ (running)") || strings.Contains(got, "▶") {
		t.Errorf("stopped row = %q", got)
	}
	run.Phase = "stopped: gate green" // finished work is not active: it stays as is
	if got := phaseRow(false); !strings.Contains(got, "stopped: gate green") {
		t.Errorf("finished row = %q", got)
	}
}
