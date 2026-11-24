package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	rexcontrib "github.com/andrew-bierman/lambe/contrib/rex"
)

// rexManagedVersion is the managed-file format version written into the
// header. Bump it when the header layout changes.
const rexManagedVersion = 1

const rexManagedPrefix = "-- lambe-managed: "

// rexFileState classifies lambe.lua as found in the Rex config directory.
type rexFileState int

const (
	rexMissing  rexFileState = iota // no file
	rexManaged                      // header present, body matches its recorded hash
	rexModified                     // header present, body edited since install
	rexVerbatim                     // a hand copy byte-identical to the shipped script
	rexForeign                      // someone else's file
)

// rexManagedContent returns a script with its managed header: the header
// format version, the plugin version, and the SHA-256 of everything after the
// header line.
func rexManagedContent(body []byte, plugin int) []byte {
	sum := sha256.Sum256(body)
	header := fmt.Sprintf("%sv%d plugin=%d sha256=%s\n", rexManagedPrefix, rexManagedVersion, plugin, hex.EncodeToString(sum[:]))
	return append([]byte(header), body...)
}

// classifyRexFile reports whether existing may be replaced without asking.
func classifyRexFile(existing []byte, found bool, shipped []byte) rexFileState {
	if !found {
		return rexMissing
	}
	if bytes.Equal(existing, shipped) {
		return rexVerbatim
	}
	line, body, _ := bytes.Cut(existing, []byte("\n"))
	rest, ok := strings.CutPrefix(string(line), rexManagedPrefix)
	if !ok {
		return rexForeign
	}
	var recorded string
	for _, field := range strings.Fields(rest) {
		if v, ok := strings.CutPrefix(field, "sha256="); ok {
			recorded = v
		}
	}
	sum := sha256.Sum256(body)
	if recorded == hex.EncodeToString(sum[:]) {
		return rexManaged
	}
	return rexModified
}

// rexSetupEnv is the slice of the host `setup rex` reads; tests swap it.
type rexSetupEnv struct {
	Getenv func(string) string
	Home   string
	// Executable is the absolute path of this lambe binary, baked into the
	// generated lambe.config.lua so Rex never depends on a login-shell PATH.
	Executable string
	// RunRex runs the rex CLI (config check, keymap); nil when it is not
	// installed, in which case init.lua is written without verification.
	RunRex rexRunner
}

func hostRexSetupEnv() rexSetupEnv {
	home, _ := os.UserHomeDir()
	return rexSetupEnv{Getenv: os.Getenv, Home: home, Executable: lambeExecutable(), RunRex: hostRexRunner()}
}

// lambeExecutable prefers the `lambe` on PATH when it is this very binary
// (a stable path that survives upgrades), else the running executable.
func lambeExecutable() string {
	self, err := os.Executable()
	if err != nil {
		return ""
	}
	if onPath, err := exec.LookPath("lambe"); err == nil {
		a, errA := os.Stat(onPath)
		b, errB := os.Stat(self)
		if errA == nil && errB == nil && os.SameFile(a, b) {
			if abs, err := filepath.Abs(onPath); err == nil {
				return abs
			}
		}
	}
	return self
}

// rexConfigDir resolves Rex's config directory the way `rex config` documents
// it: the directory of $REX_CONFIG, else $XDG_CONFIG_HOME/rex, else
// ~/.config/rex.
func (e rexSetupEnv) rexConfigDir() string {
	if file := e.Getenv("REX_CONFIG"); file != "" {
		return filepath.Dir(file)
	}
	if xdg := e.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "rex")
	}
	return filepath.Join(e.Home, ".config", "rex")
}

// runSetup dispatches `lambe setup TARGET`; only rex exists.
func runSetup(args []string, env rexSetupEnv, out io.Writer) error {
	if len(args) == 0 || args[0] != "rex" {
		return usageError{errors.New("usage: lambe setup rex [--apply] [--uninstall] [--force] [--no-init] [--keys] [--json] [--dir DIR]")}
	}
	return runSetupRex(args[1:], env, out)
}

