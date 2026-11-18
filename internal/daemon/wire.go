package daemon

// This file is the wiring for units this package consumes but does not own:
// the harness driver (Pi by default, ACP by configuration) adapted to the
// symphony AgentRunner seam, and the verified-completion gate runner.
// DefaultWire is the Options.Wire implementation the lambe binary installs.
import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/andrew-bierman/lambe/internal/config"
	"github.com/andrew-bierman/lambe/internal/gate"
	"github.com/andrew-bierman/lambe/internal/harness"
	acpdrv "github.com/andrew-bierman/lambe/internal/harness/acp"
	pidrv "github.com/andrew-bierman/lambe/internal/harness/pi"
	"github.com/andrew-bierman/lambe/internal/logging"
	"github.com/andrew-bierman/lambe/internal/mcp"
	"github.com/andrew-bierman/lambe/internal/proc"
	"github.com/andrew-bierman/lambe/internal/symphony"
	"github.com/andrew-bierman/lambe/internal/tracker"
)

// Driver lifecycle bounds. A wedged agent shutdown must never stall the
// loop, and the persist seam must never hang a turn start.
const (
	// driverCloseBound bounds one driver Close call.
	driverCloseBound = 30 * time.Second
	// persistBound bounds one persist-seam tracker round trip.
	persistBound = 10 * time.Second
	// Leave time for subprocess cleanup before the daemon's shutdown deadline.
	persistTransactionBound = ShutdownBound - 5*time.Second
	// settleDrainBound bounds how long a settled turn waits for the
	// event drain to deliver the final assistant message.
	settleDrainBound = 500 * time.Millisecond
)

// DefaultWire fills the Assembly with the first-party implementations: the
// configured harness driver as the agent runner, and the gate runner over
// the primary tracker's formula directory. The gate hub doubles as the
// escalation KV store, so counters persist in the same tracker metadata as
// the work itself.
func DefaultWire(cfg *config.Config, bdBin, actor string) func(*Assembly) error {
	return func(asm *Assembly) error {
		if _, err := HarnessCommand(cfg); err != nil {
			return err
		}
		log := logging.Default()

		primary := ""
		if len(cfg.Trackers) > 0 {
			primary = cfg.Trackers[0].Path
		}
		hub, err := gate.NewBdHub(bdBin, primary, actor)
		if err != nil {
			return fmt.Errorf("wire gate hub: %w", err)
		}
		// bd has no built-in needs-human status; configure it once at wiring
		// time so gate escalation can park an issue. Best-effort: the hub
		// falls back to blocked when the value cannot be configured.
		bootCtx, cancelBoot := context.WithTimeout(context.Background(), persistBound)
		if err := hub.EnsureNeedsHumanStatus(bootCtx); err != nil {
			log.Warn("needs-human custom status not configured; escalations will park as blocked", "error", err)
		}
		cancelBoot()
		esc := gate.NewEscalator(hub)
		gates := gate.NewRunner(formulaDir(primary), hub, esc, actor, log)
		gates.CheckTimeout = controlTimeout(cfg)
		asm.Gates = gates

		name := "pi"
		if cfg.Harness != "" {
			name = cfg.Harness
		}
		log.Info("agent harness configured", "harness", name)
		runner := newHarnessRunner(name, cfg)
		runner.persist = newAttemptPersister(bdBin, primary, actor, log)
		asm.Runner = runner
		return nil
	}
}

// formulaDir resolves where responsibility formulas live: the conventional
// in-tracker .beads/formulas directory.
func formulaDir(trackerDir string) string {
	if trackerDir == "" {
		return ".beads/formulas"
	}
	return trackerDir + "/.beads/formulas"
}

// harnessRunner adapts harness drivers to the symphony AgentRunner seam.
// One driver belongs to one issue workspace. Pi reopens the stable backend
// session; ACP currently creates a new conversation after process retirement.
type harnessRunner struct {
	name string
	live *liveView
	cfg  *config.Config

	// persist records an agent process group id onto the session's
	// latest attempt bead for crash recovery; nil disables the seam.
	persist func(sessionID string, pgid int) error
	// factory constructs drivers; nil means newDriver. Tests substitute
	// fakes here.
	factory func(req symphony.TurnRequest) (harness.Driver, error)

	mu      sync.Mutex
	drivers map[string]*supervisedDriver
}

func newHarnessRunner(name string, cfg *config.Config) *harnessRunner {
	return &harnessRunner{name: name, cfg: cfg, drivers: make(map[string]*supervisedDriver)}
}

