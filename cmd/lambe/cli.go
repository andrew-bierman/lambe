package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/andrew-bierman/lambe/internal/daemon"
	"github.com/andrew-bierman/lambe/internal/logging"
)

// Exit codes: the contract documented in docs/REFERENCE.md.
const (
	exitOK       = 0 // success
	exitFailure  = 1 // the command ran and failed (runtime problem)
	exitUsage    = 2 // bad command line: unknown command/flag, missing argument
	exitLockHeld = 3 // `lambe run` only: another daemon holds the instance lock
)

// configFlagHelp is the one wording of the shared --config flag.
const configFlagHelp = "path to the lambe config file (env LAMBE_CONFIG; default: the user config path)"

// helpTopic is one `lambe help` page.
type helpTopic struct {
	name    string // "init", "service install"
	summary string // one line, shown in the top-level listing
	body    string // usage, flags and examples
}

const globalFooter = `
exit codes:
  0  success
  1  the command ran and failed (runtime problem; doctor: a check failed)
  2  usage error (unknown command or flag, missing argument)
  3  lambe run: another lambe daemon already holds the instance lock

environment:
  LAMBE_CONFIG     default for --config
  LAMBE_STATE_DIR  state directory (see lambe state-dir)
  NO_COLOR         lambe prints plain text; it never emits color escapes

Flags accept one or two dashes (-json and --json are the same).
Run 'lambe help COMMAND' for details and examples.
`

