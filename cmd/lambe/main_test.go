package main

import (
	"errors"
	"fmt"
	"testing"

	"github.com/andrew-bierman/lambe/internal/daemon"
)

func TestExitCodeFor(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"nil", nil, 1},
		{"plain error", errors.New("boom"), 1},
		{"lock held", fmt.Errorf("wrap: %w", daemon.ErrLockHeld), 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := exitCodeFor(tt.err); got != tt.want {
				t.Errorf("exitCodeFor(%v) = %d, want %d", tt.err, got, tt.want)
			}
		})
	}
}
