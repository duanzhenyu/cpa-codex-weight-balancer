package controller

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/duanzhenyu/cpa-codex-weight-balancer/internal/balancer"
	"github.com/duanzhenyu/cpa-codex-weight-balancer/internal/codex"
)

// A Codex window starts at the account's first metered request, so a credential
// nothing has ever used reports a reset exactly one window away and keeps
// reporting it. Verified live on 2026-09-12: the probe does not start the
// window, only inference does.
const dormantUsage = `{"rate_limit":{
  "primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_after_seconds":18000},
  "secondary_window":{"used_percent":0,"limit_window_seconds":604800,"reset_after_seconds":604800}}}`

// The short window has merely rolled over here: the account's week is running,
// but nothing has been spent in the last five hours, so Codex reports the short
// budget untouched with a full span to go.
const shortDormantUsage = `{"rate_limit":{
  "primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_after_seconds":18000},
  "secondary_window":{"used_percent":3,"limit_window_seconds":604800,"reset_after_seconds":604177}}}`

func shortCtl(h Host, opts ...func(*Options)) *Controller {
	o := Options{Policy: balancer.DefaultPolicy(), KickstartShortWindow: true}
	for _, fn := range opts {
		fn(&o)
	}
	return New(h, o)
}

func shortDormantHost() *fakeHost {
	h := newFakeHost()
	h.usage = shortDormantUsage
	return h
}

func kickCtl(h Host, opts ...func(*Options)) *Controller {
	o := Options{Policy: balancer.DefaultPolicy(), Kickstart: true}
	for _, fn := range opts {
		fn(&o)
	}
	return New(h, o)
}

func dormantHost() *fakeHost {
	h := newFakeHost()
	h.usage = dormantUsage
	return h
}

func TestDormantAccountIsKickstarted(t *testing.T) {
	h := dormantHost()
	run(t, kickCtl(h), 1)

	posts := h.byMethod("POST")
	if len(posts) != 1 {
		t.Fatalf("got %d inference requests, want 1", len(posts))
	}
	if posts[0].URL != KickstartEndpoint {
		t.Fatalf("posted to %q", posts[0].URL)
	}
	if got := posts[0].Headers["Authorization"]; len(got) != 1 || got[0] != "Bearer tok" {
		t.Fatalf("authorization %v", got)
	}
	if got := posts[0].Headers["Chatgpt-Account-Id"]; len(got) != 1 || got[0] != "acct-1" {
		t.Fatalf("account header %v", got)
	}
	var body map[string]any
	if err := json.Unmarshal(posts[0].Body, &body); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if body["model"] != DefaultKickstartModel {
		t.Fatalf("model %v, want %s", body["model"], DefaultKickstartModel)
	}
}

func TestKickstartModelIsConfigurable(t *testing.T) {
	h := dormantHost()
	run(t, kickCtl(h, func(o *Options) { o.KickstartModel = "gpt-5.6-luna" }), 1)

	posts := h.byMethod("POST")
	if len(posts) != 1 {
		t.Fatalf("got %d inference requests", len(posts))
	}
	var body map[string]any
	if err := json.Unmarshal(posts[0].Body, &body); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if body["model"] != "gpt-5.6-luna" {
		t.Fatalf("model %v", body["model"])
	}
}

func TestRunningAccountIsNeverKickstarted(t *testing.T) {
	h := newFakeHost() // the default fixture is 45% into its week
	run(t, kickCtl(h), 2)
	if posts := h.byMethod("POST"); len(posts) != 0 {
		t.Fatalf("kickstarted a running account: %v", posts)
	}
}

func TestKickstartIsOffByDefault(t *testing.T) {
	h := dormantHost()
	run(t, New(h, Options{Policy: balancer.DefaultPolicy()}), 2)
	if posts := h.byMethod("POST"); len(posts) != 0 {
		t.Fatalf("spent quota without being asked: %v", posts)
	}
}

// dry_run is the operator's promise that the plugin changes nothing upstream.
// Spending quota under it would break that promise more severely than a write.
func TestDryRunNeverKickstarts(t *testing.T) {
	h := dormantHost()
	run(t, kickCtl(h, func(o *Options) { o.DryRun = true }), 2)
	if posts := h.byMethod("POST"); len(posts) != 0 {
		t.Fatalf("dry run spent quota: %v", posts)
	}
}