// supervisedDriver pairs one driver with its event-drain goroutine. The
// drain keeps the driver's buffered event stream flowing (a full stream
// blocks the driver's emit path and gets a healthy agent watchdog-killed)
// and remembers the last assistant message so a settled turn can hand the
// agent's own summary to the gate.
type supervisedDriver struct {
	drv      harness.Driver
	done     chan struct{} // closed when the drain goroutine exits
	barriers chan chan struct{}

	mu            sync.Mutex
	lastAssistant string
	onEvent       func(harness.Event)
}

type agentStderr struct{ log *slog.Logger }

func (w agentStderr) Write(p []byte) (int, error) {
	w.log.Warn("agent stderr", "output", string(p))
	return len(p), nil
}

func newSupervisedDriver(drv harness.Driver) *supervisedDriver {
	return &supervisedDriver{drv: drv, done: make(chan struct{}), barriers: make(chan chan struct{})}
}

// drain consumes the driver's event stream until Close closes it, logging
// each event at debug level and tracking the last assistant message.
func (sd *supervisedDriver) drain(log *slog.Logger) {
	defer close(sd.done)
	log = log.With("component", "harness")
	events := sd.drv.Events()
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return
			}
			sd.recordEvent(ev, log)
		case barrier := <-sd.barriers:
			// Run has returned; consume its queued events before acknowledging.
			for len(events) > 0 {
				sd.recordEvent(<-events, log)
			}
			close(barrier)
		}
	}
}

func (sd *supervisedDriver) recordEvent(ev harness.Event, log *slog.Logger) {
	if sd.onEvent != nil {
		sd.onEvent(ev)
	}
	if msg, ok := ev.(harness.MessageCompletedEvent); ok && msg.Role == "assistant" && msg.Text != "" {
		sd.mu.Lock()
		sd.lastAssistant = msg.Text
		sd.mu.Unlock()
	}
	log.Debug("agent event", "type", fmt.Sprintf("%T", ev))
}

// resetAssistant forgets the previous turn's message so a settled turn
// reports this turn's summary, not a stale one.
func (sd *supervisedDriver) resetAssistant() {
	sd.mu.Lock()
	defer sd.mu.Unlock()
	sd.lastAssistant = ""
}

// awaitAssistantText synchronizes with the event reader after Run returns,
// so the gate sees the final assistant message rather than an earlier one.
func (sd *supervisedDriver) awaitAssistantText(bound time.Duration) string {
	timer := time.NewTimer(bound)
	defer timer.Stop()
	barrier := make(chan struct{})
	select {
	case sd.barriers <- barrier:
		select {
		case <-barrier:
		case <-sd.done:
		case <-timer.C:
		}
	case <-sd.done:
	case <-timer.C:
	}
	sd.mu.Lock()
	defer sd.mu.Unlock()
	return sd.lastAssistant
}

// closeDriver closes one driver under a bounded context so a wedged agent
// shutdown cannot stall the loop.
func closeDriver(drv harness.Driver) {
	ctx, cancel := context.WithTimeout(context.Background(), driverCloseBound)
	defer cancel()
	_ = drv.Close(ctx)
}

