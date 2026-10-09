package balancer

import (
	"math"
	"testing"
	"time"
)

func penalized() Policy {
	p := DefaultPolicy()
	p.ErrorPenalty = true
	return p
}

// run folds the same per-account outcomes into n ticks the way the controller
// does — fold, then price — and returns the last decision for each account.
func run(t *testing.T, e *Engine, n int, outcomes map[string]Outcomes) map[string]Decision {
	t.Helper()
	out := map[string]Decision{}
	for range n {
		var obs []Observation
		for id := range outcomes {
			obs = append(obs, healthy(id, now))
		}
		e.Fold(outcomes)
		r := cycle(e, obs, now)
		for id := range outcomes {
			out[id] = find(t, r, id)
		}
	}
	return out
}

func TestErrorRateIsSmoothedOverAFloorOfRequests(t *testing.T) {
	e := NewEngine(penalized())
	rate := func() float64 { return e.Accounts()["a"].ErrorRate }

	// One failure on an idle account reads as one in ten, half-weighted.
	run(t, e, 1, map[string]Outcomes{"a": {Requests: 1, Failures: 1}})
	near(t, rate(), 0.05)
	// A cycle with no traffic carries no errors and halves the rate.
	run(t, e, 1, map[string]Outcomes{"a": {}})
	near(t, rate(), 0.025)
	run(t, e, 1, map[string]Outcomes{"a": {Requests: 40, Failures: 20}})
	near(t, rate(), 0.2625)
}

func TestErrorsLowerTheWeight(t *testing.T) {
	e := NewEngine(penalized())
	d := run(t, e, 3, map[string]Outcomes{
		"clean":  {Requests: 20},
		"faulty": {Requests: 20, Failures: 10},
	})
	clean, faulty := d["clean"], d["faulty"]
	if faulty.Weight <= 0 || faulty.Weight >= clean.Weight {
		t.Fatalf("faulty weight %d, clean %d: errors must lower the weight without zeroing it", faulty.Weight, clean.Weight)
	}
	if faulty.State == StateUnavailable {
		t.Fatalf("errors must never exclude: %+v", faulty)
	}
}

func TestEveryRequestFailingStillNeverExcludes(t *testing.T) {
	// weight 0 is the host's hard exclusion and evicts sticky sessions; only a
	// spent window or an account-level 429 may earn it.
	e := NewEngine(penalized())
	d := run(t, e, 80, map[string]Outcomes{
		"clean":  {Requests: 50},
		"broken": {Requests: 50, Failures: 50},
	})
	broken := d["broken"]
	if broken.State == StateUnavailable || broken.Weight < e.Policy.Quantizer.MinWeight {
		t.Fatalf("got %+v, want a positive weight", broken)
	}
	if broken.Weight != e.Policy.Quantizer.MinWeight {
		t.Fatalf("broken at %d: a credential failing everything must sink to the floor", broken.Weight)
	}
}

func TestErrorPenaltyOffIgnoresOutcomes(t *testing.T) {
	e := NewEngine(DefaultPolicy())
	d := run(t, e, 3, map[string]Outcomes{
		"clean":  {Requests: 20},
		"faulty": {Requests: 20, Failures: 20},
	})
	if d["faulty"].Weight != d["clean"].Weight {
		t.Fatalf("faulty %d, clean %d: with the switch off errors must not move weights", d["faulty"].Weight, d["clean"].Weight)
	}
}

func TestOccasionalErrorsStayInsideHysteresis(t *testing.T) {
	// Every write wipes the host's routing accumulator fleet-wide, so a stray
	// failure must not cost one.
	e := NewEngine(penalized())
	run(t, e, 2, map[string]Outcomes{"a": {Requests: 100}})
	if d := run(t, e, 1, map[string]Outcomes{"a": {Requests: 100, Failures: 1}})["a"]; d.Write {
		t.Fatalf("got %+v, want one failure in a hundred absorbed", d)
	}
}

