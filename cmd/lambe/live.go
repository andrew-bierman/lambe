package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/andrew-bierman/lambe/internal/config"
	"github.com/andrew-bierman/lambe/internal/daemon"
	"github.com/andrew-bierman/lambe/internal/surface/tui"
	"github.com/andrew-bierman/lambe/internal/tracker"
)

func terminalText(text string) string {
	return strings.Map(func(r rune) rune {
		if (unicode.IsControl(r) || unicode.In(r, unicode.Cf)) && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, strings.ToValidUTF8(text, "�"))
}

func runLive(args []string, useTUI bool, _ io.Reader, out io.Writer) error {
	command := "lambe logs"
	if useTUI {
		command = "lambe tui"
	}
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	configFile := fs.String("config", "", "config file")
	var issue string
	var demo, follow, asJSON bool
	if useTUI {
		fs.BoolVar(&demo, "demo", false, "open the dashboard with sample runs")
	} else {
		fs.StringVar(&issue, "issue", "", "show one issue")
		fs.BoolVar(&follow, "follow", false, "follow live output")
		fs.BoolVar(&asJSON, "json", false, "print the shared live snapshot as JSON")
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	if demo {
		if *configFile != "" {
			return errors.New("-demo requires lambe tui without -config")
		}
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		return tui.RunDemo(ctx)
	}
	configPath, err := cliConfigPath(*configFile)
	if err != nil {
		return err
	}
	if err := requireConfig(configPath); err != nil {
		return err
	}
	cfg, err := config.Load(config.Options{File: configPath})
	if err != nil {
		return err
	}
	path := daemon.LivePath(daemon.DefaultStateDir(), cfg)

	if useTUI {
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		trackerPath := ""
		if len(cfg.Trackers) > 0 {
			trackerPath = cfg.Trackers[0].Path
		}
		var queue func(string) (string, error)
		if trackerPath != "" {
			queue = func(title string) (string, error) {
				local, err := tracker.NewLocal("", trackerPath, "lambe@"+daemon.DefaultMachine())
				if err != nil {
					return "", err
				}
				issue, err := local.CreateTask(ctx, title, cfg.Labels)
				return issue.ID, err
			}
		}
		return tui.Run(ctx, path, trackerPath, queue)
	}

	return runLogsFollow(path, issue, follow, asJSON, out)
}

func runLogsFollow(path, issue string, follow, asJSON bool, out io.Writer) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	previous := map[string]daemon.LiveRun{}
	render := func() error {
		snapshot, alive, err := daemon.ReadLive(path)
		if err != nil {
			return fmt.Errorf("read live output: %w; start `lambe run` with the same configuration first", err)
		}
		if asJSON {
			return json.NewEncoder(out).Encode(snapshot)
		}
		if !alive && len(previous) == 0 {
			if _, err := fmt.Fprintln(out, "Daemon stopped; showing historical output."); err != nil {
				return err
			}
		}
		for _, run := range snapshot.Runs {
			if issue != "" && run.IssueID != issue {
				continue
			}
			text := run.Tail
			old, seen := previous[run.IssueID]
			if seen && old.TailBytes == run.TailBytes && old.Progress == run.Progress {
				continue
			}
			text = tailDelta(text, run.TailBytes-old.TailBytes)
			previous[run.IssueID] = run
			if !run.WakeAt.IsZero() {
				text = "wake at " + run.WakeAt.Format(time.RFC3339) + "\n" + text
			}
			if _, err := fmt.Fprintf(out, "%s  %s  attempt %d · turn %d  %s\n%s\n", terminalText(run.IssueID), terminalText(run.Phase), run.Attempt, run.Turn, terminalText(run.Title), terminalText(text)); err != nil {
				return err
			}
		}
		return nil
	}
	if err := render(); err != nil {
		if !asJSON && follow && errors.Is(err, os.ErrNotExist) {
			if _, writeErr := fmt.Fprintln(out, "Waiting for Lambe output; start lambe run with the same configuration."); writeErr != nil {
				return writeErr
			}
		} else {
			return err
		}
	}
	if asJSON || !follow {
		return nil
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := render(); err != nil {
				if errors.Is(err, os.ErrNotExist) {
					continue
				}
				return err
			}
		}
	}
}

func tailDelta(current string, added int64) string {
	if added < 0 || added >= int64(len(current)) {
		return current
	}
	return current[len(current)-int(added):]
}