// runSetupRex installs, updates, or removes the managed Lua files. The
// default is a dry run; init.lua is only ever read.
func runSetupRex(args []string, env rexSetupEnv, out io.Writer) error {
	var report strings.Builder // writes to a Builder cannot fail
	err := setupRex(args, env, &report)
	if _, werr := io.WriteString(out, report.String()); werr != nil && err == nil {
		err = werr
	}
	return err
}

const (
	rexShimName   = "lambe.lua"
	rexModuleDir  = "lambe"
	rexConfigName = "lambe.config.lua"
)

// rexItem is one managed file: where it goes, what it should hold, what is
// there now, and what setup will do about it.
type rexItem struct {
	path     string
	body     []byte // shipped body; nil for a stale file that will be removed
	existing []byte
	state    rexFileState
	verb     string
}

// luaString quotes s as a Lua 5.1 string literal.
func luaString(s string) string { return strconv.Quote(s) }

// rexShimBody is the shim with the Rex config directory filled in.
func rexShimBody(dir string) ([]byte, error) {
	shim, err := rexcontrib.Shim()
	if err != nil {
		return nil, err
	}
	if !bytes.Contains(shim, []byte(rexcontrib.ShimDirPlaceholder)) {
		return nil, errors.New("embedded lambe.lua has no directory placeholder")
	}
	return bytes.Replace(shim, []byte(rexcontrib.ShimDirPlaceholder), []byte(luaString(dir)), 1), nil
}

// rexConfigBody is the generated lambe.config.lua. It belongs to the user
// from then on: setup creates it once and never overwrites it.
func rexConfigBody(executable string) []byte {
	bin := luaString(executable)
	if executable == "" {
		bin = `""`
	}
	return []byte(fmt.Sprintf(`-- Lambe settings for Rex. Yours to edit: `+"`lambe setup rex`"+` creates this file
-- once and never overwrites it. Empty strings mean Lambe's defaults.
-- Environment variables LAMBE_REX_BIN, LAMBE_REX_CONFIG, and
-- LAMBE_REX_STATE_DIR in the Rex server's environment take precedence.
return {
  -- The lambe binary. Absolute, so Rex's minimal PATH does not matter.
  bin = %s,
  -- Path passed as `+"`-config`"+` to every live invocation.
  config = "",
  -- LAMBE_STATE_DIR of the daemon whose runs the actions read.
  state_dir = "",
  -- Where run output opens: "tab" (a window of its own) or "split" (beside the
  -- pane you ran the action from). The placement= argument overrides it.
  placement = "tab",
}
`, bin))
}

// rexReport is the machine-readable result of `lambe setup rex --json`.
type rexReport struct {
	Mode    string          `json:"mode"` // dry-run or apply
	Dir     string          `json:"dir"`
	Plugin  int             `json:"plugin"`
	Files   []rexReportFile `json:"files"`
	Init    *rexReportFile  `json:"init,omitempty"`
	Check   *rexCheckResult `json:"check,omitempty"`
	Keys    *rexReportKeys  `json:"keys,omitempty"`
	Next    []string        `json:"next,omitempty"`
	Error   string          `json:"error,omitempty"`
	Applied bool            `json:"applied"`
}

type rexReportFile struct {
	Path   string `json:"path"`
	Action string `json:"action"`
	Note   string `json:"note,omitempty"`
}

type rexReportKeys struct {
	Added   []string `json:"added"`
	Skipped []string `json:"skipped"`
}

