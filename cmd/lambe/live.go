package main

import (
	"bufio"
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
	"github.com/andrew-bierman/lambe/internal/surface/rex"
)

func terminalText(text string) string {
	return strings.Map(func(r rune) rune {
		if (unicode.IsControl(r) || unicode.In(r, unicode.Cf)) && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, strings.ToValidUTF8(text, "�"))
}

func runLive(args []string, tui bool, in io.Reader, out io.Writer) error {
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
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if tui {
		go func() {
			scanner := bufio.NewScanner(in)
			for scanner.Scan() {
				if strings.TrimSpace(scanner.Text()) == "q" {
					stop()
					return
				}
			}
		}()
	}
	previous := map[string]daemon.LiveRun{}
	render := func() error {
		snapshot, alive, err := daemon.ReadLive(path)
		if err != nil {
			return fmt.Errorf("read live output: %w; start `lambe run` with the same configuration first", err)
		}
		if *asJSON {
			return json.NewEncoder(out).Encode(snapshot)
		}
		if tui {
			if os.Getenv("REX_SESSION") != "" {
				status := rex.StatusIdle
				for _, run := range snapshot.Runs {
					if run.Phase == "running" || run.Phase == "gating" || run.Phase == "preparing" || run.Phase == "claiming" {
						status = rex.StatusWorking
						break
					}
				}
				if err := rex.EmitStatus(out, status, "Lambe supervisor: "+stateDescription(alive)); err != nil {
					return err
				}
			}
			if _, err := fmt.Fprint(out, "\x1b[H\x1b[2J"); err != nil {
				return err
			}
			state := "running"
			if !alive {
				state = "stopped (historical output)"
			}
			if _, err := fmt.Fprintf(out, "Lambe — %s — updated %s\nq + Enter or Ctrl-C closes this viewer; the daemon keeps running\n\n", state, snapshot.UpdatedAt.Format(time.RFC3339)); err != nil {
				return err
			}
		} else if !alive && len(previous) == 0 {
			if _, err := fmt.Fprintln(out, "Daemon stopped; showing historical output."); err != nil {
				return err
			}
		}
		runs := snapshot.Runs
		if tui && *issue == "" {
			var visible []daemon.LiveRun
			for _, run := range runs {
				if !strings.HasPrefix(run.Phase, "stopped:") {
					visible = append(visible, run)
				}
			}
			for i := len(runs) - 1; i >= 0 && len(visible) < 4; i-- {
				if strings.HasPrefix(runs[i].Phase, "stopped:") {
					visible = append(visible, runs[i])
				}
			}
			if len(visible) > 4 {
				visible = visible[:4]
			}
			runs = visible
		}
		for _, run := range runs {
			if *issue != "" && run.IssueID != *issue {
				continue
			}
			text := run.Tail
			if !tui {
				old, seen := previous[run.IssueID]
				if seen && old.TailBytes == run.TailBytes && old.Progress == run.Progress {
					continue
				}
				text = tailDelta(text, run.TailBytes-old.TailBytes)
			}
			previous[run.IssueID] = run
			if tui && *issue == "" {
				lines := strings.Split(strings.TrimSpace(text), "\n")
				if len(lines) > 2 {
					lines = lines[len(lines)-2:]
				}
				text = strings.Join(lines, "\n")
			}
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
		if !*asJSON && (tui || *follow) && errors.Is(err, os.ErrNotExist) {
			if _, writeErr := fmt.Fprintln(out, "Waiting for Lambe output; start lambe run with the same configuration."); writeErr != nil {
				return writeErr
			}
		} else {
			return err
		}
	}
	if *asJSON || (!tui && !*follow) {
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

func stateDescription(alive bool) string {
	if alive {
		return "running"
	}
	return "stopped"
}

func tailDelta(current string, added int64) string {
	if added < 0 || added >= int64(len(current)) {
		return current
	}
	return current[len(current)-int(added):]
}
