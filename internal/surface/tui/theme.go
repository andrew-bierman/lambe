package tui

import (
	"strings"

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
func phaseLabel(phase string) string {
	icon, style := phasePresentation(phase)
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
