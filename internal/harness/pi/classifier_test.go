package pi

import (
	"testing"
	"time"
)

// TestLimitClassifierTable exercises the pattern table and the
// no-progress-streak requirement. The pseudo-error "progress" feeds
// [LimitClassifier.Progress] instead of an error observation.
func TestLimitClassifierTable(t *testing.T) {
	tests := []struct {
		name         string
		observations []string
		wantSuspect  bool
	}{
		{
			name:         "single rate limit error is not enough",
			observations: []string{"rate limit exceeded"},
			wantSuspect:  false,
		},
		{
			name:         "repeated rate limit errors with no progress",
			observations: []string{"rate limit exceeded", "still rate limited"},
			wantSuspect:  true,
		},
		{
			name:         "quota phrase matches",
			observations: []string{"quota exceeded", "quota exceeded again"},
			wantSuspect:  true,
		},
		{
			name:         "429 matches",
			observations: []string{"HTTP 429", "HTTP 429"},
			wantSuspect:  true,
		},
		{
			name:         "usage limit matches",
			observations: []string{"usage limit reached", "usage limit reached"},
			wantSuspect:  true,
		},
		{
			name:         "capacity phrase matches",
			observations: []string{"provider at capacity", "provider at capacity"},
			wantSuspect:  true,
		},
		{
			name:         "unrelated errors never match",
			observations: []string{"boom", "boom again"},
			wantSuspect:  false,
		},
		{
			name:         "progress clears the streak",
			observations: []string{"rate limit exceeded", "progress", "rate limit exceeded"},
			wantSuspect:  false,
		},
		{
			name:         "errors after progress reaccumulate",
			observations: []string{"rate limit exceeded", "progress", "rate limit", "rate limit"},
			wantSuspect:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := NewLimitClassifier(time.Hour)
			for _, obs := range tt.observations {
				if obs == "progress" {
					c.Progress()
					continue
				}
				c.ObserveError(obs)
			}
			conf, evidence := c.Suspicion()
			if (conf > 0) != tt.wantSuspect {
				t.Fatalf("suspected = %v (confidence %v), want %v", conf > 0, conf, tt.wantSuspect)
			}
			if tt.wantSuspect {
				if conf <= 0 || conf > 1 {
					t.Fatalf("confidence %v outside (0,1]", conf)
				}
				if len(evidence) == 0 {
					t.Fatal("suspected classification carries no evidence")
				}
			} else if conf != 0 {
				t.Fatalf("confidence %v on non-suspected classification", conf)
			}
		})
	}
}

// TestLimitClassifierReset verifies Reset clears stale evidence between
// turns.
func TestLimitClassifierReset(t *testing.T) {
	c := NewLimitClassifier(time.Hour)
	c.ObserveError("rate limit")
	c.ObserveError("rate limit")
	if conf, _ := c.Suspicion(); conf == 0 {
		t.Fatal("expected suspicion before reset")
	}
	c.Reset()
	if conf, evidence := c.Suspicion(); conf != 0 || evidence != nil {
		t.Fatalf("after reset: confidence %v evidence %v, want 0/nil", conf, evidence)
	}
}