func setupRex(args []string, env rexSetupEnv, out *strings.Builder) (err error) {
	fset := flag.NewFlagSet("setup rex", flag.ContinueOnError)
	fset.SetOutput(io.Discard)
	apply := fset.Bool("apply", false, "write the changes (default: dry run)")
	uninstall := fset.Bool("uninstall", false, "remove the managed Lua files and the init.lua block")
	force := fset.Bool("force", false, "replace or remove files you modified")
	noInit := fset.Bool("no-init", false, "do not add the managed block to init.lua")
	keys := fset.Bool("keys", false, "also bind suggested keys (checked for conflicts with rex keymap)")
	asJSON := fset.Bool("json", false, "print a machine-readable report")
	dir := fset.String("dir", "", "Rex config directory (default: Rex's own)")
	if err := fset.Parse(args); err != nil {
		return usageError{err}
	}
	if fset.NArg() > 0 {
		return usageError{fmt.Errorf("unexpected argument %q", fset.Arg(0))}
	}
	if *dir == "" {
		*dir = env.rexConfigDir()
	}
	plugin, err := rexcontrib.Version()
	if err != nil {
		return err
	}
	rep := &rexReport{Mode: "dry-run", Dir: *dir, Plugin: plugin, Files: []rexReportFile{}}
	if *apply {
		rep.Mode = "apply"
	}
	defer func() {
		if !*asJSON {
			return
		}
		if err != nil {
			rep.Error = err.Error()
		}
		data, merr := json.MarshalIndent(rep, "", "  ")
		if merr != nil {
			return
		}
		out.Reset()
		out.Write(data)
		out.WriteByte('\n')
	}()
	items, err := planRexItems(*dir)
	if err != nil {
		return err
	}

	mode := "dry run; pass --apply to write"
	if *apply {
		mode = "applying"
	}
	fmt.Fprintf(out, "lambe setup rex (%s)\nRex config directory: %s\n", mode, *dir)

	if *uninstall {
		return rexUninstall(out, rep, *dir, items, *apply, *force)
	}

	blocked := false
	for i := range items {
		it := &items[i]
		switch {
		case it.body == nil: // stale managed file from an older version
			it.verb = "remove"
		case it.state == rexMissing:
			it.verb = "create"
		case (it.state == rexModified || it.state == rexForeign) && !*force:
			it.verb = "refuse"
			blocked = true
		case it.state == rexModified || it.state == rexForeign:
			it.verb = "replace"
		case bytes.Equal(it.existing, rexManagedContent(it.body, plugin)):
			it.verb = "keep"
		default:
			it.verb = "update"
		}
		rep.Files = append(rep.Files, rexReportFile{Path: it.path, Action: it.verb})
		switch it.verb {
		case "refuse":
			fmt.Fprintf(out, "  refuse  %s: not managed by lambe or edited since install; review it, or pass --force to replace it\n", it.path)
		case "remove":
			fmt.Fprintf(out, "  remove  %s (no longer shipped)\n", it.path)
		default:
			fmt.Fprintf(out, "  %-7s %s (managed, v%d)\n", it.verb, it.path, plugin)
		}
	}
	if blocked {
		fmt.Fprintln(out, "Settings belong in lambe.config.lua, not in these files; nothing was written.")
		return errors.New("refusing to overwrite files in " + *dir)
	}

	configPath := filepath.Join(*dir, rexConfigName)
	_, statErr := os.Stat(configPath)
	haveConfig := statErr == nil
	if haveConfig {
		fmt.Fprintf(out, "  keep    %s (your settings)\n", configPath)
		rep.Files = append(rep.Files, rexReportFile{Path: configPath, Action: "keep", Note: "your settings"})
	} else {
		fmt.Fprintf(out, "  create  %s (your settings; bin = %s)\n", configPath, orDefault(env.Executable, "lambe"))
		rep.Files = append(rep.Files, rexReportFile{Path: configPath, Action: "create", Note: "your settings"})
	}

	// Plan everything that can fail before writing anything.
	var binds []string // nil keeps the keys already in the block
	if *keys {
		claims, kerr := rexKeymapClaims(env.RunRex)
		if kerr != nil {
			return kerr
		}
		var skipped []string
		binds, skipped = rexPlanKeys(claims)
		if binds == nil {
			binds = []string{}
		}
		rep.Keys = &rexReportKeys{Added: []string{}, Skipped: skipped}
		rep.Keys.Added = append(rep.Keys.Added, binds...)
		for _, k := range skipped {
			fmt.Fprintf(out, "  skip    key %s\n", k)
		}
	}
	var initPlan *rexInitPlan
	if !*noInit {
		pl, perr := planRexInit(*dir, binds)
		if perr != nil {
			return perr
		}
		initPlan = &pl
		rep.Init = &rexReportFile{Path: pl.path, Action: pl.verb, Note: pl.note}
		if pl.note != "" {
			fmt.Fprintf(out, "  %-7s %s: %s\n", pl.verb, pl.path, pl.note)
		} else {
			fmt.Fprintf(out, "  %-7s %s (managed block between the lambe-rex markers)\n", pl.verb, pl.path)
		}
	}

	if *apply {
		if err := writeRexItems(*dir, configPath, items, plugin, !haveConfig, env.Executable); err != nil {
			return err
		}
		rep.Applied = true
		if initPlan != nil && initPlan.verb != "keep" && initPlan.verb != "skip" {
			res, cerr := commitRexInit(*initPlan, env.RunRex)
			rep.Check = &res
			if cerr != nil {
				if res.Output != "" {
					fmt.Fprintf(out, "rex config check said:\n%s\n", res.Output)
				}
				return cerr
			}
			if !res.Ran {
				fmt.Fprintln(out, "  note    the rex CLI was not found; init.lua was written without `rex config check`")
			} else {
				fmt.Fprintln(out, "  checked init.lua with `rex config check`: ok")
			}
		}
	}
	printRexNextSteps(out, rep, *dir, initPlan != nil && initPlan.verb != "skip")
	return nil
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// planRexItems reads the current state of every managed file: the shim, each
// shipped module, and any managed module an older version shipped that this
// one no longer does. Files lambe did not write are never listed as stale.
func planRexItems(dir string) ([]rexItem, error) {
	shim, err := rexShimBody(dir)
	if err != nil {
		return nil, err
	}
	modules, err := rexcontrib.Modules()
	if err != nil {
		return nil, err
	}
	var items []rexItem
	for _, name := range slices.Sorted(maps.Keys(modules)) {
		items = append(items, rexItem{path: filepath.Join(dir, rexModuleDir, name), body: modules[name]})
	}
	items = append(items, rexItem{path: filepath.Join(dir, rexShimName), body: shim}) // last: it loads the rest
	for i := range items {
		data, err := os.ReadFile(items[i].path) // #nosec G304 -- operator-chosen config dir
		found := err == nil
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("read %s: %w", items[i].path, err)
		}
		items[i].existing = data
		items[i].state = classifyRexFile(data, found, items[i].body)
	}
	entries, err := os.ReadDir(filepath.Join(dir, rexModuleDir))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("read %s: %w", filepath.Join(dir, rexModuleDir), err)
	}
	for _, e := range entries {
		if _, shipped := modules[e.Name()]; shipped || e.IsDir() {
			continue
		}
		path := filepath.Join(dir, rexModuleDir, e.Name())
		data, err := os.ReadFile(path) // #nosec G304 -- operator-chosen config dir
		if err != nil {
			continue
		}
		if classifyRexFile(data, true, nil) == rexManaged {
			items = append(items, rexItem{path: path, existing: data, state: rexManaged})
		}
	}
	return items, nil
}

