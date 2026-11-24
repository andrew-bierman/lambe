package tui

import (
	"context"
	"errors"
	"os"

	tea "github.com/charmbracelet/bubbletea"
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
	p := tea.NewProgram(model, tea.WithContext(ctx), tea.WithAltScreen(), tea.WithMouseCellMotion())
	if _, err := p.Run(); err != nil {
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	}
	return nil
}
