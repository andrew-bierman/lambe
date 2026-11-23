package daemon

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"

	"github.com/andrew-bierman/lambe/internal/config"
)

// Gate rejection log bounds. The log is a status convenience, not a
// ledger: it keeps the newest records and drops the rest.
const (
	// maxGateRejectionOutput caps one record's stored gate output, in bytes.
	maxGateRejectionOutput = 512
	// maxGateRejectionRecords is how many records survive a compaction.
	maxGateRejectionRecords = 200
	// compactGateRejectionsAt is the file size that triggers a compaction.
	compactGateRejectionsAt = 256 << 10
	// GateRejectionWindow is how far back `lambe status` reports rejections.
	GateRejectionWindow = 24 * time.Hour
)

// GateRejectionRecord is one persisted gate rejection.
type GateRejectionRecord struct {
	Issue   string    `json:"issue"`
	Attempt int       `json:"attempt"`
	At      time.Time `json:"at"`
	Output  string    `json:"output,omitempty"`
}

// GateRejectionLogPath returns the per-instance rejection log path. It lives
// beside, not inside, the live directory that viewers enumerate.
func GateRejectionLogPath(stateDir string, cfg *config.Config) string {
	return filepath.Join(stateDir, "gates", strings.TrimSuffix(InstanceLockName(cfg), ".lock")+".jsonl")
}

// GateRejectionLog appends rejection records to a bounded JSONL file. The
// daemon's loop calls it from concurrent workers.
type GateRejectionLog struct {
	path string
	mu   sync.Mutex
}

// NewGateRejectionLog returns a log writing to path.
func NewGateRejectionLog(path string) *GateRejectionLog { return &GateRejectionLog{path: path} }

// Record appends one rejection: output is stripped of control characters and
// truncated, so nothing unbounded or terminal-hostile lands on disk.
func (l *GateRejectionLog) Record(issue string, attempt int, output string, at time.Time) error {
	rec := GateRejectionRecord{Issue: issue, Attempt: attempt, At: at.UTC(), Output: SanitizeGateOutput(output)}
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(l.path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) // #nosec G304 -- daemon state directory
	if err != nil {
		return err
	}
	_, werr := f.Write(append(data, '\n'))
	cerr := f.Close()
	if werr != nil {
		return werr
	}
	if cerr != nil {
		return cerr
	}
	if info, err := os.Stat(l.path); err == nil && info.Size() > compactGateRejectionsAt {
		return l.compact()
	}
	return nil
}

// compact rewrites the file with only its newest records; the caller holds mu.
func (l *GateRejectionLog) compact() error {
	recs, err := ReadGateRejections(l.path, time.Time{})
	if err != nil {
		return err
	}
	if len(recs) > maxGateRejectionRecords {
		recs = recs[len(recs)-maxGateRejectionRecords:]
	}
	var buf []byte
	for _, rec := range recs {
		line, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		buf = append(append(buf, line...), '\n')
	}
	tmp, err := os.CreateTemp(filepath.Dir(l.path), ".gates-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(buf); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), l.path)
}

// ReadGateRejections returns the records at or after since, oldest first. A
// missing file is no records and no error; unparsable lines are skipped.
func ReadGateRejections(path string, since time.Time) ([]GateRejectionRecord, error) {
	f, err := os.Open(path) // #nosec G304 -- daemon state directory
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var recs []GateRejectionRecord
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		var rec GateRejectionRecord
		if json.Unmarshal(sc.Bytes(), &rec) != nil || rec.Issue == "" || rec.At.Before(since) {
			continue
		}
		recs = append(recs, rec)
	}
	return recs, sc.Err()
}

// SanitizeGateOutput strips control characters (newlines become spaces) and
// truncates to the stored bound on a rune boundary.
func SanitizeGateOutput(s string) string {
	s = strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			return ' '
		case unicode.IsControl(r) || unicode.In(r, unicode.Cf):
			return -1
		}
		return r
	}, ansi.Strip(s))), " ")
	if len(s) <= maxGateRejectionOutput {
		return s
	}
	s = s[:maxGateRejectionOutput]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s + "…"
}
