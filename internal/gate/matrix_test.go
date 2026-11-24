package gate_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/andrew-bierman/lambe/internal/gate"
	"github.com/andrew-bierman/lambe/internal/symphony"
)

// blockingGH is a GHChecker whose every call blocks until its context ends,
// modelling a hung gh CLI.
type blockingGH struct{}

func (blockingGH) CheckRun(ctx context.Context, _ string) (gate.GHResult, error) {
	<-ctx.Done()
	return gate.GHPending, ctx.Err()
}

func (blockingGH) CheckPR(ctx context.Context, _ string) (gate.GHResult, error) {
	<-ctx.Done()
	return gate.GHPending, ctx.Err()
}

func (blockingGH) ResolvePRHead(ctx context.Context, _ string) (string, error) {
	<-ctx.Done()
	return "", ctx.Err()
}

// matrixCondition is one column of the gate boundary matrix.
type matrixCondition string

const (
	condGreen   matrixCondition = "green"
	condRed     matrixCondition = "red"
	condPending matrixCondition = "pending"
	condError   matrixCondition = "error"
	condTimeout matrixCondition = "timeout"
	// condOwnershipLost is a green gate whose guarded close lost the race
	// while the issue is still in progress.
	condOwnershipLost matrixCondition = "ownership-lost"
	// condOwnershipLostClosed is the same race, but the winner already closed.
	condOwnershipLostClosed matrixCondition = "ownership-lost-already-closed"
)

// matrixRunner builds a runner for kind in condition cond.
func matrixRunner(t *testing.T, kind string, cond matrixCondition, hub *fakeHub, kv *memKV) *gate.Runner {
	t.Helper()
	dir := t.TempDir()
	var r *gate.Runner
	switch kind {
	case "shell":
		writeFormula(t, dir, "g", "type = \"shell\"\ncommand = \"true\"")
		fe := &fakeExec{}
		fe.fn = func(string, []string) (string, error) {
			switch cond {
			case condRed:
				return "", &gate.ExitFailure{Code: 3, Output: "boom\n"}
			case condError:
				return "", errors.New("sh: not found")
			}
			return "fine\n", nil
		}
		exec := fe.exec
		if cond == condTimeout {
			exec = func(ctx context.Context, _ string, _ ...string) (string, error) {
				<-ctx.Done()
				return "", ctx.Err()
			}
		}
		r = newTestRunner(t, dir, hub, kv, exec, nil)
	case "gh:run", "gh:pr":
		if kind == "gh:run" {
			writeFormula(t, dir, "g", "type = \"gh:run\"\nref = \""+fullSha+"\"")
		} else {
			writeFormula(t, dir, "g", "type = \"gh:pr\"\npr = \"7\"")
		}
		var gh gate.GHChecker
		switch cond {
		case condRed:
			gh = &fakeGH{result: gate.GHFail}
		case condPending:
			gh = &fakeGH{result: gate.GHPending}
		case condError:
			gh = &fakeGH{err: errors.New("gh: api unreachable")}
		case condTimeout:
			gh = blockingGH{}
		default:
			gh = &fakeGH{result: gate.GHPass}
		}
		r = newTestRunner(t, dir, hub, kv, nil, gh)
	default:
		t.Fatalf("unknown kind %q", kind)
	}
	r.CheckTimeout = 30 * time.Millisecond
	switch cond {
	case condOwnershipLost:
		hub.closeErr = fmt.Errorf("close: %w", gate.ErrCloseRace)
		hub.statuses[testIssue().ID] = "in_progress"
	case condOwnershipLostClosed:
		hub.closeErr = fmt.Errorf("close: %w", gate.ErrCloseRace)
		hub.statuses[testIssue().ID] = gate.StatusClosed
	}
	return r
}

