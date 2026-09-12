package controller

import (
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/duanzhenyu/cpa-codex-weight-balancer/internal/balancer"
)

// liveHeaders mirrors what an upstream Codex response carries, using the same
// two windows as usageBody so a header-fed cycle and a probe-fed one are
// directly comparable.
func liveHeaders() http.Header {
	return http.Header{
		"X-Codex-Plan-Type":                     {"plus"},
		"X-Codex-Primary-Used-Percent":          {"20"},
		"X-Codex-Primary-Window-Minutes":        {"300"},
		"X-Codex-Primary-Reset-After-Seconds":   {"7200"},
		"X-Codex-Secondary-Used-Percent":        {"45"},
		"X-Codex-Secondary-Window-Minutes":      {"10080"},
		"X-Codex-Secondary-Reset-After-Seconds": {"400000"},
	}
}

func TestObservationReplacesTheProbeForThatCycle(t *testing.T) {
	h := newFakeHost()
	c := ctl(h)
	if !c.Observe("a", liveHeaders(), now) {
		t.Fatal("a quota-bearing response must be accepted")
	}
	run(t, c, 1)
	if len(h.requests) != 0 {
		t.Fatalf("got %d probes, want none: the observation already priced the account", len(h.requests))
	}
	// The credential is still read: the weight on disk and the file name come
	// from it, not from the response.
	if len(h.gets) == 0 {
		t.Fatal("the credential must still be read")
	}
}

func TestObservationIsConsumedOnce(t *testing.T) {
	h := newFakeHost()
	c := ctl(h)
	c.Observe("a", liveHeaders(), now)
	run(t, c, 1)
	if len(h.requests) != 0 {
		t.Fatalf("cycle 1 probed %d times", len(h.requests))
	}
	run(t, c, 1)
	if len(h.requests) != 1 {
		t.Fatalf("cycle 2 made %d probes, want 1: a spent observation must not be reused", len(h.requests))
	}
}

func TestObservedAndProbedCyclesPriceTheSame(t *testing.T) {
	probed := newFakeHost()
	pc := ctl(probed)
	run(t, pc, 2)

	observed := newFakeHost()
	oc := ctl(observed)
	for i := range 2 {
		at := now.Add(time.Duration(i) * time.Minute)
		oc.Observe("a", liveHeaders(), at)
		if _, err := oc.RunCycle(at); err != nil {
			t.Fatal(err)
		}
	}
	if len(observed.requests) != 0 {
		t.Fatalf("observed run still probed %d times", len(observed.requests))
	}

	want := pc.Engine().Accounts()["a"].Weight
	got := oc.Engine().Accounts()["a"].Weight
	if want == 0 || got != want {
		t.Fatalf("observed weight %d, probed weight %d: the same windows must score the same", got, want)
	}
}

func TestObserveRejectsAResponseWithoutQuota(t *testing.T) {
	h := newFakeHost()
	c := ctl(h)
	for name, header := range map[string]http.Header{
		"nil":            nil,
		"empty":          {},
		"unrelated only": {"Content-Type": {"application/json"}},
	} {
		if c.Observe("a", header, now) {
			t.Fatalf("%s: must not be recorded", name)
		}
	}
	run(t, c, 1)
	if len(h.requests) != 1 {
		t.Fatal("a rejected observation must leave the probe in charge")
	}
}

func TestObserveRejectsAnUnidentifiedCredential(t *testing.T) {
	// The host lists credentials before it assigns runtime indices. Filing an
	// observation under the empty id collapses the fleet onto a phantom account.
	c := ctl(newFakeHost())
	if c.Observe("", liveHeaders(), now) || c.Observe("   ", liveHeaders(), now) {
		t.Fatal("an observation with no auth index must be dropped")
	}
}

func TestObserveDoesNotWriteByItself(t *testing.T) {
	h := newFakeHost()
	c := ctl(h)
	run(t, c, 2) // past warmup, so a write is otherwise possible
	writes := len(h.saves)
	for range 50 {
		c.Observe("a", liveHeaders(), now)
	}
	if len(h.saves) != writes {
		t.Fatalf("observing wrote %d times: it must only feed the next cycle", len(h.saves)-writes)
	}
}

