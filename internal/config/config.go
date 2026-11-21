package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
)

// EnvPrefix is the prefix every lambe environment variable carries.
const EnvPrefix = "LAMBE_"

// DefaultStallAfter is the stall threshold when none is configured.
const DefaultStallAfter = 5 * time.Minute

// Defaults for every tunable the supervisor reads. The heartbeat default is
// one third of the assumed 450s heartbeat TTL. The state surface binds an
// ephemeral loopback port and stays disabled until the operator opts in.
const (
	defaultTickSeconds        = 10
	defaultAttemptCap         = 8
	defaultSilenceTimeout     = 10 * time.Minute
	defaultTurnTimeout        = 1 * time.Hour
	defaultHeartbeatInterval  = 150 * time.Second
	defaultHeartbeatTTLSecond = 450
	defaultStallAfter         = DefaultStallAfter
	defaultReclaimGrace       = 10 * time.Minute
	defaultStateAPIAddr       = "127.0.0.1:0"
)

// Supervisor holds the watchdog tuning knobs.
type Supervisor struct {
	TickSeconds        int
	AttemptCap         int
	SilenceTimeout     time.Duration
	TurnTimeout        time.Duration
	HeartbeatInterval  time.Duration
	HeartbeatTTLSecond int
	// AutoReclaim makes the daemon run `bd reclaim` every tick, reverting
	// in_progress issues whose lease expired more than ReclaimGrace ago back
	// to open. Default off: enable it only for a multi-host deployment
	// sharing one Dolt server (see docs/MULTI-MACHINE.md).
	AutoReclaim  bool
	ReclaimGrace time.Duration
	// StallAfter is the single stall threshold: a running run with no agent
	// activity for this long is "stalled" to the state API, dashboard,
	// TUI, and doctor. It is observation only; the watchdog still acts at
	// SilenceTimeout.
	StallAfter time.Duration
	// ProbeWindow bounds the wait for any response after the Pi steer
	// probe; zero uses the driver default (30s). Pi harness only.
	ProbeWindow time.Duration
}

// Gate holds completion-gate tuning.
type Gate struct {
	// CheckTimeout bounds each external gate check (a test run, a CI
	// poll). Zero derives it from silence_timeout/2, floored at one minute.
	CheckTimeout time.Duration
}

// Tracker names one Beads source lambe may consult.
type Tracker struct {
	Name string
	Type string // "hub" or "repo"
	Path string
}

// StateAPI configures the optional local HTTP state surface (Symphony
// §13.7). The surface is an explicit opt-in: until Enable is true the
// daemon serves nothing, exactly as before.
type StateAPI struct {
	// Enable serves the bearer-guarded API on loopback. The default is
	// false: no listener exists until the operator asks for one.
	Enable bool
	// Addr is the loopback host:port to bind; port 0 selects an ephemeral
	// port. Only loopback IP literals are accepted; validation rejects
	// anything wider or routable.
	Addr string
}

// ACP selects a known ACP agent or an explicit argv (command takes precedence).
type ACP struct {
	Agent   string
	Command []string
}

// NamedWorkflow maps a Beads routing label to a workflow file.
type NamedWorkflow struct {
	Name    string
	Label   string
	Path    string
	GateDir string
}

// Config is the fully resolved daemon configuration.
type Config struct {
	// WorkspaceRoot overrides the per-issue workspace directory; empty uses the state directory.
	WorkspaceRoot string

	// Machine names this host for dispatch affinity (lambe/machine:<name>
	// labels). Empty falls back to the hostname's first label; see Identity.
	Machine string
	// Actor is the Beads identity stamped on claims and heartbeats. Every
	// host sharing one tracker MUST have a distinct actor: bd treats a
	// repeated claim by the same actor as idempotent, so two hosts with the
	// same actor would both "win" the same issue. Empty derives
	// "lambe@<machine>"; see Identity.
	Actor string

	Supervisor Supervisor
	Gate       Gate
	// Harness selects "pi", "codex", or a configurable "acp" backend.
	Harness  string
	ACP      ACP
	Trackers []Tracker
	// WorkflowPath is the resolved WORKFLOW.md location: an explicit
	// workflow_path override if set, otherwise the first existing candidate
	// from DefaultWorkflowPath's search order.
	WorkflowPath string
	Workflows    []NamedWorkflow
	// StateAPI configures the local HTTP state surface; disabled by
	// default.
	StateAPI StateAPI
	// Labels is the opt-in work lane: when non-empty, the daemon claims
	// only issues that carry every listed label. An empty slice (the
	// default) dispatches every ready issue, preserving the pre-filter
	// behavior. On a shared tracker this keeps lambe's workflow isolated
	// from other agents' tasks.
	Labels []string
}

