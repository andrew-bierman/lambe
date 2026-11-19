package symphony

import "time"

// Progress is a read-only projection of a supervised issue, never scheduler state.
type Progress struct {
	IssueID   string
	Title     string
	Phase     string
	Attempt   int
	Turn      int
	Thread    string
	Workspace string
	WakeAt    time.Time
	LastGate  string
}

func (l *Loop) progress(s *session, phase string, retry *Retry) {
	if l.Observe == nil {
		return
	}
	p := Progress{IssueID: s.issue.ID, Title: s.issue.Title, Phase: phase, Attempt: s.attempt, Turn: s.turn, Thread: s.thread, Workspace: s.workspace}
	const maxGatePreview = 2048
	p.LastGate = s.lastGate[:min(maxGatePreview, len(s.lastGate))]
	if retry != nil {
		p.WakeAt = retry.At
	}
	l.Observe(p)
}
