# lambe skills

This directory ships an installable agent skill for coding agents that are
supervised by lambe. It teaches the operating discipline lambe expects:
claim Beads work, post progress comments, respect external gates, never
self-declare done.

Install it from this repo with a skills registry CLI, e.g.
`npx skills add andrew-bierman/lambe`, or copy `skills/lambe/SKILL.md` into
your agent's skills directory. It works with any SKILL.md-compatible agent or
CLI — Claude Code, OpenCode, Codex, and similar.

For tool-call access to supervised runs, point an MCP client at
`lambe mcp` (a stdio MCP server); agents supervised by lambe find it on
their PATH next to the daemon binary.
