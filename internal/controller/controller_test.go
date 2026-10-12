package controller

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/duanzhenyu/cpa-codex-weight-balancer/internal/authfile"
	"github.com/duanzhenyu/cpa-codex-weight-balancer/internal/balancer"
)

var now = time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)

const usageBody = `{"rate_limit":{
  "primary_window":{"used_percent":20,"limit_window_seconds":18000,"reset_after_seconds":7200},
  "secondary_window":{"used_percent":45,"limit_window_seconds":604800,"reset_after_seconds":400000}}}`

type save struct {
	Name string
	Raw  []byte
}

type fakeHost struct {
	entries []AuthEntry
	files   map[string][]byte // authIndex -> credential JSON
	names   map[string]string // authIndex -> file name
	usage   string
	status  int
	httpErr error
	getErr  map[string]error
	saves   []save
	saveErr error
	listErr error
	// kickStatus and kickErr answer the inference request, which has its own
	// failure modes: the probe can be healthy while a kickstart is rejected.
	kickStatus int
	kickErr    error
	// resets answers the reset-credits endpoint by access token; a token with
	// no entry holds no credits.
	resets       map[string]string
	resetsStatus int
	resetsErr    error
	requests     []HTTPRequest
	gets         []string
	logs         []string
	// onGet fires before returning a credential, to simulate a concurrent write.
	onGet func(authIndex string, call int)
}

func newFakeHost() *fakeHost {
	return &fakeHost{
		entries: []AuthEntry{{AuthIndex: "a", Name: "a.json", Type: "codex"}},
		files:   map[string][]byte{"a": []byte(`{"type":"codex","access_token":"tok","account_id":"acct-1"}`)},
		names:   map[string]string{"a": "a.json"},
		usage:   usageBody,
		status:  200,
		getErr:  map[string]error{},
	}
}

func (h *fakeHost) AuthList() ([]AuthEntry, error) { return h.entries, h.listErr }

func (h *fakeHost) AuthGet(authIndex string) (string, []byte, error) {
	h.gets = append(h.gets, authIndex)
	if h.onGet != nil {
		count := 0
		for _, g := range h.gets {
			if g == authIndex {
				count++
			}
		}
		h.onGet(authIndex, count)
	}
	if err := h.getErr[authIndex]; err != nil {
		return "", nil, err
	}
	raw, ok := h.files[authIndex]
	if !ok {
		return "", nil, fmt.Errorf("no such auth %q", authIndex)
	}
	return h.names[authIndex], append([]byte(nil), raw...), nil
}

func (h *fakeHost) AuthSave(name string, raw []byte) error {
	if h.saveErr != nil {
		return h.saveErr
	}
	h.saves = append(h.saves, save{Name: name, Raw: append([]byte(nil), raw...)})
	for idx, n := range h.names {
		if n == name {
			h.files[idx] = append([]byte(nil), raw...)
		}
	}
	return nil
}

func (h *fakeHost) HTTPDo(req HTTPRequest) (HTTPResponse, error) {
	h.requests = append(h.requests, req)
	if req.Method == "POST" {
		if h.kickErr != nil {
			return HTTPResponse{}, h.kickErr
		}
		status := h.kickStatus
		if status == 0 {
			status = 200
		}
		return HTTPResponse{StatusCode: status}, nil
	}
	if req.URL == ResetCreditsEndpoint {
		if h.resetsErr != nil {
			return HTTPResponse{}, h.resetsErr
		}
		status, body := h.resetsStatus, `{"credits":[]}`
		if status == 0 {
			status = 200
		}
		if held, ok := h.resets[strings.TrimPrefix(req.Headers["Authorization"][0], "Bearer ")]; ok {
			body = held
		}
		return HTTPResponse{StatusCode: status, Body: []byte(body)}, nil
	}
	if h.httpErr != nil {
		return HTTPResponse{}, h.httpErr
	}
	return HTTPResponse{StatusCode: h.status, Body: []byte(h.usage)}, nil
}

// byMethod isolates one transport's calls: a cycle that kickstarts issues both
// a probe and an inference request.
func (h *fakeHost) byMethod(method string) []HTTPRequest {
	var out []HTTPRequest
	for _, r := range h.requests {
		if r.Method == method {
			out = append(out, r)
		}
	}
	return out
}

