package tui

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"testing"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// forceBackground pins lipgloss to truecolor output on a light or dark
// terminal background for one test, so adaptive colors resolve
// deterministically no matter where the suite runs.
func forceBackground(t *testing.T, dark bool) {
	t.Helper()
	profile := lipgloss.ColorProfile()
	wasDark := lipgloss.HasDarkBackground()
	lipgloss.SetColorProfile(termenv.TrueColor)
	lipgloss.SetHasDarkBackground(dark)
	t.Cleanup(func() {
		lipgloss.SetColorProfile(profile)
		lipgloss.SetHasDarkBackground(wasDark)
	})
}

// cell is one terminal cell with the colors the terminal would paint it in.
type cell struct {
	r      rune
	fg, bg string // "#rrggbb", "ansiN", or "" for the terminal default
	bold   bool
}

// parseCells replays the SGR escapes in a rendered view and returns each
// line as cells. Only the sequences lipgloss emits (reset, bold, faint,
// reverse, 16/256/truecolor fg and bg) are interpreted; other CSI sequences
// are skipped.
func parseCells(view string) [][]cell {
	var (
		lines   [][]cell
		cur     []cell
		fg, bg  string
		bold    bool
		reverse bool
	)
	rs := []rune(view)
	for i := 0; i < len(rs); i++ {
		switch r := rs[i]; {
		case r == '\x1b' && i+1 < len(rs) && rs[i+1] == '[':
			j := i + 2
			for j < len(rs) && (rs[j] < 0x40 || rs[j] > 0x7e) {
				j++
			}
			if j >= len(rs) {
				return append(lines, cur)
			}
			if rs[j] == 'm' {
				fg, bg, bold, reverse = applySGR(string(rs[i+2:j]), fg, bg, bold, reverse)
			}
			i = j
		case r == '\n':
			lines, cur = append(lines, cur), nil
		default:
			c := cell{r: r, fg: fg, bg: bg, bold: bold}
			if reverse {
				c.fg, c.bg = bg, fg
			}
			cur = append(cur, c)
		}
	}
	return append(lines, cur)
}

func applySGR(params, fg, bg string, bold, reverse bool) (string, string, bool, bool) {
	p := strings.Split(params, ";")
	if params == "" {
		p = []string{"0"}
	}
	num := func(i int) int { n, _ := strconv.Atoi(p[i]); return n }
	for i := 0; i < len(p); i++ {
		switch n := num(i); {
		case n == 0:
			fg, bg, bold, reverse = "", "", false, false
		case n == 1:
			bold = true
		case n == 22:
			bold = false
		case n == 7:
			reverse = true
		case n == 27:
			reverse = false
		case n == 39:
			fg = ""
		case n == 49:
			bg = ""
		case n >= 30 && n <= 37, n >= 90 && n <= 97:
			fg = fmt.Sprintf("ansi%d", n)
		case n >= 40 && n <= 47, n >= 100 && n <= 107:
			bg = fmt.Sprintf("ansi%d", n)
		case n == 38 || n == 48:
			var color string
			switch {
			case i+4 < len(p) && p[i+1] == "2":
				color = fmt.Sprintf("#%02x%02x%02x", num(i+2), num(i+3), num(i+4))
				i += 4
			case i+2 < len(p) && p[i+1] == "5":
				color = "ansi256-" + p[i+2]
				i += 2
			}
			if n == 38 {
				fg = color
			} else {
				bg = color
			}
		}
	}
	return fg, bg, bold, reverse
}

// annotate renders cells as text with a {fg bg} marker wherever the colors
// change, so a golden shows what is drawn in which color.
func annotate(lines [][]cell) string {
	var b strings.Builder
	for _, line := range lines {
		for len(line) > 0 && line[len(line)-1].r == ' ' && line[len(line)-1].bg == "" {
			line = line[:len(line)-1]
		}
		var pfg, pbg string
		var pbold bool
		for _, c := range line {
			if c.fg != pfg || c.bg != pbg || c.bold != pbold {
				b.WriteString("{" + orDefault(c.fg) + " on " + orDefault(c.bg))
				if c.bold {
					b.WriteString(" bold")
				}
				b.WriteString("}")
				pfg, pbg, pbold = c.fg, c.bg, c.bold
			}
			b.WriteRune(c.r)
		}
		b.WriteString("\n")
	}
	return b.String()
}

func orDefault(c string) string {
	if c == "" {
		return "default"
	}
	return c
}

func channel(v float64) float64 {
	v /= 255
	if v <= 0.03928 {
		return v / 12.92
	}
	return math.Pow((v+0.055)/1.055, 2.4)
}

