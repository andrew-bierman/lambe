package daemon

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/andrew-bierman/lambe/internal/config"
	"github.com/andrew-bierman/lambe/internal/httpstate"
	"github.com/andrew-bierman/lambe/internal/proc"
)

// ReportCache holds the morning report as of the daemon's last tick, so the
// read-only HTTP state surface can serve tracker-derived sections
// (needs-human, gate rejections) without a tracker read per request. It is
// safe for concurrent use.
type ReportCache struct {
	mu  sync.RWMutex
	rep Report
	at  time.Time
}

// Store replaces the cached report and stamps the read time.
func (c *ReportCache) Store(rep Report) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rep = rep
	c.at = time.Now()
}

// Load returns the cached report and when it was taken. The zero Report is
// returned before the first tick.
func (c *ReportCache) Load() (Report, time.Time) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.rep, c.at
}

// StateSourceAdapter implements httpstate.StateSource over daemon state:
// live runs come from the run registry, tracker-derived sections from the
// report cache, operator phases and gate rejections from the live
// projection file, and Refresh pokes the daemon for a prompt
// re-evaluation. It is safe for concurrent use.
type StateSourceAdapter struct {
	registry *RunRegistry
	reports  *ReportCache
	poke     func()
	inflight atomic.Bool

	// info is the daemon identity block served with every response; zero
	// until Identify wires it.
	info httpstate.Daemon
	// livePath is the live projection file backing the phase overlays, the
	// gate-rejection derivation, and the history merge; empty disables
	// those reads.
	livePath string
}

// Compile-time check that StateSourceAdapter satisfies the HTTP state seam.
var _ httpstate.StateSource = (*StateSourceAdapter)(nil)

// NewStateSource builds the adapter. poke, when non-nil, is the advisory
// refresh hook the daemon wires to an immediate background tick.
func NewStateSource(registry *RunRegistry, reports *ReportCache, poke func()) *StateSourceAdapter {
	return &StateSourceAdapter{registry: registry, reports: reports, poke: poke}
}

// Identify stamps the daemon identity facts served with every response.
// PID and process identity are taken from the serving process; machine,
// version, and config path come from the daemon's configuration.
func (a *StateSourceAdapter) Identify(machine, version, configPath string) {
	a.info = httpstate.Daemon{
		PID:        os.Getpid(),
		Machine:    machine,
		Version:    version,
		ConfigPath: configPath,
	}
	if id, err := proc.ProcessIdentity(os.Getpid()); err == nil {
		a.info.Identity = id
	}
}

// WatchLive points the adapter at the daemon's live projection file, the
// source of the operator phases, gate rejections, and restart history. The
// file is written by the projection's own flusher; reads degrade to
// "unknown" when it is missing or unreadable.
func (a *StateSourceAdapter) WatchLive(path string) {
	a.livePath = path
}

// Identity implements StateSource, returning the daemon identity block
// stamped by Identify.
func (a *StateSourceAdapter) Identity() httpstate.Daemon { return a.info }

// readProjection reads the live projection file; a missing or unreadable
// projection yields the zero value, degrading everything it feeds to
// "unknown" rather than an error a viewer surface would have to handle.
func (a *StateSourceAdapter) readProjection() LiveSnapshot {
	if a.livePath == "" {
		return LiveSnapshot{}
	}
	snap, _, err := ReadLive(a.livePath)
	if err != nil {
		return LiveSnapshot{}
	}
	return snap
}

// projectionIndex maps the projection's runs by issue for phase overlays.
func projectionIndex(snap LiveSnapshot) map[string]LiveRun {
	if len(snap.Runs) == 0 {
		return nil
	}
	byIssue := make(map[string]LiveRun, len(snap.Runs))
	for _, run := range snap.Runs {
		byIssue[run.IssueID] = run
	}
	return byIssue
}

// gateRejectionPhase reports whether an operator phase marks a run the
// completion gate rejected outright.
func gateRejectionPhase(phase string) bool {
	return phase == "stopped: gate red" || phase == "stopped: gate escalation"
}

