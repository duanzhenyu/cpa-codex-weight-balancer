package main

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/duanzhenyu/cpa-codex-weight-balancer/internal/balancer"
	"github.com/duanzhenyu/cpa-codex-weight-balancer/internal/controller"
)

func pendingRejections(t *testing.T) int {
	t.Helper()
	mu.Lock()
	defer mu.Unlock()
	if current == nil {
		return 0
	}
	return current.ctl.Rejections()
}

func rateLimited() pluginapi.UsageRecord {
	record := codexRecord()
	record.Failed = true
	record.Failure.StatusCode = http.StatusTooManyRequests
	record.ResponseHeaders.Set("X-Codex-Primary-Used-Percent", "100")
	return record
}

func TestRateLimitedRequestIsNotedForARecheck(t *testing.T) {
	// The reading a served request left behind can predate the budget running
	// out, which is how a spent account kept its full weight for a whole cycle.
	configure(t, "interval: 15m")
	if env := call(t, pluginabi.MethodUsageHandle, rateLimited()); !env.OK {
		t.Fatalf("usage.handle rejected: %+v", env.Error)
	}
	if n := pendingRejections(t); n != 1 {
		t.Fatalf("rejections pending = %d, want the 429 noted", n)
	}
	if n := pendingObservations(t); n != 0 {
		t.Fatalf("observations pending = %d: a 429 must never be priced", n)
	}
}

func TestOnlyRateLimitsAreNoted(t *testing.T) {
	configure(t, "interval: 15m")
	for name, mark := range map[string]func(*pluginapi.UsageRecord){
		"served":         func(*pluginapi.UsageRecord) {},
		"failed flag":    func(r *pluginapi.UsageRecord) { r.Failed = true },
		"401":            func(r *pluginapi.UsageRecord) { r.Failed, r.Failure.StatusCode = true, http.StatusUnauthorized },
		"500":            func(r *pluginapi.UsageRecord) { r.Failed, r.Failure.StatusCode = true, http.StatusInternalServerError },
		"502 overloaded": func(r *pluginapi.UsageRecord) { r.Failed, r.Failure.StatusCode = true, http.StatusBadGateway },
		"429 elsewhere": func(r *pluginapi.UsageRecord) {
			r.Failed, r.Failure.StatusCode, r.Provider = true, http.StatusTooManyRequests, "gemini"
		},
	} {
		record := codexRecord()
		mark(&record)
		if env := call(t, pluginabi.MethodUsageHandle, record); !env.OK {
			t.Fatalf("%s: %+v", name, env.Error)
		}
		if n := pendingRejections(t); n != 0 {
			t.Fatalf("%s: rejections pending = %d, want none", name, n)
		}
	}
}

func TestReconfigureCarriesPendingRejectionsAcross(t *testing.T) {
	configure(t, "interval: 15m")
	call(t, pluginabi.MethodUsageHandle, rateLimited())
	if env := call(t, pluginabi.MethodPluginReconfigure, lifecycleRequest{ConfigYAML: []byte("interval: 20m")}); !env.OK {
		t.Fatalf("reconfigure rejected: %+v", env.Error)
	}
	if n := pendingRejections(t); n != 1 {
		t.Fatalf("rejections pending = %d: a 429 queued on the replaced worker was dropped", n)
	}
}

func TestRejectNeverBlocksTheHostQueue(t *testing.T) {
	cfg := decode(t, "interval: 15m")
	w := &worker{cfg: cfg, ctl: controller.New(hostBridge{}, cfg.Options), wake: make(chan struct{}, 1)}
	for range 100 {
		w.reject("a") // a blocking send would hang the host's usage queue here
	}
	if len(w.wake) != 1 {
		t.Fatalf("wake holds %d, want one coalesced signal", len(w.wake))
	}
	<-w.wake
	w.reject("  ")
	if len(w.wake) != 0 {
		t.Fatal("a rejection with no auth index must not wake the worker")
	}
}

// listingHost reports each credential listing, which both a cycle and a recheck
// start with.
type listingHost struct{ lists chan struct{} }

func (h listingHost) AuthList() ([]controller.AuthEntry, error) {
	h.lists <- struct{}{}
	return nil, nil
}
func (listingHost) AuthGet(string) (string, []byte, error) { return "", nil, errors.New("unused") }
func (listingHost) AuthSave(string, []byte) error          { return errors.New("unused") }
func (listingHost) HTTPDo(controller.HTTPRequest) (controller.HTTPResponse, error) {
	return controller.HTTPResponse{}, errors.New("unused")
}
func (listingHost) Log(string, string) {}

func TestWorkerRechecksWhenARejectionWakesIt(t *testing.T) {
	cfg := decode(t, "interval: 15m")
	h := listingHost{lists: make(chan struct{}, 8)}
	w := &worker{
		cfg:  cfg,
		ctl:  controller.New(h, cfg.Options),
		stop: make(chan struct{}),
		done: make(chan struct{}),
		wake: make(chan struct{}, 1),
	}
	go w.loop()
	defer w.halt()

	listed := func(what string) {
		t.Helper()
		select {
		case <-h.lists:
		case <-time.After(5 * time.Second):
			t.Fatalf("%s never listed credentials", what)
		}
	}
	listed("the first cycle")
	w.reject("a")
	listed("the woken recheck")
}

func TestRecheckDelay(t *testing.T) {
	now := time.Date(2026, 9, 14, 9, 0, 0, 0, time.UTC)
	if d := recheckDelay(now, now.Add(-time.Minute), false); d != 0 {
		t.Fatalf("overdue recheck waits %s, want none", d)
	}
	if d := recheckDelay(now, now.Add(90*time.Second), false); d != 90*time.Second {
		t.Fatalf("got %s, want 90s", d)
	}
	// A recheck that could not even list credentials learned nothing. Firing an
	// overdue timer straight back at it spins on the host.
	if d := recheckDelay(now, now.Add(-time.Minute), true); d != controller.RecheckGap {
		t.Fatalf("after a failure got %s, want the gap", d)
	}
	if d := recheckDelay(now, now.Add(2*time.Minute), true); d != 2*time.Minute {
		t.Fatalf("after a failure got %s, want the later due time", d)
	}
}

func TestStatusShowsARecheckBeforeTheNextCycle(t *testing.T) {
	// A recheck moves an account between cycles. Rendering only the last cycle's
	// verdict shows a spent account at full weight until the ticker comes round.
	cfg := decode(t, "interval: 15m")
	w := &worker{cfg: cfg, ctl: controller.New(hostBridge{}, cfg.Options)}
	w.lastRep = balancer.Report{Decisions: []balancer.Decision{{AuthID: "a", State: balancer.StateActive, Weight: 900}}}
	w.ctl.Engine().Restore(
		map[string]balancer.Account{"a": {State: balancer.StateUnavailable, RecoverAt: time.Now().Add(time.Hour)}},
		map[string]int64{"a": 0},
	)

	out := w.status()
	rows := out["credentials"].([]map[string]any)
	if rows[0]["state"] != "unavailable" || rows[0]["weight"] != int64(0) {
		t.Fatalf("row %v, want the excluded account as it stands now", rows[0])
	}
	if _, ok := out["rejections_pending"]; !ok {
		t.Fatal("status must report whether rejections are arriving")
	}
}
