// SPDX-License-Identifier: GPL-3.0-or-later

package gate

import "testing"

func TestKindStringRoundTripsThroughParseKind(t *testing.T) {
	for _, kind := range []Kind{KindShell, KindGhRun, KindGhPr} {
		text := kind.String()
		parsed, err := ParseKind(text)
		if err != nil {
			t.Errorf("ParseKind(%q) error: %v", text, err)
			continue
		}
		if parsed != kind {
			t.Errorf("ParseKind(%q) = %v, want %v", text, parsed, kind)
		}
	}
}

func TestKindStringInvalid(t *testing.T) {
	for _, kind := range []Kind{KindInvalid, Kind(99)} {
		if got := kind.String(); got != "invalid" {
			t.Errorf("Kind(%d).String() = %q, want invalid", int(kind), got)
		}
	}
}

func TestExitFailureError(t *testing.T) {
	for _, tc := range []struct {
		failure ExitFailure
		want    string
	}{
		{ExitFailure{Code: 1, Output: "boom"}, "exit 1: boom"},
		{ExitFailure{Code: 128, Output: "fatal: not a git repository"}, "exit 128: fatal: not a git repository"},
		{ExitFailure{Code: 0, Output: ""}, "exit 0: "},
	} {
		if got := tc.failure.Error(); got != tc.want {
			t.Errorf("Error() = %q, want %q", got, tc.want)
		}
	}
}
