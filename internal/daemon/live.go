package daemon

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/andrew-bierman/lambe/internal/config"
	"github.com/andrew-bierman/lambe/internal/harness"
	"github.com/andrew-bierman/lambe/internal/mcp"
	"github.com/andrew-bierman/lambe/internal/proc"
	"github.com/andrew-bierman/lambe/internal/symphony"
)

// LiveRun combines supervisor progress with the latest agent output.
type LiveRun struct {
	symphony.Progress
	UpdatedAt time.Time
	Tail      string
	TailBytes int64
}

// LiveSnapshot is a derived view; checkpoints and the tracker remain authoritative.
type LiveSnapshot struct {
	PID       int
	Identity  string
	UpdatedAt time.Time
	Runs      []LiveRun
}

// LivePath returns the private projection path for one configured daemon.
func LivePath(stateDir string, cfg *config.Config) string {
	return filepath.Join(stateDir, "live", strings.TrimSuffix(InstanceLockName(cfg), ".lock")+".json")
}

// ReadLive reads a bounded snapshot and marks a stopped daemon as historical.
func ReadLive(path string) (LiveSnapshot, bool, error) {
	var s LiveSnapshot
	f, err := os.Open(path) // #nosec G304 -- local configuration selects the state directory
	if err != nil {
		return s, false, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, 8<<20))
	if err != nil {
		return s, false, err
	}
	if len(data) == 8<<20 {
		return s, false, fmt.Errorf("live snapshot exceeds size limit")
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return s, false, err
	}
	identity, err := proc.ProcessIdentity(s.PID)
	return s, err == nil && identity == s.Identity && s.Identity != "", nil
}

type liveView struct {
	stop chan struct{}
	done chan struct{}

	registry *RunRegistry

	mu       sync.Mutex
	path     string
	snapshot LiveSnapshot
	logError func(error)
}

func newLiveView(path string, report func(error)) *liveView {
	identity, _ := proc.ProcessIdentity(os.Getpid())
	v := &liveView{stop: make(chan struct{}), done: make(chan struct{}), path: path, snapshot: LiveSnapshot{PID: os.Getpid(), Identity: identity, UpdatedAt: time.Now()}, logError: report}
	if err := v.save(); err != nil && report != nil {
		report(err)
	}
	return v
}

func (v *liveView) progress(p symphony.Progress) {
	v.update(p.IssueID, func(r *LiveRun) { r.Progress = p })
}

func (v *liveView) event(id string, event harness.Event) {
	var text string
	switch e := event.(type) {
	case harness.MessageCompletedEvent:
		if e.Role == "assistant" {
			text = e.Text
		}
	case harness.ToolExecutionStartedEvent:
		text = "tool started: " + e.ToolName
	case harness.ToolExecutionEndedEvent:
		text = fmt.Sprintf("tool finished: %s (error=%t)", e.ToolName, e.IsError)
	case harness.DiagnosticEvent:
		text = e.Message
	case harness.AutoRetryScheduledEvent:
		text = "backend retry: " + e.ErrorMessage
	}
	if text == "" {
		return
	}
	v.update(id, func(r *LiveRun) {
		if text != "" {
			r.TailBytes += int64(len(text) + 1)
			r.Tail += text + "\n"
			if len(r.Tail) > maxRunTail {
				r.Tail = r.Tail[len(r.Tail)-maxRunTail:]
			}
		}
	})
}

func (v *liveView) update(id string, change func(*LiveRun)) {
	v.mu.Lock()
	defer v.mu.Unlock()
	index := -1
	for i := range v.snapshot.Runs {
		if v.snapshot.Runs[i].IssueID == id {
			index = i
			break
		}
	}
	if index < 0 {
		// Keep all owned work visible; bound terminal history to the remaining slots.
		if len(v.snapshot.Runs) >= 100 {
			for i, old := range v.snapshot.Runs {
				if !strings.HasPrefix(old.Phase, "stopped:") {
					continue
				}
				v.snapshot.Runs = slices.Delete(v.snapshot.Runs, i, i+1)
				if reg := v.registry; reg != nil {
					reg.mu.Lock()
					if r := reg.runs[old.IssueID]; r != nil && r.ProjectionOnly {
						delete(reg.runs, old.IssueID)
						reg.order = slices.DeleteFunc(reg.order, func(id string) bool { return id == old.IssueID })
					}
					reg.mu.Unlock()
				}
				break
			}
		}
		v.snapshot.Runs = append(v.snapshot.Runs, LiveRun{Progress: symphony.Progress{IssueID: id}})
		index = len(v.snapshot.Runs) - 1
	}
	change(&v.snapshot.Runs[index])
	if reg := v.registry; reg != nil {
		r := v.snapshot.Runs[index]
		state := mcp.StateRunning
		if r.Phase == "retry" || r.Phase == "deferred" || r.Phase == "interrupted" {
			state = mcp.StateDeferred
		}
		if strings.HasPrefix(r.Phase, "stopped:") {
			state = mcp.StateCanceled
			if r.Phase == "stopped: gate green" {
				state = mcp.StateSettled
			}
		}
		reg.mu.Lock()
		if _, exists := reg.runs[id]; !exists {
			reg.order = append(reg.order, id)
		}
		run := reg.runs[id]
		if run == nil {
			run = &Run{IssueID: id, Agent: "issues", ProjectionOnly: true}
			reg.runs[id] = run
		}
		run.ProjectionOnly = true
		run.Session, run.Workspace, run.State, run.Attempt = r.Thread, r.Workspace, state, r.Attempt
		run.LastEventAt, run.WakeAt, run.Tail, run.Outcome = time.Now(), r.WakeAt, r.Tail, r.Phase
		reg.mu.Unlock()
	}
	v.snapshot.UpdatedAt = time.Now()
	v.snapshot.Runs[index].UpdatedAt = v.snapshot.UpdatedAt
}

func (v *liveView) save() error {
	if err := os.MkdirAll(filepath.Dir(v.path), 0o700); err != nil {
		return err
	}
	v.mu.Lock()
	data, err := json.Marshal(v.snapshot)
	v.mu.Unlock()
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(v.path), ".live-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	_, err = f.Write(data)
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), v.path)
}

func (v *liveView) start() {
	go func() {
		defer close(v.done)
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		var last time.Time
		var warned time.Time
		flush := func() {
			v.mu.Lock()
			updated := v.snapshot.UpdatedAt
			v.mu.Unlock()
			if updated.Equal(last) {
				return
			}
			if err := v.save(); err != nil {
				if v.logError != nil && time.Since(warned) > 30*time.Second {
					v.logError(err)
					warned = time.Now()
				}
				return
			}
			last = updated
		}
		for {
			select {
			case <-v.stop:
				flush()
				return
			case <-ticker.C:
				flush()
			}
		}
	}()
}

func (v *liveView) close() {
	close(v.stop)
	select {
	case <-v.done:
	case <-time.After(2 * time.Second):
	}
}
