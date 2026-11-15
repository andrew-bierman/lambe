package daemon

// This file is the wiring for units this package consumes but does not own:
// the harness driver (Pi by default, ACP by configuration) adapted to the
// symphony AgentRunner seam, and the verified-completion gate runner.
// DefaultWire is the Options.Wire implementation the lambe binary installs.
import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/andrew-bierman/lambe/internal/config"
	"github.com/andrew-bierman/lambe/internal/gate"
	"github.com/andrew-bierman/lambe/internal/harness"
	acpdrv "github.com/andrew-bierman/lambe/internal/harness/acp"
	pidrv "github.com/andrew-bierman/lambe/internal/harness/pi"
	"github.com/andrew-bierman/lambe/internal/logging"
	"github.com/andrew-bierman/lambe/internal/symphony"
)

// DefaultWire fills the Assembly with the first-party implementations: the
// configured harness driver as the agent runner, and the gate runner over
// the primary tracker's formula directory. The gate hub doubles as the
// escalation KV store, so counters persist in the same tracker metadata as
// the work itself.
func DefaultWire(cfg *config.Config, bdBin, actor string) func(*Assembly) error {
	return func(asm *Assembly) error {
		log := logging.Default()

		primary := ""
		if len(cfg.Trackers) > 0 {
			primary = cfg.Trackers[0].Path
		}
		hub, err := gate.NewBdHub(bdBin, primary, actor)
		if err != nil {
			return fmt.Errorf("wire gate hub: %w", err)
		}
		esc := gate.NewEscalator(hub)
		asm.Gates = gate.NewRunner(formulaDir(primary), hub, esc, actor, log)

		name := "pi"
		if cfg.Harness != "" {
			name = cfg.Harness
		}
		asm.Runner = newHarnessRunner(name, cfg)
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
// One driver instance is created per supervised issue: a driver's session
// and working directory belong to a single workspace. The backend session
// id is derived from the issue id, and both shipped drivers treat starting
// with an existing session id as resuming it, so a daemon restart picks up
// the same backend session where one exists.
type harnessRunner struct {
	name string
	cfg  *config.Config

	mu      sync.Mutex
	drivers map[string]harness.Driver
}

func newHarnessRunner(name string, cfg *config.Config) *harnessRunner {
	return &harnessRunner{name: name, cfg: cfg, drivers: make(map[string]harness.Driver)}
}

// RunTurn implements symphony.AgentRunner: it drives exactly one turn on
// the issue's driver and maps the typed harness outcome onto the loop's
// turn exits. Settled is the only completion signal; a suspected provider
// limit defers the attempt without consuming it.
func (r *harnessRunner) RunTurn(ctx context.Context, req symphony.TurnRequest) (symphony.TurnResult, error) {
	r.mu.Lock()
	drv, ok := r.drivers[req.SessionID]
	if !ok {
		var err error
		drv, err = r.newDriver(req)
		if err != nil {
			r.mu.Unlock()
			return symphony.TurnResult{Exit: symphony.TurnFailure}, err
		}
		r.drivers[req.SessionID] = drv
	}
	r.mu.Unlock()

	if err := drv.Start(ctx); err != nil {
		return symphony.TurnResult{Exit: symphony.TurnFailure}, err
	}
	outcome, err := drv.Run(ctx, req.Prompt)
	res := symphony.TurnResult{ThreadID: sessionIDFor(req.SessionID)}
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
	if outcome.Err != nil {
		res.Output = outcome.Err.Error()
	}
	return res, err
}

// newDriver constructs the configured harness driver bound to one issue's
// workspace and derived session id.
func (r *harnessRunner) newDriver(req symphony.TurnRequest) (harness.Driver, error) {
	sess := sessionIDFor(req.SessionID)
	switch r.name {
	case "acp":
		return acpdrv.New(acpdrv.Options{
			Agent:        "opencode",
			Dir:          req.Workspace,
			Logger:       logging.Default(),
			SilenceAfter: silenceTimeout(r.cfg),
			MaxRun:       turnTimeout(r.cfg),
		})
	default:
		return pidrv.New(pidrv.Options{
			SessionID:    sess,
			Dir:          req.Workspace,
			Logger:       logging.Default(),
			SilenceAfter: silenceTimeout(r.cfg),
			MaxRun:       turnTimeout(r.cfg),
		}), nil
	}
}

// sessionIDFor derives a stable backend session id from the issue id so
// restarts resume the same session.
func sessionIDFor(issueID string) string {
	return "lambe-" + issueID
}

func silenceTimeout(cfg *config.Config) time.Duration { return cfg.Supervisor.SilenceTimeout }
func turnTimeout(cfg *config.Config) time.Duration    { return cfg.Supervisor.TurnTimeout }
