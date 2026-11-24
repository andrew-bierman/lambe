package gate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/andrew-bierman/lambe/internal/tracker"
)

// Issue statuses lambe writes.
const (
	// StatusClosed is bd's terminal closed status.
	StatusClosed = tracker.StatusClosed
	// StatusNeedsHuman parks an issue for human attention. bd requires
	// custom statuses to be configured; see BdHub.EnsureNeedsHumanStatus.
	StatusNeedsHuman = tracker.StatusNeedsHuman
)

// MetaTombstone marks an issue whose green completion was cleaned up, so
// a purged id is not re-dispatched.
const MetaTombstone = "lambe_gate_tombstone"

// MetaNeedsHumanReason is the metadata key carrying why an issue was
// parked for human attention. Every needs-human write stamps it — the
// primary path and the blocked fallback alike — so the served attention
// view always has an authoritative reason to read.
const MetaNeedsHumanReason = "lambe_needs_human_reason"

// MetaLastGateOutput is the metadata key carrying the escalation's last
// gate output. Every needs-human write stamps it alongside
// MetaNeedsHumanReason — the primary path and the blocked fallback
// alike — so the served attention view always has the gate-output
// record to read.
const MetaLastGateOutput = "lambe_last_gate_output"

// ErrCloseRace reports a CAS-guarded close whose assignee guard no longer
// held: another actor won, nothing was written.
var ErrCloseRace = errors.New("gate close lost race")

// Hub is the bd writeback surface the gate layer owns: comments,
// CAS-guarded close, gate-retry defers, needs-human transitions, and
// dispatch tombstones. Everything it writes is readable through bd.
type Hub interface {
	// Comment appends a comment to the issue.
	Comment(ctx context.Context, issue, body string) error
	// Close moves the issue to closed, guarded by ifAssignee when
	// non-empty. A lost guard race returns an error wrapping
	// ErrCloseRace.
	Close(ctx context.Context, issue, ifAssignee, reason string) error
	// Defer parks the issue until until with a reason.
	Defer(ctx context.Context, issue string, until time.Time, reason string) error
	// NeedsHuman moves the issue to the needs-human status and comments
	// the reason together with the last gate output.
	NeedsHuman(ctx context.Context, issue, reason, lastGateOutput string) error
	// Tombstone marks the issue as completed-and-cleaned.
	Tombstone(ctx context.Context, issue string) error
	// IsTombstoned reports the tombstone state.
	IsTombstoned(ctx context.Context, issue string) (bool, error)
	// IssueStatus reports the issue's current tracker status. It is the
	// close-race reconcile read: when a guarded close loses its race, the
	// runner re-reads the status to decide whether the race winner
	// already moved the issue terminal.
	IssueStatus(ctx context.Context, issue string) (tracker.Status, error)
}

// BdHub implements Hub (and KV) over the bd CLI in one tracker directory.
type BdHub struct {
	bin   string
	dir   string
	actor string
	tr    *tracker.ShellTransport
}

// NewBdHub returns a hub over the bd binary at bin (empty means PATH
// lookup) operating on dir and writing as actor.
func NewBdHub(bin, dir, actor string) (*BdHub, error) {
	bin, err := tracker.ResolveBdBin(bin)
	if err != nil {
		return nil, err
	}
	return &BdHub{bin: bin, dir: dir, actor: actor, tr: tracker.NewShellTransport(bin, dir, actor)}, nil
}

// run executes one bd invocation outside the typed transport surface.
func (h *BdHub) run(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, h.bin, args...) // #nosec G204 -- fixed argv over the configured bd binary
	cmd.Dir = h.dir
	cmd.WaitDelay = time.Second // a descendant holding the pipe must not outlive ctx
	if h.actor != "" {
		cmd.Env = append(cmd.Environ(), "BEADS_ACTOR="+h.actor)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("bd %s: %w: %s", args[0], err, out)
	}
	return nil
}

// EnsureNeedsHumanStatus configures bd's custom statuses to accept the
// needs-human value. It is idempotent.
func (h *BdHub) EnsureNeedsHumanStatus(ctx context.Context) error {
	return h.run(ctx, "config", "set", "status.custom", StatusNeedsHuman.String())
}

// Comment implements Hub.
func (h *BdHub) Comment(ctx context.Context, issue, body string) error {
	return h.run(ctx, "comment", issue, body)
}

// Close implements Hub: a guarded status move to closed. bd reports a
// lost assignee guard as exit 13 or a guard_mismatch body; both map to
// ErrCloseRace. The close reason is written as a trailing comment so it
// is readable through bd; the batch grammar's `close <id> <reason>`
// (close_reason) form carries the same text but takes no assignee guard,
// so the guarded single-op path wins here.
func (h *BdHub) Close(ctx context.Context, issue, ifAssignee, reason string) error {
	req := tracker.UpdateRequest{ID: issue, Status: StatusClosed}
	if ifAssignee != "" {
		guard := ifAssignee
		req.IfAssignee = &guard
	}
	if _, err := h.tr.Update(ctx, req); err != nil {
		if isGuardMismatch(err) {
			return fmt.Errorf("close %s: %w", issue, ErrCloseRace)
		}
		return fmt.Errorf("close %s: %w", issue, err)
	}
	if reason != "" {
		if cerr := h.Comment(ctx, issue, reason); cerr != nil {
			return fmt.Errorf("close %s: comment reason: %w", issue, cerr)
		}
	}
	return nil
}

// Defer implements Hub.
func (h *BdHub) Defer(ctx context.Context, issue string, until time.Time, reason string) error {
	if _, err := h.tr.Defer(ctx, issue, &until, reason); err != nil {
		return fmt.Errorf("defer %s: %w", issue, err)
	}
	return nil
}

