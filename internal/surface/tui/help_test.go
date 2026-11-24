package tui

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
)

func runes(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

// allBindings returns every binding in the keyMap by field name.
func allBindings(k keyMap) map[string]key.Binding {
	out := map[string]key.Binding{}
	v := reflect.ValueOf(k)
	for i := range v.NumField() {
		if !v.Field(i).CanInterface() {
			continue
		}
		if b, ok := v.Field(i).Interface().(key.Binding); ok {
			out[v.Type().Field(i).Name] = b
		}
	}
	return out
}

func TestEveryBoundKeyAppearsInHelp(t *testing.T) {
	k := defaultKeyMap(new(focusArea))
	documented := map[string]bool{}
	for _, v := range []helpView{helpQueue, helpAttention, helpHistory, helpDetail} {
		for _, g := range k.helpGroups(v) {
			for _, r := range g.rows {
				documented[r.keys] = true
			}
		}
	}
	for name, b := range allBindings(k) {
		if len(b.Keys()) == 0 || b.Help().Key == "" || b.Help().Desc == "" {
			t.Errorf("%s: binding needs keys, a help key and a description", name)
		}
		if !documented[b.Help().Key] {
			t.Errorf("%s (%s) is bound but missing from every help view", name, b.Help().Key)
		}
	}
}

func TestFooterHintsAreBoundKeys(t *testing.T) {
	m := withSnapshot(t, newSized(t))
	bound := map[string]bool{}
	for _, b := range allBindings(m.keys) {
		for _, key := range b.Keys() {
			bound[key] = true
		}
	}
	check := func(state string) {
		for _, h := range m.hints() {
			for _, key := range h.b.Keys() {
				if !bound[key] {
					t.Errorf("%s: hint %q uses key %q that no binding owns", state, h.b.Help().Key, key)
				}
			}
		}
	}
	check("list")
	m.Update(tea.KeyMsg{Type: tea.KeyTab})
	check("detail")
	m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m.Update(runes("/"))
	check("filtering")
}

func TestPaletteHintsAreBoundKeys(t *testing.T) {
	m := withSnapshot(t, newSized(t))
	bound := map[string]bool{}
	for _, b := range allBindings(m.keys) {
		for _, key := range b.Keys() {
			bound[key] = true
		}
	}
	for _, c := range m.paletteCommands(&commandPalette{}) {
		if c.desc == "" {
			t.Errorf("%q has no description", c.label)
		}
		if c.hint != "" && !bound[c.hint] && !bound[c.hint[:1]] { // "yy" is the chord y then y
			t.Errorf("%q hints key %q that nothing binds", c.label, c.hint)
		}
	}
}

func TestHelpOverlayLifecycle(t *testing.T) {
	m := withSnapshot(t, newSized(t))
	m.Update(runes("?"))
	if !m.showHelp {
		t.Fatal("? did not open help")
	}
	m.helpVP.GotoBottom() // commands follow the key groups
	view := m.View()
	for _, want := range []string{"Help · Queue", "Show attempt history", "Esc close"} {
		if !strings.Contains(view, want) {
			t.Fatalf("help missing %q:\n%s", want, view)
		}
	}
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m.showHelp {
		t.Fatal("esc did not close help")
	}
	m.Update(runes("3"))
	m.Update(runes("?"))
	if !strings.Contains(m.View(), "Help · History") {
		t.Fatal("help does not name the history view")
	}
	m.Update(runes("?"))
	if m.showHelp {
		t.Fatal("? did not toggle help closed")
	}
	m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m.Update(runes("?"))
	if !strings.Contains(m.View(), "Help · Run details") {
		t.Fatal("help does not name the detail view")
	}
}

func TestHelpScrolls(t *testing.T) {
	m := withSnapshot(t, newSized(t))
	m.Update(tea.WindowSizeMsg{Width: 40, Height: 14})
	m.Update(runes("?"))
	if m.helpVP.AtBottom() {
		t.Fatal("help should overflow a 14-row window")
	}
	m.Update(runes("G"))
	if !m.helpVP.AtBottom() || !strings.Contains(m.View(), "Mouse wheel") {
		t.Fatal("G did not reach the end of help")
	}
	m.Update(runes("g"))
	if !m.helpVP.AtTop() {
		t.Fatal("g did not return to the top")
	}
}

func TestQBacksOutOfDetailBeforeQuitting(t *testing.T) {
	m := withSnapshot(t, newSized(t))
	m.Update(tea.KeyMsg{Type: tea.KeyTab})
	if _, cmd := m.Update(runes("q")); cmd != nil || m.focus != focusList {
		t.Fatalf("q in details should return to the list (focus %v, cmd %v)", m.focus, cmd)
	}
	if _, cmd := m.Update(runes("q")); cmd == nil {
		t.Fatal("q in the list should quit")
	}
}

func TestEscClosesEveryOverlay(t *testing.T) {
	m := withSnapshot(t, newSized(t))
	open := map[string]func(){
		"help":     func() { m.Update(runes("?")) },
		"palette":  func() { m.Update(runes(":")) },
		"activity": func() { m.showActivity = true },
	}
	for name, f := range open {
		f()
		m.Update(tea.KeyMsg{Type: tea.KeyEsc})
		if m.showHelp || m.palette != nil || m.showActivity {
			t.Errorf("esc did not close %s", name)
		}
	}
}

func TestPaletteFuzzyMatchesLabelsThenDescriptions(t *testing.T) {
	m := demoModel(frozenClock(t))
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m.Update(runes(":"))
	for query, want := range map[string]string{
		"ahist":   "Show attempt history",
		"qtask":   "Queue an agent task",
		"retries": "Show runs needing attention", // description-only hit
	} {
		m.palette.input.SetValue(query)
		matches := m.palette.matches()
		if len(matches) == 0 || matches[0].label != want {
			t.Errorf("%q: got %v, want first %q", query, labelsOf(matches), want)
		}
	}
	m.palette.input.SetValue("")
	if got := len(m.palette.matches()); got != len(m.palette.commands) {
		t.Errorf("empty query shows %d of %d commands", got, len(m.palette.commands))
	}
}

func labelsOf(cs []paletteCommand) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.label)
	}
	return slices.Clone(out)
}

func TestHelpGoldens(t *testing.T) {
	now := frozenClock(t)
	for _, size := range [][2]int{{40, 24}, {100, 30}} {
		w, h := size[0], size[1]
		t.Run(fmt.Sprintf("help-%dx%d", w, h), func(t *testing.T) {
			m := demoModel(now)
			m.Update(tea.WindowSizeMsg{Width: w, Height: h})
			m.Update(runes("?"))
			fits(t, m.View(), w, h)
			checkGolden(t, fmt.Sprintf("help-%dx%d", w, h), plain(m.View()))
		})
	}
}
