package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Markers around the block `lambe setup rex` owns inside init.lua. Everything
// between them is regenerated; nothing outside them is ever touched.
const (
	rexInitBegin = "-- lambe-rex: begin (managed by `lambe setup rex`; settings live in lambe.config.lua)"
	rexInitEnd   = "-- lambe-rex: end"
)

// rexKeyPrefix is the chord every suggested binding starts with; one prefix
// keeps the footprint in programs that want their own keys to one key.
const rexKeyPrefix = "cmd+shift+l"

// rexSuggestedKeys maps the key after the prefix to an action.
var rexSuggestedKeys = []struct{ key, action string }{
	{"o", "lambe_open_run"},
	{"n", "lambe_needs_you"},
	{"d", "lambe_dashboard"},
	{"w", "lambe_open_workflow_run"},
}

// rexRunner runs the rex CLI and returns its stdout. A non-zero exit returns
// the output and an error. Tests replace it; nil means rex is not installed.
type rexRunner func(args ...string) ([]byte, error)

func hostRexRunner() rexRunner {
	path, err := exec.LookPath("rex")
	if err != nil {
		return nil
	}
	return func(args ...string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, path, args...) // #nosec G204 -- fixed rex subcommands
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			return append(out, stderr.Bytes()...), err
		}
		return out, nil
	}
}

// rexBlockSpan finds the managed block in data: the byte range from the start
// of the begin line through the end of the end line (including its newline).
func rexBlockSpan(data []byte) (start, end int, ok bool) {
	i := bytes.Index(data, []byte("-- lambe-rex: begin"))
	if i < 0 {
		return 0, 0, false
	}
	if nl := bytes.LastIndexByte(data[:i], '\n'); nl >= 0 {
		i = nl + 1
	} else {
		i = 0
	}
	j := bytes.Index(data[i:], []byte(rexInitEnd))
	if j < 0 {
		return 0, 0, false
	}
	end = i + j + len(rexInitEnd)
	if nl := bytes.IndexByte(data[end:], '\n'); nl >= 0 {
		end += nl + 1
	} else {
		end = len(data)
	}
	return i, end, true
}

// rexInitBlock renders the managed block: the dofile line and bind lines.
func rexInitBlock(dir string, binds []string) string {
	var b strings.Builder
	b.WriteString(rexInitBegin + "\n")
	fmt.Fprintf(&b, "dofile(%s)\n", strconv.Quote(filepath.Join(dir, rexShimName)))
	for _, line := range binds {
		b.WriteString(line + "\n")
	}
	b.WriteString(rexInitEnd + "\n")
	return b.String()
}

// rexBindLine is the rex.bind line for one suggested key.
func rexBindLine(key, action string) string {
	return fmt.Sprintf("rex.bind(%s, %s)", strconv.Quote(rexKeyPrefix+">"+key), strconv.Quote(action))
}

// rexExistingBinds returns the rex.bind lines inside an existing managed
// block, so re-running setup without --keys keeps the keys you chose.
func rexExistingBinds(block []byte) []string {
	var binds []string
	for _, line := range strings.Split(string(block), "\n") {
		if strings.HasPrefix(line, "rex.bind(") {
			binds = append(binds, line)
		}
	}
	return binds
}

// rexInitPlan is what setup will do to init.lua.
type rexInitPlan struct {
	path     string
	existing []byte
	found    bool
	next     []byte
	verb     string // create, add, update, keep, skip
	note     string
}

// planRexInit decides how init.lua changes. binds nil keeps the bind lines
// already in the block.
func planRexInit(dir string, binds []string) (rexInitPlan, error) {
	p := rexInitPlan{path: filepath.Join(dir, "init.lua")}
	data, err := os.ReadFile(p.path) // #nosec G304 -- operator-chosen config dir
	switch {
	case err == nil:
		p.existing, p.found = data, true
	case !errors.Is(err, fs.ErrNotExist):
		return p, fmt.Errorf("read %s: %w", p.path, err)
	}
	start, end, hasBlock := rexBlockSpan(p.existing)
	switch {
	case hasBlock:
		if binds == nil {
			binds = rexExistingBinds(p.existing[start:end])
		}
		block := rexInitBlock(dir, binds)
		p.next = slices.Concat(p.existing[:start], []byte(block), p.existing[end:])
		p.verb = "update"
		if bytes.Equal(p.next, p.existing) {
			p.verb = "keep"
		}
	case bytes.Contains(p.existing, []byte("lambe.lua")):
		p.verb, p.note = "skip", "already loads lambe.lua yourself; not touched"
		p.next = p.existing
	default:
		block := rexInitBlock(dir, binds)
		sep := ""
		if len(p.existing) > 0 && !bytes.HasSuffix(p.existing, []byte("\n")) {
			sep = "\n"
		}
		p.next = slices.Concat(p.existing, []byte(sep+block))
		p.verb = "add"
		if !p.found {
			p.verb = "create"
		}
	}
	return p, nil
}

