package tracker

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"time"
)

// Local is the embedded operational tracker: it shells to bd in a working
// directory (a repo-level tracker or an in-repo .beads/), runs no server,
// and enables the events journal for its own writes so a supervisor can
// watch its records.
type Local struct {
	*Beads
	shell *ShellTransport
	bin   string
	dir   string
}

// ResolveBdBin resolves the bd binary path: a non-empty bin is taken as-is
// (caller-configured path); an empty bin means PATH lookup, and a failed
// lookup is wrapped with what was attempted.
func ResolveBdBin(bin string) (string, error) {
	if bin != "" {
		return bin, nil
	}
	found, err := exec.LookPath("bd")
	if err != nil {
		return "", fmt.Errorf("find bd binary: %w", err)
	}
	return found, nil
}

// NewLocal returns an embedded tracker over the bd binary at bin (empty
// means PATH lookup) operating on dir and writing as actor. The journal is
// enabled for every invocation this tracker makes.
func NewLocal(bin, dir, actor string) (*Local, error) {
	bin, err := ResolveBdBin(bin)
	if err != nil {
		return nil, err
	}
	shell := &ShellTransport{Bin: bin, Dir: dir, Actor: actor, Env: []string{"BD_EVENTS_JOURNAL=1"}}
	return &Local{Beads: NewBeads(shell, actor), shell: shell, bin: bin, dir: dir}, nil
}

// CreateTask adds an open task for the supervisor to pick up on its next poll.
func (l *Local) CreateTask(ctx context.Context, title string, labels []string) (Issue, error) {
	result, err := l.shell.Create(ctx, CreateRequest{Title: title, Type: "task", Priority: 2, Labels: labels})
	if err != nil {
		return Issue{}, err
	}
	return decodeOneIssue(result.Data)
}

// Init creates the tracker's .beads/ storage if the directory has none.
// It is a no-op when storage already exists.
func (l *Local) Init(ctx context.Context) error {
	status, err := l.shell.run(ctx, []string{"status"})
	if err == nil && len(status.Raw) > 0 {
		return nil
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, l.bin, "init") // #nosec G204 -- fixed argv, binary resolved by the constructor
	cmd.Dir = l.dir
	cmd.WaitDelay = time.Second
	if out, err := cmd.CombinedOutput(); err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return fmt.Errorf("bd init in %s: %w: %s", l.dir, err, out)
	}
	return nil
}

// RunRecord is one ephemeral run trace, recorded as a wisp where the
// backend supports ephemeral issues.
type RunRecord struct {
	Title string
	Kind  string // free-form wisp kind carried in the title
	At    time.Time
}

// RecordRun writes an ephemeral run trace. Wisps are subject to TTL
// compaction, so these are observability, never state.
func (l *Local) RecordRun(ctx context.Context, rec RunRecord) (Issue, error) {
	res, err := l.shell.Create(ctx, CreateRequest{
		Title:     fmt.Sprintf("%s %s", rec.Kind, rec.Title),
		Priority:  3,
		Ephemeral: true,
		Metadata:  map[string]string{metaKind: "run", "at": rec.At.UTC().Format(time.RFC3339)},
	})
	if err != nil {
		return Issue{}, fmt.Errorf("record run: %w", err)
	}
	return decodeOneIssue(res.Data)
}

// ArmWake parks the issue until until using a defer, which the dispatch
// front lazily honors: once until passes, the next Dispatchable read wakes
// the issue back to open.
func (l *Local) ArmWake(ctx context.Context, id string, until time.Time, reason string) error {
	return l.Defer(ctx, id, until, reason)
}
