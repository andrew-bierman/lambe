package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/term"

	"github.com/andrew-bierman/lambe/internal/daemon"
	"github.com/andrew-bierman/lambe/internal/proc"
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

func TestLogsJSONReportsDaemonLivenessAndMachine(t *testing.T) {
	isolatedConfigDir(t)
	state := t.TempDir()
	t.Setenv("LAMBE_STATE_DIR", state)
	t.Setenv("LAMBE_MACHINE", "mini")
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
	identity, err := proc.ProcessIdentity(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		snapshot daemon.LiveSnapshot
		alive    bool
	}{
		{"running daemon", daemon.LiveSnapshot{PID: os.Getpid(), Identity: identity}, true},
		{"stopped daemon", daemon.LiveSnapshot{PID: os.Getpid(), Identity: "someone-else"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.snapshot.Runs = []daemon.LiveRun{{Progress: symphony.Progress{IssueID: "ac-1", Workflow: "Fleet"}}}
			data, err := json.Marshal(tc.snapshot)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			out.Reset()
			if err := runLive([]string{"-json"}, false, strings.NewReader(""), &out); err != nil {
				t.Fatal(err)
			}
			var got struct {
				Alive   bool
				Machine string
				Runs    []struct{ IssueID, Workflow string }
			}
			if err := json.Unmarshal(out.Bytes(), &got); err != nil {
				t.Fatalf("decode %q: %v", out.String(), err)
			}
			if got.Alive != tc.alive || got.Machine != "mini" {
				t.Errorf("Alive=%v Machine=%q, want %v and mini", got.Alive, got.Machine, tc.alive)
			}
			if len(got.Runs) != 1 || got.Runs[0].IssueID != "ac-1" || got.Runs[0].Workflow != "Fleet" {
				t.Errorf("runs = %+v, want the flat snapshot rows with their workflow", got.Runs)
			}
		})
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

func TestLogsIssueReadsArchivedRunsAndDedupes(t *testing.T) {
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
	current := daemon.LiveRun{
		Progress: symphony.Progress{IssueID: "ac-live", Title: "Ship the picker", Phase: "running", Attempt: 2, Turn: 3},
		Tail:     "latest attempt\n", TailBytes: int64(len("latest attempt\n")),
	}
	archivedSame := daemon.LiveRun{
		Progress:  symphony.Progress{IssueID: "ac-live", Title: "Ship the picker", Phase: "stopped: gate green", Attempt: 1, Turn: 9},
		UpdatedAt: time.Now().Add(-time.Hour), Tail: "first attempt tail\n",
	}
	archivedOnly := daemon.LiveRun{
		Progress:  symphony.Progress{IssueID: "ac-old", Title: "Archived run", Phase: "stopped: gate green", Attempt: 1, Turn: 4},
		UpdatedAt: time.Now().Add(-2 * time.Hour), Tail: "archived tail\n",
	}
	data, err := json.Marshal(daemon.LiveSnapshot{Runs: []daemon.LiveRun{current}, History: []daemon.LiveRun{archivedSame, archivedOnly}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name    string
		args    []string
		want    []string
		notWant []string
	}{
		{
			name:    "unfiltered shows only current runs",
			args:    nil,
			want:    []string{"ac-live", "attempt 2", "latest attempt"},
			notWant: []string{"ac-old", "Archived run", "first attempt tail"},
		},
		{
			name:    "archived issue is readable",
			args:    []string{"-issue", "ac-old"},
			want:    []string{"ac-old", "Archived run", "stopped: gate green", "archived tail"},
			notWant: nil,
		},
		{
			name:    "current attempt wins over archived attempts",
			args:    []string{"-issue", "ac-live"},
			want:    []string{"attempt 2", "latest attempt"},
			notWant: []string{"attempt 1", "first attempt tail"},
		},
		{
			name:    "unknown issue says so once",
			args:    []string{"-issue", "ac-missing"},
			want:    []string{"No current or archived output for ac-missing"},
			notWant: []string{"ac-live", "ac-old"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var view bytes.Buffer
			if err := runLive(tc.args, false, strings.NewReader(""), &view); err != nil {
				t.Fatal(err)
			}
			for _, want := range tc.want {
				if !strings.Contains(view.String(), want) {
					t.Errorf("output missing %q:\n%s", want, view.String())
				}
			}
			for _, notWant := range tc.notWant {
				if strings.Contains(view.String(), notWant) {
					t.Errorf("output leaked %q:\n%s", notWant, view.String())
				}
			}
		})
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
