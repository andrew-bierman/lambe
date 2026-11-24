package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	rexcontrib "github.com/andrew-bierman/lambe/contrib/rex"
)

func testRexEnv(dir string) rexSetupEnv {
	return rexSetupEnv{Getenv: func(string) string { return "" }, Home: dir, Executable: "/opt/lambe/bin/lambe"}
}

func installRex(t *testing.T, dir string, extra ...string) string {
	t.Helper()
	var out bytes.Buffer
	if err := runSetupRex(append([]string{"--dir", dir, "--apply"}, extra...), testRexEnv(dir), &out); err != nil {
		t.Fatalf("install: %v\n%s", err, out.String())
	}
	return out.String()
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path) // #nosec G304 -- test temp dir
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestSetupRexInstallLayout(t *testing.T) {
	dir := t.TempDir()
	out := installRex(t, dir)
	plugin, err := rexcontrib.Version()
	if err != nil {
		t.Fatal(err)
	}
	modules, err := rexcontrib.Modules()
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range modules {
		got := readFile(t, filepath.Join(dir, "lambe", name))
		if got != string(rexManagedContent(body, plugin)) {
			t.Errorf("module %s differs from the embedded source", name)
		}
	}
	shim := readFile(t, filepath.Join(dir, "lambe.lua"))
	if !strings.Contains(shim, "-- lambe-managed: v1 plugin="+itoa(plugin)+" sha256=") {
		t.Errorf("shim header missing plugin version:\n%s", clip(shim))
	}
	if !strings.Contains(shim, `local DIR = "`+dir+`"`) {
		t.Errorf("shim does not carry the config directory %q", dir)
	}
	cfg := readFile(t, filepath.Join(dir, "lambe.config.lua"))
	if !strings.Contains(cfg, `bin = "/opt/lambe/bin/lambe"`) {
		t.Errorf("config does not bake the binary path:\n%s", cfg)
	}
	if !strings.Contains(out, "create") || !strings.Contains(out, "rex do lambe_check") {
		t.Errorf("unexpected output:\n%s", out)
	}
}

// Editing the settings file must never make an update refuse: that was the
// failure mode when the overrides lived inside the managed script.
func TestSetupRexKeepsUserConfigAcrossUpdates(t *testing.T) {
	dir := t.TempDir()
	installRex(t, dir)
	cfgPath := filepath.Join(dir, "lambe.config.lua")
	mustWrite(t, cfgPath, `return { bin = "/mine/lambe", state_dir = "/s" }`)
	out := installRex(t, dir)
	if got := readFile(t, cfgPath); got != `return { bin = "/mine/lambe", state_dir = "/s" }` {
		t.Errorf("config overwritten: %q", got)
	}
	if !strings.Contains(out, "keep    "+cfgPath) {
		t.Errorf("output does not say the config is kept:\n%s", out)
	}
}

