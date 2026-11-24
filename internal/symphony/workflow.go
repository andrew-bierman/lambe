// Package symphony implements the Symphony-conformant core loop: the
// WORKFLOW.md loader with hot reload, sanitized per-issue workspaces with
// the four lifecycle hooks, strict prompt rendering, the retry queue,
// reconciliation, and the tick orchestration that ties them together. Agent
// execution and completion gates are narrow interfaces implemented by other
// packages; this package never spawns an agent itself.
package symphony

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/andrew-bierman/lambe/internal/tracker"
	"github.com/fsnotify/fsnotify"
	"github.com/knadh/koanf/parsers/yaml"
)

// HookName names one of the four workspace lifecycle hooks.
type HookName string

// The four lifecycle hooks. Semantics per hook are enforced by the
// WorkspaceManager, not here.
const (
	HookAfterCreate  HookName = "after_create"
	HookBeforeRun    HookName = "before_run"
	HookAfterRun     HookName = "after_run"
	HookBeforeRemove HookName = "before_remove"
)

// Defaults from the Symphony specification: hooks time out after 60s and a
// run may take up to 20 turns.
const (
	DefaultMaxTurns            = 20
	DefaultMaxConcurrentAgents = 10
	DefaultHookTimeout         = 60 * time.Second
)

// Revision is the content hash of one WORKFLOW.md revision. It is pinned to
// an attempt at claim time so an in-flight run is judged under the workflow
// it started with, while a reload applies from the next claim.
type Revision string

// Workflow is one parsed WORKFLOW.md: front matter (hooks, turn cap, hook
// timeout, tracker rules) plus the prompt body, identified by its content revision.
type Workflow struct {
	Hooks               map[HookName]string
	PromptBody          string
	MaxTurns            int
	MaxConcurrentAgents int
	HookTimeout         time.Duration
	// RequiredLabels is the spec-mandated tracker.required_labels list.
	// An issue must carry every listed label to be dispatched or continued.
	RequiredLabels []string
	Revision       Revision
	Name           string
	GateSpec       json.RawMessage
}

// EnvLookup resolves an environment variable name to its value, reporting
// whether it was set. It is a parameter so tests can fix the environment.
type EnvLookup func(name string) (string, bool)

// OSEnv is the process-environment EnvLookup.
func OSEnv(name string) (string, bool) {
	v, ok := os.LookupEnv(name)
	return v, ok
}

// hookNames is the closed set of front-matter hook keys.
var hookNames = map[HookName]bool{
	HookAfterCreate:  true,
	HookBeforeRun:    true,
	HookAfterRun:     true,
	HookBeforeRemove: true,
}

