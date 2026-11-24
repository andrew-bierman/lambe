package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompletionScriptsCoverEveryCommandAndFlag(t *testing.T) {
	order, specs := completionSpecs()
	for _, shell := range []string{"bash", "zsh", "fish"} {
		t.Run(shell, func(t *testing.T) {
			code, out, errs := run(t, "completion", shell)
			if code != 0 || errs != "" {
				t.Fatalf("code=%d stderr=%q", code, errs)
			}
			for _, name := range order {
				for _, f := range specs[name].flags {
					if !strings.Contains(out, strings.TrimPrefix(f.name, "--")) {
						t.Errorf("%s completion lacks flag %s of %q", shell, f.name, name)
					}
				}
			}
			for _, name := range topCommands(order, specs) {
				if !strings.Contains(out, name) {
					t.Errorf("%s completion lacks command %s", shell, name)
				}
			}
		})
	}
}

func TestCompletionDerivesValuesFromHelp(t *testing.T) {
	_, specs := completionSpecs()
	var harness, tracker completionFlag
	for _, f := range specs["init"].flags {
		switch f.name {
		case "--harness":
			harness = f
		case "--tracker":
			tracker = f
		}
	}
	if strings.Join(harness.values, ",") != "pi,codex,acp" || !tracker.file {
		t.Errorf("harness=%+v tracker=%+v", harness, tracker)
	}
	if got := strings.Join(specs["service"].subs, ","); got != "install,status,uninstall" {
		t.Errorf("service subs = %q", got)
	}
}

func TestCompletionUnknownShellIsUsageError(t *testing.T) {
	for _, args := range [][]string{{"completion"}, {"completion", "powershell"}, {"completion", "bash", "zsh"}} {
		if code, _, _ := run(t, args...); code != 2 {
			t.Errorf("%v: code=%d, want 2", args, code)
		}
	}
}

// The generated scripts must at least parse in the shells that are installed.
func TestCompletionScriptsParse(t *testing.T) {
	for shell, flag := range map[string]string{"bash": "-n", "zsh": "-n", "fish": "--no-execute"} {
		path, err := exec.LookPath(shell)
		if err != nil {
			continue
		}
		_, script, _ := run(t, "completion", shell)
		file := filepath.Join(t.TempDir(), "lambe."+shell)
		if err := os.WriteFile(file, []byte(script), 0o600); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command(path, flag, file).CombinedOutput(); err != nil { //nolint:gosec // G204: shell found on PATH; script written above
			t.Errorf("%s rejects the script: %v\n%s", shell, err, out)
		}
	}
}