// RunTurn implements symphony.AgentRunner: it drives exactly one turn on
// the issue's driver and maps the typed harness outcome onto the loop's
// turn exits. Settled is the only completion signal; a suspected provider
// limit defers the attempt without consuming it. Supervision outcomes that
// leave the driver's process dead (watchdog kill, limit kill, exit,
// outright failure) retire the driver so the next turn starts fresh
// instead of reusing a Start no-op.
func (r *harnessRunner) RunTurn(ctx context.Context, req symphony.TurnRequest) (symphony.TurnResult, error) {
	if err := ctx.Err(); err != nil {
		return symphony.TurnResult{Exit: symphony.TurnFailure}, err
	}
	backend := r.backendKey()
	foreign := req.ThreadID != "" && ((req.ThreadHarness != "" && req.ThreadHarness != backend) || (r.name != "pi" && req.ThreadID == sessionIDFor(req.SessionID)))
	if foreign {
		if req.Turn > 1 {
			return symphony.TurnResult{Exit: symphony.TurnFailure}, symphony.ErrRestartFromFullPrompt
		}
		logging.Default().Warn("harness changed; starting from the full task prompt", "issue", req.SessionID)
		req.ThreadID = ""
		if r.name == "pi" {
			req.ThreadID = sessionIDFor(req.SessionID) + "-" + rand.Text()
		}
	}
	sd, err := r.driverFor(req)
	if err != nil {
		return symphony.TurnResult{Exit: symphony.TurnFailure}, err
	}

	err = sd.drv.Start(ctx)
	if err != nil && ctx.Err() == nil && req.Turn == 1 && req.ThreadID != "" && (errors.Is(err, acpdrv.ErrSessionNotFound) || errors.Is(err, acpdrv.ErrResumeNotSupported)) {
		// Reconstruct at a full-prompt boundary; a continuation fragment cannot replace lost context.
		logging.Default().Warn("saved conversation unavailable; reconstructing from the full task prompt", "issue", req.SessionID, "thread", req.ThreadID, "error", err)
		r.retire(req.SessionID, sd)
		req.ThreadID = ""
		sd, err = r.driverFor(req)
		if err == nil {
			err = sd.drv.Start(ctx)
		}
	}
	if err != nil {
		if req.Turn > 1 && (errors.Is(err, acpdrv.ErrSessionNotFound) || errors.Is(err, acpdrv.ErrResumeNotSupported)) {
			return symphony.TurnResult{Exit: symphony.TurnFailure}, symphony.ErrRestartFromFullPrompt
		}
		return symphony.TurnResult{Exit: symphony.TurnFailure}, err
	}
	if err := ctx.Err(); err != nil {
		r.retire(req.SessionID, sd)
		return symphony.TurnResult{Exit: symphony.TurnFailure}, err
	}
	thread := sessionIDFor(req.SessionID)
	if r.name == "pi" {
		if req.ThreadID != "" {
			thread = req.ThreadID
		}
	} else {
		stats, statsErr := sd.drv.SessionStats(ctx)
		if statsErr != nil || stats.SessionID == "" {
			r.retire(req.SessionID, sd)
			return symphony.TurnResult{Exit: symphony.TurnFailure}, fmt.Errorf("%w: backend did not supply its session identity: %v", proc.ErrBindingUnavailable, statsErr)
		}
		thread = stats.SessionID
	}
	if req.RecordThread != nil {
		if err := req.RecordThread(thread, backend); err != nil {
			r.retire(req.SessionID, sd)
			return symphony.TurnResult{Exit: symphony.TurnFailure}, fmt.Errorf("%w: save backend session: %w", proc.ErrBindingUnavailable, err)
		}
	}
	if err := ctx.Err(); err != nil {
		r.retire(req.SessionID, sd)
		return symphony.TurnResult{Exit: symphony.TurnFailure}, err
	}
	sd.resetAssistant()
	outcome, err := sd.drv.Run(ctx, req.Prompt)
	if deadOutcome(outcome.Kind) {
		r.retire(req.SessionID, sd)
	}
	res := symphony.TurnResult{ThreadID: thread, ThreadHarness: backend}
	switch outcome.Kind {
	case harness.OutcomeSettled:
		res.Exit = symphony.TurnSuccess
		res.Settled = true
	case harness.OutcomeLimitSuspected:
		res.Exit = symphony.TurnDeferred
		res.WakeAt = outcome.WakeAt
	case harness.OutcomeWatchdogKilled:
		res.Exit = symphony.TurnTimeout
	default:
		res.Exit = symphony.TurnFailure
	}
	if outcome.Kind == harness.OutcomeSettled {
		// The gate judges the agent's own account of the work; prefer
		// the last assistant message over bare outcome diagnostics.
		res.Output = sd.awaitAssistantText(settleDrainBound)
	}
	if res.Output == "" && outcome.Err != nil {
		res.Output = outcome.Err.Error()
	}
	return res, err
}

// CloseSession implements symphony.SessionCloser: it shuts the issue's
// driver down under a bounded context and forgets it. The next turn for
// the issue constructs a fresh driver resuming the same backend session.
func (r *harnessRunner) CloseSession(ctx context.Context, sessionID string) {
	r.mu.Lock()
	sd, ok := r.drivers[sessionID]
	if ok {
		delete(r.drivers, sessionID)
	}
	r.mu.Unlock()
	if !ok {
		return
	}
	bound := driverCloseBound
	if deadline, ok := ctx.Deadline(); ok {
		bound = min(bound, time.Until(deadline))
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), bound)
	defer cancel()
	_ = sd.drv.Close(cctx)
}

