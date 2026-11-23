package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
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

// rexManagedContent returns the script with its managed header: a version and
// the SHA-256 of everything after the header line.
func rexManagedContent(body []byte) []byte {
	sum := sha256.Sum256(body)
	header := fmt.Sprintf("%sv%d sha256=%s\n", rexManagedPrefix, rexManagedVersion, hex.EncodeToString(sum[:]))
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
}

func hostRexSetupEnv() rexSetupEnv {
	home, _ := os.UserHomeDir()
	return rexSetupEnv{Getenv: os.Getenv, Home: home}
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
		return usageError{errors.New("usage: lambe setup rex [--apply] [--uninstall] [--force] [--dir DIR]")}
	}
	return runSetupRex(args[1:], env, out)
}

// runSetupRex installs, updates, or removes the managed lambe.lua. The
// default is a dry run; init.lua is only ever read.
func runSetupRex(args []string, env rexSetupEnv, out io.Writer) error {
	var report strings.Builder // writes to a Builder cannot fail
	err := setupRex(args, env, &report)
	if _, werr := io.WriteString(out, report.String()); werr != nil && err == nil {
		err = werr
	}
	return err
}

func setupRex(args []string, env rexSetupEnv, out *strings.Builder) error {
	fset := flag.NewFlagSet("setup rex", flag.ContinueOnError)
	fset.SetOutput(io.Discard)
	apply := fset.Bool("apply", false, "write the changes (default: dry run)")
	uninstall := fset.Bool("uninstall", false, "remove the managed lambe.lua")
	force := fset.Bool("force", false, "replace or remove a lambe.lua you modified")
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
	target := filepath.Join(*dir, "lambe.lua")
	shipped := rexcontrib.LambeLua
	wanted := rexManagedContent(shipped)

	existing, err := os.ReadFile(target) // #nosec G304 -- operator-chosen config dir
	found := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("read %s: %w", target, err)
	}
	state := classifyRexFile(existing, found, shipped)

	mode := "dry run; pass --apply to write"
	if *apply {
		mode = "applying"
	}
	fmt.Fprintf(out, "lambe setup rex (%s)\nRex config directory: %s\n", mode, *dir)

	if *uninstall {
		return rexUninstall(out, target, state, *apply, *force)
	}

	var verb string
	switch {
	case state == rexMissing:
		verb = "create"
	case (state == rexModified || state == rexForeign) && !*force:
		fmt.Fprintf(out, "  refuse  %s: not managed by lambe or edited since install; review it, or pass --force to replace it\n", target)
		return errors.New("refusing to overwrite " + target)
	case state == rexModified || state == rexForeign:
		verb = "replace"
	case bytes.Equal(existing, wanted):
		verb = "keep"
	default:
		verb = "update"
	}
	fmt.Fprintf(out, "  %-7s %s (managed, v%d)\n", verb, target, rexManagedVersion)
	if *apply && verb != "keep" {
		if err := os.MkdirAll(*dir, 0o750); err != nil {
			return fmt.Errorf("create %s: %w", *dir, err)
		}
		if err := writeFileAtomic(target, wanted); err != nil {
			return err
		}
	}
	printRexNextSteps(out, *dir)
	return nil
}

func rexUninstall(out *strings.Builder, target string, state rexFileState, apply, force bool) error {
	switch {
	case state == rexMissing:
		fmt.Fprintf(out, "  skip    %s: not installed\n", target)
		return nil
	case (state == rexModified || state == rexForeign) && !force:
		fmt.Fprintf(out, "  refuse  %s: not managed by lambe or edited since install; pass --force to remove it\n", target)
		return errors.New("refusing to remove " + target)
	}
	fmt.Fprintf(out, "  remove  %s\n", target)
	if apply {
		if err := os.Remove(target); err != nil {
			return fmt.Errorf("remove %s: %w", target, err)
		}
	}
	fmt.Fprintf(out, "Remove the dofile line for it from %s yourself (lambe never edits init.lua), then run: rex config reload\n",
		filepath.Join(filepath.Dir(target), "init.lua"))
	return nil
}

// printRexNextSteps prints what lambe will not do itself: edit init.lua.
func printRexNextSteps(out *strings.Builder, dir string) {
	initLua := filepath.Join(dir, "init.lua")
	loaded := false
	if data, err := os.ReadFile(initLua); err == nil { // #nosec G304 -- operator-chosen config dir
		loaded = bytes.Contains(data, []byte("lambe.lua"))
	}
	if loaded {
		fmt.Fprintf(out, "%s already loads lambe.lua; it is not touched.\n", initLua)
	} else {
		fmt.Fprintf(out, "%s is never edited by lambe. Add this to it:\n\n  dofile(%s)\n  -- optional key binding (pick a key you do not use):\n  -- rex.bind(\"cmd+shift+l\", \"lambe_open_run\")\n\n",
			initLua, strconv.Quote(filepath.Join(dir, "lambe.lua")))
	}
	fmt.Fprintln(out, "Then run: rex config check && rex config reload   (on the machine that runs the Rex server)")
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
