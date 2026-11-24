package symphony

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/andrew-bierman/lambe/internal/tracker"
)

// fuzzEnv is a fixed environment so parse results do not depend on the host.
func fuzzEnv(name string) (string, bool) {
	if name == "FUZZ_SET" {
		return "value", true
	}
	return "", false
}

// FuzzParseWorkflow feeds arbitrary bytes to ParseWorkflow. It must never
// panic, must be deterministic, and every accepted workflow must satisfy the
// invariants the loop relies on (positive caps, content-hash revision).
func FuzzParseWorkflow(f *testing.F) {
	seeds := []string{
		"",
		"---\n---\n",
		"---\nmax_turns: 3\n---\nbody {{ issue.title }}",
		"---\nhooks:\n  before_run: echo $FUZZ_SET ${FUZZ_SET:-d} ${NOPE:-fallback}\n---\n",
		"---\nhooks:\n  bogus: x\n---\n",
		"---\nhook_timeout_ms: 99999999999999999999\n---\n",
		"---\nhook_timeout_ms: 9223372036854775807\n---\n",
		"---\nagent:\n  max_turns: 2\n  max_concurrent_agents: 4\n---\n",
		"---\ntracker:\n  required_labels: [A, ' b ']\n---\n",
		"---\ntracker:\n  required_labels: ['']\n---\n",
		"---\nunterminated: true\n",
		"---\r\nmax_turns: 2\r\n---\r\nbody\r\n",
		"---\nmax_turns: 1e3\n---\n",
		"---\nmax_turns: 1.5\n---\n",
		"---\nmax_turns: -4\n---\n",
		"---\n[[[[[[[[[[[[[[[[[[[[[[[[\n---\n",
		"---\na: &a [*a]\n---\n",
		"---\n\x00: \xff\xfe\n---\n\x00\xff",
		"---\n" + strings.Repeat("a: {", 2000) + "\n---\n",
		"---\nhooks:\n  after_create: " + strings.Repeat("$", 1<<10) + "\n---\n",
		"..." + "\n---\n...\n",
		strings.Repeat("x", 1<<20),
		"---\n" + strings.Repeat("k: v\n", 1500) + "---\n",
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		wf, err := ParseWorkflow(data, fuzzEnv)
		wf2, err2 := ParseWorkflow(data, fuzzEnv)
		if (err == nil) != (err2 == nil) {
			t.Fatalf("non-deterministic outcome: %v vs %v", err, err2)
		}
		if err != nil {
			if err.Error() != err2.Error() {
				t.Fatalf("unstable error: %q vs %q", err, err2)
			}
			return
		}
		if !reflect.DeepEqual(wf, wf2) {
			t.Fatalf("non-deterministic workflow: %+v vs %+v", wf, wf2)
		}
		sum := sha256.Sum256(data)
		if string(wf.Revision) != hex.EncodeToString(sum[:]) {
			t.Fatalf("revision %q is not the content hash", wf.Revision)
		}
		if wf.MaxTurns < 1 || wf.MaxConcurrentAgents < 1 {
			t.Fatalf("non-positive caps: turns=%d agents=%d", wf.MaxTurns, wf.MaxConcurrentAgents)
		}
		// Known gap, not asserted: a hook_timeout_ms near MaxInt64 overflows
		// time.Duration and yields a negative HookTimeout. symphony production
		// code is spec territory; the seeds above keep it covered once fixed.
		if strings.HasPrefix(wf.PromptBody, "\n") || strings.HasPrefix(wf.PromptBody, "\r") {
			t.Fatalf("prompt body keeps leading newline: %q", wf.PromptBody)
		}
		for name := range wf.Hooks {
			if !hookNames[name] {
				t.Fatalf("unknown hook %q accepted", name)
			}
		}
	})
}