func (h *fakeHost) byURL(url string) []HTTPRequest {
	var out []HTTPRequest
	for _, r := range h.requests {
		if r.URL == url {
			out = append(out, r)
		}
	}
	return out
}

func (h *fakeHost) Log(level, message string) { h.logs = append(h.logs, level+": "+message) }

func run(t *testing.T, c *Controller, times int) balancer.Report {
	t.Helper()
	var r balancer.Report
	for i := range times {
		var err error
		r, err = c.RunCycle(now.Add(time.Duration(i) * time.Minute))
		if err != nil {
			t.Fatalf("cycle %d: %v", i, err)
		}
	}
	return r
}

func ctl(h Host) *Controller { return New(h, Options{Policy: balancer.DefaultPolicy()}) }

func TestProbeRequestShape(t *testing.T) {
	h := newFakeHost()
	run(t, ctl(h), 1)
	if len(h.requests) != 1 {
		t.Fatalf("got %d requests", len(h.requests))
	}
	req := h.requests[0]
	if req.Method != "GET" || req.URL != UsageEndpoint {
		t.Fatalf("got %s %s", req.Method, req.URL)
	}
	for header, want := range map[string]string{
		"Authorization":      "Bearer tok",
		"Chatgpt-Account-Id": "acct-1",
		"Originator":         Originator,
		"User-Agent":         UserAgent,
	} {
		if got := req.Headers[header]; len(got) != 1 || got[0] != want {
			t.Fatalf("header %s = %v, want %q", header, got, want)
		}
	}
}

func TestSkipsNonCodexAndDisabled(t *testing.T) {
	h := newFakeHost()
	h.entries = append(h.entries,
		AuthEntry{AuthIndex: "b", Name: "b.json", Type: "gemini-cli"},
		AuthEntry{AuthIndex: "c", Name: "c.json", Type: "codex", Disabled: true},
	)
	h.files["b"] = []byte(`{"type":"gemini-cli"}`)
	h.files["c"] = []byte(`{"type":"codex","access_token":"t"}`)
	h.names["b"], h.names["c"] = "b.json", "c.json"

	r := run(t, ctl(h), 2)
	if len(r.Decisions) != 1 || r.Decisions[0].AuthID != "a" {
		t.Fatalf("got %+v, want only the enabled codex credential", r.Decisions)
	}
}

func TestWritesWeightAfterWarmup(t *testing.T) {
	h := newFakeHost()
	c := ctl(h)
	run(t, c, 1)
	if len(h.saves) != 0 {
		t.Fatal("must not write during warmup")
	}
	run(t, c, 1)
	if len(h.saves) != 1 || h.saves[0].Name != "a.json" {
		t.Fatalf("got %d saves %+v", len(h.saves), h.saves)
	}
	weight, ok := authfile.Weight(h.saves[0].Raw)
	if !ok || weight <= 0 {
		t.Fatalf("saved weight %d ok=%v", weight, ok)
	}
	var got map[string]any
	if err := json.Unmarshal(h.saves[0].Raw, &got); err != nil {
		t.Fatal(err)
	}
	if got["access_token"] != "tok" || got["account_id"] != "acct-1" {
		t.Fatalf("unrelated credential keys lost: %v", got)
	}
}

func TestProbeFailureFreezesAndDoesNotWrite(t *testing.T) {
	h := newFakeHost()
	c := ctl(h)
	run(t, c, 2)
	writes := len(h.saves)
	h.httpErr = errors.New("network down")
	if _, err := c.RunCycle(now.Add(time.Hour)); err != nil {
		t.Fatalf("a failed probe must not fail the cycle: %v", err)
	}
	if len(h.saves) != writes {
		t.Fatal("a failed probe must not rewrite the credential")
	}
}

func TestNonOKProbeIsTreatedAsFailure(t *testing.T) {
	h := newFakeHost()
	h.status = 500
	c := ctl(h)
	r := run(t, c, 2)
	if len(h.saves) != 0 {
		t.Fatal("a 5xx probe must not produce a weight")
	}
	if r.Decisions[0].State != balancer.StatePending {
		t.Fatalf("got %v, want pending", r.Decisions[0].State)
	}
}

