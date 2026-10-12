package balancer

import (
	"time"

	"github.com/duanzhenyu/cpa-codex-weight-balancer/internal/quota"
)

// State is where an account sits in the lifecycle of doc §5.4 constraint 2.
type State uint8

const (
	// StatePending means the plugin has not seen enough consistent probes to
	// take over the credential's weight; it must leave the field alone.
	StatePending State = iota
	// StateActive means the weight tracks the quantized quota rate.
	StateActive
	// StateDraining means the rate saturated at the minimum positive weight:
	// the account still serves, but takes the smallest cold-binding share.
	StateDraining
	// StateUnavailable means a window is exhausted or the account tripped an
	// account-level 429. Weight 0 evicts sticky bindings on purpose — the
	// account cannot serve them — and lifts automatically at RecoverAt.
	StateUnavailable
)

// Account is the per-credential state the plugin persists across restarts.
type Account struct {
	State     State
	Bucket    Bucket
	Weight    int64
	OKStreak  int
	LastGood  time.Time
	RecoverAt time.Time
	Stale     bool
	// ErrorRate is the share of the account's requests within the error window
	// that failed for reasons of its own. Only Engine.Fold moves it.
	ErrorRate float64
	// Errors are the window's tallies, oldest first. Fold replaces the slice
	// rather than editing it: Accounts hands out shallow copies.
	Errors []Sample
}

// Sample is one tick's outcomes, stamped with the tick that folded them.
type Sample struct {
	At time.Time
	Outcomes
}

// Observation is one probe outcome for one account.
type Observation struct {
	Snapshot      quota.Snapshot
	Err           error
	CircuitBroken bool
	// Disk is what the credential file held when it was read this cycle. The
	// engine's own record of what it wrote is not authority: anything else may
	// have rewritten the file since — a re-login, the management UI, another
	// plugin — and a weight that vanished there is a credential the host is
	// routing at its default of 1.
	Disk DiskWeight
	// Resets are the account's banked reset credits, each one full window in
	// the snapshot's unit, due by its expiry. They come from their own endpoint,
	// not from the snapshot's reading.
	Resets []quota.Window
}

// Outcomes counts requests by result. Failures holds only those that reflect on
// the credential, and Requests includes them.
type Outcomes struct {
	Requests int
	Failures int
}

// DiskWeight is the tri-state result of looking for a weight in a credential
// file. Read separates "the file said nothing" from "the file was never
// opened"; collapsing them would rewrite the fleet whenever a read fails.
type DiskWeight struct {
	Weight  int64
	Present bool // the file carried a weight key
	Read    bool // the file was read this cycle
}

// Policy drives the state machine.
type Policy struct {
	Quantizer Quantizer
	Params    Params
	// Warmup is the number of consecutive usable probes required before the
	// plugin starts writing weights.
	Warmup int
	// Freshness bounds how long a frozen weight is trusted after probes fail.
	Freshness time.Duration
	// ErrorPenalty discounts the score by the account's error rate.
	ErrorPenalty bool
	// ErrorWindow is how far back the error rate looks. A penalized account
	// draws less traffic and so less evidence; its errors leaving the window is
	// what lets it earn its weight back.
	ErrorWindow time.Duration
}

const (
	DefaultWarmup      = 2
	DefaultFreshness   = 90 * time.Minute
	DefaultErrorWindow = 12 * time.Hour
)

// errorPrior is the fewest requests the window's error rate is taken over, so a
// single failure on an idle account does not read as all of its traffic.
const errorPrior = 10

// DefaultPolicy returns the documented starting configuration.
func DefaultPolicy() Policy {
	return Policy{
		Quantizer:   DefaultQuantizer(),
		Params:      Params{TFloor: DefaultTFloor, Alpha: DefaultAlpha},
		Warmup:      DefaultWarmup,
		Freshness:   DefaultFreshness,
		ErrorWindow: DefaultErrorWindow,
	}
}

// Step advances one account by one observation.
func (p Policy) Step(prev Account, obs Observation, now time.Time) Account {
	next, _, _ := p.evaluate(prev, obs, now)
	return next
}

