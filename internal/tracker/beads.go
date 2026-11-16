package tracker

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Beads is the Tracker implementation over a Transport. All typed-outcome
// mapping (lost races, freezes, lease loss, journal truncation) lives here,
// so neither transport needs to know the semantics.
type Beads struct {
	t     Transport
	actor string

	mu      sync.Mutex
	prefix  string
	pending []BatchOp
}

// NewBeads returns a tracker bound to t, writing guarded operations as
// actor.
func NewBeads(t Transport, actor string) *Beads {
	return &Beads{t: t, actor: actor}
}

// issueJSON mirrors the bead fields lambe reads. Metadata values arrive as
// arbitrary JSON scalars, so they are normalized to strings.
type issueJSON struct {
	ID       string         `json:"id"`
	Title    string         `json:"title"`
	Status   string         `json:"status"`
	Assignee string         `json:"assignee"`
	Revision string         `json:"revision"`
	Metadata map[string]any `json:"metadata"`
}

func (j issueJSON) toIssue() Issue {
	meta := make(map[string]string, len(j.Metadata))
	for k, v := range j.Metadata {
		meta[k] = metadataString(v)
	}
	return Issue{
		ID:       j.ID,
		Title:    j.Title,
		Status:   j.Status,
		Assignee: j.Assignee,
		Revision: j.Revision,
		Metadata: meta,
	}
}

// metadataString renders a decoded metadata value losslessly. bd returns
// numbers as JSON floats; plain %v formatting turns large integers (real
// process-group ids are typically >= 1e6) into scientific notation like
// "2.900324e+06", which strconv.Atoi rejects and crash recovery then loses
// the pgid binding. Integral floats below 1e15 render as plain integers.
func metadataString(v any) string {
	switch n := v.(type) {
	case float64:
		if n == math.Trunc(n) && math.Abs(n) < 1e15 {
			return strconv.FormatInt(int64(n), 10)
		}
	case bool:
		return strconv.FormatBool(n)
	case nil:
		return ""
	}
	return fmt.Sprintf("%v", v)
}

// classifyExit maps bd exit codes to typed outcomes. It returns nil when
// the error carries no recognized code, leaving the original error in play.
func classifyExit(err error) error {
	var exitErr *ExitError
	if !errors.As(err, &exitErr) {
		return nil
	}
	switch exitErr.Code {
	case 13:
		return fmt.Errorf("%w: guard precondition no longer held: %w", ErrLostRace, err)
	case 14:
		return fmt.Errorf("%w: retry later: %w", ErrFrozen, err)
	default:
		return nil
	}
}

// Capabilities implements Tracker by parsing the backend version: the
// journal-watch, tombstone, and HTTP-lease features all landed after
// 1.3.x.
func (b *Beads) Capabilities(ctx context.Context) (Capabilities, error) {
	ver, err := b.t.Version(ctx)
	if err != nil {
		return Capabilities{}, fmt.Errorf("read bd version: %w", err)
	}
	major, minor, ok := parseBdVersion(ver)
	if !ok {
		return Capabilities{}, fmt.Errorf("unparseable bd version %q", ver)
	}
	atLeast := major > 1 || (major == 1 && minor >= 4)
	return Capabilities{SSEWatch: atLeast, RetainedTombstones: atLeast, HTTPHeartbeat: atLeast}, nil
}

// parseBdVersion pulls major and minor from strings like
// "bd version 1.3.4 (7ef2fd05e)".
func parseBdVersion(s string) (int, int, bool) {
	fields := strings.Fields(s)
	for i := 0; i < len(fields)-1; i++ {
		if fields[i] != "version" {
			continue
		}
		parts := strings.Split(fields[i+1], ".")
		if len(parts) < 2 {
			return 0, 0, false
		}
		maj, err1 := strconv.Atoi(parts[0])
		minor, err2 := strconv.Atoi(parts[1])
		if err1 != nil || err2 != nil {
			return 0, 0, false
		}
		return maj, minor, true
	}
	return 0, 0, false
}

