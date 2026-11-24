package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/andrew-bierman/lambe/internal/daemon"
	"github.com/andrew-bierman/lambe/internal/symphony"
)

var updateBriefGolden = flag.Bool("update", false, "rewrite golden files")

func briefFixture(now time.Time, runs int) daemon.LiveSnapshot {
	tail := strings.Repeat("t", 4096)
	run := func(id, phase string, updated time.Duration) daemon.LiveRun {
		return daemon.LiveRun{
			Progress:  symphony.Progress{IssueID: id, Title: "Title of " + id, Workflow: "build", Phase: phase, Attempt: 2, Turn: 3},
			UpdatedAt: now.Add(-updated), Tail: tail, TailBytes: int64(len(tail)),
		}
	}
	snap := daemon.LiveSnapshot{
		PID: 4242, Identity: "test:id", UpdatedAt: now.Add(-time.Second), TickAt: now.Add(-3 * time.Second),
		Runs: []daemon.LiveRun{
			run("w-1", "running", 20*time.Second),
			run("w-2", "retry", 14*time.Minute),
			run("w-3", "stopped: gate escalation", 2*time.Hour),
			run("w-4", "stopped: gate green", time.Hour),
		},
		History: []daemon.LiveRun{run("w-5", "stopped: gate red", 3*time.Hour)},
	}
	snap.Runs[0].LastOutputAt = now.Add(-8 * time.Second)
	snap.Runs[0].LastActivityAt = now.Add(-2 * time.Second)
	for i := range runs {
		snap.Runs = append(snap.Runs, run(fmt.Sprintf("bulk-%d", i), "running", time.Minute))
	}
	return snap
}

func TestLogsBriefGolden(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	data, err := json.MarshalIndent(newLiveBrief(briefFixture(now, 0), true, "mini", "v9.9.9", now), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	path := filepath.Join("testdata", "logs-brief.golden")
	if *updateBriefGolden {
		if err := os.MkdirAll("testdata", 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path) //nolint:gosec // fixed testdata path
	if err != nil {
		t.Fatalf("%v (run with -update)", err)
	}
	if !bytes.Equal(want, data) {
		t.Fatalf("brief output differs from %s (run with -update to accept):\n%s", path, data)
	}
}

// The brief document and the dashboard share one rule, so they agree on
// which runs need a human.
func TestLogsBriefAttentionMatchesSharedRule(t *testing.T) {
	now := time.Now()
	snap := briefFixture(now, 0)
	brief := newLiveBrief(snap, true, "m", "v", now)
	for i, run := range append(snap.Runs, snap.History...) {
		reason, want := daemon.NeedsAttention(run)
		got := append(brief.Runs, brief.History...)[i]
		if (got.Attention != nil) != want || (want && got.Attention.Reason != reason) {
			t.Errorf("%s (%s): attention %+v, shared rule says %q %v", run.IssueID, run.Phase, got.Attention, reason, want)
		}
	}
}

func TestLogsBriefIsFarSmallerOnLargeSnapshots(t *testing.T) {
	now := time.Now()
	snap := briefFixture(now, 2500)
	full, err := json.Marshal(liveJSON{LiveSnapshot: snap, Alive: true, Machine: "m"})
	if err != nil {
		t.Fatal(err)
	}
	brief, err := json.Marshal(newLiveBrief(snap, true, "m", "v", now))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("2.5k runs: full %d bytes, brief %d bytes (%.1fx smaller)", len(full), len(brief), float64(len(full))/float64(len(brief)))
	if len(brief)*8 > len(full) {
		t.Fatalf("brief is %d bytes against %d: expected at least 8x smaller", len(brief), len(full))
	}
	if bytes.Contains(brief, []byte(`"Tail"`)) {
		t.Fatal("brief must not carry Tail")
	}
}

func TestLogsBriefFlag(t *testing.T) {
	writeLiveFixture(t, briefFixture(time.Now(), 0))
	var out bytes.Buffer
	if err := runLive([]string{"-brief"}, false, strings.NewReader(""), &out); err == nil {
		t.Fatal("--brief without --json must be a usage error")
	} else if !errors.As(err, new(usageError)) {
		t.Fatalf("want usageError, got %T %v", err, err)
	}
	out.Reset()
	if err := runLive([]string{"--json", "--brief"}, false, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	var got struct {
		LambeVersion string
		Machine      string
		Runs         []struct {
			IssueID   string
			AgeSec    int64
			Attention *struct {
				Reason string
				AgeSec int64
			}
			Tail *string
		}
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v\n%s", err, out.String())
	}
	if got.LambeVersion == "" || len(got.Runs) != 4 {
		t.Fatalf("version %q, %d runs", got.LambeVersion, len(got.Runs))
	}
	for _, run := range got.Runs {
		if run.Tail != nil {
			t.Errorf("%s carries Tail", run.IssueID)
		}
		if run.IssueID == "w-2" && (run.Attention == nil || run.Attention.Reason != "retrying" || run.Attention.AgeSec < 14*60) {
			t.Errorf("retry run attention = %+v", run.Attention)
		}
	}
}

// writeLiveFixture points lambe at a state directory holding snap and
// is read from there.
func writeLiveFixture(t *testing.T, snap daemon.LiveSnapshot) {
	t.Helper()
	isolatedConfigDir(t)
	state := t.TempDir()
	t.Setenv("LAMBE_STATE_DIR", state)
	t.Chdir(t.TempDir())
	if err := os.Mkdir(".beads", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := runInit([]string{"-non-interactive"}, strings.NewReader(""), &bytes.Buffer{}); err != nil {
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
	data, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