// evaluate is Step plus the rate it derived, which the engine reports.
func (p Policy) evaluate(prev Account, obs Observation, now time.Time) (Account, Rate, error) {
	if obs.Err != nil {
		return p.freeze(prev, now), Rate{}, obs.Err
	}
	rate, err := Compute(obs.Snapshot, now, p.Params)
	if err != nil {
		// A snapshot whose window already rolled over carries quota from the
		// previous window. Freezing is the only safe move: repricing it would
		// clamp a negative time-to-reset onto TFloor and manufacture urgency
		// that never decays. See doc §5.3 "陈旧快照安全阀".
		return p.freeze(prev, now), Rate{}, err
	}

	next := prev
	next.OKStreak = prev.OKStreak + 1
	next.LastGood = now
	next.Stale = false

	if next.OKStreak < p.Warmup {
		next.State = StatePending
		return next, rate, nil
	}
	if obs.CircuitBroken || rate.Blocked || rate.Score <= 0 {
		next.State = StateUnavailable
		next.Weight = 0
		next.RecoverAt = recoverAt(obs.Snapshot, now)
		return next, rate, nil
	}

	next.RecoverAt = time.Time{}
	score := rate.Score
	// Added only past the exclusion: a credit is quota the account holds once
	// someone redeems it, and until then a spent window still answers 429.
	for _, credit := range obs.Resets {
		if s, ok := WindowScore(credit, now, p.Params); ok {
			score += s
		}
	}
	if p.ErrorPenalty {
		score *= 1 - next.ErrorRate
	}
	if score > 0 {
		next.Bucket, _ = p.Quantizer.Commit(prev.Bucket, score)
	} else {
		// The discount took the whole score, or underflowed it. The quantizer
		// ignores a zero score, which would leave an account that fails every
		// request at the weight it held — or at the anchor, if a reload has just
		// cleared its bucket.
		lo, _ := p.Quantizer.Bounds()
		next.Bucket = Bucket{Index: lo, Set: true}
	}
	next.Weight = p.Quantizer.Weight(next.Bucket.Index)
	next.State = StateActive
	if next.Weight <= p.Quantizer.MinWeight {
		next.State = StateDraining
	}
	return next, rate, nil
}

func errorRate(window []Sample) float64 {
	var sum Outcomes
	for _, s := range window {
		sum.Requests += s.Requests
		sum.Failures += s.Failures
	}
	return float64(sum.Failures) / float64(max(sum.Requests, errorPrior))
}

// freeze holds the last committed weight. A transient probe failure must never
// zero a credential: that would evict its sticky sessions fleet-wide.
//
// An exclusion is the exception. It was granted against a reset that has now
// passed, so holding it would strand the account at weight 0 for as long as the
// probe stays down. Re-admission is at the minimum weight: without a fresh
// snapshot there is nothing to price, and the next good probe corrects it.
func (p Policy) freeze(a Account, now time.Time) Account {
	a.OKStreak = 0
	if !a.LastGood.IsZero() && now.Sub(a.LastGood) > p.Freshness {
		a.Stale = true
	}
	if a.State == StateUnavailable && !a.RecoverAt.IsZero() && !now.Before(a.RecoverAt) {
		a.State = StateDraining
		a.Weight = p.Quantizer.MinWeight
		a.RecoverAt = time.Time{}
	}
	return a
}

// recoverAt is the reset that could lift the exclusion: the latest reset among
// the windows actually drained, since every drained window must roll over
// before the account can serve again. When no window is drained the account was
// excluded by the circuit breaker, and the next boundary of any kind will do.
func recoverAt(s quota.Snapshot, now time.Time) time.Time {
	var at time.Time
	for _, w := range [...]quota.Window{s.Weekly, s.FiveHour} {
		if w.Remaining > 0 || w.Expired(now) {
			continue
		}
		if at.IsZero() || w.ResetAt.After(at) {
			at = w.ResetAt
		}
	}
	if at.IsZero() {
		// Neither window is drained, so fall back to the soonest boundary that
		// exists at all. A missing window is the zero time, which would
		// otherwise win this comparison and never arrive.
		for _, w := range [...]quota.Window{s.Weekly, s.FiveHour} {
			if w.ResetAt.IsZero() {
				continue
			}
			if at.IsZero() || w.ResetAt.Before(at) {
				at = w.ResetAt
			}
		}
	}
	return at
}
