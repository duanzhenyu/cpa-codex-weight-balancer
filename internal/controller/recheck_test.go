package controller

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/duanzhenyu/cpa-codex-weight-balancer/internal/authfile"
	"github.com/duanzhenyu/cpa-codex-weight-balancer/internal/balancer"
)

// spentUsage is the shape /wham/usage answered, live on 2026-09-14, for a team
// account whose requests were being rejected with usage_limit_reached.
const spentUsage = `{"rate_limit":{"allowed":false,"limit_reached":true,
  "primary_window":{"used_percent":100,"limit_window_seconds":18000,"reset_after_seconds":7200},
  "secondary_window":{"used_percent":27,"limit_window_seconds":604800,"reset_after_seconds":400000}}}`

// busyUsage has quota left but far less than usageBody, so pricing it would move
// the bucket.
const busyUsage = `{"rate_limit":{
  "primary_window":{"used_percent":60,"limit_window_seconds":18000,"reset_after_seconds":7200},
  "secondary_window":{"used_percent":95,"limit_window_seconds":604800,"reset_after_seconds":400000}}}`

var rejectedAt = now.Add(2 * time.Minute)

// warmCtl returns a controller that already owns credential "a", with the host's
// call log cleared.
func warmCtl(t *testing.T, opts ...func(*Options)) (*fakeHost, *Controller) {
	t.Helper()
	h := newFakeHost()
	o := Options{Policy: balancer.DefaultPolicy()}
	for _, fn := range opts {
		fn(&o)
	}
	c := New(h, o)
	run(t, c, 2)
	h.requests, h.saves, h.logs = nil, nil, nil
	return h, c
}

