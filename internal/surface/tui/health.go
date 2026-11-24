package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// Loop-heartbeat thresholds. They are lambe doctor's defaults (control bound of
// a minute plus three 10s ticks warns, twice the bound plus three ticks fails);
// shortcut: the TUI has no config, so a tuned supervisor.tick_seconds or
// silence_timeout is not honoured here. Upgrade by exporting the daemon's
// thresholds in the snapshot.
const (
	staleAfter  = 90 * time.Second
	wedgedAfter = 150 * time.Second
)

type healthKind int

const (
	healthLoading    healthKind = iota // nothing read yet
	healthRunning                      // live daemon, loop ticking
	healthStale                        // live PID, loop heartbeat late
	healthWedged                       // live PID, loop heartbeat gone
	healthStopped                      // the daemon that wrote the snapshot is gone
	healthMissing                      // no snapshot file (never seen, or removed)
	healthUnreadable                   // snapshot present but unreadable or corrupt
)

// health is the connection state of the dashboard to its daemon. Every state
// has a glyph and a word so meaning never rests on colour alone.
type health struct {
	kind   healthKind
	age    time.Duration // since the heartbeat (live) or the last update; valid when hasAge
	hasAge bool
}

func (m *Model) health(now time.Time) health {
	since := func(t time.Time) (time.Duration, bool) {
		if t.IsZero() {
			return 0, false
		}
		return max(0, now.Sub(t)), true
	}
	h := health{kind: healthRunning}
	switch {
	case m.err != nil:
		h.kind = healthUnreadable
		h.age, h.hasAge = since(m.snapshot.UpdatedAt)
	case m.missing:
		h.kind = healthMissing
		h.age, h.hasAge = since(m.snapshot.UpdatedAt)
	case m.snapshot.UpdatedAt.IsZero():
		h.kind = healthLoading
	case !m.alive:
		h.kind = healthStopped
		h.age, h.hasAge = since(m.snapshot.UpdatedAt)
	default:
		// A zero TickAt is an older daemon without the heartbeat: assume healthy.
		if age, ok := since(m.snapshot.TickAt); ok {
			h.age, h.hasAge = age, true
			switch {
			case age >= wedgedAfter:
				h.kind = healthWedged
			case age >= staleAfter:
				h.kind = healthStale
			}
		}
	}
	return h
}

func (h health) glyph() string {
	switch h.kind {
	case healthRunning:
		return "●"
	case healthStale, healthWedged:
		return "▲"
	case healthStopped:
		return "■"
	case healthUnreadable:
		return "✖"
	case healthLoading, healthMissing:
		return "○"
	}
	return "○"
}

func (h health) word() string {
	switch h.kind {
	case healthRunning:
		return "running"
	case healthStale:
		return "stale"
	case healthWedged:
		return "wedged"
	case healthStopped:
		return "stopped"
	case healthMissing:
		return "no data"
	case healthUnreadable:
		return "unreadable"
	case healthLoading:
		return "waiting"
	}
	return "waiting"
}

func (h health) style() lipgloss.Style {
	switch h.kind {
	case healthRunning:
		return runningStyle
	case healthStale:
		return retryStyle.Bold(true)
	case healthWedged, healthStopped:
		return redStyle.Bold(true)
	case healthUnreadable:
		return errorStyle
	case healthLoading, healthMissing:
		return neutralStyle
	}
	return neutralStyle
}

// healthy reports whether the snapshot can be taken at face value.
func (h health) healthy() bool { return h.kind == healthRunning || h.kind == healthLoading }

// label is the header segment: glyph, word, and the age when the state is not
// healthy. Narrow headers spell a healthy daemon "run" to leave room for counts.
func (h health) label(narrow bool) string {
	word := h.word()
	if narrow && h.kind == healthRunning {
		word = "run"
	}
	s := h.glyph() + " " + word
	if !h.healthy() && h.hasAge {
		s += " " + ageText(h.age)
	}
	return s
}

// bannerWanted reports whether the dashboard has room for, and needs, a banner.
func (m *Model) bannerWanted() bool {
	return m.height >= 8 && !m.health(m.clock()).healthy()
}

// syncHealth re-lays-out when the banner appears or disappears; the snapshot
// poll and the tick both call it because health also changes with time alone.
func (m *Model) syncHealth() {
	if m.bannerWanted() != (m.bannerH > 0) {
		follow := !m.showHistory && !m.showAttention && m.detail.AtBottom()
		m.layout()
		m.refreshDetail(follow)
	}
}

// renderBanner is the one-line explanation under the header for an unhealthy
// state. It always opens with the glyph, the word and the last-update age.
func (m *Model) renderBanner() string {
	h := m.health(m.clock())
	ago := func(prefix string) string {
		if !h.hasAge {
			return ""
		}
		return fmt.Sprintf("%s %s ago", prefix, ageText(h.age))
	}
	var segs []fitSegment
	add := func(rank int, parts ...string) {
		if text := strings.Join(nonEmpty(parts), " "); text != "" {
			segs = append(segs, fitSegment{text, rank})
		}
	}
	lead := h.glyph() + " " + h.word()
	switch h.kind {
	case healthStopped:
		add(0, lead, "·", ago("last update"))
		add(1, "rows show its last state")
		add(2, "start `lambe run`")
	case healthStale:
		add(0, lead, "· loop quiet for", ageText(h.age))
		add(1, "rows may be out of date")
		add(2, "check the daemon log")
	case healthWedged:
		add(0, lead, "· loop not ticking for", ageText(h.age))
		add(1, "rows are frozen")
		add(2, "restart the daemon")
	case healthMissing:
		if h.hasAge {
			add(0, lead, "· snapshot file removed,", ago("last update"))
			add(1, "showing last data")
		} else {
			add(0, lead, "· no live snapshot yet")
		}
		add(2, "start `lambe run`")
	case healthUnreadable:
		add(0, lead, "· "+truncate(strings.Join(strings.Fields(ansi.Strip(m.err.Error())), " "), 48))
		add(1, ago("showing data from"))
		add(2, "r retries")
	case healthLoading, healthRunning:
		// Nothing to explain: the bar only appears for an unhealthy snapshot.
	}
	style := retryStyle
	if h.kind != healthStale {
		style = redStyle
	}
	if h.kind == healthMissing && !h.hasAge {
		style = neutralStyle
	}
	line := fitSegments(segs, " · ", max(1, m.width-2))
	return lipgloss.NewStyle().Background(colBarBg).Width(m.width).MaxWidth(m.width).
		Render(" " + style.Bold(true).Background(colBarBg).Render(line))
}

func nonEmpty(parts []string) []string {
	out := parts[:0:0]
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// presentPhase is the icon, style and PHASE word of one run. A row of a
// stopped daemon that still says "running" would be a lie, so active phases
// of a dead daemon are drawn dim and parenthesised: "(running)".
func presentPhase(phase string, live, stalled bool) (string, lipgloss.Style, string) {
	icon, style := phasePresentation(phase)
	word := oneLine(phaseWord(phase, stalled))
	switch {
	case stalled:
		icon, style = "⚠", redStyle.Bold(true)
	case !live && isActivePhase(phase):
		icon, style, word = "◌", dimStyle, "("+word+")"
	}
	return icon, style, word
}

func isActivePhase(phase string) bool {
	switch phase {
	case "running", "gating", "preparing", "claiming", "queued", "retry", "deferred":
		return true
	}
	return false
}
