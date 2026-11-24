package tui

import (
	"bytes"
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/andrew-bierman/lambe/internal/daemon"
	"github.com/andrew-bierman/lambe/internal/proc"
	"github.com/andrew-bierman/lambe/internal/symphony"
)

// syncBuffer is the program's terminal: bubbletea writes frames from its own
// goroutine while the test polls.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) text() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return ansi.Strip(s.b.String())
}

// probe wraps the model so the test can observe state after every message
// without racing the program's event loop: Update runs on that loop and the
// slices are read only after Run returns.
type probe struct {
	*Model
	states []probeState
}

type probeState struct {
	help, filtering bool
	filter          string
	width, height   int
	visible         int
	misfit          string // non-empty when the frame exceeded the window
}

func (p *probe) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	_, cmd := p.Model.Update(msg)
	if _, ok := msg.(tickMsg); ok {
		return p, cmd // ticks are timing noise, not a user-visible step
	}
	st := probeState{
		help: p.showHelp, filtering: p.list.SettingFilter(), filter: p.list.FilterValue(),
		width: p.width, height: p.height, visible: len(p.list.VisibleItems()),
	}
	if p.width > 0 && p.height > 0 {
		view := p.View()
		if w, h := lipgloss.Size(view); w > p.width || h > p.height {
			st.misfit = fmt.Sprintf("%dx%d frame in %dx%d window", w, h, p.width, p.height)
		}
	}
	p.states = append(p.states, st)
	return p, cmd
}

// publishLive writes a snapshot of running tasks owned by the test process,
// so the dashboard reads it as a live daemon.
func publishLive(t *testing.T, path string, runs ...string) {
	t.Helper()
	identity, err := proc.ProcessIdentity(os.Getpid())
	if err != nil {
		t.Skipf("no process identity: %v", err)
	}
	now := time.Now()
	snap := daemon.LiveSnapshot{PID: os.Getpid(), Identity: identity, UpdatedAt: now, TickAt: now}
	for _, id := range runs {
		snap.Runs = append(snap.Runs, daemon.LiveRun{
			Progress:     symphony.Progress{IssueID: id, Title: "Task " + id, Phase: "running", Attempt: 1},
			UpdatedAt:    now,
			LastOutputAt: now,
			Tail:         "output of " + id + "\n",
		})
	}
	writeLive(t, path, snap)
}

// session is one real tea.Program on a fake terminal.
type session struct {
	t    *testing.T
	p    *tea.Program
	out  *syncBuffer
	pr   *probe
	done chan error
}

func startSession(t *testing.T, path string) *session {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	s := &session{t: t, out: &syncBuffer{}, pr: &probe{Model: New(path)}, done: make(chan error, 1)}
	opts := append(programOptions(ctx), tea.WithInput(nil), tea.WithOutput(s.out), tea.WithoutSignals())
	s.p = tea.NewProgram(s.pr, opts...)
	go func() {
		_, err := s.p.Run()
		s.done <- err
	}()
	return s
}

func (s *session) send(msgs ...tea.Msg) {
	s.t.Helper()
	for _, m := range msgs {
		s.p.Send(m)
	}
}

func (s *session) keys(text string) {
	s.t.Helper()
	for _, r := range text {
		s.send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
}

// waitFor blocks until the terminal has shown text.
func (s *session) waitFor(text string) {
	s.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(s.out.text(), text) {
		if time.Now().After(deadline) {
			s.t.Fatalf("terminal never showed %q; last output:\n%.2000s", text, s.out.text())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// finish asks the program to quit and returns the states it saw.
func (s *session) finish(quit tea.Msg) []probeState {
	s.t.Helper()
	s.send(quit)
	select {
	case err := <-s.done:
		if err != nil {
			s.t.Fatalf("program ended with %v", err)
		}
	case <-time.After(10 * time.Second):
		s.t.Fatal("program did not quit")
	}
	return s.pr.states
}

func TestProgramStartupLoadsAndPolls(t *testing.T) {
	path := filepath.Join(t.TempDir(), "live.json")
	publishLive(t, path, "prog-1", "prog-2")
	s := startSession(t, path)
	s.send(tea.WindowSizeMsg{Width: 100, Height: 30})
	s.waitFor("prog-2")
	// Init armed the tick: a rewritten file shows up without any key press.
	publishLive(t, path, "prog-1", "prog-2", "prog-3")
	s.waitFor("prog-3")
	s.finish(tea.Quit())
	if s.pr.loading || len(s.pr.snapshot.Runs) != 3 || !s.pr.alive {
		t.Fatalf("loading=%v runs=%d alive=%v", s.pr.loading, len(s.pr.snapshot.Runs), s.pr.alive)
	}
}

func TestProgramFilterHelpAndQuit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "live.json")
	publishLive(t, path, "prog-1", "prog-2", "prog-3")
	s := startSession(t, path)
	s.send(tea.WindowSizeMsg{Width: 100, Height: 30})
	s.waitFor("prog-3")

	s.keys("/prog-2")
	s.send(tea.KeyMsg{Type: tea.KeyEnter})
	s.keys("?")
	s.send(tea.KeyMsg{Type: tea.KeyEsc}) // closes help
	s.send(tea.KeyMsg{Type: tea.KeyEsc}) // clears the applied filter
	states := s.finish(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})

	// states[0] is the window size, then one entry per key above.
	var filtered, helped, cleared bool
	for _, st := range states {
		filtered = filtered || (st.filter == "prog-2" && st.visible == 1)
		helped = helped || st.help
		cleared = cleared || (filtered && st.filter == "" && st.visible == 3)
		if st.misfit != "" {
			t.Errorf("frame does not fit: %s", st.misfit)
		}
	}
	if !filtered || !helped || !cleared {
		t.Fatalf("filtered=%v helped=%v cleared=%v states=%+v", filtered, helped, cleared, states)
	}
	if last := states[len(states)-1]; last.help {
		t.Fatal("help overlay still open after esc")
	}
}

func TestProgramCtrlCQuits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "live.json")
	publishLive(t, path, "prog-1")
	s := startSession(t, path)
	s.send(tea.WindowSizeMsg{Width: 100, Height: 30})
	s.waitFor("prog-1")
	s.finish(tea.KeyMsg{Type: tea.KeyCtrlC})
}

// TestProgramResizeStorm fires 60 random window sizes, from 1x1 up, with
// navigation keys between them; every frame must fit the size it was drawn at.
func TestProgramResizeStorm(t *testing.T) {
	path := filepath.Join(t.TempDir(), "live.json")
	publishLive(t, path, "prog-1", "prog-2", "prog-3", "prog-4")
	s := startSession(t, path)
	s.send(tea.WindowSizeMsg{Width: 100, Height: 30})
	s.waitFor("prog-4")
	rng := rand.New(rand.NewPCG(7, 11)) //nolint:gosec // a fixed seed keeps the storm reproducible
	safe := []string{"1", "2", "3", "j", "k", "?", "G", "g", "\t", "\x1b"}
	for range 60 {
		s.send(tea.WindowSizeMsg{Width: 1 + rng.IntN(200), Height: 1 + rng.IntN(60)})
		k := safe[rng.IntN(len(safe))]
		switch k {
		case "\t":
			s.send(tea.KeyMsg{Type: tea.KeyTab})
		case "\x1b":
			s.send(tea.KeyMsg{Type: tea.KeyEsc})
		default:
			s.keys(k)
		}
	}
	for i, st := range s.finish(tea.Quit()) {
		if st.misfit != "" {
			t.Errorf("state %d: %s", i, st.misfit)
		}
	}
}
