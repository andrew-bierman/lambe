package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	p := unitParams{Binary: "/bin/lambe", Config: "/cfg/a&b.yaml", Path: "/x:/y", Log: "/log/d.log", Label: "com.lambe.daemon"}
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

func TestServiceLabelIsolatesUnitLogAndLaunchctlCalls(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		goos, unit string
	}{
		{"darwin", filepath.Join("Library", "LaunchAgents", "dev.test.lambe.plist")},
		{"linux", filepath.Join(".config", "systemd", "user", "dev.test.lambe.service")},
	} {
		home := t.TempDir()
		f := &fakeSys{}
		var out bytes.Buffer
		if err := runService([]string{"install", "-label", "dev.test.lambe", "-config", writeConfig(t)}, f.env(tc.goos, home), &out); err != nil {
			t.Fatalf("%s install: %v", tc.goos, err)
		}
		data, err := os.ReadFile(filepath.Join(home, tc.unit)) //nolint:gosec // G304: path under t.TempDir.
		if err != nil {
			t.Fatalf("%s: %v", tc.goos, err)
		}
		if tc.goos == "darwin" {
			if !strings.Contains(string(data), "<string>dev.test.lambe</string>") || strings.Contains(string(data), "com.lambe.daemon") {
				t.Errorf("plist label not isolated:\n%s", data)
			}
			if !strings.Contains(string(data), filepath.Join("Logs", "lambe", "dev.test.lambe.log")) {
				t.Errorf("plist log path not isolated:\n%s", data)
			}
		}
		for _, call := range f.calls {
			if strings.Contains(call, "com.lambe.daemon") || strings.Contains(call, " lambe.service") {
				t.Errorf("%s: call touches the default unit: %s", tc.goos, call)
			}
		}
		if err := runService([]string{"uninstall", "-label", "dev.test.lambe"}, f.env(tc.goos, home), &out); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(home, tc.unit)); !os.IsNotExist(err) {
			t.Errorf("%s: unit survived uninstall: %v", tc.goos, err)
		}
	}
	if err := runService([]string{"install", "-label", "../evil", "-dry-run"}, (&fakeSys{}).env("darwin", t.TempDir()), io.Discard); err == nil {
		t.Error("path-like label accepted")
	}
}

func TestRenderUnitCarriesStateDir(t *testing.T) {
	t.Parallel()
	p := unitParams{Binary: "/bin/lambe", Config: "/c.yaml", Path: "/x", Label: "l", StateDir: "/s/a&b"}
	plist, err := renderUnit("launchd", p)
	if err != nil || !strings.Contains(plist, "<key>LAMBE_STATE_DIR</key>\n        <string>/s/a&amp;b</string>") {
		t.Errorf("plist: %v\n%s", err, plist)
	}
	unit, err := renderUnit("systemd", p)
	if err != nil || !strings.Contains(unit, `Environment="LAMBE_STATE_DIR=/s/a&b"`) {
		t.Errorf("unit: %v\n%s", err, unit)
	}
	for _, m := range []string{"launchd", "systemd"} {
		p.StateDir = ""
		if out, _ := renderUnit(m, p); strings.Contains(out, "LAMBE_STATE_DIR") {
			t.Errorf("%s: state dir emitted when unset", m)
		}
	}
}

func TestServiceUninstallWaitsForLaunchdToStopTheJob(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	prints := 0
	f := &fakeSys{reply: func(_ string, args []string) (string, error) {
		if len(args) > 0 && args[0] == "print" {
			if prints++; prints < 3 {
				return "pid = 1\n", nil // still shutting down
			}
			return "", errors.New("could not find service")
		}
		return "", nil
	}}
	env := f.env("darwin", home)
	env.StopWait = 10 * time.Second
	var out bytes.Buffer
	if err := runService([]string{"install", "-config", writeConfig(t)}, env, &out); err != nil {
		t.Fatal(err)
	}
	f.calls, prints = nil, 0
	if err := runService([]string{"uninstall"}, env, &out); err != nil {
		t.Fatal(err)
	}
	if prints != 3 {
		t.Errorf("uninstall polled launchd %d times, want 3 (calls: %v)", prints, f.calls)
	}
}
