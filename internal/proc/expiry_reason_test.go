// SPDX-License-Identifier: GPL-3.0-or-later

package proc_test

import (
	"testing"

	"github.com/andrew-bierman/lambe/internal/proc"
)

func TestExpiryReasonString(t *testing.T) {
	for _, tc := range []struct {
		reason proc.ExpiryReason
		want   string
	}{
		{proc.ReasonSilence, "silence"},
		{proc.ReasonDeadline, "deadline"},
		{proc.ExpiryReason(99), "unknown"},
	} {
		if got := tc.reason.String(); got != tc.want {
			t.Errorf("ExpiryReason(%d).String() = %q, want %q", int(tc.reason), got, tc.want)
		}
	}
}
