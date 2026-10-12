package balancer

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/duanzhenyu/cpa-codex-weight-balancer/internal/quota"
)

func obsFor(id string, weeklyRem float64, weeklyIn time.Duration, fiveRem float64, fiveIn time.Duration, at time.Time) Observation {
	return Observation{Snapshot: quota.Snapshot{
		AuthID: id, Generation: 1, ObservedAt: at,
		Weekly:   quota.Window{Remaining: weeklyRem, Absolute: true, ResetAt: at.Add(weeklyIn)},
		FiveHour: quota.Window{Remaining: fiveRem, Absolute: true, ResetAt: at.Add(fiveIn)},
	}}
}

func healthy(id string, at time.Time) Observation {
	return obsFor(id, 500, 7*24*time.Hour, 50, 3*time.Hour, at)
}

// cycle runs one round the way the controller does: fold the observations,
// then confirm the writes the host accepted. The engine records nothing as
// written until that confirmation arrives.
func cycle(e *Engine, obs []Observation, at time.Time) Report {
	r := e.Cycle(obs, at)
	for _, d := range r.Decisions {
		if d.Write {
			e.Commit(d.AuthID, d.Weight)
			r.Wrote++
		}
	}
	return r
}

func find(t *testing.T, r Report, id string) Decision {
	t.Helper()
	for _, d := range r.Decisions {
		if d.AuthID == id {
			return d
		}
	}
	t.Fatalf("no decision for %q", id)
	return Decision{}
}

func TestCycleWithholdsDuringWarmup(t *testing.T) {
	e := NewEngine(DefaultPolicy())
	r := e.Cycle([]Observation{healthy("a", now)}, now)
	d := find(t, r, "a")
	if d.State != StatePending || d.Write {
		t.Fatalf("got %+v, want pending without a write", d)
	}
	if r.Changed != 0 {
		t.Fatalf("Changed=%d, want 0", r.Changed)
	}
}

func TestCycleWritesOnceWarm(t *testing.T) {
	e := NewEngine(DefaultPolicy())
	e.Cycle([]Observation{healthy("a", now)}, now)
	r := e.Cycle([]Observation{healthy("a", now)}, now)
	d := find(t, r, "a")
	if !d.Write || d.Weight <= 0 || r.Changed != 1 {
		t.Fatalf("got %+v changed=%d, want a single write", d, r.Changed)
	}
}

func TestCycleIsQuietWhenNothingMoves(t *testing.T) {
	e := NewEngine(DefaultPolicy())
	for range 2 {
		cycle(e, []Observation{healthy("a", now)}, now)
	}
	r := e.Cycle([]Observation{healthy("a", now)}, now)
	if d := find(t, r, "a"); d.Write {
		t.Fatalf("unchanged weight must not be rewritten: %+v", d)
	}
	if r.Changed != 0 {
		t.Fatalf("Changed=%d, want 0", r.Changed)
	}
}

func TestCycleWritesWhenTheBucketMoves(t *testing.T) {
	e := NewEngine(DefaultPolicy())
	for range 2 {
		e.Cycle([]Observation{healthy("a", now)}, now)
	}
	// Heavy consumption pushes the rate well out of the dead band.
	r := e.Cycle([]Observation{obsFor("a", 20, 7*24*time.Hour, 50, 3*time.Hour, now)}, now)
	d := find(t, r, "a")
	if !d.Write || r.Changed != 1 {
		t.Fatalf("got %+v changed=%d, want a write", d, r.Changed)
	}
}

func TestCycleReportsBurnFromQuotaDelta(t *testing.T) {
	e := NewEngine(DefaultPolicy())
	later := now.Add(time.Hour)
	e.Cycle([]Observation{obsFor("a", 500, 7*24*time.Hour, 50, 3*time.Hour, now)}, now)
	// 20 units gone over one hour, and the window did not roll over.
	next := obsFor("a", 480, 7*24*time.Hour-time.Hour, 30, 2*time.Hour, later)
	r := e.Cycle([]Observation{next}, later)
	d := find(t, r, "a")
	if !d.Burn.Known {
		t.Fatal("want a known burn rate")
	}
	near(t, d.Burn.Rate, 20.0/3600)
}