// maxHistoryEntries bounds the history merge; it matches the projection's
// own retained-history bound.
const maxHistoryEntries = 100

// Snapshot returns the full supervised state: live runs in registration
// order, the earliest armed wake as the supervisor wake time, and the
// cached needs-human section. Gate rejections come from the live
// projection's own records — the last gate output and the moment the
// projection recorded the stop — and carry a gate kind or timestamp only
// when those records actually have one.
func (a *StateSourceAdapter) Snapshot() httpstate.Snapshot {
	snap := httpstate.Snapshot{RunsAsOf: time.Now()}
	var wake time.Time
	live := a.readProjection()
	byIssue := projectionIndex(live)
	for _, run := range a.registry.List() {
		entry := httpstate.Run{
			Issue:     run.IssueID,
			Session:   run.Session,
			Attempt:   run.Attempt,
			State:     stateName(run.State),
			LastEvent: run.LastEventAt,
		}
		if projected, ok := byIssue[run.IssueID]; ok {
			entry.Phase = projected.Phase
		}
		snap.Runs = append(snap.Runs, entry)
		if run.WakeAt.IsZero() {
			continue
		}
		if wake.IsZero() || run.WakeAt.Before(wake) {
			wake = run.WakeAt
		}
	}
	snap.WakeAt = wake

	if a.reports != nil {
		rep, at := a.reports.Load()
		snap.NeedsHumanAsOf = at
		for _, nh := range rep.NeedsHuman {
			snap.NeedsHuman = append(snap.NeedsHuman, httpstate.NeedsHuman{
				Issue:  nh.Issue,
				Reason: nh.Reason,
				Detail: nh.LastGateOutput,
				Since:  nh.Since,
			})
		}
	}
	// Recent gate rejections derive from the projection's own records:
	// gate-stopped rows carry the last gate output and the row's last
	// update — the moment the rejection was observed. No gate kind exists
	// in those records, so none is served; times come only from records.
	// Their as_of is the projection read, never the tracker cache stamp.
	snap.GateRejectionsAsOf = snap.RunsAsOf
	for _, section := range [][]LiveRun{live.Runs, live.History} {
		for _, run := range section {
			if !gateRejectionPhase(run.Phase) {
				continue
			}
			snap.GateRejections = append(snap.GateRejections, httpstate.GateRejection{
				Issue:  run.IssueID,
				Reason: run.LastGate,
				At:     run.UpdatedAt,
			})
		}
	}
	sort.SliceStable(snap.GateRejections, func(i, j int) bool {
		return snap.GateRejections[i].At.Before(snap.GateRejections[j].At)
	})
	return snap
}

// Queue returns the cached readiness projection: the daemon's canonical
// tracker read of the configured label lanes, taken outside the request
// path and shared with the status projection.
func (a *StateSourceAdapter) Queue() httpstate.Queue {
	queue := httpstate.Queue{}
	if a.reports == nil {
		return queue
	}
	rep, at := a.reports.Load()
	queue.AsOf = at
	for _, row := range rep.Queue {
		qr := httpstate.QueueRow{
			ID:      row.ID,
			Title:   row.Title,
			Reason:  row.Reason,
			Blocked: row.Blocked,
		}
		for _, b := range row.Blockers {
			qr.Blockers = append(qr.Blockers, httpstate.QueueBlocker{ID: b.ID, Title: b.Title})
		}
		queue.Rows = append(queue.Rows, qr)
	}
	return queue
}