// Options controls where Load reads overrides from. Env and Flags are
// injectable for testability; when Env is nil the process environment is
// used. Flags maps dotted config keys (supervisor.tick_seconds) to raw
// string values exactly as parsed from the command line.
type Options struct {
	File  string
	Env   []string
	Flags map[string]string
}

// errorCollection aggregates every validation problem found in one pass.
type errorCollection struct {
	errs []error
}

func (e *errorCollection) Error() string {
	parts := make([]string, len(e.errs))
	for i, err := range e.errs {
		parts[i] = err.Error()
	}
	return strings.Join(parts, "; ")
}

// Load resolves configuration as defaults < file < env < flags and validates
// the result, reporting every problem at once.
func Load(opts Options) (*Config, error) {
	k := koanf.New(".")

	if err := k.Load(defaultsProvider(), nil); err != nil {
		return nil, err
	}

	if opts.File != "" {
		if err := k.Load(file.Provider(opts.File), yaml.Parser()); err != nil {
			return nil, fmt.Errorf("config file: %w", err)
		}
	}

	envVars := opts.Env
	if envVars == nil {
		envVars = os.Environ()
	}
	if err := k.Load(envProvider(envVars), nil); err != nil {
		return nil, err
	}

	if len(opts.Flags) > 0 {
		if err := k.Load(flagProvider(opts.Flags), nil); err != nil {
			return nil, err
		}
	}

	harness := k.String("harness")
	if harness == "" {
		harness = "pi"
	}

	cfg := &Config{
		Harness: harness,
		ACP:     ACP{Agent: k.String("acp.agent")},
		Supervisor: Supervisor{
			TickSeconds:        k.Int("supervisor.tick_seconds"),
			AttemptCap:         k.Int("supervisor.attempt_cap"),
			SilenceTimeout:     k.Duration("supervisor.silence_timeout"),
			TurnTimeout:        k.Duration("supervisor.turn_timeout"),
			HeartbeatInterval:  k.Duration("supervisor.heartbeat_interval"),
			HeartbeatTTLSecond: k.Int("supervisor.heartbeat_ttl_second"),
			StallAfter:         k.Duration("supervisor.stall_after"),
			ProbeWindow:        k.Duration("supervisor.probe_window"),
			AutoReclaim:        k.Bool("supervisor.auto_reclaim"),
			ReclaimGrace:       k.Duration("supervisor.reclaim_grace"),
		},
		Gate:          Gate{CheckTimeout: k.Duration("gate.check_timeout")},
		Machine:       strings.TrimSpace(k.String("machine")),
		Actor:         strings.TrimSpace(k.String("actor")),
		WorkflowPath:  strings.TrimSpace(k.String("workflow_path")),
		WorkspaceRoot: strings.TrimSpace(k.String("workspace.root")),
		StateAPI: StateAPI{
			Enable: k.Bool("stateapi.enable"),
			Addr:   strings.TrimSpace(k.String("stateapi.addr")),
		},
	}

	if cfg.ACP.Agent == "" {
		cfg.ACP.Agent = "opencode"
	}
	if raw := k.Get("acp.command"); raw != nil {
		items, ok := raw.([]any)
		if !ok {
			return nil, fmt.Errorf("acp.command must be a YAML list of arguments")
		}
		for i, item := range items {
			arg, ok := item.(string)
			if !ok || strings.IndexByte(arg, 0) >= 0 || (i == 0 && strings.TrimSpace(arg) == "") {
				return nil, fmt.Errorf("acp.command[%d] must be a string without NUL (executable must not be empty)", i)
			}
			cfg.ACP.Command = append(cfg.ACP.Command, arg)
		}
		if len(items) == 0 {
			return nil, fmt.Errorf("acp.command must contain an executable")
		}
		if strings.ContainsRune(cfg.ACP.Command[0], filepath.Separator) && !filepath.IsAbs(cfg.ACP.Command[0]) {
			base := "."
			if opts.File != "" {
				base = filepath.Dir(opts.File)
			}
			absolute, err := filepath.Abs(filepath.Join(base, cfg.ACP.Command[0]))
			if err != nil {
				return nil, err
			}
			cfg.ACP.Command[0] = absolute
		}
	}

	for _, raw := range k.Slices("trackers") {
		cfg.Trackers = append(cfg.Trackers, Tracker{
			Name: raw.String("name"),
			Type: raw.String("type"),
			Path: raw.String("path"),
		})
	}

	if raw := k.Get("labels"); raw != nil {
		items, ok := raw.([]any)
		if !ok {
			return nil, fmt.Errorf("labels must be a YAML list of strings")
		}
		for i, item := range items {
			s, ok := item.(string)
			if !ok || strings.TrimSpace(s) == "" {
				return nil, fmt.Errorf("labels[%d] must be a non-empty string", i)
			}
			cfg.Labels = append(cfg.Labels, strings.TrimSpace(s))
		}
	}

	routes := k.Slices("workflows")
	if raw := k.Get("workflows"); raw != nil {
		items, ok := raw.([]any)
		if !ok || len(items) == 0 || len(items) != len(routes) {
			return nil, fmt.Errorf("workflows must be a non-empty YAML list of workflow mappings")
		}
	}
	workflowNames, workflowLabels := map[string]bool{}, map[string]bool{}
	for _, raw := range routes {
		name := strings.TrimSpace(raw.String("name"))
		label := strings.ToLower(strings.TrimSpace(raw.String("label")))
		path := strings.TrimSpace(raw.String("path"))
		if name == "" || label == "" || path == "" || strings.ContainsAny(label, ", \t\n\r") {
			return nil, fmt.Errorf("each workflow requires a name, a single label, and a path")
		}
		if workflowNames[name] || workflowLabels[label] {
			return nil, fmt.Errorf("duplicate workflow name or label: %s (%s)", name, label)
		}
		if opts.File != "" && !filepath.IsAbs(path) {
			path = filepath.Join(filepath.Dir(opts.File), path)
		}
		gateDir := strings.TrimSpace(raw.String("gate_dir"))
		if gateDir != "" && opts.File != "" && !filepath.IsAbs(gateDir) {
			gateDir = filepath.Join(filepath.Dir(opts.File), gateDir)
		}
		workflowNames[name], workflowLabels[label] = true, true
		cfg.Workflows = append(cfg.Workflows, NamedWorkflow{Name: name, Label: label, Path: path, GateDir: gateDir})
	}
	if len(cfg.Workflows) > 0 && len(cfg.Labels) > 0 {
		return nil, fmt.Errorf("use workflow labels instead of top-level labels when workflows are configured")
	}

	if cfg.WorkspaceRoot != "" {
		if strings.IndexByte(cfg.WorkspaceRoot, 0) >= 0 {
			return nil, fmt.Errorf("workspace.root must not contain NUL")
		}
		if !filepath.IsAbs(cfg.WorkspaceRoot) && opts.File != "" {
			cfg.WorkspaceRoot = filepath.Join(filepath.Dir(opts.File), cfg.WorkspaceRoot)
		}
		absolute, err := filepath.Abs(cfg.WorkspaceRoot)
		if err != nil {
			return nil, err
		}
		cfg.WorkspaceRoot = absolute
	}

	if cfg.WorkflowPath == "" {
		if len(cfg.Workflows) > 0 {
			cfg.WorkflowPath = cfg.Workflows[0].Path
		} else {
			cfg.WorkflowPath = DefaultWorkflowPath(cfg.Trackers)
		}
	}

	if errs := validate(cfg); len(errs) > 0 {
		return nil, &errorCollection{errs: errs}
	}
	return cfg, nil
}

