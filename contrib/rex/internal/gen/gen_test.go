package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestStubSchemaMatchesFixtures fails when the committed Lua table is not
// what the fixtures generate: capture or edit fixtures, then rerun
// `go run ./contrib/rex/internal/gen`.
func TestStubSchemaMatchesFixtures(t *testing.T) {
	want, err := Generate("../../testdata/api")
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile("../../test/apischema.lua")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatal("contrib/rex/test/apischema.lua is stale; run: go run ./contrib/rex/internal/gen")
	}
}

func TestGenerateRejectsMismatchedNames(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "rev9"), 0o750); err != nil {
		t.Fatal(err)
	}
	doc := `{"name":"session.other","input_schema":{"properties":{},"additionalProperties":false}}`
	if err := os.WriteFile(filepath.Join(dir, "rev9", "session.view.json"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Generate(dir); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("err = %v, want a name mismatch", err)
	}
}

func TestGenerateTypesEnumsRequired(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "rev1"), 0o750); err != nil {
		t.Fatal(err)
	}
	doc := `{"name":"m.x","input_schema":{"properties":{"a":{"type":["string","null"]},"d":{"type":"string","enum":["h","v"]}},"required":["d","a"],"additionalProperties":false}}`
	if err := os.WriteFile(filepath.Join(dir, "rev1", "m.x.json"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := Generate(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`[1] = {`, `["m.x"] = { closed = true,`, `required = {"a", "d"}`,
		`["a"] = { types = {"null", "string"} }`, `["d"] = { types = {"string"}, enum = {"h", "v"} }`,
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}