// A kickstart that returns 200 but leaves the window dormant would otherwise
// repeat every cycle, billing the account forever.
func TestKickstartCooldownSuppressesRepeats(t *testing.T) {
	h := dormantHost()
	run(t, kickCtl(h), 4)
	if posts := h.byMethod("POST"); len(posts) != 1 {
		t.Fatalf("got %d inference requests across 4 cycles, want 1", len(posts))
	}
}

func TestKickstartRetriesAfterTheCooldown(t *testing.T) {
	h := dormantHost()
	c := kickCtl(h, func(o *Options) { o.KickstartCooldown = 30 * time.Minute })
	if _, err := c.RunCycle(now); err != nil {
		t.Fatalf("first cycle: %v", err)
	}
	if _, err := c.RunCycle(now.Add(31 * time.Minute)); err != nil {
		t.Fatalf("second cycle: %v", err)
	}
	if posts := h.byMethod("POST"); len(posts) != 2 {
		t.Fatalf("got %d inference requests, want 2", len(posts))
	}
}

// The account is priced from its quota, which the kickstart does not change.
// A failure here must cost nothing but the attempt.
func TestKickstartFailureLeavesTheWeightAlone(t *testing.T) {
	healthy := dormantHost()
	want := run(t, kickCtl(healthy), 3)

	for name, break_ := range map[string]func(*fakeHost){
		"transport": func(h *fakeHost) { h.kickErr = errors.New("dial tcp: refused") },
		"rejected":  func(h *fakeHost) { h.kickStatus = 400 },
	} {
		t.Run(name, func(t *testing.T) {
			h := dormantHost()
			break_(h)
			got := run(t, kickCtl(h), 3)
			if len(got.Decisions) != len(want.Decisions) {
				t.Fatalf("decision count %d, want %d", len(got.Decisions), len(want.Decisions))
			}
			for i := range got.Decisions {
				if got.Decisions[i].Weight != want.Decisions[i].Weight {
					t.Fatalf("weight %d, want %d", got.Decisions[i].Weight, want.Decisions[i].Weight)
				}
				if got.Decisions[i].Error != "" {
					t.Fatalf("kickstart failure leaked into the probe error: %q", got.Decisions[i].Error)
				}
			}
		})
	}
}

// Codex fills these headers before metering the request that carries them, so a
// served response can report a window that its own request has just started.
// Trusting it would price the account as dormant and kickstart it again.
func TestDormantObservationIsDiscardedInFavourOfAProbe(t *testing.T) {
	h := dormantHost()
	c := kickCtl(h)
	if !c.Observe("a", http.Header{
		"X-Codex-Primary-Used-Percent":          {"0"},
		"X-Codex-Primary-Window-Minutes":        {"300"},
		"X-Codex-Primary-Reset-After-Seconds":   {"18000"},
		"X-Codex-Secondary-Used-Percent":        {"0"},
		"X-Codex-Secondary-Window-Minutes":      {"10080"},
		"X-Codex-Secondary-Reset-After-Seconds": {"604800"},
	}, now) {
		t.Fatal("headers should have parsed")
	}
	if _, err := c.RunCycle(now); err != nil {
		t.Fatalf("cycle: %v", err)
	}
	if gets := h.byMethod("GET"); len(gets) != 1 {
		t.Fatalf("got %d probes, want the dormant reading replaced by one", len(gets))
	}
	if c.Pending() != 0 {
		t.Fatalf("observation still held: %d", c.Pending())
	}
}

// A live reading that is not dormant is still cheaper than a probe.
func TestRunningObservationStillSparesTheProbe(t *testing.T) {
	h := dormantHost()
	c := kickCtl(h)
	if !c.Observe("a", http.Header{
		"X-Codex-Secondary-Used-Percent":        {"3"},
		"X-Codex-Secondary-Window-Minutes":      {"10080"},
		"X-Codex-Secondary-Reset-After-Seconds": {"604177"},
	}, now) {
		t.Fatal("headers should have parsed")
	}
	if _, err := c.RunCycle(now); err != nil {
		t.Fatalf("cycle: %v", err)
	}
	if gets := h.byMethod("GET"); len(gets) != 0 {
		t.Fatalf("probed despite a usable reading: %v", gets)
	}
	if posts := h.byMethod("POST"); len(posts) != 0 {
		t.Fatalf("kickstarted a running account: %v", posts)
	}
}

