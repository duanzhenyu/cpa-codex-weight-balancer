package main

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// pendingObservations reads what the live worker has collected but not yet
// priced. It is the only externally visible effect the hook is allowed to have.
func pendingObservations(t *testing.T) int {
	t.Helper()
	mu.Lock()
	defer mu.Unlock()
	if current == nil {
		return 0
	}
	return current.ctl.Pending()
}

func codexRecord() pluginapi.UsageRecord {
	return pluginapi.UsageRecord{
		Provider:  "codex",
		AuthIndex: "a",
		ResponseHeaders: http.Header{
			"X-Codex-Primary-Used-Percent":          {"20"},
			"X-Codex-Primary-Window-Minutes":        {"300"},
			"X-Codex-Primary-Reset-After-Seconds":   {"7200"},
			"X-Codex-Secondary-Used-Percent":        {"45"},
			"X-Codex-Secondary-Window-Minutes":      {"10080"},
			"X-Codex-Secondary-Reset-After-Seconds": {"400000"},
		},
	}
}

func TestRegisterAdvertisesTheUsageHook(t *testing.T) {
	configure(t, "interval: 15m")
	env := call(t, pluginabi.MethodPluginRegister, lifecycleRequest{ConfigYAML: []byte("interval: 15m")})
	var reg registration
	if err := json.Unmarshal(env.Result, &reg); err != nil {
		t.Fatal(err)
	}
	if !reg.Capabilities.UsagePlugin {
		t.Fatal("the host only delivers usage records to a plugin that declares usage_plugin")
	}
}

func TestUsageRecordIsAccepted(t *testing.T) {
	configure(t, "interval: 15m")
	if env := call(t, pluginabi.MethodUsageHandle, codexRecord()); !env.OK {
		t.Fatalf("usage.handle rejected: %+v", env.Error)
	}
	if n := pendingObservations(t); n != 1 {
		t.Fatalf("pending = %d, want the record recorded", n)
	}
}

func TestUsageRecordsFromOtherProvidersAreIgnored(t *testing.T) {
	// Every provider's records arrive on the same hook. A Gemini response
	// carries no Codex windows, but filing it under a codex auth index would
	// still cost that credential its probe for a cycle.
	configure(t, "interval: 15m")
	for _, provider := range []string{"gemini", "claude", "", "CODEX-LIKE"} {
		record := codexRecord()
		record.Provider = provider
		call(t, pluginabi.MethodUsageHandle, record)
	}
	if n := pendingObservations(t); n != 0 {
		t.Fatalf("pending = %d, want none", n)
	}
}

func TestUsageProviderMatchIsCaseInsensitive(t *testing.T) {
	configure(t, "interval: 15m")
	record := codexRecord()
	record.Provider = " Codex "
	call(t, pluginabi.MethodUsageHandle, record)
	if n := pendingObservations(t); n != 1 {
		t.Fatalf("pending = %d, want the record recorded", n)
	}
}

func TestUsageRecordWithoutQuotaIsIgnored(t *testing.T) {
	configure(t, "interval: 15m")
	for name, headers := range map[string]http.Header{
		"none":      nil,
		"unrelated": {"Content-Type": {"application/json"}},
	} {
		record := codexRecord()
		record.ResponseHeaders = headers
		if env := call(t, pluginabi.MethodUsageHandle, record); !env.OK {
			t.Fatalf("%s: the hook must answer ok even when it records nothing: %+v", name, env.Error)
		}
	}
	if n := pendingObservations(t); n != 0 {
		t.Fatalf("pending = %d, want none", n)
	}
}

func TestUsageRecordBeforeConfigureIsNotAnError(t *testing.T) {
	// The host may deliver a record between shutdown and the next register.
	// Failing here would put the plugin on the host's fuse path for something
	// it can simply drop.
	stopRuntime()
	if env := call(t, pluginabi.MethodUsageHandle, codexRecord()); !env.OK {
		t.Fatalf("usage.handle before configure must not fail: %+v", env.Error)
	}
}