// ParseWorkflow parses WORKFLOW.md content: YAML front matter between ---
// delimiters followed by the prompt body. Front-matter string values undergo
// $VAR / ${VAR} / ${VAR:-default} environment resolution; a referenced
// variable that is unset without a default is an error. The returned
// Workflow carries the content hash as its Revision.
func ParseWorkflow(content []byte, env EnvLookup) (*Workflow, error) {
	front, body, err := splitFrontMatter(content)
	if err != nil {
		return nil, err
	}
	raw := map[string]any{}
	if len(front) > 0 {
		parsed, err := yaml.Parser().Unmarshal(front)
		if err != nil {
			return nil, fmt.Errorf("parse WORKFLOW.md front matter: %w", err)
		}
		raw = parsed
	}
	if err := substituteEnv(raw, env); err != nil {
		return nil, err
	}

	wf := &Workflow{
		Hooks:               map[HookName]string{},
		MaxTurns:            DefaultMaxTurns,
		MaxConcurrentAgents: DefaultMaxConcurrentAgents,
		HookTimeout:         DefaultHookTimeout,
		Revision:            Revision(hashContent(content)),
	}

	hooksRaw, ok := raw["hooks"].(map[string]any)
	if ok {
		for key, val := range hooksRaw {
			name := HookName(key)
			if !hookNames[name] {
				return nil, fmt.Errorf("WORKFLOW.md: unknown hook %q", key)
			}
			cmd, ok := val.(string)
			if !ok {
				return nil, fmt.Errorf("WORKFLOW.md: hook %q must be a string", key)
			}
			wf.Hooks[name] = cmd
		}
	}

	if v, ok := raw["max_turns"]; ok {
		n, err := asInt(v)
		if err != nil || n < 1 {
			return nil, fmt.Errorf("WORKFLOW.md: max_turns must be a positive integer, got %v", v)
		}
		wf.MaxTurns = n
	}
	if v, ok := raw["agent"]; ok {
		agent, ok := v.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("WORKFLOW.md: agent must be an object")
		}
		for key, dest := range map[string]*int{"max_turns": &wf.MaxTurns, "max_concurrent_agents": &wf.MaxConcurrentAgents} {
			if v, ok := agent[key]; ok {
				n, err := asInt(v)
				if err != nil || n < 1 {
					return nil, fmt.Errorf("WORKFLOW.md: agent.%s must be a positive integer, got %v", key, v)
				}
				*dest = n
			}
		}
	}

	if v, ok := raw["hook_timeout_ms"]; ok {
		n, err := asInt(v)
		if err != nil || n < 1 {
			return nil, fmt.Errorf("WORKFLOW.md: hook_timeout_ms must be a positive integer, got %v", v)
		}
		if int64(n) > math.MaxInt64/int64(time.Millisecond) {
			return nil, fmt.Errorf("WORKFLOW.md: hook_timeout_ms is too large, got %v", v)
		}
		wf.HookTimeout = time.Duration(n) * time.Millisecond
	}

	if v, ok := raw["tracker"]; ok {
		tracker, ok := v.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("WORKFLOW.md: tracker must be an object")
		}
		if labelsRaw, ok := tracker["required_labels"]; ok {
			labels, ok := labelsRaw.([]any)
			if !ok {
				return nil, fmt.Errorf("WORKFLOW.md: tracker.required_labels must be a list of strings")
			}
			for i, item := range labels {
				s, ok := item.(string)
				if !ok {
					return nil, fmt.Errorf("WORKFLOW.md: tracker.required_labels[%d] must be a string", i)
				}
				s = strings.ToLower(strings.TrimSpace(s))
				if s == "" {
					return nil, fmt.Errorf("WORKFLOW.md: tracker.required_labels[%d] must not be empty", i)
				}
				wf.RequiredLabels = append(wf.RequiredLabels, s)
			}
		}
	}

	wf.PromptBody = strings.TrimLeft(string(body), "\r\n")
	return wf, nil
}

// splitFrontMatter separates the optional leading YAML front matter from
// the body. Front matter is the block between an opening line of --- and a
// closing line of --- or ...
func splitFrontMatter(content []byte) (front, body []byte, err error) {
	text := strings.ReplaceAll(string(content), "\r\n", "\n")
	lines := strings.Split(text, "\n")
	if lines[0] != "---" {
		return nil, content, nil
	}
	for i := 1; i < len(lines); i++ {
		if lines[i] == "---" || lines[i] == "..." {
			front = []byte(strings.Join(lines[1:i], "\n"))
			body = []byte(strings.Join(lines[i+1:], "\n"))
			return front, body, nil
		}
	}
	return nil, nil, fmt.Errorf("WORKFLOW.md: unterminated front matter")
}

// asInt coerces a YAML scalar to an int.
func asInt(v any) (int, error) {
	switch t := v.(type) {
	case int:
		return t, nil
	case int64:
		return int(t), nil
	case float64:
		n := int(t)
		if float64(n) != t {
			return 0, fmt.Errorf("not an integer")
		}
		return n, nil
	case string:
		return strconv.Atoi(t)
	default:
		return 0, fmt.Errorf("not an integer")
	}
}

