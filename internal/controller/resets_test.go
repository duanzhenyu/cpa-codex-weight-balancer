package controller

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/duanzhenyu/cpa-codex-weight-balancer/internal/balancer"
)

func creditsBody(expiries ...time.Time) string {
	items := make([]string, 0, len(expiries))
	for _, e := range expiries {
		items = append(items, fmt.Sprintf(`{"reset_type":"codex_rate_limits","is_supported_by_plan":true,"status":"available","expires_at":%q}`, e.Format(time.RFC3339)))
	}
	return `{"credits":[` + strings.Join(items, ",") + `]}`
}

// pair lists two credentials on the same quota, of which only "b" holds a
// credit.
func pair() *fakeHost {
	h := newFakeHost()
	h.entries = append(h.entries, AuthEntry{AuthIndex: "b", Name: "b.json", Type: "codex"})
	h.files["b"] = []byte(`{"type":"codex","access_token":"tok-b","account_id":"acct-2"}`)
	h.names["b"] = "b.json"
	h.resets = map[string]string{"tok-b": creditsBody(now.Add(3 * 24 * time.Hour))}
	return h
}

func resetOpts() Options {
	return Options{Policy: balancer.DefaultPolicy(), ResetCredits: true}
}

func weightOf(t *testing.T, r balancer.Report, id string) int64 {
	t.Helper()
	for _, d := range r.Decisions {
		if d.AuthID == id {
			return d.Weight
		}
	}
	t.Fatalf("no decision for %q", id)
	return 0
}

func TestResetCreditsOffFetchesNothing(t *testing.T) {
	h := pair()
	r := run(t, ctl(h), 2)
	if n := len(h.byURL(ResetCreditsEndpoint)); n != 0 {
		t.Fatalf("fetched credits %d times with the switch off", n)
	}
	if weightOf(t, r, "a") != weightOf(t, r, "b") {
		t.Fatal("with the switch off credits must not move weights")
	}
}

func TestResetCreditsRequestShape(t *testing.T) {
	h := newFakeHost()
	run(t, New(h, resetOpts()), 1)
	reqs := h.byURL(ResetCreditsEndpoint)
	if len(reqs) != 1 || reqs[0].Method != "GET" {
		t.Fatalf("got %+v, want one GET", reqs)
	}
	for header, want := range map[string]string{
		"Authorization":      "Bearer tok",
		"Chatgpt-Account-Id": "acct-1",
		"Accept":             "application/json",
		"OpenAI-Beta":        "codex-1",
	} {
		if got := reqs[0].Headers[header]; len(got) != 1 || got[0] != want {
			t.Fatalf("header %s = %v, want %q", header, got, want)
		}
	}
}

func TestResetCreditsRaiseTheWrittenWeight(t *testing.T) {
	r := run(t, New(pair(), resetOpts()), 2)
	if weightOf(t, r, "b") <= weightOf(t, r, "a") {
		t.Fatalf("b %d, a %d: the credential holding a credit must weigh more", weightOf(t, r, "b"), weightOf(t, r, "a"))
	}
}

// Headers carry no credits, so an account priced from a served request still
// needs them fetched.
func TestObservedAccountStillCarriesItsCredits(t *testing.T) {
	h := pair()
	c := New(h, resetOpts())
	var r balancer.Report
	for i := range 2 {
		at := now.Add(time.Duration(i) * time.Minute)
		c.Observe("a", liveHeaders(), at)
		c.Observe("b", liveHeaders(), at)
		var err error
		if r, err = c.RunCycle(at); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(h.byURL(UsageEndpoint)); n != 0 {
		t.Fatalf("probed %d times, want none: both accounts were observed", n)
	}
	if weightOf(t, r, "b") <= weightOf(t, r, "a") {
		t.Fatalf("b %d, a %d: an observed account lost its credits", weightOf(t, r, "b"), weightOf(t, r, "a"))
	}
}

// A failed fetch says nothing about the credits. Reading it as "none" would
// reprice the account, and every reprice wipes the host's routing accumulator.
func TestResetCreditsFetchFailureKeepsTheLastList(t *testing.T) {
	undated := `{"credits":[{"reset_type":"codex_rate_limits","is_supported_by_plan":true,"status":"available"}]}`
	for name, fail := range map[string]func(*fakeHost){
		"non-2xx":        func(h *fakeHost) { h.resetsStatus = 503 },
		"transport":      func(h *fakeHost) { h.resetsErr = errors.New("connection reset") },
		"malformed JSON": func(h *fakeHost) { h.resets["tok-b"] = "not json" },
		"missing list":   func(h *fakeHost) { h.resets["tok-b"] = "{}" },
		"undated credit": func(h *fakeHost) { h.resets["tok-b"] = undated },
		"missing token": func(h *fakeHost) {
			h.files["b"] = []byte(strings.Replace(string(h.files["b"]), `"tok-b"`, `""`, 1))
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := pair()
			c := New(h, resetOpts())
			run(t, c, 2)
			saved, file := len(h.saves), h.files["b"]

			fail(h)
			if _, err := c.RunCycle(now.Add(2 * time.Minute)); err != nil {
				t.Fatal(err)
			}
			if len(h.saves) != saved {
				t.Fatalf("saves %+v: a failed fetch dropped the credits", h.saves[saved:])
			}
			state := c.ResetCredits()
			if len(state.Held["b"]) != 1 || state.Errors["b"] == "" {
				t.Fatalf("got %+v, want the last list kept and the failure reported", state)
			}

			h.resetsStatus, h.resetsErr = 0, nil
			h.resets["tok-b"], h.files["b"] = creditsBody(now.Add(3*24*time.Hour)), file
			if _, err := c.RunCycle(now.Add(3 * time.Minute)); err != nil {
				t.Fatal(err)
			}
			if err := c.ResetCredits().Errors["b"]; err != "" {
				t.Fatalf("error %q outlived a successful fetch", err)
			}
		})
	}
}

