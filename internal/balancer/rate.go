package balancer

import (
	"fmt"
	"math"
	"time"

	"github.com/duanzhenyu/cpa-codex-weight-balancer/internal/quota"
)

// Params tunes the urgency score. See doc §5.3.
type Params struct {
	TFloor time.Duration // lower clamp on time-to-reset; 0 means DefaultTFloor
	Alpha  float64       // urgency exponent; 0 means DefaultAlpha (=1)
}

const (
	DefaultTFloor = 15 * time.Minute
	DefaultAlpha  = 1.0
)

// Rate is the urgency score of one snapshot, in quota units per second when
// Alpha == 1.
//
// The score comes from the long (weekly) window alone. Codex reports each
// window as a bare used_percent with no plan capacity, so a weekly percentage
// and a 5h percentage are different units and cannot be combined — the
// min(weekly, 5h) gate the design originally called for is not computable on
// the real payload. The short window instead acts as a pure availability gate.
type Rate struct {
	Score float64
	// Blocked reports that the short window is spent, so the account cannot
	// serve right now however much weekly quota it still holds.
	Blocked bool
	// Degraded reports that the window is a bare percentage rather than an
	// absolute quantity. Scaling it does not make plan tiers comparable — that
	// needs per-account capacity — it only puts the score in a known unit.
	Degraded bool
}

// WindowScore returns remaining / max(timeToReset, TFloor)^Alpha.
func WindowScore(w quota.Window, now time.Time, p Params) (float64, bool) {
	if !w.Usable(now) {
		return 0, false
	}
	floor, alpha := p.TFloor, p.Alpha
	if floor <= 0 {
		floor = DefaultTFloor
	}
	if alpha <= 0 {
		alpha = DefaultAlpha
	}
	left := w.ResetAt.Sub(now)
	if left < floor {
		left = floor
	}
	score := w.Remaining / math.Pow(left.Seconds(), alpha)
	// Reject a score the arithmetic destroyed rather than one the account
	// earned: a NaN alpha poisons the result, and a huge one overflows Pow so
	// that quota still in hand underflows to zero. Both would otherwise read as
	// "exhausted" and zero a healthy credential.
	if math.IsNaN(score) || math.IsInf(score, 0) || (score == 0 && w.Remaining > 0) {
		return 0, false
	}
	return score, true
}

// Compute scores a snapshot: the weekly window sets the pace, the short window
// only says whether the account can serve at all.
func Compute(s quota.Snapshot, now time.Time, p Params) (Rate, error) {
	if s.ObservedAt.After(now) {
		return Rate{}, fmt.Errorf("snapshot %s gen %d observed in the future", s.AuthID, s.Generation)
	}
	weekly, ok := WindowScore(s.Weekly, now, p)
	if !ok {
		return Rate{}, fmt.Errorf("snapshot %s gen %d: weekly window unusable", s.AuthID, s.Generation)
	}
	// A spent short window blocks the account until its own reset; an expired
	// or missing one carries no information and must not block anything.
	blocked := !s.FiveHour.Expired(now) && s.FiveHour.Remaining <= 0
	return Rate{Score: weekly, Blocked: blocked, Degraded: !s.Weekly.Absolute}, nil
}