// driverFor returns the supervised driver for the session, constructing
// and draining a new one when none is live.
func (r *harnessRunner) driverFor(req symphony.TurnRequest) (*supervisedDriver, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if sd, ok := r.drivers[req.SessionID]; ok {
		return sd, nil
	}
	factory := r.factory
	if factory == nil {
		factory = r.newDriver
	}
	drv, err := factory(req)
	if err != nil {
		return nil, err
	}
	sd := newSupervisedDriver(drv)
	if r.live != nil {
		sd.onEvent = func(ev harness.Event) { r.live.event(req.SessionID, ev) }
	}
	r.drivers[req.SessionID] = sd
	go sd.drain(logging.Default())
	return sd, nil
}

// retire closes a dead driver and removes it from the table (only when it
// is still the mapped one) so the next turn constructs a fresh driver.
func (r *harnessRunner) retire(sessionID string, sd *supervisedDriver) {
	r.mu.Lock()
	if r.drivers[sessionID] == sd {
		delete(r.drivers, sessionID)
	}
	r.mu.Unlock()
	closeDriver(sd.drv)
}

// deadOutcome reports whether a run outcome leaves the driver's process
// unusable: watchdog and limit kills tear down the process group, an exit
// or outright failure means there is nothing left to reuse. Settled and
// handled turns leave a live process for the session's next turn.
func deadOutcome(kind harness.OutcomeKind) bool {
	switch kind {
	case harness.OutcomeSettled, harness.OutcomeHandled:
		return false
	default:
		return true
	}
}

// newDriver constructs the configured harness driver bound to one issue's
// workspace and derived session id, wiring the persist seam so the process
// group id lands on the session's attempt bead before Start returns.
func (r *harnessRunner) newDriver(req symphony.TurnRequest) (harness.Driver, error) {
	sess := sessionIDFor(req.SessionID)
	if req.ThreadID != "" {
		sess = req.ThreadID
	}
	persist := r.persistFor(req.SessionID)
	// Loop dispatch is top-level: supervised agents run at depth 1 so the
	// MCP server's recursion pre-check sees them and refuses their spawns.
	env := append(os.Environ(), mcp.DepthEnvVar+"=1")
	stderr := agentStderr{log: logging.Default().With("session", sess)}
	if r.name == "pi" {
		return pidrv.New(pidrv.Options{SessionID: sess, Dir: req.Workspace, Env: env, Logger: logging.Default(), Stderr: stderr, Persist: persist, SilenceAfter: silenceTimeout(r.cfg), MaxRun: turnTimeout(r.cfg)}), nil
	}
	argv, err := launchCommand(r.cfg)
	if err != nil {
		return nil, err
	}
	if len(r.cfg.ACP.Command) == 0 {
		env = runtimeEnv(argv, env)
	}
	return acpdrv.New(acpdrv.Options{SessionID: req.ThreadID, Argv: argv, Dir: req.Workspace, Env: env, Logger: logging.Default(), Stderr: stderr, Persist: persist, SilenceAfter: silenceTimeout(r.cfg), MaxRun: turnTimeout(r.cfg)})
}

// persistFor binds the runner's persist seam to one issue. It returns nil
// when no persist function is configured, leaving the drivers'
// crash-recovery seam dormant.
func (r *harnessRunner) persistFor(issueID string) func(pgid int, session string) error {
	if r.persist == nil {
		return nil
	}
	return func(pgid int, _ string) error { return r.persist(issueID, pgid) }
}

// newAttemptPersister returns the persist seam DefaultWire installs: when
// a driver's agent process exists, the latest attempt bead for the session
// is re-put with the process group id so orphan recovery can find and kill
// the group after a crash.
//
// The attempt number is the highest number already recorded for the
// session, because the loop records each attempt bead before it hands the
// turn to the runner: the latest bead at persist time is the running
// attempt. PutAttempt is an idempotent update for the same session and
// number, so the re-put only adds the pgid. When no attempt bead exists
// yet (a runner invoked outside the loop), number 1 is created.
//
// The tracker is opened lazily because the wire hook runs before the
// daemon opens its own trackers; bd's shell transport makes a second local
// over the same directory safe.
func newAttemptPersister(bdBin, dir, actor string, log *slog.Logger) func(sessionID string, pgid int) error {
	persist := newAttemptPersisterFrom(func(ctx context.Context) (tracker.Tracker, error) {
		local, err := tracker.NewLocal(bdBin, dir, actor)
		if err != nil {
			return nil, err
		}
		if err := local.Init(ctx); err != nil {
			return nil, err
		}
		return local, nil
	}, log, proc.ProcessIdentity)
	return func(sessionID string, pgid int) error {
		return persist(context.Background(), sessionID, pgid)
	}
}