func TestKickstartStatusIsReportable(t *testing.T) {
	h := dormantHost()
	c := kickCtl(h)
	run(t, c, 1)
	got := c.Kickstarts()
	if got.Sent != 1 || got.LastErr != "" {
		t.Fatalf("sent=%d lastErr=%q", got.Sent, got.LastErr)
	}
	if !got.Dormant["a"].Weekly {
		t.Fatalf("dormant map %v", got.Dormant)
	}
	if got.Last["a"].IsZero() {
		t.Fatal("the attempt was not booked against the cooldown")
	}

	h2 := dormantHost()
	h2.kickStatus = 429
	c2 := kickCtl(h2)
	run(t, c2, 1)
	if got := c2.Kickstarts(); got.Sent != 0 || !strings.Contains(got.LastErr, "429") {
		t.Fatalf("sent=%d lastErr=%q", got.Sent, got.LastErr)
	}
}

// A kickstart that fails in flight may still have been metered upstream, so the
// cooldown starts at the attempt. Retrying a rejected nudge every cycle would
// bill the account on exactly the path that is already going wrong.
func TestAFailedKickstartAlsoStartsTheCooldown(t *testing.T) {
	h := dormantHost()
	h.kickStatus = 500
	run(t, kickCtl(h), 4)
	if posts := h.byMethod("POST"); len(posts) != 1 {
		t.Fatalf("got %d inference requests after a failure, want 1", len(posts))
	}
}

// The cycle writes weights between capturing a credential and nudging it, and
// host.auth.save replaces the whole document with no CAS — so the bytes read at
// the top of the cycle may already name a token that has since been refreshed.
func TestKickstartReReadsTheCredentialBeforeSpending(t *testing.T) {
	h := dormantHost()
	h.onGet = func(authIndex string, call int) {
		if call > 1 {
			h.files[authIndex] = []byte(`{"type":"codex","access_token":"fresh","account_id":"acct-1"}`)
		}
	}
	run(t, kickCtl(h), 1)

	posts := h.byMethod("POST")
	if len(posts) != 1 {
		t.Fatalf("got %d kickstarts, want 1", len(posts))
	}
	if got := posts[0].Headers["Authorization"]; len(got) != 1 || got[0] != "Bearer fresh" {
		t.Fatalf("kickstart used a stale token: %v", got)
	}
}

func TestKickstartFailsClosedWhenTheReReadDoes(t *testing.T) {
	h := dormantHost()
	c := kickCtl(h)
	h.onGet = func(authIndex string, call int) {
		if call > 1 {
			h.getErr[authIndex] = errors.New("store unavailable")
		}
	}
	if _, err := c.RunCycle(now); err != nil {
		t.Fatalf("a failed re-read must not fail the cycle: %v", err)
	}
	if posts := h.byMethod("POST"); len(posts) != 0 {
		t.Fatalf("spent quota without a fresh credential: %v", posts)
	}
	// "re-read failed" and "credential carries no access token" point at
	// entirely different problems, and the status route is the only place an
	// operator can tell them apart.
	got := c.Kickstarts().LastErr
	if !strings.Contains(got, "re-read") {
		t.Fatalf("last error %q does not name the re-read", got)
	}
	// The status route is an HTTP endpoint and must carry no file names. The
	// host's own error for a missing credential is free to name the document it
	// could not read, so it cannot be passed through.
	if strings.Contains(got, "store unavailable") {
		t.Fatalf("the host's error text reached the status route: %q", got)
	}
}

// A credential the host has taken out of service, or that says outright it is
// rate limited, answers a kickstart with an error and bills nothing useful.
func TestKickstartSkipsCredentialsThatCannotServe(t *testing.T) {
	for name, setup := range map[string]func(*fakeHost){
		"host marked it unavailable": func(h *fakeHost) { h.entries[0].Unavailable = true },
		"account reports limited": func(h *fakeHost) {
			h.usage = strings.Replace(dormantUsage, `"rate_limit":{`, `"rate_limit":{"limit_reached":true,`, 1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := dormantHost()
			setup(h)
			run(t, kickCtl(h), 1)
			if posts := h.byMethod("POST"); len(posts) != 0 {
				t.Fatalf("kickstarted a credential that cannot serve: %v", posts)
			}
		})
	}
}

