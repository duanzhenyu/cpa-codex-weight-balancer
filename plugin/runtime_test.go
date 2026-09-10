package main

import (
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"

	"github.com/duanzhenyu/cpa-codex-weight-balancer/internal/balancer"
	"github.com/duanzhenyu/cpa-codex-weight-balancer/internal/controller"
	"github.com/duanzhenyu/cpa-codex-weight-balancer/internal/pluginconf"
)

// call drives the plugin the way the host does, through the method dispatcher,
// and unwraps the envelope. No host API is stored, so every host callback fails
// closed — which is itself a path worth covering.
func call(t *testing.T, method string, payload any) envelope {
	t.Helper()
	var raw []byte
	if payload != nil {
		var err error
		if raw, err = json.Marshal(payload); err != nil {
			t.Fatal(err)
		}
	}
	out, err := handleMethod(method, raw)
	if err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	var env envelope
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	return env
}

func configure(t *testing.T, yaml string) {
	t.Helper()
	if env := call(t, pluginabi.MethodPluginRegister, lifecycleRequest{ConfigYAML: []byte(yaml)}); !env.OK {
		t.Fatalf("register rejected: %+v", env.Error)
	}
	t.Cleanup(stopRuntime)
}

func TestRegisterAdvertisesTheHostContract(t *testing.T) {
	configure(t, "interval: 15m")
	env := call(t, pluginabi.MethodPluginRegister, lifecycleRequest{ConfigYAML: []byte("interval: 15m")})
	var reg registration
	if err := json.Unmarshal(env.Result, &reg); err != nil {
		t.Fatal(err)
	}
	if reg.SchemaVersion != pluginabi.SchemaVersion {
		t.Fatalf("schema %d, want %d", reg.SchemaVersion, pluginabi.SchemaVersion)
	}
	if !reg.Capabilities.ManagementAPI || reg.Metadata.Name != pluginName {
		t.Fatalf("got %+v", reg)
	}
	if len(reg.Metadata.ConfigFields) == 0 {
		t.Fatal("no config fields advertised")
	}
	// The host's validPlugin rejects the registration outright when any of
	// these is blank, whatever capabilities it declares — the plugin then loads
	// and is immediately discarded as "invalid metadata or no capabilities".
	for field, value := range map[string]string{
		"Name":             reg.Metadata.Name,
		"Version":          reg.Metadata.Version,
		"Author":           reg.Metadata.Author,
		"GitHubRepository": reg.Metadata.GitHubRepository,
	} {
		if strings.TrimSpace(value) == "" {
			t.Fatalf("metadata.%s is blank; the host rejects the registration", field)
		}
	}
}

func TestBadConfigIsRejectedNotIgnored(t *testing.T) {
	// A rejected config must leave the previous worker running rather than
	// silently falling back to defaults the operator did not ask for.
	for _, yaml := range []string{
		"interval: 1s",
		"endpoint: http://chatgpt.com/usage",
		"alpha: .nan",
		"interval: [not, a, duration]",
	} {
		if _, err := handleMethod(pluginabi.MethodPluginReconfigure, mustJSON(t, lifecycleRequest{ConfigYAML: []byte(yaml)})); err == nil {
			t.Fatalf("%q was accepted", yaml)
		}
	}
}

func TestManagementRegisterNamespacesItsRoutes(t *testing.T) {
	env := call(t, pluginabi.MethodManagementRegister, nil)
	var reg managementRegistration
	if err := json.Unmarshal(env.Result, &reg); err != nil {
		t.Fatal(err)
	}
	if len(reg.Routes) != 2 {
		t.Fatalf("got %d routes", len(reg.Routes))
	}
	for _, route := range reg.Routes {
		if !strings.HasPrefix(route.Path, pluginName+"/") {
			t.Fatalf("route %q escapes the plugin namespace", route.Path)
		}
	}
}

