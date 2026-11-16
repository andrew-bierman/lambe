package pi

import (
	"regexp"
	"sync"
	"time"
)

// limitPattern is one row of the heuristic limit-classification table.
type limitPattern struct {
	re     *regexp.Regexp
	weight float64
}

// defaultLimitPatterns is the phrase table for provider limit errors,
// matched case-insensitively against error text. Weights are the base
// confidence contribution of a match.
var defaultLimitPatterns = []limitPattern{
	{regexp.MustCompile(`(?i)rate.?limit`), 0.9},
	{regexp.MustCompile(`(?i)\bquota\b`), 0.8},
	{regexp.MustCompile(`(?i)\b429\b`), 0.8},
	{regexp.MustCompile(`(?i)usage limit`), 0.9},
	{regexp.MustCompile(`(?i)\bcapacity\b`), 0.6},
	{regexp.MustCompile(`(?i)\bcredits?\b`), 0.7},
	{regexp.MustCompile(`(?i)\bbilling\b`), 0.7},
}

// LimitClassifier heuristically classifies a stream of error observations
// into a suspected provider limit: a pattern-table match on the error text
// plus repeated failures with no intervening progress. Pi reports errors as
// free-form strings, so the classification carries a confidence rather
// than a proof.
type LimitClassifier struct {
	mu             sync.Mutex
	horizon        time.Duration
	minConsecutive int
	patterns       []limitPattern
	consecutive    int
	bestWeight     float64
	evidence       []string
}

// NewLimitClassifier returns a classifier suggesting wake-at times one
// horizon ahead. minConsecutive defaults to 2.
func NewLimitClassifier(horizon time.Duration) *LimitClassifier {
	return &LimitClassifier{
		horizon:        horizon,
		minConsecutive: 2,
		patterns:       defaultLimitPatterns,
	}
}

// Reset clears all accumulated evidence, e.g. at the start of a turn.
func (c *LimitClassifier) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.consecutive = 0
	c.bestWeight = 0
	c.evidence = nil
}

// Progress records a non-error advance (a successful message, a completed
// tool call): it clears the no-progress error streak.
func (c *LimitClassifier) Progress() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.consecutive = 0
	c.bestWeight = 0
	c.evidence = nil
}

// ObserveError feeds one error observation and reports whether a limit is
// suspected now.
func (c *LimitClassifier) ObserveError(text string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.consecutive++
	for _, p := range c.patterns {
		if p.re.MatchString(text) {
			if p.weight > c.bestWeight {
				c.bestWeight = p.weight
			}
		}
	}
	if len(c.evidence) < 8 {
		c.evidence = append(c.evidence, text)
	}
	return c.suspectedLocked()
}

// Suspicion returns the current suspicion: confidence in [0,1] (zero when
// not suspected) and the accumulated error evidence.
func (c *LimitClassifier) Suspicion() (float64, []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.suspectedLocked() {
		return 0, nil
	}
	return c.confidenceLocked(), append([]string(nil), c.evidence...)
}

// WakeHorizon returns the configured wake-at horizon.
func (c *LimitClassifier) WakeHorizon() time.Duration { return c.horizon }

// suspectedLocked reports whether the pattern table matched within a
// no-progress error streak long enough to suspect a limit. Caller holds mu.
func (c *LimitClassifier) suspectedLocked() bool {
	return c.bestWeight > 0 && c.consecutive >= c.minConsecutive
}

// confidenceLocked blends the strongest pattern weight with the streak
// length. Caller holds mu.
func (c *LimitClassifier) confidenceLocked() float64 {
	conf := c.bestWeight*0.6 + 0.15*float64(c.consecutive-1)
	if conf > 0.95 {
		conf = 0.95
	}
	return conf
}
