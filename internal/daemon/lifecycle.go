package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/andrew-bierman/lambe/internal/config"
	"github.com/andrew-bierman/lambe/internal/httpstate"
	"github.com/andrew-bierman/lambe/internal/mcp"
	"github.com/andrew-bierman/lambe/internal/surface/rex"
	"github.com/andrew-bierman/lambe/internal/symphony"
	"github.com/andrew-bierman/lambe/internal/tracker"
)

// ShutdownBound is the wall-clock bound for the shutdown ladder: SIGTERM to
// every child group, at most this long of grace, then SIGKILL, then exit.
const ShutdownBound = 30 * time.Second

// Options configures one daemon life.
type Options struct {
	// Config is the resolved daemon configuration.
	Config *config.Config
	// StateDir holds the lock file and workspaces; it is created as needed.
	StateDir string
	// LockName is the single-instance lock file name inside StateDir.
	// Empty defaults to "lambe.lock"; InstanceLockName derives a
	// per-tracker name so disjoint configs can run concurrent instances.
	LockName string
	// Machine is this machine's configured identity for the label-skip rule.
	Machine string
	// Actor is the daemon's tracker identity for claims and heartbeats.
	Actor string
	// BdBin is the bd binary path; empty means PATH lookup.
	BdBin string
	// WorkflowPath is the WORKFLOW.md the symphony loop runs; empty skips
	// loop construction and keeps only the tick cadence.
	WorkflowPath string
	// ConfigPath is the file configuration was loaded from; a SIGHUP
	// re-reads it and an invalid reload is logged without effect.
	ConfigPath string
	// Log receives lifecycle events; nil uses the default logger.
	Log *slog.Logger
	// Wire is the single assembly point for units not owned by this
	// package. It runs before trackers open and may fill any Assembly
	// field; whatever it leaves unset gets the local default.
	Wire func(*Assembly) error
}

// Assembly carries the seams this package consumes but other units own: the
// agent runner and gate evaluator (harness and gate packages), the label
// source, the hub syncer, and the child supervisor. The orchestrator
// finalizes these in Options.Wire after those units land.
type Assembly struct {
	// Trackers, when set, replaces the trackers opened from configuration
	// (used by tests and by wired-in tracker sources).
	Trackers []tracker.Tracker
	// Runner executes agent turns; defaults to a placeholder that fails
	// every turn until the harness driver is wired.
	Runner symphony.AgentRunner
	// Gates judge settled turns; defaults to a placeholder that errors
	// every evaluation until the gate runner is wired.
	Gates symphony.GateEvaluator
	// Labels resolves issue labels; defaults to MetadataLabelSource.
	Labels LabelSource
	// Sync synchronizes the hub; defaults to NopSyncer.
	Sync Syncer
	// Supervisor terminates live children at shutdown; defaults to an
	// empty ChildSet.
	Supervisor *ChildSet
	// Children is the registry live runners add spawned children to so
	// shutdown reaches their process groups.
	Children *ChildSet
	// Registry is the live run state the Control and StateSource adapters
	// read; defaults to an empty registry.
	Registry *RunRegistry
	// Reports caches the morning report between ticks for the read-only
	// HTTP state surface; defaults to an empty cache.
	Reports *ReportCache
	// Surface is the Rex adapter run lifecycle events feed; when nil the
	// daemon detects Rex itself and degrades to a no-op when absent.
	Surface *rex.Adapter
}

// placeholderRunner is the pre-wiring stand-in for the harness driver: it
// fails every turn so the loop retries nothing destructively.
type placeholderRunner struct{}

// RunTurn implements symphony.AgentRunner.
func (placeholderRunner) RunTurn(_ context.Context, _ symphony.TurnRequest) (symphony.TurnResult, error) {
	return symphony.TurnResult{Exit: symphony.TurnFailure}, errors.New("agent harness not wired")
}

// placeholderGates is the pre-wiring stand-in for the gate runner: every
// evaluation errors, which the loop treats as a gate retry.
type placeholderGates struct{}

// Evaluate implements symphony.GateEvaluator.
func (placeholderGates) Evaluate(_ context.Context, _ symphony.GateRequest) (symphony.GateOutcome, error) {
	return symphony.GateOutcome{Verdict: symphony.GateErr}, errors.New("gate runner not wired")
}

