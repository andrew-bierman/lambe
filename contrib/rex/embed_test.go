package rexcontrib

import (
	"bytes"
	"io/fs"
	"os"
	"os/exec"
	"testing"
)

// The embedded tree must be exactly the Lua source tree: a file left out of
// the embed (or a stray one) would ship a different plugin than the tests ran.
func TestEmbeddedMatchesSource(t *testing.T) {
	disk := os.DirFS("lua")
	embedded, err := fs.Sub(FS, "lua")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	err = fs.WalkDir(disk, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		seen[path] = true
		want, err := fs.ReadFile(disk, path)
		if err != nil {
			return err
		}
		got, err := fs.ReadFile(embedded, path)
		if err != nil {
			t.Errorf("%s is not embedded: %v", path, err)
			return nil
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s: embedded copy differs from source", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = fs.WalkDir(embedded, ".", func(path string, d fs.DirEntry, _ error) error {
		if !d.IsDir() && !seen[path] {
			t.Errorf("%s is embedded but not in the source tree", path)
		}
		return nil
	})
}

func TestVersion(t *testing.T) {
	v, err := Version()
	if err != nil || v < 1 {
		t.Fatalf("Version() = %d, %v", v, err)
	}
}

// Runs the pure-Lua suite when luajit is installed; CI's rex-lua job always
// runs it, so a missing luajit only skips it locally.
func TestLuaSuite(t *testing.T) {
	luajit, err := exec.LookPath("luajit")
	if err != nil {
		t.Skip("luajit not installed; CI runs contrib/rex/test/run.lua")
	}
	out, err := exec.Command(luajit, "test/run.lua").CombinedOutput() // #nosec G204 -- fixed test runner
	if err != nil {
		t.Fatalf("lua tests failed: %v\n%s", err, out)
	}
}
