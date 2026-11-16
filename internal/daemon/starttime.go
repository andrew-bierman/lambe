package daemon

import (
	"os"
	"strconv"
	"strings"
)

// procStartTime reads field 22 (starttime, in clock ticks since boot) of
// /proc/<pid>/stat — the kernel's identity for "this exact process
// incarnation". A pid alone is not identity: pids are recycled, so a stale
// recorded pgid can point at an unrelated process the daemon must never
// kill. The comm field (2) may contain spaces and parentheses, so parsing
// splits after the last ')' rather than by field count from the start.
//
// ok is false when the stat file cannot be read or parsed (non-Linux, a
// gone process, a malformed file); callers treat unverified as stand-down.
func procStartTime(pid int) (uint64, bool) {
	if pid <= 0 {
		return 0, false
	}
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, false
	}
	s := string(raw)
	// Fields after the comm: state(3) ppid(4) ... starttime(22). With comm
	// removed, starttime is field 22-2 = the 20th remaining field.
	rest, ok := cutLast(s, ")")
	if !ok {
		return 0, false
	}
	fields := strings.Fields(rest)
	// fields[0] is state (field 3); starttime is field 22 → index 22-3.
	if len(fields) < 20 {
		return 0, false
	}
	v, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// cutLast splits s around the last occurrence of sep.
func cutLast(s, sep string) (before string, ok bool) {
	i := strings.LastIndex(s, sep)
	if i < 0 {
		return "", false
	}
	return s[i+len(sep):], true
}