func TestManagementBeforeConfigureIsUnavailable(t *testing.T) {
	stopRuntime()
	if code := managementCall(t, "GET", pluginName+statusPath).StatusCode; code != 503 {
		t.Fatalf("got %d, want 503", code)
	}
}

func TestUnknownRouteIsNotFound(t *testing.T) {
	configure(t, "interval: 15m")
	if code := managementCall(t, "GET", pluginName+"/secrets").StatusCode; code != 404 {
		t.Fatalf("got %d, want 404", code)
	}
}

func TestUnknownMethodAnswersWithAnEnvelope(t *testing.T) {
	// An unknown method is the host's problem, not a reason to fail the call.
	out, err := handleMethod("plugin.doesNotExist", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var env envelope
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatal(err)
	}
	if env.OK || env.Error == nil || env.Error.Code != "unknown_method" {
		t.Fatalf("got %+v", env)
	}
}

// The plugin cannot observe whether the host actually reset its smooth-WRR
// credits. The host only resets them when a credential present in both the old
// and new weight vectors changes value, and its vector holds positive weights
// only — so a save that drops a credential to 0 removes it from the candidate
// set and resets nothing. Reporting every confirmed save as a wipe was a claim
// the plugin had no way to back, so the field is gone; weights_written is the
// honest number.
func TestStatusDoesNotClaimToKnowTheHostAccumulatorWasReset(t *testing.T) {
	configure(t, "interval: 15m\ndry_run: true")
	body := string(managementCall(t, "GET", pluginName+statusPath).Body)
	if strings.Contains(body, "accumulator_wiped") {
		t.Fatalf("status asserts an unobservable host state:\n%s", body)
	}
	if !strings.Contains(body, `"weights_written"`) {
		t.Fatalf("status must still report what it did write:\n%s", body)
	}
}

// One capacity for the whole fleet cannot establish that two credentials are on
// the same plan, and the plugin never reads a plan tier. Reporting the setting
// as a comparability verdict contradicts the documented limit.
func TestStatusReportsTheCapacitySettingNotATierVerdict(t *testing.T) {
	configure(t, "interval: 15m\nquota_capacity: 5000\ndry_run: true")
	body := string(managementCall(t, "GET", pluginName+statusPath).Body)
	if strings.Contains(body, "tier_comparable") {
		t.Fatalf("status claims to have verified account tiers:\n%s", body)
	}
	if !strings.Contains(body, `"capacity_scaled": true`) {
		t.Fatalf("status must report the observable capacity setting:\n%s", body)
	}
}

func TestStatusCarriesNoCredentialMaterial(t *testing.T) {
	configure(t, "interval: 15m\ndry_run: true")
	body := string(managementCall(t, "GET", pluginName+statusPath).Body)
	for _, forbidden := range []string{"access_token", "Bearer", "token", ".json", "account_id"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("status leaked %q:\n%s", forbidden, body)
		}
	}
	if !strings.Contains(body, `"dry_run": true`) {
		t.Fatalf("status must surface dry_run:\n%s", body)
	}
}

func TestReconfigureCarriesTheCommittedWeightsAcross(t *testing.T) {
	// A config reload that drops either half re-prices every credential on the
	// next cycle, and a weight change can reset the host's routing credits.
	// Interval is not a pricing input, so the committed state still applies.
	configure(t, "interval: 15m")
	engine := current.ctl.Engine()
	engine.Restore(
		map[string]balancer.Account{"a": {State: balancer.StateActive, Weight: 1200, Bucket: balancer.Bucket{Index: 3, Set: true}}},
		map[string]int64{"a": 1200},
	)

	if env := call(t, pluginabi.MethodPluginReconfigure, lifecycleRequest{ConfigYAML: []byte("interval: 20m")}); !env.OK {
		t.Fatalf("reconfigure rejected: %+v", env.Error)
	}
	next := current.ctl.Engine()
	if got := next.Accounts()["a"]; got.Weight != 1200 || got.Bucket.Index != 3 {
		t.Fatalf("account state lost across reload: %+v", got)
	}
	if got := next.Written()["a"]; got != 1200 {
		t.Fatalf("written weight lost across reload: %d", got)
	}
}