// Daemon is the assembled supervisor: lock, trackers, recovery, heartbeat,
// tick loop, and signal handling for one daemon life.
type Daemon struct {
	opts    Options
	asm     Assembly
	log     *slog.Logger
	lock    *StateLock
	cancel  context.CancelFunc
	runCtx  context.Context
	primary tracker.Tracker
	loop    Ticker
	flusher Flusher
	poking  atomic.Bool
	// cycleMu serializes cycles: the Symphony loop requires one tick at a
	// time, so the poke path must never interleave with (or run beside) a
	// tick cycle.
	cycleMu sync.Mutex
	// tickCount counts completed non-immediate cycles and drives the
	// report-cache refresh cadence.
	tickCount atomic.Int64
}

// Registry returns the daemon's live run registry. Wired runners record
// their runs' lifecycle transitions here; the Control and StateSource
// adapters read it.
func (d *Daemon) Registry() *RunRegistry { return d.asm.Registry }

// Control returns the mcp.Control adapter over the daemon's live run state,
// for the MCP server surface to proxy tool calls into.
func (d *Daemon) Control() mcp.Control {
	return &ControlAdapter{Registry: d.asm.Registry, Tracker: d.primary, Log: d.log}
}

// OpenControl assembles the control stack without a daemon life: it opens
// the configured trackers, takes no state lock, and starts no tick loop or
// heartbeats. It is the constructor behind `lambe mcp` — an MCP server
// process proxies tool calls into the returned control, whose spawns
// register runs in the registry the running daemon's next tick dispatches.
// The returned release func drops the assembled stack; trackers are
// shell-transport locals that need no explicit close today.
func OpenControl(ctx context.Context, opts Options) (mcp.Control, func(), error) {
	opts.WorkflowPath = "" // no loop, no workflow
	d, err := New(opts)
	if err != nil {
		return nil, nil, err
	}
	trackers, err := d.trackers(ctx)
	if err != nil {
		return nil, nil, err
	}
	control := &ControlAdapter{Registry: d.asm.Registry, Tracker: trackers[0], Log: d.log}
	return control, d.cancel, nil
}

// StateSource returns the httpstate.StateSource adapter over the daemon's
// live run state and cached report, for the read-only HTTP state surface.
func (d *Daemon) StateSource() httpstate.StateSource {
	return NewStateSource(d.asm.Registry, d.asm.Reports, d.poke)
}

// New assembles the daemon: it runs the Wire hook, applies defaults to
// every unset seam, and validates the resolved options. It does not touch
// the lock or any tracker; that ordering belongs to Run.
func New(opts Options) (*Daemon, error) {
	if opts.Config == nil {
		return nil, errors.New("daemon: config is required")
	}
	if opts.StateDir == "" {
		return nil, errors.New("daemon: state dir is required")
	}
	if opts.Machine == "" {
		return nil, errors.New("daemon: machine identity is required")
	}
	if opts.Actor == "" {
		opts.Actor = "lambe@" + opts.Machine
	}
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}

	asm := Assembly{
		Runner: placeholderRunner{},
		Gates:  placeholderGates{},
	}
	if opts.Wire != nil {
		if err := opts.Wire(&asm); err != nil {
			return nil, fmt.Errorf("wire assembly: %w", err)
		}
	}
	// The single defaults site: whatever Wire left unset gets the local
	// default, with the Supervisor = Children alias applied last so a Wire
	// that fills only one of them still yields a consistent pair.
	if asm.Sync == nil {
		asm.Sync = NopSyncer{}
	}
	if asm.Children == nil {
		asm.Children = NewChildSet()
	}
	if asm.Registry == nil {
		asm.Registry = NewRunRegistry(nil, log)
	}
	if asm.Reports == nil {
		asm.Reports = &ReportCache{}
	}
	if asm.Supervisor == nil {
		asm.Supervisor = asm.Children
	}
	if asm.Surface == nil {
		av := rex.Detector{Env: os.Getenv, Runner: execCommand}.Detect(context.Background())
		if av.Available {
			asm.Surface = rex.NewAdapter(av, rex.NewShellClient(execCommand), os.Stdout)
		}
	}
	if asm.Surface != nil {
		asm.Registry.SetSurface(asm.Surface, log)
	}

	runCtx, cancel := context.WithCancel(context.Background())
	return &Daemon{opts: opts, asm: asm, log: log, runCtx: runCtx, cancel: cancel}, nil
}

// Trackers opens the configured trackers as shell-transport locals over bd,
// unless the assembly supplied its own. The first tracker is primary: it
// backs recovery, heartbeats, dispatch, and status.
func (d *Daemon) trackers(ctx context.Context) ([]tracker.Tracker, error) {
	if len(d.asm.Trackers) > 0 {
		return d.asm.Trackers, nil
	}
	var trackers []tracker.Tracker
	for _, t := range d.opts.Config.Trackers {
		local, err := tracker.NewLocal(d.opts.BdBin, t.Path, d.opts.Actor)
		if err != nil {
			return nil, fmt.Errorf("open tracker %s: %w", t.Name, err)
		}
		if err := local.Init(ctx); err != nil {
			return nil, fmt.Errorf("init tracker %s: %w", t.Name, err)
		}
		trackers = append(trackers, local)
	}
	if len(trackers) == 0 {
		return nil, errors.New("no trackers configured")
	}
	return trackers, nil
}

