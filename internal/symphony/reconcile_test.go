package symphony

import (
	"context"
	"testing"

	"github.com/andrew-bierman/lambe/internal/tracker"
)

func TestStatusActive(t *testing.T) {
	t.Parallel()
	tests := []struct {
		status string
		want   bool
	}{
		{status: "open", want: true},
		{status: "ready", want: true},
		{status: "in_progress", want: true},
		{status: "", want: true},
		{status: "closed", want: false},
		{status: "done", want: false},
		{status: "removed", want: false},
	}
	for _, tt := range tests {
		if got := StatusActive(tt.status); got != tt.want {
			t.Errorf("StatusActive(%q) = %v, want %v", tt.status, got, tt.want)
		}
	}
}

func TestReconcileStopsTerminalAndMissing(t *testing.T) {
	t.Parallel()
	ft := newFakeTracker(
		tracker.Issue{ID: "i-live", Status: "in_progress"},
		tracker.Issue{ID: "i-closed", Status: "closed"},
		tracker.Issue{ID: "i-gone", Status: "ready"},
	)
	delete(ft.issues, "i-gone")

	stop, err := Reconcile(context.Background(), ft, []string{"i-live", "i-closed", "i-gone"})
	if err != nil {
		t.Fatal(err)
	}
	if len(stop) != 2 || stop[0] != "i-closed" || stop[1] != "i-gone" {
		t.Errorf("stop = %v, want [i-closed i-gone]", stop)
	}

	if stop, err = Reconcile(context.Background(), ft, nil); err != nil || stop != nil {
		t.Errorf("empty reconcile = %v, %v; want nil, nil", stop, err)
	}
}
