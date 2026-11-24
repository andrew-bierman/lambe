# Upgrading

Lambe follows [SemVer](https://semver.org). While the version is 0.x, minor
releases may change behavior; each change that needs your attention is called
out here and in the [CHANGELOG](../CHANGELOG.md). The support policy and what
counts as public API are in [RELEASING.md](RELEASING.md).

## Every upgrade

1. **Keep the state directory** (`lambe state-dir`). It holds the checkpoints
   that let a restart resume pinned workflows, attempts and retry deadlines.
2. Install the new binary (`brew upgrade lambe`, your package manager, the
   install script, or `go install github.com/andrew-bierman/lambe/cmd/lambe@latest`).
3. Run `lambe doctor`. It reports a config that no longer loads, a service
   unit pointing at an old path, and stalls.
4. Restart the daemon: `lambe service install --force` rewrites and reloads the
   unit. Restarting is safe; in-flight runs are checkpointed and resumed.
5. After a *reinstall of the binary at a new path*, re-run `lambe service install --force`
   so launchd/systemd points at it.

## Changes that may need action

### Unreleased (next release)

- **Config files are now validated strictly.** An unknown key (including a
  typo such as `silence_timout`) or a value of the wrong type (for example
  `attempt_cap: banana`) is a load error, where before it was silently ignored
  or read as 0. Run `lambe doctor` or `lambe status` after upgrading; the error
  names the file, the key, and a "did you mean" suggestion. The full list of
  valid keys is the [configuration reference](reference/config.md).
- **`lambe init` no longer adopts a tracker above the current directory.** It
  uses a `.beads` in the directory you ran it from, and needs `--tracker PATH`
  (or an interactive confirmation that names the path) to use an ancestor's. It
  never writes into your home directory on its own. If an earlier `lambe init`
  left a stray `WORKFLOW.md` or `.beads/formulas/keep-tests-green.formula.toml`
  in `~`, you can delete them.
- **Errors from one-shot commands are plain text.** Scripts that parsed the JSON
  log line from a failing `lambe status` should read the exit code instead
  (see [exit codes](REFERENCE.md#help-and-exit-codes)); `lambe run` and `lambe mcp`
  still log JSON.
- **Usage errors exit 2** consistently (some commands exited 1), and `lambe
  --help`, `help`, and `--version` now succeed on stdout.
- `lambe doctor -service-label NAME` is now `--label NAME`; the old spelling still works.
- `lambe status -json` returns `[]` instead of `null` for empty lists.
- New: `lambe completion bash|zsh|fish`, installed by Homebrew and the Linux packages.

### 0.11.0

- `supervisor.orphan_claim_ttl` defaults to 1h: the daemon stops heartbeating an
  orphaned claim whose agent is provably gone. Set it to `0` for the old
  behavior. Inspect with `lambe claims list`; release with `lambe claims release`.
- `bd` calls are capped (`supervisor.bd_max_concurrency`, default 3) behind a
  priority queue. On a slow embedded hub, raise `supervisor.bd_timeout`.
- `lambe status` exits 2 on a usage error (it was 1).

### 0.10.0

- Lambe is licensed GPL-3.0-or-later (earlier releases remain MIT).
- Releases use one `vX.Y.Z` tag for the CLI and the Mac app.

## Rolling back

Downgrades are not tested. If you must, back up the state directory first,
install the previous version and restart the daemon. The tracker is the source
of truth, so any claim an older version cannot resume shows up as an orphaned
claim you can inspect with `lambe claims list`.
