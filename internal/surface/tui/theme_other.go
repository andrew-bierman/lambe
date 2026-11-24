//go:build !unix

package tui

import (
	"os"
	"time"
)

// detectBackground is unsupported off unix; the dark default applies.
func detectBackground(_, _ *os.File, _ string, _ time.Duration) (dark, ok bool) { return false, false }
