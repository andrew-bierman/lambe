# Contributing to lambe

lambe is a Go supervisor daemon for autonomous coding agents. Thanks for
helping.

## Prerequisites

- [mise](https://mise.jdx.dev/) — the repo pins the Go toolchain and tools
  via `mise.toml` and `go.mod` tool directives.

## Everyday commands

```sh
mise exec go -- go build ./...
mise exec go -- go vet ./...
mise exec go -- go test ./...
mise exec go -- go tool gofumpt -l .        # must print nothing
mise exec go -- go tool golangci-lint run
mise exec go -- go tool govulncheck ./...
mise exec go -- go tool staticcheck ./...
```

## Code conventions

- Go 1.26 language floor; formatting via gofumpt.
- Configuration is koanf-based (`internal/config`): defaults < file < env <
  flags. See the package comment for the env flattening rule
  (`LAMBE_SUPERVISOR_TICK_SECONDS` ← `supervisor.tick_seconds`).
- Logging is stdlib `log/slog` JSON (`internal/logging`): constant message
  strings with structured attributes — never Sprintf'd messages.
- Validation must collect and report every problem in one pass.

## Commits

Commit messages follow the repo's gitmoji + imperative summary convention
(see `git log`). Keep commits focused; one logical change per commit.

## Licensing

By contributing you agree your contributions are licensed under the GNU GPL v3.0 or
later, as in [LICENSE](LICENSE).
