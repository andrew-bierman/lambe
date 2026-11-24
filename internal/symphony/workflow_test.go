package symphony

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/andrew-bierman/lambe/internal/tracker"
)

// fixedEnv is an EnvLookup over a fixed map.
func fixedEnv(vars map[string]string) EnvLookup {
	return func(name string) (string, bool) {
		v, ok := vars[name]
		return v, ok
	}
}

func TestParseWorkflow(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		content string
		env     map[string]string
		wantErr string
		check   func(t *testing.T, wf *Workflow)
	}{
		{
			name:    "defaults apply with empty front matter",
			content: "---\n---\nDo the work for {{ issue }}.",
			check: func(t *testing.T, wf *Workflow) {
				if wf.MaxTurns != DefaultMaxTurns {
					t.Errorf("MaxTurns = %d, want %d", wf.MaxTurns, DefaultMaxTurns)
				}
				if wf.HookTimeout != DefaultHookTimeout {
					t.Errorf("HookTimeout = %s, want %s", wf.HookTimeout, DefaultHookTimeout)
				}
			},
		},
		{
			name: "hooks and knobs parse",
			content: `---
hooks:
  after_create: "git init"
  before_run: "echo run"
max_turns: 5
hook_timeout_ms: 1500
---
Body {{ attempt }}.
`,
			check: func(t *testing.T, wf *Workflow) {
				if wf.Hooks[HookAfterCreate] != "git init" || wf.Hooks[HookBeforeRun] != "echo run" {
					t.Errorf("hooks = %v", wf.Hooks)
				}
				if wf.MaxTurns != 5 || wf.HookTimeout != 1500*time.Millisecond {
					t.Errorf("max_turns=%d hook_timeout=%s", wf.MaxTurns, wf.HookTimeout)
				}
			},
		},
		{
			name: "hook_timeout_ms that would overflow is rejected",
			content: `---
hook_timeout_ms: 9223372036854775807
---
body
`,
			wantErr: "hook_timeout_ms is too large",
		},
		{
			name: "env substitution with default",
			content: `---
hooks:
  after_create: "clone ${LAMBE_REPO_URL:-https://example.invalid/repo.git}"
---
body
`,
			env: map[string]string{},
			check: func(t *testing.T, wf *Workflow) {
				want := "clone https://example.invalid/repo.git"
				if wf.Hooks[HookAfterCreate] != want {
					t.Errorf("after_create = %q, want %q", wf.Hooks[HookAfterCreate], want)
				}
			},
		},
		{
			name: "env substitution set variable wins over default",
			content: `---
hooks:
  after_run: "echo ${LAMBE_TOKEN:-none} $LAMBE_USER"
---
body
`,
			env: map[string]string{"LAMBE_TOKEN": "s3cret", "LAMBE_USER": "lambe"},
			check: func(t *testing.T, wf *Workflow) {
				if wf.Hooks[HookAfterRun] != "echo s3cret lambe" {
					t.Errorf("after_run = %q", wf.Hooks[HookAfterRun])
				}
			},
		},
		{
			name: "missing env var without default is an error",
			content: `---
hooks:
  after_run: "echo $LAMBE_MISSING"
---
body
`,
			wantErr: "unset environment variable",
		},
		{
			name: "tracker required_labels parse",
			content: `---
tracker:
  required_labels:
    - "lambe:babysit"
    - "priority:high"
---
body`,
			check: func(t *testing.T, wf *Workflow) {
				if len(wf.RequiredLabels) != 2 || wf.RequiredLabels[0] != "lambe:babysit" || wf.RequiredLabels[1] != "priority:high" {
					t.Errorf("RequiredLabels = %v", wf.RequiredLabels)
				}
			},
		},
		{
			name: "tracker required_labels normalizes case",
			content: `---
tracker:
  required_labels:
    - "  Lambe:Babysit  "
---
body`,
			check: func(t *testing.T, wf *Workflow) {
				if len(wf.RequiredLabels) != 1 || wf.RequiredLabels[0] != "lambe:babysit" {
					t.Errorf("RequiredLabels = %v", wf.RequiredLabels)
				}
			},
		},
		{
			name:    "tracker required_labels must be a list",
			content: "---\ntracker:\n  required_labels: notalist\n---\nbody",
			wantErr: "tracker.required_labels must be a list",
		},
		{
			name:    "tracker required_labels rejects empty string",
			content: "---\ntracker:\n  required_labels:\n    - \"\"\n---\nbody",
			wantErr: "tracker.required_labels[0] must not be empty",
		},
		{
			name:    "tracker must be an object",
			content: "---\ntracker: \"not-an-object\"\n---\nbody",
			wantErr: "tracker must be an object",
		},
		{
			name:    "unterminated front matter is an error",
			content: "---\nhooks: {}\nbody",
			wantErr: "unterminated front matter",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			wf, err := ParseWorkflow([]byte(tt.content), fixedEnv(tt.env))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseWorkflow: %v", err)
			}
			if tt.check != nil {
				tt.check(t, wf)
			}
		})
	}
}

