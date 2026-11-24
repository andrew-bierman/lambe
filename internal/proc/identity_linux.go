// SPDX-License-Identifier: GPL-3.0-or-later

//go:build linux

package proc

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// ProcessIdentity identifies this incarnation of a process, rather than its recyclable pid.
func ProcessIdentity(pid int) (string, error) {
	if pid <= 1 {
		return "", fmt.Errorf("invalid agent pid %d", pid)
	}
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return "", err
	}
	split := strings.LastIndexByte(string(data), ')')
	if split < 0 {
		return "", fmt.Errorf("malformed process stat for %d", pid)
	}
	fields := strings.Fields(string(data[split+1:]))
	if len(fields) < 20 {
		return "", fmt.Errorf("missing process start time for %d", pid)
	}
	start, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil || start == 0 {
		return "", fmt.Errorf("invalid process start time for %d", pid)
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil || strings.TrimSpace(string(boot)) == "" {
		return "", fmt.Errorf("read boot identity: %w", err)
	}
	return "linux:" + strings.TrimSpace(string(boot)) + ":" + fields[19], nil
}
