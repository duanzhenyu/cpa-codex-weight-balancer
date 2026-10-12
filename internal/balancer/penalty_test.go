package balancer

import (
	"math"
	"slices"
	"testing"
	"time"
)

func penalized() Policy {
	p := DefaultPolicy()
	p.ErrorPenalty = true
	return p
}

const tickEvery = 15 * time.Minute

// fleet drives an engine through scheduled ticks the way the controller does.
type fleet struct {
	*Engine
	at time.Time
}

func newFleet(p Policy) *fleet { return &fleet{Engine: NewEngine(p), at: now} }

// run folds the same per-account outcomes into n ticks one interval apart —
// fold, then price — and returns the last decision for each account.
func (f *fleet) run(t *testing.T, n int, outcomes map[string]Outcomes) map[string]Decision {
	t.Helper()
	out := map[string]Decision{}
	for range n {
		f.at = f.at.Add(tickEvery)
		var obs []Observation
		for id := range outcomes {
			obs = append(obs, healthy(id, f.at))
		}
		f.Fold(outcomes, f.at)
		r := cycle(f.Engine, obs, f.at)
		for id := range outcomes {
			out[id] = find(t, r, id)
		}
	}
	return out
}

func TestErrorRateIsTheShareOfFailuresInTheWindow(t *testing.T) {
	f := newFleet(penalized())
	rate := func() float64 { return f.Accounts()["a"].ErrorRate }

	// One failure on an idle account reads as one in ten.
	f.run(t, 1, map[string]Outcomes{"a": {Requests: 1, Failures: 1}})
	near(t, rate(), 0.1)
	// A tick with no traffic carries no evidence either way.
	f.run(t, 1, map[string]Outcomes{"a": {}})
	near(t, rate(), 0.1)
	f.run(t, 1, map[string]Outcomes{"a": {Requests: 40, Failures: 20}})
	near(t, rate(), 21.0/41)
}

func TestErrorsAgeOutOfTheWindow(t *testing.T) {
	e := NewEngine(penalized())
	rate := func() float64 { return e.Accounts()["a"].ErrorRate }
	e.Fold(map[string]Outcomes{"a": {Requests: 10, Failures: 10}}, now)
	e.Fold(map[string]Outcomes{"a": {Requests: 10}}, now.Add(time.Hour))
	near(t, rate(), 0.5)
	e.Fold(map[string]Outcomes{"a": {}}, now.Add(DefaultErrorWindow-time.Nanosecond))
	near(t, rate(), 0.5)
	e.Fold(map[string]Outcomes{"a": {}}, now.Add(DefaultErrorWindow))
	near(t, rate(), 0)
}

func TestErrorWindowIsThePolicys(t *testing.T) {
	p := penalized()
	p.ErrorWindow = time.Hour
	e := NewEngine(p)
	e.Fold(map[string]Outcomes{"a": {Requests: 10, Failures: 10}}, now)
	e.Fold(map[string]Outcomes{"a": {}}, now.Add(time.Hour-time.Nanosecond))
	near(t, e.Accounts()["a"].ErrorRate, 1)
	e.Fold(map[string]Outcomes{"a": {}}, now.Add(time.Hour))
	near(t, e.Accounts()["a"].ErrorRate, 0)
}

func TestUnsetErrorWindowIsTheDefault(t *testing.T) {
	// A policy built without DefaultPolicy must not shrink the window to a tick.
	p := penalized()
	p.ErrorWindow = 0
	e := NewEngine(p)
	e.Fold(map[string]Outcomes{"a": {Requests: 10, Failures: 10}}, now)
	e.Fold(map[string]Outcomes{"a": {}}, now.Add(DefaultErrorWindow-time.Nanosecond))
	near(t, e.Accounts()["a"].ErrorRate, 1)
}

