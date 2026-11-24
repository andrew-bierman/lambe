package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/andrew-bierman/lambe/internal/daemon"
	"github.com/andrew-bierman/lambe/internal/stateapi"
)

// stateDirInfo is the machine-readable form of `lambe state-dir -json`.
// Descriptors never carry the bearer token (see stateapi.Entry).
type stateDirInfo struct {
	// StateDir is the resolved state directory.
	StateDir string `json:"state_dir"`
	// Source is "env" when LAMBE_STATE_DIR chose it, "default" otherwise.
	Source daemon.StateDirSource `json:"source"`
	// Exists reports whether StateDir is present on disk.
	Exists bool `json:"exists"`
	// LiveDir is where daemons publish state API descriptors.
	LiveDir string `json:"live_dir"`
	// Descriptors are the live descriptors found under LiveDir; empty, not
	// null, when there are none.
	Descriptors []stateapi.Entry `json:"descriptors"`
}

// runStateDir prints the resolved state directory and the live state API
// descriptors under it, so clients need not re-derive the default or guess
// at an override. It is read-only: it creates nothing and never prints a
// token.
func runStateDir(args []string, stateDir string, source daemon.StateDirSource, out io.Writer) error {
	fs := flag.NewFlagSet("lambe state-dir", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print JSON")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if err := noArgs(fs); err != nil {
		return err
	}
	entries, err := stateapi.List(stateDir)
	if err != nil {
		return err
	}
	_, statErr := os.Stat(stateDir)
	info := stateDirInfo{StateDir: stateDir, Source: source, Exists: statErr == nil, LiveDir: stateapi.LiveDir(stateDir), Descriptors: entries}
	if *asJSON {
		enc := json.NewEncoder(out)
		enc.SetEscapeHTML(false)
		return enc.Encode(info)
	}
	origin := "built-in default"
	if source == daemon.StateDirEnv {
		origin = "from LAMBE_STATE_DIR"
	}
	if _, err := fmt.Fprintf(out, "state dir: %s (%s)\n", info.StateDir, origin); err != nil {
		return err
	}
	if !info.Exists {
		if _, err := fmt.Fprintln(out, "  the directory does not exist yet (no daemon has used it)"); err != nil {
			return err
		}
	}
	if len(entries) == 0 {
		_, err := fmt.Fprintf(out, "no state API descriptors under %s\n", info.LiveDir)
		return err
	}
	for _, e := range entries {
		if _, err := fmt.Fprintf(out, "descriptor: %s\n  instance %s  addr %s  pid %d  started %s\n", e.Path, e.Instance, e.Addr, e.PID, e.StartedAt); err != nil {
			return err
		}
	}
	return nil
}
