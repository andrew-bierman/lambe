// SPDX-License-Identifier: GPL-3.0-or-later

package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestJSONOutputWithAttrs(t *testing.T) {
	var buf bytes.Buffer
	log := New(&buf, slog.LevelInfo)

	log = WithIssue(log, "ISSUE-1")
	log = WithSession(log, "sess-7")
	log = WithAttempt(log, 3)
	log = WithPgid(log, 4242)
	log.Info("supervisor loop started")

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("output is not JSON: %v (%s)", err, buf.String())
	}
	for key, want := range map[string]any{
		"msg":     "supervisor loop started",
		"issue":   "ISSUE-1",
		"session": "sess-7",
		"attempt": float64(3),
		"pgid":    float64(4242),
	} {
		if rec[key] != want {
			t.Errorf("attr %q: got %v, want %v", key, rec[key], want)
		}
	}
}

func TestConstantMessageNotInterpolated(t *testing.T) {
	var buf bytes.Buffer
	log := New(&buf, slog.LevelInfo)
	log.Warn("attempt failed", slog.Int("attempt", 9))

	if strings.Contains(buf.String(), "attempt failed 9") {
		t.Errorf("message should stay constant, got %s", buf.String())
	}
	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatal(err)
	}
	if rec["msg"] != "attempt failed" {
		t.Errorf("msg: got %v", rec["msg"])
	}
}

func TestDefaultLoggerLevels(t *testing.T) {
	log := Default()
	if log == nil {
		t.Fatal("Default() returned nil")
	}
	if !log.Enabled(context.Background(), slog.LevelInfo) {
		t.Error("Default() should enable info")
	}
	if log.Enabled(context.Background(), slog.LevelDebug) {
		t.Error("Default() should disable debug")
	}
}