func luminance(hex string) float64 {
	var r, g, b int
	_, _ = fmt.Sscanf(hex, "#%02x%02x%02x", &r, &g, &b)
	return 0.2126*channel(float64(r)) + 0.7152*channel(float64(g)) + 0.0722*channel(float64(b))
}

// contrast is the WCAG 2.x contrast ratio between two "#rrggbb" colors.
func contrast(a, b string) float64 {
	la, lb := luminance(a), luminance(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

func TestContrastMath(t *testing.T) {
	if got := contrast("#000000", "#ffffff"); math.Abs(got-21) > 0.01 {
		t.Errorf("black on white = %.2f, want 21", got)
	}
	if got := contrast("#777777", "#777777"); math.Abs(got-1) > 0.01 {
		t.Errorf("same color = %.2f, want 1", got)
	}
}

// Terminal defaults assumed for a light profile: the dashboard leaves most
// text and the background to the terminal.
const (
	lightDefaultFg = "#000000"
	lightDefaultBg = "#ffffff"
)

// minTextContrast is WCAG AA for normal text.
const minTextContrast = 4.5

// lightViews are the screens checked on a light background.
func lightViews(t *testing.T) map[string]string {
	t.Helper()
	now := frozenClock(t)
	views := map[string]string{}
	for _, w := range []int{40, 100} {
		h := 24
		if w == 100 {
			h = 30
		}
		m := demoModel(now)
		m.Update(tea.WindowSizeMsg{Width: w, Height: h})
		views[fmt.Sprintf("queue-%dx%d", w, h)] = m.View()
		for key, name := range map[rune]string{'2': "attention", '3': "history"} {
			m := demoModel(now)
			m.Update(tea.WindowSizeMsg{Width: w, Height: h})
			m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{key}})
			views[fmt.Sprintf("%s-%dx%d", name, w, h)] = m.View()
		}
	}
	return views
}

// TestGoldenLightBackground snapshots the colors of the dashboard on a light
// terminal at the two targeted widths (a 40-column split and a wide window).
func TestGoldenLightBackground(t *testing.T) {
	forceBackground(t, false)
	views := lightViews(t)
	for _, name := range []string{"queue-40x24", "queue-100x30", "attention-40x24", "history-40x24", "history-100x30"} {
		t.Run(name, func(t *testing.T) {
			checkGolden(t, "light-"+name, annotate(parseCells(views[name])))
		})
	}
}

// TestLightBackgroundTextIsReadable fails when any letter or digit is drawn
// with less than WCAG AA contrast against its background on a light terminal.
// Box-drawing rules and symbols are decoration and are not held to it.
func TestLightBackgroundTextIsReadable(t *testing.T) {
	forceBackground(t, false)
	for name, view := range lightViews(t) {
		t.Run(name, func(t *testing.T) {
			seen := map[string]bool{}
			for y, line := range parseCells(view) {
				for x, c := range line {
					if !unicode.IsLetter(c.r) && !unicode.IsDigit(c.r) {
						continue
					}
					fg, bg := c.fg, c.bg
					if fg == "" {
						fg = lightDefaultFg
					}
					if bg == "" {
						bg = lightDefaultBg
					}
					if strings.HasPrefix(fg, "ansi") || strings.HasPrefix(bg, "ansi") {
						continue // palette colors depend on the terminal theme
					}
					if ratio := contrast(fg, bg); ratio < minTextContrast && !seen[fg+bg] {
						seen[fg+bg] = true
						t.Errorf("%s: %.2f:1 (< %.1f) for %s on %s, first at row %d col %d (%q)", name, ratio, minTextContrast, fg, bg, y, x, c.r)
					}
				}
			}
		})
	}
}

// TestLightProfileUsesLightPalette guards the forcing itself: a light render
// must not contain the dark palette's foreground, or the tests above would
// prove nothing.
func TestLightProfileUsesLightPalette(t *testing.T) {
	forceBackground(t, false)
	view := lightViews(t)["queue-100x30"]
	if !strings.Contains(view, "38;2;") {
		t.Fatal("expected truecolor escapes")
	}
	for _, dark := range []string{"#F4EBFF", "#C5A7FF", "#29203A"} {
		var r, g, b int
		_, _ = fmt.Sscanf(dark, "#%02X%02X%02X", &r, &g, &b)
		if strings.Contains(view, fmt.Sprintf("2;%d;%d;%dm", r, g, b)) {
			t.Errorf("light render contains dark palette color %s", dark)
		}
	}
}