// Changing a pricing input invalidates the committed bucket — it is a
// coordinate in the old scoring function — but nothing else. Dropping the whole
// account drops the exclusion's recovery deadline with it, and freeze() needs
// that deadline to re-admit an excluded credential while the probe is down.
// Without it a config edit during an outage strands the account at weight 0,
// which the host reads as a hard exclusion.
func TestReconfigureInvalidatesBucketButKeepsRecoveryState(t *testing.T) {
	recoverAt := time.Date(2026, 9, 12, 4, 0, 0, 0, time.UTC)
	for _, reload := range []string{"interval: 15m\nalpha: 2", "interval: 15m\nbucket_q: 0.2", "interval: 15m\nt_floor: 45m"} {
		func() {
			configure(t, "interval: 15m\nalpha: 1")
			current.ctl.Engine().Restore(
				map[string]balancer.Account{"a": {
					State:     balancer.StateUnavailable,
					Weight:    0,
					Bucket:    balancer.Bucket{Index: 3, Set: true},
					LastGood:  recoverAt.Add(-40 * time.Hour),
					RecoverAt: recoverAt,
				}},
				map[string]int64{"a": 0},
			)
			if env := call(t, pluginabi.MethodPluginReconfigure, lifecycleRequest{ConfigYAML: []byte(reload)}); !env.OK {
				t.Fatalf("reconfigure rejected: %+v", env.Error)
			}
			got, ok := current.ctl.Engine().Accounts()["a"]
			if !ok {
				t.Fatalf("%q discarded the account's recovery state", reload)
			}
			if got.Bucket.Set {
				t.Fatalf("%q reused a bucket from the old pricing: %+v", reload, got)
			}
			if got.State != balancer.StateUnavailable || got.Weight != 0 || !got.RecoverAt.Equal(recoverAt) {
				t.Fatalf("%q lost the exclusion's recovery deadline: %+v", reload, got)
			}
			if written, okWritten := current.ctl.Engine().Written()["a"]; !okWritten || written != 0 {
				t.Fatalf("%q lost the confirmed disk weight: %d present=%v", reload, written, okWritten)
			}
			stopRuntime()
		}()
	}
}

// A bucket index is a coordinate in the scoring function, so only the inputs
// that move that coordinate may invalidate it. Treating an operational knob as
// a pricing change re-commits every account on reload and rewrites the fleet;
// hysteresis is the sharpest case, where clearing the bucket is precisely the
// thing hysteresis exists to prevent.
func TestSamePricingSeparatesScoringInputsFromOperationalKnobs(t *testing.T) {
	base := decode(t, "interval: 15m")
	for _, raw := range []string{
		"interval: 20m",
		"interval: 15m\ndry_run: true",
		"interval: 15m\nwarmup: 5",
		"interval: 15m\nfreshness: 2h",
		"interval: 15m\nhysteresis: 0.8",
		"interval: 15m\nendpoint: https://example.invalid/usage",
		"interval: 15m\nquota_capacity: 5000",
	} {
		if !samePricing(base, decode(t, raw)) {
			t.Fatalf("%q invalidated a still-valid bucket", raw)
		}
	}
	for _, raw := range []string{
		"interval: 15m\nalpha: 2",
		"interval: 15m\nt_floor: 45m",
		"interval: 15m\nbucket_q: 0.2",
	} {
		if samePricing(base, decode(t, raw)) {
			t.Fatalf("%q kept a bucket from the old scoring function", raw)
		}
	}
}