// envToken matches ${VAR}, ${VAR:-default}, and bare $VAR forms.
var envToken = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(:-([^}]*))?\}|\$([A-Za-z_][A-Za-z0-9_]*)`)

// substituteEnv rewrites environment references in every string leaf of a
// decoded front-matter tree. A missing variable without a default fails.
func substituteEnv(tree map[string]any, env EnvLookup) error {
	if env == nil {
		env = OSEnv
	}
	var walk func(v any) (any, error)
	walk = func(v any) (any, error) {
		switch t := v.(type) {
		case string:
			return expandEnv(t, env)
		case map[string]any:
			for k, item := range t {
				replaced, err := walk(item)
				if err != nil {
					return nil, err
				}
				t[k] = replaced
			}
			return t, nil
		case []any:
			for i, item := range t {
				replaced, err := walk(item)
				if err != nil {
					return nil, err
				}
				t[i] = replaced
			}
			return t, nil
		default:
			return v, nil
		}
	}
	_, err := walk(tree)
	return err
}

// expandEnv performs $VAR resolution over one string.
func expandEnv(s string, env EnvLookup) (string, error) {
	var missing []string
	out := envToken.ReplaceAllStringFunc(s, func(m string) string {
		groups := envToken.FindStringSubmatch(m)
		name, def := groups[1], groups[3]
		if name == "" {
			name = groups[4]
			def = ""
		}
		if val, ok := env(name); ok {
			return val
		}
		if groups[2] != "" {
			return def // ${VAR:-default}
		}
		missing = append(missing, name)
		return m
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("WORKFLOW.md: unset environment variable %q without default", strings.Join(missing, ", "))
	}
	return out, nil
}

// hashContent returns the hex sha256 of the file content, used as the
// workflow revision.
func hashContent(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// templateToken matches {{ ... }} substitution sites, optionally with a
// filter: {{ issue | trim }}.
var templateToken = regexp.MustCompile(`\{\{([^{}]*)\}\}`)

// filters is the closed set of supported template filters.
var filters = map[string]func(string) string{
	"trim":  strings.TrimSpace,
	"upper": strings.ToUpper,
	"lower": strings.ToLower,
}

// render expands a template strictly: every {{ name }} or
// {{ name | filter }} token must reference a known variable and a known
// filter. Dot notation (e.g. {{ issue.identifier }}) traverses nested maps.
// An unknown variable or filter is a hard error — rendering never silently
// produces empty output.
func render(tpl string, vars map[string]any) (string, error) {
	var b strings.Builder
	for {
		loc := templateToken.FindStringSubmatchIndex(tpl)
		if loc == nil {
			b.WriteString(tpl)
			return b.String(), nil
		}
		b.WriteString(tpl[:loc[0]])
		token := strings.TrimSpace(tpl[loc[2]:loc[3]])
		if token == "" {
			return "", fmt.Errorf("render: empty template variable")
		}
		name, filter := token, ""
		if before, after, found := strings.Cut(token, "|"); found {
			name = strings.TrimSpace(before)
			filter = strings.TrimSpace(after)
		}
		val, err := resolveVar(vars, name)
		if err != nil {
			return "", fmt.Errorf("render: %w", err)
		}
		s := fmt.Sprint(val)
		if filter != "" {
			fn, ok := filters[filter]
			if !ok {
				return "", fmt.Errorf("render: unknown template filter %q", filter)
			}
			s = fn(s)
		}
		b.WriteString(s)
		tpl = tpl[loc[1]:]
	}
}

// resolveVar looks up a dotted variable name in a nested map. If the path
// has one part and resolves to a map containing a key equal to that part,
// the key's value is returned — this lets {{ issue }} render the issue title
// while {{ issue.identifier }} traverses normally.
func resolveVar(vars map[string]any, name string) (any, error) {
	parts := strings.Split(name, ".")
	var current any = vars
	for i, part := range parts {
		m, ok := current.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("unknown template variable %q", name)
		}
		current, ok = m[part]
		if !ok {
			return nil, fmt.Errorf("unknown template variable %q", name)
		}
		// Bare variable name that resolved to a map: if the map has a key
		// matching the variable's root name, treat that as the string value
		// for backward compatibility (e.g. {{ issue }} == {{ issue.issue }}).
		if i == len(parts)-1 {
			if inner, ok := current.(map[string]any); ok {
				if self, ok := inner[part]; ok {
					return self, nil
				}
			}
		}
	}
	return current, nil
}

// RenderPrompt renders the WORKFLOW.md body over the spec-mandated issue
// object and attempt count. The issue map exposes identifier, title,
// description, state, labels, blocked_by, url, and priority so templates can
// use {{ issue.identifier }}, {{ issue.title }}, etc. For backward
// compatibility, {{ issue }} still resolves to the issue title.
func RenderPrompt(body string, issue tracker.Issue, attempt int) (string, error) {
	vars := map[string]any{
		"issue":   issueVars(issue),
		"attempt": strconv.Itoa(attempt),
	}
	return render(body, vars)
}

// issueVars builds the template-visible map for one issue. Backward
// compatibility: the bare "issue" key returns the title, matching the
// original single-variable contract.
func issueVars(issue tracker.Issue) map[string]any {
	labels := strings.Join(issue.Labels, ", ")
	blocked := ""
	if len(issue.BlockedBy) > 0 {
		parts := make([]string, 0, len(issue.BlockedBy))
		for _, b := range issue.BlockedBy {
			parts = append(parts, b.Identifier)
		}
		blocked = strings.Join(parts, ", ")
	}
	return map[string]any{
		"issue":       issue.Title,
		"identifier":  issue.Identifier,
		"title":       issue.Title,
		"description": issue.Description,
		"state":       issue.State,
		"status":      issue.Status,
		"labels":      labels,
		"blocked_by":  blocked,
		"url":         issue.URL,
		"priority":    strconv.Itoa(issue.Priority),
	}
}

// continuationTemplate is the guidance sent on the same live thread between
// turns of one attempt.
const continuationTemplate = "Issue \"{{ issue.title }}\" is still active. This is attempt {{ attempt }}.\n{{ gate_output }}\nContinue or hand off."

// ContinuationGuidance renders the between-turns continuation message: the
// issue title, the attempt number, the last gate output when present, and
// the continue-or-hand-off instruction.
func ContinuationGuidance(issue tracker.Issue, attempt int, lastGateOutput string) (string, error) {
	gate := "No gate output yet."
	if lastGateOutput != "" {
		gate = "Last gate output:\n" + lastGateOutput
	}
	return render(continuationTemplate, map[string]any{
		"issue":       issueVars(issue),
		"attempt":     strconv.Itoa(attempt),
		"gate_output": gate,
	})
}

// Loader reads WORKFLOW.md from disk and keeps the last-good parsed copy.
// Reloads apply to the next claim; an invalid reload is logged and the
// last-good workflow keeps being served — the loader never crashes the
// daemon on a bad edit.
type Loader struct {
	path string
	env  EnvLookup
	log  *slog.Logger

	mu sync.RWMutex
	wf *Workflow
}

// NewLoader loads the initial WORKFLOW.md at path. An invalid initial file
// is an error: there is no last-good copy to fall back to.
func NewLoader(path string, env EnvLookup, log *slog.Logger) (*Loader, error) {
	if log == nil {
		log = slog.Default()
	}
	content, err := os.ReadFile(path) // #nosec G304 -- path is operator configuration
	if err != nil {
		return nil, fmt.Errorf("read workflow: %w", err)
	}
	wf, err := ParseWorkflow(content, env)
	if err != nil {
		return nil, err
	}
	return &Loader{path: path, env: env, log: log, wf: wf}, nil
}

// Current returns the last-good workflow. The result is treated as
// immutable by callers.
func (l *Loader) Current() *Workflow {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.wf
}

// Reload re-reads the file. On an invalid file it logs, keeps the last-good
// copy, and returns the parse error so callers can surface it; on success
// the new revision applies from the next claim.
func (l *Loader) Reload() error {
	content, err := os.ReadFile(l.path) // #nosec G304 -- path is operator configuration
	if err != nil {
		l.log.Error("workflow reload failed, keeping last-good", "error", err)
		return err
	}
	wf, err := ParseWorkflow(content, l.env)
	if err != nil {
		l.log.Error("workflow reload rejected, keeping last-good", "error", err)
		return err
	}
	l.mu.Lock()
	l.wf = wf
	l.mu.Unlock()
	l.log.Info("workflow reloaded", "revision", wf.Revision)
	return nil
}

// Watch polls the workflow directory for changes to the configured file and
// reloads (debounced). It blocks until ctx is done or the watcher fails.
func (l *Loader) Watch(ctx context.Context) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("watch workflow: %w", err)
	}
	defer func() { _ = watcher.Close() }()
	dir := filepath.Dir(l.path)
	if err := watcher.Add(dir); err != nil {
		return fmt.Errorf("watch %s: %w", dir, err)
	}
	debounce := time.NewTimer(time.Hour)
	debounce.Stop()
	pending := false
	for {
		select {
		case <-ctx.Done():
			return nil
		case err, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			l.log.Warn("workflow watcher error", "error", err)
		case ev, ok := <-watcher.Events:
			if !ok {
				return nil
			}
			if filepath.Base(ev.Name) != filepath.Base(l.path) || ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename) == 0 {
				continue
			}
			if !pending {
				debounce.Reset(100 * time.Millisecond)
				pending = true
			}
		case <-debounce.C:
			pending = false
			if err := l.Reload(); err != nil {
				continue
			}
		}
	}
}
