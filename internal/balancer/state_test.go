package balancer

import (
	"errors"
	"testing"
	"time"

	"github.com/duanzhenyu/cpa-codex-weight-balancer/internal/quota"
)

func pol() Policy { return DefaultPolicy() }

func obsOK(weekly, five quota.Window) Observation {
	return Observation{Snapshot: snap(weekly, five)}
}

// warm drives an account through warmup with a healthy observation.
func warm(t *testing.T, p Policy, o Observation) Account {
	t.Helper()
	var a Account
	for range p.Warmup {
		a = p.Step(a, o, now)
	}
	return a
}

func TestDefaultPolicy(t *testing.T) {
	p := pol()
	if p.Warmup != DefaultWarmup || p.Freshness != DefaultFreshness || p.Quantizer.Q != DefaultQ {
		t.Fatalf("defaults not applied: %+v", p)
	}
}

func TestWarmupWithholdsWeight(t *testing.T) {
	p := pol()
	o := obsOK(win(500, 7*24*time.Hour), win(50, 3*time.Hour))
	a := p.Step(Account{}, o, now)
	if a.State != StatePending {
		t.Fatalf("first probe must stay pending, got %v", a.State)
	}
	if a.OKStreak != 1 {
		t.Fatalf("streak %d, want 1", a.OKStreak)
	}
	a = p.Step(a, o, now)
	if a.State != StateActive {
		t.Fatalf("after warmup want active, got %v", a.State)
	}
	if a.Weight <= p.Quantizer.MinWeight {
		t.Fatalf("weight %d, want a healthy positive weight", a.Weight)
	}
}

func TestActiveWeightTracksBucket(t *testing.T) {
	p := pol()
	a := warm(t, p, obsOK(win(500, 7*24*time.Hour), win(50, 3*time.Hour)))
	if !a.Bucket.Set || a.Weight != p.Quantizer.Weight(a.Bucket.Index) {
		t.Fatalf("weight %d does not match bucket %+v", a.Weight, a.Bucket)
	}
}

func TestDrainingWhenRateSaturatesLow(t *testing.T) {
	p := pol()
	// A sliver of quota against a week-long window saturates at MinWeight.
	a := warm(t, p, obsOK(win(1e-9, 7*24*time.Hour), win(1e-9, 4*time.Hour)))
	if a.State != StateDraining {
		t.Fatalf("got %v, want draining", a.State)
	}
	if a.Weight != p.Quantizer.MinWeight {
		t.Fatalf("weight %d, want %d", a.Weight, p.Quantizer.MinWeight)
	}
}

func TestExhaustedWindowGoesUnavailable(t *testing.T) {
	p := pol()
	// 5h bucket empty, weekly still full: cannot serve now, recovers at the
	// 5h reset, not at the weekly one.
	fiveReset := 2 * time.Hour
	a := warm(t, p, obsOK(win(500, 7*24*time.Hour), win(0, fiveReset)))
	if a.State != StateUnavailable {
		t.Fatalf("got %v, want unavailable", a.State)
	}
	if a.Weight != 0 {
		t.Fatalf("weight %d, want 0", a.Weight)
	}
	if !a.RecoverAt.Equal(now.Add(fiveReset)) {
		t.Fatalf("RecoverAt %v, want the 5h reset %v", a.RecoverAt, now.Add(fiveReset))
	}
}

// With both windows drained the account cannot serve until the *last* of them
// resets. Recovering at the soonest one re-admits an account whose weekly quota
// is still empty, and while the probe stays down nothing corrects that.
func TestBothWindowsDrainedRecoverAtTheLaterReset(t *testing.T) {
	p := pol()
	fiveReset, weeklyReset := 2*time.Hour, 40*time.Hour
	a := warm(t, p, obsOK(win(0, weeklyReset), win(0, fiveReset)))
	if a.State != StateUnavailable {
		t.Fatalf("got %v, want unavailable", a.State)
	}
	if !a.RecoverAt.Equal(now.Add(weeklyReset)) {
		t.Fatalf("RecoverAt %v, want the weekly reset %v", a.RecoverAt, now.Add(weeklyReset))
	}
}

// The exclusion must still be held past the earlier window's reset.
func TestBothWindowsDrainedStayExcludedAfterTheShortReset(t *testing.T) {
	p := pol()
	a := warm(t, p, obsOK(win(0, 40*time.Hour), win(0, 2*time.Hour)))
	down := Observation{Snapshot: quota.Snapshot{AuthID: "a", Generation: 2}, Err: errors.New("probe failed")}
	held := p.Step(a, down, now.Add(3*time.Hour))
	if held.Weight != 0 || held.State != StateUnavailable {
		t.Fatalf("weight %d state %v after the 5h reset, want the exclusion held", held.Weight, held.State)
	}
}

func TestUnavailableRecoversAfterReset(t *testing.T) {
	p := pol()
	a := warm(t, p, obsOK(win(500, 7*24*time.Hour), win(0, 2*time.Hour)))
	later := now.Add(3 * time.Hour)
	healthy := Observation{Snapshot: quota.Snapshot{
		AuthID: "a", Generation: 2, ObservedAt: later,
		Weekly:   quota.Window{Remaining: 500, Absolute: true, ResetAt: later.Add(5 * 24 * time.Hour)},
		FiveHour: quota.Window{Remaining: 60, Absolute: true, ResetAt: later.Add(5 * time.Hour)},
	}}
	a = p.Step(a, healthy, later)
	if a.State != StateActive || a.Weight <= 0 {
		t.Fatalf("got %v weight %d, want active with a positive weight", a.State, a.Weight)
	}
}