// Run executes one daemon life in the mandated order — acquire lock, open
// trackers, recovery, tick loop — until SIGTERM or SIGINT arrives. On a
// terminating signal it forwards the bounded kill ladder to every live
// child, releases the lock, and returns. A held lock returns an error
// wrapping ErrLockHeld immediately, with the holder unaffected.
func (d *Daemon) Run(ctx context.Context) error {
	runCtx := d.runCtx
	defer d.cancel()
	stopCancel := context.AfterFunc(ctx, d.cancel)
	defer stopCancel()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer signal.Stop(sig)

	lockName := d.opts.LockName
	if lockName == "" {
		lockName = "lambe.lock"
	}
	lock, err := AcquireStateLock(filepath.Join(d.opts.StateDir, lockName))
	if err != nil {
		return err
	}
	d.lock = lock
	defer func() { _ = lock.Release() }()
	d.log.Info("state lock acquired", "path", lock.path, "pid_hint", lock.PidHint())

	trackers, err := d.trackers(runCtx)
	if err != nil {
		return err
	}
	primary := trackers[0]
	d.primary = primary
	primaryDir := ""
	if len(d.opts.Config.Trackers) > 0 {
		primaryDir = d.opts.Config.Trackers[0].Path
	}
	if f, ok := primary.(Flusher); ok {
		d.flusher = f
	}

	adopted, err := Recover(runCtx, TrackerOrphans{Tracker: primary}, primary, d.opts.Actor,
		ShutdownBound, d.log)
	if err != nil {
		d.log.Error("startup recovery incomplete, continuing without adoption",
			"category", ErrorCategory(err), "error", err)
	}
	for _, a := range adopted {
		d.log.Info("session adopted at startup", "issue", a.SessionID, "pgid", a.PGID)
	}

	var loop Ticker
	if d.opts.WorkflowPath != "" {
		loader, err := symphony.NewLoader(d.opts.WorkflowPath, symphony.OSEnv, d.log)
		if err != nil {
			return fmt.Errorf("load workflow: %w", err)
		}
		filtered := &FilteredTracker{
			Tracker: primary,
			Machine: d.opts.Machine,
			Labels:  d.asm.Labels,
			Log:     d.log,
		}
		if filtered.Labels == nil {
			filtered.Labels = MetadataLabelSource{Tracker: primary}
		}
		workspaceRoot := filepath.Join(d.opts.StateDir, "workspaces")
		if d.opts.Config != nil && d.opts.Config.WorkspaceRoot != "" {
			workspaceRoot = d.opts.Config.WorkspaceRoot
		}
		spaces := symphony.NewWorkspaceManager(workspaceRoot, d.log)
		l := symphony.NewLoop(filtered, loader, d.asm.Runner, d.asm.Gates, spaces, d.log)
		if d.opts.Config != nil {
			l.DispatchLabels = d.opts.Config.Labels
		}
		view := newLiveView(LivePath(d.opts.StateDir, d.opts.Config), func(err error) { d.log.Warn("live view update failed", "error", err) })
		view.registry = d.asm.Registry
		view.start()
		defer view.close()
		l.Observe = view.progress
		if runner, ok := d.asm.Runner.(*harnessRunner); ok {
			runner.live = view
		}
		l.ControlTimeout = d.cycleBound()
		l.CheckpointDir = filepath.Join(d.opts.StateDir, "sessions", strings.TrimSuffix(lockName, ".lock"))
		if err := l.Restore(runCtx, d.opts.Actor); err != nil {
			if errors.Is(err, symphony.ErrWorkspaceRootChanged) {
				return err
			}
			d.log.Error("session recovery incomplete; healthy sessions continue", "error", err)
		}
		// Load responsibility formulas from the tracker's conventional
		// .beads/formulas directory and wire the bd materializer so
		// triggers materialize finite issues into the dispatch front.
		formulaDir := filepath.Join(primaryDir, ".beads", "formulas")
		formulas, err := symphony.LoadFormulas(formulaDir)
		if err != nil {
			d.log.Warn("formula loading failed; running without responsibilities", "dir", formulaDir, "error", err)
		} else {
			l.Formulas = formulas
			bdBin := d.opts.BdBin
			if bdBin == "" {
				if resolved, err := tracker.ResolveBdBin(""); err == nil {
					bdBin = resolved
				}
			}
			l.Materializer = &symphony.BeadsMaterializer{
				Bin: bdBin,
				Dir: primaryDir,
				Log: d.log,
			}
			if n := len(formulas); n > 0 {
				d.log.Info("responsibilities loaded", "count", n, "dir", formulaDir)
			}
		}
		loop = l
		d.loop = loop
		go func() {
			if err := loader.Watch(runCtx); err != nil {
				d.log.Warn("workflow watcher stopped", "error", err)
			}
		}()
	}

	go HeartbeatLoop(runCtx, primary, d.opts.Actor,
		func() time.Duration { return d.opts.Config.Supervisor.HeartbeatInterval }, d.log)

	d.log.Info("lambe daemon running",
		"machine", d.opts.Machine, "actor", d.opts.Actor,
		"tick_seconds", d.opts.Config.Supervisor.TickSeconds,
		"trackers", len(trackers))

	ticksDone := make(chan struct{})
	go func() {
		defer close(ticksDone)
		RunTicks(runCtx, time.Duration(d.opts.Config.Supervisor.TickSeconds)*time.Second,
			d.tickCycle)
	}()

	for {
		select {
		case <-ctx.Done():
			return d.shutdown(ticksDone)
		case s, ok := <-sig:
			if !ok {
				continue
			}
			switch s {
			case syscall.SIGHUP:
				d.reload()
			default:
				return d.shutdown(ticksDone)
			}
		}
	}
}

