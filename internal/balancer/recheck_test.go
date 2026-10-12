package balancer

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/duanzhenyu/cpa-codex-weight-balancer/internal/quota"
)

func spent(id string, fiveIn time.Duration, at time.Time) Observation {
	return obsFor(id, 500, 7*24*time.Hour, 0, fiveIn, at)
}

// excluded warms an account and then lets a recheck exclude it, confirming the
// write the way the controller does.
func excluded(t *testing.T, e *Engine, fiveIn time.Duration) {
	t.Helper()
	warmed(t, e)
	d, ok := e.Recheck(spent("a", fiveIn, now), now)
	if !ok || !d.Write {
		t.Fatalf("got %+v ok=%v, want the exclusion written", d, ok)
	}
	e.Commit(d.AuthID, d.Weight)
}

func TestRecheckExcludesAnAccountWhoseShortWindowIsSpent(t *testing.T) {
	e := NewEngine(DefaultPolicy())
	warmed(t, e)

	d, ok := e.Recheck(spent("a", 2*time.Hour, now), now)
	if !ok || d.State != StateUnavailable || d.Weight != 0 || !d.Write {
		t.Fatalf("got %+v ok=%v, want an exclusion to write", d, ok)
	}
	if got := e.Accounts()["a"].RecoverAt; !got.Equal(now.Add(2 * time.Hour)) {
		t.Fatalf("RecoverAt %v, want the 5h reset", got)
	}
}

func TestRecheckLeavesAServingAccountUntouched(t *testing.T) {
	// Off the timer only exclusions may move. A reading that merely re-prices a
	// serving account would otherwise commit its bucket at 429 rate, and the
	// next cycle would write it — an extra accumulator wipe per rejection.
	e, twin := NewEngine(DefaultPolicy()), NewEngine(DefaultPolicy())
	warmed(t, e)
	warmed(t, twin)
	before := e.Accounts()["a"]

	moved := obsFor("a", 20, 7*24*time.Hour, 50, 3*time.Hour, now)
	if d, ok := e.Recheck(moved, now); ok {
		t.Fatalf("got %+v, want a serving account left alone", d)
	}
	if after := e.Accounts()["a"]; !reflect.DeepEqual(after, before) {
		t.Fatalf("state moved %+v -> %+v", before, after)
	}

	next := healthy("a", now.Add(time.Minute))
	got, want := find(t, e.Cycle([]Observation{next}, now.Add(time.Minute)), "a"), find(t, twin.Cycle([]Observation{next}, now.Add(time.Minute)), "a")
	if got.Write != want.Write || got.Weight != want.Weight || got.Burn != want.Burn {
		t.Fatalf("cycle after a no-op recheck %+v, without one %+v", got, want)
	}
}

func TestRecheckReadmitsAfterTheReset(t *testing.T) {
	e := NewEngine(DefaultPolicy())
	excluded(t, e, 2*time.Hour)

	later := now.Add(2*time.Hour + time.Minute)
	d, ok := e.Recheck(obsFor("a", 500, 5*24*time.Hour, 100, 5*time.Hour, later), later)
	if !ok || d.State == StateUnavailable || d.Weight <= 0 || !d.Write {
		t.Fatalf("got %+v ok=%v, want re-admission with a positive weight", d, ok)
	}
	if !e.Accounts()["a"].RecoverAt.IsZero() {
		t.Fatal("a lifted exclusion must clear its recovery time")
	}
}

func TestRecheckStillSpentMovesRecoverAtWithoutAWrite(t *testing.T) {
	// Upstream can lag its own reset. Holding the old RecoverAt would have the
	// timer probe again every retry floor until the next cycle corrected it.
	e := NewEngine(DefaultPolicy())
	excluded(t, e, 2*time.Hour)

	later := now.Add(2*time.Hour + time.Minute)
	d, ok := e.Recheck(spent("a", 10*time.Minute, later), later)
	if !ok || d.Write || d.Weight != 0 {
		t.Fatalf("got %+v ok=%v, want the exclusion held without a write", d, ok)
	}
	if got := e.Accounts()["a"].RecoverAt; !got.Equal(later.Add(10 * time.Minute)) {
		t.Fatalf("RecoverAt %v, want the reset the fresh probe reported", got)
	}
}

func TestRecheckWithAFailedProbeLiftsOnlyPastRecoverAt(t *testing.T) {
	e := NewEngine(DefaultPolicy())
	excluded(t, e, 2*time.Hour)
	down := Observation{Snapshot: quota.Snapshot{AuthID: "a"}, Err: errors.New("probe down")}

	if d, _ := e.Recheck(down, now.Add(time.Hour)); d.Write || e.Accounts()["a"].Weight != 0 {
		t.Fatalf("got %+v, want the exclusion held before its reset", d)
	}
	d, ok := e.Recheck(down, now.Add(2*time.Hour+time.Minute))
	if !ok || !d.Write || d.Weight != DefaultPolicy().Quantizer.MinWeight {
		t.Fatalf("got %+v ok=%v, want re-admission at the minimum weight", d, ok)
	}
}

func TestRecheckDuringWarmupWritesNothing(t *testing.T) {
	// The cycle does not trust a credential before warmup, and a rejection is no
	// reason for the recheck to.
	e := NewEngine(DefaultPolicy())
	if d, ok := e.Recheck(spent("a", 2*time.Hour, now), now); ok || d.Write {
		t.Fatalf("got %+v ok=%v, want nothing during warmup", d, ok)
	}
}