func TestUsageHookNeverWritesAWeight(t *testing.T) {
	// No host API is installed in tests, so any host call fails closed and a
	// write attempt would surface as an error envelope. The stronger guarantee
	// is structural: the hook only ever reaches Controller.Observe and
	// NoteRejected, neither of which calls the host.
	configure(t, "interval: 15m")
	for range 100 {
		if env := call(t, pluginabi.MethodUsageHandle, codexRecord()); !env.OK {
			t.Fatalf("usage.handle must stay a pure observer: %+v", env.Error)
		}
	}
	if n := pendingObservations(t); n != 1 {
		t.Fatalf("pending = %d: repeated records for one credential must collapse", n)
	}
}

func TestStatusReportsWhetherUsageRecordsAreArriving(t *testing.T) {
	// Without this the only way to tell a working hook from a silent one is to
	// turn on host debug logging, which is exactly what the status route exists
	// to avoid.
	configure(t, "interval: 15m")
	before := managementCall(t, http.MethodGet, pluginName+statusPath)
	var out map[string]any
	if err := json.Unmarshal(before.Body, &out); err != nil {
		t.Fatal(err)
	}
	if got, ok := out["observations_pending"]; !ok || got.(float64) != 0 {
		t.Fatalf("observations_pending = %v, ok=%v", got, ok)
	}

	call(t, pluginabi.MethodUsageHandle, codexRecord())
	after := managementCall(t, http.MethodGet, pluginName+statusPath)
	if err := json.Unmarshal(after.Body, &out); err != nil {
		t.Fatal(err)
	}
	if out["observations_pending"].(float64) != 1 {
		t.Fatalf("observations_pending = %v, want 1", out["observations_pending"])
	}
}

func TestFailedRequestsAreNotUsedAsQuotaReadings(t *testing.T) {
	// A 429 answers with perfectly well-formed headers saying the account is
	// spent. Ingesting them would run the credential through rate.Blocked to
	// weight 0 — a hard exclusion that also evicts its bound sticky sessions —
	// at request rate rather than on the timer. A 429 only queues a probe; this
	// plugin prices from responses the host actually served.
	configure(t, "interval: 15m")
	for name, mark := range map[string]func(*pluginapi.UsageRecord){
		"failed flag": func(r *pluginapi.UsageRecord) { r.Failed = true },
		"429":         func(r *pluginapi.UsageRecord) { r.Failure.StatusCode = http.StatusTooManyRequests },
		"500":         func(r *pluginapi.UsageRecord) { r.Failure.StatusCode = http.StatusInternalServerError },
		"spent window": func(r *pluginapi.UsageRecord) {
			r.Failed = true
			r.ResponseHeaders.Set("X-Codex-Primary-Used-Percent", "100")
		},
	} {
		record := codexRecord()
		mark(&record)
		if env := call(t, pluginabi.MethodUsageHandle, record); !env.OK {
			t.Fatalf("%s: a dropped record must still be acknowledged: %+v", name, env.Error)
		}
		if n := pendingObservations(t); n != 0 {
			t.Fatalf("%s: pending = %d, want the credential left to its probe", name, n)
		}
	}
}

func TestObservedAtIsWhenTheRequestCompleted(t *testing.T) {
	// ObservedAt is the denominator of the engine's observed burn rate, and the
	// host dispatches usage records off a queue, so reading the clock in this
	// hook stretches the interval by however long that queue was backed up.
	requested := time.Now().UTC().Add(-time.Minute)
	record := codexRecord()
	record.RequestedAt, record.Latency = requested, 10*time.Second
	if got, want := usageObservedAt(record), requested.Add(10*time.Second); !got.Equal(want) {
		t.Fatalf("observed at %v, want %v", got, want)
	}

	// Without both halves there is nothing better than the clock.
	for name, record := range map[string]pluginapi.UsageRecord{
		"no start":    {Latency: time.Second},
		"no latency":  {RequestedAt: requested},
		"bad latency": {RequestedAt: requested, Latency: -time.Second},
		"zero record": {},
	} {
		if got := usageObservedAt(record); got.IsZero() {
			t.Fatalf("%s: must fall back to the clock, got zero", name)
		}
	}

	// A record whose arithmetic lands ahead of the clock is not a licence to
	// stamp the future: Compute rejects such a snapshot outright.
	ahead := codexRecord()
	ahead.RequestedAt, ahead.Latency = time.Now().UTC(), time.Hour
	if usageObservedAt(ahead).After(time.Now().UTC().Add(time.Second)) {
		t.Fatal("observed time must never run ahead of the clock")
	}
}
