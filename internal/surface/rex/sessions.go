package rex

import (
	"context"
	"encoding/json"
	"fmt"
)

// Session is one Rex session as returned by the Rex server API.
type Session struct {
	// ID is the server-assigned session identifier.
	ID string `json:"id"`
	// Name is the session's display name.
	Name string `json:"name"`
}

// SessionRequest describes a session lambe wants created for one supervised
// agent.
type SessionRequest struct {
	// Name is the session name; lambe uses the agent's name.
	Name string `json:"name"`
	// Labels attach structured metadata, e.g. surface=lambe.
	Labels map[string]string `json:"labels,omitempty"`
}

// Client is the thin Rex server seam. Production code shells the `rex` CLI
// via ShellClient; tests provide scripted fakes.
type Client interface {
	// CreateSession creates a labeled session for a supervised agent.
	CreateSession(ctx context.Context, req SessionRequest) (Session, error)
	// ListSessions returns the sessions visible to lambe, so a session made
	// by an earlier daemon is found instead of created again.
	ListSessions(ctx context.Context) ([]Session, error)
}

// ShellClient implements Client by shelling the rex CLI with --json.
type ShellClient struct {
	// run executes one command.
	run CommandRunner
}

// NewShellClient returns a Client that drives the `rex` CLI through the
// given runner.
func NewShellClient(run CommandRunner) *ShellClient {
	return &ShellClient{run: run}
}

// call runs `rex ARGS... --json` and decodes its JSON response.
func (c *ShellClient) call(ctx context.Context, result any, args ...string) error {
	if c.run == nil {
		return fmt.Errorf("rex: shell client has no runner")
	}
	full := append([]string{}, args...)
	full = append(full, "--json")
	stdout, code, err := c.run(ctx, "rex", full...)
	if err != nil {
		return fmt.Errorf("rex %s: %w", args[0], err)
	}
	if code != 0 {
		return fmt.Errorf("rex %s: exit %d: %s", args[0], code, truncate(stdout))
	}
	if err := json.Unmarshal([]byte(stdout), result); err != nil {
		return fmt.Errorf("rex %s: decode response: %w", args[0], err)
	}
	return nil
}

// CreateSession creates a session named req.Name. `rex new` takes a label
// only, so req.Labels is not sent.
func (c *ShellClient) CreateSession(ctx context.Context, req SessionRequest) (Session, error) {
	var created struct {
		ID string `json:"session_id"`
	}
	if err := c.call(ctx, &created, "new", req.Name); err != nil {
		return Session{}, err
	}
	if created.ID == "" {
		return Session{}, fmt.Errorf("rex new returned no session id")
	}
	s := Session{ID: created.ID, Name: req.Name}
	return s, nil
}

// ListSessions lists sessions visible to lambe.
func (c *ShellClient) ListSessions(ctx context.Context) ([]Session, error) {
	var out struct {
		Sessions []struct {
			ID   string `json:"session_id"`
			Name string `json:"label"`
		} `json:"sessions"`
	}
	if err := c.call(ctx, &out, "ls"); err != nil {
		return nil, err
	}
	sessions := make([]Session, 0, len(out.Sessions))
	for _, s := range out.Sessions {
		sessions = append(sessions, Session{ID: s.ID, Name: s.Name})
	}
	return sessions, nil
}

// truncate shortens command output embedded in error messages.
func truncate(s string) string {
	const maxOut = 200
	if len(s) > maxOut {
		return s[:maxOut] + "..."
	}
	return s
}
