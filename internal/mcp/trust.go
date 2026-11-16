package mcp

import (
	"fmt"
	"strconv"
	"strings"
)

// DepthEnvVar is the environment variable lambe exports to the agents it
// supervises. Its integer value is the agent's supervision depth: 0 for an
// agent started directly by a human, 1 for an agent started by that agent,
// and so on. The MCP server reads it from its own inherited environment to
// decide whether the calling agent may spawn further runs.
const DepthEnvVar = "LAMBE_DEPTH"

// DefaultMaxDepth is the default recursion ceiling. At 1, an agent that is
// itself lambe-supervised (depth >= 1) cannot spawn further runs.
const DefaultMaxDepth = 1

// depthFromEnv extracts the caller's supervision depth from an
// environment. A missing, malformed, or negative value means depth 0
// (a top-level, human-started agent).
func depthFromEnv(env []string) int {
	for _, kv := range env {
		if !strings.HasPrefix(kv, DepthEnvVar+"=") {
			continue
		}
		raw := strings.TrimPrefix(kv, DepthEnvVar+"=")
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			return n
		}
		return 0
	}
	return 0
}

// DepthError explains a refused spawn. It is returned by the spawn tool
// when the calling agent is already at or beyond the configured depth
// limit, so the refusal is a clear, machine-visible error rather than a
// silent no-op.
type DepthError struct {
	// CallerDepth is the depth of the agent that called lambe_spawn.
	CallerDepth int
	// MaxDepth is the configured ceiling.
	MaxDepth int
}

// Error implements error with a message written for agent consumption.
func (e *DepthError) Error() string {
	return fmt.Sprintf(
		"refusing to spawn: this agent runs at lambe supervision depth %d and the configured maximum is %d; "+
			"runs spawned by an already-supervised agent are disabled to bound prompt-injection recursion",
		e.CallerDepth, e.MaxDepth,
	)
}

// depthGuard decides whether a caller at a given depth may spawn a run at
// depth+1. The zero value refuses everything; use [newDepthGuard].
type depthGuard struct {
	max int
}

// newDepthGuard returns a guard with the given ceiling; a ceiling below 1
// is raised to [DefaultMaxDepth].
func newDepthGuard(maxDepth int) depthGuard {
	if maxDepth < 1 {
		maxDepth = DefaultMaxDepth
	}
	return depthGuard{max: maxDepth}
}

// check returns the depth the spawned run would run at, or a
// [*DepthError] when the caller is not allowed to spawn.
func (g depthGuard) check(callerDepth int) (int, error) {
	runDepth := callerDepth + 1
	if runDepth > g.max {
		return 0, &DepthError{CallerDepth: callerDepth, MaxDepth: g.max}
	}
	return runDepth, nil
}
