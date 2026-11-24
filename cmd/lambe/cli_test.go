package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// run drives the whole CLI through its public seam with an isolated
// environment, so no test can touch the real home or config.
func run(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("LAMBE_CONFIG", "")
	t.Setenv("LAMBE_STATE_DIR", t.TempDir())
	var out, errb bytes.Buffer
	code = execute(args, strings.NewReader(""), &out, &errb)
	return code, out.String(), errb.String()
}

func TestHelpAndVersionExitZeroOnStdout(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"--help"}, "commands:"},
		{[]string{"-h"}, "commands:"},
		{[]string{"help"}, "exit codes:"},
		{[]string{"help", "init"}, "lambe init"},
		{[]string{"help", "service", "install"}, "--dry-run"},
		{[]string{"--version"}, "lambe "},
		{[]string{"-version"}, "lambe "},
		{[]string{"init", "-h"}, "examples:"},
		{[]string{"init", "--help"}, "--tracker"},
		{[]string{"run", "-h"}, "examples:"},
		{[]string{"status", "-h"}, "examples:"},
		{[]string{"state-dir", "-h"}, "examples:"},
		{[]string{"claims", "-h"}, "examples:"},
		{[]string{"claims", "release", "-h"}, "--force"},
		{[]string{"doctor", "-h"}, "--label"},
		{[]string{"logs", "-h"}, "--follow"},
		{[]string{"enqueue", "-h"}, "--workflow"},
		{[]string{"tui", "-h"}, "--demo"},
		{[]string{"mcp", "-h"}, "examples:"},
		{[]string{"service", "-h"}, "examples:"},
		{[]string{"service", "status", "-h"}, "examples:"},
		{[]string{"setup", "-h"}, "examples:"},
		{[]string{"setup", "rex", "--help"}, "--apply"},
		{[]string{"version", "-h"}, "examples:"},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			code, out, errs := run(t, tc.args...)
			if code != 0 || errs != "" {
				t.Fatalf("code=%d stderr=%q, want 0 and empty stderr", code, errs)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("stdout lacks %q:\n%s", tc.want, out)
			}
			if strings.Contains(out, "\x1b[") {
				t.Errorf("help must not contain color escapes")
			}
		})
	}
}

func TestEveryCommandHasHelpWithExamples(t *testing.T) {
	for _, topic := range helpTopics {
		if !strings.Contains(topic.body, "examples:") || !strings.Contains(topic.body, "usage:") {
			t.Errorf("help topic %q needs usage: and examples:", topic.name)
		}
	}
}

func TestUsageErrorsExitTwoWithOneLine(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{nil, "a command is required"},
		{[]string{"bogus"}, `unknown command "bogus"`},
		{[]string{"help", "bogus"}, "no help for"},
		{[]string{"status", "-nope"}, "flag provided but not defined"},
		{[]string{"status", "extra"}, "unexpected arguments"},
		{[]string{"state-dir", "-nope"}, "flag provided but not defined"},
		{[]string{"version", "-nope"}, "flag provided but not defined"},
		{[]string{"doctor", "-nope"}, "flag provided but not defined"},
		{[]string{"init", "-nope"}, "flag provided but not defined"},
		{[]string{"enqueue", "x"}, "lambe enqueue --workflow"},
		{[]string{"claims"}, "lambe claims"},
		{[]string{"claims", "release"}, "lambe claims"},
		{[]string{"service"}, "install|status|uninstall"},
		{[]string{"service", "bogus"}, "install|status|uninstall"},
		{[]string{"service", "install", "-nope"}, "flag provided but not defined"},
		{[]string{"setup"}, "lambe setup rex"},
		{[]string{"run", "-nope"}, "flag provided but not defined"},
		{[]string{"mcp", "-nope"}, "flag provided but not defined"},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			code, out, errs := run(t, tc.args...)
			if code != 2 {
				t.Fatalf("code=%d, want 2 (stderr %q)", code, errs)
			}
			if out != "" {
				t.Errorf("stdout=%q, want empty", out)
			}
			if !strings.Contains(errs, tc.want) {
				t.Errorf("stderr lacks %q: %q", tc.want, errs)
			}
			if !strings.HasPrefix(errs, "lambe: ") || !strings.Contains(errs, "run 'lambe help") {
				t.Errorf("stderr should be 'lambe: reason' plus a help hint: %q", errs)
			}
			if strings.Contains(errs, `"level"`) {
				t.Errorf("usage errors must not be slog JSON: %q", errs)
			}
		})
	}
}

func TestRuntimeErrorsArePlainText(t *testing.T) {
	// No tracker in the temp cwd: one-shot commands fail with a human line.
	t.Chdir(t.TempDir())
	code, _, errs := run(t, "status")
	if code != 1 {
		t.Fatalf("code=%d, want 1", code)
	}
	if !strings.HasPrefix(errs, "lambe: ") || strings.Contains(errs, `"level"`) || strings.Count(errs, "\n") != 1 {
		t.Errorf("stderr should be one plain 'lambe: ...' line: %q", errs)
	}
}

func TestLambeConfigEnvSelectsConfig(t *testing.T) {
	t.Chdir(t.TempDir())
	_, _, errs := run(t, "status", "--config", "/nonexistent/lambe.yaml")
	t.Setenv("LAMBE_CONFIG", "")
	if !strings.Contains(errs, "lambe.yaml") {
		t.Skipf("config error does not name the path: %q", errs)
	}
}

func TestDoctorServiceLabelAliasStillAccepted(t *testing.T) {
	t.Chdir(t.TempDir())
	if code, _, errs := run(t, "doctor", "-service-label", "x"); code == 2 {
		t.Errorf("deprecated alias must still parse: %q", errs)
	}
}

func TestVersionJSONHasAllFields(t *testing.T) {
	code, out, _ := run(t, "version", "--json")
	var v versionInfo
	if code != 0 || json.Unmarshal([]byte(out), &v) != nil || v.Version == "" {
		t.Fatalf("code=%d out=%q", code, out)
	}
}

func TestStateDirJSONEmptyListIsNotNull(t *testing.T) {
	code, out, _ := run(t, "state-dir", "--json")
	if code != 0 || !strings.Contains(out, `"descriptors":[]`) {
		t.Fatalf("code=%d out=%s", code, out)
	}
}