// TestGateBoundaryMatrix crosses every gate kind with every terminal
// condition and pins the verdict plus the exact hub writeback and counter
// state, so a change in any cell of the matrix fails by name.
func TestGateBoundaryMatrix(t *testing.T) {
	type want struct {
		verdict    symphony.GateVerdict
		closes     int
		comments   int
		tombstone  bool
		errCounter string // MetaGateErrors after evaluation
		deferCount string // MetaGateDeferrals after evaluation
	}
	id := testIssue().ID
	wants := map[matrixCondition]want{
		condGreen:               {verdict: symphony.GateGreen, closes: 1, tombstone: true, errCounter: "0", deferCount: "0"},
		condRed:                 {verdict: symphony.GateRed, comments: 1, errCounter: "0", deferCount: "0"},
		condPending:             {verdict: symphony.GateErr, deferCount: "1"},
		condError:               {verdict: symphony.GateErr, errCounter: "1"},
		condTimeout:             {verdict: symphony.GateErr, errCounter: "1"},
		condOwnershipLost:       {verdict: symphony.GateErr},
		condOwnershipLostClosed: {verdict: symphony.GateGreen},
	}
	for _, kind := range []string{"shell", "gh:run", "gh:pr"} {
		for _, cond := range []matrixCondition{condGreen, condRed, condPending, condError, condTimeout, condOwnershipLost, condOwnershipLostClosed} {
			if kind == "shell" && cond == condPending {
				continue // a shell command has no pending state
			}
			w := wants[cond]
			t.Run(kind+"/"+string(cond), func(t *testing.T) {
				hub, kv := newFakeHub(), newMemKV()
				r := matrixRunner(t, kind, cond, hub, kv)
				start := time.Now()
				out, err := r.Evaluate(t.Context(), reqWithWorkspace(t, 1, symphony.Revision("wf-1")))
				if err != nil {
					t.Fatal(err)
				}
				if out.Verdict != w.verdict {
					t.Fatalf("verdict = %d, want %d (output %q)", out.Verdict, w.verdict, out.Output)
				}
				if cond == condTimeout && time.Since(start) > 20*time.Second {
					t.Errorf("timeout cell took %v: CheckTimeout not enforced", time.Since(start))
				}
				if got := len(hub.closes); got != w.closes && cond != condOwnershipLost && cond != condOwnershipLostClosed {
					t.Errorf("closes = %d, want %d", got, w.closes)
				}
				if got := len(hub.comments); got != w.comments {
					t.Errorf("comments = %d, want %d", got, w.comments)
				}
				if hub.tombstones[id] != w.tombstone {
					t.Errorf("tombstoned = %v, want %v", hub.tombstones[id], w.tombstone)
				}
				if len(hub.needHuman) != 0 || len(hub.defers) != 0 {
					t.Errorf("unexpected parking: needsHuman=%v defers=%v", hub.needHuman, hub.defers)
				}
				if got, _ := kv.Get(t.Context(), id, gate.MetaGateErrors); got != w.errCounter {
					t.Errorf("error counter = %q, want %q", got, w.errCounter)
				}
				if got, _ := kv.Get(t.Context(), id, gate.MetaGateDeferrals); got != w.deferCount {
					t.Errorf("deferral counter = %q, want %q", got, w.deferCount)
				}
				if cond == condGreen && (hub.closes[0].ifAssignee != "lambe@test" || hub.closes[0].issue != id) {
					t.Errorf("close not guarded by the actor claim: %+v", hub.closes[0])
				}
			})
		}
	}
}

