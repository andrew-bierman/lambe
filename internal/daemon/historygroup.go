package daemon

import (
	"slices"
	"time"
)

// PhaseClosedExternally is the stop phase of a run whose issue was closed
// outside lambe (a person, another machine) before the daemon finished it.
// It is neutral, not a failure.
const PhaseClosedExternally = "stopped: closed externally"

// legacyPhaseRecoveredClosed is the phase older daemons persisted for the
// same condition; reads map it to PhaseClosedExternally.
const legacyPhaseRecoveredClosed = "stopped: recovered closed issue"

// canonicalPhase maps legacy phase spellings to their current name.
func canonicalPhase(phase string) string {
	if phase == legacyPhaseRecoveredClosed {
		return PhaseClosedExternally
	}
	return phase
}

// AttemptKey is what grouping needs to know about one history entry.
type AttemptKey struct {
	Issue   string
	Attempt int
	Phase   string
	At      time.Time
}

// GroupHistory partitions history entries into one group per issue, the
// single grouping shared by the TUI and the state API. Each group lists its
// attempts newest first, one entry per attempt number, and its first entry is
// the headline: the highest attempt, never a restart-interrupted one when a
// non-interrupted attempt at the same or a later number exists. An
// interrupted entry loses to a non-interrupted one of the same attempt
// number (as does a row with no phase), so a restart's leftover row never
// shadows the real outcome. Groups
// are ordered by their headline's time, newest first.
func GroupHistory[T any](items []T, key func(T) AttemptKey) [][]T {
	byIssue := map[string][]T{}
	var order []string
	for _, it := range items {
		issue := key(it).Issue
		if _, ok := byIssue[issue]; !ok {
			order = append(order, issue)
		}
		byIssue[issue] = append(byIssue[issue], it)
	}
	groups := make([][]T, 0, len(order))
	for _, issue := range order {
		group := slices.Clone(byIssue[issue])
		slices.SortStableFunc(group, func(a, b T) int {
			ka, kb := key(a), key(b)
			if ka.Attempt != kb.Attempt {
				return kb.Attempt - ka.Attempt
			}
			if ia, ib := weakPhase(ka.Phase), weakPhase(kb.Phase); ia != ib {
				if ia {
					return 1
				}
				return -1
			}
			return kb.At.Compare(ka.At)
		})
		// One entry per attempt number: the first of each run of equals is
		// the best by the order above.
		group = slices.CompactFunc(group, func(a, b T) bool { return key(a).Attempt == key(b).Attempt })
		groups = append(groups, group)
	}
	slices.SortStableFunc(groups, func(a, b []T) int { return key(b[0]).At.Compare(key(a[0]).At) })
	return groups
}

// weakPhase marks records that lose to a real outcome for the same attempt: a
// restart-interrupted row, or a registry row that carries no operator phase.
func weakPhase(phase string) bool { return phase == "" || phase == "interrupted" }
