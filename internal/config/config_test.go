package config

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "lambe.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDefaultsApplyWithNoOverrides(t *testing.T) {
	cfg, err := Load(Options{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Supervisor.TickSeconds != 10 {
		t.Errorf("tick_seconds: got %d, want 10", cfg.Supervisor.TickSeconds)
	}
	if cfg.Supervisor.AttemptCap != 8 {
		t.Errorf("attempt_cap: got %d, want 8", cfg.Supervisor.AttemptCap)
	}
	if cfg.Supervisor.SilenceTimeout != 10*time.Minute {
		t.Errorf("silence_timeout: got %s, want 10m", cfg.Supervisor.SilenceTimeout)
	}
	if cfg.Supervisor.TurnTimeout != time.Hour {
		t.Errorf("turn_timeout: got %s, want 1h", cfg.Supervisor.TurnTimeout)
	}
	if cfg.Supervisor.HeartbeatInterval != 150*time.Second {
		t.Errorf("heartbeat_interval: got %s, want 150s", cfg.Supervisor.HeartbeatInterval)
	}
	if len(cfg.Trackers) != 1 || cfg.Trackers[0] != (Tracker{Name: "local", Type: "repo", Path: "."}) {
		t.Errorf("default tracker: got %+v, want current directory", cfg.Trackers)
	}
}

func TestPrecedenceFileEnvFlag(t *testing.T) {
	file := writeTemp(t, `
supervisor:
  tick_seconds: 20
  attempt_cap: 3
  turn_timeout: 2h
`)
	env := []string{
		"LAMBE_SUPERVISOR_TICK_SECONDS=30",
		"LAMBE_SUPERVISOR_ATTEMPT_CAP=4",
	}
	flags := map[string]string{
		"supervisor.tick_seconds": "40",
	}

	cfg, err := Load(Options{File: file, Env: env, Flags: flags})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Flag beats env and file.
	if cfg.Supervisor.TickSeconds != 40 {
		t.Errorf("flag should win: tick_seconds got %d, want 40", cfg.Supervisor.TickSeconds)
	}
	// Env beats file.
	if cfg.Supervisor.AttemptCap != 4 {
		t.Errorf("env should beat file: attempt_cap got %d, want 4", cfg.Supervisor.AttemptCap)
	}
	// File beats default.
	if cfg.Supervisor.TurnTimeout != 2*time.Hour {
		t.Errorf("file should beat default: turn_timeout got %s, want 2h", cfg.Supervisor.TurnTimeout)
	}
}

func TestFileAloneOverridesDefault(t *testing.T) {
	file := writeTemp(t, `
supervisor:
  silence_timeout: 3m
trackers:
  - name: main
    type: repo
    path: /srv/beads
`)
	cfg, err := Load(Options{File: file})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Supervisor.SilenceTimeout != 3*time.Minute {
		t.Errorf("silence_timeout: got %s, want 3m", cfg.Supervisor.SilenceTimeout)
	}
	if len(cfg.Trackers) != 1 || cfg.Trackers[0].Name != "main" || cfg.Trackers[0].Type != "repo" || cfg.Trackers[0].Path != "/srv/beads" {
		t.Errorf("trackers: got %+v", cfg.Trackers)
	}
}

func TestWorkflowPathExplicitOverride(t *testing.T) {
	file := writeTemp(t, `
workflow_path: /tmp/elsewhere/WORKFLOW.md
`)
	cfg, err := Load(Options{File: file})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.WorkflowPath != "/tmp/elsewhere/WORKFLOW.md" {
		t.Errorf("workflow_path: got %q, want explicit override honored", cfg.WorkflowPath)
	}
}

func TestDefaultWorkflowPathPrecedence(t *testing.T) {
	home := isolatedConfigDir(t)
	t.Chdir(t.TempDir())
	trackerDir := t.TempDir()
	trackers := []Tracker{{Name: "local", Type: "repo", Path: trackerDir}}

	// No candidate exists: stable cwd-relative fallback.
	if got := DefaultWorkflowPath(trackers); got != "WORKFLOW.md" {
		t.Errorf("no candidates: got %q, want WORKFLOW.md", got)
	}

	// Tracker candidate only.
	trackerWorkflow := filepath.Join(trackerDir, "WORKFLOW.md")
	if err := os.WriteFile(trackerWorkflow, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := DefaultWorkflowPath(trackers); got != trackerWorkflow {
		t.Errorf("tracker candidate: got %q, want %q", got, trackerWorkflow)
	}

	// cwd beats the tracker candidate.
	cwdWorkflow := filepath.Join(t.TempDir(), "WORKFLOW.md")
	t.Chdir(filepath.Dir(cwdWorkflow))
	if err := os.WriteFile("WORKFLOW.md", []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := DefaultWorkflowPath(trackers); got != cwdWorkflow {
		t.Errorf("cwd should beat tracker: got %q, want %q", got, cwdWorkflow)
	}

	// User config dir candidate when cwd and tracker have none.
	t.Chdir(t.TempDir())
	if err := os.Remove(trackerWorkflow); err != nil {
		t.Fatal(err)
	}
	userWorkflow := filepath.Join(home, "lambe", "WORKFLOW.md")
	if err := os.MkdirAll(filepath.Dir(userWorkflow), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(userWorkflow, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := DefaultWorkflowPath(trackers); got != userWorkflow {
		t.Errorf("user config candidate: got %q, want %q", got, userWorkflow)
	}
}

func TestDefaultPathUnderUserConfigDir(t *testing.T) {
	home := isolatedConfigDir(t)
	want := filepath.Join(home, "lambe", "config.yaml")
	if got := DefaultPath(); got != want {
		t.Errorf("DefaultPath: got %q, want %q", got, want)
	}
}

func TestInvalidConfigReportsAllErrors(t *testing.T) {
	file := writeTemp(t, `
supervisor:
  tick_seconds: 0
  attempt_cap: 0
  silence_timeout: -1s
trackers:
  - name: bad
    type: s3
  - name: nopath
    type: repo
`)
	_, err := Load(Options{File: file})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	for _, want := range []string{
		"tick_seconds", "attempt_cap", "silence_timeout",
		"tracker \"bad\"", "type", "tracker \"nopath\"", "path",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q: %v", want, err)
		}
	}
	if !errors.As(err, new(*errorCollection)) && !strings.Contains(err.Error(), "; ") {
		t.Errorf("error should aggregate all problems: %v", err)
	}
}

// isolatedConfigDir keeps platform-specific defaults inside the test's temporary home.
func isolatedConfigDir(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("APPDATA", home)
	dir, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(home, dir)
	if err != nil || !filepath.IsLocal(rel) {
		t.Fatalf("user config directory %q escapes isolated home %q", dir, home)
	}
	return dir
}

func TestHarnessAndACPConfiguration(t *testing.T) {
	for _, tt := range []struct{ name, body, wantErr string }{
		{"codex", "harness: codex", ""},
		{"gemini", "harness: acp\nacp:\n  agent: gemini", ""},
		{"unknown", "harness: typo", "harness must"},
		{"argv-string", "harness: acp\nacp:\n  command: opencode acp", "YAML list"},
		{"argv-empty", "harness: acp\nacp:\n  command: []", "executable"},
		{"argv-invalid", "harness: acp\nacp:\n  command: [opencode, 42]", "acp.command[1]"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Load(Options{File: writeTemp(t, tt.body), Env: []string{}})
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("configuration error = %v", err)
				}
				return
			}
			if err != nil || cfg == nil {
				t.Fatalf("valid harness rejected: %v", err)
			}
		})
	}
	file := writeTemp(t, "harness: acp\nacp:\n  command: [./bin/agent, '--acp', 'argument with spaces']")
	cfg, err := Load(Options{File: file, Env: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.ACP.Command) != 3 || cfg.ACP.Command[0] != filepath.Join(filepath.Dir(file), "bin", "agent") || cfg.ACP.Command[2] != "argument with spaces" {
		t.Fatalf("argv was split or resolved outside configuration: %v", cfg.ACP.Command)
	}
}

func TestACPCommandRejectsNULBeforeLaunching(t *testing.T) {
	file := writeTemp(t, `harness: acp
acp:
  command: ["agent", "bad\0arg"]
`)
	_, err := Load(Options{File: file, Env: []string{}})
	if err == nil || !strings.Contains(err.Error(), "acp.command[1]") {
		t.Fatalf("NUL argv was not rejected at configuration loading: %v", err)
	}
}

func TestWorkspaceRootResolvesRelativeToConfigurationAndHonorsEnvironment(t *testing.T) {
	path := writeTemp(t, "workspace:\n  root: isolated-workspaces\n")
	cfg, err := Load(Options{File: path, Env: []string{}})
	if err != nil || cfg.WorkspaceRoot != filepath.Join(filepath.Dir(path), "isolated-workspaces") {
		t.Fatalf("wrong workspace root: %+v %v", cfg, err)
	}
	override := t.TempDir()
	cfg, err = Load(Options{File: path, Env: []string{"LAMBE_WORKSPACE_ROOT=" + override}})
	if err != nil || cfg.WorkspaceRoot != override {
		t.Fatalf("workspace environment override lost: %+v %v", cfg, err)
	}
}

func TestWorkspaceRootRejectsNUL(t *testing.T) {
	if _, err := Load(Options{Env: []string{"LAMBE_WORKSPACE_ROOT=bad\x00path"}}); err == nil {
		t.Fatal("invalid workspace path accepted")
	}
}

func TestLabelsDefaultEmpty(t *testing.T) {
	cfg, err := Load(Options{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Labels) != 0 {
		t.Errorf("default labels should be empty, got %v", cfg.Labels)
	}
}

func TestLabelsFromConfigFile(t *testing.T) {
	file := writeTemp(t, `labels:
  - "lambe:babysit"
  - "priority:high"
`)
	cfg, err := Load(Options{File: file, Env: []string{}})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Labels) != 2 || cfg.Labels[0] != "lambe:babysit" || cfg.Labels[1] != "priority:high" {
		t.Errorf("labels: got %v, want [lambe:babysit priority:high]", cfg.Labels)
	}
}

func TestLabelsRejectEmptyString(t *testing.T) {
	file := writeTemp(t, `labels:
  - "lambe:babysit"
  - ""
`)
	_, err := Load(Options{File: file, Env: []string{}})
	if err == nil {
		t.Fatal("empty label string should be rejected")
	}
}

func TestLabelsRejectNonList(t *testing.T) {
	file := writeTemp(t, `labels: "not-a-list"`)
	_, err := Load(Options{File: file, Env: []string{}})
	if err == nil {
		t.Fatal("non-list labels should be rejected")
	}
}

func TestNamedWorkflowConfig(t *testing.T) {
	for _, tc := range []struct {
		name      string
		body      string
		wantError bool
	}{
		{"valid", "workflows:\n  - name: General\n    label: lambe:general\n    path: GENERAL.md\n    gate_dir: gates/general\n", false},
		{"invalid collection", "workflows: typo\n", true},
		{"duplicate label", "workflows:\n  - {name: A, label: route, path: A.md}\n  - {name: B, label: Route, path: B.md}\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "lambe.yaml")
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(Options{File: path, Env: []string{}})
			if tc.wantError {
				if err == nil {
					t.Fatal("expected invalid workflow configuration to fail")
				}
				return
			}
			if err != nil || len(cfg.Workflows) != 1 || cfg.WorkflowPath != filepath.Join(dir, "GENERAL.md") ||
				cfg.Workflows[0].GateDir != filepath.Join(dir, "gates", "general") {
				t.Fatalf("unexpected workflow config: %+v, %v", cfg, err)
			}
		})
	}
}

func TestStateAPIDisabledByDefault(t *testing.T) {
	cfg, err := Load(Options{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.StateAPI.Enable {
		t.Error("stateapi must be an explicit opt-in: default is disabled")
	}
	if cfg.StateAPI.Addr != "127.0.0.1:0" {
		t.Errorf("stateapi.addr default: got %q, want ephemeral loopback", cfg.StateAPI.Addr)
	}
	if s := cfg.Summary(); !strings.Contains(s, "stateapi=off") {
		t.Errorf("Summary should report the disabled surface: %q", s)
	}
}

func TestStateAPIPrecedenceFileEnvFlag(t *testing.T) {
	file := writeTemp(t, `
stateapi:
  enable: false
  addr: 127.0.0.1:9999
`)
	env := []string{
		"LAMBE_STATEAPI_ENABLE=true",
		"LAMBE_STATEAPI_ADDR=127.0.0.1:0",
	}
	// Env beats file on both keys.
	cfg, err := Load(Options{File: file, Env: env})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.StateAPI.Enable {
		t.Error("env should enable the surface over the file's opt-out")
	}
	if cfg.StateAPI.Addr != "127.0.0.1:0" {
		t.Errorf("env should override the file addr: got %q", cfg.StateAPI.Addr)
	}
	// Flags beat env.
	cfg, err = Load(Options{File: file, Env: env, Flags: map[string]string{"stateapi.addr": "127.0.0.1:1"}})
	if err != nil {
		t.Fatalf("Load with flag: %v", err)
	}
	if cfg.StateAPI.Addr != "127.0.0.1:1" {
		t.Errorf("flag should win: addr got %q, want 127.0.0.1:1", cfg.StateAPI.Addr)
	}
	// File alone beats defaults.
	cfg, err = Load(Options{File: file, Env: []string{}})
	if err != nil || cfg.StateAPI.Enable || cfg.StateAPI.Addr != "127.0.0.1:9999" {
		t.Errorf("file overrides lost: %+v, %v", cfg.StateAPI, err)
	}
	// Summary surfaces the configured addr when enabled, never a token.
	enabled := writeTemp(t, "stateapi:\n  enable: true\n  addr: 127.0.0.1:8137\n")
	cfg, err = Load(Options{File: enabled, Env: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	if s := cfg.Summary(); !strings.Contains(s, "stateapi=on addr=127.0.0.1:8137") {
		t.Errorf("Summary should surface the configured addr: %q", s)
	}
}

func TestStateAPIRejectsNonLoopbackWithAllErrorsCollected(t *testing.T) {
	file := writeTemp(t, `
supervisor:
  tick_seconds: 0
stateapi:
  enable: true
  addr: 0.0.0.0:8080
`)
	_, err := Load(Options{File: file, Env: []string{}})
	if err == nil {
		t.Fatal("non-loopback bind must be rejected")
	}
	for _, want := range []string{"tick_seconds", "stateapi.addr", "loopback", "; "} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q: %v", want, err)
		}
	}
}

func TestStateAPIAddrValidationTable(t *testing.T) {
	for _, tt := range []struct {
		name  string
		addr  string
		valid bool
	}{
		{"ephemeral loopback", "127.0.0.1:0", true},
		{"fixed loopback port", "127.0.0.1:8137", true},
		{"loopback /8 range", "127.5.5.5:1", false},
		{"alternate loopback", "127.0.0.2:1", false},
		{"ipv6 loopback", "[::1]:0", false},
		{"localhost name", "localhost:8080", false},
		{"wildcard host", ":8080", false},
		{"routable address", "10.0.0.1:8080", false},
		{"all interfaces", "0.0.0.0:8080", false},
		{"missing port", "127.0.0.1", false},
		{"hostname", "dashboard.internal:8080", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			file := writeTemp(t, "stateapi:\n  enable: true\n  addr: "+strconv.Quote(tt.addr)+"\n")
			cfg, err := Load(Options{File: file, Env: []string{}})
			if tt.valid {
				if err != nil || cfg.StateAPI.Addr != tt.addr {
					t.Fatalf("valid addr rejected: %q, %v", tt.addr, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "stateapi.addr") {
				t.Fatalf("addr %q accepted: %v", tt.addr, err)
			}
		})
	}
}

func TestStateAPIAddrIgnoredWhenDisabled(t *testing.T) {
	// Opt-in doctrine: with the surface disabled, a suspicious addr is
	// inert configuration, not an error to fail startup on.
	file := writeTemp(t, "stateapi:\n  enable: false\n  addr: 10.9.9.9:1\n")
	cfg, err := Load(Options{File: file, Env: []string{}})
	if err != nil || cfg.StateAPI.Enable {
		t.Fatalf("disabled surface must not fail validation: %+v, %v", cfg, err)
	}
}

func TestIdentity(t *testing.T) {
	host := func() (string, error) { return "box-7.lan", nil }
	tests := []struct {
		name        string
		env         []string
		file        string
		wantMachine string
		wantActor   string
	}{
		{"hostname fallback", nil, "", "box-7", "lambe@box-7"},
		{"env machine derives actor", []string{"LAMBE_MACHINE=east"}, "", "east", "lambe@east"},
		{"file actor", nil, "machine: east\nactor: lambe@east-2\n", "east", "lambe@east-2"},
		{"env beats file", []string{"LAMBE_ACTOR=env-actor"}, "actor: file-actor\n", "box-7", "env-actor"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := Options{Env: tt.env}
			if opts.Env == nil {
				opts.Env = []string{}
			}
			if tt.file != "" {
				opts.File = writeTemp(t, tt.file)
			}
			cfg, err := Load(opts)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			machine, actor := cfg.Identity(host)
			if machine != tt.wantMachine || actor != tt.wantActor {
				t.Errorf("Identity = (%q, %q), want (%q, %q)", machine, actor, tt.wantMachine, tt.wantActor)
			}
		})
	}
}

func TestActorRejectsWhitespace(t *testing.T) {
	if _, err := Load(Options{Env: []string{"LAMBE_ACTOR=bad actor"}}); err == nil {
		t.Fatal("actor with whitespace must fail validation")
	}
}

func TestStallAndTimeoutKnobs(t *testing.T) {
	cfg, err := Load(Options{Env: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Supervisor.StallAfter != 5*time.Minute {
		t.Errorf("stall_after default = %s, want 5m", cfg.Supervisor.StallAfter)
	}
	if cfg.Supervisor.ProbeWindow != 0 || cfg.Gate.CheckTimeout != 0 {
		t.Errorf("probe_window / gate.check_timeout default to unset, got %s / %s", cfg.Supervisor.ProbeWindow, cfg.Gate.CheckTimeout)
	}
	file := writeTemp(t, "supervisor:\n  stall_after: 2m\n  probe_window: 45s\ngate:\n  check_timeout: 20m\n")
	cfg, err = Load(Options{File: file, Env: []string{"LAMBE_GATE_CHECK_TIMEOUT=30m"}})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Supervisor.StallAfter != 2*time.Minute || cfg.Supervisor.ProbeWindow != 45*time.Second {
		t.Errorf("file knobs: stall_after=%s probe_window=%s", cfg.Supervisor.StallAfter, cfg.Supervisor.ProbeWindow)
	}
	if cfg.Gate.CheckTimeout != 30*time.Minute {
		t.Errorf("env must override file: gate.check_timeout = %s, want 30m", cfg.Gate.CheckTimeout)
	}
	for _, bad := range []string{"supervisor.stall_after: 0s", "supervisor.probe_window: -1s", "gate.check_timeout: -5m"} {
		key, val, _ := strings.Cut(bad, ": ")
		section, name, _ := strings.Cut(key, ".")
		if _, err := Load(Options{File: writeTemp(t, section+":\n  "+name+": "+val+"\n"), Env: []string{}}); err == nil {
			t.Errorf("%s accepted, want a validation error", bad)
		}
func TestAutoReclaimDefaultsOffAndValidatesGrace(t *testing.T) {
	cfg, err := Load(Options{Env: []string{}})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Supervisor.AutoReclaim {
		t.Error("auto_reclaim must default to false")
	}
	if cfg.Supervisor.ReclaimGrace != 10*time.Minute {
		t.Errorf("reclaim_grace: got %s, want 10m", cfg.Supervisor.ReclaimGrace)
	}

	on, err := Load(Options{File: writeTemp(t, "supervisor:\n  auto_reclaim: true\n"), Env: []string{}})
	if err != nil || !on.Supervisor.AutoReclaim {
		t.Fatalf("auto_reclaim: true: got %+v, %v", on, err)
	}

	_, err = Load(Options{File: writeTemp(t, "supervisor:\n  auto_reclaim: true\n  reclaim_grace: 5s\n"), Env: []string{}})
	if err == nil || !strings.Contains(err.Error(), "reclaim_grace") {
		t.Errorf("grace below tick interval with auto_reclaim: got %v, want reclaim_grace error", err)
	}
	// A short grace is only a problem when the reaper is on.
	if _, err := Load(Options{File: writeTemp(t, "supervisor:\n  reclaim_grace: 5s\n"), Env: []string{}}); err != nil {
		t.Errorf("short grace with auto_reclaim off: %v", err)
	}
}
