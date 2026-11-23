package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	rexcontrib "github.com/andrew-bierman/lambe/contrib/rex"
)

func TestSetupRex(t *testing.T) {
	managed := string(rexManagedContent(rexcontrib.LambeLua))
	tampered := strings.Replace(managed, "local override", "local overridden", 1)
	oldManaged := string(rexManagedContent([]byte("-- old script\n")))

	tests := []struct {
		name      string
		args      []string
		existing  *string // lambe.lua before the run; nil means absent
		initLua   string
		wantFile  *string // lambe.lua after the run; nil means absent
		wantErr   bool
		wantUsage bool
		wantOut   []string
		denyOut   []string
	}{
		{
			name: "dry run writes nothing", existing: nil, wantFile: nil,
			wantOut: []string{"dry run", "create", "dofile(", "rex config reload"},
		},
		{
			name: "apply creates the managed file", args: []string{"--apply"}, wantFile: &managed,
			wantOut: []string{"applying", "create"},
		},
		{
			name: "apply is idempotent", args: []string{"--apply"}, existing: &managed, wantFile: &managed,
			wantOut: []string{"keep"},
		},
		{
			name: "apply upgrades an older managed file", args: []string{"--apply"}, existing: &oldManaged, wantFile: &managed,
			wantOut: []string{"update"},
		},
		{
			name: "apply adopts a verbatim hand copy", args: []string{"--apply"}, existing: ptr(string(rexcontrib.LambeLua)), wantFile: &managed,
			wantOut: []string{"update"},
		},
		{
			name: "refuses a user-modified managed file", args: []string{"--apply"}, existing: &tampered, wantFile: &tampered, wantErr: true,
			wantOut: []string{"refuse", "--force"},
		},
		{
			name: "refuses a foreign file", args: []string{"--apply"}, existing: ptr("-- mine\n"), wantFile: ptr("-- mine\n"), wantErr: true,
			wantOut: []string{"refuse"},
		},
		{
			name: "force replaces a foreign file", args: []string{"--apply", "--force"}, existing: ptr("-- mine\n"), wantFile: &managed,
			wantOut: []string{"replace"},
		},
		{
			name: "init.lua that loads lambe.lua gets no snippet", args: []string{"--apply"}, initLua: `dofile("x/lambe.lua")`, wantFile: &managed,
			wantOut: []string{"already loads lambe.lua"}, denyOut: []string{"Add this"},
		},
		{
			name: "uninstall dry run keeps the file", args: []string{"--uninstall"}, existing: &managed, wantFile: &managed,
			wantOut: []string{"remove"},
		},
		{
			name: "uninstall removes a managed file", args: []string{"--uninstall", "--apply"}, existing: &managed, wantFile: nil,
			wantOut: []string{"remove", "init.lua yourself"},
		},
		{
			name: "uninstall refuses a modified file", args: []string{"--uninstall", "--apply"}, existing: &tampered, wantFile: &tampered, wantErr: true,
			wantOut: []string{"refuse"},
		},
		{name: "uninstall force removes a modified file", args: []string{"--uninstall", "--apply", "--force"}, existing: &tampered, wantFile: nil},
		{
			name: "uninstall with nothing installed", args: []string{"--uninstall", "--apply"}, wantFile: nil,
			wantOut: []string{"not installed"},
		},
		{name: "stray argument is a usage error", args: []string{"nope"}, wantErr: true, wantUsage: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			target := filepath.Join(dir, "lambe.lua")
			if tc.existing != nil {
				mustWrite(t, target, *tc.existing)
			}
			if tc.initLua != "" {
				mustWrite(t, filepath.Join(dir, "init.lua"), tc.initLua)
			}
			var out bytes.Buffer
			err := runSetupRex(append([]string{"--dir", dir}, tc.args...), rexSetupEnv{Getenv: func(string) string { return "" }, Home: dir}, &out)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v\n%s", err, tc.wantErr, out.String())
			}
			var usage usageError
			if tc.wantUsage != errors.As(err, &usage) {
				t.Errorf("usage error = %v, want %v (%v)", errors.As(err, &usage), tc.wantUsage, err)
			}
			got, readErr := os.ReadFile(target) // #nosec G304 -- test temp dir
			switch {
			case tc.wantFile == nil && readErr == nil:
				t.Errorf("lambe.lua exists, want absent")
			case tc.wantFile != nil && string(got) != *tc.wantFile:
				t.Errorf("lambe.lua = %q..., want %q...", clip(string(got)), clip(*tc.wantFile))
			}
			for _, want := range tc.wantOut {
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
			data, err := os.ReadFile(filepath.Join(dir, "init.lua")) // #nosec G304 -- test temp dir
			if tc.initLua == "" && err == nil {
				t.Errorf("init.lua was created")
			} else if tc.initLua != "" && string(data) != tc.initLua {
				t.Errorf("init.lua was edited")
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

func TestSetupRequiresRexTarget(t *testing.T) {
	var usage usageError
	if err := runSetup(nil, rexSetupEnv{}, &bytes.Buffer{}); !errors.As(err, &usage) {
		t.Errorf("err = %v, want usage error", err)
	}
}

func ptr(s string) *string { return &s }

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
