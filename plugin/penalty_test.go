package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/duanzhenyu/cpa-codex-weight-balancer/internal/balancer"
	"github.com/duanzhenyu/cpa-codex-weight-balancer/internal/controller"
)

func pendingOutcomes(t *testing.T) map[string]balancer.Outcomes {
	t.Helper()
	mu.Lock()
	defer mu.Unlock()
	if current == nil {
		return nil
	}
	return current.ctl.Outcomes()
}

func failedRecord(status int, body string) pluginapi.UsageRecord {
	r := codexRecord()
	r.Failed = true
	r.Failure = pluginapi.UsageFailure{StatusCode: status, Body: body}
	return r
}

// mixedTraffic is one of each kind of record the hook sorts.
func mixedTraffic() []pluginapi.UsageRecord {
	noQuota := codexRecord()
	noQuota.ResponseHeaders = nil
	return []pluginapi.UsageRecord{
		codexRecord(),
		noQuota, // served, even though it carried no quota reading
		failedRecord(http.StatusInternalServerError, "upstream exploded"),
		failedRecord(0, "connection reset by peer"),
		failedRecord(499, "context canceled"),
		failedRecord(http.StatusBadRequest, `{"error":{"code":"context_length_exceeded"}}`),
		rateLimited(),
	}
}

func TestHookTalliesOutcomesWhenThePenaltyIsOn(t *testing.T) {
	configure(t, "interval: 15m\nerror_penalty: true")
	for _, r := range mixedTraffic() {
		if env := call(t, pluginabi.MethodUsageHandle, r); !env.OK {
			t.Fatalf("usage.handle rejected: %+v", env.Error)
		}
	}
	// A client that hung up or sent a bad request says nothing about the
	// account, and a 429 belongs to the recheck path.
	if got := pendingOutcomes(t)["a"]; got != (balancer.Outcomes{Requests: 4, Failures: 2}) {
		t.Fatalf("tally %+v, want 4 requests with 2 failures", got)
	}
	if n := pendingRejections(t); n != 1 {
		t.Fatalf("rejections = %d: the 429 must still queue its probe", n)
	}

	// An image-generation tool call arrives as two served records of one attempt.
	tool := codexRecord()
	tool.AuthIndex, tool.RequestedAt = "b", time.Now().UTC().Add(-time.Minute)
	call(t, pluginabi.MethodUsageHandle, tool)
	call(t, pluginabi.MethodUsageHandle, tool)
	if got := pendingOutcomes(t)["b"]; got.Requests != 1 {
		t.Fatalf("tally %+v, want one request", got)
	}
}

func TestHookTalliesNothingWithThePenaltyOff(t *testing.T) {
	configure(t, "interval: 15m")
	for _, r := range mixedTraffic() {
		call(t, pluginabi.MethodUsageHandle, r)
	}
	if got := pendingOutcomes(t); len(got) != 0 {
		t.Fatalf("tallies %+v, want none with the switch off", got)
	}
}

