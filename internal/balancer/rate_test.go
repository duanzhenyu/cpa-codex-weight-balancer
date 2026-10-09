package balancer

import (
	"math"
	"testing"
	"time"

	"github.com/duanzhenyu/cpa-codex-weight-balancer/internal/quota"
)

var now = time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)

func win(rem float64, in time.Duration) quota.Window {
	return quota.Window{Remaining: rem, Absolute: true, ResetAt: now.Add(in)}
}

func snap(weekly, five quota.Window) quota.Snapshot {
	return quota.Snapshot{AuthID: "a", Generation: 1, ObservedAt: now, Weekly: weekly, FiveHour: five}
}

func near(t *testing.T, got, want float64) {
	t.Helper()
	if !(math.Abs(got-want) <= math.Abs(want)*1e-9+1e-12) {
		t.Fatalf("got %g, want %g", got, want)
	}
}

func TestWindowScoreBasic(t *testing.T) {
	got, ok := WindowScore(win(100, time.Hour), now, Params{})
	if !ok {
		t.Fatal("want usable")
	}
	near(t, got, 100.0/3600)
}

func TestWindowScoreTFloorClamp(t *testing.T) {
	// 1 min to reset must be clamped to the 15 min floor, not diverge.
	got, _ := WindowScore(win(100, time.Minute), now, Params{})
	near(t, got, 100.0/900)
}

func TestWindowScoreExpiredIsUnusable(t *testing.T) {
	// Crossed reset: Remaining is from the previous window. Must never be
	// clamped to TFloor and reused as fake urgency (doc §5.3 safety valve).
	if _, ok := WindowScore(win(100, -time.Second), now, Params{}); ok {
		t.Fatal("expired window must be unusable")
	}
}

func TestWindowScoreAlpha(t *testing.T) {
	near2 := func(a float64, want float64) {
		t.Helper()
		got, _ := WindowScore(win(100, time.Hour), now, Params{Alpha: a})
		near(t, got, want)
	}
	near2(math.SmallestNonzeroFloat64, 100) // alpha->0 degenerates to pure remaining
	near2(2, 100.0/(3600*3600))
}

func TestWindowScorePercentIsScorable(t *testing.T) {
	// Codex only ever reports used_percent, so a percentage window must score.
	// Comparability across plan tiers is a separate concern, flagged by Degraded.
	w := win(50, time.Hour)
	w.Absolute = false
	got, ok := WindowScore(w, now, Params{})
	if !ok {
		t.Fatal("percentage window must be scorable")
	}
	near(t, got, 50.0/3600)
}

func TestWindowScoreRejectsBadRemaining(t *testing.T) {
	for _, rem := range []float64{-1, math.NaN(), math.Inf(1)} {
		if _, ok := WindowScore(win(rem, time.Hour), now, Params{}); ok {
			t.Fatalf("remaining %v must be unusable", rem)
		}
	}
}

func TestWindowScoreZeroRemaining(t *testing.T) {
	got, ok := WindowScore(win(0, time.Hour), now, Params{})
	if !ok || got != 0 {
		t.Fatalf("got %g ok=%v, want 0 usable", got, ok)
	}
}

func TestComputeWeeklyDrivesTheScore(t *testing.T) {
	// The two windows are percentages of different budgets, so they cannot be
	// combined. Weekly sets the pace; the short window never lowers the score.
	r, err := Compute(snap(win(100, time.Hour), win(10, 4*time.Hour)), now, Params{})
	if err != nil {
		t.Fatal(err)
	}
	if r.Blocked || r.Degraded {
		t.Fatalf("got %+v, want a clean score", r)
	}
	near(t, r.Score, 100.0/3600)
}

func TestComputeBlockedWhenShortWindowSpent(t *testing.T) {
	// Weekly quota is untouched but the 5h bucket is empty: the account cannot
	// serve until its own short reset, whatever the weekly figure says.
	r, err := Compute(snap(win(500, 7*24*time.Hour), win(0, 2*time.Hour)), now, Params{})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Blocked {
		t.Fatalf("got %+v, want Blocked", r)
	}
	if r.Score <= 0 {
		t.Fatal("the weekly score stays positive; blocking is a separate signal")
	}
}

func TestComputeExpiredShortWindowDoesNotBlock(t *testing.T) {
	// A rolled-over short window carries no information about right now.
	r, err := Compute(snap(win(100, time.Hour), win(0, -time.Minute)), now, Params{})
	if err != nil {
		t.Fatal(err)
	}
	if r.Blocked {
		t.Fatal("an expired short window must not block the account")
	}
	near(t, r.Score, 100.0/3600)
}

func TestComputeDegradedOnBarePercentage(t *testing.T) {
	s := snap(win(60, 7*24*time.Hour), win(40, 3*time.Hour))
	s.Weekly.Absolute = false
	r, err := Compute(s, now, Params{})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Degraded {
		t.Fatal("a bare percentage is not comparable across plan tiers")
	}
}

func TestComputeRejectsExpiredWeekly(t *testing.T) {
	if _, err := Compute(snap(win(100, -time.Minute), win(10, time.Hour)), now, Params{}); err == nil {
		t.Fatal("expired weekly window must invalidate the snapshot")
	}
}

func TestComputeRejectsSnapshotFromTheFuture(t *testing.T) {
	s := snap(win(100, time.Hour), win(10, 2*time.Hour))
	s.ObservedAt = now.Add(time.Minute)
	if _, err := Compute(s, now, Params{}); err == nil {
		t.Fatal("ObservedAt after now indicates clock skew; must be rejected")
	}
}

func TestNonFiniteScoreIsRejectedNotPriced(t *testing.T) {
	// A NaN alpha slips past every "<= 0" guard, and a NaN score is not <= 0
	// either — so without this the account is priced Active on a bucket that
	// has nothing to do with its quota.
	s := snap(win(500, 7*24*time.Hour), win(50, 3*time.Hour))
	for name, alpha := range map[string]float64{
		"nan":       math.NaN(),
		"+inf":      math.Inf(1),
		"overflows": 1e308,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Compute(s, now, Params{TFloor: DefaultTFloor, Alpha: alpha}); err == nil {
				t.Fatal("want an error, got a usable score")
			}
		})
	}
}