// The host rebuilds the worker on every configuration change, far more often
// than a cooldown is long. A controller that starts with an empty cooldown map
// re-bills every credential whose window refused to start.
func TestKickstartStateSurvivesAControllerHandover(t *testing.T) {
	h := dormantHost()
	old := kickCtl(h)
	run(t, old, 1)
	if posts := h.byMethod("POST"); len(posts) != 1 {
		t.Fatalf("setup sent %d kickstarts", len(posts))
	}

	next := kickCtl(h)
	next.RestoreKickstarts(old.Kickstarts())
	if _, err := next.RunCycle(now.Add(time.Minute)); err != nil {
		t.Fatalf("cycle: %v", err)
	}
	if posts := h.byMethod("POST"); len(posts) != 1 {
		t.Fatalf("the handover lost the cooldown: %d kickstarts", len(posts))
	}
	if got := next.Kickstarts(); got.Sent != 1 {
		t.Fatalf("sent %d, want the count carried over", got.Sent)
	}
}

// Restore must copy, not alias: two controllers sharing one map would race the
// status route against the next cycle.
func TestRestoredKickstartStateIsCopied(t *testing.T) {
	h := dormantHost()
	old := kickCtl(h)
	run(t, old, 1)

	state := old.Kickstarts()
	next := kickCtl(newFakeHost())
	next.RestoreKickstarts(state)
	state.Last["a"] = time.Time{}
	state.Dormant["a"] = codex.Dormancy{}

	if got := next.Kickstarts(); got.Last["a"].IsZero() || !got.Dormant["a"].Weekly {
		t.Fatalf("restored state aliases the caller's maps: %+v", got)
	}
}

// The dormancy map is what the status route renders. Keyed by auth index and
// only ever updated per key, it would go on listing a credential the host has
// stopped serving — a diagnostic that lies for as long as the process lives.
func TestDormancyListForgetsCredentialsTheHostNoLongerLists(t *testing.T) {
	h := dormantHost()
	h.entries = append(h.entries, AuthEntry{AuthIndex: "b", Name: "b.json", Type: "codex"})
	h.files["b"] = []byte(`{"type":"codex","access_token":"tok","account_id":"acct-2"}`)
	h.names["b"] = "b.json"

	c := kickCtl(h)
	run(t, c, 1)
	if got := c.Kickstarts().Dormant; !got["a"].Any() || !got["b"].Any() {
		t.Fatalf("setup: %v", got)
	}

	h.entries = h.entries[:1]
	if _, err := c.RunCycle(now.Add(time.Minute)); err != nil {
		t.Fatalf("cycle: %v", err)
	}
	if got := c.Kickstarts().Dormant; got["b"].Any() {
		t.Fatalf("status still lists a credential the host dropped: %v", got)
	}
	if got := c.Kickstarts().Dormant; !got["a"].Any() {
		t.Fatalf("lost the credential that is still listed: %v", got)
	}
}

// The two switches select different budgets. A short window that has merely
// rolled over is no evidence the account's week has yet to start, so the weekly
// switch must leave it alone — and the short switch must not need the weekly
// one turned on to do its job.
func TestShortDormancyIsKickstartedOnlyByItsOwnSwitch(t *testing.T) {
	for name, tc := range map[string]struct {
		weekly, short bool
		want          int
	}{
		"neither switch": {false, false, 0},
		"weekly only":    {true, false, 0},
		"short only":     {false, true, 1},
		"both":           {true, true, 1},
	} {
		t.Run(name, func(t *testing.T) {
			h := shortDormantHost()
			run(t, New(h, Options{
				Policy:               balancer.DefaultPolicy(),
				Kickstart:            tc.weekly,
				KickstartShortWindow: tc.short,
			}), 1)
			if posts := h.byMethod("POST"); len(posts) != tc.want {
				t.Fatalf("got %d kickstarts, want %d", len(posts), tc.want)
			}
		})
	}
}