// Dispatchable implements Tracker. The empty query must not touch the
// tracker at all.
func (b *Beads) Dispatchable(ctx context.Context, q DispatchQuery) ([]Issue, error) {
	if q.Empty() {
		return nil, nil
	}
	res, err := b.t.List(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list dispatchable: %w", err)
	}
	var rows []issueJSON
	if err := json.Unmarshal(res.Data, &rows); err != nil {
		return nil, fmt.Errorf("decode dispatchable: %w", err)
	}
	issues := make([]Issue, 0, len(rows))
	for _, r := range rows {
		issues = append(issues, r.toIssue())
	}
	return issues, nil
}

// Claim implements Tracker.
func (b *Beads) Claim(ctx context.Context, id string) (Issue, error) {
	res, err := b.t.Update(ctx, UpdateRequest{ID: id, Claim: true})
	if err != nil {
		if typed := classifyExit(err); typed != nil {
			return Issue{}, fmt.Errorf("claim %s: %w", id, typed)
		}
		if claimsRace(err) {
			return Issue{}, fmt.Errorf("claim %s: %w: already held by another actor: %w", id, ErrLostRace, err)
		}
		return Issue{}, fmt.Errorf("claim %s: %w", id, err)
	}
	issue, err := decodeOneIssue(res.Data)
	if err != nil {
		return Issue{}, fmt.Errorf("claim %s: %w", id, err)
	}
	return issue, nil
}

// claimsRace recognizes the non-guarded claim refusal: bd exit 1 with the
// "already claimed by" body. The guarded path (exit 13) is classified by
// exit code.
func claimsRace(err error) bool {
	var exitErr *ExitError
	return errors.As(err, &exitErr) && strings.Contains(exitErr.Stderr+exitErr.Stdout, "already claimed by")
}

// Release implements Tracker. The release is guarded by our own identity so
// we never drop another actor's claim; a mismatch reads as lease loss.
func (b *Beads) Release(ctx context.Context, id string) error {
	_, err := b.t.Unclaim(ctx, id, b.actor)
	if err != nil {
		if typed := classifyExit(err); typed != nil {
			return fmt.Errorf("release %s: %w", id, typed)
		}
		if strings.Contains(exitText(err), "assignee mismatch") {
			return fmt.Errorf("release %s: %w: held by another actor: %w", id, ErrNotOwner, err)
		}
		if strings.Contains(exitText(err), "cannot unclaim closed issue") {
			return fmt.Errorf("release %s: %w: issue is closed: %w", id, ErrNotOwner, err)
		}
		return fmt.Errorf("release %s: %w", id, err)
	}
	return nil
}

// Heartbeat implements Tracker. bd distinguishes "you no longer hold this"
// only by message (the failure is a plain exit 1), so the texts bd uses for
// reclaimed, released, and closed claims are matched here.
func (b *Beads) Heartbeat(ctx context.Context, id string) error {
	_, err := b.t.Heartbeat(ctx, id)
	if err == nil {
		return nil
	}
	if typed := classifyExit(err); typed != nil {
		return fmt.Errorf("heartbeat %s: %w", id, typed)
	}
	text := strings.ToLower(exitText(err))
	if strings.Contains(text, "claim") || strings.Contains(text, "lease") || strings.Contains(text, "not owner") {
		return fmt.Errorf("heartbeat %s: %w: %w", id, ErrNotOwner, err)
	}
	return fmt.Errorf("heartbeat %s: %w", id, err)
}

// Reclaim implements Tracker.
func (b *Beads) Reclaim(ctx context.Context, olderThan time.Duration, ids []string) error {
	_, err := b.t.Reclaim(ctx, olderThan, ids)
	if err != nil {
		if typed := classifyExit(err); typed != nil {
			return fmt.Errorf("reclaim: %w", typed)
		}
		return fmt.Errorf("reclaim: %w", err)
	}
	return nil
}

// Defer implements Tracker: the until date is the wake timer, enforced
// lazily by the next dispatch read.
func (b *Beads) Defer(ctx context.Context, id string, until time.Time, reason string) error {
	_, err := b.t.Defer(ctx, id, &until, reason)
	if err != nil {
		if typed := classifyExit(err); typed != nil {
			return fmt.Errorf("defer %s: %w", id, typed)
		}
		return fmt.Errorf("defer %s: %w", id, err)
	}
	return nil
}

