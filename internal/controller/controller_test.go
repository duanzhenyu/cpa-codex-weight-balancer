package controller

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"gitlab.duanzhenyu.top/dzy/cpa-codex-weight-balancer/internal/authfile"
	"gitlab.duanzhenyu.top/dzy/cpa-codex-weight-balancer/internal/balancer"
)

var now = time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)

const usageBody = `{"rate_limits":{
  "primary":{"used_percent":20,"window_minutes":300,"reset_after_seconds":7200},
  "secondary":{"used_percent":45,"window_minutes":10080,"reset_after_seconds":400000}}}`

type save struct {
	Name string
	Raw  []byte
}

type fakeHost struct {
	entries  []AuthEntry
	files    map[string][]byte // authIndex -> credential JSON
	names    map[string]string // authIndex -> file name
	usage    string
	status   int
	httpErr  error
	getErr   map[string]error
	saves    []save
	saveErr  error
	requests []HTTPRequest
	gets     []string
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

func (h *fakeHost) AuthList() ([]AuthEntry, error) { return h.entries, nil }

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
	if h.httpErr != nil {
		return HTTPResponse{}, h.httpErr
	}
	return HTTPResponse{StatusCode: h.status, Body: []byte(h.usage)}, nil
}

func (h *fakeHost) Log(string, string) {}

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
	h.usage = `{"rate_limits":{"limit_reached":true,
	  "primary":{"used_percent":100,"window_minutes":300,"reset_after_seconds":600},
	  "secondary":{"used_percent":10,"window_minutes":10080,"reset_after_seconds":400000}}}`
	r := run(t, ctl(h), 2)
	if r.Decisions[0].State != balancer.StateUnavailable || r.Decisions[0].Weight != 0 {
		t.Fatalf("got %+v, want unavailable at weight 0", r.Decisions[0])
	}
	if w, _ := authfile.Weight(h.files["a"]); w != 0 {
		t.Fatalf("on-disk weight %d, want 0", w)
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

func TestCapacityScalingMarksComparable(t *testing.T) {
	h := newFakeHost()
	plain := New(h, Options{Policy: balancer.DefaultPolicy()})
	if r := run(t, plain, 2); !r.Decisions[0].Degraded {
		t.Fatal("bare percentages must report Degraded")
	}
	h2 := newFakeHost()
	scaled := New(h2, Options{Policy: balancer.DefaultPolicy(), Capacity: 5000})
	if r := run(t, scaled, 2); r.Decisions[0].Degraded {
		t.Fatal("a capacity-scaled snapshot is comparable")
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
