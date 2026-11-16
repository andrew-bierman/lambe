package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
)

// Syncer synchronizes the local Beads database with its hub remote. It is a
// narrow seam: the shell-transport tracker has no sync operation of its own,
// so the daemon drives bd's Dolt plumbing around the tracker's DB directory.
// Implementations must never force.
type Syncer interface {
	// Pull fast-forwards the local database from the remote.
	Pull(ctx context.Context) error
	// Push publishes local commits to the remote.
	Push(ctx context.Context) error
}

// NopSyncer is the Syncer for trackers with no remote; every call succeeds.
type NopSyncer struct{}

// Pull implements Syncer.
func (NopSyncer) Pull(context.Context) error { return nil }

// Push implements Syncer.
func (NopSyncer) Push(context.Context) error { return nil }

// BdSyncer shells `bd dolt pull` / `bd dolt push` in the tracker's
// database directory. It never passes a force flag.
type BdSyncer struct {
	// Bin is the bd binary path.
	Bin string
	// Dir is the tracker working directory the database lives under.
	Dir string
	// Log receives sync outcomes; nil discards them.
	Log *slog.Logger
}

// Pull implements Syncer.
func (s *BdSyncer) Pull(ctx context.Context) error {
	return s.run(ctx, "pull")
}

// Push implements Syncer. A rejected non-fast-forward push returns an error
// wrapping ErrNonFastForward so callers can retry pull-then-push.
func (s *BdSyncer) Push(ctx context.Context) error {
	return s.run(ctx, "push")
}

// ErrNonFastForward reports a push the remote rejected because the local
// database diverged; the remedy is a pull before the next push, never force.
var ErrNonFastForward = errors.New("non-fast-forward push rejected")

func (s *BdSyncer) run(ctx context.Context, verb string) error {
	cmd := exec.CommandContext(ctx, s.Bin, "dolt", verb) // #nosec G204 -- fixed argv over a configured binary
	cmd.Dir = s.Dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return nil
	}
	text := stderr.String() + stdout.String()
	if verb == "push" && strings.Contains(strings.ToLower(text), "non-fast-forward") {
		return fmt.Errorf("%w: bd dolt push: %s", ErrNonFastForward, text)
	}
	return fmt.Errorf("bd dolt %s: %w: %s", verb, err, text)
}

// SyncCycle runs one full synchronization cycle: pull, then push, and on a
// non-fast-forward rejection exactly one pull-then-push retry. It never
// forces.
func SyncCycle(ctx context.Context, s Syncer) error {
	if err := s.Pull(ctx); err != nil {
		return fmt.Errorf("pull: %w", err)
	}
	return PushAfterWrites(ctx, s)
}

// PushAfterWrites publishes the write batch a tick just produced. A
// non-fast-forward rejection triggers exactly one pull-then-push retry;
// it never forces.
func PushAfterWrites(ctx context.Context, s Syncer) error {
	err := s.Push(ctx)
	if err == nil || !errors.Is(err, ErrNonFastForward) {
		return err
	}
	if err := s.Pull(ctx); err != nil {
		return fmt.Errorf("pull after divergence: %w", err)
	}
	if err := s.Push(ctx); err != nil {
		return fmt.Errorf("push after pull: %w", err)
	}
	return nil
}
