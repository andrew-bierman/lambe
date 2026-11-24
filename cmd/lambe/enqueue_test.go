package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/andrew-bierman/lambe/internal/config"
	"github.com/andrew-bierman/lambe/internal/tracker"
)

// TestEnqueueJSON drives `lambe enqueue --json` against a throwaway bd
// database under an isolated HOME and state dir.
func TestEnqueueJSON(t *testing.T) {
	if os.Getenv("LAMBE_TEST_BD") != "1" {
		t.Skip("set LAMBE_TEST_BD=1 for real tracker integration")
	}
	isolatedConfigDir(t)
	t.Setenv("LAMBE_STATE_DIR", t.TempDir())

	dir := t.TempDir()
	initCmd := exec.Command("bd", "init", "--prefix", "enq", "--non-interactive")
	initCmd.Dir = dir
	if out, err := initCmd.CombinedOutput(); err != nil {
		t.Fatalf("bd init: %v: %s", err, out)
	}
	gates := filepath.Join(dir, "gates")
	if err := os.MkdirAll(gates, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gates, "g.formula.toml"), []byte("[gate]\ntype = \"shell\"\ncommand = \"exit 0\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "REPO.md"), []byte("---\n---\nDo the task.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgFile := filepath.Join(dir, "lambe.yaml")
	yaml := "trackers:\n  - name: local\n    type: repo\n    path: " + dir + "\n" +
		"workflows:\n  - name: Repo tasks\n    label: lambe:repo\n    path: REPO.md\n    gate_dir: " + gates + "\n"
	if err := os.WriteFile(cfgFile, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.Options{File: cfgFile, Env: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	_, actor := hostIdentity(cfg)
	local, err := tracker.NewLocal("", dir, actor)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := local.CreateTask(t.Context(), "plain", nil)
	if err != nil {
		t.Fatal(err)
	}
	labelled, err := local.CreateTask(t.Context(), "labelled", []string{"lambe:repo"})
	if err != nil {
		t.Fatal(err)
	}

	type result struct {
		SchemaVersion int    `json:"schema_version"`
		Issue         string `json:"issue"`
		Workflow      string `json:"workflow"`
		Applied       bool   `json:"applied"`
		WouldAddLabel bool   `json:"would_add_label"`
	}
	run := func(id string, extra ...string) (string, error) {
		var out bytes.Buffer
		args := append([]string{"--config", cfgFile, "--workflow", "Repo tasks", "--json"}, extra...)
		err := runEnqueue(append(args, id), &out)
		return out.String(), err
	}
	// Cases run in order: the apply case labels the issue the last one reads.
	for _, tc := range []struct {
		name  string
		id    string
		extra []string
		want  result
	}{
		{"preview", plain.ID, nil, result{1, plain.ID, "Repo tasks", false, true}},
		{"already labelled", labelled.ID, nil, result{1, labelled.ID, "Repo tasks", false, false}},
		{"apply", plain.ID, []string{"--apply"}, result{1, plain.ID, "Repo tasks", true, true}},
		{"applied label is now present", plain.ID, nil, result{1, plain.ID, "Repo tasks", false, false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := run(tc.id, tc.extra...)
			if err != nil {
				t.Fatal(err)
			}
			var got result
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatalf("stdout is not one JSON object: %v: %q", err, out)
			}
			if got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
	t.Run("missing issue", func(t *testing.T) {
		out, err := run("enq-nope")
		if readExitCode(err) != exitFailure {
			t.Errorf("exit = %d (%v), want %d", readExitCode(err), err, exitFailure)
		}
		if out != "" {
			t.Errorf("stdout = %q, want empty", out)
		}
	})
}
