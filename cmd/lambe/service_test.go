package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeSys records service-manager calls instead of executing them.
type fakeSys struct {
	calls []string
	reply func(name string, args []string) (string, error)
}

func (f *fakeSys) env(goos, home string) sysEnv {
	return sysEnv{GOOS: goos, Home: home, UID: 501, Run: func(_ context.Context, name string, args ...string) (string, error) {
		f.calls = append(f.calls, name+" "+strings.Join(args, " "))
		if f.reply != nil {
			return f.reply(name, args)
		}
		return "", nil
	}}
}

func writeConfig(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte("harness: pi\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRenderUnit(t *testing.T) {
	t.Parallel()
	p := unitParams{Binary: "/bin/lambe", Config: "/cfg/a&b.yaml", Path: "/x:/y", Log: "/log/d.log"}
	plist, err := renderUnit("launchd", p)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"com.lambe.daemon", "a&amp;b.yaml", "<key>PATH</key>", "<integer>45</integer>", "<key>KeepAlive</key>"} {
		if !strings.Contains(plist, want) {
			t.Errorf("plist missing %q", want)
		}
	}
	unit, err := renderUnit("systemd", unitParams{Binary: "/bin/lambe", Config: "/cfg/100%.yaml", Path: "/x:/y"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`ExecStart="/bin/lambe" run --config "/cfg/100%%.yaml"`, `Environment="PATH=/x:/y"`, "SuccessExitStatus=3", "TimeoutStopSec=45"} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit missing %q:\n%s", want, unit)
		}
	}
	if _, err := renderUnit("systemd", unitParams{Binary: `/bin/"lambe"`}); err == nil {
		t.Error("quote in path accepted")
	}
}

func TestServiceInstallDryRunWritesNothing(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	f := &fakeSys{}
	var out bytes.Buffer
	err := runService([]string{"install", "-dry-run", "-config", writeConfig(t)}, f.env("darwin", home), &out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "com.lambe.daemon") {
		t.Errorf("dry run printed no unit: %s", out.String())
	}
	if entries, _ := os.ReadDir(home); len(entries) != 0 {
		t.Errorf("dry run wrote into home: %v", entries)
	}
	if len(f.calls) != 0 {
		t.Errorf("dry run ran commands: %v", f.calls)
	}
}

func TestServiceRootInstallNeverCallsServiceManager(t *testing.T) {
	t.Parallel()
	home, root := t.TempDir(), t.TempDir()
	f := &fakeSys{}
	var out bytes.Buffer
	if err := runService([]string{"install", "-root", root, "-config", writeConfig(t)}, f.env("linux", home), &out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, ".config", "systemd", "user", "lambe.service")); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 0 {
		t.Errorf("--root touched the service manager: %v", f.calls)
	}
	// Re-install with identical content is fine; changed content needs --force.
	if err := runService([]string{"install", "-root", root, "-config", writeConfig(t)}, f.env("linux", home), &out); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Errorf("different config replaced without --force: %v", err)
	}
	if err := runService([]string{"uninstall", "-root", root}, f.env("linux", home), &out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, ".config", "systemd", "user", "lambe.service")); !os.IsNotExist(err) {
		t.Errorf("unit survived uninstall: %v", err)
	}
}

func TestServiceInstallLaunchdLoadsAndStatus(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	f := &fakeSys{reply: func(_ string, args []string) (string, error) {
		if len(args) > 0 && args[0] == "print" {
			return "state = running\n\tpid = 4242\n", nil
		}
		return "", nil
	}}
	var out bytes.Buffer
	if err := runService([]string{"install", "-config", writeConfig(t)}, f.env("darwin", home), &out); err != nil {
		t.Fatal(err)
	}
	if want := "launchctl bootstrap gui/501 " + filepath.Join(home, "Library", "LaunchAgents", "com.lambe.daemon.plist"); len(f.calls) != 1 || f.calls[0] != want {
		t.Errorf("calls = %v, want [%s]", f.calls, want)
	}
	if _, err := os.Stat(filepath.Join(home, "Library", "Logs", "lambe")); err != nil {
		t.Errorf("log directory not created: %v", err)
	}
	out.Reset()
	if err := runService([]string{"status"}, f.env("darwin", home), &out); err != nil {
		t.Fatalf("status of a running service: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "running") {
		t.Errorf("status output: %s", out.String())
	}
}

func TestServiceStatusNotInstalledFails(t *testing.T) {
	t.Parallel()
	f := &fakeSys{}
	if err := runService([]string{"status"}, f.env("linux", t.TempDir()), &bytes.Buffer{}); err == nil {
		t.Error("status of a missing service succeeded")
	}
}

func TestServiceInstallRequiresConfig(t *testing.T) {
	t.Parallel()
	f := &fakeSys{}
	err := runService([]string{"install", "-config", filepath.Join(t.TempDir(), "missing.yaml")}, f.env("linux", t.TempDir()), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "lambe init") {
		t.Errorf("err = %v", err)
	}
}