// One metered request starts both of an account's windows at the same instant:
// the fleet's plus account read primary 7917/18000 and secondary 594717/604800
// on 2026-09-12, both exactly 10083s in. So a fresh account satisfies both
// switches at once and must still cost exactly one request.
func TestBothBudgetsDormantStillCostsOneRequest(t *testing.T) {
	h := dormantHost()
	run(t, kickCtl(h, func(o *Options) { o.KickstartShortWindow = true }), 1)
	if posts := h.byMethod("POST"); len(posts) != 1 {
		t.Fatalf("got %d kickstarts, want 1", len(posts))
	}
}

func TestShortWindowKickstartIsSuppressedByDryRun(t *testing.T) {
	h := shortDormantHost()
	run(t, shortCtl(h, func(o *Options) { o.DryRun = true }), 2)
	if posts := h.byMethod("POST"); len(posts) != 0 {
		t.Fatalf("dry run spent quota: %v", posts)
	}
}

// The guards that keep a paid request off a credential that cannot serve apply
// to whichever budget triggered it.
func TestShortWindowKickstartSkipsCredentialsThatCannotServe(t *testing.T) {
	for name, setup := range map[string]func(*fakeHost){
		"host marked it unavailable": func(h *fakeHost) { h.entries[0].Unavailable = true },
		"account reports limited": func(h *fakeHost) {
			h.usage = strings.Replace(shortDormantUsage, `"rate_limit":{`, `"rate_limit":{"limit_reached":true,`, 1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := shortDormantHost()
			setup(h)
			run(t, shortCtl(h), 1)
			if posts := h.byMethod("POST"); len(posts) != 0 {
				t.Fatalf("kickstarted a credential that cannot serve: %v", posts)
			}
		})
	}
}

// The status route is the only diagnostic there is, and an operator running one
// switch needs to see which budget each credential is actually dormant on.
func TestTheTwoBudgetsAreReportedSeparately(t *testing.T) {
	rolled := shortDormantHost()
	c := shortCtl(rolled)
	run(t, c, 1)
	if got := c.Kickstarts().Dormant["a"]; got.Weekly || !got.Short {
		t.Fatalf("a rolled-over short window on a running week: %+v", got)
	}

	fresh := dormantHost()
	c2 := kickCtl(fresh)
	run(t, c2, 1)
	if got := c2.Kickstarts().Dormant["a"]; !got.Weekly || !got.Short {
		t.Fatalf("an untouched account is dormant on both: %+v", got)
	}
}

// A hook record exists only because the host served a request on that
// credential, and Codex stamps its headers before metering. So a reading that
// calls the short window untouched is evidence that this very request just
// started it — the short clock is already running. Probing on the strength of
// it would race Codex's backend, which took twelve seconds to begin decrementing
// when this was measured live, and buy a request the traffic already bought.
//
// The weekly window is different and stays discarded: it drives the score, so a
// pre-request reading of a week that has just begun is wrong where it counts.
func TestShortDormantObservationIsTrustedAndDefersTheKickstart(t *testing.T) {
	h := shortDormantHost()
	c := shortCtl(h)
	if !c.Observe("a", http.Header{
		"X-Codex-Primary-Used-Percent":          {"0"},
		"X-Codex-Primary-Window-Minutes":        {"300"},
		"X-Codex-Primary-Reset-After-Seconds":   {"18000"},
		"X-Codex-Secondary-Used-Percent":        {"3"},
		"X-Codex-Secondary-Window-Minutes":      {"10080"},
		"X-Codex-Secondary-Reset-After-Seconds": {"604177"},
	}, now) {
		t.Fatal("headers should have parsed")
	}
	if _, err := c.RunCycle(now); err != nil {
		t.Fatalf("cycle: %v", err)
	}
	if gets := h.byMethod("GET"); len(gets) != 0 {
		t.Fatalf("probed away a reading that still prices the week: %v", gets)
	}
	if posts := h.byMethod("POST"); len(posts) != 0 {
		t.Fatalf("kickstarted off a reading whose own request started the clock: %v", posts)
	}
	// The credential is only nudged once a cycle with no traffic behind it
	// confirms the window really is unstarted.
	if _, err := c.RunCycle(now.Add(time.Minute)); err != nil {
		t.Fatalf("confirming cycle: %v", err)
	}
	if posts := h.byMethod("POST"); len(posts) != 1 {
		t.Fatalf("got %d deferred kickstarts, want 1", len(posts))
	}
}
