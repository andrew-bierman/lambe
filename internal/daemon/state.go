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
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/andrew-bierman/lambe/internal/config"
	"github.com/andrew-bierman/lambe/internal/httpstate"
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
// report cache, and Refresh pokes the daemon for a prompt re-evaluation.
// It is safe for concurrent use.
type StateSourceAdapter struct {
	registry *RunRegistry
	reports  *ReportCache
	poke     func()
	inflight atomic.Bool
}

// Compile-time check that StateSourceAdapter satisfies the HTTP state seam.
var _ httpstate.StateSource = (*StateSourceAdapter)(nil)

// NewStateSource builds the adapter. poke, when non-nil, is the advisory
// refresh hook the daemon wires to an immediate background tick.
func NewStateSource(registry *RunRegistry, reports *ReportCache, poke func()) *StateSourceAdapter {
	return &StateSourceAdapter{registry: registry, reports: reports, poke: poke}
}

// Snapshot returns the full supervised state: live runs in registration
// order, the earliest armed wake as the supervisor wake time, and the
// cached needs-human and gate-rejection sections.
func (a *StateSourceAdapter) Snapshot() httpstate.Snapshot {
	snap := httpstate.Snapshot{}
	var wake time.Time
	for _, run := range a.registry.List() {
		snap.Runs = append(snap.Runs, httpstate.Run{
			Issue:     run.IssueID,
			Session:   run.Session,
			Attempt:   run.Attempt,
			State:     stateName(run.State),
			LastEvent: run.LastEventAt,
		})
		if run.WakeAt.IsZero() {
			continue
		}
		if wake.IsZero() || run.WakeAt.Before(wake) {
			wake = run.WakeAt
		}
	}
	snap.WakeAt = wake

	if a.reports != nil {
		rep, _ := a.reports.Load()
		for _, nh := range rep.NeedsHuman {
			snap.NeedsHuman = append(snap.NeedsHuman, httpstate.NeedsHuman{
				Issue:  nh.Issue,
				Reason: "gate_rejected",
				Detail: nh.LastGateOutput,
			})
		}
		for _, gr := range rep.GateRejections {
			snap.GateRejections = append(snap.GateRejections, httpstate.GateRejection{
				Issue:  gr.Issue,
				Gate:   "formula",
				Reason: gr.Output,
			})
		}
	}
	return snap
}

// Issue returns one issue's state from the live registry; issues only known
// to the cached report report their tracker status. The second return is
// false when the issue is unknown to both.
func (a *StateSourceAdapter) Issue(id string) (httpstate.IssueState, bool) {
	if run, ok := a.registry.Lookup(id); ok {
		return httpstate.IssueState{
			State:     stateName(run.State),
			Attempt:   run.Attempt,
			Session:   run.Session,
			LastEvent: run.LastEventAt,
			Summary:   run.Outcome,
		}, true
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
