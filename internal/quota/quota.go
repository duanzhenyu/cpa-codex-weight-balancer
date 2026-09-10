// Package quota models a point-in-time observation of one Codex account's
// rate-limit windows. Remaining and ResetAt of a snapshot must originate from
// the same probe response; see doc §5.3 "陈旧快照安全阀".
package quota

import (
	"math"
	"time"
)

// Window is one rate-limit window of an account.
type Window struct {
	// Remaining is the quota left in the window. Absolute reports whether it has
	// been scaled by the account's plan capacity; when false it is a bare
	// percentage, comparable between accounts only on the same window and only
	// while every account is on the same plan tier.
	Remaining float64
	Absolute  bool
	ResetAt   time.Time
}

// Snapshot is one atomic probe result for an account.
type Snapshot struct {
	AuthID     string
	Generation uint64
	ObservedAt time.Time
	Weekly     Window
	FiveHour   Window
}

// Expired reports whether the window has rolled over since it was observed,
// which makes Remaining meaningless.
func (w Window) Expired(now time.Time) bool { return !now.Before(w.ResetAt) }

// Usable reports whether the window can be scored at all. Absolute is not
// required: a bare percentage is comparable between accounts on the same
// window, just not across windows or across plan tiers.
func (w Window) Usable(now time.Time) bool {
	if w.Expired(now) {
		return false
	}
	return !math.IsInf(w.Remaining, 0) && w.Remaining >= 0
}

// Scaled resolves both windows against the plan capacity: Remaining is a
// percentage, so the absolute quantity is that fraction of the window's whole
// budget. A non-positive capacity leaves the snapshot as percentages.
//
// The capacity is one number for the whole fleet, so this only makes the
// snapshot absolute — it does not make accounts on different plans comparable.
func (s Snapshot) Scaled(capacity float64) Snapshot {
	if capacity <= 0 {
		return s
	}
	for _, w := range []*Window{&s.Weekly, &s.FiveHour} {
		scaled := w.Remaining / 100 * capacity
		// A balance the account still holds must not underflow into the value
		// that means exhausted: that writes weight 0, which the host reads as a
		// hard exclusion and answers by evicting the sticky sessions too.
		if scaled == 0 && w.Remaining > 0 {
			scaled = math.SmallestNonzeroFloat64
		}
		w.Remaining = scaled
		w.Absolute = true
	}
	return s
}
