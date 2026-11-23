package tracker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ShellTransport runs the bd CLI as a subprocess per operation. It is the
// transport for embedded local trackers; each call pays engine startup, so
// callers should prefer batched writes where available.
type ShellTransport struct {
	// Bin is the bd binary path.
	Bin string
	// Dir is the working directory whose .beads/ (or hub clone) to act on.
	Dir string
	// Actor is the stable identity stamped on every write, e.g.
	// "lambe@host". Empty means bd's default resolution.
	Actor string
	// Env carries extra environment entries (KEY=VALUE) appended after the
	// transport's own BD_JSON_ENVELOPE and BEADS_ACTOR.
	Env []string
	// CommandTimeout bounds each bd invocation's run, overriding the
	// process-wide Limits for this transport; zero or negative defers to
	// SetLimits (default DefaultCommandTimeout). The clock starts when the
	// subprocess does, never while the call waits for a slot. A call whose
	// bd subprocess outlives the bound fails with (an error wrapping)
	// context.DeadlineExceeded.
	CommandTimeout time.Duration
}

// commandTimeout resolves the effective per-run bound for a lane.
func (t *ShellTransport) commandTimeout(lane Lane) time.Duration {
	if t.CommandTimeout > 0 {
		return t.CommandTimeout
	}
	return currentLimits().runTimeout(lane)
}

// NewShellTransport returns a subprocess transport for the bd binary at bin
// operating on dir, writing as actor.
func NewShellTransport(bin, dir, actor string) *ShellTransport {
	return &ShellTransport{Bin: bin, Dir: dir, Actor: actor}
}

// run executes one bd invocation: it waits for a subprocess slot in lane
// (bounded by the queue-wait limit), then runs bd under the per-run timeout
// and splits success from exit failure. A subprocess killed by the bound
// surfaces as an error wrapping context.DeadlineExceeded so callers can
// classify it (the daemon maps it to CategoryUnavailable).
func (t *ShellTransport) run(ctx context.Context, lane Lane, args []string, extraEnv ...string) (Result, error) {
	release, err := bdGate.acquire(ctx, lane, currentLimits().queueWait())
	if err != nil {
		return Result{}, fmt.Errorf("run bd %s: %w", strings.Join(args, " "), err)
	}
	defer release()
	if len(args) > 0 {
		bdStats.add(args[0])
	}
	callCtx, cancel := context.WithTimeout(ctx, t.commandTimeout(lane))
	defer cancel()
	cmd := exec.CommandContext(callCtx, t.Bin, args...) // #nosec G204 G702 -- args are constructed in this package from typed requests, never user input
	// A killed bd's orphaned descendants can inherit the stdout pipe and
	// hold it open; WaitDelay stops Wait from blocking on them after the
	// process (or the deadline) is gone.
	cmd.WaitDelay = time.Second
	cmd.Dir = t.Dir
	env := append([]string{"BD_JSON_ENVELOPE=1"}, t.Env...)
	env = append(env, extraEnv...)
	if t.Actor != "" {
		env = append(env, "BEADS_ACTOR="+t.Actor)
	}
	cmd.Env = mergeEnv(os.Environ(), env)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if cerr := callCtx.Err(); cerr != nil {
			return Result{}, fmt.Errorf("run bd %s: %w", strings.Join(args, " "), cerr)
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return Result{}, &ExitError{Code: exitErr.ExitCode(), Stdout: stdout.String(), Stderr: stderr.String()}
		}
		return Result{}, fmt.Errorf("run bd %s: %w", strings.Join(args, " "), err)
	}
	return Result{Data: unwrapEnvelope(stdout.Bytes()), Raw: stdout.Bytes()}, nil
}

// environ is indirected for tests.
// sortedPairs returns the map's pairs sorted by key so command lines are
// deterministic.
func sortedPairs(m map[string]string) [][2]string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	pairs := make([][2]string, 0, len(m))
	for _, k := range keys {
		pairs = append(pairs, [2]string{k, m[k]})
	}
	return pairs
}