func TestLimitReachedExcludesAccount(t *testing.T) {
	h := newFakeHost()
	h.usage = `{"rate_limit":{"limit_reached":true,
	  "primary_window":{"used_percent":100,"limit_window_seconds":18000,"reset_after_seconds":600},
	  "secondary_window":{"used_percent":10,"limit_window_seconds":604800,"reset_after_seconds":400000}}}`
	r := run(t, ctl(h), 2)
	if r.Decisions[0].State != balancer.StateUnavailable || r.Decisions[0].Weight != 0 {
		t.Fatalf("got %+v, want unavailable at weight 0", r.Decisions[0])
	}
	// The key must be present: weight 0 is the host's hard exclusion, while an
	// absent weight is its default of 1 — the opposite of an exclusion.
	if w, ok := authfile.Weight(h.files["a"]); !ok || w != 0 {
		t.Fatalf("on-disk weight %d present=%v, want an explicit 0", w, ok)
	}
}

func TestMissingTokenIsAFailureNotAPanic(t *testing.T) {
	h := newFakeHost()
	h.files["a"] = []byte(`{"type":"codex"}`)
	if _, err := ctl(h).RunCycle(now); err != nil {
		t.Fatalf("cycle must survive a tokenless credential: %v", err)
	}
	if len(h.requests) != 0 {
		t.Fatal("must not probe without a token")
	}
}

func TestWriteRereadsAndPreservesConcurrentChanges(t *testing.T) {
	// host.auth.save replaces the whole document and offers no CAS, so the
	// write must re-read immediately beforehand. Here a token refresh lands
	// between the probe read and the write read; it must survive.
	h := newFakeHost()
	c := ctl(h)
	run(t, c, 1)
	// Reads for account "a": #1 was cycle 1's probe, #2 is cycle 2's probe, #3
	// is the write's re-read. Firing on #3 means an implementation that patched
	// the probe's stale bytes instead of re-reading would save the old token.
	h.onGet = func(authIndex string, call int) {
		if authIndex == "a" && call == 3 {
			h.files["a"] = []byte(`{"type":"codex","access_token":"REFRESHED","account_id":"acct-1"}`)
		}
	}
	run(t, c, 1)

	if len(h.saves) != 1 {
		t.Fatalf("got %d saves, want 1", len(h.saves))
	}
	var got map[string]any
	if err := json.Unmarshal(h.saves[0].Raw, &got); err != nil {
		t.Fatal(err)
	}
	if got["access_token"] != "REFRESHED" {
		t.Fatalf("clobbered a concurrent token refresh: %v", got["access_token"])
	}
	if _, ok := got["weight"]; !ok {
		t.Fatal("weight missing from the saved document")
	}
}

func TestSkipsSaveWhenOnDiskWeightAlreadyMatches(t *testing.T) {
	h := newFakeHost()
	c := ctl(h)
	run(t, c, 2)
	if len(h.saves) != 1 {
		t.Fatalf("got %d saves, want 1", len(h.saves))
	}
	run(t, c, 3)
	if len(h.saves) != 1 {
		t.Fatalf("got %d saves; an unchanged weight must not be rewritten", len(h.saves))
	}
}

func TestAuthGetFailureIsPerAccount(t *testing.T) {
	h := newFakeHost()
	h.entries = append(h.entries, AuthEntry{AuthIndex: "b", Name: "b.json", Type: "codex"})
	h.names["b"] = "b.json"
	h.getErr["b"] = errors.New("unreadable")
	r := run(t, ctl(h), 2)
	if len(r.Decisions) != 2 {
		t.Fatalf("got %d decisions, want both accounts reported", len(r.Decisions))
	}
	if strings.Contains(fmt.Sprint(r.Decisions), "panic") {
		t.Fatal("unreachable")
	}
}

// One capacity for the whole fleet carries no information about how accounts
// differ, so switching it on may change the unit but must not change routing.
// It does if the quantizer anchor stays in percentage units while Remaining
// moves to capacity units: every bucket shifts by the same constant, which
// rewrites the fleet for nothing and saturates it outright at a large capacity.
func TestCapacityScalingIsRoutingNeutral(t *testing.T) {
	plain := run(t, New(newFakeHost(), Options{Policy: balancer.DefaultPolicy()}), 2)
	if !plain.Decisions[0].Degraded {
		t.Fatal("bare percentages must report Degraded")
	}
	for _, capacity := range []float64{5000, 1e9} {
		scaled := run(t, New(newFakeHost(), Options{Policy: balancer.DefaultPolicy(), Capacity: capacity}), 2)
		if scaled.Decisions[0].Degraded {
			t.Fatalf("capacity %v: a scaled snapshot is absolute, not degraded", capacity)
		}
		if got, want := scaled.Decisions[0].Weight, plain.Decisions[0].Weight; got != want {
			t.Fatalf("capacity %v moved the weight from %d to %d", capacity, want, got)
		}
	}
}

