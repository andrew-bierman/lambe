package gate_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/andrew-bierman/lambe/internal/gate"
)

// fakeBd writes a bd stand-in that appends its argv to a log and then runs
// body (a sh snippet that may branch on "$1"). These tests therefore run
// without a real bd and pin the exact commands BdHub issues.
func fakeBd(t *testing.T, body string) (bin string, readLog func() []string) {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	bin = filepath.Join(dir, "bd")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$*\" >> %q\n%s\n", logPath, body)
	syscall.ForkLock.RLock()                        // avoid ETXTBSY: no fork while the write fd is open
	err := os.WriteFile(bin, []byte(script), 0o700) //nolint:gosec // G306: the fake bd must be executable.
	syscall.ForkLock.RUnlock()
	if err != nil {
		t.Fatal(err)
	}
	return bin, func() []string {
		raw, err := os.ReadFile(logPath) //nolint:gosec // G304: test-owned log path.
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			t.Fatal(err)
		}
		return strings.Split(strings.TrimSpace(string(raw)), "\n")
	}
}

func newFakeBdHub(t *testing.T, body string) (*gate.BdHub, func() []string) {
	t.Helper()
	bin, calls := fakeBd(t, body)
	hub, err := gate.NewBdHub(bin, t.TempDir(), "lambe@test")
	if err != nil {
		t.Fatal(err)
	}
	return hub, calls
}

func TestBdHubDefer(t *testing.T) {
	until := time.Date(2030, 1, 2, 3, 4, 5, 0, time.FixedZone("x", 3600))
	t.Run("issues defer with UTC until and reason", func(t *testing.T) {
		hub, calls := newFakeBdHub(t, `echo '{"data":{}}'`)
		if err := hub.Defer(t.Context(), "bd-1", until, "ci pending"); err != nil {
			t.Fatal(err)
		}
		got := calls()
		want := "defer bd-1 --json --until 2030-01-02T02:04:05Z --reason ci pending"
		if len(got) != 1 || got[0] != want {
			t.Fatalf("bd calls = %q, want [%q]", got, want)
		}
	})
	t.Run("bd failure is wrapped with the issue", func(t *testing.T) {
		hub, _ := newFakeBdHub(t, `echo "no such issue" >&2; exit 1`)
		err := hub.Defer(t.Context(), "bd-9", until, "r")
		if err == nil || !strings.Contains(err.Error(), "defer bd-9") || !strings.Contains(err.Error(), "no such issue") {
			t.Fatalf("err = %v, want wrapped bd stderr naming the issue", err)
		}
	})
}

func TestBdHubCloseGuardMapping(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		reason    string
		wantRace  bool
		wantErr   bool
		wantCalls []string
	}{
		{
			name: "success writes guarded close then the reason comment", body: `echo '{}'`, reason: "gate green",
			wantCalls: []string{
				"update bd-1 --json --if-assignee me --status closed",
				"comment bd-1 gate green",
			},
		},
		{
			name: "no reason means no comment", body: `echo '{}'`, reason: "",
			wantCalls: []string{"update bd-1 --json --if-assignee me --status closed"},
		},
		{
			name: "exit 1 guard_mismatch body is a lost race", reason: "r", wantRace: true, wantErr: true,
			body:      `echo '{"error":"guard_mismatch"}'; exit 1`,
			wantCalls: []string{"update bd-1 --json --if-assignee me --status closed"},
		},
		{
			name: "assignee mismatch text is a lost race", reason: "r", wantRace: true, wantErr: true,
			body:      `echo "Assignee Mismatch: held by other" >&2; exit 1`,
			wantCalls: []string{"update bd-1 --json --if-assignee me --status closed"},
		},
		{
			name: "other failure is not a race", reason: "r", wantErr: true,
			body:      `echo "disk full" >&2; exit 2`,
			wantCalls: []string{"update bd-1 --json --if-assignee me --status closed"},
		},
		{
			name: "comment failure after close is surfaced", reason: "r", wantErr: true,
			body:      `if [ "$1" = comment ]; then echo "comment boom" >&2; exit 1; fi; echo '{}'`,
			wantCalls: []string{"update bd-1 --json --if-assignee me --status closed", "comment bd-1 r"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hub, calls := newFakeBdHub(t, tc.body)
			err := hub.Close(t.Context(), "bd-1", "me", tc.reason)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if got := errors.Is(err, gate.ErrCloseRace); got != tc.wantRace {
				t.Errorf("ErrCloseRace = %v, want %v (err %v)", got, tc.wantRace, err)
			}
			if got := calls(); strings.Join(got, "|") != strings.Join(tc.wantCalls, "|") {
				t.Errorf("bd calls = %q, want %q", got, tc.wantCalls)
			}
		})
	}
}