func recheck(t *testing.T, c *Controller, at time.Time) balancer.Report {
	t.Helper()
	r, err := c.Recheck(at)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// exclude drives "a" through a rejection whose probe finds the short window spent.
func exclude(t *testing.T, h *fakeHost, c *Controller) {
	t.Helper()
	h.usage = spentUsage
	c.NoteRejected("a")
	recheck(t, c, rejectedAt)
	if c.Engine().Accounts()["a"].State != balancer.StateUnavailable {
		t.Fatal("setup: the spent account was not excluded")
	}
	h.requests, h.saves, h.logs = nil, nil, nil
}

func savedWeight(t *testing.T, s save) int64 {
	t.Helper()
	w, ok := authfile.Weight(s.Raw)
	if !ok {
		t.Fatal("save carries no weight")
	}
	return w
}

func TestRejectionExcludesASpentAccount(t *testing.T) {
	h, c := warmCtl(t)
	h.usage = spentUsage
	c.NoteRejected("a")

	r := recheck(t, c, rejectedAt)
	if n := len(h.byMethod("GET")); n != 1 {
		t.Fatalf("got %d probes, want 1", n)
	}
	if len(h.saves) != 1 || savedWeight(t, h.saves[0]) != 0 || r.Wrote != 1 {
		t.Fatalf("saves %+v wrote %d, want weight 0 written once", h.saves, r.Wrote)
	}
	if got := c.Engine().Accounts()["a"].RecoverAt; !got.Equal(rejectedAt.Add(2 * time.Hour)) {
		t.Fatalf("RecoverAt %v, want the 5h reset", got)
	}
}

func TestRejectionIsCheckedByProbeNotByAPendingObservation(t *testing.T) {
	// The reading a served request left behind was stamped before the budget
	// ran out. That is how the live account stayed at full weight for a cycle.
	h, c := warmCtl(t)
	c.Observe("a", liveHeaders(), rejectedAt.Add(-time.Second))
	h.usage = spentUsage
	c.NoteRejected("a")

	recheck(t, c, rejectedAt)
	if n := len(h.byMethod("GET")); n != 1 {
		t.Fatalf("got %d probes, want the rejection checked by probe", n)
	}
	if len(h.saves) != 1 || savedWeight(t, h.saves[0]) != 0 {
		t.Fatalf("saves %+v, want the exclusion written", h.saves)
	}
}

func TestRejectionWithQuotaLeftWritesNothing(t *testing.T) {
	// Not every 429 is a spent budget. Re-pricing off the timer would wipe the
	// host's accumulator once per rejection.
	h, c := warmCtl(t)
	h.usage = busyUsage
	c.NoteRejected("a")

	r := recheck(t, c, rejectedAt)
	if len(h.byMethod("GET")) != 1 {
		t.Fatal("the rejection must still be checked")
	}
	if len(h.saves) != 0 || r.Changed != 0 {
		t.Fatalf("saves %+v changed %d, want nothing written", h.saves, r.Changed)
	}
}

func TestRejectionsAreCoalescedAndSpaced(t *testing.T) {
	h, c := warmCtl(t)
	h.usage = busyUsage
	for range 100 {
		c.NoteRejected("a")
	}
	if n := c.Rejections(); n != 1 {
		t.Fatalf("Rejections %d, want one credential pending", n)
	}
	recheck(t, c, rejectedAt)
	if n := len(h.byMethod("GET")); n != 1 {
		t.Fatalf("got %d probes for a burst, want 1", n)
	}

	c.NoteRejected("a")
	recheck(t, c, rejectedAt.Add(30*time.Second))
	if n := len(h.byMethod("GET")); n != 1 {
		t.Fatalf("got %d probes, want none inside the gap", n)
	}
	// The last rejection of a burst is the one that matters most, so one that
	// lands inside the gap is held, not dropped.
	if c.Rejections() != 1 {
		t.Fatal("a rejection deferred by the gap must stay pending")
	}
	if at, ok := c.NextRecheck(rejectedAt); !ok || !at.Equal(rejectedAt.Add(RecheckGap)) {
		t.Fatalf("NextRecheck %v ok=%v, want the end of the gap", at, ok)
	}
	recheck(t, c, rejectedAt.Add(RecheckGap))
	if n := len(h.byMethod("GET")); n != 2 {
		t.Fatalf("got %d probes, want the deferred rejection checked", n)
	}
	if _, ok := c.NextRecheck(rejectedAt); ok {
		t.Fatal("nothing is left to recheck")
	}
}

func TestExcludedAccountIsNotReprobedPerRejection(t *testing.T) {
	h, c := warmCtl(t)
	exclude(t, h, c)
	c.NoteRejected("a")

	recheck(t, c, rejectedAt.Add(10*time.Minute))
	if len(h.requests) != 0 {
		t.Fatalf("got %d requests, want none before the exclusion falls due", len(h.requests))
	}
	if !c.LastRecheck().Equal(rejectedAt) {
		t.Fatalf("LastRecheck %v, want the recheck that probed", c.LastRecheck())
	}
}

func TestExclusionIsRecheckedAtItsReset(t *testing.T) {
	h, c := warmCtl(t)
	exclude(t, h, c)
	due := rejectedAt.Add(2*time.Hour + RecoveryGrace)
	if at, ok := c.NextRecheck(rejectedAt); !ok || !at.Equal(due) {
		t.Fatalf("NextRecheck %v ok=%v, want the reset plus grace %v", at, ok, due)
	}

	h.usage = usageBody
	recheck(t, c, due.Add(-time.Second))
	if len(h.requests) != 0 {
		t.Fatal("must not probe before the grace has passed")
	}
	recheck(t, c, due)
	if len(h.byMethod("GET")) != 1 || len(h.saves) != 1 || savedWeight(t, h.saves[0]) <= 0 {
		t.Fatalf("probes %d saves %+v, want re-admission with a positive weight", len(h.byMethod("GET")), h.saves)
	}
	if _, ok := c.NextRecheck(rejectedAt); ok {
		t.Fatal("a re-admitted account has nothing left to recheck")
	}
}

func TestStillSpentAtTheResetRetriesNoSoonerThanTheGap(t *testing.T) {
	h, c := warmCtl(t)
	exclude(t, h, c)
	due := rejectedAt.Add(2*time.Hour + RecoveryGrace)

	h.usage = strings.Replace(spentUsage, `"reset_after_seconds":7200`, `"reset_after_seconds":5`, 1)
	recheck(t, c, due)
	if len(h.byMethod("GET")) != 1 || len(h.saves) != 0 {
		t.Fatalf("probes %d saves %+v, want one probe and the exclusion held", len(h.byMethod("GET")), h.saves)
	}
	if at, ok := c.NextRecheck(rejectedAt); !ok || at.Before(due.Add(RecheckGap)) {
		t.Fatalf("NextRecheck %v ok=%v, want no sooner than %v", at, ok, due.Add(RecheckGap))
	}
}

func TestCycleProbesAnExcludedAccountInsteadOfTrustingAnObservation(t *testing.T) {
	// A request that began before the budget ran out can finish after the
	// exclusion, and its headers still describe quota the account no longer has.
	h, c := warmCtl(t)
	exclude(t, h, c)
	c.Observe("a", liveHeaders(), rejectedAt.Add(time.Minute))

	if _, err := c.RunCycle(rejectedAt.Add(5 * time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(h.byMethod("GET")) != 1 {
		t.Fatal("an excluded account must be probed")
	}
	if len(h.saves) != 0 {
		t.Fatalf("saves %+v: re-admitted on a stale observation", h.saves)
	}
}

func TestUnlistedExclusionSchedulesNothing(t *testing.T) {
	// Recheck skips a credential the host no longer serves, so an exclusion
	// falling due there would stay due forever and spin the worker's timer.
	h, c := warmCtl(t)
	exclude(t, h, c)
	h.entries[0].Disabled = true

	recheck(t, c, rejectedAt.Add(3*time.Hour))
	if at, ok := c.NextRecheck(rejectedAt); ok {
		t.Fatalf("NextRecheck %v for a disabled credential, want nothing", at)
	}

	h.entries[0].Disabled = false
	if _, err := c.RunCycle(rejectedAt.Add(3 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.NextRecheck(rejectedAt); !ok {
		t.Fatal("a re-enabled credential that is still excluded must be scheduled again")
	}
}

func TestRecheckSkipsCredentialsItMustNotTouch(t *testing.T) {
	h, c := warmCtl(t)
	h.entries = append(h.entries,
		AuthEntry{AuthIndex: "c", Name: "c.json", Type: "codex", Disabled: true},
		AuthEntry{AuthIndex: "g", Name: "g.json", Type: "gemini-cli"},
	)
	h.files["c"], h.files["g"] = []byte(`{"access_token":"t"}`), []byte(`{"access_token":"t"}`)
	h.names["c"], h.names["g"] = "c.json", "g.json"
	h.usage = spentUsage

	if c.NoteRejected("") || c.NoteRejected("   ") {
		t.Fatal("a rejection with no auth index must be dropped")
	}
	for _, id := range []string{"c", "g", "gone"} {
		c.NoteRejected(id)
	}
	recheck(t, c, rejectedAt)
	if len(h.requests) != 0 || len(h.saves) != 0 {
		t.Fatalf("requests %d saves %d, want none", len(h.requests), len(h.saves))
	}
}

func TestRejectionLeftByAFailedListingIsRetried(t *testing.T) {
	// The wake that carried the rejection is spent and no later 429 is owed, so
	// without a time of its own the rejection waits for the ticker.
	h, c := warmCtl(t)
	h.usage = spentUsage
	h.listErr = errors.New("host busy")
	c.NoteRejected("a")
	if _, err := c.Recheck(rejectedAt); err == nil {
		t.Fatal("setup: the listing must fail")
	}

	at, ok := c.NextRecheck(rejectedAt)
	if !ok || at.After(rejectedAt) {
		t.Fatalf("NextRecheck %v ok=%v, want the rejection due now", at, ok)
	}
	h.listErr = nil
	recheck(t, c, at)
	if len(h.saves) != 1 || savedWeight(t, h.saves[0]) != 0 {
		t.Fatalf("saves %+v, want the retried rejection to exclude the account", h.saves)
	}
}

func TestRecheckStateSurvivesAControllerHandover(t *testing.T) {
	// The host rebuilds the worker on a config change. A rejection queued on the
	// old one would wait for the ticker, and a lost gap lets the new one probe
	// again at once.
	h, c := warmCtl(t)
	h.usage = busyUsage
	c.NoteRejected("a")
	recheck(t, c, rejectedAt)
	c.NoteRejected("a")

	next := New(h, Options{Policy: balancer.DefaultPolicy()})
	next.RestoreRechecks(c.Rechecks())
	c.NoteRejected("b")
	if n := next.Rejections(); n != 1 {
		t.Fatalf("Rejections %d, want the one queued before the handover and nothing after", n)
	}

	h.requests = nil
	if _, err := next.Recheck(rejectedAt.Add(30 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if len(h.requests) != 0 {
		t.Fatal("the gap must carry across the handover")
	}
	if at, ok := next.NextRecheck(rejectedAt); !ok || !at.Equal(rejectedAt.Add(RecheckGap)) {
		t.Fatalf("NextRecheck %v ok=%v, want the end of the carried gap", at, ok)
	}
}

func TestHandoverKeepsExclusionsScheduledBeforeTheFirstListing(t *testing.T) {
	// The new worker's first listing can fail. Without the old listing its
	// exclusions get no recovery timer and wait out a whole ticker interval.
	h, c := warmCtl(t)
	exclude(t, h, c)

	next := New(h, Options{Policy: balancer.DefaultPolicy()})
	next.Engine().Restore(c.Engine().Accounts(), c.Engine().Written())
	next.RestoreRechecks(c.Rechecks())
	if at, ok := next.NextRecheck(rejectedAt); !ok || !at.Equal(rejectedAt.Add(2*time.Hour+RecoveryGrace)) {
		t.Fatalf("NextRecheck %v ok=%v, want the carried exclusion scheduled", at, ok)
	}
	if !next.LastRecheck().Equal(rejectedAt) {
		t.Fatalf("LastRecheck %v, want it carried across", next.LastRecheck())
	}
}

func TestFailedRecheckProbeIsRetriedOnce(t *testing.T) {
	// A host that cools the account after its 429 sends no further rejection
	// to re-queue it, so one failed probe would otherwise drop the recheck.
	// Retrying forever would probe a down endpoint every minute without end.
	h, c := warmCtl(t)
	h.httpErr = errors.New("usage endpoint down")
	c.NoteRejected("a")

	recheck(t, c, rejectedAt)
	if at, ok := c.NextRecheck(rejectedAt); !ok || !at.Equal(rejectedAt.Add(RecheckGap)) {
		t.Fatalf("NextRecheck %v ok=%v, want one retry after the gap", at, ok)
	}
	recheck(t, c, rejectedAt.Add(RecheckGap))
	if at, ok := c.NextRecheck(rejectedAt.Add(RecheckGap)); ok {
		t.Fatalf("NextRecheck %v: a probe that keeps failing must not be retried again", at)
	}
	if n := len(h.byMethod("GET")); n != 2 {
		t.Fatalf("got %d probes, want the attempt and one retry", n)
	}

	// A fresh 429 is fresh evidence and earns its own retry.
	c.NoteRejected("a")
	recheck(t, c, rejectedAt.Add(2*RecheckGap))
	if _, ok := c.NextRecheck(rejectedAt.Add(2 * RecheckGap)); !ok {
		t.Fatal("a new rejection must get its own retry")
	}
}

func TestRejectionAfterASuccessfulRetryEarnsItsOwnRetry(t *testing.T) {
	h, c := warmCtl(t)
	h.usage = busyUsage
	down := errors.New("usage endpoint down")
	h.httpErr = down
	c.NoteRejected("a")
	recheck(t, c, rejectedAt)
	h.httpErr = nil
	recheck(t, c, rejectedAt.Add(RecheckGap))

	h.httpErr = down
	c.NoteRejected("a")
	recheck(t, c, rejectedAt.Add(2*RecheckGap))
	if _, ok := c.NextRecheck(rejectedAt.Add(2 * RecheckGap)); !ok {
		t.Fatal("a new rejection must get its own retry even after an earlier retry succeeded")
	}
}

func TestDryRunRecheckSavesNothing(t *testing.T) {
	h, c := warmCtl(t, func(o *Options) { o.DryRun = true })
	h.usage = spentUsage
	c.NoteRejected("a")

	recheck(t, c, rejectedAt)
	if len(h.saves) != 0 {
		t.Fatalf("dry run saved %+v", h.saves)
	}
	if !slicesContain(h.logs, "dry run") {
		t.Fatalf("logs %v, want the would-be write reported", h.logs)
	}
}

func slicesContain(lines []string, part string) bool {
	for _, l := range lines {
		if strings.Contains(l, part) {
			return true
		}
	}
	return false
}

func TestRecheckIsSafeAlongsideCyclesAndRejections(t *testing.T) {
	// The usage hook notes rejections while the worker rechecks and cycles and
	// the status route reads. Run with -race; the barrier makes them overlap.
	h, c := warmCtl(t)
	h.usage = spentUsage

	var wg sync.WaitGroup
	start := make(chan struct{})
	for _, work := range []func(int){
		func(int) { c.NoteRejected("a") },
		func(i int) { c.Recheck(rejectedAt.Add(time.Duration(i) * time.Second)) },
		func(i int) { c.RunCycle(rejectedAt.Add(time.Duration(i) * time.Second)) },
		func(int) { c.NextRecheck(rejectedAt) },
		func(int) { c.Rejections(); c.LastRecheck() },
	} {
		wg.Add(1)
		go func(work func(int)) {
			defer wg.Done()
			<-start
			for i := range 200 {
				work(i)
			}
		}(work)
	}
	close(start)
	wg.Wait()
}
