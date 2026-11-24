package pi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/andrew-bierman/lambe/internal/proc"
)

// ClientOptions configures one RPC client and the process it manages.
type ClientOptions struct {
	// Path is the executable to run; "pi" is typical.
	Path string
	// Args are the command-line arguments after Path, e.g. --mode rpc.
	Args []string
	// Dir is the working directory; Pi discovers its config from it.
	Dir string
	// Env is the child environment; nil inherits the parent's.
	Env []string
	// Stderr receives the child's diagnostics; never protocol data.
	Stderr io.Writer
	// Logger receives lifecycle events; nil discards them.
	Logger *slog.Logger
	// Session identifies the supervised session for the Persist seam.
	Session string
	// Persist runs between process creation and spawn return; see
	// spawnOptions.Persist.
	Persist func(pgid int, session string) error
	// WaitDelay bounds post-exit pipe waits; zero means the proc default.
	WaitDelay time.Duration
	// OnRecord, when set, is invoked for every valid stdout record; the
	// watchdog liveness hook.
	OnRecord func()
}

// Client is the Pi RPC client: it owns the spawned process, correlates
// command responses by id, and exposes session events as a stream. Command
// handling inside Pi is asynchronous; responses may arrive in any order.
type Client struct {
	proc    *agentProc
	log     *slog.Logger
	events  chan Record
	pending sync.Map // id -> chan Record
	nextID  atomic.Int64
	writeMu sync.Mutex
	done    chan struct{}
}

// StartClient spawns the Pi process and starts its read loop. Event
// records begin flowing to Events immediately, so listeners attached right
// after StartClient returns cannot miss a fast completion before the first
// prompt.
func StartClient(opts ClientOptions) (*Client, error) {
	p, err := spawnAgent(spawnOptions{
		Path:      opts.Path,
		Args:      opts.Args,
		Env:       opts.Env,
		Dir:       opts.Dir,
		Stderr:    opts.Stderr,
		Logger:    opts.Logger,
		Session:   opts.Session,
		Persist:   opts.Persist,
		WaitDelay: opts.WaitDelay,
		OnLine:    opts.OnRecord,
	})
	if err != nil {
		return nil, err
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	c := &Client{
		proc:   p,
		log:    logger,
		events: make(chan Record, 256),
		done:   make(chan struct{}),
	}
	go c.readLoop()
	return c, nil
}

// Events returns the stream of session-event records: everything on stdout
// that is not a correlated command response, including id-less responses.
// The channel closes when the stdout stream ends.
func (c *Client) Events() <-chan Record { return c.events }

// Done returns a channel closed when the stdout stream has ended (process
// exit, EOF, or bounded teardown).
func (c *Client) Done() <-chan struct{} { return c.done }

// Pgid returns the managed process's group id.
func (c *Client) Pgid() int { return c.proc.Pgid() }

// Call sends one command and blocks until its correlated response arrives,
// the context is done, or the stream ends. success:false responses return
// a *CommandError.
func (c *Client) Call(ctx context.Context, cmd Command) (Record, error) {
	id := fmt.Sprintf("c%d", c.nextID.Add(1))
	line, err := cmd.withID(id)
	if err != nil {
		return Record{}, fmt.Errorf("marshal %s: %w", cmd.Type, err)
	}
	ch := make(chan Record, 1)
	c.pending.Store(id, ch)
	defer c.pending.Delete(id)
	if err := c.write(line); err != nil {
		return Record{}, err
	}
	select {
	case rec := <-ch:
		if !rec.OK() {
			return rec, &CommandError{Command: cmd.Type, Message: rec.Error}
		}
		return rec, nil
	case <-c.done:
		return Record{}, fmt.Errorf("pi %s: stream ended before response", cmd.Type)
	case <-ctx.Done():
		return Record{}, fmt.Errorf("pi %s: %w", cmd.Type, ctx.Err())
	}
}

// Notify sends one command without waiting for its response. Uncorrelated
// responses flow to Events. Use for fire-and-forget steering probes and
// aborts against a possibly-hung child.
func (c *Client) Notify(cmd Command) error {
	line, err := cmd.withID("")
	if err != nil {
		return fmt.Errorf("marshal %s: %w", cmd.Type, err)
	}
	return c.write(line)
}

// Wait blocks until the process is reaped and returns its wait error; a
// clean exit after a descendant held the pipe is nil.
func (c *Client) Wait() error { return c.proc.Wait() }

// WaitErr blocks until the process is reaped and returns its wait error.
func (c *Client) WaitErr() error { return c.proc.WaitErr() }

// CloseStdin requests an orderly shutdown by closing the child's stdin.
func (c *Client) CloseStdin() { c.proc.CloseStdin() }

// Close shuts the process down: stdin close for an orderly exit, a grace
// period, then the proc kill ladder. A grace of zero or less means the
// proc default. A deliberate-kill exit status is not an error.
func (c *Client) Close(ctx context.Context, grace time.Duration) error {
	if grace <= 0 {
		grace = proc.TerminateGrace
	}
	c.proc.CloseStdin()
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-c.proc.WaitDone():
	case <-timer.C:
	case <-ctx.Done():
	}
	return c.proc.Terminate(ctx, grace)
}

// Terminate runs the kill ladder directly, without an orderly attempt.
func (c *Client) Terminate(ctx context.Context, grace time.Duration) error {
	return c.proc.Terminate(ctx, grace)
}

// write serializes one command line onto stdin under the write mutex.
func (c *Client) write(line []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := c.proc.WriteLine(line); err != nil {
		if errors.Is(err, os.ErrClosed) {
			return fmt.Errorf("agent stdin closed: %w", err)
		}
		return err
	}
	return nil
}

// readLoop demultiplexes stdout records: correlated responses go to their
// pending Call; everything else flows to Events until the stream ends.
func (c *Client) readLoop() {
	defer close(c.done)
	defer close(c.events)
	for line := range c.proc.Lines() {
		var rec Record
		if err := json.Unmarshal(line, &rec); err != nil {
			c.log.Warn("unparseable record", "err", err, "line", string(line))
			continue
		}
		if rec.IsResponse() && rec.ID != "" {
			if v, ok := c.pending.LoadAndDelete(rec.ID); ok {
				if ch, isChan := v.(chan Record); isChan {
					ch <- rec
					continue
				}
			}
		}
		c.events <- rec
	}
}
