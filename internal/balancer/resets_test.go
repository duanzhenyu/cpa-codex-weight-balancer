package balancer

import (
	"testing"
	"time"

	"github.com/duanzhenyu/cpa-codex-weight-balancer/internal/quota"
)

// withResets gives an observation banked reset credits, each one full window of
// the same 500 units healthy() prices the week in, expiring after the given
// durations.
func withResets(o Observation, expiries ...time.Duration) Observation {
	for _, in := range expiries {
		o.Resets = append(o.Resets, quota.Window{Remaining: 500, Absolute: true, ResetAt: now.Add(in)})
	}
	return o
}

// weights warms a fresh engine on each observation and returns where each one
// settled.
func weights(t *testing.T, p Policy, obs ...Observation) map[string]Decision {
	t.Helper()
	e := NewEngine(p)
	var r Report
	for range p.Warmup {
		r = cycle(e, obs, now)
	}
	out := map[string]Decision{}
	for _, o := range obs {
		out[o.Snapshot.AuthID] = find(t, r, o.Snapshot.AuthID)
	}
	return out
}

func TestResetCreditsRaiseTheWeight(t *testing.T) {
	d := weights(t, DefaultPolicy(),
		healthy("none", now),
		withResets(healthy("banked", now), 10*24*time.Hour),
	)
	if d["banked"].Weight <= d["none"].Weight {
		t.Fatalf("banked %d, none %d: a credit is quota the account must spend before it expires", d["banked"].Weight, d["none"].Weight)
	}
}

func TestMoreAndSoonerCreditsWeighMore(t *testing.T) {
	d := weights(t, DefaultPolicy(),
		withResets(healthy("one late", now), 25*24*time.Hour),
		withResets(healthy("one soon", now), 3*24*time.Hour),
		withResets(healthy("two late", now), 25*24*time.Hour, 25*24*time.Hour),
	)
	if d["one soon"].Weight <= d["one late"].Weight {
		t.Fatalf("soon %d, late %d: a credit closer to expiry is more urgent", d["one soon"].Weight, d["one late"].Weight)
	}
	if d["two late"].Weight <= d["one late"].Weight {
		t.Fatalf("two %d, one %d: every credit is a window of its own", d["two late"].Weight, d["one late"].Weight)
	}
}

func TestExpiredCreditAddsNothing(t *testing.T) {
	d := weights(t, DefaultPolicy(),
		healthy("none", now),
		withResets(healthy("lapsed", now), -time.Minute),
	)
	if d["lapsed"].Weight != d["none"].Weight {
		t.Fatalf("lapsed %d, none %d: an expired credit can no longer be redeemed", d["lapsed"].Weight, d["none"].Weight)
	}
}

// A credit is quota the account does not have until someone redeems it. A spent
// window answers every request with 429 however many credits are banked, and
// weight 0 is the only weight that stops the host routing to it.
func TestCreditsNeverKeepASpentAccountServing(t *testing.T) {
	d := weights(t, DefaultPolicy(),
		withResets(obsFor("weekly spent", 0, 3*24*time.Hour, 50, 3*time.Hour, now), time.Hour),
		withResets(obsFor("short spent", 500, 3*24*time.Hour, 0, 3*time.Hour, now), time.Hour),
	)
	for id, got := range d {
		if got.State != StateUnavailable || got.Weight != 0 {
			t.Errorf("%s: got %+v, want excluded at weight 0", id, got)
		}
	}
}

func TestErrorPenaltyDiscountsTheCreditsToo(t *testing.T) {
	e := NewEngine(penalized())
	e.Restore(map[string]Account{"broken": {ErrorRate: 1}}, nil)
	var r Report
	for range e.Policy.Warmup {
		r = cycle(e, []Observation{withResets(healthy("broken", now), time.Hour)}, now)
	}
	if got := find(t, r, "broken"); got.Weight != e.Policy.Quantizer.MinWeight {
		t.Fatalf("got %+v: an account failing every request sinks to the floor, credits or not", got)
	}
}
