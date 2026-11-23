package daemon

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/andrew-bierman/lambe/internal/config"
	"github.com/andrew-bierman/lambe/internal/symphony"
	"github.com/andrew-bierman/lambe/internal/tracker"
)

func TestGroupHistory(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	at := func(m int) time.Time { return base.Add(time.Duration(m) * time.Minute) }
	type entry = AttemptKey
	tests := []struct {
		name string
		in   []entry
		want []string // per group: issue:headline-attempt:headline-phase:count
	}{
		{
			name: "interrupted rows never headline over the real outcome of the same attempt",
			in: []entry{
				{"A", 1, "interrupted", at(30)},
				{"A", 2, "interrupted", at(20)},
				{"A", 2, "stopped: gate green", at(10)},
				{"A", 1, "interrupted", at(5)},
			},
			want: []string{"A:2:stopped: gate green:2"},
		},
		{
			name: "latest attempt wins even when an older one was observed later",
			in: []entry{
				{"A", 3, "stopped: gate escalation", at(1)},
				{"A", 1, "interrupted", at(50)},
			},
			want: []string{"A:3:stopped: gate escalation:2"},
		},
		{
			name: "a trailing interrupted attempt is the headline when nothing later exists",
			in: []entry{
				{"A", 1, "stopped: gate red", at(1)},
				{"A", 2, "interrupted", at(2)},
			},
			want: []string{"A:2:interrupted:2"},
		},
		{
			name: "phase-less registry rows lose to a phase for the same attempt",
			in: []entry{
				{"A", 1, "", at(9)},
				{"A", 1, "stopped: gate green", at(1)},
			},
			want: []string{"A:1:stopped: gate green:1"},
		},
		{
			name: "groups are ordered by headline time, newest first",
			in: []entry{
				{"old", 1, "stopped: gate green", at(1)},
				{"new", 1, "stopped: gate green", at(9)},
				{"mid", 1, "stopped: closed externally", at(5)},
			},
			want: []string{"new:1:stopped: gate green:1", "mid:1:stopped: closed externally:1", "old:1:stopped: gate green:1"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			groups := GroupHistory(tc.in, func(k AttemptKey) AttemptKey { return k })
			var got []string
			for _, g := range groups {
				got = append(got, strings.Join([]string{g[0].Issue, itoa(g[0].Attempt), g[0].Phase, itoa(len(g))}, ":"))
			}
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Fatalf("groups = %v, want %v", got, tc.want)
			}
		})
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

// TestStateSourceHistoryGroupsAttemptsPerIssue is the production shape: three
// restart-interrupted rows above a final green one collapse to one row.
func TestStateSourceHistoryGroupsAttemptsPerIssue(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Truncate(time.Second)
	path := filepath.Join(t.TempDir(), "live.json")
	writeLive(t, path, LiveSnapshot{
		PID: os.Getpid(),
		Runs: []LiveRun{
			{Progress: symphony.Progress{IssueID: "demo", Title: "Demo", Phase: "stopped: gate green", Attempt: 3}, UpdatedAt: now.Add(-time.Hour)},
		},
		History: []LiveRun{
			{Progress: symphony.Progress{IssueID: "demo", Phase: "interrupted", Attempt: 3}, UpdatedAt: now},
			{Progress: symphony.Progress{IssueID: "demo", Phase: "interrupted", Attempt: 2}, UpdatedAt: now.Add(-time.Minute)},
			{Progress: symphony.Progress{IssueID: "demo", Phase: "interrupted", Attempt: 1}, UpdatedAt: now.Add(-2 * time.Minute)},
			{Progress: symphony.Progress{IssueID: "ext", Phase: "stopped: recovered closed issue", Attempt: 1}, UpdatedAt: now.Add(-3 * time.Hour)},
		},
	})
	src := NewStateSource(NewRunRegistry(nil, nil), &ReportCache{}, nil)
	src.WatchLive(path)
	runs := src.History().Runs
	if len(runs) != 2 {
		t.Fatalf("history = %+v, want one row per issue", runs)
	}
	demo := runs[0]
	if demo.Issue != "demo" || demo.Phase != "stopped: gate green" || demo.Attempt != 3 || len(demo.Attempts) != 3 || demo.Title != "Demo" {
		t.Fatalf("demo row = %+v, want green attempt 3 headlining three attempts", demo)
	}
	if got := runs[1].Phase; got != PhaseClosedExternally {
		t.Fatalf("legacy phase = %q, want %q", got, PhaseClosedExternally)
	}
}

func writeLive(t *testing.T, path string, snap LiveSnapshot) {
	t.Helper()
	v := &liveView{path: path, snapshot: snap}
	if err := v.save(); err != nil {
		t.Fatal(err)
	}
}

func TestGateRejectionLog(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "gates", "i.jsonl")
	log := NewGateRejectionLog(path)
	now := time.Now()
	if err := log.Record("A-1", 2, "\x1b[31mFAIL\x1b[0m bad\x00 thing\nsecond line\t"+strings.Repeat("é", 600), now); err != nil {
		t.Fatal(err)
	}
	if err := log.Record("A-2", 1, "old", now.Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	recs, err := ReadGateRejections(path, now.Add(-GateRejectionWindow))
	if err != nil || len(recs) != 1 {
		t.Fatalf("window read = %+v, %v; want only the recent record", recs, err)
	}
	r := recs[0]
	if r.Issue != "A-1" || r.Attempt != 2 || !strings.HasPrefix(r.Output, "FAIL bad thing second line é") {
		t.Fatalf("record = %+v", r)
	}
	if strings.ContainsAny(r.Output, "\x1b\x00\n\t") || len(r.Output) > maxGateRejectionOutput+len("…") {
		t.Fatalf("output not sanitized/bounded: %d bytes %q", len(r.Output), r.Output)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("log mode = %v, %v; want 0600", info, err)
	}
	if recs, err := ReadGateRejections(filepath.Join(t.TempDir(), "missing"), time.Time{}); err != nil || recs != nil {
		t.Fatalf("missing log = %v, %v; want none and no error", recs, err)
	}
}

func TestGateRejectionLogCompactsToNewest(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "i.jsonl")
	log := NewGateRejectionLog(path)
	now := time.Now()
	out := strings.Repeat("x", maxGateRejectionOutput)
	total := compactGateRejectionsAt/maxGateRejectionOutput + 50
	for i := range total {
		if err := log.Record("I-"+string(rune('A'+i%26)), i, out, now.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	recs, err := ReadGateRejections(path, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) > total || len(recs) < 1 || recs[len(recs)-1].Attempt != total-1 {
		t.Fatalf("after compaction: %d records, last attempt %d; want the newest kept", len(recs), recs[len(recs)-1].Attempt)
	}
	if info, _ := os.Stat(path); info.Size() > compactGateRejectionsAt+2*(maxGateRejectionOutput+200) {
		t.Fatalf("log grew unbounded: %d bytes", info.Size())
	}
}

func TestRenderReportGateRejections(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 20, 3, 4, 0, 0, time.Local)
	tests := []struct {
		name string
		rep  Report
		want string
	}{
		{"unknown", Report{}, "last-night gate rejections:\n  (history unavailable)\n"},
		{"known and empty", Report{GateRejectionsKnown: true}, "last-night gate rejections:\n  (none)\n"},
		{"rows", Report{GateRejectionsKnown: true, GateRejections: []GateRejection{{Issue: "T-9", Attempt: 2, Output: "tests failed", At: at}}}, "T-9"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var b strings.Builder
			if err := RenderReport(&b, tc.rep); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(b.String(), tc.want) {
				t.Fatalf("report = %q, want %q", b.String(), tc.want)
			}
		})
	}
}

