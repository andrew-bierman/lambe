package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andrew-bierman/lambe/internal/config"
)

func TestCodexUsesTheNodeRuntimeThatInstalledItsAdapter(t *testing.T) {
	old := t.TempDir()
	modern := t.TempDir()
	for path, body := range map[string]string{filepath.Join(old, "node"): "#!/bin/sh\necho old-node >&2\nexit 1\n", filepath.Join(modern, "node"): "#!/bin/sh\nexit 0\n", filepath.Join(modern, "codex-acp"): "#!/usr/bin/env node\nthrow new Error('adapter must not be launched by preflight')\n"} {
		if err := os.WriteFile(path, []byte(body), 0o700); err != nil { // #nosec G306 -- private executable test fixture
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", old+string(os.PathListSeparator)+modern)
	cfg := &config.Config{Harness: "codex"}
	argv, err := launchCommand(cfg)
	if err != nil || len(argv) != 2 || argv[0] != filepath.Join(modern, "node") {
		t.Fatalf("selected old shell runtime: %v %v", argv, err)
	}
	if err := CheckHarness(cfg); err != nil {
		t.Fatalf("compatible sibling runtime rejected: %v", err)
	}
	if err := os.Remove(filepath.Join(modern, "node")); err != nil {
		t.Fatal(err)
	}
	if err := CheckHarness(cfg); err == nil || !strings.Contains(err.Error(), "incompatible") {
		t.Fatalf("incompatible fallback was accepted: %v", err)
	}
}

func TestNativeCodexAdapterDoesNotRequireNode(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "codex-acp"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil { // #nosec G306 -- private executable test fixture
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	if err := CheckHarness(&config.Config{Harness: "codex"}); err != nil {
		t.Fatalf("non-Node adapter forced a Node dependency: %v", err)
	}
}

func TestCodexResolvesMiseShimBeforeSelectingNode(t *testing.T) {
	dir := t.TempDir()
	shim := filepath.Join(dir, "shims")
	modern := filepath.Join(dir, "modern")
	for _, path := range []string{shim, modern} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	manager := filepath.Join(dir, "mise")
	for path, body := range map[string]string{
		manager:                            "#!/bin/sh\nprintf '%s\\n' '" + filepath.Join(modern, "codex-acp") + "'\n",
		filepath.Join(modern, "node"):      "#!/bin/sh\nexit 0\n",
		filepath.Join(modern, "codex-acp"): "#!/usr/bin/env node\n",
	} {
		if err := os.WriteFile(path, []byte(body), 0o700); err != nil { // #nosec G306 -- private executable test fixture
			t.Fatal(err)
		}
	}
	if err := os.Symlink(manager, filepath.Join(shim, "codex-acp")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", shim+string(os.PathListSeparator)+modern)
	argv, err := launchCommand(&config.Config{Harness: "codex"})
	if err != nil || len(argv) != 2 || argv[0] != filepath.Join(modern, "node") || argv[1] != filepath.Join(modern, "codex-acp") {
		t.Fatalf("shim hid the adapter runtime: %v %v", argv, err)
	}
}

func TestInstalledCodexAdapterRuntimePreflight(t *testing.T) {
	if os.Getenv("LAMBE_TEST_CODEX") != "1" {
		t.Skip("set LAMBE_TEST_CODEX=1 to check the installed adapter")
	}
	if err := CheckHarness(&config.Config{Harness: "codex"}); err != nil {
		t.Fatal(err)
	}
	argv, err := launchCommand(&config.Config{Harness: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("resolved installed Codex launch executable: %s", argv[0])
}

func TestExplicitAdapterCommandKeepsItsRuntime(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "codex-acp"), []byte("#!/usr/bin/env node\n"), 0o700); err != nil { // #nosec G306 -- private executable test fixture
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	cfg := &config.Config{Harness: "acp", ACP: config.ACP{Command: []string{"codex-acp", "--custom"}}}
	argv, err := launchCommand(cfg)
	if err != nil || len(argv) != 2 || argv[0] != "codex-acp" || argv[1] != "--custom" {
		t.Fatalf("custom runtime rewritten: %v %v", argv, err)
	}
	if err := CheckHarness(cfg); err != nil {
		t.Fatalf("custom command forced an implicit runtime: %v", err)
	}
}

func TestSelectedRuntimeIsInheritedByAdapterChildren(t *testing.T) {
	original := []string{"PATH=/older/bin", "KEEP=value"}
	env := runtimeEnv([]string{"/modern/bin/node", "/modern/bin/codex-acp"}, original)
	if original[0] != "PATH=/older/bin" || env[0] != "PATH=/modern/bin"+string(os.PathListSeparator)+"/older/bin" || env[1] != "KEEP=value" {
		t.Fatalf("runtime environment leaked or lost values: %v %v", original, env)
	}
}