// mergeEnv appends overrides to base, replacing same-key entries.
func mergeEnv(base, overrides []string) []string {
	out := make([]string, 0, len(base)+len(overrides))
	prefixes := make(map[string]bool, len(overrides))
	for _, kv := range overrides {
		if k, _, ok := strings.Cut(kv, "="); ok {
			prefixes[k+"="] = true
		}
	}
	for _, kv := range base {
		if !overridesKey(prefixes, kv) {
			out = append(out, kv)
		}
	}
	return append(out, overrides...)
}

func overridesKey(prefixes map[string]bool, kv string) bool {
	for p := range prefixes {
		if strings.HasPrefix(kv, p) {
			return true
		}
	}
	return false
}

// unwrapEnvelope extracts the data payload from bd's {"data": ...} JSON
// envelope, passing non-envelope bytes through untouched.
func unwrapEnvelope(b []byte) json.RawMessage {
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(b), &env); err == nil && env.Data != nil {
		return env.Data
	}
	return json.RawMessage(bytes.TrimSpace(b))
}

// List implements Transport. The single status "ready" selects bd's
// blocker-aware ready-front read (--ready), which is also the read that
// lazily wakes deferred issues whose wake time has passed.
func (t *ShellTransport) List(ctx context.Context, q DispatchQuery) (Result, error) {
	args := []string{"list", "--json"}
	if len(q.IDs) > 0 {
		args = append(args, "--id", strings.Join(q.IDs, ","))
	}
	if len(q.Statuses) == 1 && q.Statuses[0] == StatusReady {
		args = append(args, "--ready")
	} else if len(q.Statuses) > 0 {
		args = append(args, "--status", strings.Join(q.Statuses, ","))
	}
	if q.Unlimited {
		args = append(args, "--limit", "0")
	}
	if q.IncludeClosed {
		args = append(args, "--all")
	}
	for _, kv := range sortedPairs(q.Metadata) {
		args = append(args, "--metadata-field", kv[0]+"="+kv[1])
	}
	if len(q.Labels) > 0 {
		args = append(args, "--label", strings.Join(q.Labels, ","))
	}
	return t.run(ctx, LaneRead, args)
}

// Show implements Transport.
func (t *ShellTransport) Show(ctx context.Context, id string) (Result, error) {
	return t.run(ctx, LaneRead, []string{"show", "--json", id})
}

// Create implements Transport.
func (t *ShellTransport) Create(ctx context.Context, r CreateRequest) (Result, error) {
	args := []string{"create", "--title", r.Title, "--json", "--priority", strconv.Itoa(r.Priority)}
	if r.ID != "" {
		args = append(args, "--id", r.ID)
	}
	if r.Type != "" {
		args = append(args, "--type", r.Type)
	}
	if len(r.Labels) > 0 {
		args = append(args, "--labels", strings.Join(r.Labels, ","))
	}
	if len(r.Metadata) > 0 {
		m, err := json.Marshal(r.Metadata)
		if err != nil {
			return Result{}, fmt.Errorf("marshal metadata: %w", err)
		}
		args = append(args, "--metadata", string(m))
	}
	if r.Status != "" {
		args = append(args, "--status", r.Status)
	}
	if r.Ephemeral {
		args = append(args, "--ephemeral")
	}
	return t.run(ctx, LaneWrite, args)
}

// Update implements Transport.
func (t *ShellTransport) Update(ctx context.Context, r UpdateRequest) (Result, error) {
	args := []string{"update", r.ID, "--json"}
	if r.Claim {
		args = append(args, "--claim")
	}
	if r.IfAssignee != nil {
		args = append(args, "--if-assignee", *r.IfAssignee)
	}
	if r.Assignee != "" {
		args = append(args, "--assignee", r.Assignee)
	}
	if r.Status != "" {
		args = append(args, "--status", r.Status)
	}
	for _, kv := range sortedPairs(r.Metadata) {
		args = append(args, "--set-metadata", kv[0]+"="+kv[1])
	}
	if r.DeferUntil != nil {
		args = append(args, "--defer", r.DeferUntil.UTC().Format(time.RFC3339))
	}
	if r.DeferClear {
		args = append(args, "--defer", "")
	}
	return t.run(ctx, LaneWrite, args)
}