// TestEvaluateUnknownGateKindIsInfraError pins that a snapshot carrying a
// kind this build does not know is an error verdict, never a green.
func TestEvaluateUnknownGateKindIsInfraError(t *testing.T) {
	hub, kv := newFakeHub(), newMemKV()
	r := newTestRunner(t, t.TempDir(), hub, kv, nil, nil)
	req := reqWithWorkspace(t, 1, symphony.Revision("wf-1"))
	req.GateSpec = []byte(`[{"Name":"future","Config":{"Kind":99}}]`)
	out, err := r.Evaluate(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if out.Verdict != symphony.GateErr || !strings.Contains(out.Output, "unknown gate kind") {
		t.Fatalf("outcome = %+v, want error verdict naming the unknown kind", out)
	}
	if len(hub.closes) != 0 || hub.tombstones[testIssue().ID] {
		t.Fatal("unknown gate kind must not close or tombstone")
	}
	if v, _ := kv.Get(t.Context(), testIssue().ID, gate.MetaGateErrors); v != "1" {
		t.Fatalf("error counter = %q, want 1", v)
	}
}

// TestEvaluateMalformedGateSnapshotIsInfraError pins that undecodable
// checkpoint gate config is an error verdict rather than falling through to
// the live directory.
func TestEvaluateMalformedGateSnapshotIsInfraError(t *testing.T) {
	dir := t.TempDir()
	writeFormula(t, dir, "g", "type = \"shell\"\ncommand = \"true\"")
	hub := newFakeHub()
	r := newTestRunner(t, dir, hub, newMemKV(), (&fakeExec{}).exec, nil)
	req := reqWithWorkspace(t, 1, symphony.Revision("wf-1"))
	req.GateSpec = []byte(`{not json`)
	out, err := r.Evaluate(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if out.Verdict != symphony.GateErr || !strings.Contains(out.Output, "gate config unreadable") {
		t.Fatalf("outcome = %+v, want gate config unreadable error", out)
	}
	if len(hub.closes) != 0 {
		t.Fatal("malformed snapshot must not close the issue")
	}
}

// TestMultiGateErrorDominatesRed pins that one unobservable gate cannot be
// masked by another gate's red.
func TestMultiGateErrorDominatesRed(t *testing.T) {
	dir := t.TempDir()
	writeFormula(t, dir, "a-red", "type = \"shell\"\ncommand = \"false\"")
	writeFormula(t, dir, "b-ci", "type = \"gh:pr\"\npr = \"7\"")
	hub, kv := newFakeHub(), newMemKV()
	fe := &fakeExec{fn: func(string, []string) (string, error) { return "", &gate.ExitFailure{Code: 1, Output: "red"} }}
	r := newTestRunner(t, dir, hub, kv, fe.exec, &fakeGH{err: errors.New("api down")})
	out, err := r.Evaluate(t.Context(), reqWithWorkspace(t, 1, symphony.Revision("wf-1")))
	if err != nil {
		t.Fatal(err)
	}
	if out.Verdict != symphony.GateErr {
		t.Fatalf("verdict = %d, want error to dominate red", out.Verdict)
	}
	if len(hub.comments) != 0 || len(hub.closes) != 0 {
		t.Fatalf("no red comment or close while a gate is unobservable: %+v %+v", hub.comments, hub.closes)
	}
}

// ---- FinalizeCompletion ----

type tombstoneFailHub struct {
	*fakeHub
	err error
}

func (h tombstoneFailHub) Tombstone(context.Context, string) error { return h.err }

type putFailKV struct {
	*memKV
	err error
}

func (k putFailKV) Put(context.Context, string, string, string) error { return k.err }

func TestFinalizeCompletion(t *testing.T) {
	id := testIssue().ID
	t.Run("tombstones and clears streaks", func(t *testing.T) {
		hub, kv := newFakeHub(), newMemKV()
		_ = kv.Put(t.Context(), id, gate.MetaGateErrors, "3")
		_ = kv.Put(t.Context(), id, gate.MetaGateDeferrals, "4")
		r := newTestRunner(t, t.TempDir(), hub, kv, nil, nil)
		if err := r.FinalizeCompletion(t.Context(), id); err != nil {
			t.Fatal(err)
		}
		if !hub.tombstones[id] {
			t.Error("issue not tombstoned")
		}
		for _, key := range []string{gate.MetaGateErrors, gate.MetaGateDeferrals} {
			if v, _ := kv.Get(t.Context(), id, key); v != "0" {
				t.Errorf("%s = %q, want 0", key, v)
			}
		}
		if len(hub.closes)+len(hub.comments)+len(hub.needHuman) != 0 {
			t.Error("repair must only tombstone and reset; the issue is already closed")
		}
	})
	t.Run("tombstone failure is returned and counters untouched", func(t *testing.T) {
		boom := errors.New("hub down")
		kv := newMemKV()
		_ = kv.Put(t.Context(), id, gate.MetaGateErrors, "3")
		hub := tombstoneFailHub{fakeHub: newFakeHub(), err: boom}
		r := gate.NewRunner(t.TempDir(), hub, gate.NewEscalator(kv), "a", discardLog())
		if err := r.FinalizeCompletion(t.Context(), id); !errors.Is(err, boom) {
			t.Fatalf("err = %v, want hub error", err)
		}
		if v, _ := kv.Get(t.Context(), id, gate.MetaGateErrors); v != "3" {
			t.Errorf("counter = %q, want 3 untouched after failed tombstone", v)
		}
	})
	t.Run("counter reset failure is returned", func(t *testing.T) {
		boom := errors.New("kv down")
		hub := newFakeHub()
		r := gate.NewRunner(t.TempDir(), hub, gate.NewEscalator(putFailKV{memKV: newMemKV(), err: boom}), "a", discardLog())
		if err := r.FinalizeCompletion(t.Context(), id); !errors.Is(err, boom) {
			t.Fatalf("err = %v, want kv error", err)
		}
		if !hub.tombstones[id] {
			t.Error("tombstone should have been written before the reset failed")
		}
	})
	t.Run("nil KV is a no-op reset", func(t *testing.T) {
		hub := newFakeHub()
		r := gate.NewRunner(t.TempDir(), hub, gate.NewEscalator(nil), "a", discardLog())
		if err := r.FinalizeCompletion(t.Context(), id); err != nil {
			t.Fatal(err)
		}
	})
}

// ---- escalation caps ----

func TestEscalatorCapReached(t *testing.T) {
	tests := []struct {
		name    string
		cap     int
		attempt int
		want    bool
	}{
		{"below custom cap", 3, 2, false},
		{"at custom cap", 3, 3, true},
		{"above custom cap", 3, 9, true},
		{"zero cap uses default below", 0, gate.DefaultAttemptCap - 1, false},
		{"zero cap uses default at", 0, gate.DefaultAttemptCap, true},
		{"negative cap uses default", -4, gate.DefaultAttemptCap, true},
		{"attempt zero", 3, 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := &gate.Escalator{AttemptCap: tc.cap}
			if got := e.CapReached(tc.attempt); got != tc.want {
				t.Errorf("CapReached(%d) with cap %d = %v, want %v", tc.attempt, tc.cap, got, tc.want)
			}
		})
	}
}

type getFailKV struct{ err error }

func (k getFailKV) Get(context.Context, string, string) (string, error) { return "", k.err }
func (k getFailKV) Put(context.Context, string, string, string) error   { return nil }

func TestEscalatorRecordBoundaries(t *testing.T) {
	ctx := t.Context()
	t.Run("threshold is inclusive", func(t *testing.T) {
		e := &gate.Escalator{KV: newMemKV(), ErrorThreshold: 3}
		for i := 1; i <= 3; i++ {
			hit, err := e.RecordGateError(ctx, "i")
			if err != nil {
				t.Fatal(err)
			}
			if hit != (i == 3) {
				t.Fatalf("call %d: escalated = %v", i, hit)
			}
		}
	})
	t.Run("corrupt stored count restarts the streak", func(t *testing.T) {
		kv := newMemKV()
		_ = kv.Put(ctx, "i", gate.MetaGateDeferrals, "not-a-number")
		e := &gate.Escalator{KV: kv, DeferThreshold: 2}
		if hit, err := e.RecordDeferral(ctx, "i"); err != nil || hit {
			t.Fatalf("hit=%v err=%v, want first deferral below threshold", hit, err)
		}
		if v, _ := kv.Get(ctx, "i", gate.MetaGateDeferrals); v != "1" {
			t.Fatalf("count = %q, want 1", v)
		}
	})
	t.Run("kv read failure surfaces and does not escalate", func(t *testing.T) {
		boom := errors.New("read failed")
		e := &gate.Escalator{KV: getFailKV{err: boom}, ErrorThreshold: 1}
		hit, err := e.RecordGateError(ctx, "i")
		if !errors.Is(err, boom) || hit {
			t.Fatalf("hit=%v err=%v, want read error and no escalation", hit, err)
		}
	})
	t.Run("nil KV never escalates", func(t *testing.T) {
		e := &gate.Escalator{ErrorThreshold: 1}
		if hit, err := e.RecordGateError(ctx, "i"); hit || err != nil {
			t.Fatalf("hit=%v err=%v", hit, err)
		}
	})
}

// TestDecidedGateResetsUnderdecidedStreak pins that a red verdict between
// infra errors restarts the streak, so errors separated by a decision never
// add up to an escalation.
func TestDecidedGateResetsUnderdecidedStreak(t *testing.T) {
	dir := t.TempDir()
	writeFormula(t, dir, "g", "type = \"shell\"\ncommand = \"x\"")
	hub, kv := newFakeHub(), newMemKV()
	mode := "error"
	fe := &fakeExec{fn: func(string, []string) (string, error) {
		if mode == "red" {
			return "", &gate.ExitFailure{Code: 1, Output: "red"}
		}
		return "", errors.New("infra")
	}}
	r := newTestRunner(t, dir, hub, kv, fe.exec, nil)
	eval := func(attempt int) {
		t.Helper()
		if _, err := r.Evaluate(t.Context(), reqWithWorkspace(t, attempt, symphony.Revision("wf-1"))); err != nil {
			t.Fatal(err)
		}
	}
	for range gate.DefaultEscalationThreshold - 1 {
		eval(1)
	}
	mode = "red"
	eval(1)
	mode = "error"
	for range gate.DefaultEscalationThreshold - 1 {
		eval(1)
	}
	if len(hub.needHuman) != 0 {
		t.Fatalf("escalated across a decided gate: %+v", hub.needHuman)
	}
	eval(1) // fifth consecutive error since the reset
	if len(hub.needHuman) != 1 || !strings.Contains(hub.needHuman[0].reason, "gate errors") {
		t.Fatalf("needs-human = %+v, want one error-streak escalation", hub.needHuman)
	}
}

// TestEscalationReasonDistinguishesStreaks pins the operator-visible reason
// text for the two streak kinds.
func TestEscalationReasonDistinguishesStreaks(t *testing.T) {
	for _, tc := range []struct {
		name    string
		gh      *fakeGH
		wantSub string
	}{
		{"errors", &fakeGH{err: errors.New("x")}, "gate errors"},
		{"deferrals", &fakeGH{result: gate.GHPending}, "gate deferrals"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFormula(t, dir, "g", "type = \"gh:pr\"\npr = \"7\"")
			hub := newFakeHub()
			r := newTestRunner(t, dir, hub, newMemKV(), nil, tc.gh)
			r.Esc.ErrorThreshold, r.Esc.DeferThreshold = 1, 1
			if _, err := r.Evaluate(t.Context(), reqWithWorkspace(t, 1, symphony.Revision("wf-1"))); err != nil {
				t.Fatal(err)
			}
			if len(hub.needHuman) != 1 || !strings.Contains(hub.needHuman[0].reason, tc.wantSub) {
				t.Fatalf("needs-human = %+v, want reason containing %q", hub.needHuman, tc.wantSub)
			}
		})
	}
}

// ---- small value types ----

func TestKindAndExitFailureStrings(t *testing.T) {
	for in, want := range map[string]gate.Kind{"shell": gate.KindShell, "gh:run": gate.KindGhRun, "gh:pr": gate.KindGhPr} {
		k, err := gate.ParseKind(in)
		if err != nil || k != want || k.String() != in {
			t.Errorf("ParseKind(%q) = %v, %v; String() = %q", in, k, err, k.String())
		}
	}
	if k, err := gate.ParseKind("bogus"); err == nil || k != gate.KindInvalid || k.String() != "invalid" {
		t.Errorf("ParseKind(bogus) = %v, %v", k, err)
	}
	e := &gate.ExitFailure{Code: 7, Output: "nope"}
	if got := e.Error(); got != "exit 7: nope" {
		t.Errorf("ExitFailure.Error() = %q", got)
	}
}
