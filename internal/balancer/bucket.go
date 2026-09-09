package balancer

import "math"

// Bucket is an account's committed logarithmic quota-rate bucket. A zero value
// means "never committed"; see doc §5.4 constraint 1 — the bucket must be
// persisted so an account whose rate did not cross a boundary keeps a
// byte-identical weight across restarts.
type Bucket struct {
	Index int
	Set   bool
}

// Quantizer maps a continuous rate onto a coarse logarithmic weight ladder.
type Quantizer struct {
	Q         float64 // relative width of one bucket, e.g. 0.08
	R0        float64 // rate anchor
	W0        float64 // weight at bucket 0
	Margin    float64 // Schmitt overshoot, in buckets, required before committing
	MinWeight int64
	MaxWeight int64
}

const (
	DefaultQ      = 0.08
	DefaultMargin = 0.35
	// DefaultR0 anchors bucket 0 at the rate of an account pacing to consume
	// exactly one full weekly window, expressed in the same unit as Remaining
	// (100 units per 7 days). Anchor placement is not free: put it far from the
	// fleet's real rates and every account saturates onto one weight. Recalibrate
	// from measured quantiles — see doc §5.4 constraint 1 point 4.
	DefaultR0        = 100.0 / (7 * 24 * 60 * 60)
	DefaultW0        = 1000.0
	DefaultMinWeight = 1
	DefaultMaxWeight = 1_000_000
)

// DefaultQuantizer returns the calibration starting point documented in §5.4.
// Q and Margin are placeholders pending measurement of probe noise and the
// per-cycle cold-binding count.
func DefaultQuantizer() Quantizer {
	return Quantizer{
		Q: DefaultQ, R0: DefaultR0, W0: DefaultW0, Margin: DefaultMargin,
		MinWeight: DefaultMinWeight, MaxWeight: DefaultMaxWeight,
	}
}

// Position is the real-valued bucket coordinate of rate.
func (q Quantizer) Position(rate float64) (float64, bool) {
	if !(rate > 0) || math.IsInf(rate, 0) {
		return 0, false
	}
	return math.Log(rate/q.R0) / math.Log1p(q.Q), true
}

// Bounds returns the widest bucket range whose weights stay inside
// [MinWeight, MaxWeight]. Outside it Weight saturates, which collapses distinct
// rates onto one weight — see doc §5.4 constraint 1 point 4.
func (q Quantizer) Bounds() (int, int) {
	step := math.Log1p(q.Q)
	lo := int(math.Ceil(math.Log(float64(q.MinWeight)/q.W0) / step))
	hi := int(math.Floor(math.Log(float64(q.MaxWeight)/q.W0) / step))
	return lo, hi
}

func (q Quantizer) clamp(b int) int {
	lo, hi := q.Bounds()
	switch {
	case b < lo:
		return lo
	case b > hi:
		return hi
	}
	return b
}

// Weight maps a bucket index onto an integer weight, saturating at the bounds.
func (q Quantizer) Weight(b int) int64 {
	lo, hi := q.Bounds()
	switch {
	case b < lo:
		return q.MinWeight
	case b > hi:
		return q.MaxWeight
	}
	w := int64(math.Round(q.W0 * math.Pow(1+q.Q, float64(b))))
	return min(max(w, q.MinWeight), q.MaxWeight)
}

// Commit applies Schmitt hysteresis: the bucket moves only once rate leaves the
// retained bucket by more than Margin.
func (q Quantizer) Commit(prev Bucket, rate float64) (Bucket, bool) {
	x, ok := q.Position(rate)
	if !ok {
		return prev, false
	}
	next := q.clamp(int(math.Floor(x)))
	if !prev.Set {
		return Bucket{Index: next, Set: true}, true
	}
	held := float64(prev.Index)
	if x >= held-q.Margin && x < held+1+q.Margin {
		return prev, false
	}
	if next == prev.Index {
		return prev, false
	}
	return Bucket{Index: next, Set: true}, true
}
