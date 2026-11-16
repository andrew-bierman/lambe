package proc_test

import (
	"bufio"
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/andrew-bierman/lambe/internal/proc"
)

func TestParseFrame(t *testing.T) {
	tests := []struct {
		name    string
		line    string
		wantErr bool
		wantTyp string
	}{
		{name: "plain frame", line: `{"type":"result","data":{"status":"done"}}`, wantTyp: "result"},
		{name: "no data", line: `{"type":"ping"}`, wantTyp: "ping"},
		{name: "empty type", line: `{"type":"","data":{}}`, wantErr: true},
		{name: "missing type", line: `{"data":{}}`, wantErr: true},
		{name: "not json", line: `noise on stdout`, wantErr: true},
		{name: "truncated json", line: `{"type":"result"`, wantErr: true},
		{
			// U+2028 and U+2029 are valid inside JSON strings and must not
			// terminate the line.
			name:    "unicode line separators inside payload",
			line:    `{"type":"text","data":{"v":"a b c"}}`,
			wantTyp: "text",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			frame, err := proc.ParseFrame([]byte(tt.line))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseFrame(%q) = %+v, want error", tt.line, frame)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseFrame(%q): %v", tt.line, err)
			}
			if frame.Type != tt.wantTyp {
				t.Fatalf("type = %q, want %q", frame.Type, tt.wantTyp)
			}
		})
	}
}

func TestScanFramesSplitsOnlyOnLF(t *testing.T) {
	// U+2028/U+2029 embedded raw in the stream must stay inside their lines.
	u2028 := "\u2028"
	u2029 := "\u2029"
	input := "{\"type\":\"a\",\"data\":{\"v\":\"x" + u2028 + "y" + u2029 + "z\"}}\n" +
		"{\"type\":\"b\"}\r\n" + // CR stays: only LF ends the line
		"garbage\n" +
		"{\"type\":\"c\"}"

	var types []string
	err := proc.ScanFrames(strings.NewReader(input), func(f proc.Frame) error {
		types = append(types, f.Type)
		return nil
	})
	if err != nil {
		t.Fatalf("ScanFrames: %v", err)
	}
	want := []string{"a", "b", "c"}
	if len(types) != len(want) {
		t.Fatalf("types = %v, want %v", types, want)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("types[%d] = %q, want %q", i, types[i], want[i])
		}
	}
}

func TestScanFramesPropagatesError(t *testing.T) {
	stop := errors.New("stop")
	calls := 0
	err := proc.ScanFrames(strings.NewReader("{\"type\":\"a\"}\n{\"type\":\"b\"}\n"), func(proc.Frame) error {
		calls++
		return stop
	})
	if !errors.Is(err, stop) {
		t.Fatalf("err = %v, want stop", err)
	}
	if calls != 1 {
		t.Fatalf("callback ran %d times, want 1", calls)
	}
}

func TestScanFramesRejectsOversizedLine(t *testing.T) {
	huge := "{\"type\":\"big\",\"data\":{\"p\":\"" + strings.Repeat("x", proc.MaxFrameBytes) + "\"}}\n"
	err := proc.ScanFrames(bytes.NewReader([]byte(huge)), func(proc.Frame) error {
		t.Fatal("oversized frame must not be delivered")
		return nil
	})
	if !errors.Is(err, bufio.ErrTooLong) && err == nil {
		t.Fatalf("err = %v, want a too-long error", err)
	}
}

func FuzzScanFrames(f *testing.F) {
	seeds := []string{
		"",
		"\n",
		"{\"type\":\"a\"}\n",
		"{\"type\":\"a\"}",
		"{\"type\":\"a\",\"data\":null}\n",
		string(bytes.Repeat([]byte("{\"type\":\"x\"}\n"), 50)),
		"\u2028\u2029\n\u2028",
		"\r\n\r{\"type\":\"crlf\"}\r\n",
		"{\"type\":\"\\ud800\"}\n",
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		var frames int
		err := proc.ScanFrames(bytes.NewReader(data), func(proc.Frame) error {
			frames++
			return nil
		})
		if err != nil && !errors.Is(err, bufio.ErrTooLong) {
			t.Fatalf("unexpected error: %v", err)
		}
		// LF-strict splitting: at most one frame per LF byte, plus a final
		// unterminated line.
		lfCount := bytes.Count(data, []byte{'\n'})
		if frames > lfCount+1 {
			t.Fatalf("%d frames from %d LFs: split is not LF-only", frames, lfCount)
		}
	})
}
