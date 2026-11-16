package daemon

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/andrew-bierman/lambe/internal/config"
)

// launchCommand keeps an npm-installed Codex adapter with its sibling Node runtime.
func launchCommand(cfg *config.Config) ([]string, error) {
	argv, err := HarnessCommand(cfg)
	if err != nil {
		return nil, err
	}
	bin, err := exec.LookPath(argv[0])
	if err != nil {
		return nil, fmt.Errorf("cannot find agent executable %q: %w; install the harness or configure acp.command", argv[0], err)
	}
	if argv[0] != "codex-acp" || len(cfg.ACP.Command) > 0 {
		return argv, nil
	}
	file, err := os.Open(bin) // #nosec G304 -- resolved configured executable
	if err != nil {
		return nil, err
	}
	header, readErr := io.ReadAll(io.LimitReader(file, 128))
	_ = file.Close()
	if readErr != nil {
		return nil, readErr
	}
	if !strings.HasPrefix(string(header), "#!/usr/bin/env node\n") {
		return argv, nil
	}
	node, err := exec.LookPath(filepath.Join(filepath.Dir(bin), "node"))
	if err != nil {
		node, err = exec.LookPath("node")
	}
	if err != nil {
		return nil, fmt.Errorf("codex npm adapter requires Node.js: %w", err)
	}
	return append([]string{node, bin}, argv[1:]...), nil
}

func checkAdapterRuntime(argv []string) error {
	if len(argv) < 2 || filepath.Base(argv[1]) != "codex-acp" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], "-e", `if (typeof AbortSignal.any !== "function") { console.error("AbortSignal.any is missing"); process.exit(1) }`) // #nosec G204 -- runtime resolved from the selected installed adapter
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("codex adapter runtime %s is incompatible: %w (%s); use a current Node.js installation for codex-acp", argv[0], err, strings.TrimSpace(string(out)))
	}
	return nil
}

func runtimeEnv(argv, env []string) []string {
	if len(argv) < 2 || filepath.Base(argv[1]) != "codex-acp" {
		return env
	}
	env = append([]string(nil), env...)
	for i, entry := range env {
		if strings.HasPrefix(entry, "PATH=") {
			env[i] = "PATH=" + filepath.Dir(argv[0]) + string(os.PathListSeparator) + strings.TrimPrefix(entry, "PATH=")
			return env
		}
	}
	return append(env, "PATH="+filepath.Dir(argv[0]))
}
