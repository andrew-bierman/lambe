package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/andrew-bierman/lambe/internal/config"
	"github.com/andrew-bierman/lambe/internal/tracker"
)

func runEnqueue(args []string, out io.Writer) error {
	flags := flag.NewFlagSet("lambe enqueue", flag.ContinueOnError)
	cfgPath := flags.String("config", "", configFlagHelp)
	name := flags.String("workflow", "", "named workflow to run")
	apply := flags.Bool("apply", false, "add the workflow label to the task")
	if err := parseFlags(flags, args); err != nil {
		return err
	}
	if flags.NArg() != 1 || *name == "" {
		return usageError{errors.New("usage: lambe enqueue --workflow NAME [--apply] ISSUE_ID")}
	}
	id := flags.Arg(0)
	if id == "" || strings.HasPrefix(id, "-") {
		return usageError{fmt.Errorf("invalid task ID %q", id)}
	}
	path, err := cliConfigPath(*cfgPath)
	if err != nil {
		return err
	}
	if err := requireConfig(path); err != nil {
		return err
	}
	cfg, err := config.Load(config.Options{File: path})
	if err != nil {
		return err
	}
	if err := requireLocalTracker(cfg); err != nil {
		return err
	}
	routeLabels := map[string]bool{}
	var chosen config.NamedWorkflow
	for _, route := range cfg.Workflows {
		routeLabels[route.Label] = true
		if route.Name == *name {
			chosen = route
		}
	}
	if chosen.Name == "" {
		return fmt.Errorf("no configured workflow named %q", *name)
	}
	dir := cfg.Trackers[0].Path
	_, actor := hostIdentity(cfg)
	local, err := tracker.NewLocal("", dir, actor)
	if err != nil {
		return err
	}
	ctx := context.Background()
	issues, err := local.Dispatchable(ctx, tracker.DispatchQuery{IDs: []string{id}, Statuses: []string{tracker.StatusOpen}})
	if err != nil {
		return err
	}
	if len(issues) != 1 || issues[0].Status != tracker.StatusOpen || issues[0].Assignee != "" || len(issues[0].BlockedBy) != 0 {
		return fmt.Errorf("task %s must be open, unassigned, and unblocked", id)
	}
	for _, label := range issues[0].Labels {
		if routeLabels[label] && label != chosen.Label {
			return fmt.Errorf("task %s already has a different workflow label %q", id, label)
		}
	}
	gateDir := cmp.Or(chosen.GateDir, filepath.Join(dir, ".beads", "formulas"))
	if err := requireGateDir(gateDir); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(out, "%s  %s\nWorkflow: %s\nInstructions: %s\nHarness: %s\nGates: %s\n", id, terminalText(issues[0].Title), chosen.Name, chosen.Path, cfg.Harness, gateDir); err != nil {
		return err
	}
	if !*apply {
		_, err := fmt.Fprintln(out, "Preview only. Add --apply to queue the task.")
		return err
	}
	if err := local.AddWorkflowLabel(ctx, id, chosen.Label); err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "Queued %s with %s\n", id, chosen.Label)
	return err
}
