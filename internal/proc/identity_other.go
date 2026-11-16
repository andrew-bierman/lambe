//go:build !linux && !darwin

package proc

import "fmt"

// ProcessIdentity reports that safe process identity lookup is unsupported on this platform.
func ProcessIdentity(pid int) (string, error) {
	return "", fmt.Errorf("process identity lookup is unsupported for pid %d", pid)
}