// eventJSON mirrors one journal record line.
type eventJSON struct {
	Seq     int64  `json:"seq"`
	TS      string `json:"ts"`
	Op      string `json:"op"`
	IssueID string `json:"issue_id"`
	Actor   string `json:"actor"`
}

// Events implements Tracker. A checkpoint below the retained floor yields
// a *TruncatedError so the consumer re-baselines from Floor.
func (b *Beads) Events(ctx context.Context, since int64) ([]Event, error) {
	res, err := b.t.Events(ctx, since)
	if err != nil {
		if trunc := asTruncated(err); trunc != nil {
			return nil, trunc
		}
		if typed := classifyExit(err); typed != nil {
			return nil, typed
		}
		if strings.Contains(exitText(err), "journal is disabled") {
			return nil, ErrJournalDisabled
		}
		return nil, fmt.Errorf("read events: %w", err)
	}
	var events []Event
	sc := bufio.NewScanner(strings.NewReader(string(res.Raw)))
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || !strings.HasPrefix(line, "{") {
			continue // bd prints human notes alongside the records
		}
		var r eventJSON
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			return nil, fmt.Errorf("decode event line %q: %w", line, err)
		}
		at, err := time.Parse(time.RFC3339, r.TS)
		if err != nil {
			return nil, fmt.Errorf("decode event %d timestamp: %w", r.Seq, err)
		}
		events = append(events, Event{Seq: r.Seq, At: at, Op: r.Op, IssueID: r.IssueID, Actor: r.Actor})
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("scan events: %w", err)
	}
	return events, nil
}

// asTruncated pulls the structured truncation reply out of a failed events
// call. bd reports it as exit 1 whose stdout envelope carries the code and
// the retained window.
func asTruncated(err error) *TruncatedError {
	var exitErr *ExitError
	if !errors.As(err, &exitErr) {
		return nil
	}
	var env struct {
		Data struct {
			Code  string `json:"code"`
			Floor int64  `json:"floor"`
			Head  int64  `json:"head"`
		} `json:"data"`
	}
	if jsonErr := json.Unmarshal([]byte(exitErr.Stdout), &env); jsonErr != nil {
		return nil
	}
	if env.Data.Code != "events_journal_truncated" {
		return nil
	}
	return &TruncatedError{Floor: env.Data.Floor, Head: env.Data.Head}
}

// issuePrefix returns (and caches) the tracker's issue id prefix so pinned
// creates can compose prefix-qualified ids.
func (b *Beads) issuePrefix(ctx context.Context) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.prefix != "" {
		return b.prefix, nil
	}
	p, err := b.t.ConfigGet(ctx, "issue_prefix")
	if err != nil {
		return "", fmt.Errorf("read issue_prefix: %w", err)
	}
	if p == "" {
		return "", fmt.Errorf("tracker reported empty issue_prefix")
	}
	b.prefix = p
	return p, nil
}

// EnsureIssue implements Tracker.
func (b *Beads) EnsureIssue(ctx context.Context, key string, spec IssueSpec) (Issue, error) {
	prefix, err := b.issuePrefix(ctx)
	if err != nil {
		return Issue{}, err
	}
	id := prefix + "-" + key
	res, err := b.t.Create(ctx, CreateRequest{
		ID:        id,
		Title:     spec.Title,
		Type:      spec.Type,
		Priority:  spec.Priority,
		Labels:    spec.Labels,
		Metadata:  spec.Metadata,
		Ephemeral: spec.Ephemeral,
	})
	if err == nil {
		issue, err := decodeOneIssue(res.Data)
		if err != nil {
			return Issue{}, fmt.Errorf("ensure %s: %w", id, err)
		}
		return issue, nil
	}
	if strings.Contains(exitText(err), "already exists") {
		show, showErr := b.t.Show(ctx, id)
		if showErr != nil {
			return Issue{}, fmt.Errorf("ensure %s: show existing: %w", id, showErr)
		}
		issue, decErr := decodeOneIssue(show.Data)
		if decErr != nil {
			return Issue{}, fmt.Errorf("ensure %s: decode existing: %w", id, decErr)
		}
		return issue, nil
	}
	if typed := classifyExit(err); typed != nil {
		return Issue{}, fmt.Errorf("ensure %s: %w", id, typed)
	}
	return Issue{}, fmt.Errorf("ensure %s: %w", id, err)
}

