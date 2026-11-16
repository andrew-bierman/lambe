// Package testchild implements a re-exec fake coding agent for tests of the
// process-supervision primitives. A test binary opts in from its TestMain via
// [Main] or [MaybeReexec]; when the behavior environment variable is set, the
// binary runs the named fake-agent behavior instead of the test suite. Every
// behavior speaks the trivial JSONL frame shape {"type":"...","data":{...}}.
package testchild

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"
)

// EnvVar selects the fake-agent behavior on re-exec.
const EnvVar = "LAMBE_TEST_CHILD"

// Behavior names understood by the re-exec dispatcher.
const (
	// BehaviorHang emits one frame, then blocks forever.
	BehaviorHang = "hang"
	// BehaviorSilent emits nothing and stays alive.
	BehaviorSilent = "silent"
	// BehaviorExit0 exits cleanly and immediately.
	BehaviorExit0 = "exit0"
	// BehaviorSpam emits many frames quickly, then exits.
	BehaviorSpam = "spam"
	// BehaviorIgnoreSIGTERM traps SIGTERM and keeps running; only SIGKILL ends it.
	BehaviorIgnoreSIGTERM = "ignore-sigterm"
	// BehaviorGrandchildHoldsPipe forks a grandchild that inherits stdout and
	// stderr, then exits cleanly while the grandchild keeps the pipes open.
	BehaviorGrandchildHoldsPipe = "grandchild-holds-pipe"
	// BehaviorLimitError emits provider-error-shaped frames, then settles.
	BehaviorLimitError = "limit-error"
	// BehaviorStreamingThenSettled emits streaming events, then one large
	// settle frame, then exits.
	BehaviorStreamingThenSettled = "streaming-then-settled"
)

// Main is the TestMain hook: it re-execs as the fake agent when EnvVar is set
// and otherwise runs the test suite.
func Main(m *testing.M) {
	MaybeReexec()
	os.Exit(m.Run())
}

// MaybeReexec runs the fake-agent behavior selected by EnvVar and exits the
// process; it returns immediately when EnvVar is unset, letting the normal
// test binary continue.
func MaybeReexec() {
	behavior := os.Getenv(EnvVar)
	if behavior == "" {
		return
	}
	run(behavior)
	// Long-lived behaviors never return; short ones fall through to exit 0.
	os.Exit(0)
}

// run dispatches one behavior on the current process.
func run(behavior string) {
	switch behavior {
	case BehaviorHang:
		emit("progress", map[string]string{"note": "starting"})
		block()
	case BehaviorSilent:
		block()
	case BehaviorExit0:
		return
	case BehaviorSpam:
		for i := range 500 {
			emit("event", map[string]string{"seq": fmt.Sprintf("%d", i)})
		}
		emit("result", map[string]string{"status": "done"})
	case BehaviorIgnoreSIGTERM:
		discard := make(chan os.Signal, 1)
		signal.Notify(discard, syscall.SIGTERM)
		block()
	case BehaviorGrandchildHoldsPipe:
		startGrandchild(BehaviorSilent)
		return
	case BehaviorLimitError:
		for range 3 {
			emit("error", map[string]string{"code": "rate_limited"})
		}
		emit("result", map[string]string{"status": "settled"})
		block()
	case BehaviorStreamingThenSettled:
		for i := range 20 {
			emit("stream", map[string]string{"seq": fmt.Sprintf("%d", i)})
		}
		// The settle frame is deliberately larger than a pipe buffer so a
		// buggy concurrent-wait teardown loses it.
		emit("result", map[string]string{"status": "done", "payload": strings.Repeat("x", 256*1024)})
		// A final unterminated frame: a correct EOF drain delivers it, a
		// StdoutPipe-style close on Wait loses it.
		_, _ = fmt.Fprint(os.Stdout, `{"type":"trailing"}`)
		return
	default:
		fmt.Fprintf(os.Stderr, "testchild: unknown behavior %q\n", behavior)
		os.Exit(2)
	}
}

// emit writes one JSONL frame to stdout. Each Fprintln is one write, so no
// buffering can reorder or drop frames.
func emit(typ string, data map[string]string) {
	b, err := json.Marshal(struct {
		Type string            `json:"type"`
		Data map[string]string `json:"data"`
	}{Type: typ, Data: data})
	if err != nil {
		fmt.Fprintf(os.Stderr, "testchild: marshal %q: %v\n", typ, err)
		os.Exit(2)
	}
	_, _ = fmt.Fprintf(os.Stdout, "%s\n", b)
}

// block sleeps forever; a plain select{} would trip the runtime deadlock
// detector in a process with no other runnable goroutines.
func block() {
	for {
		time.Sleep(time.Hour)
	}
}

// startGrandchild launches a grandchild that inherits stdout and stderr and
// remains in the same process group, then returns without waiting for it.
func startGrandchild(behavior string) {
	//nolint:gosec // G204: the binary is this very test binary, re-exec'd.
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), EnvVar+"="+behavior)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "testchild: grandchild: %v\n", err)
		os.Exit(2)
	}
}
