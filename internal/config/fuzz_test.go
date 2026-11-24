package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// FuzzLoad feeds arbitrary YAML, environment entries and a flag through
// Load. It must never panic or report non-deterministic errors, and an
// accepted configuration must pass validation and summarize cleanly.
func FuzzLoad(f *testing.F) {
	type seed struct{ file, env, key, val string }
	for _, s := range []seed{
		{"", "", "", ""},
		{"harness: pi\n", "LAMBE_SUPERVISOR_TICK_SECONDS=7", "", ""},
		{"supervisor:\n  tick_seconds: 5\n  silence_timeout: 30s\n", "", "supervisor.tick_seconds", "9"},
		{"harness: acp\nacp:\n  agent: gemini\n", "", "", ""},
		{"harness: acp\nacp:\n  command: []\n", "", "", ""},
		{"harness: acp\nacp:\n  command: opencode acp\n", "", "", ""},
		{"harness: typo\n", "LAMBE_HARNESS=codex", "", ""},
		{"stateapi:\n  enable: true\n  addr: 0.0.0.0:80\n", "", "", ""},
		{"stateapi:\n  enable: true\n  addr: 127.0.0.1:0\n", "", "", ""},
		{"supervisor:\n  tick_seconds: forever\n", "", "", ""},
		{"supervisor: 5\n", "", "", ""},
		{"supervisor:\n  tick_seconds: 99999999999999999999\n", "", "", ""},
		{"supervisor:\n  silence_timeout: -5h\n", "", "", ""},
		{"unknown_key: 1\n", "LAMBE_=x\nLAMBE=y\nLAMBE__A=b", "..", "x"},
		{"labels: [a, b]\nactor: ' x '\nmachine: \"a/b\"\n", "", "a", "b"},
		{"a: &a [*a]\n", "", "", ""},
		{"\x00\xff: [", "LAMBE_\xff=\x00", "\x00", "\xff"},
		{"{" + strings.Repeat("a: {", 2000), "", "", ""},
		{strings.Repeat("harness: pi\n", 1500), strings.Repeat("LAMBE_X=1\n", 1000), "", ""},
		{"actor: " + strings.Repeat("x", 1<<20) + "\n", "", "", ""},
	} {
		f.Add([]byte(s.file), s.env, s.key, s.val)
	}
	dir := f.TempDir()
	f.Fuzz(func(t *testing.T, file []byte, env, key, val string) {
		path := filepath.Join(dir, "config.yaml")
		if err := os.WriteFile(path, file, 0o600); err != nil {
			t.Fatal(err)
		}
		opts := Options{File: path, Env: strings.Split(env, "\n")}
		if key != "" {
			opts.Flags = map[string]string{key: val}
		}
		cfg, err := Load(opts)
		cfg2, err2 := Load(opts)
		if (err == nil) != (err2 == nil) {
			t.Fatalf("non-deterministic outcome: %v vs %v", err, err2)
		}
		if err != nil {
			if err.Error() != err2.Error() {
				t.Fatalf("unstable error: %q vs %q", err, err2)
			}
			return
		}
		if errs := validate(cfg); len(errs) > 0 {
			t.Fatalf("accepted config fails validation: %v", errs)
		}
		if cfg.Summary() != cfg2.Summary() {
			t.Fatal("non-deterministic summary")
		}
	})
}