// rexNormalizeKey puts the modifiers of every step of a key (`a>b` for a
// sequence) in sorted order, so `shift+cmd+l` and `cmd+shift+l` compare equal.
func rexNormalizeKey(key string) string {
	steps := strings.Split(strings.ToLower(key), ">")
	for i, step := range steps {
		parts := strings.Split(step, "+")
		if len(parts) > 1 {
			mods := parts[:len(parts)-1]
			slices.Sort(mods)
			step = strings.Join(append(mods, parts[len(parts)-1]), "+")
		}
		steps[i] = step
	}
	return strings.Join(steps, ">")
}

// rexKeymapClaims returns the keys already bound in the Rex config, as
// `a>b` sequences, ignoring bindings to lambe's own actions. It needs the rex
// CLI; `rex keymap` without a client request lists the config file's bindings, not a
// client's built-in shortcuts, so a clash with those is not detected.
func rexKeymapClaims(run rexRunner) (map[string]bool, error) {
	if run == nil {
		return nil, errors.New("the rex CLI is not on PATH, so key conflicts cannot be checked")
	}
	out, err := run("keymap", "--json")
	if err != nil {
		return nil, fmt.Errorf("rex keymap failed: %w", err)
	}
	var km struct {
		Bindings []struct {
			Keys   []string `json:"keys"`
			Action string   `json:"action"`
		} `json:"bindings"`
	}
	if err := json.Unmarshal(out, &km); err != nil {
		return nil, fmt.Errorf("rex keymap printed unexpected output: %w", err)
	}
	claims := map[string]bool{}
	for _, b := range km.Bindings {
		if strings.HasPrefix(b.Action, "lambe_") || len(b.Keys) == 0 {
			continue
		}
		claims[rexNormalizeKey(strings.Join(b.Keys, ">"))] = true
	}
	return claims, nil
}

// rexPlanKeys returns the bind lines for the suggested keys that do not
// conflict, and the keys skipped with the reason.
func rexPlanKeys(claims map[string]bool) (binds, skipped []string) {
	if claims[rexNormalizeKey(rexKeyPrefix)] {
		for _, k := range rexSuggestedKeys {
			skipped = append(skipped, rexKeyPrefix+">"+k.key+" (the prefix "+rexKeyPrefix+" is already bound)")
		}
		return nil, skipped
	}
	for _, k := range rexSuggestedKeys {
		full := rexKeyPrefix + ">" + k.key
		if claims[rexNormalizeKey(full)] {
			skipped = append(skipped, full+" (already bound)")
			continue
		}
		binds = append(binds, rexBindLine(k.key, k.action))
	}
	return binds, skipped
}

// rexCheckResult is the outcome of validating a candidate init.lua.
type rexCheckResult struct {
	Ran    bool   `json:"ran"`
	OK     bool   `json:"ok"`
	Output string `json:"output,omitempty"`
}

// commitRexInit writes the planned init.lua. The candidate is written beside
// it and validated with `rex config check --json PATH` first, so a config rex
// would reject never replaces a working one. Without the rex CLI the file is
// written unverified.
func commitRexInit(p rexInitPlan, run rexRunner) (rexCheckResult, error) {
	if err := os.MkdirAll(filepath.Dir(p.path), 0o750); err != nil {
		return rexCheckResult{}, fmt.Errorf("create %s: %w", filepath.Dir(p.path), err)
	}
	if run == nil {
		return rexCheckResult{}, writeFileAtomic(p.path, p.next)
	}
	candidate := p.path + ".lambe-candidate.lua"
	if err := os.WriteFile(candidate, p.next, 0o600); err != nil {
		return rexCheckResult{}, fmt.Errorf("write %s: %w", candidate, err)
	}
	defer os.Remove(candidate) //nolint:errcheck // no-op once renamed
	out, err := run("config", "check", "--json", candidate)
	res := rexCheckResult{Ran: true, OK: err == nil, Output: strings.TrimSpace(string(out))}
	if err != nil {
		return res, fmt.Errorf("rex config check rejected the new init.lua, which was left unchanged: %w", err)
	}
	if err := os.Chmod(candidate, 0o644); err != nil { // #nosec G302 -- a Lua config file
		return res, fmt.Errorf("write %s: %w", p.path, err)
	}
	if err := os.Rename(candidate, p.path); err != nil {
		return res, fmt.Errorf("write %s: %w", p.path, err)
	}
	return res, nil
}

// removeRexInitBlock deletes the managed block from init.lua, if present.
func removeRexInitBlock(dir string, apply bool) (removed bool, path string, err error) {
	path = filepath.Join(dir, "init.lua")
	data, err := os.ReadFile(path) // #nosec G304 -- operator-chosen config dir
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, path, nil
		}
		return false, path, fmt.Errorf("read %s: %w", path, err)
	}
	start, end, ok := rexBlockSpan(data)
	if !ok {
		return false, path, nil
	}
	if apply {
		if err := writeFileAtomic(path, slices.Concat(data[:start], data[end:])); err != nil {
			return false, path, err
		}
	}
	return true, path, nil
}
