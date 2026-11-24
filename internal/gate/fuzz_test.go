package gate_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/andrew-bierman/lambe/internal/gate"
)

var hex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// FuzzParseSpecFile writes arbitrary bytes as a formula file. Parsing must
// never panic; an accepted spec must pass its own validation, carry a content
// revision, and marshal to JSON (the daemon marshals specs into the
// workflow gate spec).
func FuzzParseSpecFile(f *testing.F) {
	for _, s := range []string{
		"",
		"[gate]\n",
		"[gate]\ntype = \"shell\"\ncommand = \"true\"\n",
		"[trigger]\ncommand = \"true\"\n\n[gate]\ntype = \"gh:pr\"\npr = \"12\"\n",
		"[gate]\ntype = \"gh:run\"\nref = \"1234567\"\n",
		"[gate]\ntype = \"gh:run\"\nref = \"main\"\n",
		"[gate]\ntype = \"gh:run\"\nrun_id = \"0123456789abcdef0123456789abcdef01234567\"\n",
		"[gate]\ntype = \"nope\"\n",
		"[gate]\ntype = 5\n",
		"[gate\ntype = \"shell\"\n",
		"[gate]\ntype = \"shell\"\ncommand = \"\\ud800\"\n",
		"[gate]\ntype = \"shell\"\ncommand = \"a\x00b\xff\"\n",
		"[gate]\ntype = \"shell\"\ncommand = \"x\"\ncommand = \"dup\"\n",
		"a = " + "[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[",
		"[gate]\ntype = \"shell\"\ncommand = \"" + string(repeatByte('a', 1<<20)) + "\"\n",
	} {
		f.Add([]byte(s))
	}
	dir := f.TempDir()
	f.Fuzz(func(t *testing.T, data []byte) {
		path := filepath.Join(dir, "fuzz.formula.toml")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		spec, err := gate.ParseSpecFile(path)
		if err != nil {
			if _, err2 := gate.ParseSpecFile(path); err2 == nil || err2.Error() != err.Error() {
				t.Fatalf("unstable error: %v vs %v", err, err2)
			}
			return
		}
		if errors.Is(err, gate.ErrNoGate) {
			t.Fatal("success wrapped ErrNoGate")
		}
		if err := spec.Config.Validate(); err != nil {
			t.Fatalf("accepted spec fails validation: %v", err)
		}
		if spec.Name != "fuzz" || !hex64.MatchString(spec.Revision) {
			t.Fatalf("bad spec identity: name=%q revision=%q", spec.Name, spec.Revision)
		}
		if _, err := json.Marshal(spec); err != nil {
			t.Fatalf("marshal: %v", err)
		}
	})
}

func repeatByte(b byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return out
}
