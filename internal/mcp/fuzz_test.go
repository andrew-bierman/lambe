package mcp

import (
	"strings"
	"testing"
	"unicode"
)

// FuzzParseRunHandle checks the handle parser accepts exactly the strings
// free of whitespace and control characters, and that accepted handles
// round-trip through their wire form.
func FuzzParseRunHandle(f *testing.F) {
	for _, s := range []string{
		"", "run-1", "lambe-abc.1", "a b", " lead", "trail ", "tab\t", "nl\n", "nul\x00",
		"\u2028", "\u00a0nbsp", "\x7f", "\xff\xfe", "日本語", "emoji-\U0001F600",
		strings.Repeat("a", 1<<20), strings.Repeat("\x00", 4096),
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		h, err := ParseRunHandle(s)
		bad := s == ""
		for _, r := range s {
			if unicode.IsControl(r) || unicode.IsSpace(r) {
				bad = true
			}
		}
		if bad != (err != nil) {
			t.Fatalf("ParseRunHandle(%q) err = %v, want rejected=%v", s, err, bad)
		}
		if err != nil {
			return
		}
		if h.String() != s {
			t.Fatalf("String() = %q, want %q", h.String(), s)
		}
		back, err := ParseRunHandle(h.String())
		if err != nil || back != h {
			t.Fatalf("round trip: %v, %v != %v", err, back, h)
		}
	})
}
