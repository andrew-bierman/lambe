package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
)

// EnvPrefix is the prefix every lambe environment variable carries.
const EnvPrefix = "LAMBE_"

// Defaults for every tunable the supervisor reads. The heartbeat default is
// one third of the assumed 450s heartbeat TTL.
const (
	defaultTickSeconds        = 10
	defaultAttemptCap         = 8
	defaultSilenceTimeout     = 10 * time.Minute
	defaultTurnTimeout        = 1 * time.Hour
	defaultHeartbeatInterval  = 150 * time.Second
	defaultHeartbeatTTLSecond = 450
)

// Supervisor holds the watchdog tuning knobs.
type Supervisor struct {
	TickSeconds        int
	AttemptCap         int
	SilenceTimeout     time.Duration
	TurnTimeout        time.Duration
	HeartbeatInterval  time.Duration
	HeartbeatTTLSecond int
}

// Tracker names one Beads source lambe may consult.
type Tracker struct {
	Name string
	Type string // "hub" or "repo"
	Path string
}

// ACP selects a known ACP agent or an explicit argv (command takes precedence).
type ACP struct {
	Agent   string
	Command []string
}

// Config is the fully resolved daemon configuration.
type Config struct {
	// WorkspaceRoot overrides the per-issue workspace directory; empty uses the state directory.
	WorkspaceRoot string

	Supervisor Supervisor
	// Harness selects "pi", "codex", or a configurable "acp" backend.
	Harness  string
	ACP      ACP
	Trackers []Tracker
	// WorkflowPath is the resolved WORKFLOW.md location: an explicit
	// workflow_path override if set, otherwise the first existing candidate
	// from DefaultWorkflowPath's search order.
	WorkflowPath string
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
		},
		WorkflowPath:  strings.TrimSpace(k.String("workflow_path")),
		WorkspaceRoot: strings.TrimSpace(k.String("workspace.root")),
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
		cfg.WorkflowPath = DefaultWorkflowPath(cfg.Trackers)
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
		"supervisor": map[string]any{
			"tick_seconds":         defaultTickSeconds,
			"attempt_cap":          defaultAttemptCap,
			"silence_timeout":      defaultSilenceTimeout.String(),
			"turn_timeout":         defaultTurnTimeout.String(),
			"heartbeat_interval":   defaultHeartbeatInterval.String(),
			"heartbeat_ttl_second": defaultHeartbeatTTLSecond,
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

func validate(cfg *Config) []error {
	var errs []error
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
	if s.HeartbeatInterval <= 0 {
		errs = append(errs, fmt.Errorf("supervisor.heartbeat_interval must be positive, got %s", s.HeartbeatInterval))
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

// Summary renders the resolved config for startup logs.
func (c *Config) Summary() string {
	var b strings.Builder
	b.WriteString("tick_seconds=" + strconv.Itoa(c.Supervisor.TickSeconds))
	b.WriteString(" attempt_cap=" + strconv.Itoa(c.Supervisor.AttemptCap))
	b.WriteString(" silence_timeout=" + c.Supervisor.SilenceTimeout.String())
	b.WriteString(" turn_timeout=" + c.Supervisor.TurnTimeout.String())
	b.WriteString(" heartbeat_interval=" + c.Supervisor.HeartbeatInterval.String())
	b.WriteString(" trackers=" + strconv.Itoa(len(c.Trackers)))
	return b.String()
}