func TestResetCreditsAreScaledWithTheSnapshot(t *testing.T) {
	plain := run(t, New(pair(), resetOpts()), 2)
	opts := resetOpts()
	opts.Capacity = 5000
	scaled := run(t, New(pair(), opts), 2)
	for _, id := range []string{"a", "b"} {
		if got, want := weightOf(t, scaled, id), weightOf(t, plain, id); got != want {
			t.Fatalf("%s: capacity moved the weight from %d to %d", id, want, got)
		}
	}
}

// Re-admission commits a bucket, so it must price the way the next cycle will.
// Only the timer fetches: a recheck answers a 429 and works from the cache.
func TestRecheckPricesWithTheCachedCredits(t *testing.T) {
	readmit := func(on bool) (int64, *fakeHost) {
		h := newFakeHost()
		h.resets = map[string]string{"tok": creditsBody(now.Add(3 * 24 * time.Hour))}
		c := New(h, Options{Policy: balancer.DefaultPolicy(), ResetCredits: on})
		run(t, c, 2)
		exclude(t, h, c)
		h.requests, h.saves = nil, nil
		h.usage = usageBody
		recheck(t, c, rejectedAt.Add(2*time.Hour+RecoveryGrace))
		if len(h.saves) != 1 {
			t.Fatalf("saves %+v, want the re-admission", h.saves)
		}
		return savedWeight(t, h.saves[0]), h
	}
	plain, _ := readmit(false)
	banked, h := readmit(true)
	if banked <= plain {
		t.Fatalf("re-admitted at %d, %d without credits: the recheck ignored them", banked, plain)
	}
	if n := len(h.byURL(ResetCreditsEndpoint)); n != 0 {
		t.Fatalf("a recheck fetched credits %d times", n)
	}
}

func TestResetCreditsSurviveHandOver(t *testing.T) {
	h := pair()
	c := New(h, resetOpts())
	run(t, c, 1)
	next := New(h, resetOpts())
	next.RestoreResetCredits(c.ResetCredits())
	if got := next.ResetCredits().Held["b"]; len(got) != 1 {
		t.Fatalf("got %v, want the credit handed over", got)
	}
}

// A handed-over list must not keep pricing once the switch is turned off: no
// cycle refreshes it any more, so it would hold until the credits expire.
func TestResetCreditsOffIgnoresAHandedOverList(t *testing.T) {
	h := pair()
	c := New(h, resetOpts())
	run(t, c, 1)
	next := New(h, Options{Policy: balancer.DefaultPolicy()})
	next.RestoreResetCredits(c.ResetCredits())
	r := run(t, next, 2)
	if weightOf(t, r, "a") != weightOf(t, r, "b") {
		t.Fatal("with the switch off credits must not move weights")
	}
}

func TestResetCreditsForgetUnlistedCredentials(t *testing.T) {
	for name, list := range map[string]func(*Controller){
		"cycle":   func(c *Controller) { run(t, c, 1) },
		"recheck": func(c *Controller) { recheck(t, c, now.Add(time.Minute)) },
	} {
		h := pair()
		c := New(h, resetOpts())
		run(t, c, 1)
		h.resetsStatus = 503
		run(t, c, 1)
		h.entries = h.entries[:1]
		list(c)
		state := c.ResetCredits()
		if _, held := state.Held["b"]; held {
			t.Errorf("%s kept credits for a credential the host no longer lists", name)
		}
		if _, failed := state.Errors["b"]; failed {
			t.Errorf("%s kept a fetch error for a credential the host no longer lists", name)
		}
	}
}

// A copy out and a restore in must own their maps and expiry lists: sharing
// either lets a caller change the cache without its lock.
func TestRestoredResetCreditsAreCopied(t *testing.T) {
	h := pair()
	c := New(h, resetOpts())
	run(t, c, 1)
	h.resetsStatus = 503
	run(t, c, 1)
	state := c.ResetCredits()
	next := New(pair(), resetOpts())
	next.RestoreResetCredits(state)
	want := state.Held["b"][0]
	state.Held["b"][0] = time.Time{}
	delete(state.Held, "b")
	delete(state.Errors, "b")
	for _, got := range []ResetCreditsState{c.ResetCredits(), next.ResetCredits()} {
		if len(got.Held["b"]) != 1 || !got.Held["b"][0].Equal(want) || got.Errors["b"] == "" {
			t.Fatalf("state aliases the caller's maps or lists: %+v", got)
		}
	}
}

func TestResetCreditsAreSafeAlongsideCycles(t *testing.T) {
	// The status route reads the credits while the worker cycles and rechecks.
	// Run with -race; the barrier makes them overlap.
	h := pair()
	c := New(h, resetOpts())
	c.NoteRejected("b")

	var wg sync.WaitGroup
	start := make(chan struct{})
	for _, work := range []func(int){
		func(i int) { c.RunCycle(now.Add(time.Duration(i) * time.Second)) },
		func(i int) { c.NoteRejected("b"); c.Recheck(now.Add(time.Duration(i) * time.Minute)) },
		func(int) { c.ResetCredits() },
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