// Defer implements Transport. A nil until is an indefinite icebox.
func (t *ShellTransport) Defer(ctx context.Context, id string, until *time.Time, reason string) (Result, error) {
	args := []string{"defer", id, "--json"}
	if until != nil {
		args = append(args, "--until", until.UTC().Format(time.RFC3339))
	}
	if reason != "" {
		args = append(args, "--reason", reason)
	}
	return t.run(ctx, LaneWrite, args)
}

// Heartbeat implements Transport.
func (t *ShellTransport) Heartbeat(ctx context.Context, id string) (Result, error) {
	return t.run(ctx, LaneBackground, []string{"heartbeat", id, "--json"})
}

// Unclaim implements Transport. A non-empty ifAssignee guards the release:
// a mismatch fails rather than touching another actor's claim.
func (t *ShellTransport) Unclaim(ctx context.Context, id string, ifAssignee string) (Result, error) {
	args := []string{"unclaim", id}
	if ifAssignee != "" {
		args = append(args, "--if-assignee", ifAssignee)
	}
	return t.run(ctx, LaneWrite, args)
}

// Reclaim implements Transport.
func (t *ShellTransport) Reclaim(ctx context.Context, olderThan time.Duration, ids []string) (Result, error) {
	args := []string{"reclaim", "--older-than", olderThan.String(), "--json"}
	for _, id := range ids {
		args = append(args, "--id", id)
	}
	return t.run(ctx, LaneWrite, args)
}

// Batch implements Transport. All ops render into a temp batch file applied
// by a single `bd batch -f` invocation: one engine start, one DOLT_COMMIT,
// rolled back entirely on any error. Render failures return before bd runs.
func (t *ShellTransport) Batch(ctx context.Context, ops []BatchOp) (Result, error) {
	if len(ops) == 0 {
		return Result{}, nil
	}
	var sb strings.Builder
	for i, op := range ops {
		line, err := op.Render()
		if err != nil {
			return Result{}, fmt.Errorf("batch op %d: %w", i+1, err)
		}
		sb.WriteString(line)
		sb.WriteByte('\n')
	}
	path := filepath.Join(os.TempDir(), fmt.Sprintf("lambe-bd-batch-%d-%d.bd", os.Getpid(), time.Now().UnixNano()))
	if err := os.WriteFile(path, []byte(sb.String()), 0o600); err != nil {
		return Result{}, fmt.Errorf("write batch file: %w", err)
	}
	defer os.Remove(path) //nolint:errcheck // best-effort cleanup of a temp file
	return t.run(ctx, LaneWrite, []string{"batch", "-f", path, "--json"})
}

// Events implements Transport. BD_EVENTS_JOURNAL=1 makes the journal record
// mutations even when the workspace default is off.
func (t *ShellTransport) Events(ctx context.Context, since int64) (Result, error) {
	return t.run(ctx, LaneRead, []string{"events", "tail", "--since", strconv.FormatInt(since, 10), "--json"}, "BD_EVENTS_JOURNAL=1")
}

// Version implements Transport, returning bd's version string.
func (t *ShellTransport) Version(ctx context.Context) (string, error) {
	res, err := t.run(ctx, LaneRead, []string{"--version"})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(res.Raw)), nil
}

// ConfigGet implements Transport, returning the raw config value.
func (t *ShellTransport) ConfigGet(ctx context.Context, key string) (string, error) {
	res, err := t.run(ctx, LaneRead, []string{"config", "get", key})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(res.Raw)), nil
}
