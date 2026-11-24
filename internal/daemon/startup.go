package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/andrew-bierman/lambe/internal/config"
	"github.com/andrew-bierman/lambe/internal/gate"
	"github.com/andrew-bierman/lambe/internal/symphony"
	"github.com/andrew-bierman/lambe/internal/tracker"
)

// loopBuilder assembles the Symphony loop for one daemon life. Each method is
// one startup step; startLoop runs them in the mandated order. It only wires
// the loop's exported seams — the loop itself is untouched.
type loopBuilder struct {
	d          *Daemon
	runCtx     context.Context
	primary    tracker.Tracker
	primaryDir string
	view       *liveView
	lockName   string
	loader     *symphony.Loader
	loop       *symphony.Loop
}

// startLoop builds the Symphony loop from the workflow file and records it as
// the daemon's ticker. It returns the loop's orphan registry for the
// heartbeat goroutine.
func (d *Daemon) startLoop(runCtx context.Context, primary tracker.Tracker, view *liveView, lockName string) (*symphony.OrphanRegistry, error) {
	b := &loopBuilder{d: d, runCtx: runCtx, primary: primary, view: view, lockName: lockName}
	if len(d.config().Trackers) > 0 {
		b.primaryDir = d.config().Trackers[0].Path
	}
	var err error
	if b.loader, err = symphony.NewLoader(d.opts.WorkflowPath, symphony.OSEnv, d.log); err != nil {
		return nil, fmt.Errorf("load workflow: %w", err)
	}
	b.newLoop()
	if cfg := d.config(); cfg != nil {
		b.loop.DispatchLabels = cfg.Labels
		if err := b.wireRoutes(cfg.Workflows); err != nil {
			return nil, err
		}
	}
	b.wireObservers()
	b.applyTimeouts()
	orphans := b.loop.Orphans
	if err := b.restore(); err != nil {
		return nil, err
	}
	b.loadFormulas()
	d.loop = b.loop
	go func() {
		if err := b.loader.Watch(runCtx); err != nil {
			d.log.Warn("workflow watcher stopped", "error", err)
		}
	}()
	return orphans, nil
}

// newLoop constructs the loop over the machine-filtered tracker and the
// workspace manager.
func (b *loopBuilder) newLoop() {
	d := b.d
	filtered := &FilteredTracker{
		Tracker: b.primary,
		Machine: d.opts.Machine,
		Labels:  d.asm.Labels,
		Log:     d.log,
	}
	if filtered.Labels == nil {
		filtered.Labels = MetadataLabelSource{Tracker: b.primary}
	}
	workspaceRoot := filepath.Join(d.opts.StateDir, "workspaces")
	if d.config() != nil && d.config().WorkspaceRoot != "" {
		workspaceRoot = d.config().WorkspaceRoot
	}
	spaces := symphony.NewWorkspaceManager(workspaceRoot, d.log)
	b.loop = symphony.NewLoop(filtered, b.loader, d.asm.Runner, d.asm.Gates, spaces, d.log)
}

// wireRoutes loads each named workflow, installs the gate-spec hook, and
// starts watchers for route workflows distinct from the main one.
func (b *loopBuilder) wireRoutes(routes []config.NamedWorkflow) error {
	if len(routes) == 0 {
		return nil
	}
	d, l := b.d, b.loop
	l.WorkflowRoutes = make(map[string]*symphony.Loader, len(routes))
	for _, route := range routes {
		routeLoader := b.loader
		if route.Path != d.opts.WorkflowPath {
			var err error
			routeLoader, err = symphony.NewLoader(route.Path, symphony.OSEnv, d.log)
			if err != nil {
				return fmt.Errorf("load workflow %s: %w", route.Name, err)
			}
		}
		l.WorkflowRoutes[route.Label] = routeLoader
	}
	l.PrepareWorkflow = b.prepareWorkflow(routes)
	for label, routeLoader := range l.WorkflowRoutes {
		if routeLoader == b.loader {
			continue
		}
		go func() {
			if err := routeLoader.Watch(b.runCtx); err != nil {
				d.log.Warn("workflow watcher stopped", "label", label, "error", err)
			}
		}()
	}
	return nil
}

