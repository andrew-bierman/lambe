# Best-practices research — lambe supervisor daemon (Go, 2026)

Authority levels: [A] official docs/stdlib/source · [B] authoritative practitioners/maintainers · [C] community consensus.

## 1. Supervising child processes

**Process groups are the kill primitive; build them into spawn from day one.**
- Start every agent child with `SysProcAttr{Setpgid: true}` so the child leads its own group; kill the tree with `syscall.Kill(-pgid, sig)`. Without Setpgid, group-kill hits the daemon itself ("suicide"). [A: golang/go exec_linux.go; B: Felix Geisendörfer]
- macOS and Linux both support Setpgid + negative-PID kill; keep the code in one `killTree(pgid, sig)` helper behind build tags only if Windows is ever added. [B: luth.io containment comparison, 2026]
- SIGTERM→grace→SIGKILL escalation ladder: send SIGTERM to the group, wait a bounded grace, then SIGKILL the group. Go 1.20+ `Cmd.Cancel` + `Cmd.WaitDelay` give you most of this: `Cancel` fires on context cancellation (use it to group-SIGINT/SIGTERM), `WaitDelay` bounds the post-exit pipe wait and force-kills stragglers. Set `WaitDelay = grace + buffer` so your own group-SIGKILL always wins the race. [A: os/exec docs; B: Buildkite agent PR #4141, 2026]

**PDEATHSIG: Linux-only belt-and-suspenders, not a mechanism to rely on.**
- `Pdeathsig` in `SysProcAttr` exists on Linux only; no macOS equivalent. Also fires on the death of the *creating thread*, not the process (go.dev/issue/27505), which is unreliable in a Go runtime. [A: golang/go]
- The daemon's crash-orphan story must instead be handled at restart: on startup, scan for surviving agent process groups (recorded pgids in Beads local state) and either adopt (resume) or group-kill them. This is a *design constraint*: persisted pgid/session bindings must be written before `Start()` returns.

**The StdoutPipe footgun — the single most likely bug class for this codebase.**
- Calling `cmd.Wait()` concurrently with a `StdoutPipe` reader silently discards the child's final buffered frames (the result frame with cost/status). Wait returns nil; nothing looks wrong. Documented: "incorrect to call Wait before all reads from the pipe have completed." Reproduced deterministically 2026 (136/200 frames read, result frame lost) — Linux-only flakes. [A: os/exec docs; B: luth.io "stdoutpipe footgun", Sep 2026 — a supervisor for claude/codex JSONL, i.e., exactly lambe's shape]
- **Pattern to mandate:** one goroutine owns `Wait` and it is the *only* closer of pipe read-ends, sequenced after readers return EOF; or skip `StdoutPipe` entirely and pass `os.Pipe()` ends / `cmd.Stdout = writer` so os/exec does the copying. Every spawn path (fresh AND resume) must be reaped — unreaped children are zombies for the daemon's lifetime. [B: luth.io; A: golang/go issue #59055]
- Orphaned grandchildren holding the pipe write-end keep `Wait` blocked past child exit. `WaitDelay` bounds this; treat `exec.ErrWaitDelay` on an otherwise-clean exit as success, not failure. [B: Buildkite #4141; A: os/exec docs]

**Watchdog (timeout-on-silence).**
- Two independent timers per child: max-turn deadline (context) and silence watchdog (reset on every valid JSONL event). On silence expiry, run the same escalation ladder (group SIGTERM → grace → group SIGKILL). No standard library exists for this; it's a ~50-line component. Feeds the architecture's "heartbeat ≠ last-successful-tick" distinction.

## 2. Daemon lifecycle

**Single-instance: flock held for life, never delete the lock file.**
- Use `flock(LOCK_EX|LOCK_NB)` (works on macOS + Linux via `syscall.Flock` or gofrs/flock). The kernel releases it on *any* death including SIGKILL — a crashed daemon cannot leave a stale lock, which directly serves the kill -9 fault-injection requirement. [A: flock(2); B: hollis-labs localdaemon, ubgo/lock docs]
- Anti-patterns to forbid: O_EXCL pidfiles as the lock (stale on crash), pidfile-liveness as identity (PID recycling kills someone else's process), deleting the lock file (inode split → two holders). PID file is a *hint* for humans/stop commands, never the arbiter. Never lock on NFS. [B: multiple]
- Order: acquire flock → bind sockets → write pidfile. Verify PID identity (proc start-time check) before ever signaling a pid from a pidfile. [B]

**Graceful shutdown & crash-only design.**
- Trap SIGTERM/SIGINT; forward to each child's process group with the escalation ladder; wait with bound; then exit. Keep restart idempotent: all reconcile decisions derive from Beads state, never from in-memory state — the daemon must be safe to SIGKILL at any instruction. [C: crash-only design, standard for supervisors]
- Recommend, don't require, supervisors: ship a `LaunchAgent` plist for macOS (launchd `KeepAlive`) and a systemd **user** unit (`Restart=on-failure`) as docs/examples, not installed by default. Don't double-fork/daemonize; run foreground and let launchd/systemd own backgrounding. Also `SIGHUP` = config reload is convention. [B: bytelearn.dev daemons; C]

**Logging: slog (stdlib) is settled practice in 2026.**
- `slog.JSONHandler` to stderr/stdout; constant messages with variable attrs (never Sprintf'd messages); enrich per-operation loggers (`slog.With("resp_id", …, "attempt", …)`); runtime level via `*slog.LevelVar`. Go 1.26 adds `slog.NewMultiHandler` (human text + machine JSON). For post-mortem debuggability include child pid, pgid, session id, and turn attempt in every lifecycle log line. [A: go.dev/blog/slog; B: 2025-26 conventions writeups]

## 3. Layout, config, toolchain

- **Layout:** standard small-binary layout: `cmd/lambe/` (plus `cmd/lambed/` only if daemon is a separate binary), `internal/` for everything else (supervisor, harness drivers, store, config). No `pkg/`. Interfaces at the architecture's seams live in `internal/`. [C: strong 2025-26 consensus, Go team wiki]
- **Config:** defaults < file < env < flags. **koanf v2** is the 2026 consensus pick for this shape (explicit provider/merge order, small dep tree, case-sensitive keys); viper is widely advised *against* for greenfield (key-lowercases spec violations, env-binding bug graveyard #641/#2001, 4x binary bloat). Alternative: caarlos0/env + hand-rolled file merge if the layering stays simple. Write the env-key flattening rule down on day one. [B: multiple 2026 comparisons; A-adjacent: koanf README]
- Validate config before booting; fail with all errors collected, not first. [B]
- **Toolchain:** Go 1.27 is current (Aug 2026, patch 1.27.4 Nov 2026); 1.26 (Feb 2026) is the conservative floor and brings `new(expr)`, `errors.AsType`, `slog.NewMultiHandler`, Green Tea GC default. Note Go 1.27: `encoding/json` v2 backing, macOS 12 dropped (1.27 requires macOS 13+ — fine). Pin `go 1.26` in go.mod minimum; JSONL parsing benefits from json/v2 error messages. [A: go.dev release history]

## 4. Testing

- **Re-exec helper pattern is the established harness** (stdlib's own approach): `TestMain` dispatches on env var (e.g. `LAMBE_TEST_CHILD=hang|silent|exit0|spam`); tests spawn `os.Args[0]` re-exec'd as the fake agent. Real subprocess, real signals, fully scriptable failure modes (emit events then go silent; fork a grandchild that holds stdout open; ignore SIGTERM). [A: os/exec exec_test.go; B: joeshaw, abhinavg, rednafi]
- A `testing.Entrypoint` API for this is proposed (golang/go #79941, Jun 2026) but not shipped — use the TestMain env-var form. [A]
- Build the real daemon binary once in `TestMain` for fault-injection tests that need a *separate* daemon process (kill -9 the daemon, restart, assert idempotency). Guard with `testing.Short()` skip. [B: CLI integration patterns]
- Determinism rules from the shipmates postmortem: force the timing in tests rather than hoping (hold a reader past the bound; put the frame in the pipe *then* complete the reap). Every kill-path test asserts both the signal was delivered AND the child was reaped (no zombie). A leaked-grace timeout is a deadlock guard, never the thing that decides whether data arrived. [B: luth.io]
- Fault-injection suite maps directly: daemon kill -9 → flock-kernel-release + Beads-state-derived restart; agent SIGKILL mid-turn → re-exec fake killed externally, assert resume-by-session and no duplicate side effects (idempotency key = pinned issue id); duplicate tick → flock + claim semantics.

## Sequencing / scope implications

1. The pipe/Wait/reap discipline and Setpgid-killTree belong in the *first* spawn abstraction, not later — retrofitting them is the documented way these bugs ship twice.
2. Persist pgid + session binding *before* Start() returns, or crash-recovery can't find orphaned agents.
3. flock single-instance + state-derived idempotency are what make kill -9 tests pass cheaply; design them in slice one.
4. Config: pick koanf (or stdlib-only) at kickoff; define env-key flattening rule day one.
