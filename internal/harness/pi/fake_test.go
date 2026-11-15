package pi_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Fake-agent re-exec plumbing: the test binary re-execs as a scripted Pi
// when the behavior env var is set, mirroring internal/testchild's re-exec
// approach but speaking the Pi RPC protocol (commands on stdin, responses
// and events on stdout). Every received line and the argv are appended to
// a log file so tests can assert exactly which commands were sent.
const (
	fakeEnv    = "LAMBE_TEST_PI_FAKE"
	fakeLogEnv = "LAMBE_TEST_PI_FAKE_LOG"
)

// Fake script names.
const (
	scriptHappy      = "happy"
	scriptRetried    = "retried"
	scriptStreaming  = "streaming"
	scriptLimit      = "limit"
	scriptSilentAck  = "silent-after-ack"
	scriptGrandkid   = "grandchild"
	scriptGrandSleep = "grandchild-sleep"
)

// TestMain re-execs as the fake Pi when the behavior env var is set.
func TestMain(m *testing.M) {
	if script := os.Getenv(fakeEnv); script != "" {
		if script == scriptGrandSleep {
			blockForever()
		}
		runFake(script)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// blockForever sleeps forever; a plain select{} would trip the runtime
// deadlock detector in a process with no other runnable goroutines.
func blockForever() {
	for {
		time.Sleep(time.Hour)
	}
}

// fakeLog appends one line to the log file when configured.
func fakeLog(format string, args ...any) {
	path := os.Getenv(fakeLogEnv)
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644) //nolint:gosec // G304: test-owned log path.
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	_, _ = fmt.Fprintf(f, format+"\n", args...)
}

// sessionIDFromArgs extracts the --session-id value from the fake's argv.
func sessionIDFromArgs() string {
	args := os.Args
	for i := range len(args) - 1 {
		if args[i] == "--session-id" {
			return args[i+1]
		}
	}
	return "fake-session"
}

// runFake speaks just enough of the Pi RPC protocol to exercise the
// driver: get_state, get_session_stats, prompt, steer, follow_up, abort,
// and per-script event sequences.
func runFake(script string) {
	fakeLog("argv %s", strings.Join(os.Args, " "))
	sid := sessionIDFromArgs()
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		var cmd struct {
			ID   string `json:"id"`
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(line), &cmd); err != nil {
			continue
		}
		fakeLog("recv %s", line)
		switch cmd.Type {
		case "get_state":
			respond(cmd.ID, true, "", map[string]any{
				"isStreaming":         script == scriptStreaming,
				"sessionId":           sid,
				"sessionFile":         "/sessions/" + sid + ".jsonl",
				"pendingMessageCount": 0,
			})
		case "get_session_stats":
			respond(cmd.ID, true, "", map[string]any{
				"sessionId":   sid,
				"sessionFile": "/sessions/" + sid + ".jsonl",
				"tokens": map[string]any{
					"input": 120, "output": 34, "cacheRead": 0, "cacheWrite": 0, "total": 154,
				},
				"cost": 0.012,
				"contextUsage": map[string]any{
					"tokens": 50000, "contextWindow": 200000, "percent": 25.0,
				},
			})
		case "prompt":
			if script == scriptStreaming {
				// A prompt during streaming without streamingBehavior is
				// a protocol error; the driver must have sent steer.
				fakeLog("PROTOCOL-VIOLATION prompt-during-streaming")
				respond(cmd.ID, false, "agent is streaming: prompt requires streamingBehavior", nil)
				continue
			}
			respond(cmd.ID, true, "", map[string]any{"disposition": "started"})
			go scriptEvents(script)
		case "steer", "follow_up":
			respond(cmd.ID, true, "", map[string]any{"disposition": "queued"})
			go scriptEvents(script)
		case "abort":
			respond(cmd.ID, true, "", nil)
		default:
			respond(cmd.ID, false, "unknown command "+cmd.Type, nil)
		}
	}
}

// respond writes one command response record.
func respond(id string, success bool, errMsg string, data any) {
	rec := map[string]any{
		"id":      id,
		"type":    "response",
		"success": success,
	}
	if data != nil {
		rec["data"] = data
	}
	if errMsg != "" {
		rec["error"] = errMsg
	}
	writeRecord(rec)
}

// event writes one session event record.
func event(typ string, fields map[string]any) {
	rec := map[string]any{"type": typ}
	for k, v := range fields {
		rec[k] = v
	}
	writeRecord(rec)
}

// writeRecord marshals and writes one LF-terminated JSON record.
func writeRecord(rec map[string]any) {
	b, err := json.Marshal(rec)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fake pi: marshal: %v\n", err)
		os.Exit(2)
	}
	fmt.Printf("%s\n", b)
}

// scriptEvents emits the scripted event sequence for one accepted prompt.
func scriptEvents(script string) {
	switch script {
	case scriptHappy, scriptStreaming:
		event("agent_start", nil)
		event("message_end", map[string]any{
			"message": map[string]any{
				"role":       "assistant",
				"content":    []map[string]any{{"type": "text", "text": "ready"}},
				"stopReason": "stop",
			},
		})
		event("agent_end", map[string]any{"willRetry": false})
		event("agent_settled", map[string]any{"aborted": false})
	case scriptRetried:
		event("agent_start", nil)
		event("auto_retry_start", map[string]any{
			"attempt": 1, "maxAttempts": 3, "delayMs": 1000, "errorMessage": "529 overloaded",
		})
		event("agent_end", map[string]any{"willRetry": true})
		// The gap before the retry makes a driver that mistakes
		// agent_end for completion observably early.
		time.Sleep(250 * time.Millisecond)
		event("agent_start", nil)
		event("agent_end", map[string]any{"willRetry": false})
		event("agent_settled", map[string]any{"aborted": false})
	case scriptLimit:
		for range 3 {
			event("auto_retry_start", map[string]any{
				"attempt": 1, "maxAttempts": 3, "delayMs": 5000,
				"errorMessage": "rate limit exceeded (HTTP 429): usage limit reached",
			})
		}
		blockForever()
	case scriptSilentAck:
		// Acked the prompt above, then goes silent forever.
		blockForever()
	case scriptGrandkid:
		event("agent_start", nil)
		event("agent_settled", map[string]any{"aborted": false})
		// Give the driver a beat to fetch per-turn stats before exiting.
		time.Sleep(200 * time.Millisecond)
		startFakeGrandchild()
		os.Exit(0)
	}
}

// startFakeGrandchild re-execs this binary as a silent descendant that
// inherits stdout, keeping the pipe open past the parent's exit.
func startFakeGrandchild() {
	//nolint:gosec // G204: the binary is this very test binary, re-exec'd.
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), fakeEnv+"="+scriptGrandSleep)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "fake pi: grandchild: %v\n", err)
		os.Exit(2)
	}
}
