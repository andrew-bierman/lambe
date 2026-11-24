package tui

import (
	"os"
	"slices"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/andrew-bierman/lambe/internal/daemon"
)

const tickInterval = 250 * time.Millisecond

// snapshotMsg carries a freshly read live snapshot.
type snapshotMsg struct {
	snapshot daemon.LiveSnapshot
	alive    bool
	err      error
	// unchanged marks a poll that found the file untouched: snapshot is the
	// previous read and only alive was re-checked.
	unchanged bool
}

// tickMsg triggers the next snapshot read.
type tickMsg time.Time

func (m *Model) visibleRuns() []daemon.LiveRun {
	return m.runsFor(m.showHistory, m.showAttention)
}

// runsFor returns the runs of one dashboard view (queue, attention or history).
func (m *Model) runsFor(showHistory, showAttention bool) []daemon.LiveRun {
	if !showHistory {
		if !showAttention && len(m.upcoming) == 0 {
			return m.snapshot.Runs
		}
		runs := make([]daemon.LiveRun, 0, len(m.snapshot.Runs)+len(m.upcoming))
		seen := make(map[string]bool, len(m.snapshot.Runs))
		for _, run := range m.snapshot.Runs {
			seen[run.IssueID] = true
			if _, attention := daemon.NeedsAttention(run); !showAttention || attention {
				runs = append(runs, run)
			}
		}
		for _, run := range m.upcoming {
			if !seen[run.IssueID] && (!showAttention || run.Phase == "blocked") {
				runs = append(runs, run)
			}
		}
		return runs
	}
	groups := m.historyGroups()
	heads := make([]daemon.LiveRun, len(groups))
	for i, g := range groups {
		heads[i] = g[0]
	}
	return heads
}

// historyAttempts is every history entry, archived and terminal, ungrouped.
func (m *Model) historyAttempts() []daemon.LiveRun {
	history := slices.Clone(m.snapshot.History)
	for _, run := range m.snapshot.Runs {
		if !m.alive || strings.HasPrefix(run.Phase, "stopped:") {
			history = append(history, run)
		}
	}
	return history
}

// historyGroups groups the history per issue (see daemon.GroupHistory): one
// group per issue, headline attempt first, newest group first.
func (m *Model) historyGroups() [][]daemon.LiveRun {
	return daemon.GroupHistory(m.historyAttempts(), func(r daemon.LiveRun) daemon.AttemptKey {
		return daemon.AttemptKey{Issue: r.IssueID, Attempt: r.Attempt, Phase: r.Phase, At: r.UpdatedAt}
	})
}

// setRuns replaces the list contents while preserving the selected issue.
func (m *Model) setRuns(runs []daemon.LiveRun) {
	items := make([]list.Item, len(runs))
	var counts map[string]int
	if m.showHistory {
		counts = map[string]int{}
		for _, g := range m.historyGroups() {
			counts[g[0].IssueID] = len(g)
		}
	}
	for i, run := range runs {
		items[i] = runItem{run: run, live: m.alive, attempts: counts[run.IssueID]}
	}
	m.replaceListItems(items)
	if m.selectedID != "" {
		for i, item := range m.list.VisibleItems() {
			if runItem, ok := item.(runItem); ok && runItem.run.IssueID == m.selectedID &&
				(!m.showHistory || (runItem.run.Attempt == m.selectedAttempt && runItem.run.UpdatedAt.Equal(m.selectedAt))) {
				m.list.Select(i)
				return
			}
		}
	}
	if visible := m.list.VisibleItems(); len(visible) > 0 && m.list.SelectedItem() == nil {
		m.list.Select(len(visible) - 1)
	}
	m.syncSelection()
}

func (m *Model) replaceListItems(items []list.Item) {
	if filter := m.list.SetItems(items); filter != nil {
		m.list, _ = m.list.Update(filter())
		m.list.SetSize(m.list.Width(), m.list.Height())
	}
}

// syncSelection records the selected issue and refreshes the detail pane when
// the selection moved.
func (m *Model) syncSelection() {
	previousID, previousAt, previousAttempt := m.selectedID, m.selectedAt, m.selectedAttempt
	if item, ok := m.list.SelectedItem().(runItem); ok {
		m.selectedID = item.run.IssueID
		m.selectedAt = item.run.UpdatedAt
		m.selectedAttempt = item.run.Attempt
	} else {
		m.selectedID = ""
	}
	if m.selectedID != previousID || (m.showHistory && (!m.selectedAt.Equal(previousAt) || m.selectedAttempt != previousAttempt)) {
		m.refreshDetail(!m.showHistory && !m.showAttention && m.detail.AtBottom())
		if m.showHistory || m.showAttention {
			m.detail.GotoTop()
		}
	}
}

func (m *Model) selectedRun() (daemon.LiveRun, bool) {
	item, ok := m.list.SelectedItem().(runItem)
	if !ok {
		return daemon.LiveRun{}, false
	}
	return item.run, true
}

// snapshotReader polls one live file. It stats the file first and skips the
// read, the JSON decode and the model's deep compare when the file is the same
// inode with the same mtime and size: the daemon replaces the file by rename,
// so any write is a new inode. One reader serves one in-flight read at a time
// (see Model.readNow); it is not safe for concurrent use.
type snapshotReader struct {
	path string
	info os.FileInfo
	snap daemon.LiveSnapshot
	have bool
}

// read polls the file; force bypasses the unchanged short-circuit.
func (r *snapshotReader) read(force bool) snapshotMsg {
	info, err := os.Stat(r.path)
	if err != nil {
		r.have = false
		return snapshotMsg{err: err}
	}
	if r.have && !force && os.SameFile(r.info, info) && r.info.ModTime().Equal(info.ModTime()) && r.info.Size() == info.Size() {
		return snapshotMsg{snapshot: r.snap, alive: daemon.LiveAlive(r.snap), unchanged: true}
	}
	// The stat above predates the read, so a write in between is re-read next poll.
	snap, alive, err := daemon.ReadLive(r.path)
	if err != nil {
		r.have = false
		return snapshotMsg{err: err}
	}
	r.info, r.snap, r.have = info, snap, true
	return snapshotMsg{snapshot: snap, alive: alive}
}

// readNow returns a command that polls the snapshot, or nil when a read is
// already in flight: polls never overlap, however slow the disk. A forced read
// requested meanwhile runs as soon as the in-flight one lands.
func (m *Model) readNow(force bool) tea.Cmd {
	if m.reader == nil {
		return nil
	}
	if m.reading {
		m.forceNext = m.forceNext || force
		return nil
	}
	m.reading = true
	reader := m.reader
	return func() tea.Msg { return reader.read(force) }
}

// tick returns a command that fires after tickInterval.
func tick() tea.Cmd {
	return tea.Tick(tickInterval, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}