func TestErrorPenaltyIsAdvertisedAndReported(t *testing.T) {
	env := call(t, pluginabi.MethodPluginRegister, lifecycleRequest{ConfigYAML: []byte("interval: 15m")})
	t.Cleanup(stopRuntime)
	var reg registration
	if err := json.Unmarshal(env.Result, &reg); err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(reg.Metadata.ConfigFields, func(f pluginapi.ConfigField) bool {
		return f.Name == "error_penalty" && f.Type == pluginapi.ConfigFieldTypeBoolean
	}) {
		t.Fatal("error_penalty is not advertised")
	}
	if !slices.ContainsFunc(reg.Metadata.ConfigFields, func(f pluginapi.ConfigField) bool {
		return f.Name == "error_window" && f.Type == pluginapi.ConfigFieldTypeString
	}) {
		t.Fatal("error_window is not advertised")
	}

	for yaml, on := range map[string]bool{"interval: 15m\nerror_penalty: true": true, "interval: 15m": false} {
		cfg := decode(t, yaml)
		w := &worker{cfg: cfg, ctl: controller.New(hostBridge{}, cfg.Options)}
		w.lastRep = balancer.Report{Decisions: []balancer.Decision{{AuthID: "a", State: balancer.StateActive, Weight: 900}}}
		w.ctl.Engine().Restore(map[string]balancer.Account{"a": {State: balancer.StateActive, Weight: 900, ErrorRate: 0.25}}, nil)

		out := w.status()
		if out["error_penalty"] != on {
			t.Fatalf("%q: error_penalty = %v", yaml, out["error_penalty"])
		}
		if out["error_window"] != "12h0m0s" {
			t.Fatalf("%q: error_window = %v", yaml, out["error_window"])
		}
		rate, shown := out["credentials"].([]map[string]any)[0]["error_rate"]
		if shown != on || (on && rate != 0.25) {
			t.Fatalf("%q: error_rate = %v (shown=%v)", yaml, rate, shown)
		}
	}
}

// faultyHost lists one Codex credential whose file cannot be read, which is
// enough for a cycle to fold that credential's outcomes.
type faultyHost struct{ lists chan struct{} }

func (h faultyHost) AuthList() ([]controller.AuthEntry, error) {
	select {
	case h.lists <- struct{}{}:
	default:
	}
	return []controller.AuthEntry{{AuthIndex: "a", Type: "codex"}}, nil
}
func (faultyHost) AuthGet(string) (string, []byte, error) { return "", nil, errors.New("unreadable") }
func (faultyHost) AuthSave(string, []byte) error          { return errors.New("unused") }
func (faultyHost) HTTPDo(controller.HTTPRequest) (controller.HTTPResponse, error) {
	return controller.HTTPResponse{}, errors.New("unused")
}
func (faultyHost) Log(string, string) {}

func faultyWorker(t *testing.T, interval time.Duration) (*worker, faultyHost) {
	t.Helper()
	cfg := decode(t, "interval: 15m\nerror_penalty: true")
	cfg.Interval = interval
	h := faultyHost{lists: make(chan struct{}, 1)}
	w := &worker{
		cfg:  cfg,
		ctl:  controller.New(h, cfg.Options),
		stop: make(chan struct{}),
		done: make(chan struct{}),
		wake: make(chan struct{}, 1),
	}
	for i := range 10 {
		w.ctl.NoteOutcome("a", true, time.Now().Add(time.Duration(i)))
	}
	return w, h
}

func listed(t *testing.T, h faultyHost) {
	t.Helper()
	select {
	case <-h.lists:
	case <-time.After(5 * time.Second):
		t.Fatal("no cycle listed credentials")
	}
}

func TestOnlyTheTickerFoldsOutcomes(t *testing.T) {
	// The cycle a reload starts with and an operator's refresh price with the
	// rate as it stands rather than consume the tally.
	w, h := faultyWorker(t, 15*time.Minute)
	mu.Lock()
	current = w
	mu.Unlock()
	go w.loop()
	t.Cleanup(stopRuntime)
	listed(t, h)
	managementCall(t, http.MethodPost, pluginName+refreshPath)
	if rate := w.ctl.Engine().Accounts()["a"].ErrorRate; rate != 0 {
		t.Fatalf("error rate %v after the first cycle and a refresh, want nothing folded", rate)
	}
	if got := w.ctl.Outcomes()["a"]; got.Requests != 10 {
		t.Fatalf("tally %+v, want it left for the ticker", got)
	}

	w, h = faultyWorker(t, 10*time.Millisecond)
	go w.loop()
	listed(t, h)
	listed(t, h)
	w.halt()
	if rate := w.ctl.Engine().Accounts()["a"].ErrorRate; rate <= 0 {
		t.Fatal("the ticker never folded the tally")
	}
}