func TestErrorsLowerTheWeight(t *testing.T) {
	d := newFleet(penalized()).run(t, 3, map[string]Outcomes{
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
	f := newFleet(penalized())
	d := f.run(t, 80, map[string]Outcomes{
		"clean":  {Requests: 50},
		"broken": {Requests: 50, Failures: 50},
	})
	broken := d["broken"]
	if broken.State == StateUnavailable || broken.Weight < f.Policy.Quantizer.MinWeight {
		t.Fatalf("got %+v, want a positive weight", broken)
	}
	if broken.Weight != f.Policy.Quantizer.MinWeight {
		t.Fatalf("broken at %d: a credential failing everything must sink to the floor", broken.Weight)
	}
}

func TestErrorPenaltyOffIgnoresOutcomes(t *testing.T) {
	d := newFleet(DefaultPolicy()).run(t, 3, map[string]Outcomes{
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
	f := newFleet(penalized())
	f.run(t, 2, map[string]Outcomes{"a": {Requests: 100}})
	if d := f.run(t, 1, map[string]Outcomes{"a": {Requests: 100, Failures: 1}})["a"]; d.Write {
		t.Fatalf("got %+v, want one failure in a hundred absorbed", d)
	}
}

func TestPenaltyHoldsForTheWindowThenLifts(t *testing.T) {
	f := newFleet(penalized())
	burst := f.run(t, 4, map[string]Outcomes{"clean": {Requests: 20}, "faulty": {Requests: 20, Failures: 20}})
	if burst["faulty"].Weight >= burst["clean"].Weight {
		t.Fatalf("setup: faulty %d not below clean %d", burst["faulty"].Weight, burst["clean"].Weight)
	}
	steady := map[string]Outcomes{"clean": {Requests: 20}, "faulty": {Requests: 20}}
	// Clean traffic dilutes the burst but must not wash it out within hours.
	half := f.run(t, int(DefaultErrorWindow/tickEvery)/2, steady)
	if half["faulty"].Weight >= half["clean"].Weight {
		t.Fatalf("faulty %d, clean %d halfway through the window: the burst is still in it", half["faulty"].Weight, half["clean"].Weight)
	}
	// The last failing tick leaves the window exactly a window after it.
	after := f.run(t, int(DefaultErrorWindow/tickEvery)/2, steady)
	if after["faulty"].Weight != after["clean"].Weight {
		t.Fatalf("faulty %d, clean %d: the penalty must lift once the burst leaves the window", after["faulty"].Weight, after["clean"].Weight)
	}
}

func TestIdlePenaltyLiftsInOneWrite(t *testing.T) {
	// Every write wipes the host's accumulator. An account that draws no traffic
	// gathers no evidence, so its rate moves only as samples leave the window; a
	// burst of nothing but failures holds at 1 until the last of it leaves, and
	// comes back in one step rather than a write per bucket on the way up.
	f := newFleet(penalized())
	f.run(t, 4, map[string]Outcomes{"clean": {Requests: 20}, "faulty": {Requests: 20, Failures: 20}})
	idle := map[string]Outcomes{"clean": {}, "faulty": {}}
	writes := 0
	var after map[string]Decision
	for range int(DefaultErrorWindow / tickEvery) {
		after = f.run(t, 1, idle)
		if after["faulty"].Write {
			writes++
		}
	}
	if after["faulty"].Weight != after["clean"].Weight || writes != 1 {
		t.Fatalf("faulty %d, clean %d after %d writes: want the weight back in one", after["faulty"].Weight, after["clean"].Weight, writes)
	}
}

func TestFoldLeavesTheWeightToTheNextCycle(t *testing.T) {
	// Folding records evidence; only a cycle may turn it into a weight.
	f := newFleet(penalized())
	f.run(t, 2, map[string]Outcomes{"a": {Requests: 10}})
	before := f.Accounts()["a"]
	f.Fold(map[string]Outcomes{"a": {Requests: 10, Failures: 10}}, f.at.Add(tickEvery))
	after := f.Accounts()["a"]
	if after.Weight != before.Weight || after.Bucket != before.Bucket || after.State != before.State {
		t.Fatalf("fold moved %+v -> %+v", before, after)
	}
	near(t, after.ErrorRate, 10.0/30)
}

func TestFoldNeverRewritesAHandedOutWindow(t *testing.T) {
	// Accounts hands out shallow copies: the status page reads them, and a reload
	// hands them to the next engine.
	e := NewEngine(penalized())
	e.Fold(map[string]Outcomes{"a": {Requests: 10, Failures: 10}}, now)
	e.Fold(map[string]Outcomes{"a": {Requests: 10}}, now.Add(time.Hour))
	held := e.Accounts()["a"].Errors
	if len(held) != 2 {
		t.Fatalf("setup: %d samples, want 2", len(held))
	}
	want := slices.Clone(held)
	e.Fold(map[string]Outcomes{"a": {Requests: 10, Failures: 5}}, now.Add(DefaultErrorWindow))
	if !slices.Equal(held, want) {
		t.Fatalf("a fold rewrote a window already handed out: %+v, want %+v", held, want)
	}
	near(t, e.Accounts()["a"].ErrorRate, 0.25)
}

func TestRestoredEngineKeepsTheWindow(t *testing.T) {
	old := newFleet(penalized())
	old.run(t, 1, map[string]Outcomes{"a": {Requests: 10, Failures: 10}})
	next := NewEngine(penalized())
	next.Restore(old.Accounts(), old.Written())
	next.Fold(map[string]Outcomes{"a": {Requests: 10}}, old.at.Add(tickEvery))
	near(t, next.Accounts()["a"].ErrorRate, 0.5)
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
			e.Fold(map[string]Outcomes{"a": {Requests: 10, Failures: 10}}, now)
			cycle(e, []Observation{tiny}, now)
		}
		if got := e.Accounts()["a"]; got.Weight != p.Quantizer.MinWeight {
			t.Errorf("%s: got %+v, want the minimum weight", name, got)
		}
	}
}

func TestRecheckReadmitsWithThePenalty(t *testing.T) {
	f, twin := newFleet(penalized()), newFleet(penalized())
	for _, x := range []*fleet{f, twin} {
		x.run(t, 3, map[string]Outcomes{"a": {Requests: 10}})
	}
	f.run(t, 3, map[string]Outcomes{"a": {Requests: 10, Failures: 10}})
	twin.run(t, 3, map[string]Outcomes{"a": {Requests: 10}})
	for _, x := range []*fleet{f, twin} {
		d, ok := x.Recheck(spent("a", time.Hour, x.at), x.at)
		if !ok {
			t.Fatalf("setup: exclusion not applied: %+v", d)
		}
		x.Commit(d.AuthID, d.Weight)
	}
	later := f.at.Add(2 * time.Hour)
	got, ok := f.Recheck(healthy("a", later), later)
	want, _ := twin.Recheck(healthy("a", later), later)
	if !ok || got.Weight <= 0 || got.Weight >= want.Weight {
		t.Fatalf("readmitted at %d (ok=%v), error-free twin at %d: the penalty must survive an exclusion", got.Weight, ok, want.Weight)
	}
}

func TestFailingAccountStillSinksAfterItsBucketIsCleared(t *testing.T) {
	// A reload that changes pricing clears every bucket. A rate of exactly 1
	// discounts the score to 0, the quantizer ignores it, and the unset bucket
	// hands a credential failing every request the anchor weight.
	f := newFleet(penalized())
	outcomes := map[string]Outcomes{"clean": {Requests: 50}, "broken": {Requests: 50, Failures: 50}}
	f.run(t, 80, outcomes)
	accounts := f.Accounts()
	for id, a := range accounts {
		a.Bucket = Bucket{}
		accounts[id] = a
	}
	f.Restore(accounts, f.Written())
	d := f.run(t, 1, outcomes)
	if d["broken"].Weight != f.Policy.Quantizer.MinWeight {
		t.Fatalf("broken at %d after a bucket reset, want the floor", d["broken"].Weight)
	}
}
