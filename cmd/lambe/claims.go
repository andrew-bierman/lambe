package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/andrew-bierman/lambe/internal/daemon"
	"github.com/andrew-bierman/lambe/internal/tracker"
)

const claimsUsage = "usage: lambe claims list [-config FILE] | lambe claims release [-config FILE] [--force] ISSUE"

// runClaims implements `lambe claims list|release`: the operator's view of
// orphaned claims and the only path that ever releases one.
func runClaims(args []string, out io.Writer) error {
	if len(args) == 0 || (args[0] != "list" && args[0] != "release") {
		return usageError{errors.New(claimsUsage)}
	}
	sub := args[0]
	fs := flag.NewFlagSet("lambe claims "+sub, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	configFile := fs.String("config", "", "path to a lambe config file")
	force := fs.Bool("force", false, "release without checking workspace and agent (release only)")
	if err := fs.Parse(args[1:]); err != nil {
		return usageError{err}
	}
	if (sub == "list" && fs.NArg() != 0) || (sub == "release" && fs.NArg() != 1) || (sub == "list" && *force) {
		return usageError{errors.New(claimsUsage)}
	}
	cfg, _, err := loadConfig(*configFile)
	if err != nil {
		return err
	}
	if err := requireLocalTracker(cfg); err != nil {
		return err
	}
	_, actor := hostIdentity(cfg)
	local, err := tracker.NewLocal("", cfg.Trackers[0].Path, actor)
	if err != nil {
		return fmt.Errorf("open tracker %s: %w", cfg.Trackers[0].Name, err)
	}
	stateDir := daemon.DefaultStateDir()
	ctx := context.Background()
	if sub == "release" {
		if err := daemon.ReleaseOrphanClaim(ctx, local, stateDir, cfg, fs.Arg(0), *force); err != nil {
			return err
		}
		_, err := fmt.Fprintf(out, "released %s\n", fs.Arg(0))
		return err
	}
	claims, err := daemon.ListOrphanClaims(ctx, local, stateDir, cfg)
	if err != nil {
		return err
	}
	if len(claims) == 0 {
		_, err := fmt.Fprintln(out, "no orphaned claims")
		return err
	}
	tw := tabwriter.NewWriter(out, 2, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ISSUE\tAGE\tOWNER\tWORKSPACE\tREASON")
	for _, c := range claims {
		ws := "absent"
		if c.WorkspacePresent {
			ws = "present"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", c.Issue, time.Since(c.Since).Truncate(time.Minute), c.Owner, ws, c.Reason)
	}
	return tw.Flush()
}
