package tui

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// hostLabel is the short host name shown in the header.
func hostLabel() string {
	name, err := os.Hostname()
	if err != nil || name == "" {
		return "localhost"
	}
	short, _, _ := strings.Cut(name, ".")
	return short
}

// workflowLabel names the workflow(s) the snapshot is serving.
func (m *Model) workflowLabel() string {
	var names []string
	add := func(w string) {
		w = oneLine(w)
		if w != "" && !slices.Contains(names, w) {
			names = append(names, w)
		}
	}
	for _, run := range m.snapshot.Runs {
		add(run.Workflow)
	}
	for _, run := range m.snapshot.History {
		add(run.Workflow)
	}
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	}
	return fmt.Sprintf("%d workflows", len(names))
}

type fitSegment struct {
	text string
	rank int // higher ranks are dropped first when the line is too long
}

// fitSegments joins segments with gap, dropping the highest-ranked ones until
// the line fits width.
func fitSegments(segs []fitSegment, gap string, width int) string {
	segs = slices.Clone(segs)
	join := func() string {
		parts := make([]string, len(segs))
		for i, s := range segs {
			parts[i] = s.text
		}
		return strings.Join(parts, gap)
	}
	for len(segs) > 1 && lipgloss.Width(join()) > width {
		worst := 0
		for i, s := range segs {
			if s.rank >= segs[worst].rank {
				worst = i
			}
		}
		segs = slices.Delete(segs, worst, worst+1)
	}
	return ansi.Truncate(join(), width, "…")
}

func (m *Model) renderHeader() string {
	bar := lipgloss.NewStyle().Background(colBarBg)
	on := func(s lipgloss.Style) lipgloss.Style { return s.Background(colBarBg) }
	badge := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FFFFFF")).
		Background(lipgloss.AdaptiveColor{Light: "#6236A1", Dark: "#654399"}).Padding(0, 1).Render("lambe")

	h := m.health(m.clock())
	health := on(h.style()).Render(h.label(m.width < 36))

	active, waiting, stalled := 0, 0, 0
	if m.alive {
		now := m.clock()
		for _, run := range m.snapshot.Runs {
			if stalledFor(run, true, now) > 0 {
				stalled++
			}
			switch run.Phase {
			case "running", "claiming", "preparing", "gating":
				active++
			case "queued", "retry", "deferred", "interrupted":
				waiting++
			}
		}
	}
	counts := []fitSegment{{on(valueStyle).Render(fmt.Sprintf("%d active", active)) +
		on(dimStyle).Render(" · ") + on(valueStyle).Render(fmt.Sprintf("%d waiting", waiting)), 1}}
	if stalled > 0 {
		counts = append([]fitSegment{{on(redStyle.Bold(true)).Render(fmt.Sprintf("⚠ %d stalled", stalled)), 0}}, counts...)
	}
	if m.width < 60 { // glyph counts, warning first so truncation never hides it
		counts = []fitSegment{{on(runningStyle).Render(fmt.Sprintf("%d▶", active)) + on(lipgloss.NewStyle()).Render(" ") +
			on(retryStyle).Render(fmt.Sprintf("%d↻", waiting)), 1}}
		if stalled > 0 {
			counts = append([]fitSegment{{on(redStyle.Bold(true)).Render(fmt.Sprintf("⚠%d", stalled)), 0}}, counts...)
		}
	}

	context := "demo"
	if m.path != "" {
		context = hostLabel()
		if wf := m.workflowLabel(); wf != "" {
			context += " · " + wf
		}
	}
	segs := []fitSegment{{badge, 0}, {health, 0}}
	if m.width >= 70 {
		segs = append(segs, fitSegment{on(dimStyle).Render(context), 5})
	} else if m.path == "" {
		segs = append(segs, fitSegment{on(dimStyle).Render("demo"), 4})
	}
	segs = append(segs, counts...)
	if m.readinessErr != nil && !m.showHistory {
		segs = append(segs, fitSegment{on(retryStyle).Render("Beads unavailable"), 6})
	}
	left := fitSegments(segs, bar.Render("  "), m.width)
	if m.width >= 100 && !m.snapshot.UpdatedAt.IsZero() {
		right := on(dimStyle).Render("updated " + m.snapshot.UpdatedAt.Format(time.TimeOnly) + " ")
		if gap := m.width - lipgloss.Width(left) - lipgloss.Width(right); gap > 0 {
			left += bar.Render(strings.Repeat(" ", gap)) + right
		}
	}
	return bar.Width(m.width).MaxWidth(m.width).Render(left)
}