// helpTopics lists every command in display order. Subcommand pages
// ("service install") follow their parent and are not listed at top level.
var helpTopics = []helpTopic{
	{"init", "set up config, workflow, and starter formula", `usage:
  lambe init [--config FILE] [--harness pi|codex|acp] [--tracker PATH] [--non-interactive]

Writes the lambe config, a WORKFLOW.md and a starter formula for the Beads
tracker in the current directory. It never writes outside that directory
tree without --tracker PATH.

flags:
  --config FILE       where to write the config (default: the user config path)
  --harness pi|codex|acp  agent backend (default pi)
  --tracker PATH      adopt the Beads tracker at PATH (needed to use a tracker above the current directory)
  --non-interactive   never prompt; refuse instead of asking

examples:
  lambe init
  lambe init --harness codex --non-interactive
  lambe init --tracker ~/work/shared-repo
`},
	{"run", "run the supervisor daemon until SIGTERM/SIGINT", `usage:
  lambe run [--config FILE]

Runs the supervisor loop in the foreground. Logs are JSON lines on stderr.
Normally started by 'lambe service install' rather than by hand.

flags:
  --config FILE   ` + configFlagHelp + `

examples:
  lambe run
  lambe run --config ./lambe.yaml
`},
	{"status", "print the morning report", `usage:
  lambe status [--config FILE] [--json]

Prints what ran, what is running, and what needs a human, read from the
tracker. It never contacts a daemon, so it works whether or not one runs.

flags:
  --config FILE   ` + configFlagHelp + `
  --json          print a state API snapshot as JSON

examples:
  lambe status
  lambe status --json | jq '.needs_human'
`},
	{"state-dir", "print the state directory and live state API descriptors", `usage:
  lambe state-dir [--json]

flags:
  --json   print JSON

examples:
  lambe state-dir
  lambe state-dir --json
`},
	{"claims", "inspect or release orphaned claims", `usage:
  lambe claims list [--config FILE]
  lambe claims release [--config FILE] [--force] ISSUE

subcommands:
  list      show claims whose run is gone
  release   release one orphaned claim

examples:
  lambe claims list
  lambe claims release lambe-abc
`},
	{"claims list", "", `usage:
  lambe claims list [--config FILE]

flags:
  --config FILE   ` + configFlagHelp + `

examples:
  lambe claims list
`},
	{"claims release", "", `usage:
  lambe claims release [--config FILE] [--force] ISSUE

flags:
  --config FILE   ` + configFlagHelp + `
  --force         release without checking the workspace and agent

examples:
  lambe claims release lambe-abc
  lambe claims release --force lambe-abc
`},
	{"doctor", "check for anything that could stall your agents", `usage:
  lambe doctor [--config FILE] [--json] [--label NAME]

Prints ok/warn/fail/skip per check with a fix: line for each problem.
Exits 1 if any check failed.

flags:
  --config FILE   ` + configFlagHelp + `
  --json          print the report as JSON
  --label NAME    check the service installed with 'lambe service install --label NAME'
                  (-service-label is a deprecated alias)

examples:
  lambe doctor
  lambe doctor --json
`},
	{"service", "keep the daemon alive under launchd/systemd", `usage:
  lambe service install [--config FILE] [--dry-run] [--no-start] [--force] [--label NAME] [--root DIR] [--manager launchd|systemd]
  lambe service status  [--label NAME] [--root DIR] [--manager launchd|systemd]
  lambe service uninstall [--label NAME] [--root DIR] [--manager launchd|systemd]

examples:
  lambe service install --dry-run
  lambe service status
`},
	{"service install", "", `usage:
  lambe service install [--config FILE] [--dry-run] [--no-start] [--force] [--label NAME] [--root DIR] [--manager launchd|systemd]

Writes the launchd/systemd unit and starts it. Unlike 'setup rex' and
'enqueue', install writes by default; --dry-run previews.

flags:
  --config FILE   config file the service runs with (default: the default config path)
  --dry-run       print the unit and its destination without writing anything
  --no-start      write the unit but do not load/start it
  --force         replace a different existing unit
  --label NAME    launchd label / systemd unit name (isolated installs)
  --root DIR      write the unit under DIR instead of $HOME; never call launchctl/systemctl
  --manager launchd|systemd  service manager (default: by OS)

examples:
  lambe service install --dry-run
  lambe service install
`},
	{"service status", "", `usage:
  lambe service status [--label NAME] [--root DIR] [--manager launchd|systemd]

Exits 1 if the service is not installed or not running.

flags:
  --label NAME    launchd label / systemd unit name
  --root DIR      look under DIR instead of $HOME
  --manager launchd|systemd  service manager (default: by OS)

examples:
  lambe service status
`},
	{"service uninstall", "", `usage:
  lambe service uninstall [--label NAME] [--root DIR] [--manager launchd|systemd]

flags:
  --label NAME    launchd label / systemd unit name
  --root DIR      remove from under DIR instead of $HOME
  --manager launchd|systemd  service manager (default: by OS)

examples:
  lambe service uninstall
`},
	{"setup", "install editor/terminal integrations (rex)", `usage:
  lambe setup rex [--apply] [--uninstall] [--force] [--no-init] [--keys] [--json] [--dir DIR]

Dry run by default: nothing is written without --apply.

flags:
  --apply       write the changes
  --uninstall   remove the managed lambe.lua and the init.lua block
  --force       replace or remove a lambe.lua you modified
  --no-init     leave init.lua alone (print the line to add instead)
  --keys        bind suggested keys under a prefix (skips keys already bound)
  --json        print the plan or result as JSON
  --dir DIR     Rex config directory (default: Rex's own)

examples:
  lambe setup rex
  lambe setup rex --apply
`},
	{"setup rex", "", `usage:
  lambe setup rex [--apply] [--uninstall] [--force] [--no-init] [--keys] [--json] [--dir DIR]

Dry run by default: nothing is written without --apply.

flags:
  --apply       write the changes
  --uninstall   remove the managed lambe.lua and the init.lua block
  --force       replace or remove a lambe.lua you modified
  --no-init     leave init.lua alone (print the line to add instead)
  --keys        bind suggested keys under a prefix (skips keys already bound)
  --json        print the plan or result as JSON
  --dir DIR     Rex config directory (default: Rex's own)

examples:
  lambe setup rex
  lambe setup rex --apply
`},
	{"mcp", "serve lambe's MCP tools over stdio", `usage:
  lambe mcp [--config FILE]

flags:
  --config FILE   ` + configFlagHelp + `

examples:
  lambe mcp --config ./lambe.yaml
`},
	{"logs", "show agent output", `usage:
  lambe logs [--config FILE] [--issue ID] [--follow] [--json [--brief]]

flags:
  --config FILE   ` + configFlagHelp + `
  --issue ID      show one issue
  --follow        follow live output
  --json          print the shared live snapshot as JSON
  --brief         with --json: leave out agent output (far smaller) and add each
                  run's AgeSec and Attention {Reason, AgeSec}, plus LambeVersion

examples:
  lambe logs --issue lambe-abc --follow
  lambe logs --json
  lambe logs --json --brief
`},
	{"enqueue", "queue a task onto a named workflow", `usage:
  lambe enqueue [--config FILE] --workflow NAME [--apply] ISSUE_ID

Preview by default: the task is only labeled with --apply.

flags:
  --config FILE     ` + configFlagHelp + `
  --workflow NAME   named workflow to run
  --apply           add the workflow label to the task

examples:
  lambe enqueue --workflow review lambe-abc
  lambe enqueue --workflow review --apply lambe-abc
`},
	{"tui", "interactive dashboard (split panes, scrollback)", `usage:
  lambe tui [--config FILE | --demo]

Needs an interactive terminal; use 'lambe logs --follow' for pipes.

flags:
  --config FILE   ` + configFlagHelp + `
  --demo          open the dashboard with sample runs

examples:
  lambe tui
  lambe tui --demo
`},
	{"version", "print build information", `usage:
  lambe version [--json]
  lambe --version

flags:
  --json   print JSON

examples:
  lambe version
  lambe version --json
`},
	{"completion", "print a shell completion script (bash, zsh, fish)", `usage:
  lambe completion bash|zsh|fish

Prints the completion script for your shell on stdout. Homebrew and the
deb/rpm/apk packages install it for you; see docs/REFERENCE.md for manual
installation.

examples:
  source <(lambe completion bash)
  lambe completion zsh > "${fpath[1]}/_lambe"
  lambe completion fish > ~/.config/fish/completions/lambe.fish
`},
	{"help", "show help for a command", `usage:
  lambe help [COMMAND [SUBCOMMAND]]

examples:
  lambe help
  lambe help service install
`},
}