// NeedsHuman implements Hub: the status move — stamped with the reason
// and last-gate-output metadata, mirroring the blocked fallback — plus a
// comment carrying the reason and the last gate output, so the
// escalation is readable through bd alone.
func (h *BdHub) NeedsHuman(ctx context.Context, issue, reason, lastGateOutput string) error {
	meta := map[string]string{MetaNeedsHumanReason: reason, MetaLastGateOutput: lastGateOutput}
	if _, err := h.tr.Update(ctx, tracker.UpdateRequest{
		ID:       issue,
		Status:   StatusNeedsHuman,
		Metadata: meta,
	}); err != nil {
		// bd rejects needs-human unless the custom status is configured
		// (EnsureNeedsHumanStatus). Escalation must still park the issue:
		// fall back to blocked, which hides it from dispatch, and keep
		// the reason and gate output in metadata so the parking is
		// auditable.
		if _, berr := h.tr.Update(ctx, tracker.UpdateRequest{
			ID:       issue,
			Status:   tracker.StatusBlocked,
			Metadata: meta,
		}); berr != nil {
			return fmt.Errorf("needs-human %s: %w (blocked fallback: %w)", issue, err, berr)
		}
	}
	body := "lambe gate escalated this issue to needs-human: " + reason + "."
	if lastGateOutput != "" {
		body += "\nLast gate output:\n" + lastGateOutput
	}
	if err := h.Comment(ctx, issue, body); err != nil {
		return fmt.Errorf("needs-human %s: comment: %w", issue, err)
	}
	return nil
}

// Tombstone implements Hub.
func (h *BdHub) Tombstone(ctx context.Context, issue string) error {
	if _, err := h.tr.Update(ctx, tracker.UpdateRequest{ID: issue, Metadata: map[string]string{MetaTombstone: "1"}}); err != nil {
		return fmt.Errorf("tombstone %s: %w", issue, err)
	}
	return nil
}

// IsTombstoned implements Hub.
func (h *BdHub) IsTombstoned(ctx context.Context, issue string) (bool, error) {
	meta, err := h.metadata(ctx, issue)
	if err != nil {
		return false, err
	}
	return meta[MetaTombstone] == "1", nil
}

// Get implements KV.
func (h *BdHub) Get(ctx context.Context, issue, key string) (string, error) {
	meta, err := h.metadata(ctx, issue)
	if err != nil {
		return "", err
	}
	return meta[key], nil
}

// Put implements KV.
func (h *BdHub) Put(ctx context.Context, issue, key, value string) error {
	if _, err := h.tr.Update(ctx, tracker.UpdateRequest{ID: issue, Metadata: map[string]string{key: value}}); err != nil {
		return fmt.Errorf("put %s/%s: %w", issue, key, err)
	}
	return nil
}

// IssueStatus implements Hub: bd show, decoding either the single-object
// or one-element-array shape, mirroring metadata.
func (h *BdHub) IssueStatus(ctx context.Context, issue string) (tracker.Status, error) {
	res, err := h.tr.Show(ctx, issue)
	if err != nil {
		return "", fmt.Errorf("show %s: %w", issue, err)
	}
	var rows []issueStatusJSON
	if err := unmarshalStrict(string(res.Data), &rows); err == nil && len(rows) > 0 {
		return rows[0].Status, nil
	}
	var row issueStatusJSON
	if err := unmarshalStrict(string(res.Data), &row); err != nil {
		return "", fmt.Errorf("decode %s: %w", issue, err)
	}
	return row.Status, nil
}

// issueStatusJSON mirrors the status field of bd's issue JSON.
type issueStatusJSON struct {
	Status tracker.Status `json:"status"`
}

// issueMetaJSON mirrors the metadata field of bd's issue JSON.
type issueMetaJSON struct {
	Metadata map[string]any `json:"metadata"`
}

// metadata reads an issue's metadata through bd show, which returns
// either a single object or a one-element array.
func (h *BdHub) metadata(ctx context.Context, issue string) (map[string]string, error) {
	res, err := h.tr.Show(ctx, issue)
	if err != nil {
		return nil, fmt.Errorf("show %s: %w", issue, err)
	}
	var rows []issueMetaJSON
	if err := unmarshalStrict(string(res.Data), &rows); err == nil && len(rows) > 0 {
		return normalizeMetadata(rows[0].Metadata), nil
	}
	var row issueMetaJSON
	if err := unmarshalStrict(string(res.Data), &row); err != nil {
		return nil, fmt.Errorf("decode %s: %w", issue, err)
	}
	return normalizeMetadata(row.Metadata), nil
}

// normalizeMetadata renders bd's arbitrary JSON scalar values as strings.
func normalizeMetadata(m map[string]any) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = fmt.Sprintf("%v", v)
	}
	return out
}

// isGuardMismatch recognizes both guard-refusal shapes bd produces: the
// typed exit 13 path and the exit 1 assignee-mismatch body.
func isGuardMismatch(err error) bool {
	if errors.Is(err, tracker.ErrLostRace) {
		return true
	}
	text := strings.ToLower(tracker.ExitOutput(err))
	return strings.Contains(text, "guard_mismatch") || strings.Contains(text, "assignee mismatch")
}

// unmarshalStrict decodes JSON after trimming surrounding noise.
func unmarshalStrict(data string, v any) error {
	return json.Unmarshal([]byte(strings.TrimSpace(data)), v)
}

// trimSpace is strings.TrimSpace, kept local for the runner's import
// hygiene.
func trimSpace(s string) string { return strings.TrimSpace(s) }

// compile-time interface checks.
var (
	_ Hub = (*BdHub)(nil)
	_ KV  = (*BdHub)(nil)
)
