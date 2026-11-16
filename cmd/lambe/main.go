// Command lambe is the supervisor daemon for autonomous coding agents.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/andrew-bierman/lambe/internal/config"
	"github.com/andrew-bierman/lambe/internal/daemon"
	"github.com/andrew-bierman/lambe/internal/logging"
	"github.com/andrew-bierman/lambe/internal/mcp"
)

// Build information, overridable at link time via -ldflags.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

const usage = `lambe is the supervisor daemon for autonomous coding agents.

usage:
  lambe run     [-config FILE] run the supervisor daemon until SIGTERM/SIGINT
  lambe status  [-config FILE] print the morning report
  lambe mcp     [-config FILE] serve lambe's MCP tools over stdio
  lambe version print build information
`

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	switch args[0] {
	case "version":
		fmt.Printf("lambe %s (commit %s, built %s)\n", version, commit, date)
	case "run":
		if err := runDaemon(args[1:]); err != nil {
			logging.Default().Error("lambe run failed", "error", err.Error())
			os.Exit(exitCodeFor(err))
		}
	case "status":
		if err := runStatus(args[1:]); err != nil {
			logging.Default().Error("lambe status failed", "error", err.Error())
			os.Exit(1)
		}
	case "mcp":
		if err := runMCP(args[1:]); err != nil {
			logging.Default().Error("lambe mcp failed", "error", err.Error())
			os.Exit(1)
		}
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
}

// parseCommon loads configuration from the shared -config flag.
func parseCommon(args []string) (*config.Config, string) {
	fs := flag.NewFlagSet("lambe", flag.ContinueOnError)
	configFile := fs.String("config", "", "path to a lambe config file")
	if err := fs.Parse(args); err != nil {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cfg, err := config.Load(config.Options{File: *configFile})
	if err != nil {
		logging.Default().Error("configuration invalid", "error", err.Error())
		os.Exit(1)
	}
	return cfg, *configFile
}

// runDaemon assembles and runs the daemon until a terminating signal.
func runDaemon(args []string) error {
	cfg, configPath := parseCommon(args)
	if err := requireLocalTracker(configPath); err != nil {
		return err
	}
	workflowPath := daemon.ResolveWorkflowPath(cfg)
	if _, err := os.Stat(workflowPath); err != nil {
		return fmt.Errorf("read %s: %w; create the agent workflow first (see https://github.com/andrew-bierman/lambe#quickstart)", workflowPath, err)
	}

	d, err := daemon.New(daemonOptions(cfg, configPath))
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	return d.Run(ctx)
}

func daemonOptions(cfg *config.Config, configPath string) daemon.Options {
	machine := daemon.DefaultMachine()
	actor := "lambe@" + machine
	return daemon.Options{
		Config:       cfg,
		ConfigPath:   configPath,
		StateDir:     daemon.DefaultStateDir(),
		Machine:      machine,
		Actor:        actor,
		WorkflowPath: daemon.ResolveWorkflowPath(cfg),
		LockName:     daemon.InstanceLockName(cfg),
		Log:          logging.Default(),
		Wire:         daemon.DefaultWire(cfg, "", actor),
	}
}

// runStatus prints the morning report from the local tracker.
func runStatus(args []string) error {
	cfg, configPath := parseCommon(args)
	if err := requireLocalTracker(configPath); err != nil {
		return err
	}
	return daemon.Status(os.Stdout, cfg, daemon.DefaultMachine())
}

// runMCP serves lambe's MCP tools over stdio: it assembles the daemon's
// control stack (trackers plus the live-run registry) without starting the
// tick loop, then proxies tool calls into it. The daemon proper may be
// running separately; spawns register runs its next tick dispatches.
func runMCP(args []string) error {
	cfg, configPath := parseCommon(args)
	if err := requireLocalTracker(configPath); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	control, release, err := daemon.OpenControl(ctx, daemon.Options{
		Config:   cfg,
		StateDir: daemon.DefaultStateDir(),
		Machine:  daemon.DefaultMachine(),
		Log:      logging.Default(),
	})
	if err != nil {
		return err
	}
	defer release()
	return mcp.New(control, mcp.Options{Env: os.Environ()}).ServeStdio(ctx, os.Stdin, os.Stdout)
}

func requireLocalTracker(configPath string) error {
	if configPath != "" {
		return nil
	}
	if _, err := os.Stat(".beads"); err != nil {
		return fmt.Errorf("read local .beads tracker: %w; run bd init in the project root, or pass -config FILE to select another tracker", err)
	}
	return nil
}

// exitCodeFor maps daemon errors to process exit codes: lock contention is
// an immediate, clean exit so service managers do not restart-loop.
func exitCodeFor(err error) int {
	if errors.Is(err, daemon.ErrLockHeld) {
		return 3
	}
	return 1
}
