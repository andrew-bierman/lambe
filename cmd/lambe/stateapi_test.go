package main

// The assembled-binary proof of the HTTP state surface wiring: this test
// builds the real lambe binary, runs `lambe run` against a disposable bd
// tracker with the surface explicitly enabled, and asserts an
// authenticated /api/v1/state answers while the per-instance descriptor
// exists — then that the descriptor is gone after a graceful stop. If the
// daemon-side wiring is ever removed, the descriptor never appears and
// this test fails (the dead-seam regression). The in-process assembled
// proof lives in internal/daemon's stateapi tests.

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// syncBuffer is a concurrency-safe capture of the daemon process output.
type processLogBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *processLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *processLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// stateAPIDescriptor mirrors the discovery document the daemon publishes.
// The test enumerates stateapi-*.json like a real client instead of
// rebuilding the instance-name hash.
type stateAPIDescriptor struct {
	Instance    string `json:"instance"`
	Addr        string `json:"addr"`
	BearerToken string `json:"bearer_token"`
	PID         int    `json:"pid"`
	Version     string `json:"version"`
	StartedAt   string `json:"started_at"`
}

func TestRunBinaryServesAuthenticatedStateAPI(t *testing.T) {
	if testing.Short() {
		t.Skip("-short skips the assembled-binary state surface test")
	}
	if os.Getenv("LAMBE_TEST_BD") != "1" {
		t.Skip("set LAMBE_TEST_BD=1 for real tracker integration")
	}
	if _, err := exec.LookPath("bd"); err != nil {
		t.Skip("bd binary not found")
	}

	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "lambe")
	build := exec.Command("go", "build", "-o", bin, "./cmd/lambe") //nolint:gosec // G204: fixed package path; output lands in the test's own temp dir
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build lambe: %v: %s", err, out)
	}

	project := t.TempDir()
	init := exec.Command("bd", "init", "--prefix", "bin1", "--non-interactive")
	init.Dir = project
	if out, err := init.CombinedOutput(); err != nil {
		t.Fatalf("bd init: %v: %s", err, out)
	}
	workflowPath := filepath.Join(project, "WORKFLOW.md")
	if err := os.WriteFile(workflowPath, []byte("---\n---\nWork on {{ issue }}.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	formulas := filepath.Join(project, ".beads", "formulas")
	if err := os.MkdirAll(formulas, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(formulas, "verify.formula.toml"), []byte("[gate]\ntype = \"shell\"\ncommand = \"exit 0\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "lambe.yaml")
	cfgYAML := "harness: acp\n" +
		"acp:\n  command: [\"true\"]\n" +
		"workflow_path: " + quoteYAML(workflowPath) + "\n" +
		"trackers:\n  - name: local\n    type: repo\n    path: " + quoteYAML(project) + "\n" +
		"stateapi:\n  enable: true\n  addr: 127.0.0.1:0\n"
	if err := os.WriteFile(configPath, []byte(cfgYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	stateDir := t.TempDir()
	cmd := exec.Command(bin, "run", "-config", configPath) //nolint:gosec // G204: binary built by this test into a temp dir
	cmd.Env = append(os.Environ(), "LAMBE_STATE_DIR="+stateDir)
	logs := &processLogBuffer{}
	cmd.Stdout = logs
	cmd.Stderr = logs
	if err := cmd.Start(); err != nil {
		t.Fatalf("start lambe run: %v", err)
	}
	terminated := false
	t.Cleanup(func() {
		if terminated {
			return
		}
		_ = cmd.Process.Signal(syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
	})

	// Enumerate the live dir like a client: exactly one descriptor for
	// this instance must appear. Its absence is the dead-seam failure.
	liveDir := filepath.Join(stateDir, "live")
	var desc stateAPIDescriptor
	deadline := time.Now().Add(45 * time.Second)
	for {
		matches := stateAPIDescriptorFiles(t, liveDir)
		switch {
		case len(matches) == 1:
			data, err := os.ReadFile(matches[0]) // #nosec G304 -- test-owned temporary path
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(data, &desc); err != nil {
				t.Fatalf("decode descriptor: %v", err)
			}
		case len(matches) > 1:
			t.Fatalf("expected one descriptor, found %v", matches)
		default:
			if time.Now().After(deadline) {
				t.Fatalf("assembled binary never published its stateapi descriptor (wiring regression); logs:\n%s", logs.String())
			}
			time.Sleep(100 * time.Millisecond)
			continue
		}
		break
	}
	if !strings.HasPrefix(desc.Addr, "127.0.0.1:") || desc.BearerToken == "" || desc.PID != cmd.Process.Pid {
		t.Fatalf("descriptor identity facts: %+v", desc)
	}
	base := "http://" + desc.Addr

	// With the descriptor's token, the authenticated GET answers
	// schema_version 1; without it, every route — refresh included — is a
	// uniform 401.
	resp, err := stateAPIRequest(t, "GET", base+"/api/v1/state", desc.BearerToken)
	if err != nil {
		t.Fatalf("authenticated GET: %v", err)
	}
	var snap struct {
		SchemaVersion int `json:"schema_version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || snap.SchemaVersion != 1 {
		t.Errorf("authenticated GET = %d schema %d, want 200/1", resp.StatusCode, snap.SchemaVersion)
	}
	resp, err = stateAPIRequest(t, "GET", base+"/api/v1/state", "")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated GET = %d, want 401", resp.StatusCode)
	}
	resp, err = stateAPIRequest(t, "POST", base+"/api/v1/refresh", "")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated refresh = %d, want 401", resp.StatusCode)
	}
	resp, err = stateAPIRequest(t, "POST", base+"/api/v1/refresh", desc.BearerToken)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("authenticated refresh = %d, want 202", resp.StatusCode)
	}

	// Graceful stop: SIGTERM drives the daemon's own shutdown ladder.
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM: %v", err)
	}
	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()
	select {
	case err := <-waitErr:
		terminated = true
		if err != nil {
			t.Fatalf("lambe run exit: %v", err)
		}
	case <-time.After(45 * time.Second):
		t.Fatalf("daemon did not exit after SIGTERM; logs:\n%s", logs.String())
	}
	if matches := stateAPIDescriptorFiles(t, liveDir); len(matches) != 0 {
		t.Errorf("descriptor survived graceful shutdown: %v", matches)
	}
	if out := logs.String(); strings.Contains(out, desc.BearerToken) {
		t.Error("bearer token leaked into daemon output")
	}
}

// stateAPIDescriptorFiles lists the published stateapi descriptors in dir,
// returning nothing when the dir does not exist yet.
func stateAPIDescriptorFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		return nil
	}
	var matches []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, "stateapi-") && strings.HasSuffix(name, ".json") {
			matches = append(matches, filepath.Join(dir, name))
		}
	}
	return matches
}

func stateAPIRequest(t *testing.T, method, url, token string) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return (&http.Client{Timeout: 5 * time.Second}).Do(req)
}

// quoteYAML renders a path as a double-quoted YAML scalar.
func quoteYAML(path string) string {
	return "\"" + strings.ReplaceAll(path, "\\", "\\\\") + "\""
}