func defaultsProvider() *confmap.Confmap {
	return confmap.Provider(map[string]any{
		"workspace": map[string]any{"root": ""},
		"trackers":  []any{map[string]any{"name": "local", "type": "repo", "path": "."}},
		"stateapi": map[string]any{
			"enable": false,
			"addr":   defaultStateAPIAddr,
		},
		"supervisor": map[string]any{
			"tick_seconds":         defaultTickSeconds,
			"attempt_cap":          defaultAttemptCap,
			"silence_timeout":      defaultSilenceTimeout.String(),
			"turn_timeout":         defaultTurnTimeout.String(),
			"heartbeat_interval":   defaultHeartbeatInterval.String(),
			"heartbeat_ttl_second": defaultHeartbeatTTLSecond,
			"stall_after":          defaultStallAfter.String(),
			"auto_reclaim":         false,
			"reclaim_grace":        defaultReclaimGrace.String(),
		},
	}, ".")
}

// envProvider reads "KEY=VALUE" strings through the flattening rule
// documented in the package comment.
func envProvider(vars []string) koanf.Provider {
	m := make(map[string]string)
	for _, kv := range vars {
		key, value, ok := strings.Cut(kv, "=")
		if !ok || !strings.HasPrefix(key, EnvPrefix) {
			continue
		}
		confKey := envKeyToConfigKey(key)
		if confKey == "" {
			continue
		}
		m[confKey] = value
	}
	return confmap.Provider(dotify(m), ".")
}

