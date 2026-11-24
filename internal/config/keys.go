package config

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/knadh/koanf/v2"
)

// keyKind is how a configuration key's value must parse.
type keyKind int

const (
	kindString keyKind = iota
	kindInt
	kindBool
	kindDuration
	kindList // YAML list; elements are checked where the key is read
)

// KeyInfo documents one configuration key. The table below is the single
// source for unknown-key validation, type checking, and the generated
// reference docs/reference/config.md.
type KeyInfo struct {
	// Path is the dotted key as written in the config file.
	Path string
	// Type is the human type: string, integer, boolean, duration, or list.
	Type string
	// Default is the documented default; "" means unset.
	Default string
	// Summary is one or two sentences of meaning.
	Summary string
	kind    keyKind
}

// keyTable lists every key Load reads, in documentation order.
var keyTable = []KeyInfo{
	{Path: "harness", Type: "string", Default: "pi", Summary: "Agent backend: `pi`, `codex`, or `acp`.", kind: kindString},
	{Path: "machine", Type: "string", Default: "hostname's first label", Summary: "Dispatch affinity name for this host (`lambe/machine:<name>` labels). Must be distinct per host sharing a tracker.", kind: kindString},
	{Path: "actor", Type: "string", Default: "`lambe@<machine>`", Summary: "Tracker identity stamped on claims and heartbeats. Every host sharing one tracker needs a distinct actor; `lambe doctor` prints the resolved value.", kind: kindString},
	{Path: "workflow_path", Type: "string", Default: "first existing `WORKFLOW.md` candidate", Summary: "Explicit location of the default `WORKFLOW.md`. Relative paths are not resolved against the config file.", kind: kindString},
	{Path: "workspace.root", Type: "string", Default: "under the state directory", Summary: "Directory for per-issue workspaces. A relative path is resolved against the config file's directory.", kind: kindString},
	{Path: "labels", Type: "list", Summary: "Opt-in work lane: claim only issues carrying every listed label. Cannot be combined with `workflows`.", kind: kindList},
	{Path: "trackers", Type: "list", Default: "one repo tracker `local` at `.`", Summary: "Beads sources lambe may use; each item has `name`, `type` (`hub` or `repo`) and `path`. Only the first is supervised.", kind: kindList},
	{Path: "workflows", Type: "list", Summary: "Named workflows; each item has `name`, `label`, `path`, and optional `gate_dir` and `harness`. See Named workflows in REFERENCE.md.", kind: kindList},
	{Path: "acp.agent", Type: "string", Default: "opencode", Summary: "Known ACP agent to run when `harness: acp`.", kind: kindString},
	{Path: "acp.command", Type: "list", Summary: "Explicit argv for the ACP agent; takes precedence over `acp.agent`. A relative executable path is resolved against the config file's directory.", kind: kindList},
	{Path: "gate.check_timeout", Type: "duration", Default: "`silence_timeout / 2`, minimum 1m", Summary: "Bound on each external gate check (a test run, a CI poll), independent of the silence timeout. Raise it for slow suites.", kind: kindDuration},
	{Path: "stateapi.enable", Type: "boolean", Default: "false", Summary: "Serve the bearer-guarded read-only state API on loopback. Off means no listener exists.", kind: kindBool},
	{Path: "stateapi.addr", Type: "string", Default: "127.0.0.1:0", Summary: "Loopback `host:port` to bind; port 0 picks an ephemeral port. Non-loopback addresses are rejected.", kind: kindString},
	{Path: "supervisor.tick_seconds", Type: "integer", Default: "10", Summary: "Supervisor loop cadence in seconds.", kind: kindInt},
	{Path: "supervisor.attempt_cap", Type: "integer", Default: "8", Summary: "Most attempts per work item before it becomes needs-human. Must be at least 1.", kind: kindInt},
	{Path: "supervisor.silence_timeout", Type: "duration", Default: "10m", Summary: "No agent output for this long: the watchdog probes, then kills the run.", kind: kindDuration},
	{Path: "supervisor.turn_timeout", Type: "duration", Default: "1h", Summary: "Hard cap on one agent turn.", kind: kindDuration},
	{Path: "supervisor.heartbeat_interval", Type: "duration", Default: "150s", Summary: "How often the claim lease is extended in the tracker.", kind: kindDuration},
	{Path: "supervisor.heartbeat_ttl_second", Type: "integer", Default: "450", Summary: "Assumed lease TTL in seconds; the heartbeat interval must stay well below it.", kind: kindInt},
	{Path: "supervisor.stall_after", Type: "duration", Default: "5m", Summary: "The single stall threshold: a running run with no activity this long is `stalled` in the state API, TUI and `lambe doctor`. Observation only; the watchdog acts at `silence_timeout`.", kind: kindDuration},
	{Path: "supervisor.probe_window", Type: "duration", Default: "driver default (30s)", Summary: "Pi harness only: how long the watchdog waits for any response after its steer probe before killing.", kind: kindDuration},
	{Path: "supervisor.auto_reclaim", Type: "boolean", Default: "false", Summary: "Run `bd reclaim` every tick, reverting `in_progress` issues whose lease expired more than `reclaim_grace` ago. For shared Dolt-server deployments only; see MULTI-MACHINE.md.", kind: kindBool},
	{Path: "supervisor.reclaim_grace", Type: "duration", Default: "10m", Summary: "Grace period before an expired lease is reclaimed. Only used with `auto_reclaim`; must exceed the tick interval.", kind: kindDuration},
	{Path: "supervisor.orphan_claim_ttl", Type: "duration", Default: "1h", Summary: "How long a recovered claim with no continuation state may stay blocked before the daemon stops heartbeating it so its lease can expire. `0` heartbeats forever. The claim is never released automatically.", kind: kindDuration},
	{Path: "supervisor.bd_timeout", Type: "duration", Default: "60s", Summary: "Bound on one `bd` read or heartbeat run; the clock starts when the process starts, not while it queues. Raise it on a slow embedded hub.", kind: kindDuration},
	{Path: "supervisor.bd_write_timeout", Type: "duration", Default: "0s (= `bd_timeout`)", Summary: "Bound on one `bd` write (claim, attempt record, close).", kind: kindDuration},
	{Path: "supervisor.bd_max_concurrency", Type: "integer", Default: "3", Summary: "Most `bd` subprocesses running at once. Writes jump the queue ahead of reads, and reads ahead of heartbeats.", kind: kindInt},
	{Path: "supervisor.bd_queue_wait", Type: "duration", Default: "10m", Summary: "How long a `bd` call may wait for a free slot before it fails.", kind: kindDuration},
}