// History merges the live projection's terminal rows and restart-archived
// attempts with the registry's terminal rows, newest first, bounded at
// maxHistoryEntries. Restart-archived rows arrive with the operator phase
// "interrupted"; terminal live rows keep their stop phase.
func (a *StateSourceAdapter) History() httpstate.History {
	history := httpstate.History{AsOf: time.Now()}
	live := a.readProjection()
	seen := make(map[string]bool)
	add := func(issue, title, phase, state string, attempt int, outcome string, at time.Time) {
		key := fmt.Sprintf("%s/%d", issue, attempt)
		if seen[key] {
			return
		}
		seen[key] = true
		history.Runs = append(history.Runs, httpstate.HistoryRun{
			Issue:     issue,
			Title:     title,
			Phase:     phase,
			State:     state,
			Attempt:   attempt,
			Outcome:   outcome,
			UpdatedAt: at,
		})
	}
	// The projection's archived attempts: past attempts and everything the
	// previous daemon life left behind, restart-interrupted runs included.
	for _, run := range live.History {
		add(run.IssueID, run.Title, run.Phase, "", run.Attempt, "", run.UpdatedAt)
	}
	// The projection's terminal live rows, not yet archived out.
	for _, run := range live.Runs {
		if !strings.HasPrefix(run.Phase, "stopped:") {
			continue
		}
		add(run.IssueID, run.Title, run.Phase, "", run.Attempt, "", run.UpdatedAt)
	}
	// The registry's terminal rows: runs that finished without a
	// projection record (MCP-spawned runs before the loop picks them up).
	// Those records carry no operator phase, so none is served — the
	// coarse state and outcome text are what they actually hold.
	for _, run := range a.registry.List() {
		if runLive(run.State) {
			continue
		}
		add(run.IssueID, "", "", stateName(run.State), run.Attempt, run.Outcome, run.LastEventAt)
	}
	sort.SliceStable(history.Runs, func(i, j int) bool {
		return history.Runs[i].UpdatedAt.After(history.Runs[j].UpdatedAt)
	})
	if len(history.Runs) > maxHistoryEntries {
		history.Runs = history.Runs[:maxHistoryEntries]
	}
	return history
}

// Issue returns one issue's state from the live registry, enriched with the
// projection's operator phase, turn, workflow, last gate preview, and
// output tail; issues only known to the cached report report their tracker
// status. The second return is false when the issue is unknown to both.
func (a *StateSourceAdapter) Issue(id string) (httpstate.IssueState, bool) {
	if run, ok := a.registry.Lookup(id); ok {
		st := httpstate.IssueState{
			State:     stateName(run.State),
			Attempt:   run.Attempt,
			Session:   run.Session,
			LastEvent: run.LastEventAt,
			Summary:   run.Outcome,
			Thread:    run.Session,
			Workspace: run.Workspace,
			WakeAt:    run.WakeAt,
			Tail:      run.Tail,
		}
		// The projection carries the supervision vocabulary the registry
		// does not; the file read is the projection's own flush cadence.
		if projected, ok := projectionIndex(a.readProjection())[id]; ok {
			st.Phase = projected.Phase
			st.Turn = projected.Turn
			st.Workflow = projected.Workflow
			st.LastGate = projected.LastGate
			if projected.Thread != "" {
				st.Thread = projected.Thread
			}
			if st.Session == "" {
				st.Session = projected.Thread
			}
			if st.Tail == "" {
				st.Tail = projected.Tail
			}
			if st.WakeAt.IsZero() {
				st.WakeAt = projected.WakeAt
			}
		}
		return st, true
	}
	if a.reports != nil {
		rep, _ := a.reports.Load()
		for _, active := range rep.Active {
			if active.Issue == id {
				return httpstate.IssueState{
					State:     "running",
					Attempt:   active.Attempt,
					Session:   active.Session,
					LastEvent: time.Now().Add(-active.LastFrameAge),
					Summary:   "in progress (pre-tick view)",
				}, true
			}
		}
		for _, d := range rep.Deferred {
			if d.Issue == id {
				return httpstate.IssueState{
					State:     "deferred",
					Attempt:   0,
					LastEvent: time.Now(),
					Summary:   d.Reason,
				}, true
			}
		}
	}
	return httpstate.IssueState{}, false
}