// writeRexItems writes modules before the shim, so the shim never points at
// modules that are not there yet, and removes stale managed modules last.
func writeRexItems(dir, configPath string, items []rexItem, plugin int, writeConfig bool, executable string) error {
	if err := os.MkdirAll(filepath.Join(dir, rexModuleDir), 0o750); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	if writeConfig {
		if err := writeFileAtomic(configPath, rexConfigBody(executable)); err != nil {
			return err
		}
	}
	for _, it := range items {
		switch it.verb {
		case "create", "update", "replace":
			if err := writeFileAtomic(it.path, rexManagedContent(it.body, plugin)); err != nil {
				return err
			}
		case "remove":
			if err := os.Remove(it.path); err != nil {
				return fmt.Errorf("remove %s: %w", it.path, err)
			}
		}
	}
	return nil
}

func rexUninstall(out *strings.Builder, rep *rexReport, dir string, items []rexItem, apply, force bool) error {
	var todo []rexItem
	blocked := false
	for _, it := range items {
		switch {
		case it.state == rexMissing:
		case (it.state == rexModified || it.state == rexForeign) && !force:
			fmt.Fprintf(out, "  refuse  %s: not managed by lambe or edited since install; pass --force to remove it\n", it.path)
			rep.Files = append(rep.Files, rexReportFile{Path: it.path, Action: "refuse"})
			blocked = true
		default:
			todo = append(todo, it)
		}
	}
	if blocked {
		return errors.New("refusing to remove modified files in " + dir)
	}
	removedBlock, initPath, err := removeRexInitBlock(dir, false)
	if err != nil {
		return err
	}
	if len(todo) == 0 && !removedBlock {
		fmt.Fprintf(out, "  skip    %s: not installed\n", dir)
		return nil
	}
	for _, it := range todo {
		fmt.Fprintf(out, "  remove  %s\n", it.path)
		rep.Files = append(rep.Files, rexReportFile{Path: it.path, Action: "remove"})
		if apply {
			if err := os.Remove(it.path); err != nil {
				return fmt.Errorf("remove %s: %w", it.path, err)
			}
		}
	}
	if removedBlock {
		fmt.Fprintf(out, "  edit    %s (remove the managed lambe-rex block)\n", initPath)
		rep.Init = &rexReportFile{Path: initPath, Action: "remove-block"}
		if apply {
			if _, _, err := removeRexInitBlock(dir, true); err != nil {
				return err
			}
		}
	}
	if apply {
		_ = os.Remove(filepath.Join(dir, rexModuleDir)) // only if now empty
		rep.Applied = true
	}
	fmt.Fprintf(out, "Kept %s (your settings).\n", filepath.Join(dir, rexConfigName))
	if !removedBlock {
		fmt.Fprintf(out, "If %s still loads lambe.lua, remove that line yourself.\n", initPath)
	}
	fmt.Fprintln(out, "Then run: rex config reload")
	rep.Next = []string{"rex config reload"}
	return nil
}