func TestCycleBurnUnknownAcrossReset(t *testing.T) {
	e := NewEngine(DefaultPolicy())
	later := now.Add(time.Hour)
	e.Cycle([]Observation{obsFor("a", 5, time.Minute*30, 50, 3*time.Hour, now)}, now)
	// The weekly window rolled over: remaining jumped back up.
	r := e.Cycle([]Observation{obsFor("a", 1000, 7*24*time.Hour, 40, 2*time.Hour, later)}, later)
	if d := find(t, r, "a"); d.Burn.Known {
		t.Fatalf("burn across a reset is not measurable, got %+v", d.Burn)
	}
}

func TestCycleFirstObservationHasNoBurn(t *testing.T) {
	e := NewEngine(DefaultPolicy())
	r := e.Cycle([]Observation{healthy("a", now)}, now)
	if d := find(t, r, "a"); d.Burn.Known {
		t.Fatal("no previous snapshot means no burn")
	}
}

func TestCycleFailedProbeKeepsLastWeight(t *testing.T) {
	e := NewEngine(DefaultPolicy())
	for range 2 {
		cycle(e, []Observation{healthy("a", now)}, now)
	}
	before := e.Accounts()["a"].Weight
	r := e.Cycle([]Observation{{Snapshot: quota.Snapshot{AuthID: "a"}, Err: errors.New("boom")}}, now.Add(time.Minute))
	d := find(t, r, "a")
	if d.Weight != before || d.Write {
		t.Fatalf("got %+v, want the frozen weight %d and no write", d, before)
	}
}

func TestCycleDecisionsAreSorted(t *testing.T) {
	e := NewEngine(DefaultPolicy())
	r := e.Cycle([]Observation{healthy("c", now), healthy("a", now), healthy("b", now)}, now)
	if len(r.Decisions) != 3 {
		t.Fatalf("got %d decisions", len(r.Decisions))
	}
	for i, want := range []string{"a", "b", "c"} {
		if r.Decisions[i].AuthID != want {
			t.Fatalf("decision %d is %q, want %q", i, r.Decisions[i].AuthID, want)
		}
	}
}

func TestRestoreAvoidsRepricingAfterRestart(t *testing.T) {
	e := NewEngine(DefaultPolicy())
	for range 2 {
		cycle(e, []Observation{healthy("a", now)}, now)
	}
	saved, savedWeight := e.Accounts(), e.Accounts()["a"].Weight

	// A fresh process restores the persisted bucket and last written weight;
	// an unchanged account must not be rewritten (doc §5.4 constraint 1).
	fresh := NewEngine(DefaultPolicy())
	fresh.Restore(saved, map[string]int64{"a": savedWeight})
	r := fresh.Cycle([]Observation{healthy("a", now)}, now)
	if d := find(t, r, "a"); d.Write || d.Weight != savedWeight {
		t.Fatalf("got %+v, want no rewrite of weight %d", d, savedWeight)
	}
	if r.Changed != 0 {
		t.Fatalf("Changed=%d, want 0", r.Changed)
	}
}

func TestRestoreNeedsTheWrittenWeightsToo(t *testing.T) {
	// Carrying Accounts across a reload but not Written makes the next cycle
	// rewrite every credential — exactly the accumulator wipe Restore exists
	// to prevent.
	e := NewEngine(DefaultPolicy())
	for range 2 {
		cycle(e, []Observation{healthy("a", now), healthy("b", now)}, now)
	}

	partial := NewEngine(DefaultPolicy())
	partial.Restore(e.Accounts(), nil)
	if r := partial.Cycle([]Observation{healthy("a", now), healthy("b", now)}, now); r.Changed != 2 {
		t.Fatalf("Changed=%d, want 2: without Written every weight is rewritten", r.Changed)
	}

	full := NewEngine(DefaultPolicy())
	full.Restore(e.Accounts(), e.Written())
	if r := full.Cycle([]Observation{healthy("a", now), healthy("b", now)}, now); r.Changed != 0 {
		t.Fatalf("Changed=%d, want 0", r.Changed)
	}
}

func TestCycleDegradedOnBarePercentage(t *testing.T) {
	e := NewEngine(DefaultPolicy())
	o := healthy("a", now)
	o.Snapshot.Weekly.Absolute = false // percentage-only, not an absolute quantity
	for range 2 {
		e.Cycle([]Observation{o}, now)
	}
	r := e.Cycle([]Observation{o}, now)
	if d := find(t, r, "a"); !d.Degraded {
		t.Fatalf("got %+v, want Degraded", d)
	}
}