// TestStatusReadsGateRejectionLog drives readReport's wiring: seeded records
// in the configured instance's log surface in the report window.
func TestStatusReadsGateRejectionLog(t *testing.T) {
	bin := bdBin()
	if bin == "" {
		t.Skip("bd binary not found; set LAMBE_TEST_BD=1 with bd on PATH to run")
	}
	dir := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	local, err := tracker.NewLocal(bin, dir, "lambe@testbox")
	if err != nil {
		t.Fatal(err)
	}
	if err := local.Init(t.Context()); err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	t.Setenv("LAMBE_STATE_DIR", state)
	cfg := &config.Config{Trackers: []config.Tracker{{Name: "t", Path: dir}}}
	if err := NewGateRejectionLog(GateRejectionLogPath(state, cfg)).Record("T-7", 3, "build failed", time.Now()); err != nil {
		t.Fatal(err)
	}
	rep, err := readReport(cfg, "testbox")
	if err != nil {
		t.Fatal(err)
	}
	if !rep.GateRejectionsKnown || len(rep.GateRejections) != 1 || rep.GateRejections[0].Issue != "T-7" || rep.GateRejections[0].Attempt != 3 {
		t.Fatalf("report gate rejections = %+v known=%v", rep.GateRejections, rep.GateRejectionsKnown)
	}
}
