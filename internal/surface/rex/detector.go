// Package rex surfaces lambe's supervised runs into Rex, Superlogical's
// terminal. Presence is detected via REX_SESSION/REX_SERVER environment
// variables or a `rex ls` probe (exit code 4 means Rex is absent). Status is
// reported with OSC 7501 escape sequences; structured data flows through the
// `rex` CLI with JSON output. When Rex is absent every surface degrades to a
// no-op.
package rex

import (
	"context"
	"fmt"
)

// CommandRunner executes one command and returns its stdout, exit code, and
// error. It exists so tests can fake the shell; production code uses
// RunExec.
type CommandRunner func(ctx context.Context, name string, args ...string) (stdout string, exitCode int, err error)

// Reason explains how the detector reached its availability verdict.
type Reason int

const (
	// ReasonNone means no verdict has been reached yet.
	ReasonNone Reason = iota
	// ReasonEnv means environment variables named a Rex server.
	ReasonEnv
	// ReasonProbe means the rex CLI answered the probe.
	ReasonProbe
	// ReasonAbsent means rex reported itself absent (exit code 4).
	ReasonAbsent
	// ReasonProbeFailed means the probe could not reach a verdict.
	ReasonProbeFailed
)

// String returns the reason's short machine-stable name.
func (r Reason) String() string {
	switch r {
	case ReasonEnv:
		return "env"
	case ReasonProbe:
		return "probe"
	case ReasonAbsent:
		return "absent"
	case ReasonProbeFailed:
		return "probe failed"
	default:
		return "unknown"
	}
}

// Availability reports whether a Rex server can be reached and why the
// detector reached its verdict.
type Availability struct {
	// Available is true when lambe should surface into Rex.
	Available bool
	// Reason explains the verdict: env (environment variables named a Rex
	// server), probe (the rex CLI answered), absent (rex reports itself
	// absent, exit code 4), or probe failed.
	Reason Reason
}

// exitAbsent is the exit code `rex ls` uses when no Rex server exists.
const exitAbsent = 4

// Detector decides whether Rex is present. Env lookups and command execution
// are injected so tests can script both.
type Detector struct {
	// Env resolves an environment variable; nil means none ever match.
	Env func(key string) string
	// Runner executes a command; nil means detection cannot probe.
	Runner CommandRunner
}

// Detect reports Rex availability. Environment evidence (REX_SESSION or
// REX_SERVER) short-circuits the probe; otherwise `rex ls --json` is probed
// and exit code 4 is treated as a definitive absence rather than an error.
func (d Detector) Detect(ctx context.Context) Availability {
	for _, key := range []string{"REX_SESSION", "REX_SERVER"} {
		if lookup(d.Env, key) != "" {
			return Availability{Available: true, Reason: ReasonEnv}
		}
	}
	if d.Runner == nil {
		return Availability{Reason: ReasonProbeFailed}
	}
	_, code, err := d.Runner(ctx, "rex", "ls", "--json")
	switch {
	case err == nil && code == 0:
		return Availability{Available: true, Reason: ReasonProbe}
	case err == nil && code == exitAbsent:
		return Availability{Reason: ReasonAbsent}
	default:
		return Availability{Reason: ReasonProbeFailed}
	}
}

func lookup(env func(string) string, key string) string {
	if env == nil {
		return ""
	}
	return env(key)
}

// DetectError formats a probe failure for logs.
func (a Availability) DetectError() error {
	if a.Available || a.Reason == ReasonAbsent {
		return nil
	}
	return fmt.Errorf("rex detection failed: %s", a.Reason)
}