// Metadata keys lambe stamps on its operational records.
const (
	metaKind    = "lambe_kind"
	metaSession = "lambe_session"
	metaAttempt = "lambe_attempt"
	metaPGID    = "lambe_pgid"
	metaWakeAt  = "lambe_wake_at"
)

// MetaKind is the exported name of the metadata key marking lambe's own
// operational records (attempt bindings). Surfaces reading the dispatch
// front use it to hide internal beads from human-facing views.
const MetaKind = metaKind

// kindAttempt is the lambe_kind value of attempt records.
const kindAttempt = "attempt"

// attemptKey builds the pinned-id key for one attempt record.
func attemptKey(sessionID string, number int) string {
	return fmt.Sprintf("att-%s-%d", sessionID, number)
}

// PutAttempt implements Tracker: pinned create on first record, idempotent
// metadata update on re-put.
func (b *Beads) PutAttempt(ctx context.Context, a Attempt) error {
	if a.SessionID == "" || a.Number < 1 {
		return fmt.Errorf("put attempt: session id and positive attempt number required")
	}
	meta := map[string]string{
		metaKind:    kindAttempt,
		metaSession: a.SessionID,
		metaAttempt: strconv.Itoa(a.Number),
		metaPGID:    strconv.Itoa(a.PGID),
	}
	if !a.WakeAt.IsZero() {
		meta[metaWakeAt] = a.WakeAt.UTC().Format(time.RFC3339)
	}
	issue, err := b.EnsureIssue(ctx, attemptKey(a.SessionID, a.Number), IssueSpec{
		Title:    fmt.Sprintf("attempt %d of session %s", a.Number, a.SessionID),
		Priority: 2,
		Labels:   []string{"lambe", kindAttempt},
		Metadata: meta,
	})
	if err != nil {
		return err
	}
	// Operational records stay off the ready dispatch front: an open attempt
	// bead would be dispatched as work by the loop's own two-read within the
	// same tick that wrote it. "blocked" keeps it readable via metadata query
	// while invisible to Dispatchable. The create cannot carry a status, and
	// a re-put finds the issue already existing (whose create metadata was
	// not applied), so the pgid/wake fields ride on this update as well —
	// using the id EnsureIssue returned rather than recomposing it.
	update := map[string]string{metaPGID: strconv.Itoa(a.PGID), metaWakeAt: ""}
	if !a.WakeAt.IsZero() {
		update[metaWakeAt] = a.WakeAt.UTC().Format(time.RFC3339)
	}
	if _, err := b.t.Update(ctx, UpdateRequest{ID: issue.ID, Status: StatusBlocked, Metadata: update}); err != nil {
		if typed := classifyExit(err); typed != nil {
			return fmt.Errorf("put attempt: %w", typed)
		}
		return fmt.Errorf("put attempt: %w", err)
	}
	return nil
}

// Attempts implements Tracker: exact-match metadata query, ordered by
// attempt number.
func (b *Beads) Attempts(ctx context.Context, sessionID string) ([]Attempt, error) {
	res, err := b.t.List(ctx, DispatchQuery{
		Metadata: map[string]string{metaKind: kindAttempt, metaSession: sessionID},
	})
	if err != nil {
		return nil, fmt.Errorf("list attempts for %s: %w", sessionID, err)
	}
	var rows []issueJSON
	if err := json.Unmarshal(res.Data, &rows); err != nil {
		return nil, fmt.Errorf("decode attempts: %w", err)
	}
	attempts := make([]Attempt, 0, len(rows))
	for _, r := range rows {
		meta := r.toIssue().Metadata
		n, err := strconv.Atoi(meta[metaAttempt])
		if err != nil {
			continue
		}
		a := Attempt{SessionID: sessionID, Number: n}
		if pgid, err := strconv.Atoi(meta[metaPGID]); err == nil {
			a.PGID = pgid
		}
		if wake := meta[metaWakeAt]; wake != "" {
			if t, err := time.Parse(time.RFC3339, wake); err == nil {
				a.WakeAt = t
			}
		}
		attempts = append(attempts, a)
	}
	sort.Slice(attempts, func(i, j int) bool { return attempts[i].Number < attempts[j].Number })
	return attempts, nil
}