// Refresh advises the daemon to re-evaluate promptly. It returns
// immediately; overlapping refreshes collapse into one in-flight poke.
func (a *StateSourceAdapter) Refresh() {
	if a.poke == nil || !a.inflight.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer a.inflight.Store(false)
		a.poke()
	}()
}

// StateAPIDescriptorSchemaVersion is the version of the descriptor
// document itself, independent of the HTTP wire schema it points at.
const StateAPIDescriptorSchemaVersion = 1

// StateAPIDescriptor is the per-instance discovery document an enabled
// daemon publishes under the state dir's live directory. Clients enumerate
// stateapi-*.json files and skip stale entries (pid/identity mismatch);
// they never delete descriptors — the owning daemon removes its file at
// shutdown, before releasing the instance lock. The document carries the
// bearer token, so it is written 0600 into the 0700 live directory.
type StateAPIDescriptor struct {
	// SchemaVersion is the version of this descriptor document.
	SchemaVersion int `json:"schema_version"`
	// Instance is the daemon instance name: the lock name without its
	// .lock suffix.
	Instance string `json:"instance"`
	// Addr is the bound host:port the state API answers on.
	Addr string `json:"addr"`
	// BearerToken authenticates every request to the state API. It lives
	// here and nowhere else: never in logs, the config summary, or error
	// strings.
	BearerToken string `json:"bearer_token"`
	// PID is the daemon's process id.
	PID int `json:"pid"`
	// Identity identifies this process incarnation (pid alone is
	// recyclable); empty when the platform cannot provide one.
	Identity string `json:"identity,omitempty"`
	// Version is the daemon build version.
	Version string `json:"version"`
	// ConfigPath is the configuration file the daemon loaded; empty when
	// it ran without one.
	ConfigPath string `json:"config_path,omitempty"`
	// StartedAt is when the daemon started, RFC 3339 in UTC.
	StartedAt string `json:"started_at"`
}

// StateAPIPath returns the per-instance descriptor path under stateDir.
// The file name derives from InstanceLockName, so disjoint configurations
// running under one shared state directory publish distinct files that
// clients can simply enumerate — never a merged document, and no client
// ever rebuilds a lock-name hash.
func StateAPIPath(stateDir string, cfg *config.Config) string {
	instance := strings.TrimSuffix(InstanceLockName(cfg), ".lock")
	return filepath.Join(stateDir, "live", "stateapi-"+instance+".json")
}

// WriteStateAPIDescriptor atomically publishes the descriptor at path,
// following the live-view write pattern: a 0600 temp file in the target
// directory, renamed into place.
func WriteStateAPIDescriptor(path string, d StateAPIDescriptor) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// The descriptor is the bearer token's delivery vehicle (KTD3): the
	// secret rides in a 0600 file inside the 0700 live directory, and
	// nowhere else.
	data, err := json.Marshal(d) //nolint:gosec // G117: intentional secret delivery via the 0600 descriptor file
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".stateapi-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// newStateAPIToken generates the per-start bearer token: 32 random bytes
// from crypto/rand, base64url-encoded without padding.
func newStateAPIToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// stateAPISurface holds the wired HTTP state surface for one daemon life:
// the bound server and the descriptor it published. A nil surface (and a
// nil-tolerant method set) means the surface never started.
type stateAPISurface struct {
	server     *httpstate.Server
	descriptor string
}

// shutdown stops advertising and then stops serving: the descriptor is
// removed first, so a client never resolves a listener that is already
// gone. Run releases the instance lock only after shutdown returns, which
// keeps the descriptor removal ahead of the lock release.
func (s *stateAPISurface) shutdown(ctx context.Context, log *slog.Logger) {
	if s == nil {
		return
	}
	if s.descriptor != "" {
		if err := os.Remove(s.descriptor); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Warn("stateapi descriptor removal failed", "path", s.descriptor, "error", err)
		}
	}
	if s.server != nil {
		if err := s.server.Shutdown(ctx); err != nil {
			log.Warn("stateapi shutdown incomplete", "error", err)
		}
	}
}