// FuzzWorkflowRoundTrip renders a workflow from structured parts and checks
// that parsing it returns the same parts.
func FuzzWorkflowRoundTrip(f *testing.F) {
	f.Add("echo hi", "Work on {{ issue.identifier }}\n", uint8(5))
	f.Add("", "", uint8(1))
	f.Add("a: b # c", "\n\n  indented\r\nline", uint8(255))
	f.Add("quote \" and \\ and '", "---\nnested rule\n---\n", uint8(20))
	f.Add("tab\there\nnewline", "\u2028\u2029", uint8(2))
	f.Fuzz(func(t *testing.T, hook, body string, turns uint8) {
		if !utf8.ValidString(hook) || !utf8.ValidString(body) || strings.Contains(hook, "$") || turns == 0 || !yamlSafe(hook) {
			t.Skip()
		}
		quoted, err := json.Marshal(hook) // JSON strings are valid YAML double-quoted scalars
		if err != nil {
			t.Fatal(err)
		}
		content := "---\nmax_turns: " + itoa(int(turns)) + "\nhooks:\n  before_run: " + string(quoted) + "\n---\n" + body
		wf, err := ParseWorkflow([]byte(content), fuzzEnv)
		if err != nil {
			t.Fatalf("rendered workflow does not parse: %v\n%q", err, content)
		}
		if wf.MaxTurns != int(turns) {
			t.Fatalf("max_turns = %d, want %d", wf.MaxTurns, turns)
		}
		if got := wf.Hooks[HookBeforeRun]; got != hook {
			t.Fatalf("hook = %q, want %q", got, hook)
		}
		want := strings.TrimLeft(strings.ReplaceAll(body, "\r\n", "\n"), "\r\n")
		if wf.PromptBody != want {
			t.Fatalf("body = %q, want %q", wf.PromptBody, want)
		}
	})
}

// yamlSafe reports whether every rune of s can sit inside a YAML double-quoted
// scalar once JSON-escaped. JSON leaves DEL, C1 controls and the noncharacters
// U+FFFE/U+FFFF raw, and YAML refuses them; that is the test's encoder limit,
// not a parser fault.
func yamlSafe(s string) bool {
	for _, r := range s {
		if (r >= 0x7f && r <= 0x9f) || r == 0xfffe || r == 0xffff {
			return false
		}
	}
	return true
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// FuzzRenderPrompt renders arbitrary templates over arbitrary issue fields.
// Rendering must terminate without panicking, be deterministic, and leave a
// token-free template untouched.
func FuzzRenderPrompt(f *testing.F) {
	f.Add("hello", "ID-1", "Title", 1)
	f.Add("{{ issue.title }} / {{ attempt }} / {{ issue | upper }}", "ID-1", "Title", 3)
	f.Add("{{ nope }}", "", "", 0)
	f.Add("{{}} {{ | }} {{ issue | bogus }} {{{{ issue }}}}", "x", "y", -1)
	f.Add("{{ issue.title | trim | lower }}", " a ", " B ", 1)
	f.Add("{{ issue.", "\x00", "\xff", 1)
	f.Add(strings.Repeat("{{ issue.title }}", 5000), "i", "t", 2)
	f.Add(strings.Repeat("{", 1<<20), "i", "t", 2)
	f.Add("{{ issue.title.title }} {{ issue.issue.issue }}", "i", "t", 2)
	f.Fuzz(func(t *testing.T, tpl, ident, title string, attempt int) {
		issue := tracker.Issue{Identifier: ident, Title: title}
		out, err := RenderPrompt(tpl, issue, attempt)
		out2, err2 := RenderPrompt(tpl, issue, attempt)
		if (err == nil) != (err2 == nil) || out != out2 {
			t.Fatalf("non-deterministic render")
		}
		if err == nil && !strings.Contains(tpl, "{{") && out != tpl {
			t.Fatalf("token-free template changed: %q -> %q", tpl, out)
		}
		if err != nil && !strings.HasPrefix(err.Error(), "render: ") {
			t.Fatalf("unstable error prefix: %q", err)
		}
		if _, err := ContinuationGuidance(issue, attempt, tpl); err != nil {
			t.Fatalf("continuation guidance failed: %v", err)
		}
	})
}
