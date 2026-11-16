package config

import (
	"errors"
	"os"
	"path/filepath"
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