func TestCircuitBreakerGoesUnavailable(t *testing.T) {
	p := pol()
	a := warm(t, p, obsOK(win(500, 7*24*time.Hour), win(50, 3*time.Hour)))
	o := obsOK(win(500, 7*24*time.Hour), win(50, 3*time.Hour))
	o.CircuitBroken = true
	a = p.Step(a, o, now)
	if a.State != StateUnavailable || a.Weight != 0 {
		t.Fatalf("got %v weight %d, want unavailable with weight 0", a.State, a.Weight)
	}
}

func TestProbeFailureFreezesWeight(t *testing.T) {
	p := pol()
	a := warm(t, p, obsOK(win(500, 7*24*time.Hour), win(50, 3*time.Hour)))
	before := a
	a = p.Step(a, Observation{Err: errors.New("probe failed")}, now.Add(time.Minute))
	if a.Weight != before.Weight || a.Bucket != before.Bucket || a.State != before.State {
		t.Fatalf("failure must freeze the weight: %+v -> %+v", before, a)
	}
	if a.OKStreak != 0 {
		t.Fatalf("streak %d, want reset to 0", a.OKStreak)
	}
	if a.Stale {
		t.Fatal("inside the freshness window the weight is not stale yet")
	}
}

func TestFrozenWeightGoesStaleButIsNeverZeroed(t *testing.T) {
	p := pol()
	a := warm(t, p, obsOK(win(500, 7*24*time.Hour), win(50, 3*time.Hour)))
	before := a
	a = p.Step(a, Observation{Err: errors.New("probe failed")}, now.Add(p.Freshness+time.Minute))
	if !a.Stale {
		t.Fatal("want Stale past the freshness TTL")
	}
	if a.Weight != before.Weight {
		t.Fatalf("weight %d changed to %d; a transient outage must not evict the account",
			before.Weight, a.Weight)
	}
}

func TestStaleSnapshotIsTreatedAsFailure(t *testing.T) {
	p := pol()
	a := warm(t, p, obsOK(win(500, 7*24*time.Hour), win(50, 3*time.Hour)))
	before := a
	// Weekly window already rolled over: Remaining belongs to the old window.
	// It must never be clamped to TFloor and reused as urgency.
	a = p.Step(a, obsOK(win(500, -time.Minute), win(50, 3*time.Hour)), now)
	if a.Weight != before.Weight || a.Bucket != before.Bucket {
		t.Fatalf("stale snapshot must freeze, not re-price: %+v -> %+v", before, a)
	}
	if a.OKStreak != 0 {
		t.Fatalf("streak %d, want 0", a.OKStreak)
	}
}

func TestHysteresisKeepsWeightByteIdentical(t *testing.T) {
	p := pol()
	a := warm(t, p, obsOK(win(500, 7*24*time.Hour), win(50, 3*time.Hour)))
	// A small drift inside the dead band must not move the weight, so the
	// host's smooth-WRR accumulator is not wiped (doc §5.4 constraint 1).
	b := p.Step(a, obsOK(win(497, 7*24*time.Hour-30*time.Minute), win(49, 2*time.Hour)), now)
	if b.Weight != a.Weight || b.Bucket != a.Bucket {
		t.Fatalf("drift moved the weight %d -> %d", a.Weight, b.Weight)
	}
}

func TestUnavailableLiftsAtRecoverAtEvenWhileProbesFail(t *testing.T) {
	// An account excluded by a spent short window must not stay at weight 0
	// past its own reset just because the probe endpoint went down with it.
	p := pol()
	a := warm(t, p, obsOK(win(500, 7*24*time.Hour), win(0, time.Hour)))
	if a.State != StateUnavailable || a.Weight != 0 {
		t.Fatalf("got %+v, want the exclusion", a)
	}
	if a.RecoverAt.IsZero() {
		t.Fatal("an exclusion with no recovery time can never lift")
	}

	stillDown := Observation{Err: errors.New("probe endpoint down")}
	held := p.Step(a, stillDown, now.Add(30*time.Minute))
	if held.Weight != 0 {
		t.Fatalf("got weight %d before RecoverAt, want the exclusion held", held.Weight)
	}
	lifted := p.Step(held, stillDown, a.RecoverAt.Add(time.Minute))
	if lifted.Weight != p.Quantizer.MinWeight || lifted.State != StateDraining {
		t.Fatalf("got %+v, want re-admission at the minimum weight", lifted)
	}
	if !lifted.RecoverAt.IsZero() {
		t.Fatal("a lifted exclusion must clear its recovery time")
	}
}

func TestRecoverAtFallsBackToTheWeeklyResetWhenThereIsNoShortWindow(t *testing.T) {
	// A single-window payload leaves FiveHour zero-valued. Picking the earlier
	// of the two resets then yields the zero time, which never arrives.
	p := pol()
	o := Observation{Snapshot: quota.Snapshot{
		AuthID: "a", ObservedAt: now,
		Weekly: quota.Window{Remaining: 500, Absolute: true, ResetAt: now.Add(48 * time.Hour)},
	}, CircuitBroken: true}
	a := warm(t, p, o)
	if a.State != StateUnavailable {
		t.Fatalf("got %v, want unavailable", a.State)
	}
	if !a.RecoverAt.Equal(now.Add(48 * time.Hour)) {
		t.Fatalf("RecoverAt=%v, want the weekly reset", a.RecoverAt)
	}
}
