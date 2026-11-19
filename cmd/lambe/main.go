// Command lambe is the supervisor daemon for autonomous coding agents.
package main

import (
	"bufio"
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"

	"github.com/andrew-bierman/lambe/internal/config"
	"github.com/andrew-bierman/lambe/internal/daemon"
	"github.com/andrew-bierman/lambe/internal/gate"
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
  lambe init    [-config FILE] [--non-interactive] set up config, workflow, and starter formula
  lambe run     [-config FILE] run the supervisor daemon until SIGTERM/SIGINT
  lambe status  [-config FILE] print the morning report
  lambe mcp     [-config FILE] serve lambe's MCP tools over stdio
  lambe logs    [-config FILE] [-issue ID] [-follow] [-json] show agent output
  lambe enqueue [-config FILE] -workflow NAME [-apply] ISSUE_ID
  lambe tui     [-config FILE | -demo]                 interactive dashboard (split panes, scrollback)
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
		fmt.Println(buildVersion())
	case "init":
		if err := runInit(args[1:], os.Stdin, os.Stdout); err != nil {
			logging.Default().Error("lambe init failed", "error", err.Error())
			os.Exit(1)
		}
	case "run":
		if err := runDaemon(args[1:]); err != nil {
			logging.Default().Error("lambe run failed", "error", err.Error())
			os.Exit(exitCodeFor(err))
		}
	case "enqueue":
		if err := runEnqueue(args[1:], os.Stdout); err != nil {
			logging.Default().Error("queueing task failed", "error", err)
			os.Exit(1)
		}
	case "logs", "tui":
		if err := runLive(args[1:], args[0] == "tui", os.Stdin, os.Stdout); err != nil {
			logging.Default().Error("live view failed", "error", err)
			os.Exit(1)
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
func parseCommon(args []string) (*config.Config, string, error) {
	fs := flag.NewFlagSet("lambe", flag.ContinueOnError)
	configFile := fs.String("config", "", "path to a lambe config file")
	if err := fs.Parse(args); err != nil {
		return nil, "", err
	}
	path, err := cliConfigPath(*configFile)
	if err != nil {
		return nil, "", err
	}
	cfg, err := config.Load(config.Options{File: path})
	if err != nil {
		return nil, "", fmt.Errorf("configuration invalid: %w", err)
	}
	return cfg, path, nil
}

func cliConfigPath(explicit string) (string, error) {
	if explicit != "" {
		abs, err := filepath.Abs(explicit)
		if err != nil {
			return "", fmt.Errorf("resolve config path %s: %w", explicit, err)
		}
		return abs, nil
	}
	path := config.DefaultPath()
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("read default configuration %s: %w", path, err)
	}
	return path, nil
}

// runDaemon assembles and runs the daemon until a terminating signal.
func runDaemon(args []string) error {
	cfg, configPath, err := parseCommon(args)
	if err != nil {
		return err
	}
	if err := requireConfig(configPath); err != nil {
		return err
	}
	if err := requireLocalTracker(cfg); err != nil {
		return err
	}
	workflowPath := cfg.WorkflowPath
	if _, err := os.Stat(workflowPath); err != nil {
		return fmt.Errorf("read %s: %w; create the agent workflow first (see https://github.com/andrew-bierman/lambe#quickstart)", workflowPath, err)
	}

	if err := daemon.CheckHarness(cfg); err != nil {
		return err
	}

	if err := requireGates(cfg); err != nil {
		return err
	}
	d, err := daemon.New(daemonOptions(cfg, configPath))
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	return d.Run(ctx)
}

// requireConfig refuses to start the daemon with only implicit defaults:
// with no config file at all the run would silently supervise an empty
// setup. `lambe init` is the intended fix.
func requireConfig(configPath string) error {
	if configPath != "" {
		return nil
	}
	path := config.DefaultPath()
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("no lambe configuration at %s: %w; run `lambe init` to create one (or pass -config FILE)", path, err)
	}
	return nil
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
		WorkflowPath: cfg.WorkflowPath,
		LockName:     daemon.InstanceLockName(cfg),
		Log:          logging.Default(),
		Wire:         daemon.DefaultWire(cfg, "", actor),
	}
}

