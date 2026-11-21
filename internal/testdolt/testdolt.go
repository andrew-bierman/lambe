// Package testdolt starts an isolated Dolt sql-server for multi-host
// integration tests: one server, several bd workspaces (one per simulated
// host) sharing a single database. Everything lives in t.TempDir() on a
// loopback port the OS picked; it never touches a real Beads install.
package testdolt

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Server is one running isolated dolt sql-server.
type Server struct {
	Port int
	// Bd is the bd binary used for workspaces.
	Bd   string
	root string
}

// Start launches the server, skipping the test unless LAMBE_TEST_BD=1 and
// both bd and dolt are installed.
func Start(t *testing.T) *Server {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping multi-host bd integration test in -short mode")
	}
	if os.Getenv("LAMBE_TEST_BD") != "1" {
		t.Skip("set LAMBE_TEST_BD=1 for the multi-host Dolt integration test")
	}
	bd, err := exec.LookPath("bd")
	if err != nil {
		t.Skipf("bd not on PATH: %v", err)
	}
	dolt, err := exec.LookPath("dolt")
	if err != nil {
		t.Skipf("dolt not on PATH (needed for a shared sql-server): %v", err)
	}
	if out, err := exec.Command(dolt, "version").CombinedOutput(); err != nil { // #nosec G204 -- fixed argv
		t.Skipf("dolt on PATH is not runnable (broken shim?): %v: %s", err, out)
	}
	root := t.TempDir()
	home := filepath.Join(root, "doltroot")
	data := filepath.Join(root, "data")
	for _, d := range []string{home, data} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	env := append(os.Environ(), "DOLT_ROOT_PATH="+home)
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command(dolt, args...) // #nosec G204 -- fixed argv
		cmd.Dir, cmd.Env = dir, env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("dolt %s: %v: %s", strings.Join(args, " "), err, out)
		}
	}
	run(root, "config", "--global", "--add", "user.name", "lambe-test")
	run(root, "config", "--global", "--add", "user.email", "lambe-test@example.invalid")
	run(data, "init")

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()

	srv := exec.Command(dolt, "sql-server", "-H", "127.0.0.1", "-P", strconv.Itoa(port)) // #nosec G204 -- fixed argv
	srv.Dir, srv.Env = data, env
	if err := srv.Start(); err != nil {
		t.Fatalf("start dolt sql-server: %v", err)
	}
	t.Cleanup(func() { _ = srv.Process.Kill(); _ = srv.Wait() })
	deadline := time.Now().Add(20 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 200*time.Millisecond)
		if err == nil {
			_ = c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("dolt sql-server on port %d never accepted connections", port)
		}
		time.Sleep(100 * time.Millisecond)
	}
	return &Server{Port: port, Bd: bd, root: root}
}

// Workspace creates one simulated host: its own .beads directory pointing
// at the shared database. The first call creates the database.
func (s *Server) Workspace(t *testing.T, host string) string {
	t.Helper()
	dir := filepath.Join(s.root, "host-"+host)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(s.Bd, "init", "--non-interactive", "--server", "--external", // #nosec G204 -- fixed argv
		"--server-host", "127.0.0.1", "--server-port", strconv.Itoa(s.Port), "-p", "mh", "--database", "mh")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "BEADS_ACTOR=host-"+host)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("bd init (%s): %v: %s", host, err, out)
	}
	return dir
}

// Bdx runs bd in dir as actor and returns combined output.
func (s *Server) Bdx(t *testing.T, dir, actor string, args ...string) string {
	t.Helper()
	cmd := exec.Command(s.Bd, args...) // #nosec G204 -- test-controlled argv
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "BEADS_ACTOR="+actor)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bd %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// ExpireLease backdates the lease on id by an hour, standing in for "the
// owning host died five minutes ago" without waiting out bd's fixed 5m TTL.
func (s *Server) ExpireLease(t *testing.T, dir, id string) {
	t.Helper()
	s.Bdx(t, dir, "test", "sql",
		fmt.Sprintf("update leases set lease_expires_at = now() - interval 1 hour where issue_id = '%s'", id))
}
