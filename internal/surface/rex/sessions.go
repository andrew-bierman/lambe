package rex

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
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
	// ListSessions returns the sessions visible to lambe.
	ListSessions(ctx context.Context) ([]Session, error)
	// Capture returns the captured output of one session. Output is
	// returned whole; callers must not grep it for state.
	Capture(ctx context.Context, sessionID string) (string, error)
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

// call invokes one `rex api call` method and decodes its JSON response.
func (c *ShellClient) call(ctx context.Context, result any, args ...string) error {
	if c.run == nil {
		return fmt.Errorf("rex: shell client has no runner")
	}
	full := append([]string{"api", "call"}, args...)
	full = append(full, "--json")
	stdout, code, err := c.run(ctx, "rex", full...)
	if err != nil {
		return fmt.Errorf("rex api call %s: %w", args[0], err)
	}
	if code != 0 {
		return fmt.Errorf("rex api call %s: exit %d: %s", args[0], code, truncate(stdout))
	}
	if err := json.Unmarshal([]byte(stdout), result); err != nil {
		return fmt.Errorf("rex api call %s: decode response: %w", args[0], err)
	}
	return nil
}

// CreateSession creates a labeled session for a supervised agent. Labels are
// passed as repeated key=value arguments in deterministic order.
func (c *ShellClient) CreateSession(ctx context.Context, req SessionRequest) (Session, error) {
	args := []string{"session.create", "--name", req.Name}
	for _, k := range sortedKeys(req.Labels) {
		args = append(args, "--label", k+"="+req.Labels[k])
	}
	var s Session
	if err := c.call(ctx, &s, args...); err != nil {
		return Session{}, err
	}
	if s.ID == "" {
		return Session{}, fmt.Errorf("rex session.create returned no session id")
	}
	return s, nil
}

// ListSessions lists sessions visible to lambe.
func (c *ShellClient) ListSessions(ctx context.Context) ([]Session, error) {
	var out []Session
	if err := c.call(ctx, &out, "session.list"); err != nil {
		return nil, err
	}
	return out, nil
}

// Capture returns the captured output of one session, uninterpreted.
func (c *ShellClient) Capture(ctx context.Context, sessionID string) (string, error) {
	if c.run == nil {
		return "", fmt.Errorf("rex: shell client has no runner")
	}
	stdout, code, err := c.run(ctx, "rex", "capture", "--json", sessionID)
	if err != nil {
		return "", fmt.Errorf("rex capture %s: %w", sessionID, err)
	}
	if code != 0 {
		return "", fmt.Errorf("rex capture %s: exit %d: %s", sessionID, code, truncate(stdout))
	}
	var out struct {
		Output string `json:"output"`
	}
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		return "", fmt.Errorf("rex capture %s: decode response: %w", sessionID, err)
	}
	return out.Output, nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// truncate shortens command output embedded in error messages.
func truncate(s string) string {
	const maxOut = 200
	if len(s) > maxOut {
		return s[:maxOut] + "..."
	}
	return s
}
