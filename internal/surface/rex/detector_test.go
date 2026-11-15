package rex

import (
	"context"
	"errors"
	"testing"
)

func TestDetect(t *testing.T) {
	tests := []struct {
		name      string
		env       map[string]string
		exitCode  int
		runErr    error
		wantAvail bool
		wantWhy   Reason
	}{
		{
			name:      "rex session env present",
			env:       map[string]string{"REX_SESSION": "s-1"},
			wantAvail: true,
			wantWhy:   ReasonEnv,
		},
		{
			name:      "rex server env present",
			env:       map[string]string{"REX_SERVER": "sock"},
			wantAvail: true,
			wantWhy:   ReasonEnv,
		},
		{
			name:      "probe exit 0",
			exitCode:  0,
			wantAvail: true,
			wantWhy:   ReasonProbe,
		},
		{
			name:      "probe exit 4 means rex absent",
			exitCode:  4,
			wantAvail: false,
			wantWhy:   ReasonAbsent,
		},
		{
			name:      "probe other exit is inconclusive unavailable",
			exitCode:  2,
			wantAvail: false,
			wantWhy:   ReasonProbeFailed,
		},
		{
			name:      "probe exec error",
			runErr:    errors.New("no such file"),
			wantAvail: false,
			wantWhy:   ReasonProbeFailed,
		},
		{
			name:      "env wins without probe",
			env:       map[string]string{"REX_SESSION": "s-1"},
			exitCode:  4,
			wantAvail: true,
			wantWhy:   ReasonEnv,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			probed := false
			d := Detector{
				Env: func(k string) string { return tt.env[k] },
				Runner: func(_ context.Context, name string, _ ...string) (string, int, error) {
					probed = true
					if name != "rex" {
						t.Errorf("probe command = %q, want rex", name)
					}
					return "{}", tt.exitCode, tt.runErr
				},
			}
			got := d.Detect(context.Background())
			if got.Available != tt.wantAvail {
				t.Errorf("Available = %v, want %v", got.Available, tt.wantAvail)
			}
			if got.Reason != tt.wantWhy {
				t.Errorf("Reason = %q, want %q", got.Reason, tt.wantWhy)
			}
			if probed && len(tt.env) > 0 {
				t.Error("probe ran despite env indicating Rex")
			}
		})
	}
}