// usageText is the top-level help.
func usageText() string {
	var b strings.Builder
	b.WriteString("lambe keeps autonomous coding agents working and verifies the work is done.\n\nusage:\n  lambe COMMAND [flags]\n  lambe help COMMAND\n\ncommands:\n")
	for _, t := range helpTopics {
		if t.summary != "" {
			fmt.Fprintf(&b, "  %-10s %s\n", t.name, t.summary)
		}
	}
	b.WriteString(globalFooter)
	return b.String()
}

// findTopic returns the most specific help page for words.
func findTopic(words []string) (helpTopic, bool) {
	for n := min(len(words), 2); n >= 1; n-- {
		name := strings.Join(words[:n], " ")
		for _, t := range helpTopics {
			if t.name == name {
				return t, true
			}
		}
	}
	return helpTopic{}, false
}

func topicText(t helpTopic) string {
	head := "lambe " + t.name
	if t.summary != "" {
		head += " - " + t.summary
	}
	return head + "\n\n" + t.body + "\nexit codes: 0 ok, 1 failure, 2 usage error (see 'lambe help').\n"
}

func isHelpFlag(a string) bool { return a == "-h" || a == "--help" || a == "-help" }

// helpRequested reports whether args ask for help before any "--".
func helpRequested(args []string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		if isHelpFlag(a) {
			return true
		}
	}
	return false
}

// isCommand reports whether name is a dispatchable command.
func isCommand(name string) bool {
	switch name {
	case "init", "run", "status", "state-dir", "claims", "doctor", "service", "setup", "mcp", "logs", "enqueue", "tui", "version", "completion":
		return true
	}
	return false
}

// execute is the whole CLI behind main: it returns the process exit code.
// Help, version and usage errors are handled here; commands report
// failures through errors it prints as one human line.
func execute(args []string, in io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return reportUsage(stderr, errors.New("a command is required"), "")
	}
	switch args[0] {
	case "-h", "--help", "-help":
		say(stdout, "%s", usageText())
		return exitOK
	case "--version", "-version":
		say(stdout, "%s\n", buildVersion())
		return exitOK
	case "help":
		return runHelp(args[1:], stdout, stderr)
	}
	name := args[0]
	if !isCommand(name) {
		return reportUsage(stderr, fmt.Errorf("unknown command %q", name), "")
	}
	if helpRequested(args[1:]) {
		words := append([]string{name}, nonFlagPrefix(args[1:])...)
		if t, ok := findTopic(words); ok {
			say(stdout, "%s", topicText(t))
			return exitOK
		}
	}
	err := dispatch(name, args[1:], in, stdout)
	return report(name, args[1:], err, stderr)
}

