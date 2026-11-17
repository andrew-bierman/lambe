package symphony

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/andrew-bierman/lambe/internal/tracker"
)

// fakeTracker is an in-memory tracker.Tracker for loop tests. Dispatchable
// with IDs performs the ID-refresh read and returns issues regardless of
// status; the status read filters by status.
type fakeTracker struct {
	mu       sync.Mutex
	issues   map[string]tracker.Issue
	attempts []tracker.Attempt
}

func newFakeTracker(issues ...tracker.Issue) *fakeTracker {
	ft := &fakeTracker{issues: map[string]tracker.Issue{}}
	for _, issue := range issues {
		ft.issues[issue.ID] = issue
	}
	return ft
}

func (f *fakeTracker) set(id, status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	issue := f.issues[id]
	issue.ID = id
	issue.Status = status
	f.issues[id] = issue
}

func (f *fakeTracker) get(id string) (tracker.Issue, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	issue, ok := f.issues[id]
	return issue, ok
}

func (f *fakeTracker) Capabilities(_ context.Context) (tracker.Capabilities, error) {
	return tracker.Capabilities{}, nil
}

func (f *fakeTracker) Dispatchable(_ context.Context, q tracker.DispatchQuery) ([]tracker.Issue, error) {
	if q.Empty() {
		return nil, nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(q.IDs) > 0 {
		want := map[string]bool{}
		for _, id := range q.IDs {
			want[id] = true
		}
		var out []tracker.Issue
		for id, issue := range f.issues {
			if want[id] {
				out = append(out, issue)
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
		return out, nil
	}
	var out []tracker.Issue
	for _, issue := range f.issues {
		for _, status := range q.Statuses {
			if issue.Status == status {
				out = append(out, issue)
				break
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (f *fakeTracker) Claim(_ context.Context, id string) (tracker.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	issue, ok := f.issues[id]
	if !ok {
		return tracker.Issue{}, fmt.Errorf("no issue %s", id)
	}
	if issue.Assignee != "" {
		return tracker.Issue{}, fmt.Errorf("claim %s: %w: already held", id, tracker.ErrLostRace)
	}
	issue.Assignee = "lambe"
	issue.Status = "in_progress"
	f.issues[id] = issue
	return issue, nil
}

func (f *fakeTracker) Release(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	issue, ok := f.issues[id]
	if !ok {
		return fmt.Errorf("no issue %s", id)
	}
	issue.Assignee = ""
	issue.Status = "open"
	f.issues[id] = issue
	return nil
}

func (f *fakeTracker) Heartbeat(_ context.Context, _ string) error { return nil }

func (f *fakeTracker) Reclaim(_ context.Context, _ time.Duration, _ []string) error {
	return nil
}

func (f *fakeTracker) Defer(_ context.Context, _ string, _ time.Time, _ string) error {
	return nil
}

func (f *fakeTracker) Events(_ context.Context, _ int64) ([]tracker.Event, error) {
	return nil, tracker.ErrJournalDisabled
}

func (f *fakeTracker) EnsureIssue(_ context.Context, key string, _ tracker.IssueSpec) (tracker.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if issue, ok := f.issues[key]; ok {
		return issue, nil
	}
	issue := tracker.Issue{ID: key, Status: "ready"}
	f.issues[key] = issue
	return issue, nil
}

func (f *fakeTracker) PutAttempt(_ context.Context, a tracker.Attempt) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts = append(f.attempts, a)
	return nil
}

func (f *fakeTracker) Attempts(_ context.Context, _ string) ([]tracker.Attempt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]tracker.Attempt(nil), f.attempts...), nil
}

// ListByMetadata is unused by the loop's own reads; the fixture serves
// attempts per session only.
func (f *fakeTracker) ListByMetadata(context.Context, string, string) ([]tracker.Issue, error) {
	return nil, nil
}

// turnCall records one fakeRunner invocation.
type turnCall struct {
	TurnRequest
	Result TurnResult
}

// fakeRunner is a scripted AgentRunner. Each TurnFunc handles one call in
// order; when the script is exhausted every call fails.
type fakeRunner struct {
	mu     sync.Mutex
	calls  []turnCall
	script []func(TurnRequest) TurnResult
}

func newFakeRunner(steps ...func(TurnRequest) TurnResult) *fakeRunner {
	return &fakeRunner{script: steps}
}

func (r *fakeRunner) RunTurn(_ context.Context, req TurnRequest) (TurnResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	idx := len(r.calls)
	var res TurnResult
	if idx < len(r.script) {
		res = r.script[idx](req)
	} else {
		res = TurnResult{Exit: TurnFailure}
	}
	if req.Turn > 1 && res.ThreadID == "" {
		res.ThreadID = req.ThreadID
	}
	r.calls = append(r.calls, turnCall{TurnRequest: req, Result: res})
	return res, nil
}

func (r *fakeRunner) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func (r *fakeRunner) recorded() []TurnRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	reqs := make([]TurnRequest, len(r.calls))
	for i, c := range r.calls {
		reqs[i] = c.TurnRequest
	}
	return reqs
}

// fakeGate is a scripted GateEvaluator returning one outcome per call.
type fakeGate struct {
	mu       sync.Mutex
	outcomes []GateOutcome
	errs     []error
	calls    []GateRequest
}

func (g *fakeGate) Evaluate(_ context.Context, req GateRequest) (GateOutcome, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, req)
	idx := len(g.calls) - 1
	var outcome GateOutcome
	var err error
	if idx < len(g.outcomes) {
		outcome = g.outcomes[idx]
	}
	if idx < len(g.errs) {
		err = g.errs[idx]
	}
	return outcome, err
}

// fakeMaterializer records formula materializations and can inject issues
// into a fake tracker to simulate cook+pour.
type fakeMaterializer struct {
	mu          sync.Mutex
	names       []string
	tr          *fakeTracker
	materialize func(f Formula)
}

func (m *fakeMaterializer) Materialize(_ context.Context, f Formula) error {
	m.mu.Lock()
	m.names = append(m.names, f.Name)
	fn := m.materialize
	m.mu.Unlock()
	if fn != nil {
		fn(f)
	}
	return nil
}

func (m *fakeMaterializer) materialized() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.names...)
}
