// Package proc provides the process-supervision primitives every agent
// driver builds on: spawning children in their own process groups, an
// LF-strict JSONL frame reader, the SIGTERM → bounded grace → SIGKILL group
// ladder, and a silence/max-run watchdog.
//
// Two invariants govern the design. First, the kill primitive is the process
// group: children are started with Setpgid so a negative-pgid signal reaches
// the child and any descendants. Second, exactly one goroutine owns
// [os/exec.Cmd.Wait] and is the only closer of the stdout pipe read end, so a
// final frame written just before exit can never be truncated by a concurrent
// wait — the bug class [os/exec.Cmd.StdoutPipe] invites.
package proc
