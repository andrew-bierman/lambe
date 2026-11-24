package tui

import (
	"context"
	"errors"
	"os"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/term"

	"github.com/andrew-bierman/lambe/internal/tracker"
)

// Run starts the dashboard and blocks until the user quits.
func Run(ctx context.Context, path, trackerPath string, queue func(string) (string, error), readiness func() ([]tracker.ReadinessRow, error), workflows ...QueueWorkflow) error {
	model := New(path)
	model.trackerPath = trackerPath
	model.queueTask = queue
	model.readiness = readiness
	model.sessionContext = ctx
	model.queueWorkflows = workflows
	return run(ctx, model)
}

func run(ctx context.Context, model *Model) error {
	if !term.IsTerminal(os.Stdout.Fd()) {
		return errors.New("lambe tui needs an interactive terminal; use `lambe logs -follow` for pipes")
	}
	// Resolve the background before bubbletea owns stdin; the query is bounded.
	if !applyTheme(os.Getenv, lipgloss.SetHasDarkBackground) {
		dark, ok := detectBackground(os.Stdin, os.Stdout, os.Getenv("TERM"), oscBudget)
		lipgloss.SetHasDarkBackground(dark || !ok) // silent terminal: dark default
	}
	p := tea.NewProgram(model, programOptions(ctx)...)
	if _, err := p.Run(); err != nil {
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	}
	return nil
}

// programOptions are the options the dashboard's program always runs with;
// tests append their own input and output.
func programOptions(ctx context.Context) []tea.ProgramOption {
	return []tea.ProgramOption{tea.WithContext(ctx), tea.WithAltScreen(), tea.WithMouseCellMotion()}
}