// runStatus prints the morning report from the local tracker.
func runStatus(args []string) error {
	cfg, _, err := parseCommon(args)
	if err != nil {
		return err
	}
	if err := requireLocalTracker(cfg); err != nil {
		return err
	}
	return daemon.Status(os.Stdout, cfg, daemon.DefaultMachine())
}

// runMCP serves lambe's MCP tools over stdio: it assembles the daemon's
// control stack (trackers plus the live-run registry) without starting the
// tick loop, then proxies tool calls into it. The daemon proper may be
// running separately; spawns register runs its next tick dispatches.
func runMCP(args []string) error {
	cfg, _, err := parseCommon(args)
	if err != nil {
		return err
	}
	if err := requireLocalTracker(cfg); err != nil {
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

func requireLocalTracker(cfg *config.Config) error {
	if len(cfg.Trackers) == 0 {
		return errors.New("no trackers configured; run `lambe init` to create a config")
	}
	dir := cfg.Trackers[0].Path
	if dir == "" {
		dir = "."
	}
	if _, err := os.Stat(filepath.Join(dir, ".beads")); err != nil { // #nosec G703 -- operator-configured tracker path
		return fmt.Errorf("read tracker %s: %w; run `bd init` in that project root, or pass -config FILE to select another tracker", cfg.Trackers[0].Name, err)
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

// workflowTemplate is the WORKFLOW.md `lambe init` drops into a tracker
// directory that has none. It mirrors the repository's root WORKFLOW.md.
const workflowTemplate = `---
# lambe WORKFLOW.md template.
#
# Front matter configures the supervisor for one responsibility; the body is
# the agent prompt, rendered with strict templating over {issue, attempt}.
# String values support $VAR, ${VAR}, and ${VAR:-default} environment
# resolution; a variable that is unset without a default is a load error.
#
# This file hot-reloads: a valid edit applies from the next claim; an
# invalid edit is logged and the last-good copy keeps serving. In-flight
# runs always finish under the revision they claimed.
hooks:
  # Runs once, only when the workspace directory is first created.
  # Failure is fatal to creation; creation is retried on a later tick.
  after_create: "git init -q . || true"
  # Runs before each attempt. Failure aborts the attempt.
  before_run: "echo workspace ready"
  # Runs after every attempt, including timeouts and cancellations.
  # Failures are logged and ignored.
  after_run: "echo attempt finished"
  # Runs before workspace deletion. Failures are ignored; cleanup proceeds.
  before_remove: "echo workspace removed"
max_turns: 20
hook_timeout_ms: 60000
---

You are an autonomous coding agent working the issue "{{ issue }}".

This is attempt {{ attempt }}. Previous attempts, claims, and gate results
are recorded in the tracker; read them before repeating work.

Working rules:

1. Work only inside this workspace; it is your per-issue workspace.
2. Read the tracker for the issue's current state before acting.
3. Write progress to the tracker as you go.
4. When you believe the work is finished, post your final progress comment
   and stop; an external gate verifies completion.
5. If you cannot continue, hand the issue off: leave the workspace and the
   tracker in a state a human or the next attempt can pick up.
`

// formulaTemplate is the starter formula `lambe init` writes to
// .beads/formulas/keep-tests-green.formula.toml.
const formulaTemplate = `# Starter responsibility. Enable its trigger only after configuring the
# workflow workspace hooks, a repository-specific check, and completion gate.
# Exit 0 keeps automatic materialization disabled during setup.

formula = "keep-tests-green"
version = 1
type = "workflow"
description = "Diagnose a failing repository check and submit a verified fix"

[trigger]
command = "exit 0"

[[steps]]
id = "diagnose"
title = "Diagnose the failing repository check"
description = "Run the configured check and identify the cause in the issue workspace."

[[steps]]
id = "fix"
title = "Fix and verify the failing repository check"
description = "Implement the smallest fix, run local checks, and create a pull request."
needs = ["diagnose"]

# Add a [gate] table here or in a separate *.formula.toml before running
# Lambe. All configured gates apply to every issue in this tracker.
# A gate must verify the task independently of the agent's final message.
`

// say prints init progress output; a broken terminal pipe is not a setup
// failure, so write errors are deliberately ignored.
func say(out io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(out, format, args...)
}

// runInit is the setup wizard: it detects (or asks for) the tracker
// directory, writes the default config, seeds a WORKFLOW.md and a starter
// formula, and prints next steps. With --non-interactive it never prompts
// and never runs external commands; every choice falls back to a default.
func runInit(args []string, in io.Reader, out io.Writer) error {
	fs := flag.NewFlagSet("lambe init", flag.ContinueOnError)
	configFile := fs.String("config", "", "path to write the lambe config to")
	harness := fs.String("harness", "pi", "agent backend: pi, codex, or acp")
	nonInteractive := fs.Bool("non-interactive", false, "skip all prompts and use defaults")
	if err := fs.Parse(args); err != nil {
		// -h/--help prints the flag usage and returns flag.ErrHelp: that is
		// a successful outcome, not a setup failure.
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if _, err := daemon.HarnessCommand(&config.Config{Harness: *harness}); err != nil {
		return err
	}

	trackerDir, found := detectTrackerDir(".")
	if !found {
		var err error
		trackerDir, err = os.Getwd()
		if err != nil {
			return err
		}
		trackerDir, found, err = maybeInitTracker(trackerDir, *nonInteractive, in, out)
		if err != nil {
			return err
		}
	}
	absTracker, err := filepath.Abs(trackerDir)
	if err != nil {
		return err
	}

	configPath := *configFile
	if configPath == "" {
		configPath = config.DefaultPath()
	}
	if _, err := os.Stat(configPath); err == nil {
		return fmt.Errorf("config already exists at %s; edit it directly or pass -config FILE to initialize a different one", configPath)
	}
	if err := os.MkdirAll(filepath.Dir(configPath), 0o750); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	cfgYAML := fmt.Sprintf("# lambe configuration written by lambe init.\n"+
		"# See https://github.com/andrew-bierman/lambe for every key.\n"+
		"harness: %s\n"+
		"workflow_path: %q\n"+
		"trackers:\n"+
		"  - name: local\n"+
		"    type: repo\n"+
		"    path: %q\n", *harness, filepath.Join(absTracker, "WORKFLOW.md"), absTracker)
	if err := os.WriteFile(configPath, []byte(cfgYAML), 0o600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	say(out, "wrote config %s\n", configPath)

	workflowDst := filepath.Join(absTracker, "WORKFLOW.md")
	if _, err := os.Stat(workflowDst); err == nil {
		say(out, "kept existing %s\n", workflowDst)
	} else if err := os.WriteFile(workflowDst, []byte(workflowTemplate), 0o600); err != nil {
		return fmt.Errorf("write WORKFLOW.md: %w", err)
	} else {
		say(out, "wrote %s\n", workflowDst)
	}

	formulaDir := filepath.Join(absTracker, ".beads", "formulas")
	if err := os.MkdirAll(formulaDir, 0o750); err != nil {
		return fmt.Errorf("create formulas directory: %w", err)
	}
	formulaDst := filepath.Join(formulaDir, "keep-tests-green.formula.toml")
	if _, err := os.Stat(formulaDst); err == nil {
		say(out, "kept existing %s\n", formulaDst)
	} else if err := os.WriteFile(formulaDst, []byte(formulaTemplate), 0o600); err != nil {
		return fmt.Errorf("write starter formula: %w", err)
	} else {
		say(out, "wrote %s\n", formulaDst)
	}

	say(out, "\nnext steps:\n")
	if !found {
		say(out, "  cd %s && bd init   # create the tracker `lambe init` could not find\n", absTracker)
	}
	say(out, "  edit %s to taste (prompt, hooks, gates)\n", workflowDst)
	say(out, "  add a [gate] completion check to %s (or a separate formula); all gates apply to every task\n", formulaDst)
	say(out, "  configure the gate before starting: existing ready issues are dispatched automatically; the starter trigger is disabled\n")
	say(out, "  lambe run      # Lambe starts and supervises the configured agent itself\n")
	say(out, "  bd create \"Your task, with acceptance criteria\" --type task  # queue work after configuring the gate\n")
	say(out, "  lambe status   # print the morning report\n")
	return nil
}

// detectTrackerDir walks up from start looking for a .beads directory,
// returning the containing directory and whether one was found.
func detectTrackerDir(start string) (string, bool) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", false
	}
	for {
		if info, err := os.Stat(filepath.Join(dir, ".beads")); err == nil && info.IsDir() {
			return dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

// maybeInitTracker offers to create a missing tracker with bd init. It
// returns the tracker directory and whether a tracker now exists. In
// non-interactive mode it never runs anything and reports not-found.
func maybeInitTracker(dir string, nonInteractive bool, in io.Reader, out io.Writer) (string, bool, error) {
	if nonInteractive {
		say(out, "no .beads tracker found at or above %s; using it as the tracker path anyway (run `bd init` there)\n", dir)
		return dir, false, nil
	}
	bd, err := exec.LookPath("bd")
	if err != nil {
		say(out, "no .beads tracker found at or above %s and no `bd` on PATH.\n", dir)
		say(out, "install beads (https://beads.direct) and run `bd init` there, then re-run `lambe init`.\n")
		return dir, false, nil
	}
	say(out, "no .beads tracker found at or above %s.\ninitialize one now with `%s init`? [y/N] ", dir, bd)
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && line == "" {
		return dir, false, fmt.Errorf("read answer: %w", err)
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	if answer != "y" && answer != "yes" {
		say(out, "skipping bd init; using %s as the tracker path anyway\n", dir)
		return dir, false, nil
	}
	cmd := exec.Command(bd, "init", "--non-interactive") //nolint:gosec // G204: bd path comes from LookPath; argv is fixed.
	cmd.Dir = dir
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Run(); err != nil {
		return dir, false, fmt.Errorf("bd init: %w", err)
	}
	say(out, "initialized tracker in %s\n", dir)
	return dir, true, nil
}

func buildVersion() string {
	v, c, d := version, commit, date
	if info, ok := debug.ReadBuildInfo(); ok {
		if v == "dev" && info.Main.Version != "" && info.Main.Version != "(devel)" {
			v = info.Main.Version
		}
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				if c == "none" {
					c = setting.Value
				}
			case "vcs.time":
				if d == "unknown" {
					d = setting.Value
				}
			}
		}
	}
	return fmt.Sprintf("lambe %s (commit %s, built %s)", v, c, d)
}

func requireGates(cfg *config.Config) error {
	if len(cfg.Trackers) == 0 {
		return errors.New("no trackers configured; run lambe init or configure a local tracker")
	}
	dir := filepath.Join(cfg.Trackers[0].Path, ".beads", "formulas")
	if len(cfg.Workflows) > 0 {
		for _, route := range cfg.Workflows {
			gateDir := cmp.Or(route.GateDir, dir)
			if err := requireGateDir(gateDir); err != nil {
				return fmt.Errorf("workflow %s: %w", route.Name, err)
			}
		}
		return nil
	}
	return requireGateDir(dir)
}

func requireGateDir(dir string) error {
	specs, err := gate.LoadDir(dir)
	if err != nil {
		return fmt.Errorf("completion gates invalid: %w", err)
	}
	if len(specs) == 0 {
		return fmt.Errorf("no completion gate configured in %s; add a [gate] table with a check that proves completion before starting Lambe", dir)
	}
	return nil
}