// newAttemptPersisterFrom is newAttemptPersister with the tracker opener
// injected, so tests can substitute an in-memory tracker.
func newAttemptPersisterFrom(openTracker func(context.Context) (tracker.Tracker, error), log *slog.Logger, identity func(int) (string, error)) func(context.Context, string, int) error {
	// Serialize lazy initialization without blocking past a caller's deadline.
	opening := make(chan struct{}, 1)
	var opened tracker.Tracker
	open := func(ctx context.Context) (tracker.Tracker, error) {
		select {
		case opening <- struct{}{}:
			defer func() { <-opening }()
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if opened != nil {
			return opened, nil
		}
		var tr tracker.Tracker
		err := retryPersistTimeout(ctx, func(ctx context.Context) error {
			var err error
			tr, err = openTracker(ctx)
			return err
		})
		if err != nil {
			return nil, err
		}
		if tr == nil {
			return nil, errors.New("process binding tracker unavailable")
		}
		opened = tr
		return tr, nil
	}
	return func(ctx context.Context, sessionID string, pgid int) error {
		ctx, cancel := context.WithTimeout(ctx, persistTransactionBound)
		defer cancel()
		tr, err := open(ctx)
		if err != nil {
			return err
		}

		var attempts []tracker.Attempt
		err = retryPersistTimeout(ctx, func(ctx context.Context) error {
			var readErr error
			attempts, readErr = tr.Attempts(ctx, sessionID)
			return readErr
		})
		if err != nil {
			log.Error("persist: read attempts", "session", sessionID, "error", err)
			return err
		}
		number := 0
		for _, a := range attempts {
			if a.Number > number {
				number = a.Number
			}
		}
		if number == 0 {
			number = 1
		}
		token, err := identity(pgid)
		if err != nil {
			return fmt.Errorf("read process identity: %w", err)
		}
		if err := retryPersistTimeout(ctx, func(ctx context.Context) error {
			return tr.PutAttempt(ctx, tracker.Attempt{SessionID: sessionID, Number: number, PGID: pgid, ProcessIdentity: token})
		}); err != nil {
			log.Error("persist: record pgid", "session", sessionID, "error", err)
			return err
		}
		return nil
	}
}

// retryPersistTimeout retries an idempotent tracker operation once after a timeout.
func retryPersistTimeout(ctx context.Context, operation func(context.Context) error) error {
	for i := 0; ; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		attemptCtx, cancel := context.WithTimeout(ctx, persistBound)
		err := operation(attemptCtx)
		cancel()
		if i > 0 || !errors.Is(err, context.DeadlineExceeded) {
			return err
		}
	}
}

// sessionIDFor derives a stable backend session id from the issue id so
// restarts resume the same session.
func sessionIDFor(issueID string) string {
	return "lambe-" + issueID
}

func silenceTimeout(cfg *config.Config) time.Duration { return cfg.Supervisor.SilenceTimeout }
func turnTimeout(cfg *config.Config) time.Duration    { return cfg.Supervisor.TurnTimeout }

// HarnessCommand resolves the configured backend launch argv without invoking a shell.
func HarnessCommand(cfg *config.Config) ([]string, error) {
	switch cfg.Harness {
	case "", "pi":
		return []string{"pi"}, nil
	case "codex":
		return acpdrv.Lookup("codex")
	case "acp":
		if len(cfg.ACP.Command) > 0 {
			return append([]string(nil), cfg.ACP.Command...), nil
		}
		agent := cfg.ACP.Agent
		if agent == "" {
			agent = "opencode"
		}
		return acpdrv.Lookup(agent)
	default:
		return nil, fmt.Errorf("unknown harness %q; choose pi, codex, or acp", cfg.Harness)
	}
}

// CheckHarness fails startup before claiming work when the backend executable is missing.
func CheckHarness(cfg *config.Config) error {
	argv, err := launchCommand(cfg)
	if err != nil {
		return err
	}
	if len(cfg.ACP.Command) > 0 {
		return nil
	}
	return checkAdapterRuntime(argv)
}

// backendKey keeps saved conversations attached to their originating agent command.
func (r *harnessRunner) backendKey() string {
	if r.name != "acp" || r.cfg == nil {
		return r.name
	}
	argv, err := HarnessCommand(r.cfg)
	if err != nil {
		return r.name
	}
	sum := sha256.Sum256([]byte(strings.Join(argv, "\x00")))
	return fmt.Sprintf("acp:%x", sum[:8])
}