// Keys returns the documented configuration keys in documentation order.
func Keys() []KeyInfo { return append([]KeyInfo(nil), keyTable...) }

// knownKeys maps every key to its kind, derived from keyTable.
var knownKeys = func() map[string]keyKind {
	m := make(map[string]keyKind, len(keyTable))
	for _, k := range keyTable {
		m[k.Path] = k.kind
	}
	return m
}()

// envName is the environment variable that overrides path, or "" when the
// key can only be set in the file (list keys, and top-level keys with an
// underscore, which the flattening rule would split into a section).
func envName(k KeyInfo) string {
	if k.kind == kindList || (!strings.Contains(k.Path, ".") && strings.Contains(k.Path, "_")) {
		return ""
	}
	return EnvPrefix + strings.ToUpper(strings.ReplaceAll(k.Path, ".", "_"))
}

// ReferenceMarkdown renders docs/reference/config.md. A test fails when the
// checked-in file differs, so the docs cannot drift from the code; refresh
// with `go run ./internal/config/gen docs/reference/config.md`.
func ReferenceMarkdown() string {
	var b strings.Builder
	b.WriteString("<!-- Generated by `go run ./internal/config/gen docs/reference/config.md`; do not edit. -->\n\n")
	b.WriteString("# Configuration reference\n\n")
	b.WriteString("The config file is YAML. `lambe init` writes it to the user config directory\n")
	b.WriteString("(`~/Library/Application Support/lambe/config.yaml` on macOS, `$XDG_CONFIG_HOME/lambe/config.yaml`\n")
	b.WriteString("or `~/.config/lambe/config.yaml` on Linux); `--config FILE` or `LAMBE_CONFIG` selects another.\n\n")
	b.WriteString("Precedence, lowest to highest: built-in defaults, the config file, then environment\n")
	b.WriteString("variables. There are no per-key command-line flags. Unknown keys in the file and values\n")
	b.WriteString("of the wrong type are errors, with a suggestion when a key looks misspelled.\n\n")
	b.WriteString("An environment variable is `LAMBE_` plus the key in upper case, with the first `.`\n")
	b.WriteString("replaced by `_` and any further separator also `_` (`supervisor.tick_seconds` is\n")
	b.WriteString("`LAMBE_SUPERVISOR_TICK_SECONDS`). List keys and top-level keys containing an underscore\n")
	b.WriteString("can only be set in the file.\n\n")
	b.WriteString("| Key | Type | Default | Environment | Meaning |\n| --- | --- | --- | --- | --- |\n")
	for _, k := range keyTable {
		def := k.Default
		if def == "" {
			def = "unset"
		}
		env := envName(k)
		if env == "" {
			env = "file only"
		} else {
			env = "`" + env + "`"
		}
		b.WriteString("| `" + k.Path + "` | " + k.Type + " | " + def + " | " + env + " | " + k.Summary + " |\n")
	}
	return b.String()
}