func TestWeightRecoversOnceErrorsStop(t *testing.T) {
	e := NewEngine(penalized())
	before := run(t, e, 4, map[string]Outcomes{"clean": {Requests: 20}, "faulty": {Requests: 20, Failures: 20}})
	if before["faulty"].Weight >= before["clean"].Weight {
		t.Fatalf("setup: faulty %d not below clean %d", before["faulty"].Weight, before["clean"].Weight)
	}
	// Every write wipes the host's accumulator, so the climb back must take a
	// handful of them, not one per cycle until the rate reaches zero.
	writes := 0
	var after map[string]Decision
	for range 12 {
		after = run(t, e, 1, map[string]Outcomes{"clean": {Requests: 20}, "faulty": {Requests: 20}})
		if after["faulty"].Write {
			writes++
		}
	}
	if after["faulty"].Weight != after["clean"].Weight {
		t.Fatalf("faulty %d, clean %d: the penalty must decay away", after["faulty"].Weight, after["clean"].Weight)
	}
	if writes > 5 {
		t.Fatalf("%d writes on the way back up", writes)
	}
}

func TestFoldLeavesTheWeightToTheNextCycle(t *testing.T) {
	// Folding records evidence; only a cycle may turn it into a weight.
	e := NewEngine(penalized())
	run(t, e, 2, map[string]Outcomes{"a": {Requests: 10}})
	before := e.Accounts()["a"]
	e.Fold(map[string]Outcomes{"a": {Requests: 10, Failures: 10}})
	after := e.Accounts()["a"]
	if after.Weight != before.Weight || after.Bucket != before.Bucket || after.State != before.State {
		t.Fatalf("fold moved %+v -> %+v", before, after)
	}
	near(t, after.ErrorRate, 0.5)
}

func TestDiscountNeverUnderflowsAScoreToNothing(t *testing.T) {
	// The smallest score the scorer accepts, discounted, underflows to zero. The
	// quantizer ignores zero, and a bucket never committed then reads as the
	// anchor weight instead of the floor — whatever unit the anchor is in.
	tinyAnchor := penalized()
	tinyAnchor.Quantizer.R0 = math.SmallestNonzeroFloat64
	for name, p := range map[string]Policy{"default anchor": penalized(), "tiny anchor": tinyAnchor} {
		e := NewEngine(p)
		week := 7 * 24 * time.Hour
		tiny := obsFor("a", math.SmallestNonzeroFloat64*week.Seconds(), week, 50, 3*time.Hour, now)
		for range 2 {
			e.Fold(map[string]Outcomes{"a": {Requests: 10, Failures: 10}})
			cycle(e, []Observation{tiny}, now)
		}
		if got := e.Accounts()["a"]; got.Weight != p.Quantizer.MinWeight {
			t.Errorf("%s: got %+v, want the minimum weight", name, got)
		}
	}
}

func TestRecheckReadmitsWithThePenalty(t *testing.T) {
	e, twin := NewEngine(penalized()), NewEngine(penalized())
	for _, x := range []*Engine{e, twin} {
		run(t, x, 3, map[string]Outcomes{"a": {Requests: 10}})
	}
	run(t, e, 3, map[string]Outcomes{"a": {Requests: 10, Failures: 10}})
	for _, x := range []*Engine{e, twin} {
		d, ok := x.Recheck(spent("a", time.Hour, now), now)
		if !ok {
			t.Fatalf("setup: exclusion not applied: %+v", d)
		}
		x.Commit(d.AuthID, d.Weight)
	}
	later := now.Add(2 * time.Hour)
	got, ok := e.Recheck(healthy("a", later), later)
	want, _ := twin.Recheck(healthy("a", later), later)
	if !ok || got.Weight <= 0 || got.Weight >= want.Weight {
		t.Fatalf("readmitted at %d (ok=%v), error-free twin at %d: the penalty must survive an exclusion", got.Weight, ok, want.Weight)
	}
}

func TestFailingAccountStillSinksAfterItsBucketIsCleared(t *testing.T) {
	// A reload that changes pricing clears every bucket. A rate that has rounded
	// up to exactly 1 discounts the score to 0, the quantizer ignores it, and the
	// unset bucket hands a credential failing every request the anchor weight.
	e := NewEngine(penalized())
	outcomes := map[string]Outcomes{"clean": {Requests: 50}, "broken": {Requests: 50, Failures: 50}}
	run(t, e, 80, outcomes)
	accounts := e.Accounts()
	for id, a := range accounts {
		a.Bucket = Bucket{}
		accounts[id] = a
	}
	e.Restore(accounts, e.Written())
	d := run(t, e, 1, outcomes)
	if d["broken"].Weight != e.Policy.Quantizer.MinWeight {
		t.Fatalf("broken at %d after a bucket reset, want the floor", d["broken"].Weight)
	}
}