func TestFailedSaveIsRetriedNextCycle(t *testing.T) {
	// The engine must not record a weight as written until the host confirms
	// the save. Committing on intent strands the credential at its old weight
	// forever, because every later cycle sees "already written" and skips it.
	h := newFakeHost()
	h.saveErr = errors.New("disk full")
	c := ctl(h)
	run(t, c, 2)
	if len(h.saves) != 0 {
		t.Fatalf("got %d saves, want none: the host rejected them", len(h.saves))
	}
	h.saveErr = nil
	r := run(t, c, 1)
	if len(h.saves) != 1 {
		t.Fatalf("got %d saves, want the failed write retried", len(h.saves))
	}
	if d := r.Decisions[0]; !d.Write {
		t.Fatalf("got %+v, want the write still pending", d)
	}
}

func TestDryRunNeverWritesButKeepsReporting(t *testing.T) {
	h := newFakeHost()
	c := New(h, Options{Policy: balancer.DefaultPolicy(), DryRun: true})
	run(t, c, 2)
	if len(h.saves) != 0 {
		t.Fatalf("dry run saved %d credentials", len(h.saves))
	}
	// A dry run must keep reporting the write as outstanding; recording it as
	// written would suppress the real save after dry_run is turned off.
	r := run(t, c, 1)
	if !r.Decisions[0].Write {
		t.Fatalf("got %+v, want the write still outstanding", r.Decisions[0])
	}
	if r.Wrote != 0 {
		t.Fatalf("Wrote=%d, want 0 in a dry run", r.Wrote)
	}
}

func TestWriteFollowsARenamedCredential(t *testing.T) {
	// host.auth.get is the authority on where a credential lives. If CPA
	// re-created the file between the probe and the write, saving under the
	// name captured earlier writes to the wrong document.
	h := newFakeHost()
	c := ctl(h)
	run(t, c, 1)
	h.onGet = func(authIndex string, call int) {
		if authIndex == "a" && call == 3 {
			h.names["a"] = "a-rotated.json"
		}
	}
	run(t, c, 1)
	if len(h.saves) != 1 {
		t.Fatalf("got %d saves", len(h.saves))
	}
	if h.saves[0].Name != "a-rotated.json" {
		t.Fatalf("saved to %q, want the name the re-read reported", h.saves[0].Name)
	}
}

func TestReportCountsConfirmedWrites(t *testing.T) {
	h := newFakeHost()
	r := run(t, ctl(h), 2)
	if r.Changed != 1 || r.Wrote != 1 {
		t.Fatalf("Changed=%d Wrote=%d, want 1 and 1", r.Changed, r.Wrote)
	}
	h2 := newFakeHost()
	h2.saveErr = errors.New("nope")
	r2 := run(t, ctl(h2), 2)
	if r2.Changed != 1 || r2.Wrote != 0 {
		t.Fatalf("Changed=%d Wrote=%d, want 1 intended and 0 confirmed", r2.Changed, r2.Wrote)
	}
}

func TestProbeFailureIsReportedNotSwallowed(t *testing.T) {
	// A silently dropped probe error is indistinguishable from warmup: both
	// show state=pending at weight 0. Without this the only way to tell an
	// unreachable endpoint from a healthy first cycle is a packet capture.
	h := newFakeHost()
	h.status = 401
	r := run(t, ctl(h), 2)
	d := r.Decisions[0]
	if d.Error == "" {
		t.Fatalf("got %+v, want the probe failure reported", d)
	}
	if !strings.Contains(d.Error, "401") {
		t.Fatalf("error %q does not say what went wrong", d.Error)
	}
	if !slices.ContainsFunc(h.logs, func(l string) bool { return strings.Contains(l, "401") }) {
		t.Fatalf("probe failure was never logged: %v", h.logs)
	}
}

