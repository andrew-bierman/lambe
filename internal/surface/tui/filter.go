package tui

import (
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// Filter matches what the table shows: id, title, the PHASE word and the
// workflow name. Terms are separated by spaces and all must match; a term
// may be scoped with a field prefix (id:, phase:, wf:, title:). Matching is a
// case-insensitive substring test. shortcut: no fuzzy fallback, because
// subsequence matches were noise (/passed hit escalated rows); revisit only
// if users ask for typo tolerance.

const filterSep = "\x1f" // separates fields inside runItem.FilterValue

// Field indexes inside a filter target.
const (
	fID = iota
	fTitle
	fPhase
	fWorkflow
	fieldCount
)

var filterFields = map[string]int{"id": fID, "title": fTitle, "phase": fPhase, "wf": fWorkflow, "workflow": fWorkflow}

// FilterValue is the searchable text of the row: the same words the table shows.
func (i runItem) FilterValue() string {
	now := time.Now
	if i.now != nil {
		now = i.now
	}
	_, _, phase := presentPhase(i.run.Phase, i.live, stalledFor(i.run, i.live, now()) > 0)
	parts := [fieldCount]string{
		fID:       i.run.IssueID,
		fTitle:    strings.Join(strings.Fields(cleanTaskText(i.run.Title)), " "),
		fPhase:    phase,
		fWorkflow: i.run.Workflow,
	}
	for k, p := range parts {
		parts[k] = strings.ReplaceAll(p, filterSep, " ")
	}
	return strings.Join(parts[:], filterSep)
}

// filterTerm is one parsed query term; field is -1 when unscoped.
type filterTerm struct {
	field int
	text  string // lower case
}

func parseFilter(query string) []filterTerm {
	var terms []filterTerm
	for _, w := range strings.Fields(strings.ToLower(query)) {
		field := -1
		if name, rest, ok := strings.Cut(w, ":"); ok {
			if f, known := filterFields[name]; known {
				field, w = f, rest
			}
		}
		if w != "" {
			terms = append(terms, filterTerm{field, w})
		}
	}
	return terms
}

// score ranks how well one field matches (lower is better); ok is false when it does not.
func (t filterTerm) score(field int, value string) (score int, ok bool) {
	if t.field >= 0 && t.field != field {
		return 0, false
	}
	v := strings.ToLower(value)
	switch {
	case v == t.text:
		return field, true // id 0, title 1, phase 2, workflow 3
	case strings.HasPrefix(v, t.text):
		return 4 + field, true
	case field == fTitle && strings.Contains(v, " "+t.text):
		return 8, true
	case strings.Contains(v, t.text):
		return 9 + field, true
	}
	return 0, false
}

// filterRuns is the list.FilterFunc: AND of all terms, best matches first, ties keep table order.
func filterRuns(query string, targets []string) []list.Rank {
	terms := parseFilter(query)
	type scored struct{ index, score int }
	var hits []scored
	for idx, target := range targets {
		var f [fieldCount]string
		copy(f[:], strings.Split(target, filterSep))
		total, all := 0, true
		for _, t := range terms {
			best, found := -1, false
			for field, v := range f {
				if s, ok := t.score(field, v); ok && (!found || s < best) {
					best, found = s, true
				}
			}
			if !found {
				all = false
				break
			}
			total += best
		}
		if all {
			hits = append(hits, scored{idx, total})
		}
	}
	sort.SliceStable(hits, func(a, b int) bool { return hits[a].score < hits[b].score })
	ranks := make([]list.Rank, len(hits))
	for i, h := range hits {
		ranks[i] = list.Rank{Index: h.index}
	}
	return ranks
}

// cell truncates text to width, pads it and renders it with base, marking the
// substrings the active filter terms matched.
func (m *Model) cell(text string, width int, base lipgloss.Style, field int) string {
	text = oneLine(text)
	text = ansi.Truncate(text, width, "…")
	gap := strings.Repeat(" ", max(0, width-lipgloss.Width(text)))
	hl := base.Underline(true).Bold(true).Foreground(accentColor)
	lower := strings.ToLower(text)
	if len(lower) != len(text) { // case folding changed byte lengths: skip marking
		return base.Render(text + gap)
	}
	marked := make([]bool, len(text))
	hit := false
	for _, t := range m.filterTerms {
		if t.field >= 0 && t.field != field {
			continue
		}
		for from := 0; ; {
			at := strings.Index(lower[from:], t.text)
			if at < 0 {
				break
			}
			for k := from + at; k < from+at+len(t.text); k++ {
				marked[k], hit = true, true
			}
			from += at + len(t.text)
		}
	}
	if !hit {
		return base.Render(text + gap)
	}
	var b strings.Builder
	for start := 0; start < len(text); {
		end := start
		for end < len(text) && marked[end] == marked[start] {
			end++
		}
		// Runs never split a rune: match bytes are whole ASCII/UTF-8 sequences of the lowered text.
		style := base
		if marked[start] {
			style = hl
		}
		b.WriteString(style.Render(text[start:end]))
		start = end
	}
	return b.String() + base.Render(gap)
}
