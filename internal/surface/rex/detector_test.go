package rex

import (
	"context"
	"errors"
	"testing"
	"time"
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
			name:      "REX_SERVER is not Rex evidence, the probe decides",
			env:       map[string]string{"REX_SERVER": "sock"},
			wantAvail: true,
			wantWhy:   ReasonProbe,
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
			name:     "probe exit 4 with exit error means rex absent",
			exitCode: 4,
			runErr:   errors.New("exit status 4"),
			wantWhy:  ReasonAbsent,
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
			if probed && tt.wantWhy == ReasonEnv {
				t.Error("probe ran despite env indicating Rex")
			}
		})
	}
}

func TestDetectTimesOutHungProbe(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name          string
		callerTimeout time.Duration
	}{
		{name: "default timeout"},
		{name: "shorter caller timeout", callerTimeout: 20 * time.Millisecond},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			if tt.callerTimeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tt.callerTimeout)
				defer cancel()
			}
			d := Detector{Runner: func(probeCtx context.Context, _ string, _ ...string) (string, int, error) {
				deadline, ok := probeCtx.Deadline()
				if !ok || time.Until(deadline) > probeTimeout {
					t.Fatal("probe has no bounded deadline")
				}
				if callerDeadline, ok := ctx.Deadline(); ok && !deadline.Equal(callerDeadline) {
					t.Fatal("probe did not inherit the earlier caller deadline")
				}
				<-probeCtx.Done()
				return "", -1, probeCtx.Err()
			}}
			if got := d.Detect(ctx); got != (Availability{Reason: ReasonProbeFailed}) {
				t.Errorf("Detect() = %+v, want unavailable after timeout", got)
			}
		})
	}
}

func TestDetectProbeNeverAutostartsAServer(t *testing.T) {
	var got []string
	d := Detector{Runner: func(_ context.Context, name string, args ...string) (string, int, error) {
		got = append([]string{name}, args...)
		return "", 0, nil
	}}
	d.Detect(context.Background())
	want := []string{"rex", "--autostart=false", "ls", "--json"}
	if len(got) != len(want) {
		t.Fatalf("probe argv = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("probe argv = %q, want %q", got, want)
		}
	}
}
