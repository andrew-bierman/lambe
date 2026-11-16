// Package logging builds the daemon's slog logger and the per-operation
// enrichment helpers.
//
// All output is structured JSON via slog's JSON handler. Log messages are
// constant strings; every variable detail rides along as an attribute, never
// interpolated into the message with Sprintf.
package logging

import (
	"io"
	"log/slog"
	"os"
)

// New returns a JSON slog.Logger writing to w at the given level.
func New(w io.Writer, level slog.Level) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level}))
}

// Default returns the production logger: JSON to stderr at info level.
func Default() *slog.Logger {
	return New(os.Stderr, slog.LevelInfo)
}

// WithIssue returns a logger annotated with the Beads issue id.
func WithIssue(l *slog.Logger, issue string) *slog.Logger {
	return l.With(slog.String("issue", issue))
}

// WithSession returns a logger annotated with the supervised session id.
func WithSession(l *slog.Logger, session string) *slog.Logger {
	return l.With(slog.String("session", session))
}

// WithAttempt returns a logger annotated with the attempt number.
func WithAttempt(l *slog.Logger, attempt int) *slog.Logger {
	return l.With(slog.Int("attempt", attempt))
}

// WithPgid returns a logger annotated with the process group id.
func WithPgid(l *slog.Logger, pgid int) *slog.Logger {
	return l.With(slog.Int("pgid", pgid))
}
