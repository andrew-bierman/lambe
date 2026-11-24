package tui

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/andrew-bierman/lambe/internal/daemon"
	"github.com/andrew-bierman/lambe/internal/proc"
	"github.com/andrew-bierman/lambe/internal/symphony"
)

// liveFixture writes a snapshot of n runs, each carrying tailBytes of output,
// owned by this test process so it reads as alive.
func liveFixture(tb testing.TB, n, tailBytes int) (string, daemon.LiveSnapshot) {
	tb.Helper()
	identity, err := proc.ProcessIdentity(os.Getpid())
	if err != nil {
		tb.Skipf("no process identity on this platform: %v", err)
	}
	now := time.Now()
	snap := daemon.LiveSnapshot{PID: os.Getpid(), Identity: identity, UpdatedAt: now, TickAt: now}
	tail := strings.Repeat("x", tailBytes)
	for i := range n {
		snap.Runs = append(snap.Runs, daemon.LiveRun{
			Progress:     symphony.Progress{IssueID: fmt.Sprintf("bench-%d", i), Title: "Verify the release build", Phase: "running", Attempt: 1, Turn: 2},
			Tail:         tail,
			LastOutputAt: now,
			UpdatedAt:    now,
		})
	}
	path := filepath.Join(tb.TempDir(), "live.json")
	writeLive(tb, path, snap)
	return path, snap
}

// writeLive replaces the file the way the daemon does: temp file plus rename.
func writeLive(tb testing.TB, path string, snap daemon.LiveSnapshot) {
	tb.Helper()
	data, err := json.Marshal(snap)
	if err != nil {
		tb.Fatal(err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		tb.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		tb.Fatal(err)
	}
}

func TestReaderSkipsUnchangedFile(t *testing.T) {
	path, snap := liveFixture(t, 5, 10)
	r := &snapshotReader{path: path}

	first := r.read(false)
	if first.err != nil || first.unchanged || !first.alive || len(first.snapshot.Runs) != 5 {
		t.Fatalf("first read = %+v", first)
	}
	second := r.read(false)
	if !second.unchanged || !second.alive || len(second.snapshot.Runs) != 5 {
		t.Fatalf("an untouched file must short-circuit: %+v", second)
	}
	if forced := r.read(true); forced.unchanged {
		t.Fatal("a forced read must not short-circuit")
	}

	// Same size, same mtime, new inode (the daemon's rename): still detected.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	snap.Runs[0].Phase = "ru0ning" // same length as "running", so size and mtime match
	writeLive(t, path, snap)
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if changed := r.read(false); changed.unchanged || changed.snapshot.Runs[0].Phase != "ru0ning" {
		t.Fatalf("a replaced file must be re-read: %+v", changed.snapshot.Runs[0])
	}
}

func TestReaderRecoversAfterErrors(t *testing.T) {
	path, snap := liveFixture(t, 2, 10)
	r := &snapshotReader{path: path}
	r.read(false)

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if msg := r.read(false); msg.err == nil || msg.unchanged {
		t.Fatalf("a removed file is an error, not a cached snapshot: %+v", msg)
	}
	if err := os.WriteFile(path, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if msg := r.read(false); msg.err == nil {
		t.Fatal("a corrupt file is an error")
	}
	writeLive(t, path, snap)
	if msg := r.read(false); msg.err != nil || msg.unchanged || len(msg.snapshot.Runs) != 2 {
		t.Fatalf("the next good write must be read in full: %+v", msg)
	}
}

func TestReaderRechecksLivenessOfUnchangedFile(t *testing.T) {
	path, snap := liveFixture(t, 1, 10)
	snap.Identity = "someone-else" // the writer is not this process any more
	writeLive(t, path, snap)
	r := &snapshotReader{path: path}
	if first := r.read(false); first.alive {
		t.Fatal("a snapshot of another process incarnation is not alive")
	}
	if second := r.read(false); !second.unchanged || second.alive {
		t.Fatalf("liveness must be re-checked on the short-circuit: %+v", second)
	}
}

func TestPollsNeverOverlap(t *testing.T) {
	path, _ := liveFixture(t, 1, 10)
	m := New(path)
	if m.readNow(false) == nil {
		t.Fatal("the first poll must start")
	}
	for range 3 {
		if m.readNow(false) != nil {
			t.Fatal("a poll started while one was in flight")
		}
	}
	// A tick while reading re-arms the timer but starts no read.
	if _, cmd := m.Update(tickMsg(time.Now())); cmd == nil {
		t.Fatal("tick must re-arm itself")
	}
	if m.readNow(true) != nil || !m.forceNext {
		t.Fatal("a forced read must queue behind the in-flight one")
	}
	_, next := m.Update(snapshotMsg{snapshot: daemon.LiveSnapshot{UpdatedAt: time.Now()}})
	if next == nil || !m.reading || m.forceNext {
		t.Fatalf("the queued forced read must start when the in-flight one lands: next=%v reading=%v", next, m.reading)
	}
	if msg, ok := next().(snapshotMsg); !ok || msg.unchanged {
		t.Fatalf("queued read must be forced, got %+v", msg)
	}
}

func TestUnchangedPollOnlyUpdatesLiveness(t *testing.T) {
	now := frozenClock(t)
	m := demoModel(now)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	before := m.snapshot
	m.Update(snapshotMsg{snapshot: demoSnapshot(now), alive: false, unchanged: true})
	if m.alive || len(m.snapshot.Runs) != len(before.Runs) {
		t.Fatalf("alive=%v runs=%d", m.alive, len(m.snapshot.Runs))
	}
	if got := m.health(now).kind; got != healthStopped {
		t.Fatalf("a daemon that died under an unchanged file must read stopped, got %v", got)
	}
}

func BenchmarkPoll(b *testing.B) {
	for _, size := range []struct {
		name string
		tail int
	}{{"2500runs-0.8MB", 0}, {"2500runs-7.5MB", 2600}} {
		path, _ := liveFixture(b, 2500, size.tail)
		prime := func() *Model {
			m := New(path)
			m.Update(tea.WindowSizeMsg{Width: 140, Height: 40})
			snap, alive, err := daemon.ReadLive(path)
			if err != nil {
				b.Fatal(err)
			}
			m.Update(snapshotMsg{snapshot: snap, alive: alive})
			return m
		}
		report := func(b *testing.B) {
			// One poll per 250ms tick: seconds of CPU per poll times four, as a percent of a core.
			b.ReportMetric(b.Elapsed().Seconds()/float64(b.N)*4*100, "cpu%@4Hz")
		}
		b.Run(size.name+"/read-every-tick", func(b *testing.B) { // what every tick did before the short-circuit
			m := prime()
			b.ReportAllocs()
			for b.Loop() {
				snap, alive, err := daemon.ReadLive(path)
				if err != nil {
					b.Fatal(err)
				}
				m.Update(snapshotMsg{snapshot: snap, alive: alive})
			}
			report(b)
		})
		b.Run(size.name+"/stat-short-circuit", func(b *testing.B) {
			m := prime()
			m.reader.read(false)
			b.ReportAllocs()
			for b.Loop() {
				m.Update(m.reader.read(false))
			}
			report(b)
		})
	}
}
