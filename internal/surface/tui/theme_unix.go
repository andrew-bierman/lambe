//go:build unix

package tui

import (
	"io"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/x/term"
	"golang.org/x/sys/unix"
)

// detectBackground asks the terminal for its background colour (OSC 11) and
// waits at most budget for the answer. ok is false when it never answers, is
// not an interactive terminal, or sits behind tmux/screen (which cannot relay
// the query). The caller keeps its dark default in that case.
func detectBackground(in, out *os.File, termName string, budget time.Duration) (dark, ok bool) {
	if strings.HasPrefix(termName, "screen") || strings.HasPrefix(termName, "tmux") || strings.HasPrefix(termName, "dumb") {
		return false, false
	}
	fd := in.Fd()
	if !term.IsTerminal(fd) || !term.IsTerminal(out.Fd()) {
		return false, false
	}
	state, err := term.MakeRaw(fd)
	if err != nil {
		return false, false
	}
	defer func() { _ = term.Restore(fd, state) }()
	return queryOSC11(out, int(fd), budget) //nolint:gosec // fd fits int
}

// queryOSC11 writes the query to w and reads the reply from fd until its
// terminator or the budget runs out.
func queryOSC11(w io.Writer, fd int, budget time.Duration) (dark, ok bool) {
	if _, err := io.WriteString(w, "\x1b]11;?\x1b\\"); err != nil {
		return false, false
	}
	deadline := time.Now().Add(budget)
	var reply []byte
	buf := make([]byte, 64)
	for {
		left := time.Until(deadline)
		if left <= 0 {
			return false, false
		}
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}} //nolint:gosec // fd fits int32
		n, err := unix.Poll(fds, int(left/time.Millisecond)+1)
		if err == unix.EINTR {
			continue
		}
		if err != nil || n == 0 {
			return false, false
		}
		got, err := unix.Read(fd, buf)
		if err != nil || got <= 0 {
			return false, false
		}
		reply = append(reply, buf[:got]...)
		if s := string(reply); strings.HasSuffix(s, "\a") || strings.HasSuffix(s, "\x1b\\") {
			return parseOSC11(s)
		}
	}
}
