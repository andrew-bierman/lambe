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
	primary tracker.Tracker
	loop    Ticker
	flusher Flusher
	poking  atomic.Bool
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
		Runner:   placeholderRunner{},
		Gates:    placeholderGates{},
		Sync:     NopSyncer{},
		Children: NewChildSet(),
		Registry: NewRunRegistry(nil, log),
		Reports:  &ReportCache{},
	}
	asm.Supervisor = asm.Children
	if opts.Wire != nil {
		if err := opts.Wire(&asm); err != nil {
			return nil, fmt.Errorf("wire assembly: %w", err)
		}
	}
	if asm.Sync == nil {
		asm.Sync = NopSyncer{}
	}
	if asm.Children == nil {
		asm.Children = NewChildSet()
	}
	if asm.Supervisor == nil {
		asm.Supervisor = asm.Children
	}
	if asm.Registry == nil {
		asm.Registry = NewRunRegistry(nil, log)
	}
	if asm.Reports == nil {
		asm.Reports = &ReportCache{}
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

	return &Daemon{opts: opts, asm: asm, log: log}, nil
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
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	d.cancel = cancel

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer signal.Stop(sig)

	lock, err := AcquireStateLock(filepath.Join(d.opts.StateDir, "lambe.lock"))
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
	if f, ok := primary.(Flusher); ok {
		d.flusher = f
	}

	adopted, err := Recover(runCtx, TrackerOrphans{Tracker: primary}, primary, d.opts.Actor,
		ShutdownBound, d.log)
	if err != nil {
		// Recovery read failure degrades: log and keep starting, never
		// kill on unverified ownership.
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
		spaces := symphony.NewWorkspaceManager(filepath.Join(d.opts.StateDir, "workspaces"), d.log)
		loop = symphony.NewLoop(filtered, loader, d.asm.Runner, d.asm.Gates, spaces, d.log)
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
		RunTicks(runCtx, TickOptions{
			Interval:  time.Duration(d.opts.Config.Supervisor.TickSeconds) * time.Second,
			Sync:      d.asm.Sync,
			Loop:      loop,
			Flush:     d.flusher,
			AfterTick: d.afterTick,
			Log:       d.log,
		})
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
	if d.cancel != nil {
		d.cancel()
	}
	select {
	case <-ticksDone:
	case <-time.After(2 * time.Second):
		d.log.Warn("tick goroutine did not stop within grace")
	}
	ctx, cancel := context.WithTimeout(context.Background(), ShutdownBound)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- d.asm.Supervisor.TerminateAll(ctx) }()
	select {
	case err := <-done:
		if err != nil {
			d.log.Error("shutdown ladder reported errors", "error", err)
		}
	case <-ctx.Done():
		d.log.Error("shutdown bound expired before all children exited")
	}
	d.log.Info("lambe daemon stopped")
	return nil
}

// afterTick refreshes the report cache after every tick so the HTTP state
// surface serves tracker-derived sections without per-request tracker reads.
func (d *Daemon) afterTick(ctx context.Context) {
	if d.primary == nil {
		return
	}
	rep, err := BuildReport(ctx, d.primary)
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
		ctx, cancel := context.WithTimeout(context.Background(), ShutdownBound)
		defer cancel()
		if err := d.asm.Sync.Pull(ctx); err != nil {
			d.log.Warn("poke: hub pull failed", "category", ErrorCategory(err), "error", err)
		}
		if d.loop != nil {
			if err := d.loop.Tick(ctx); err != nil {
				d.log.Warn("poke: tick failed", "category", ErrorCategory(err), "error", err)
			}
		}
		if d.flusher != nil {
			if err := d.flusher.FlushWrites(ctx); err != nil {
				d.log.Warn("poke: write batch flush failed", "category", ErrorCategory(err), "error", err)
			}
		}
		if err := PushAfterWrites(ctx, d.asm.Sync); err != nil {
			d.log.Warn("poke: hub push failed", "category", ErrorCategory(err), "error", err)
		}
		d.afterTick(ctx)
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