func decode(t *testing.T, yaml string) pluginconf.Resolved {
	t.Helper()
	cfg, err := pluginconf.Decode([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// halt() blocks until the loop returns, so a cycle the loop should never have
// started holds the reconfigure or shutdown that halted it — and a probe has no
// overall deadline.
func TestHaltedWorkerNeverStartsACycle(t *testing.T) {
	cfg := decode(t, "interval: 15m")
	w := &worker{
		cfg:  cfg,
		ctl:  controller.New(hostBridge{}, cfg.Options),
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
	close(w.stop)
	w.loop()
	if !w.lastRun.IsZero() {
		t.Fatalf("a halted worker probed anyway at %s", w.lastRun)
	}
}

func TestLifecycleIsSafeUnderConcurrentManagementCalls(t *testing.T) {
	// The host may call these concurrently. Run with -race: a status render
	// overlapping a refresh cycle used to touch the engine's maps unguarded.
	configure(t, "interval: 15m")
	var wg sync.WaitGroup
	var reloads atomic.Uint64
	start := make(chan struct{})
	for _, work := range []func(){
		func() { managementCall(t, "GET", pluginName+statusPath) },
		func() { managementCall(t, "POST", pluginName+refreshPath) },
		func() {
			// Alternating intervals so each call actually replaces the worker:
			// an unchanged config is now a no-op and would exercise nothing.
			yaml := []string{"interval: 15m", "interval: 20m"}[reloads.Add(1)%2]
			call(t, pluginabi.MethodPluginReconfigure, lifecycleRequest{ConfigYAML: []byte(yaml)})
		},
	} {
		wg.Add(1)
		go func(work func()) {
			defer wg.Done()
			<-start
			for range 20 {
				work()
			}
		}(work)
	}
	close(start)
	wg.Wait()
}

func TestShutdownThenManagementDoesNotPanic(t *testing.T) {
	configure(t, "interval: 15m")
	call(t, pluginabi.MethodPluginShutdown, nil)
	call(t, pluginabi.MethodPluginShutdown, nil)
	if code := managementCall(t, "GET", pluginName+statusPath).StatusCode; code != 503 {
		t.Fatalf("got %d after shutdown, want 503", code)
	}
}

func managementCall(t *testing.T, method, path string) managementResponse {
	t.Helper()
	env := call(t, pluginabi.MethodManagementHandle, managementRequest{Method: method, Path: path})
	if !env.OK {
		t.Fatalf("management call failed: %+v", env.Error)
	}
	var resp managementResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestUnchangedConfigDoesNotRebuildTheWorker(t *testing.T) {
	// The host calls reconfigure after every auth-file change, and that
	// includes the plugin's own weight write-back. Rebuilding the worker there
	// restarts the ticker, drops the burn baseline, and runs an extra full
	// probe whose empty report overwrites the one that recorded the write —
	// so the status route reports 0 pending, 0 written right after a real one.
	configure(t, "interval: 15m")
	before := current

	if env := call(t, pluginabi.MethodPluginReconfigure, lifecycleRequest{ConfigYAML: []byte("interval: 15m")}); !env.OK {
		t.Fatalf("reconfigure rejected: %+v", env.Error)
	}
	if current != before {
		t.Fatal("an unchanged config must leave the running worker in place")
	}
	select {
	case <-before.stop:
		t.Fatal("an unchanged config must not halt the running ticker")
	default:
	}
}

func TestChangedConfigStillRebuildsTheWorker(t *testing.T) {
	configure(t, "interval: 15m")
	before := current

	if env := call(t, pluginabi.MethodPluginReconfigure, lifecycleRequest{ConfigYAML: []byte("interval: 20m")}); !env.OK {
		t.Fatalf("reconfigure rejected: %+v", env.Error)
	}
	if current == before {
		t.Fatal("a changed config must replace the worker")
	}
	if current.cfg.Interval != 20*time.Minute {
		t.Fatalf("interval %s, want 20m", current.cfg.Interval)
	}
}
