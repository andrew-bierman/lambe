package daemon

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/andrew-bierman/lambe/internal/httpstate"
	"github.com/andrew-bierman/lambe/internal/tracker"
)

func TestRenderStatusJSONIsTheSnapshotShape(t *testing.T) {
	now := time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)
	since := now.Add(-time.Hour)
	rep := Report{
		Active:     []ActiveRun{{Issue: "T-1", Session: "T-1", Attempt: 2, Assignee: "lambe@box"}},
		NeedsHuman: []NeedsHumanItem{{Issue: "T-2", Reason: "gate red twice", LastGateOutput: "exit 1", Since: since}},
		// Queue and Deferred are not part of the snapshot DTO.
		Queue:          []tracker.ReadinessRow{{ID: "T-3"}},
		GateRejections: []GateRejection{{Issue: "T-4", Attempt: 1, Output: "no"}},
	}
	id := httpstate.Daemon{Machine: "box", Version: "v9.9.9", ConfigPath: "/cfg/lambe.yaml", Actor: "lambe@box"}
	var buf bytes.Buffer
	if err := RenderStatusJSON(&buf, rep, id, 5*time.Minute, now); err != nil {
		t.Fatal(err)
	}

	// Strict decode into the daemon's own DTO: any field the wire contract
	// does not define would fail here.
	var got httpstate.SnapshotResponse
	dec := json.NewDecoder(bytes.NewReader(buf.Bytes()))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&got); err != nil {
		t.Fatalf("not a SnapshotResponse: %v\n%s", err, buf.String())
	}
	if got.SchemaVersion != httpstate.SchemaVersion {
		t.Errorf("schema_version = %d", got.SchemaVersion)
	}
	if got.Daemon.PID != 0 || got.Daemon.Identity != "" {
		t.Errorf("CLI is not the daemon; pid/identity must be unset: %+v", got.Daemon)
	}
	if got.Daemon.Machine != "box" || got.Daemon.Version != "v9.9.9" || got.Daemon.ConfigPath != "/cfg/lambe.yaml" {
		t.Errorf("daemon block = %+v", got.Daemon)
	}
	if len(got.Runs) != 1 || got.Runs[0].Issue != "T-1" || got.Runs[0].Attempt != 2 ||
		got.Runs[0].State != "running" || got.Runs[0].Actor != "lambe@box" ||
		got.Runs[0].LastEventAge != "n/a" || got.Runs[0].Stalled {
		t.Errorf("runs = %+v", got.Runs)
	}
	if len(got.NeedsHuman) != 1 || got.NeedsHuman[0].Reason != "gate red twice" || got.NeedsHuman[0].Since != since.Format(time.RFC3339) {
		t.Errorf("needs_human = %+v", got.NeedsHuman)
	}
	if got.NeedsHumanAsOf != now.Format(time.RFC3339) || got.RunsAsOf != now.Format(time.RFC3339) {
		t.Errorf("as_of stamps = %q %q", got.RunsAsOf, got.NeedsHumanAsOf)
	}
	// No daemon was asked: nothing may claim a live loop or a gate history.
	if got.TickAt != "" || got.GateRejectionsAsOf != "" || len(got.GateRejections) != 0 {
		t.Errorf("unknowns must stay unset: tick=%q gate_as_of=%q gates=%v", got.TickAt, got.GateRejectionsAsOf, got.GateRejections)
	}
	if got.StallAfterSeconds != 300 {
		t.Errorf("stall_after_seconds = %d", got.StallAfterSeconds)
	}
	if !strings.HasSuffix(buf.String(), "}\n") {
		t.Errorf("want one JSON document and a newline: %q", buf.String())
	}
}

func TestRenderStatusJSONEmptyReport(t *testing.T) {
	var buf bytes.Buffer
	if err := RenderStatusJSON(&buf, Report{}, httpstate.Daemon{}, time.Minute, time.Now()); err != nil {
		t.Fatal(err)
	}
	var got httpstate.SnapshotResponse
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Runs) != 0 || len(got.NeedsHuman) != 0 {
		t.Errorf("empty report invented state: %+v", got)
	}
}