func TestBdHubNeedsHumanFallsBackToBlockedAndStampsMetadata(t *testing.T) {
	// bd rejects the needs-human status; the blocked fallback succeeds.
	hub, calls := newFakeBdHub(t, `case "$*" in update*--status\ needs-human*) echo "invalid status" >&2; exit 1;; esac; echo '{}'`)
	if err := hub.NeedsHuman(t.Context(), "bd-1", "why", "last out"); err != nil {
		t.Fatal(err)
	}
	got := calls()
	meta := "--set-metadata lambe_last_gate_output=last out --set-metadata lambe_needs_human_reason=why"
	want := []string{
		"update bd-1 --json --status needs-human " + meta,
		"update bd-1 --json --status blocked " + meta,
		"comment bd-1 lambe gate escalated this issue to needs-human: why.\nLast gate output:\nlast out",
	}
	// The comment body spans lines, which the one-call-per-line log splits.
	if strings.Join(got, "|") != strings.ReplaceAll(strings.Join(want, "|"), "\n", "|") {
		t.Fatalf("bd calls:\n%q\nwant:\n%q", got, want)
	}
}

func TestBdHubNeedsHumanBothStatusWritesFail(t *testing.T) {
	hub, _ := newFakeBdHub(t, `echo "down" >&2; exit 1`)
	err := hub.NeedsHuman(t.Context(), "bd-1", "why", "")
	if err == nil || !strings.Contains(err.Error(), "blocked fallback") {
		t.Fatalf("err = %v, want both failures reported", err)
	}
}

func TestBdHubReadsDecodeBothShowShapes(t *testing.T) {
	tests := []struct {
		name       string
		show       string
		wantStatus string
		wantTomb   bool
		wantKey    string
		wantErr    bool
	}{
		{name: "array shape", show: `[{"status":"open","metadata":{"lambe_gate_tombstone":"1","n":3}}]`, wantStatus: "open", wantTomb: true, wantKey: "3"},
		{name: "object shape", show: `{"status":"closed","metadata":{"n":true}}`, wantStatus: "closed", wantKey: "true"},
		{name: "enveloped object", show: `{"data":{"status":"blocked"}}`, wantStatus: "blocked"},
		{name: "garbage", show: `<html>`, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hub, _ := newFakeBdHub(t, fmt.Sprintf("echo '%s'", tc.show))
			ctx := t.Context()
			status, serr := hub.IssueStatus(ctx, "bd-1")
			tomb, terr := hub.IsTombstoned(ctx, "bd-1")
			n, nerr := hub.Get(ctx, "bd-1", "n")
			if tc.wantErr {
				if serr == nil || terr == nil || nerr == nil {
					t.Fatalf("errs = %v %v %v, want decode failures", serr, terr, nerr)
				}
				return
			}
			if serr != nil || terr != nil || nerr != nil {
				t.Fatalf("errs = %v %v %v", serr, terr, nerr)
			}
			if status != tc.wantStatus || tomb != tc.wantTomb || n != tc.wantKey {
				t.Errorf("status=%q tomb=%v n=%q, want %q %v %q", status, tomb, n, tc.wantStatus, tc.wantTomb, tc.wantKey)
			}
		})
	}
}

func TestBdHubPutAndTombstoneWriteMetadata(t *testing.T) {
	hub, calls := newFakeBdHub(t, `echo '{}'`)
	if err := hub.Put(t.Context(), "bd-1", "k", "v"); err != nil {
		t.Fatal(err)
	}
	if err := hub.Tombstone(t.Context(), "bd-1"); err != nil {
		t.Fatal(err)
	}
	want := "update bd-1 --json --set-metadata k=v|update bd-1 --json --set-metadata lambe_gate_tombstone=1"
	if got := strings.Join(calls(), "|"); got != want {
		t.Fatalf("bd calls = %q, want %q", got, want)
	}
}

func TestBdHubEnsureNeedsHumanStatus(t *testing.T) {
	hub, calls := newFakeBdHub(t, `echo ok`)
	if err := hub.EnsureNeedsHumanStatus(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := calls(); len(got) != 1 || got[0] != "config set status.custom needs-human" {
		t.Fatalf("bd calls = %q", got)
	}
	failing, _ := newFakeBdHub(t, `exit 1`)
	if err := failing.EnsureNeedsHumanStatus(t.Context()); err == nil {
		t.Fatal("bd failure not reported")
	}
}
