package main

import (
	"strings"
	"testing"
)

func TestTerminalTextCannotEmitEscapeSequences(t *testing.T) {
	text := terminalText("hello\n\x1b]52;c;secret\x07\u009b2J\r\x00world\t🦖")
	if strings.ContainsAny(text, "\x1b\x07\u009b\r\x00") || !strings.Contains(text, "hello\n") || !strings.Contains(text, "world\t🦖") {
		t.Fatalf("unsafe or damaged terminal text: %q", text)
	}
}

func TestFollowTailUsesByteOffsetAfterWrap(t *testing.T) {
	tail := strings.Repeat("old ", 1000) + "tool started: read\n"
	if got := tailDelta(tail, int64(len("tool started: read\n"))); got != "tool started: read\n" {
		t.Fatalf("repeated wrapped output: %q", got)
	}
	if got := tailDelta(tail, 0); got != "" {
		t.Fatal("phase-only change repeated output")
	}
	if got := tailDelta(tail, -1); got != tail {
		t.Fatal("new daemon lost its initial output")
	}
}