// listItemKeys are the keys allowed inside each mapping of a list key.
var listItemKeys = map[string][]string{
	"trackers":  {"name", "type", "path"},
	"workflows": {"name", "label", "path", "gate_dir", "harness"},
}

// checkFileKeys reports every key in the config file that lambe does not
// read, with a "did you mean" suggestion when one is close.
func checkFileKeys(file string, k *koanf.Koanf) []error {
	var errs []error
	known := make([]string, 0, len(knownKeys))
	for key := range knownKeys {
		known = append(known, key)
	}
	sort.Strings(known)
	for _, key := range k.Keys() {
		if _, ok := knownKeys[key]; !ok {
			errs = append(errs, unknownKeyError(file, key, known))
		}
	}
	for list, allowed := range listItemKeys {
		for i, item := range k.Slices(list) {
			for _, key := range item.Keys() {
				if !contains(allowed, key) {
					errs = append(errs, unknownKeyError(file, fmt.Sprintf("%s[%d].%s", list, i, key), allowed))
				}
			}
		}
	}
	return errs
}

func unknownKeyError(file, key string, candidates []string) error {
	hint := ""
	if s := closest(key[strings.LastIndexByte(key, '.')+1:], candidates); s != "" {
		hint = fmt.Sprintf(" (did you mean %q?)", s)
	}
	return fmt.Errorf("%s: unknown key %q%s", file, key, hint)
}

// closest returns the candidate nearest to key, or "" if none is near. For
// dotted candidates it also compares the last path segment, so a bare
// "silence_timeout" finds "supervisor.silence_timeout".
func closest(key string, candidates []string) string {
	best, bestDist := "", 1<<30
	for _, c := range candidates {
		d := min(levenshtein(key, c), levenshtein(key, c[strings.LastIndexByte(c, '.')+1:]))
		if d < bestDist {
			best, bestDist = c, d
		}
	}
	if bestDist <= max(2, len(key)/4) {
		return best
	}
	return ""
}

func levenshtein(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// checkValueTypes reports every scalar key whose merged value cannot parse
// as its kind, naming the offending value instead of a silent zero.
func checkValueTypes(k *koanf.Koanf) []error {
	var errs []error
	keys := make([]string, 0, len(knownKeys))
	for key := range knownKeys {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		raw := k.Get(key)
		if s, isStr := raw.(string); raw == nil || (isStr && strings.TrimSpace(s) == "") {
			continue
		}
		var want string
		var ok bool
		switch knownKeys[key] {
		case kindInt:
			want, ok = "an integer", isInt(raw)
		case kindBool:
			want, ok = "true or false", isBool(raw)
		case kindDuration:
			want, ok = `a duration like "30s" or "10m"`, isDuration(raw)
		default:
			continue
		}
		if !ok {
			errs = append(errs, fmt.Errorf("%s: expected %s, got %v", key, want, quoteIfString(raw)))
		}
	}
	return errs
}

func quoteIfString(v any) any {
	if s, ok := v.(string); ok {
		return strconv.Quote(s)
	}
	return v
}

func isInt(v any) bool {
	switch x := v.(type) {
	case int, int64:
		return true
	case float64:
		return x == float64(int64(x))
	case string:
		_, err := strconv.Atoi(strings.TrimSpace(x))
		return err == nil
	}
	return false
}

func isBool(v any) bool {
	switch x := v.(type) {
	case bool:
		return true
	case string:
		_, err := strconv.ParseBool(strings.TrimSpace(x))
		return err == nil
	}
	return false
}

func isDuration(v any) bool {
	switch x := v.(type) {
	case int, int64, float64:
		return true // koanf reads a bare number as a count of nanoseconds
	case string:
		_, err := time.ParseDuration(strings.TrimSpace(x))
		return err == nil
	}
	return false
}
