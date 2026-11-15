# AGENTS.md

Working conventions for this repository. Read before changing anything.

## Toolchain

Go is managed with mise (see `mise.toml`, currently Go 1.27). Run Go through
mise so the pinned version is used:

```sh
mise exec go -- go build ./...
mise exec go -- go test ./...
```

Developer tools (golangci-lint v2, gofumpt, staticcheck, govulncheck) are
pinned in `go.mod` via the `tool` directive. Run them with `go run`, e.g.:

```sh
go run mvdan.cc/gofumpt -l .
go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint run ./...
go run honnef.co/go/tools/cmd/staticcheck ./...
go run golang.org/x/vuln/cmd/govulncheck ./...
```

## Tests

Tests that need a real `bd` binary on PATH are gated behind `LAMBE_TEST_BD=1`
and skip when it is unset. CI always sets it; when working on tracker code,
set it locally so green means green:

```sh
LAMBE_TEST_BD=1 mise exec go -- go test -race ./...
```

Always run at least `go test ./...` (race detector where practical) before
finishing, plus gofumpt and the linters above.

## Commits

gitmoji + conventional commits: `✨ feat: ...`, `🐛 fix: ...`, `📝 docs: ...`,
`♻️ refactor: ...`, `✅ test: ...`, `🔧 chore: ...`.

## Package layout

- `cmd/lambe` — the single binary: `lambe run | status | version`
- `internal/config` — configuration loading, precedence, env flattening
- `internal/logging` — structured logging
- `internal/proc` — process supervision primitives
- `internal/testchild` — test-only child process helper
- `internal/tracker` — Beads tracker integration (hub/repo/in-repo layouts)
- `internal/symphony` — Symphony spec conformance; owns WORKFLOW.md semantics
- `internal/harness` — agent harness drivers (Pi first, ACP next)
- `internal/gate` — verified-completion gates (shell, gh:run, gh:pr)
- `internal/daemon` — the run loop, single-instance lock, status snapshots

## Quality bar

- Doc comments on every exported symbol.
- Strict typing: no `any` where a concrete type works, no stringly-typed state.
- Table-driven tests; test behavior through public seams.
- Clear prose in docs and comments; no marketing fluff.

## Before touching the loop

Agents: read the WORKFLOW.md semantics in `internal/symphony` before touching
the loop. The supervisor loop encodes the Symphony contract; changes there are
spec changes, not refactors.