func TestHealthyProbeReportsNoError(t *testing.T) {
	if d := run(t, ctl(newFakeHost()), 2).Decisions[0]; d.Error != "" {
		t.Fatalf("healthy cycle reported %q", d.Error)
	}
}

func TestRestartProposesNothingThatIsAlreadyOnDisk(t *testing.T) {
	// A restart loses the in-memory record of what was written; the credential
	// file supplies it again. Nothing is proposed and nothing is written, so
	// neither weights_pending nor accumulator_wiped reports a move that was
	// never going to happen.
	h := newFakeHost()
	run(t, ctl(h), 2)
	if len(h.saves) != 1 {
		t.Fatalf("got %d saves in the first run", len(h.saves))
	}
	saved := len(h.saves)

	fresh := ctl(h) // same host, same on-disk weights, empty engine state
	r := run(t, fresh, 2)
	if len(h.saves) != saved {
		t.Fatalf("got %d saves after restart, want none: the weight had not moved", len(h.saves)-saved)
	}
	if r.Changed != 0 || r.Wrote != 0 {
		t.Fatalf("Changed=%d Wrote=%d, want both 0: the weight was already on disk", r.Changed, r.Wrote)
	}
}

func TestExternallyErasedWeightIsRestored(t *testing.T) {
	// The credential was deleted and re-added through the management UI, so the
	// new file carries no weight and the host routes it at its default of 1.
	// The engine's own record still held the old value, which used to suppress
	// the repair for the lifetime of the process.
	h := newFakeHost()
	c := ctl(h)
	run(t, c, 2)
	if len(h.saves) != 1 {
		t.Fatalf("got %d saves in warmup, want 1", len(h.saves))
	}
	want, _ := authfile.Weight(h.saves[0].Raw)

	h.files["a"] = []byte(`{"type":"codex","access_token":"tok","account_id":"acct-1"}`)
	run(t, c, 1)

	if len(h.saves) != 2 {
		t.Fatalf("got %d saves, want the erased weight rewritten", len(h.saves))
	}
	got, ok := authfile.Weight(h.saves[1].Raw)
	if !ok || got != want {
		t.Fatalf("restored weight %d ok=%v, want %d", got, ok, want)
	}
}

func TestSkippedSaveUnderARaceIsNotCountedAsWritten(t *testing.T) {
	// The probe saw no weight, so the engine proposed one; by the time the
	// write re-read the file someone else had already put that exact value
	// there. Nothing reaches the host, so nothing may be reported as written —
	// accumulator_wiped exists to answer precisely this.
	h := newFakeHost()
	c := ctl(h)
	run(t, c, 2)
	target := append([]byte(nil), h.files["a"]...)

	h.files["a"] = []byte(`{"type":"codex","access_token":"tok","account_id":"acct-1"}`)
	// Reads for "a": #1 and #2 were the warmup probes, #3 that cycle's write
	// re-read. #4 is this cycle's probe, #5 its write re-read.
	h.onGet = func(authIndex string, call int) {
		if authIndex == "a" && call == 5 {
			h.files["a"] = target
		}
	}
	saved := len(h.saves)
	r := run(t, c, 1)

	if len(h.saves) != saved {
		t.Fatalf("got %d extra saves, want none: the value was already there", len(h.saves)-saved)
	}
	if r.Changed != 1 {
		t.Fatalf("Changed=%d, want 1: the erased weight was proposed", r.Changed)
	}
	if r.Wrote != 0 {
		t.Fatalf("Wrote=%d, want 0: nothing reached the host", r.Wrote)
	}
}

func TestSkipsEntriesWithoutAnAuthIndex(t *testing.T) {
	// At registration the host lists credentials before it has assigned their
	// runtime indices. Every probe of those fails with "auth_index is required",
	// and each observation is filed under the empty id — so the whole fleet
	// collapses onto one phantom account that no credential corresponds to.
	h := newFakeHost()
	h.entries = []AuthEntry{{Name: "a.json", Type: "codex"}}
	r := run(t, ctl(h), 1)

	if len(h.gets) != 0 || len(h.requests) != 0 {
		t.Fatalf("got %d credential reads and %d probes, want none", len(h.gets), len(h.requests))
	}
	if len(r.Decisions) != 0 {
		t.Fatalf("got %+v, want no decisions", r.Decisions)
	}
}