// queue validates op against the batch grammar and buffers it for the next
// FlushWrites.
func (b *Beads) queue(op BatchOp) error {
	if _, err := op.Render(); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pending = append(b.pending, op)
	return nil
}

// QueueClose buffers a terminal close whose comment rides as the close
// reason. The write is applied by the next FlushWrites.
func (b *Beads) QueueClose(id, comment string) error {
	return b.queue(BatchClose(id, comment))
}

// QueueUpdate buffers a grammar-legal field update (status, priority,
// title, assignee, force). Metadata is not settable through the batch
// grammar, so metadata writes — attempt re-puts included — must keep using
// the single-op Update path. The write is applied by the next FlushWrites.
func (b *Beads) QueueUpdate(id string, f BatchFields) error {
	return b.queue(BatchUpdate(id, f))
}

// QueueCreate buffers a grammar-legal create (type, priority, title only;
// no pinned id, labels, or metadata — those need the single-op path).
func (b *Beads) QueueCreate(typ string, priority int, title string) error {
	return b.queue(BatchCreate(typ, priority, title))
}

// QueueDepAdd buffers a dependency add. An empty depType is bd's default.
func (b *Beads) QueueDepAdd(fromID, toID, depType string) error {
	return b.queue(BatchDepAdd(fromID, toID, depType))
}

// QueueDepRemove buffers a dependency removal.
func (b *Beads) QueueDepRemove(fromID, toID string) error {
	return b.queue(BatchDepRemove(fromID, toID))
}

// FlushWrites applies every queued write in one bd batch transaction: one
// engine start and one commit instead of N. An empty buffer contacts
// nothing. A failed batch is rolled back atomically by bd, so the buffer is
// kept and the caller may retry (or drop it by re-queuing nothing and
// abandoning the tracker). Claims, defers, and heartbeats are outside the
// batch grammar and always go single-op; queue only what the grammar
// allows.
func (b *Beads) FlushWrites(ctx context.Context) error {
	b.mu.Lock()
	pending := b.pending
	b.mu.Unlock()
	if len(pending) == 0 {
		return nil
	}
	if _, err := b.t.Batch(ctx, pending); err != nil {
		if typed := classifyExit(err); typed != nil {
			return fmt.Errorf("flush writes: %w", typed)
		}
		return fmt.Errorf("flush writes: %w", err)
	}
	b.mu.Lock()
	b.pending = nil
	b.mu.Unlock()
	return nil
}

// BufferedWrites reports how many writes are queued, awaiting the next
// FlushWrites.
func (b *Beads) BufferedWrites() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.pending)
}

// decodeOneIssue accepts the single bead or single-element array shapes bd
// uses across create, update, claim, and show.
func decodeOneIssue(data json.RawMessage) (Issue, error) {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return Issue{}, fmt.Errorf("empty issue payload")
	}
	if trimmed[0] == '[' {
		var rows []issueJSON
		if err := json.Unmarshal(data, &rows); err != nil {
			return Issue{}, err
		}
		if len(rows) != 1 {
			return Issue{}, fmt.Errorf("expected one issue, got %d", len(rows))
		}
		return rows[0].toIssue(), nil
	}
	var row issueJSON
	if err := json.Unmarshal(data, &row); err != nil {
		return Issue{}, err
	}
	return row.toIssue(), nil
}

// exitText concatenates a failed command's output for message-based
// classification.
func exitText(err error) string {
	var exitErr *ExitError
	if !errors.As(err, &exitErr) {
		return err.Error()
	}
	return exitErr.Stdout + exitErr.Stderr
}

// ExitOutput returns a failed bd command's combined output for
// message-based classification. Non-exit errors render as their message
// text.
func ExitOutput(err error) string {
	return exitText(err)
}

// compile-time interface check.
var _ Tracker = (*Beads)(nil)