// envKeyToConfigKey implements the documented flattening rule: drop the
// LAMBE_ prefix, lowercase, and replace only the first underscore with the
// section delimiter.
func envKeyToConfigKey(envVar string) string {
	key := strings.ToLower(strings.TrimPrefix(envVar, EnvPrefix))
	if i := strings.Index(key, "_"); i >= 0 {
		return key[:i] + "." + key[i+1:]
	}
	return key
}

func flagProvider(flags map[string]string) koanf.Provider {
	return confmap.Provider(dotify(flags), ".")
}

// dotify converts dotted keys into a nested map.
func dotify(flags map[string]string) map[string]any {
	root := map[string]any{}
	for key, value := range flags {
		nested := root
		parts := strings.Split(key, ".")
		for _, part := range parts[:len(parts)-1] {
			next, ok := nested[part].(map[string]any)
			if !ok {
				next = map[string]any{}
				nested[part] = next
			}
			nested = next
		}
		nested[parts[len(parts)-1]] = value
	}
	return root
}

// DefaultPath returns the default configuration file location,
// <user config dir>/lambe/config.yaml. It is the file `lambe init` writes
// and the implicit fallback for every subcommand's -config flag.
func DefaultPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "config.yaml"
	}
	return filepath.Join(dir, "lambe", "config.yaml")
}

// DefaultWorkflowPath resolves the WORKFLOW.md location when the
// configuration does not set workflow_path explicitly. Candidates are
// checked in order: WORKFLOW.md in the current directory, in the first
// tracker's path, and in <user config dir>/lambe/. When none exists the
// cwd-relative "WORKFLOW.md" is returned so callers can report a
// missing-file error against a stable path.
func DefaultWorkflowPath(trackers []Tracker) string {
	candidates := []string{"WORKFLOW.md"}
	if len(trackers) > 0 {
		candidates = append(candidates, filepath.Join(trackers[0].Path, "WORKFLOW.md"))
	}
	if dir, err := os.UserConfigDir(); err == nil {
		candidates = append(candidates, filepath.Join(dir, "lambe", "WORKFLOW.md"))
	}
	for _, candidate := range candidates {
		if _, err := os.Stat(candidate); err == nil {
			if abs, absErr := filepath.Abs(candidate); absErr == nil {
				return abs
			}
			return candidate
		}
	}
	return "WORKFLOW.md"
}

// Identity resolves this host's dispatch machine name and Beads actor.
// Precedence per field: config (file, LAMBE_MACHINE / LAMBE_ACTOR env,
// flags), then the hostname's first label, with the actor defaulting to
// "lambe@<machine>". hostname is injectable for tests.
func (c *Config) Identity(hostname func() (string, error)) (machine, actor string) {
	machine = c.Machine
	if machine == "" {
		host, err := hostname()
		if err != nil || host == "" {
			host = "unknown"
		}
		machine, _, _ = strings.Cut(host, ".")
	}
	actor = c.Actor
	if actor == "" {
		actor = "lambe@" + machine
	}
	return machine, actor
}

// validIdentityToken rejects empty-after-trim, whitespace, and control
// characters: the value rides in bd argv, labels, and the audit trail.
func validIdentityToken(v string) bool {
	return !strings.ContainsFunc(v, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) })
}