// reload applies a SIGHUP: the configuration file is re-read and, when
// valid, replaces the running configuration from the next heartbeat and
// tick read. An invalid reload is logged and the running configuration
// stays in force.
func (d *Daemon) reload() {
	d.log.Info("reload requested")
	if d.opts.ConfigPath == "" {
		d.log.Warn("reload skipped: no config file path known")
		return
	}
	cfg, err := config.Load(config.Options{File: d.opts.ConfigPath})
	if err != nil {
		d.log.Error("config reload rejected, keeping running config",
			"category", CategoryUnknown, "error", err)
		return
	}
	d.opts.Config = cfg
	d.log.Info("config reloaded", "config", cfg.Summary())
}

// shutdown forwards the bounded ladder to every live child and returns once
// every group is gone or the bound expires; the deferred Release returns
// the lock. ticksDone is joined first so no tick is mid-write while the
// ladder runs.
func (d *Daemon) shutdown(ticksDone <-chan struct{}) error {
	d.log.Info("shutdown: forwarding kill ladder to children", "bound", ShutdownBound.String())
	ctx, cancel := context.WithTimeout(context.Background(), ShutdownBound)
	defer cancel()
	if d.cancel != nil {
		d.cancel()
	}
	terminated := make(chan error, 1)
	go func() { terminated <- d.asm.Supervisor.TerminateAll(ctx) }()
	select {
	case <-ticksDone:
	case <-ctx.Done():
		d.log.Warn("tick shutdown bound expired")
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	locked := false
	for ctx.Err() == nil && !locked {
		locked = d.cycleMu.TryLock()
		if !locked {
			select {
			case <-ticker.C:
			case <-ctx.Done():
			}
		}
	}
	if locked {
		if closer, ok := d.loop.(interface{ Close(context.Context) error }); ok {
			if err := closer.Close(ctx); err != nil {
				d.log.Error("worker shutdown incomplete", "error", err)
			}
		}
		if ctx.Err() == nil {
			d.refreshReport(ctx)
		}
		d.cycleMu.Unlock()
	} else {
		d.log.Error("cycle shutdown bound expired")
	}
	select {
	case err := <-terminated:
		if err != nil {
			d.log.Error("shutdown ladder reported errors", "error", err)
		}
	case <-ctx.Done():
		d.log.Error("shutdown bound expired before all children exited")
	}
	d.log.Info("lambe daemon stopped")
	return nil
}

// reportRefreshTicks is the report-cache refresh cadence, in ticks: with
// the default 10s tick the cache refreshes about once a minute. Pokes and
// shutdown always refresh immediately through the cycle's immediate path.
const reportRefreshTicks = 6

// tickCycle is the tick loop's body: one non-immediate cycle. The report
// refresh cadence is applied inside the cycle from the tick counter.
func (d *Daemon) tickCycle(ctx context.Context) {
	d.cycle(ctx, "tick", false)
}

// cycle runs one pull → tick → flush → push → report sequence; it is the
// shared body of the tick loop and the poke path, with prefix ("tick" or
// "poke") labeling its log lines. immediate marks the poke path: its push
// and report refresh run unconditionally, while the tick path skips the
// hub push when the tick queued no writes and refreshes the report cache
// only on the reportRefreshTicks cadence. Cycles are serialized (the loop
// requires non-interleaved ticks). Hub I/O is bounded separately from agent
// execution, whose runner owns the turn and silence deadlines. Every failure
// is logged with its category and the daemon keeps running.
func (d *Daemon) cycle(ctx context.Context, prefix string, immediate bool) {
	d.cycleMu.Lock()
	defer d.cycleMu.Unlock()
	cctx, cancel := context.WithTimeout(ctx, d.cycleBound())
	if err := d.asm.Sync.Pull(cctx); err != nil {
		d.log.Warn(prefix+": hub pull failed, continuing on local state",
			"category", ErrorCategory(err), "error", err)
	}
	cancel()
	if d.loop != nil {
		if err := d.loop.Tick(ctx); err != nil {
			d.log.Warn(prefix+": tick failed, continuing",
				"category", ErrorCategory(err), "error", err)
		}
	}
	wctx, wcancel := context.WithTimeout(ctx, d.cycleBound())
	defer wcancel()
	ctx = wctx
	hasWrites := immediate
	if !immediate {
		if c, ok := d.flusher.(WriteCounter); !ok || c.BufferedWrites() > 0 {
			hasWrites = true
		}
	}
	if hasWrites && d.flusher != nil {
		if err := d.flusher.FlushWrites(ctx); err != nil {
			d.log.Warn(prefix+": write batch flush failed, writes stay queued",
				"category", ErrorCategory(err), "error", err)
		}
	}
	if hasWrites {
		if err := PushAfterWrites(ctx, d.asm.Sync); err != nil {
			d.log.Warn(prefix+": hub push failed, local writes stay ahead",
				"category", ErrorCategory(err), "error", err)
		}
	}
	if immediate || d.tickCount.Add(1)%reportRefreshTicks == 1 {
		d.refreshReport(ctx)
	}
}

// cycleBound bounds control operations, not agent execution: half the
// silence timeout, never below a minute. A daemon without resolved config
// uses the minute floor.
func (d *Daemon) cycleBound() time.Duration { return controlTimeout(d.opts.Config) }

func controlTimeout(cfg *config.Config) time.Duration {
	bound := time.Duration(0)
	if cfg != nil {
		bound = cfg.Supervisor.SilenceTimeout / 2
	}
	if bound < time.Minute {
		bound = time.Minute
	}
	return bound
}

// refreshReport rebuilds the morning report from the primary tracker into
// the report cache so the HTTP state surface serves tracker-derived
// sections without per-request tracker reads.
func (d *Daemon) refreshReport(ctx context.Context) {
	if d.primary == nil {
		return
	}
	var labels []string
	if d.opts.Config != nil {
		labels = d.opts.Config.Labels
	}
	rep, err := BuildReport(ctx, d.primary, labels)
	if err != nil {
		d.log.Warn("report refresh failed", "category", ErrorCategory(err), "error", err)
		return
	}
	d.asm.Reports.Store(rep)
}

// poke is the advisory refresh behind the HTTP state surface: it runs one
// immediate pull → tick → flush → push → report-refresh cycle in the
// background. Overlapping pokes collapse; a poke never blocks its caller.
func (d *Daemon) poke() {
	if !d.poking.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer d.poking.Store(false)
		d.cycle(d.runCtx, "poke", true)
	}()
}

// execCommand runs one command and returns its stdout, exit code, and
// error. It is the CommandRunner the daemon injects into the Rex detector
// and shell client.
func execCommand(ctx context.Context, name string, args ...string) (string, int, error) {
	cmd := exec.CommandContext(ctx, name, args...) // #nosec G204 -- fixed argv against a named CLI
	out, err := cmd.Output()
	if err == nil {
		return string(out), 0, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return string(out), ee.ExitCode(), err
	}
	return "", -1, err
}

// DefaultStateDir resolves the daemon state directory: LAMBE_STATE_DIR
// when set, otherwise ~/.local/state/lambe.
func DefaultStateDir() string {
	if dir := os.Getenv("LAMBE_STATE_DIR"); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "lambe-state"
	}
	return filepath.Join(home, ".local", "state", "lambe")
}

// DefaultMachine resolves this machine's dispatch identity: LAMBE_MACHINE
// when set, otherwise the hostname's first label.
func DefaultMachine() string {
	if m := os.Getenv("LAMBE_MACHINE"); m != "" {
		return m
	}
	host, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	name, _, _ := strings.Cut(host, ".")
	return name
}