// prepareWorkflow returns the loop's PrepareWorkflow hook: it names the
// workflow after its route and attaches the completion-gate spec loaded from
// the route's gate directory (default: the tracker's formula directory).
func (b *loopBuilder) prepareWorkflow(routes []config.NamedWorkflow) func(string, symphony.Workflow) (symphony.Workflow, error) {
	return func(label string, wf symphony.Workflow) (symphony.Workflow, error) {
		dir := formulaDir(b.primaryDir)
		for _, route := range routes {
			if route.Label == label {
				wf.Name = route.Name
				if route.GateDir != "" {
					dir = route.GateDir
				}
				break
			}
		}
		specs, err := gate.LoadDir(dir)
		if err != nil {
			return wf, err
		}
		if len(specs) == 0 {
			return wf, fmt.Errorf("no completion gates in %s", dir)
		}
		wf.GateSpec, err = json.Marshal(specs)
		return wf, err
	}
}

// wireObservers connects the loop to the live projection and the gate
// rejection log.
func (b *loopBuilder) wireObservers() {
	d, l := b.d, b.loop
	l.Observe = func(p symphony.Progress) {
		b.view.progress(p)
		d.asm.Registry.ObservePhase(p.IssueID, p.Phase)
	}
	rejections := NewGateRejectionLog(GateRejectionLogPath(d.opts.StateDir, d.config()))
	l.OnGateRed = func(issue string, attempt int, output string) {
		if err := rejections.Record(issue, attempt, output, time.Now()); err != nil {
			d.log.Warn("gate rejection record failed", "issue", issue, "error", err)
		}
	}
	if runner, ok := d.asm.Runner.(*harnessRunner); ok {
		runner.live = b.view
	}
}

// applyTimeouts sets the control and recovery bounds from the supervisor
// configuration.
func (b *loopBuilder) applyTimeouts() {
	d, l := b.d, b.loop
	l.ControlTimeout = d.cycleBound()
	l.OrphanTTL = d.config().Supervisor.OrphanClaimTTL
	// A raised bd timeout must not be cut short by the recovery time-box.
	if bd := d.config().Supervisor.BdTimeout; bd > 10*time.Second {
		l.RecoveryInspect = bd
		l.RecoveryBudget = 2 * bd
	}
}

// restore replays saved session checkpoints. A changed workspace root is
// fatal; any other failure is logged and healthy sessions continue.
func (b *loopBuilder) restore() error {
	d, l := b.d, b.loop
	l.CheckpointDir = filepath.Join(d.opts.StateDir, "sessions", strings.TrimSuffix(b.lockName, ".lock"))
	if err := l.Restore(b.runCtx, d.opts.Actor); err != nil {
		if errors.Is(err, symphony.ErrWorkspaceRootChanged) {
			return err
		}
		d.log.Error("session recovery incomplete; healthy sessions continue", "error", err)
	}
	return nil
}

// loadFormulas loads responsibility formulas from the tracker's conventional
// .beads/formulas directory and wires the bd materializer so triggers
// materialize finite issues into the dispatch front.
func (b *loopBuilder) loadFormulas() {
	d, l := b.d, b.loop
	dir := filepath.Join(b.primaryDir, ".beads", "formulas")
	formulas, err := symphony.LoadFormulas(dir)
	if err != nil {
		d.log.Warn("formula loading failed; running without responsibilities", "dir", dir, "error", err)
		return
	}
	l.Formulas = formulas
	bdBin := d.opts.BdBin
	if bdBin == "" {
		if resolved, err := tracker.ResolveBdBin(""); err == nil {
			bdBin = resolved
		}
	}
	l.Materializer = &symphony.BeadsMaterializer{
		Bin: bdBin,
		Dir: b.primaryDir,
		Log: d.log,
	}
	if n := len(formulas); n > 0 {
		d.log.Info("responsibilities loaded", "count", n, "dir", dir)
	}
}
