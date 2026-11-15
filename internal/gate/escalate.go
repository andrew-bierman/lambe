package gate

import (
	"context"
	"strconv"
)

// Escalation defaults: the attempt cap matches the config default, and
// both escalation thresholds fire after five consecutive occurrences.
// These mirror the policy model in test/faultsuite (attempt cap 8,
// escalation after five consecutive deferrals, a decided gate resets the
// streak); the gate's persisted counters are the tracker-backed
// counterpart of that local model and must keep the same shape.
const (
	// DefaultAttemptCap mirrors the supervisor config's attempt cap.
	DefaultAttemptCap = 8
	// DefaultEscalationThreshold is the consecutive gate-error and
	// gate-deferral count at which an issue escalates to needs-human.
	DefaultEscalationThreshold = 5
)

// KV persists one escalation-counter value per issue and key. The local
// tracker's issue metadata implements it, so counters survive restarts
// and stay readable through bd.
type KV interface {
	// Get returns the stored value, or "" when absent.
	Get(ctx context.Context, issue, key string) (string, error)
	// Put stores the value.
	Put(ctx context.Context, issue, key, value string) error
}

// Metadata keys lambe stamps on issues for gate escalation state.
const (
	// MetaGateErrors counts consecutive gate-infra errors.
	MetaGateErrors = "lambe_gate_errors"
	// MetaGateDeferrals counts consecutive pending-condition deferrals.
	MetaGateDeferrals = "lambe_gate_deferrals"
)

// Escalator owns the attempt cap and the two consecutive-occurrence
// counters. A decided gate (red or green) resets both counters; only
// undecided gates (infra errors, pending CI) accumulate them.
type Escalator struct {
	// KV persists the counters; nil disables persistence.
	KV KV
	// AttemptCap is the red-attempt count at which an issue escalates to
	// needs-human.
	AttemptCap int
	// ErrorThreshold is the consecutive gate-error count that escalates.
	ErrorThreshold int
	// DeferThreshold is the consecutive deferral count that escalates.
	DeferThreshold int
}

// NewEscalator returns an Escalator with the default caps and thresholds.
func NewEscalator(kv KV) *Escalator {
	return &Escalator{
		KV:             kv,
		AttemptCap:     DefaultAttemptCap,
		ErrorThreshold: DefaultEscalationThreshold,
		DeferThreshold: DefaultEscalationThreshold,
	}
}

// CapReached reports whether the attempt number has reached the attempt
// cap, the point at which a red gate escalates to needs-human.
func (e *Escalator) CapReached(attempt int) bool {
	limit := e.AttemptCap
	if limit <= 0 {
		limit = DefaultAttemptCap
	}
	return attempt >= limit
}

// RecordGateError increments the consecutive gate-error counter and
// reports whether the threshold was reached.
func (e *Escalator) RecordGateError(ctx context.Context, issue string) (bool, error) {
	return e.record(ctx, issue, MetaGateErrors, e.threshold())
}

// RecordDeferral increments the consecutive gate-deferral counter and
// reports whether the threshold was reached.
func (e *Escalator) RecordDeferral(ctx context.Context, issue string) (bool, error) {
	return e.record(ctx, issue, MetaGateDeferrals, e.threshold())
}

// ResetCounters zeroes both counters after a decided gate.
func (e *Escalator) ResetCounters(ctx context.Context, issue string) error {
	if e.KV == nil {
		return nil
	}
	for _, key := range []string{MetaGateErrors, MetaGateDeferrals} {
		if err := e.KV.Put(ctx, issue, key, "0"); err != nil {
			return err
		}
	}
	return nil
}

// threshold resolves the configured escalation threshold with its default.
func (e *Escalator) threshold() int {
	if e.ErrorThreshold > 0 {
		return e.ErrorThreshold
	}
	return DefaultEscalationThreshold
}

// record loads, increments, stores, and compares one counter.
func (e *Escalator) record(ctx context.Context, issue, key string, threshold int) (bool, error) {
	if e.KV == nil {
		return false, nil
	}
	raw, err := e.KV.Get(ctx, issue, key)
	if err != nil {
		return false, err
	}
	n := 0
	if raw != "" {
		if parsed, perr := strconv.Atoi(raw); perr == nil {
			n = parsed
		}
	}
	n++
	if err := e.KV.Put(ctx, issue, key, strconv.Itoa(n)); err != nil {
		return false, err
	}
	return n >= threshold, nil
}