// printRexNextSteps prints what is left for you. managed says init.lua carries
// lambe's block, so no dofile line has to be added by hand.
func printRexNextSteps(out *strings.Builder, rep *rexReport, dir string, managed bool) {
	initLua := filepath.Join(dir, "init.lua")
	if !managed {
		loaded := false
		if data, err := os.ReadFile(initLua); err == nil { // #nosec G304 -- operator-chosen config dir
			loaded = bytes.Contains(data, []byte("lambe.lua"))
		}
		if loaded {
			fmt.Fprintf(out, "%s already loads lambe.lua; it is not touched.\n", initLua)
		} else {
			fmt.Fprintf(out, "Add this to %s:\n\n  dofile(%s)\n\n", initLua, strconv.Quote(filepath.Join(dir, rexShimName)))
		}
	}
	rep.Next = []string{"rex config reload", "rex do lambe_check"}
	fmt.Fprintln(out, "Then run: rex config reload   (on the machine that runs the Rex server)")
	fmt.Fprintln(out, "Verify with: rex do lambe_check")
}

// writeFileAtomic writes data to path via a temp file in the same directory.
func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".lambe.lua-*")
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	defer os.Remove(tmp.Name()) //nolint:errcheck // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close() //nolint:errcheck,gosec // already failing
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil { // #nosec G302 -- a Lua config file, world-readable like its siblings
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
