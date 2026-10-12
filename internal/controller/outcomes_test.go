package controller

import (
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/duanzhenyu/cpa-codex-weight-balancer/internal/balancer"
)

func penaltyCtl(h Host) *Controller {
	p := balancer.DefaultPolicy()
	p.ErrorPenalty = true
	return New(h, Options{Policy: p})
}

// note reports requests that each started at their own instant, the way
// distinct requests do.
func note(c *Controller, id string, requests, failures int) {
	for i := range requests {
		c.NoteOutcome(id, i < failures, now.Add(time.Duration(i)))
	}
}

func tick(t *testing.T, c *Controller, times int) {
	t.Helper()
	for range times {
		if _, err := c.Tick(now); err != nil {
			t.Fatal(err)
		}
	}
}

func errorRateOf(t *testing.T, c *Controller, id string, want float64) {
	t.Helper()
	if got := c.Engine().Accounts()[id].ErrorRate; !(math.Abs(got-want) <= 1e-9) {
		t.Fatalf("error rate %v, want %v", got, want)
	}
}

func TestOutcomesAreNotCountedWithTheSwitchOff(t *testing.T) {
	h := newFakeHost()
	c := ctl(h)
	if c.NoteOutcome("a", true, now) {
		t.Fatal("the switch is off: nothing may be recorded")
	}
	if n := len(c.Outcomes()); n != 0 {
		t.Fatalf("%d tallies pending, want none", n)
	}
}

func TestOutcomesFeedTheNextCycleOnce(t *testing.T) {
	h := newFakeHost()
	c := penaltyCtl(h)
	note(c, "a", 10, 3)
	if got := c.Outcomes()["a"]; got != (balancer.Outcomes{Requests: 10, Failures: 3}) {
		t.Fatalf("tally %+v", got)
	}
	tick(t, c, 1)
	errorRateOf(t, c, "a", 0.3)
	if n := len(c.Outcomes()); n != 0 {
		t.Fatalf("%d tallies left after the tick, want them consumed", n)
	}
	note(c, "a", 10, 0)
	tick(t, c, 1)
	errorRateOf(t, c, "a", 0.15)
}

func TestTicksAgeErrorsOutOfTheWindow(t *testing.T) {
	c := penaltyCtl(newFakeHost())
	note(c, "a", 10, 10)
	tick(t, c, 1)
	errorRateOf(t, c, "a", 1)
	if _, err := c.Tick(now.Add(balancer.DefaultErrorWindow)); err != nil {
		t.Fatal(err)
	}
	errorRateOf(t, c, "a", 0)
}

func TestOutcomesWithoutAnAuthIndexAreDropped(t *testing.T) {
	c := penaltyCtl(newFakeHost())
	if c.NoteOutcome("  ", true, now) {
		t.Fatal("an empty auth index must not be recorded")
	}
}

func TestOutcomesSurviveAFailedCredentialRead(t *testing.T) {
	// The tally says nothing about the credential file. Dropping it because the
	// read failed would hide the failures of an account that is failing requests.
	h := newFakeHost()
	h.getErr["a"] = errors.New("read failed")
	c := penaltyCtl(h)
	note(c, "a", 10, 10)
	tick(t, c, 1)
	errorRateOf(t, c, "a", 1)
}

func TestOutcomesOfUnlistedCredentialsAreDiscarded(t *testing.T) {
	h := newFakeHost()
	c := penaltyCtl(h)
	note(c, "ghost", 10, 10)
	tick(t, c, 1)
	if _, ok := c.Engine().Accounts()["ghost"]; ok {
		t.Fatal("a credential the host does not list must not enter the engine")
	}
	if n := len(c.Outcomes()); n != 0 {
		t.Fatalf("%d tallies left, want the unlisted one discarded", n)
	}
}

func TestOnlyATickConsumesOutcomes(t *testing.T) {
	// A refresh and the cycle a reload starts with price with the rate the last
	// scheduled cycle settled; the tally waits for the ticker.
	h := newFakeHost()
	c := penaltyCtl(h)
	note(c, "a", 10, 10)
	run(t, c, 3)
	errorRateOf(t, c, "a", 0)
	if got := c.Outcomes()["a"]; got != (balancer.Outcomes{Requests: 10, Failures: 10}) {
		t.Fatalf("tally %+v, want it left for the tick", got)
	}
	tick(t, c, 1)
	errorRateOf(t, c, "a", 1)
}

func TestARequestReportedTwiceCountsOnce(t *testing.T) {
	// An image-generation tool call reaches the hook as two served records of one
	// attempt: the request, then the tool model's usage, both stamped with the
	// attempt's start. The second can land after a tick.
	c := penaltyCtl(newFakeHost())
	if !c.NoteOutcome("a", false, now) || c.NoteOutcome("a", false, now) {
		t.Fatal("the duplicate of a served record must not count")
	}
	tick(t, c, 1)
	if c.NoteOutcome("a", false, now) {
		t.Fatal("the duplicate must not count after a tick either")
	}
	if !c.NoteOutcome("a", true, now) {
		t.Fatal("a failure is never a duplicate of a success")
	}
	for range 2 {
		if !c.NoteOutcome("b", false, time.Time{}) {
			t.Fatal("without a start time there is nothing to match on")
		}
	}
}

func TestOutcomesAreConservedUnderConcurrency(t *testing.T) {
	// The hook notes from the host's queue while ticks drain.
	c := penaltyCtl(newFakeHost())
	const writers, each = 8, 500
	start := make(chan struct{})
	var wg sync.WaitGroup
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := range each {
				c.NoteOutcome("a", i%2 == 0, now.Add(time.Duration(w*each+i)))
			}
		}()
	}
	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	var drained balancer.Outcomes
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-start
		for {
			for _, o := range c.outcomes.drain() {
				drained.Requests += o.Requests
				drained.Failures += o.Failures
			}
			select {
			case <-finished:
				return
			default:
			}
		}
	}()
	close(start)
	<-done
	left := c.Outcomes()["a"]
	if got := drained.Requests + left.Requests; got != writers*each {
		t.Fatalf("%d requests accounted for, want %d", got, writers*each)
	}
	if got := drained.Failures + left.Failures; got != writers*each/2 {
		t.Fatalf("%d failures accounted for, want %d", got, writers*each/2)
	}
}
