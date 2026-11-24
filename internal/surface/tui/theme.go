package tui

import (
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/andrew-bierman/lambe/internal/daemon"
)

// Styles.
var (
	accentColor = lipgloss.AdaptiveColor{Light: "#6941A5", Dark: "#C5A7FF"}

	headerStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.AdaptiveColor{Light: "#241447", Dark: "#F4EBFF"}).
			Background(lipgloss.AdaptiveColor{Light: "#EBE1FF", Dark: "#29203A"}).
			Padding(0, 1)

	footerStyle = lipgloss.NewStyle().
			Foreground(lipgloss.AdaptiveColor{Light: "#494256", Dark: "#C7BFD1"}).
			Background(lipgloss.AdaptiveColor{Light: "#F3EFF7", Dark: "#211B2A"}).
			Padding(0, 1)

	labelStyle = lipgloss.NewStyle().
			Foreground(colFaint)

	valueStyle = lipgloss.NewStyle().
			Foreground(lipgloss.AdaptiveColor{Light: "#21182B", Dark: "#FAFAFA"})

	dimStyle = lipgloss.NewStyle().
			Foreground(colFaint)

	errorStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.AdaptiveColor{Light: "#B6242B", Dark: "#FF6B6B"})

	runningStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.AdaptiveColor{Light: "#126447", Dark: "#8DE0BC"})

	gatingStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.AdaptiveColor{Light: "#245C82", Dark: "#9FCAEF"})

	greenStyle = lipgloss.NewStyle().
			Foreground(lipgloss.AdaptiveColor{Light: "#1B7F3B", Dark: "#5FD38D"})

	redStyle = lipgloss.NewStyle().
			Foreground(lipgloss.AdaptiveColor{Light: "#B6242B", Dark: "#FF6B6B"})

	retryStyle = lipgloss.NewStyle().
			Foreground(lipgloss.AdaptiveColor{Light: "#805000", Dark: "#F3C582"})

	neutralStyle = lipgloss.NewStyle().
			Foreground(lipgloss.AdaptiveColor{Light: "#6B6378", Dark: "#AAA3B5"})
)

// phaseLabel renders a phase with an icon and color.
// phaseLabelFor is phaseLabel for a run of a daemon that may be stopped: an
// active phase then reads "(running)" in the dim style the list rows use.
func phaseLabelFor(phase string, live bool) string {
	if !live && isActivePhase(phase) {
		icon, style, word := presentPhase(phase, false, false)
		return style.Render(icon + " " + word)
	}
	return phaseLabel(phase)
}

func phaseLabel(phase string) string {
	icon, style := phasePresentation(phase)
	phase = oneLine(phase)
	if phase == "" {
		phase = "unknown"
	}
	return style.Render(icon + " " + phase)
}

func phasePresentation(phase string) (string, lipgloss.Style) {
	switch {
	case phase == "running":
		return "▶", runningStyle
	case phase == "ready":
		return "◇", greenStyle
	case phase == "blocked":
		return "!", retryStyle
	case phase == "gating" || phase == "preparing" || phase == "claiming":
		return "◉", gatingStyle
	case phase == "stopped: gate green":
		return "✔", greenStyle
	case phase == daemon.PhaseClosedExternally:
		return "◌", neutralStyle
	case strings.HasPrefix(phase, "stopped:"):
		return "✘", redStyle
	case phase == "retry" || phase == "deferred" || phase == "interrupted":
		return "↻", retryStyle
	default:
		return "○", neutralStyle
	}
}

// oscBudget bounds the terminal background query. lipgloss would otherwise
// ask lazily with termenv's fixed 5 s timeout, blanking the first frame in
// terminals that never answer OSC 11 (tmux without passthrough, some ssh paths).
const oscBudget = 200 * time.Millisecond

// applyTheme picks the light/dark palette before the first render and reports
// whether the environment decided it; when false the caller should query the
// terminal (see detectBackground). Order: LAMBE_THEME=light|dark, NO_COLOR
// (dark; nothing is coloured anyway), COLORFGBG, then auto-detection.
func applyTheme(getenv func(string) string, setDark func(bool)) bool {
	switch strings.ToLower(strings.TrimSpace(getenv("LAMBE_THEME"))) {
	case "dark":
		setDark(true)
		return true
	case "light":
		setDark(false)
		return true
	}
	if getenv("NO_COLOR") != "" {
		setDark(true)
		return true
	}
	if dark, ok := colorFGBGDark(getenv("COLORFGBG")); ok {
		setDark(dark)
		return true
	}
	return false
}

// colorFGBGDark reads the background index (last field) of COLORFGBG.
func colorFGBGDark(v string) (dark, ok bool) {
	i := strings.LastIndex(v, ";")
	if i < 0 {
		return false, false
	}
	bg, err := strconv.Atoi(v[i+1:])
	if err != nil || bg < 0 || bg > 15 {
		return false, false
	}
	return bg < 7 || bg == 8, true
}

// parseOSC11 extracts dark/light from a reply like ESC ] 11 ; rgb:RRRR/GGGG/BBBB BEL.
func parseOSC11(reply string) (dark, ok bool) {
	_, rest, found := strings.Cut(reply, "rgb:")
	if !found {
		return false, false
	}
	var lum float64
	weights := [3]float64{0.299, 0.587, 0.114}
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) != 3 {
		return false, false
	}
	for n, part := range parts {
		digits := 0
		for digits < len(part) && digits < 4 && strings.ContainsRune("0123456789abcdefABCDEF", rune(part[digits])) {
			digits++
		}
		if digits == 0 {
			return false, false
		}
		v, err := strconv.ParseUint(part[:digits], 16, 16)
		if err != nil {
			return false, false
		}
		top := float64(uint64(1)<<(4*digits) - 1)
		lum += weights[n] * float64(v) / top
	}
	return lum < 0.5, true
}
