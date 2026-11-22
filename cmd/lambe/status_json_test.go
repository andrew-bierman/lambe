package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/andrew-bierman/lambe/internal/httpstate"
	"github.com/andrew-bierman/lambe/internal/stateapi"
)

func TestReadCommandsExitCodes(t *testing.T) {
	isolatedConfigDir(t)
	t.Chdir(t.TempDir())
	cases := []struct {
		name string
		run  func(*bytes.Buffer) error
		want int
	}{
		{"status unknown flag", func(b *bytes.Buffer) error { return runStatus([]string{"-nope"}, b) }, 2},
		{"status stray argument", func(b *bytes.Buffer) error { return runStatus([]string{"extra"}, b) }, 2},
		{"status json no tracker is a problem", func(b *bytes.Buffer) error { return runStatus([]string{"-json"}, b) }, 1},
		{"state-dir unknown flag", func(b *bytes.Buffer) error { return runStateDir([]string{"-nope"}, "/s", "default", b) }, 2},
		{"state-dir stray argument", func(b *bytes.Buffer) error { return runStateDir([]string{"extra"}, "/s", "default", b) }, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			err := tc.run(&out)
			if err == nil {
				t.Fatal("want an error")
			}
			if got := readExitCode(err); got != tc.want {
				t.Fatalf("exit code = %d, want %d (%v)", got, tc.want, err)
			}
			if out.Len() != 0 {
				t.Fatalf("failure must not write to stdout (a JSON consumer would parse it): %q", out.String())
			}
		})
	}
	if readExitCode(nil) != 0 {
		t.Fatal("nil error must exit 0")
	}
	if readExitCode(errors.New("boom")) != 1 {
		t.Fatal("plain error must exit 1")
	}
}

func TestStateDirJSONListsDescriptorsWithoutToken(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "live")
	if err := os.MkdirAll(live, 0o700); err != nil {
		t.Fatal(err)
	}
	desc := `{"schema_version":1,"instance":"lambe-abc","addr":"127.0.0.1:5000","bearer_token":"TOPSECRET","pid":` +
		strconv.Itoa(os.Getpid()) + `,"version":"v1","config_path":"/c.yaml","started_at":"2026-11-21T00:00:00Z"}`
	path := filepath.Join(live, "stateapi-lambe-abc.json")
	if err := os.WriteFile(path, []byte(desc), 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runStateDir([]string{"-json"}, dir, "env", &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "TOPSECRET") || strings.Contains(out.String(), "bearer") {
		t.Fatalf("token leaked: %s", out.String())
	}
	var got stateDirInfo
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out.String())
	}
	if got.StateDir != dir || got.Source != "env" || !got.Exists || got.LiveDir != live {
		t.Errorf("info = %+v", got)
	}
	want := stateapi.Entry{Path: path, Instance: "lambe-abc", Addr: "127.0.0.1:5000", PID: os.Getpid(), Version: "v1", ConfigPath: "/c.yaml", StartedAt: "2026-11-21T00:00:00Z"}
	if len(got.Descriptors) != 1 || got.Descriptors[0] != want {
		t.Errorf("descriptors = %+v", got.Descriptors)
	}

	var text bytes.Buffer
	if err := runStateDir(nil, dir, "env", &text); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{dir, "LAMBE_STATE_DIR", "lambe-abc", "127.0.0.1:5000", path} {
		if !strings.Contains(text.String(), s) {
			t.Errorf("text output missing %q:\n%s", s, text.String())
		}
	}
	if strings.Contains(text.String(), "TOPSECRET") {
		t.Fatalf("token leaked in text: %s", text.String())
	}
}

func TestStateDirEmptyAndMissingAreHonest(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "never-created")
	var out bytes.Buffer
	if err := runStateDir([]string{"-json"}, missing, "default", &out); err != nil {
		t.Fatalf("a missing state dir is a fact, not an error: %v", err)
	}
	var got stateDirInfo
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.StateDir != missing || got.Exists || got.Source != "default" {
		t.Errorf("info = %+v", got)
	}
	if got.Descriptors == nil || len(got.Descriptors) != 0 {
		t.Errorf("descriptors must be an empty list, not null: %s", out.String())
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("state-dir must not create the directory")
	}
	var text bytes.Buffer
	if err := runStateDir(nil, missing, "default", &text); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text.String(), "does not exist") || !strings.Contains(text.String(), "no state API descriptors") {
		t.Errorf("text should say so plainly:\n%s", text.String())
	}
}

// TestStatusJSONReadsTrackerWithNoDaemon runs `lambe status -json` against a
// real Beads tracker with no daemon: the answer must be a state-API
// snapshot built from the tracker alone.
func TestStatusJSONReadsTrackerWithNoDaemon(t *testing.T) {
	if os.Getenv("LAMBE_TEST_BD") != "1" {
		t.Skip("set LAMBE_TEST_BD=1 for real tracker integration")
	}
	if _, err := exec.LookPath("bd"); err != nil {
		t.Skip("bd binary not found")
	}
	isolatedConfigDir(t)
	project := t.TempDir()
	init := exec.Command("bd", "init", "--prefix", "sj1", "--non-interactive")
	init.Dir = project
	if out, err := init.CombinedOutput(); err != nil {
		t.Fatalf("bd init: %v: %s", err, out)
	}
	configPath := filepath.Join(t.TempDir(), "lambe.yaml")
	if err := os.WriteFile(configPath, []byte("trackers:\n  - name: local\n    type: repo\n    path: "+quoteYAML(project)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LAMBE_MACHINE", "jsonbox")

	var out bytes.Buffer
	if err := runStatus([]string{"-json", "-config", configPath}, &out); err != nil {
		t.Fatalf("status -json: %v", err)
	}
	var got httpstate.SnapshotResponse
	dec := json.NewDecoder(bytes.NewReader(out.Bytes()))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&got); err != nil {
		t.Fatalf("not a SnapshotResponse: %v\n%s", err, out.String())
	}
	if got.SchemaVersion != httpstate.SchemaVersion || got.Daemon.Machine != "jsonbox" || got.Daemon.PID != 0 ||
		got.Daemon.ConfigPath != configPath || len(got.Runs) != 0 || got.TickAt != "" {
		t.Errorf("snapshot = %+v", got)
	}
}

// TestReadCommandsExitTwoFromBinary pins the main() wiring, which the
// run* unit tests cannot see: a usage error exits 2 with nothing on stdout.
func TestReadCommandsExitTwoFromBinary(t *testing.T) {
	if testing.Short() {
		t.Skip("-short skips building the binary")
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "lambe")
	build := exec.Command("go", "build", "-o", bin, "./cmd/lambe") //nolint:gosec // G204: fixed package path; output in the test's temp dir
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v: %s", err, out)
	}
	for _, args := range [][]string{{"status", "-nope"}, {"state-dir", "-nope"}} {
		cmd := exec.Command(bin, args...) //nolint:gosec // G204: binary built above
		cmd.Env = append(os.Environ(), "LAMBE_STATE_DIR="+t.TempDir(), "HOME="+t.TempDir())
		var stdout bytes.Buffer
		cmd.Stdout = &stdout
		err := cmd.Run()
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 2 || stdout.Len() != 0 {
			t.Fatalf("%v: err=%v stdout=%q, want exit 2 and empty stdout", args, err, stdout.String())
		}
	}
}