// nonFlagPrefix returns the leading non-flag words of args (a subcommand).
func nonFlagPrefix(args []string) []string {
	var out []string
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			break
		}
		out = append(out, a)
	}
	return out
}

func runHelp(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		say(stdout, "%s", usageText())
		return exitOK
	}
	if t, ok := findTopic(args); ok {
		say(stdout, "%s", topicText(t))
		return exitOK
	}
	return reportUsage(stderr, fmt.Errorf("no help for %q", strings.Join(args, " ")), "")
}

func dispatch(name string, args []string, in io.Reader, out io.Writer) error {
	switch name {
	case "version":
		return runVersion(args, out)
	case "init":
		return runInit(args, in, out)
	case "run":
		return runDaemon(args)
	case "enqueue":
		return runEnqueue(args, out)
	case "logs", "tui":
		return runLive(args, name == "tui", in, out)
	case "doctor":
		return runDoctor(args, hostDoctorEnv(), out)
	case "claims":
		return runClaims(args, out)
	case "service":
		return runService(args, hostEnv(), out)
	case "setup":
		return runSetup(args, hostRexSetupEnv(), out)
	case "status":
		return runStatus(args, out)
	case "state-dir":
		dir, source := daemon.ResolveStateDir()
		return runStateDir(args, dir, source, out)
	case "mcp":
		return runMCP(args)
	case "completion":
		return runCompletion(args, out)
	}
	return usageError{fmt.Errorf("unknown command %q", name)}
}

// flagErrorPrefixes are the messages of the flag package's own parse
// errors, for commands (logs, tui) that do not wrap them in usageError.
var flagErrorPrefixes = []string{"flag provided but not defined", "invalid value ", "flag needs an argument", "invalid boolean"}

func isFlagError(err error) bool {
	if errors.Is(err, flag.ErrHelp) {
		return true
	}
	for _, p := range flagErrorPrefixes {
		if strings.HasPrefix(err.Error(), p) || strings.Contains(err.Error(), ": "+p) {
			return true
		}
	}
	return false
}

// report prints err the way its command's audience wants and returns the
// exit code. The daemon commands (run, mcp) keep structured JSON logs for
// runtime failures; every one-shot command prints a plain "lambe: ..." line.
func report(name string, args []string, err error, stderr io.Writer) int {
	if err == nil {
		return exitOK
	}
	if errors.Is(err, errDoctorFailed) {
		return exitFailure
	}
	var usage usageError
	if errors.As(err, &usage) || isFlagError(err) {
		return reportUsage(stderr, err, usageTopic(name, args))
	}
	if name == "run" || name == "mcp" {
		logging.Default().Error("lambe "+name+" failed", "error", err.Error())
	} else {
		say(stderr, "lambe: %s\n", err)
	}
	return exitCodeFor(err)
}

// usageTopic is the command name to suggest in a usage-error hint.
func usageTopic(name string, args []string) string {
	words := append([]string{name}, nonFlagPrefix(args)...)
	if t, ok := findTopic(words); ok {
		return t.name
	}
	return name
}

func reportUsage(stderr io.Writer, err error, topic string) int {
	msg := strings.TrimPrefix(err.Error(), "usage: ")
	say(stderr, "lambe: %s\n", firstLine(msg))
	if topic == "" {
		say(stderr, "run 'lambe help' for usage\n")
	} else {
		say(stderr, "run 'lambe help %s' for usage\n", topic)
	}
	return exitUsage
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// parseFlags parses args into fs with the flag package's own output
// silenced; failures come back as usageError.
func parseFlags(fs *flag.FlagSet, args []string) error {
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		return usageError{err}
	}
	return nil
}

// noArgs rejects leftover positional arguments.
func noArgs(fs *flag.FlagSet) error {
	if fs.NArg() != 0 {
		return usageError{fmt.Errorf("unexpected arguments: %v", fs.Args())}
	}
	return nil
}