func TestSetupRex(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		prepare   func(t *testing.T, dir string)
		wantErr   bool
		wantUsage bool
		check     func(t *testing.T, dir string)
		wantOut   []string
		denyOut   []string
	}{
		{
			name:    "dry run writes nothing",
			wantOut: []string{"dry run", "create", "dofile(", "rex config reload"},
			check: func(t *testing.T, dir string) {
				if _, err := os.Stat(filepath.Join(dir, "lambe.lua")); err == nil {
					t.Error("dry run wrote lambe.lua")
				}
				if _, err := os.Stat(filepath.Join(dir, "lambe")); err == nil {
					t.Error("dry run created lambe/")
				}
			},
		},
		{
			name: "apply is idempotent", args: []string{"--apply"},
			prepare: func(t *testing.T, dir string) { installRex(t, dir) },
			wantOut: []string{"keep    " + "%DIR%/lambe.lua"}, denyOut: []string{"update", "create"},
		},
		{
			name: "apply upgrades an older managed shim", args: []string{"--apply"},
			prepare: func(t *testing.T, dir string) {
				mustWrite(t, filepath.Join(dir, "lambe.lua"), string(rexManagedContent([]byte("-- old script\n"), 1)))
			},
			wantOut: []string{"update  %DIR%/lambe.lua"},
		},
		{
			name: "edited module is refused and nothing is written", args: []string{"--apply"},
			prepare: func(t *testing.T, dir string) {
				installRex(t, dir)
				p := filepath.Join(dir, "lambe", "rows.lua")
				mustWrite(t, p, readFile(t, p)+"-- mine\n")
			},
			wantErr: true, wantOut: []string{"refuse", "--force"},
		},
		{
			name: "refuses a foreign lambe.lua", args: []string{"--apply"},
			prepare: func(t *testing.T, dir string) { mustWrite(t, filepath.Join(dir, "lambe.lua"), "-- mine\n") },
			wantErr: true, wantOut: []string{"refuse"},
			check: func(t *testing.T, dir string) {
				if got := readFile(t, filepath.Join(dir, "lambe.lua")); got != "-- mine\n" {
					t.Errorf("foreign file overwritten: %q", got)
				}
				if _, err := os.Stat(filepath.Join(dir, "lambe", "init.lua")); err == nil {
					t.Error("modules written despite the refusal")
				}
			},
		},
		{
			name: "force replaces a foreign lambe.lua", args: []string{"--apply", "--force"},
			prepare: func(t *testing.T, dir string) { mustWrite(t, filepath.Join(dir, "lambe.lua"), "-- mine\n") },
			wantOut: []string{"replace"},
			check: func(t *testing.T, dir string) {
				if !strings.HasPrefix(readFile(t, filepath.Join(dir, "lambe.lua")), rexManagedPrefix) {
					t.Error("foreign file not replaced")
				}
			},
		},
		{
			name: "stale managed module is removed, foreign file kept", args: []string{"--apply"},
			prepare: func(t *testing.T, dir string) {
				installRex(t, dir)
				mustWrite(t, filepath.Join(dir, "lambe", "gone.lua"), string(rexManagedContent([]byte("return 1\n"), 1)))
				mustWrite(t, filepath.Join(dir, "lambe", "mine.lua"), "return 2\n")
			},
			wantOut: []string{"remove  %DIR%/lambe/gone.lua"},
			check: func(t *testing.T, dir string) {
				if _, err := os.Stat(filepath.Join(dir, "lambe", "gone.lua")); err == nil {
					t.Error("stale module kept")
				}
				if _, err := os.Stat(filepath.Join(dir, "lambe", "mine.lua")); err != nil {
					t.Error("foreign module removed")
				}
			},
		},
		{
			name: "init.lua that loads lambe.lua gets no snippet", args: []string{"--apply"},
			prepare: func(t *testing.T, dir string) { mustWrite(t, filepath.Join(dir, "init.lua"), `dofile("x/lambe.lua")`) },
			wantOut: []string{"already loads lambe.lua"}, denyOut: []string{"Add this"},
		},
		{
			name: "uninstall dry run keeps the files", args: []string{"--uninstall"},
			prepare: func(t *testing.T, dir string) { installRex(t, dir) },
			wantOut: []string{"remove"},
			check: func(t *testing.T, dir string) {
				if _, err := os.Stat(filepath.Join(dir, "lambe.lua")); err != nil {
					t.Error("dry-run uninstall removed lambe.lua")
				}
			},
		},
		{
			name: "uninstall removes managed files and keeps the settings", args: []string{"--uninstall", "--apply"},
			prepare: func(t *testing.T, dir string) { installRex(t, dir) },
			wantOut: []string{"remove", "init.lua yourself", "your settings"},
			check: func(t *testing.T, dir string) {
				for _, gone := range []string{"lambe.lua", "lambe"} {
					if _, err := os.Stat(filepath.Join(dir, gone)); err == nil {
						t.Errorf("%s survived uninstall", gone)
					}
				}
				if _, err := os.Stat(filepath.Join(dir, "lambe.config.lua")); err != nil {
					t.Error("uninstall removed the user's settings")
				}
			},
		},
		{
			name: "uninstall refuses a modified file", args: []string{"--uninstall", "--apply"},
			prepare: func(t *testing.T, dir string) {
				installRex(t, dir)
				p := filepath.Join(dir, "lambe.lua")
				mustWrite(t, p, readFile(t, p)+"-- mine\n")
			},
			wantErr: true, wantOut: []string{"refuse"},
			check: func(t *testing.T, dir string) {
				if _, err := os.Stat(filepath.Join(dir, "lambe", "init.lua")); err != nil {
					t.Error("partial uninstall happened")
				}
			},
		},
		{
			name: "uninstall with nothing installed", args: []string{"--uninstall", "--apply"},
			wantOut: []string{"not installed"},
		},
		{name: "stray argument is a usage error", args: []string{"nope"}, wantErr: true, wantUsage: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.prepare != nil {
				tc.prepare(t, dir)
			}
			var out bytes.Buffer
			err := runSetupRex(append([]string{"--dir", dir}, tc.args...), testRexEnv(dir), &out)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v\n%s", err, tc.wantErr, out.String())
			}
			var usage usageError
			if tc.wantUsage != errors.As(err, &usage) {
				t.Errorf("usage error = %v, want %v (%v)", errors.As(err, &usage), tc.wantUsage, err)
			}
			if tc.check != nil {
				tc.check(t, dir)
			}
			for _, want := range tc.wantOut {
				want = strings.ReplaceAll(want, "%DIR%", dir)
				if !strings.Contains(out.String(), want) {
					t.Errorf("output missing %q:\n%s", want, out.String())
				}
			}
			for _, deny := range tc.denyOut {
				if strings.Contains(out.String(), deny) {
					t.Errorf("output has %q:\n%s", deny, out.String())
				}
			}
			// init.lua is never created or edited.
			if _, err := os.Stat(filepath.Join(dir, "init.lua")); err == nil && !strings.Contains(tc.name, "init.lua") {
				t.Errorf("init.lua was created")
			}
		})
	}
}

func TestRexConfigDir(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"default", nil, "/home/u/.config/rex"},
		{"xdg", map[string]string{"XDG_CONFIG_HOME": "/x"}, "/x/rex"},
		{"REX_CONFIG wins", map[string]string{"REX_CONFIG": "/c/my.lua", "XDG_CONFIG_HOME": "/x"}, "/c"},
	}
	for _, tc := range tests {
		e := rexSetupEnv{Getenv: func(k string) string { return tc.env[k] }, Home: "/home/u"}
		if got := e.rexConfigDir(); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

func TestSetupRequiresRexTarget(t *testing.T) {
	var usage usageError
	if err := runSetup(nil, rexSetupEnv{}, &bytes.Buffer{}); !errors.As(err, &usage) {
		t.Errorf("err = %v, want usage error", err)
	}
}

func clip(s string) string {
	if len(s) > 60 {
		return s[:60]
	}
	return s
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