func validate(cfg *Config) []error {
	var errs []error
	if !validIdentityToken(cfg.Machine) {
		errs = append(errs, fmt.Errorf("machine must not contain whitespace or control characters, got %q", cfg.Machine))
	}
	if !validIdentityToken(cfg.Actor) {
		errs = append(errs, fmt.Errorf("actor must not contain whitespace or control characters, got %q", cfg.Actor))
	}
	switch cfg.Harness {
	case "pi", "codex", "acp":
	default:
		errs = append(errs, fmt.Errorf("harness must be pi, codex, or acp; got %q", cfg.Harness))
	}

	s := cfg.Supervisor
	if s.TickSeconds <= 0 {
		errs = append(errs, fmt.Errorf("supervisor.tick_seconds must be > 0, got %d", s.TickSeconds))
	}
	if s.AttemptCap < 1 {
		errs = append(errs, fmt.Errorf("supervisor.attempt_cap must be >= 1, got %d", s.AttemptCap))
	}
	if s.AttemptCap > 100 {
		errs = append(errs, fmt.Errorf("supervisor.attempt_cap must be <= 100, got %d", s.AttemptCap))
	}
	if s.SilenceTimeout <= 0 {
		errs = append(errs, fmt.Errorf("supervisor.silence_timeout must be positive, got %s", s.SilenceTimeout))
	}
	if s.TurnTimeout <= 0 {
		errs = append(errs, fmt.Errorf("supervisor.turn_timeout must be positive, got %s", s.TurnTimeout))
	}
	if s.TurnTimeout <= s.SilenceTimeout {
		errs = append(errs, fmt.Errorf("supervisor.turn_timeout (%s) must exceed silence_timeout (%s)", s.TurnTimeout, s.SilenceTimeout))
	}
	if s.StallAfter <= 0 {
		errs = append(errs, fmt.Errorf("supervisor.stall_after must be positive, got %s", s.StallAfter))
	}
	if s.ProbeWindow < 0 {
		errs = append(errs, fmt.Errorf("supervisor.probe_window must not be negative, got %s", s.ProbeWindow))
	}
	if cfg.Gate.CheckTimeout < 0 {
		errs = append(errs, fmt.Errorf("gate.check_timeout must not be negative, got %s", cfg.Gate.CheckTimeout))
	}
	if s.HeartbeatInterval <= 0 {
		errs = append(errs, fmt.Errorf("supervisor.heartbeat_interval must be positive, got %s", s.HeartbeatInterval))
	}
	if s.AutoReclaim && s.ReclaimGrace <= time.Duration(s.TickSeconds)*time.Second {
		errs = append(errs, fmt.Errorf("supervisor.reclaim_grace (%s) must exceed the tick interval (%ds): a slow sync must not read as a dead worker", s.ReclaimGrace, s.TickSeconds))
	}

	if cfg.StateAPI.Enable {
		if err := validateLoopbackAddr(cfg.StateAPI.Addr); err != nil {
			errs = append(errs, fmt.Errorf("stateapi.addr: %w", err))
		}
	}

	names := make(map[string]bool, len(cfg.Trackers))
	for i, tr := range cfg.Trackers {
		if tr.Name == "" {
			errs = append(errs, fmt.Errorf("trackers[%d]: name is required", i))
		} else if names[tr.Name] {
			errs = append(errs, fmt.Errorf("trackers[%d]: duplicate tracker name %q", i, tr.Name))
		} else {
			names[tr.Name] = true
		}
		if tr.Type != "hub" && tr.Type != "repo" {
			errs = append(errs, fmt.Errorf("tracker %q: type must be \"hub\" or \"repo\", got %q", tr.Name, tr.Type))
		}
		if tr.Path == "" {
			errs = append(errs, fmt.Errorf("tracker %q: path is required", tr.Name))
		}
	}

	return errs
}

// validateLoopbackAddr accepts only the exact loopback literal 127.0.0.1
// in host:port form. The state surface must never bind a routable,
// wildcard, or alternate loopback address: the Swift discovery contract
// and the middleware's strict Host check both pin the bound host to this
// one literal.
func validateLoopbackAddr(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("must be host:port, got %q", addr)
	}
	if host != "127.0.0.1" {
		return fmt.Errorf("must be the loopback IP address 127.0.0.1, got %q", host)
	}
	return nil
}

// Summary renders the resolved config for startup logs.
func (c *Config) Summary() string {
	var b strings.Builder
	b.WriteString("tick_seconds=" + strconv.Itoa(c.Supervisor.TickSeconds))
	b.WriteString(" attempt_cap=" + strconv.Itoa(c.Supervisor.AttemptCap))
	b.WriteString(" silence_timeout=" + c.Supervisor.SilenceTimeout.String())
	b.WriteString(" turn_timeout=" + c.Supervisor.TurnTimeout.String())
	b.WriteString(" heartbeat_interval=" + c.Supervisor.HeartbeatInterval.String())
	b.WriteString(" stall_after=" + c.Supervisor.StallAfter.String())
	b.WriteString(" auto_reclaim=" + strconv.FormatBool(c.Supervisor.AutoReclaim))
	b.WriteString(" trackers=" + strconv.Itoa(len(c.Trackers)))
	if c.StateAPI.Enable {
		b.WriteString(" stateapi=on addr=" + c.StateAPI.Addr)
	} else {
		b.WriteString(" stateapi=off")
	}
	return b.String()
}