func TestCycleAndAccountsAreSafeInParallel(t *testing.T) {
	// The ticker folds observations while the management status route reads the
	// same maps. Run with -race; the start barrier is what makes the two
	// actually overlap rather than run one after the other.
	e := NewEngine(DefaultPolicy())
	obs := []Observation{healthy("a", now), healthy("b", now), healthy("c", now)}

	var wg sync.WaitGroup
	start := make(chan struct{})
	for _, work := range []func(int){
		func(i int) { e.Cycle(obs, now.Add(time.Duration(i)*time.Second)) },
		func(int) { e.Accounts() },
		func(int) { e.Written() },
		func(i int) { e.Commit("a", int64(i)) },
		func(i int) {
			e.Recheck(obsFor("b", 500, 7*24*time.Hour, 0, time.Hour, now), now.Add(time.Duration(i)*time.Second))
		},
		func(i int) {
			e.Fold(map[string]Outcomes{"a": {Requests: 2, Failures: i % 2}}, now.Add(time.Duration(i)*time.Hour))
		},
		func(int) {
			// The window is handed out by reference; reading it must not race a fold.
			for _, a := range e.Accounts() {
				for _, s := range a.Errors {
					_ = s.Failures
				}
			}
		},
	} {
		wg.Add(1)
		go func(work func(int)) {
			defer wg.Done()
			<-start
			for i := range 500 {
				work(i)
			}
		}(work)
	}
	close(start)
	wg.Wait()
}

func warmed(t *testing.T, e *Engine) int64 {
	t.Helper()
	var weight int64
	for range 2 {
		weight = find(t, cycle(e, []Observation{healthy("a", now)}, now), "a").Weight
	}
	return weight
}

func TestCycleRewritesAfterTheWeightVanishesFromDisk(t *testing.T) {
	// A credential deleted and re-added through the management UI comes back
	// without a weight, so the host routes it at the default of 1. Memory still
	// holds the last confirmed write, and used to suppress the repair forever.
	e := NewEngine(DefaultPolicy())
	want := warmed(t, e)

	o := healthy("a", now)
	o.Disk = DiskWeight{Read: true}
	r := e.Cycle([]Observation{o}, now)

	d := find(t, r, "a")
	if !d.Write || d.Weight != want || r.Changed != 1 {
		t.Fatalf("got %+v changed=%d, want weight %d rewritten", d, r.Changed, want)
	}
}

func TestCycleRewritesWhenDiskDivergedFromMemory(t *testing.T) {
	e := NewEngine(DefaultPolicy())
	want := warmed(t, e)

	o := healthy("a", now)
	o.Disk = DiskWeight{Weight: want + 100, Present: true, Read: true}
	r := e.Cycle([]Observation{o}, now)

	d := find(t, r, "a")
	if !d.Write || d.Weight != want || r.Changed != 1 {
		t.Fatalf("got %+v changed=%d, want weight %d restored", d, r.Changed, want)
	}
}

func TestCycleStaysQuietWhenDiskAlreadyMatches(t *testing.T) {
	// The sparse-write guarantee: reconciling against disk must not turn every
	// cycle into a write, which would wipe the host accumulator fleet-wide.
	e := NewEngine(DefaultPolicy())
	want := warmed(t, e)

	o := healthy("a", now)
	o.Disk = DiskWeight{Weight: want, Present: true, Read: true}
	r := e.Cycle([]Observation{o}, now)

	if d := find(t, r, "a"); d.Write || r.Changed != 0 {
		t.Fatalf("got %+v changed=%d, want no write", d, r.Changed)
	}
}

func TestCycleKeepsMemoryWhenTheFileWasNotRead(t *testing.T) {
	// AuthGet failed, so nothing is known about the file this cycle. The last
	// confirmed write stands: guessing would rewrite the whole fleet whenever
	// the host is briefly unable to hand over credentials.
	e := NewEngine(DefaultPolicy())
	warmed(t, e)

	o := healthy("a", now)
	o.Disk = DiskWeight{}
	r := e.Cycle([]Observation{o}, now)

	if d := find(t, r, "a"); d.Write || r.Changed != 0 {
		t.Fatalf("got %+v changed=%d, want no write", d, r.Changed)
	}
}