// renderTabs draws the Queue / Attention / History bar with live counts and,
// when there is room, the rule underneath that marks the active section.
func (m *Model) renderTabs() string {
	queue, attention, history := m.viewCounts()
	type tab struct {
		label  string
		short  string
		count  int
		active bool
		alarm  bool
	}
	tabs := []tab{
		{"Queue", "Queue", queue, !m.showAttention && !m.showHistory, false},
		{"Attention", "Attn", attention, m.showAttention, attention > 0},
		{"History", "Hist", history, m.showHistory, false},
	}
	render := func(tier int) (string, []int, []int) {
		var line strings.Builder
		var starts, widths []int
		for i, t := range tabs {
			name := fmt.Sprintf("%d %s", i+1, t.label)
			switch tier {
			case 1:
				name = fmt.Sprintf("%d %s", i+1, t.short)
			case 2:
				name = t.label[:1]
			}
			count := fmt.Sprintf("%d", t.count)
			style, countStyle := dimStyle, dimStyle
			if t.alarm && !t.active {
				countStyle = retryStyle.Bold(true)
			}
			if t.active {
				style = lipgloss.NewStyle().Bold(true).Foreground(accentColor)
				countStyle = style
			}
			cell := " " + style.Render(name) + " " + countStyle.Render(count) + " "
			sep := ""
			if i > 0 {
				sep = dimStyle.Render("│")
			}
			line.WriteString(sep)
			starts = append(starts, lipgloss.Width(line.String()))
			widths = append(widths, lipgloss.Width(cell))
			line.WriteString(cell)
		}
		return line.String(), starts, widths
	}
	line, starts, widths := render(0)
	for tier := 1; tier <= 2 && lipgloss.Width(line) > m.width; tier++ {
		line, starts, widths = render(tier)
	}
	line = ansi.Truncate(line, m.width, "")
	line += strings.Repeat(" ", max(0, m.width-lipgloss.Width(line)))
	if m.tabsH < 2 {
		return line
	}
	rule := make([]string, m.width)
	for i := range rule {
		rule[i] = lipgloss.NewStyle().Foreground(colBorder).Render("─")
	}
	for i, t := range tabs {
		if !t.active {
			continue
		}
		for x := starts[i]; x < min(m.width, starts[i]+widths[i]); x++ {
			rule[x] = lipgloss.NewStyle().Foreground(accentColor).Bold(true).Render("━")
		}
	}
	return line + "\n" + strings.Join(rule, "")
}

// hint is a footer key hint; higher ranks are dropped first on narrow terminals.
type hint struct {
	b    key.Binding
	rank int
}

func (m *Model) hints() []hint {
	k := m.keys
	short := func(b key.Binding, keys, desc string) key.Binding { b.SetHelp(keys, desc); return b }
	// pair merges two bindings so one hint can stand for both (j/k).
	pair := func(a, b key.Binding, keys, desc string) key.Binding {
		return key.NewBinding(key.WithKeys(append(a.Keys(), b.Keys()...)...), key.WithHelp(keys, desc))
	}
	if m.list.FilterState() == list.Filtering {
		return []hint{
			{short(k.FilterApply, "enter", "apply"), 0},
			{short(k.FilterCancel, "esc", "cancel"), 1},
		}
	}
	if m.focus == focusDetail {
		return []hint{
			{short(k.Back, "esc", "back"), 0},
			{short(k.GoBottom, "G", "latest"), 1},
			{short(k.Commands, ":", "cmd"), 2},
			{short(k.Help, "?", "help"), 3},
			{pair(k.ScrollDn, k.ScrollUp, "j/k", "scroll"), 5},
			{short(k.GoTop, "g", "top"), 6},
			{short(k.Focus, "tab", "list"), 7},
		}
	}
	return []hint{
		{short(k.OpenRun, "enter", "open"), 0},
		{short(k.Commands, ":", "cmd"), 1},
		{short(k.Help, "?", "help"), 2},
		{short(k.Quit, "q", "quit"), 3},
		{short(k.Views, "1-3", "views"), 4},
		{short(k.Filter, "/", "filter"), 5},
		{pair(k.ListDown, k.ListUp, "j/k", "move"), 6},
	}
}

func (m *Model) renderHints(width int) string {
	hs := m.hints()
	if m.width < 44 { // the bare "?" is recognisable and saves seven columns
		for i := range hs {
			if hs[i].b.Help().Key == "?" {
				hs[i].b.SetHelp("?", "")
			}
		}
	}
	render := func() string {
		parts := make([]string, len(hs))
		for i, h := range hs {
			parts[i] = m.help.Styles.ShortKey.Render(h.b.Help().Key)
			if desc := h.b.Help().Desc; desc != "" {
				parts[i] += " " + m.help.Styles.ShortDesc.Render(desc)
			}
		}
		return strings.Join(parts, "  ")
	}
	for len(hs) > 1 && lipgloss.Width(render()) > width {
		worst := 0
		for i, h := range hs {
			if h.rank >= hs[worst].rank {
				worst = i
			}
		}
		hs = slices.Delete(hs, worst, worst+1)
	}
	return ansi.Truncate(render(), width, "…")
}

func (m *Model) renderFooter() string {
	style := footerStyle.Width(m.width).MaxWidth(m.width).MaxHeight(max(1, m.height-2))
	inner := max(1, m.width-2)
	var lines []string
	status := m.position()
	switch {
	case m.focus == focusDetail && !m.showHistory && !m.showAttention && m.detail.AtBottom():
		status = "following"
	case m.focus == focusDetail:
		status = fmt.Sprintf("scroll %d%%", int(m.detail.ScrollPercent()*100))
	}
	right := valueStyle.Render(status)
	left := m.renderHints(max(1, inner-lipgloss.Width(right)-2))
	if _, ok := m.activeToast(); ok {
		left = m.renderToast(max(1, inner-lipgloss.Width(right)-2))
	}
	gap := max(1, inner-lipgloss.Width(left)-lipgloss.Width(right))
	// Truncate: in a very narrow window the status alone is wider than the row and would wrap.
	lines = append(lines, ansi.Truncate(left+strings.Repeat(" ", gap)+right, inner, "…"))
	return style.Render(strings.Join(lines, "\n"))
}
