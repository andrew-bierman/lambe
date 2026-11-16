package proc

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// MaxFrameBytes is the largest single JSONL line ScanFrames accepts. Agent
// frames larger than this abort the scan rather than exhausting memory.
const MaxFrameBytes = 4 << 20 // 4 MiB

// Frame is one JSONL protocol frame from an agent process: {"type":"...","data":{...}}.
type Frame struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data,omitempty"`
}

// ParseFrame decodes one LF-delimited line into a Frame. A line is a valid
// frame only if it is well-formed JSON with a non-empty "type" field.
func ParseFrame(line []byte) (Frame, error) {
	var frame Frame
	if err := json.Unmarshal(line, &frame); err != nil {
		return Frame{}, fmt.Errorf("parse frame: %w", err)
	}
	if frame.Type == "" {
		return Frame{}, errors.New("parse frame: empty type")
	}
	return frame, nil
}

// splitLF splits data strictly on the LF byte. Unlike [bufio.ScanLines] it
// does not strip a trailing CR, and byte sequences such as the Unicode line
// separators U+2028/U+2029 never terminate a line.
func splitLF(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := bytes.IndexByte(data, '\n'); i >= 0 {
		return i + 1, data[:i], nil
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// ScanFrames reads LF-terminated JSONL frames from r and invokes fn for each
// valid frame. Lines that do not parse as frames are skipped. It returns nil
// at EOF, the first error from fn if fn aborts, or the scanner error
// (including [bufio.ErrTooLong] for oversized lines).
func ScanFrames(r io.Reader, fn func(Frame) error) error {
	scanner := bufio.NewScanner(r)
	scanner.Split(splitLF)
	scanner.Buffer(make([]byte, 0, 64*1024), MaxFrameBytes)
	for scanner.Scan() {
		frame, err := ParseFrame(scanner.Bytes())
		if err != nil {
			continue
		}
		if err := fn(frame); err != nil {
			return err
		}
	}
	return scanner.Err()
}