func TestObservationsForVanishedCredentialsAreDropped(t *testing.T) {
	h := newFakeHost()
	c := ctl(h)
	c.Observe("gone", liveHeaders(), now)
	run(t, c, 1)
	if n := c.Pending(); n != 0 {
		t.Fatalf("%d observations retained for credentials the host no longer lists", n)
	}
}

func TestObservationSurvivesACycleThatDidNotListItYet(t *testing.T) {
	// An observation that lands while a cycle is already running belongs to the
	// next cycle, not to the bin.
	h := newFakeHost()
	c := ctl(h)
	h.onGet = func(authIndex string, call int) { c.Observe("a", liveHeaders(), now) }
	run(t, c, 1)
	if n := c.Pending(); n != 1 {
		t.Fatalf("pending = %d, want the mid-cycle observation kept", n)
	}
}

func TestObservationIsScaledByCapacity(t *testing.T) {
	// Scaled() is what turns a bare percentage into an absolute quantity; an
	// observation that skipped it would report degraded and score on a
	// different axis than every probed account in the same fleet.
	h := newFakeHost()
	c := New(h, Options{Policy: balancer.DefaultPolicy(), Capacity: 1000})
	var r balancer.Report
	for i := range 3 {
		at := now.Add(time.Duration(i) * time.Minute)
		c.Observe("a", liveHeaders(), at)
		var err error
		if r, err = c.RunCycle(at); err != nil {
			t.Fatal(err)
		}
	}
	if len(r.Decisions) != 1 {
		t.Fatalf("got %d decisions", len(r.Decisions))
	}
	if r.Decisions[0].Degraded {
		t.Fatal("a capacity-scaled observation must not report as degraded")
	}
}

func TestObserveIsSafeUnderConcurrentCycles(t *testing.T) {
	h := newFakeHost()
	c := ctl(h)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range 8 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			for range 25 {
				c.Observe("a", liveHeaders(), now)
			}
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for i := range 25 {
			_, _ = c.RunCycle(now.Add(time.Duration(i) * time.Minute))
		}
	}()
	close(start)
	wg.Wait()
}

func TestObservationStampedAfterTheCycleClockIsStillUsable(t *testing.T) {
	// A cycle reads its clock before it drains, so a record can legitimately
	// carry a timestamp a hair past it. Compute rejects a snapshot observed in
	// the future, which would freeze an account that just reported fine.
	h := newFakeHost()
	c := ctl(h)
	for i := range 2 {
		at := now.Add(time.Duration(i) * time.Minute)
		c.Observe("a", liveHeaders(), at.Add(time.Millisecond))
		if _, err := c.RunCycle(at); err != nil {
			t.Fatal(err)
		}
	}
	if len(h.requests) != 0 {
		t.Fatalf("got %d probes, want none", len(h.requests))
	}
	if w := c.Engine().Accounts()["a"].Weight; w <= 0 {
		t.Fatalf("weight %d: a marginally-future observation must not freeze the account", w)
	}
}

func TestStoreKeepsTheNewestReading(t *testing.T) {
	// price() takes whatever the store holds as that credential's current
	// quota, so a record that arrives late but describes an earlier response
	// must not displace a newer one.
	fresh, stale := liveHeaders(), liveHeaders()
	fresh.Set("X-Codex-Secondary-Used-Percent", "5")
	stale.Set("X-Codex-Secondary-Used-Percent", "95")

	want := ctl(newFakeHost())
	got := ctl(newFakeHost())
	for i := range 2 {
		at := now.Add(time.Duration(i) * time.Minute)
		want.Observe("a", fresh, at)

		got.Observe("a", fresh, at)
		got.Observe("a", stale, at.Add(-time.Second))

		if _, err := want.RunCycle(at); err != nil {
			t.Fatal(err)
		}
		if _, err := got.RunCycle(at); err != nil {
			t.Fatal(err)
		}
	}
	if w, g := want.Engine().Accounts()["a"].Weight, got.Engine().Accounts()["a"].Weight; g != w {
		t.Fatalf("a stale record displaced a newer one: weight %d, want %d", g, w)
	}
}
