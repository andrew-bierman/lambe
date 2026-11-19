package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/x/term"

	"github.com/andrew-bierman/lambe/internal/daemon"
	"github.com/andrew-bierman/lambe/internal/symphony"
)

func TestTerminalTextCannotEmitEscapeSequences(t *testing.T) {
	text := terminalText("hello\n\x1b]52;c;secret\x07\u009b2J\r\x00world\t🦖")
	if strings.ContainsAny(text, "\x1b\x07\u009b\r\x00") || !strings.Contains(text, "hello\n") || !strings.Contains(text, "world\t🦖") {
		t.Fatalf("unsafe or damaged terminal text: %q", text)
	}
}

func TestFollowTailUsesByteOffsetAfterWrap(t *testing.T) {
	tail := strings.Repeat("old ", 1000) + "tool started: read\n"
	if got := tailDelta(tail, int64(len("tool started: read\n"))); got != "tool started: read\n" {
		t.Fatalf("repeated wrapped output: %q", got)
	}
	if got := tailDelta(tail, 0); got != "" {
		t.Fatal("phase-only change repeated output")
	}
	if got := tailDelta(tail, -1); got != tail {
		t.Fatal("new daemon lost its initial output")
	}
}

func TestBareLiveViewUsesInitializedTrackerNamespace(t *testing.T) {
	isolatedConfigDir(t)
	state := t.TempDir()
	t.Setenv("LAMBE_STATE_DIR", state)
	project := t.TempDir()
	t.Chdir(project)
	if err := os.Mkdir(".beads", 0o700); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runInit([]string{"-non-interactive"}, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := parseCommon(nil)
	if err != nil {
		t.Fatal(err)
	}
	path := daemon.LivePath(state, cfg)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(daemon.LiveSnapshot{Runs: []daemon.LiveRun{{Progress: symphony.Progress{IssueID: "initialized-tracker-proof"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())
	out.Reset()
	if err := runLive([]string{"-json"}, false, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "initialized-tracker-proof") {
		t.Fatal("live view selected a different tracker namespace")
	}
}

func TestLiveViewCanWaitForDaemonStartupAndQuit(t *testing.T) {
	if !term.IsTerminal(os.Stdout.Fd()) {
		t.Skip("TUI requires an interactive terminal")
	}
	isolatedConfigDir(t)
	t.Setenv("LAMBE_STATE_DIR", t.TempDir())
	t.Chdir(t.TempDir())
	if err := os.Mkdir(".beads", 0o700); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runInit([]string{"-non-interactive"}, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := runLive(nil, true, strings.NewReader("q"), &out); err != nil {
		t.Fatalf("view exited before daemon startup: %v", err)
	}
	if !strings.Contains(out.String(), "Waiting for lambe daemon") {
		t.Fatal("startup wait was invisible")
	}
}

func TestLiveCommandHelpAndFlagIsolation(t *testing.T) {
	for _, tc := range []struct {
		name string
		tui  bool
		args []string
		err  string
	}{
		{name: "dashboard help", tui: true, args: []string{"-help"}},
		{name: "logs help", args: []string{"-help"}},
		{name: "demo rejects config", tui: true, args: []string{"-demo", "-config", "unused.yaml"}, err: "-demo requires"},
		{name: "dashboard rejects logs flags", tui: true, args: []string{"-json"}, err: "flag provided but not defined"},
		{name: "logs rejects demo flag", args: []string{"-demo"}, err: "flag provided but not defined"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			err := runLive(tc.args, tc.tui, strings.NewReader(""), &out)
			if tc.err == "" && err != nil {
				t.Fatal(err)
			}
			if tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)) {
				t.Fatalf("error = %v, want %q", err, tc.err)
			}
		})
	}
}
