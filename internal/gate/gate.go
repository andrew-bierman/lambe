// Package gate implements verified completion: the three-valued gate
// evaluator behind symphony's GateEvaluator seam (green / red / error),
// gate definitions parsed from git-versioned formula TOML, escalation
// counters persisted as tracker metadata, and the hub writeback —
// rejection comments, CAS-guarded close, gate-retry defers, tombstones,
// and needs-human transitions. Every outcome is readable through bd
// (comments, status, metadata); there is no daemon-only channel.
package gate

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// Kind is the typed gate implementation selector. It is parsed from the
// formula's [gate] type string and never travels as a string afterwards.
type Kind int

// The gate kinds.
const (
	KindInvalid Kind = iota
	KindShell        // run a command in the workspace
	KindGhRun        // GitHub Actions run status
	KindGhPr         // GitHub pull-request merge status
)

// ParseKind converts a formula [gate] type string into a Kind.
func ParseKind(s string) (Kind, error) {
	switch s {
	case "shell":
		return KindShell, nil
	case "gh:run":
		return KindGhRun, nil
	case "gh:pr":
		return KindGhPr, nil
	default:
		return KindInvalid, fmt.Errorf("unknown gate type %q (want shell, gh:run, or gh:pr)", s)
	}
}

// String renders the formula TOML spelling of the kind.
func (k Kind) String() string {
	switch k {
	case KindShell:
		return "shell"
	case KindGhRun:
		return "gh:run"
	case KindGhPr:
		return "gh:pr"
	default:
		return "invalid"
	}
}

// Config is one gate definition: the [gate] table of a formula file.
type Config struct {
	// Kind selects the evaluation strategy.
	Kind Kind
	// Command is the shell gate's command. It runs in the workspace with
	// LAMBE_ISSUE_ID set to the current issue ID. A non-zero exit is red.
	Command string
	// Ref is the gh:run gate's immutable ref: a full commit SHA or a
	// numeric workflow run id. Bare branch names are rejected at parse
	// time: a static branch ref greens on whatever the branch points at,
	// not the attempt's PR head. To gate the attempt's PR, set PR and
	// leave Ref empty; the PR head SHA is resolved at evaluation time.
	// The CI status of the PR head is what counts, never local test
	// output.
	Ref string
	// PR is the gh:pr gate's pull-request number, url, or branch.
	PR string
}

// Validate checks that the config carries the parameters its kind needs.
func (c Config) Validate() error {
	switch c.Kind {
	case KindShell:
		if c.Command == "" {
			return fmt.Errorf("gate type shell: command is required")
		}
	case KindGhRun:
		// A configured PR resolves the PR head at evaluation time, so no
		// static ref is needed.
		if c.PR != "" {
			return nil
		}
		if c.Ref == "" {
			return fmt.Errorf("gate type gh:run: ref is required")
		}
		if !isImmutableRunRef(c.Ref) {
			return fmt.Errorf("gate type gh:run: bare branch-name ref %q is not allowed: it greens on whatever the branch points at, not the attempt's PR head; set pr = \"<number-or-branch>\" to gate the PR head, or ref = \"<full-commit-sha>\" or a numeric run id", c.Ref)
		}
	case KindGhPr:
		if c.PR == "" {
			return fmt.Errorf("gate type gh:pr: pr is required")
		}
	default:
		return fmt.Errorf("gate type is required")
	}
	return nil
}

// isImmutableRunRef reports whether ref pins one exact CI target: a full
// commit SHA (40 hex for SHA-1, 64 for SHA-256 repos) or a numeric
// workflow run id. Branch names, short SHAs, and URLs are refused.
func isImmutableRunRef(ref string) bool {
	if ref == "" {
		return false
	}
	if isAllDigits(ref) {
		return true
	}
	if len(ref) == 40 || len(ref) == 64 {
		return isAllHex(ref)
	}
	return false
}

// isAllDigits reports whether s consists only of ASCII digits.
func isAllDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// isAllHex reports whether s consists only of ASCII hex characters.
func isAllHex(s string) bool {
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}

// Spec is one parsed gate plus the content revision of its formula file.
// The revision pins the gate an attempt is judged by: it is snapshotted at
// claim time, so a formula edited mid-attempt does not change the rules
// under a running attempt.
type Spec struct {
	// Name is the formula file stem.
	Name string
	// Path is the formula file's absolute path.
	Path string
	// Revision is the sha256 of the formula file content.
	Revision string
	// Config is the parsed [gate] table.
	Config Config
}

// gateTOML mirrors the gate table of a formula file. Trigger tables are
// owned by symphony and ignored here.
type gateTOML struct {
	Gate struct {
		Type    string `toml:"type"`
		Command string `toml:"command"`
		Ref     string `toml:"ref"`
		RunID   string `toml:"run_id"`
		PR      string `toml:"pr"`
	} `toml:"gate"`
}

// ParseSpecFile reads and validates one formula file. A file without a
// [gate] table is not a gate spec; ParseSpecFile reports it with
// ErrNoGate so directory loaders can skip it.
func ParseSpecFile(path string) (Spec, error) {
	content, err := os.ReadFile(path) // #nosec G304 -- path is operator configuration
	if err != nil {
		return Spec{}, fmt.Errorf("read formula: %w", err)
	}
	var spec gateTOML
	meta, err := toml.Decode(string(content), &spec)
	if err != nil {
		return Spec{}, fmt.Errorf("decode formula %s: %w", path, err)
	}
	if !meta.IsDefined("gate") {
		return Spec{}, fmt.Errorf("%w: %s", ErrNoGate, path)
	}
	if spec.Gate.Type == "" {
		return Spec{}, fmt.Errorf("formula %s: gate type is required", path)
	}
	kind, err := ParseKind(spec.Gate.Type)
	if err != nil {
		return Spec{}, fmt.Errorf("formula %s: %w", path, err)
	}
	cfg := Config{Kind: kind, Command: spec.Gate.Command, PR: spec.Gate.PR}
	cfg.Ref = spec.Gate.Ref
	if cfg.Ref == "" {
		cfg.Ref = spec.Gate.RunID
	}
	if err := cfg.Validate(); err != nil {
		return Spec{}, fmt.Errorf("formula %s: %w", path, err)
	}
	sum := sha256.Sum256(content)
	return Spec{
		Name:     strings.TrimSuffix(filepath.Base(path), ".formula.toml"),
		Path:     path,
		Revision: hex.EncodeToString(sum[:]),
		Config:   cfg,
	}, nil
}

// ErrNoGate reports a formula file that carries no [gate] table.
var ErrNoGate = errors.New("formula has no gate table")

// LoadDir reads every *.formula.toml under dir and returns the ones that
// define a gate, sorted by name. A missing or empty directory yields no
// specs and no error.
func LoadDir(dir string) ([]Spec, error) {
	entries, err := os.ReadDir(dir) // #nosec G304 -- path is operator configuration
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read formulas: %w", err)
	}
	var specs []Spec
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".formula.toml") {
			continue
		}
		spec, err := ParseSpecFile(filepath.Join(dir, e.Name()))
		if err != nil {
			if errors.Is(err, ErrNoGate) {
				continue
			}
			return nil, err
		}
		specs = append(specs, spec)
	}
	sort.Slice(specs, func(i, j int) bool { return specs[i].Name < specs[j].Name })
	return specs, nil
}