func TestParseWorkflowRevisionTracksContent(t *testing.T) {
	t.Parallel()
	a, err := ParseWorkflow([]byte("---\n---\nbody one"), fixedEnv(nil))
	if err != nil {
		t.Fatal(err)
	}
	b, err := ParseWorkflow([]byte("---\n---\nbody two"), fixedEnv(nil))
	if err != nil {
		t.Fatal(err)
	}
	if a.Revision == "" {
		t.Fatal("revision is empty")
	}
	if a.Revision == b.Revision {
		t.Errorf("different content produced the same revision %s", a.Revision)
	}
}

func TestRenderPromptStrict(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		tpl     string
		want    string
		wantErr string
	}{
		{
			name: "known variables render",
			tpl:  "work {{ issue }} on attempt {{ attempt }}",
			want: "work fix the build on attempt 3",
		},
		{
			name: "filters apply",
			tpl:  "{{ issue | upper }} / {{ attempt }}",
			want: "FIX THE BUILD / 3",
		},
		{
			name:    "unknown variable is a hard error",
			tpl:     "{{ issue }} and {{ secret }}",
			wantErr: `unknown template variable "secret"`,
		},
		{
			name:    "unknown filter is a hard error",
			tpl:     "{{ issue | json }}",
			wantErr: `unknown template filter "json"`,
		},
		{
			name:    "empty variable is a hard error",
			tpl:     "{{ }}",
			wantErr: "empty template variable",
		},
		{
			name: "issue object fields render",
			tpl:  "{{ issue.identifier }}: {{ issue.title }} ({{ issue.priority }})",
			want: "fix-1: fix the build (2)",
		},
		{
			name: "issue object fields support filters",
			tpl:  "{{ issue.title | upper }} — {{ issue.state | lower }}",
			want: "FIX THE BUILD — open",
		},
		{
			name:    "unknown issue subfield is a hard error",
			tpl:     "{{ issue.secret }}",
			wantErr: `unknown template variable "issue.secret"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := RenderPrompt(tt.tpl, tracker.Issue{ID: "fix-1", Identifier: "fix-1", Title: "fix the build", State: "open", Priority: 2}, 3)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("RenderPrompt: %v", err)
			}
			if got != tt.want {
				t.Errorf("render = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestContinuationGuidance(t *testing.T) {
	t.Parallel()
	issue := tracker.Issue{ID: "fix-1", Identifier: "fix-1", Title: "fix the build"}
	got, err := ContinuationGuidance(issue, 2, "tests failed")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"fix the build", "attempt 2", "Last gate output:", "tests failed", "Continue or hand off"} {
		if !strings.Contains(got, want) {
			t.Errorf("guidance %q missing %q", got, want)
		}
	}

	got, err = ContinuationGuidance(issue, 2, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "No gate output yet.") {
		t.Errorf("guidance without gate output = %q", got)
	}
}

// writeWorkflow writes content to path and reports the path.
func writeWorkflow(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoaderReloadKeepsLastGood(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	writeWorkflow(t, path, "---\n---\noriginal body")
	log := slog.New(slog.NewTextHandler(&strings.Builder{}, nil))

	loader, err := NewLoader(path, fixedEnv(nil), log)
	if err != nil {
		t.Fatal(err)
	}
	first := loader.Current()
	if first.PromptBody != "original body" {
		t.Fatalf("initial body = %q", first.PromptBody)
	}

	// An invalid file written mid-run: Reload reports the error, the
	// daemon-side loader keeps serving the last-good copy.
	writeWorkflow(t, path, "---\nhooks:\n  bogus_hook: \"x\"\n---\nbroken")
	if err := loader.Reload(); err == nil {
		t.Fatal("Reload of invalid workflow unexpectedly succeeded")
	}
	if got := loader.Current(); got != first || got.PromptBody != "original body" {
		t.Errorf("last-good workflow replaced: %+v", got)
	}

	// A valid edit applies.
	writeWorkflow(t, path, "---\n---\nupdated body")
	if err := loader.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if got := loader.Current(); got.PromptBody != "updated body" || got.Revision == first.Revision {
		t.Errorf("reload did not apply: %+v", got)
	}
}

func TestLoaderInitialInvalidIsError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	writeWorkflow(t, path, "---\nmax_turns: zero\n---\nbody")
	if _, err := NewLoader(path, fixedEnv(nil), slog.Default()); err == nil {
		t.Fatal("expected error for invalid initial workflow")
	}
}
