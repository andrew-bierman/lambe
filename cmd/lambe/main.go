// Command lambe is the supervisor daemon for autonomous coding agents.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/andrew-bierman/lambe/internal/config"
	"github.com/andrew-bierman/lambe/internal/logging"
)

// Build information, overridable at link time via -ldflags.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Printf("lambe %s (commit %s, built %s)\n", version, commit, date)
		return
	}

	fs := flag.NewFlagSet("lambe", flag.ExitOnError)
	configFile := fs.String("config", "", "path to a lambe config file")
	tickSeconds := fs.Int("supervisor.tick_seconds", 0, "supervisor tick in seconds (overrides file and env)")
	_ = fs.Parse(os.Args[1:])

	flags := map[string]string{}
	if *tickSeconds != 0 {
		flags["supervisor.tick_seconds"] = fmt.Sprintf("%d", *tickSeconds)
	}

	log := logging.Default()

	cfg, err := config.Load(config.Options{File: *configFile, Flags: flags})
	if err != nil {
		log.Error("configuration invalid", "error", err.Error())
		os.Exit(1)
	}

	log.Info("lambe starting",
		"version", version,
		"config", cfg.Summary(),
	)
}
