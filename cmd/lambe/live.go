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
	fs := flag.NewFlagSet("lambe logs", flag.ContinueOnError)
	configFile := fs.String("config", "", "config file")
	issue := fs.String("issue", "", "show one issue")
	follow := fs.Bool("follow", false, "follow live output")
	asJSON := fs.Bool("json", false, "print the shared live snapshot as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	if useTUI && (*issue != "" || *follow || *asJSON) {
		return fmt.Errorf("tui does not support -issue, -follow, or -json")
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
		return tui.Run(ctx, path)
	}

	return runLogsFollow(path, *issue, *follow, *asJSON, out)
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
