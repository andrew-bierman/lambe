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

// knownKeys is every configuration key Load reads, with its kind. Unknown
// keys in the config file are an error, so a typo cannot silently fall back
// to a default.
var knownKeys = map[string]keyKind{
	"harness":                         kindString,
	"machine":                         kindString,
	"actor":                           kindString,
	"workflow_path":                   kindString,
	"workspace.root":                  kindString,
	"labels":                          kindList,
	"trackers":                        kindList,
	"workflows":                       kindList,
	"acp.agent":                       kindString,
	"acp.command":                     kindList,
	"gate.check_timeout":              kindDuration,
	"stateapi.enable":                 kindBool,
	"stateapi.addr":                   kindString,
	"supervisor.tick_seconds":         kindInt,
	"supervisor.attempt_cap":          kindInt,
	"supervisor.silence_timeout":      kindDuration,
	"supervisor.turn_timeout":         kindDuration,
	"supervisor.heartbeat_interval":   kindDuration,
	"supervisor.heartbeat_ttl_second": kindInt,
	"supervisor.stall_after":          kindDuration,
	"supervisor.probe_window":         kindDuration,
	"supervisor.auto_reclaim":         kindBool,
	"supervisor.reclaim_grace":        kindDuration,
	"supervisor.orphan_claim_ttl":     kindDuration,
	"supervisor.bd_timeout":           kindDuration,
	"supervisor.bd_write_timeout":     kindDuration,
	"supervisor.bd_max_concurrency":   kindInt,
	"supervisor.bd_queue_wait":        kindDuration,
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
