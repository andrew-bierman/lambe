package acp

import "fmt"

// agents maps the harness-level agent names lambe knows about to the argv
// that launches each agent in ACP mode over stdio. The table is deliberately
// tiny: any other agent is reachable by passing Argv directly in Options.
var agents = map[string][]string{
	// The maintained adapter exposes Codex app-server over ACP.
	"codex": {"codex-acp"},
	// OpenCode exposes its ACP server as `opencode acp`.
	"opencode": {"opencode", "acp"},
	// Gemini CLI exposes its ACP server as `gemini --acp`.
	"gemini": {"gemini", "--acp"},
}

// Agents returns the harness names with a known ACP launch command.
func Agents() []string {
	names := make([]string, 0, len(agents))
	for name := range agents {
		names = append(names, name)
	}
	return names
}

// Lookup resolves one harness-level agent name to its ACP launch argv, e.g.
// "opencode" → ["opencode", "acp"]. The returned slice is a copy and may be
// freely modified by the caller.
func Lookup(name string) ([]string, error) {
	argv, ok := agents[name]
	if !ok {
		return nil, fmt.Errorf("unknown acp agent %q", name)
	}
	out := make([]string, len(argv))
	copy(out, argv)
	return out, nil
}
