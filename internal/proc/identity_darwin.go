// SPDX-License-Identifier: GPL-3.0-or-later

//go:build darwin

package proc

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// ProcessIdentity identifies this incarnation of a process, rather than its recyclable pid.
func ProcessIdentity(pid int) (string, error) {
	if pid <= 1 {
		return "", fmt.Errorf("invalid agent pid %d", pid)
	}
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return "", err
	}
	if int(info.Proc.P_pid) != pid || info.Proc.P_starttime.Sec == 0 {
		return "", fmt.Errorf("process %d unavailable", pid)
	}
	boot, err := unix.Sysctl("kern.bootsessionuuid")
	if err != nil || boot == "" {
		return "", fmt.Errorf("read boot identity: %w", err)
	}
	return fmt.Sprintf("darwin:%s:%d:%d", boot, info.Proc.P_starttime.Sec, info.Proc.P_starttime.Usec), nil
}
