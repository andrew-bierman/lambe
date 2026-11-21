package main

import (
	"bytes"
	"encoding/json"
	"runtime/debug"
	"testing"
)

func TestResolveVersion(t *testing.T) {
	build := func(mainVersion string, settings ...debug.BuildSetting) *debug.BuildInfo {
		return &debug.BuildInfo{Main: debug.Module{Version: mainVersion}, Settings: settings}
	}
	rev := debug.BuildSetting{Key: "vcs.revision", Value: "abcdef1234567890"}
	dirty := debug.BuildSetting{Key: "vcs.modified", Value: "true"}
	tests := []struct {
		name    string
		ld      string
		info    *debug.BuildInfo
		want    string
		wantSHA string
	}{
		{"goreleaser ldflags omit v", "0.9.2", nil, "v0.9.2", "none"},
		{"ldflags with v untouched", "v1.0.0", nil, "v1.0.0", "none"},
		{"go install exact tag", "dev", build("v0.9.2"), "v0.9.2", "none"},
		{"checkout pseudo-version", "dev", build("v0.9.3-0.20261120120000-abcdef123456", rev), "v0.9.3-dev+abcdef1", "abcdef1234567890"},
		{"dirty pseudo-version", "dev", build("v0.9.3-0.20261120120000-abcdef123456+dirty", rev), "v0.9.3-dev+abcdef1.dirty", "abcdef1234567890"},
		{"devel with vcs", "dev", build("(devel)", rev, dirty), "v0.0.0-dev+abcdef1.dirty", "abcdef1234567890"},
		{"no build info", "dev", nil, "v0.0.0-dev+unknown", "none"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveVersion(tc.ld, "none", "unknown", tc.info)
			if got.Version != tc.want {
				t.Errorf("Version = %q, want %q", got.Version, tc.want)
			}
			if got.Commit != tc.wantSHA {
				t.Errorf("Commit = %q, want %q", got.Commit, tc.wantSHA)
			}
			if got.APIVersion != 1 {
				t.Errorf("APIVersion = %d, want 1", got.APIVersion)
			}
		})
	}
}

func TestRunVersionJSON(t *testing.T) {
	var out bytes.Buffer
	if err := runVersion([]string{"--json"}, &out); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("not JSON: %v: %s", err, out.String())
	}
	for _, k := range []string{"version", "commit", "date", "api_version"} {
		if _, ok := got[k]; !ok {
			t.Errorf("missing key %q in %s", k, out.String())
		}
	}
	out.Reset()
	if err := runVersion(nil, &out); err != nil || !bytes.HasPrefix(out.Bytes(), []byte("lambe v")) {
		t.Errorf("text output %q, err %v", out.String(), err)
	}
	if err := runVersion([]string{"--bogus"}, &out); err == nil {
		t.Error("unknown flag should error")
	}
}
