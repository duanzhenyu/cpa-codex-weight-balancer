package main

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"

	"gitlab.duanzhenyu.top/dzy/cpa-codex-weight-balancer/internal/balancer"
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
	// next cycle, and any weight change wipes the host's routing accumulator
	// fleet-wide.
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

func TestLifecycleIsSafeUnderConcurrentManagementCalls(t *testing.T) {
	// The host may call these concurrently. Run with -race: a status render
	// overlapping a refresh cycle used to touch the engine's maps unguarded.
	configure(t, "interval: 15m")
	var wg sync.WaitGroup
	start := make(chan struct{})
	for _, work := range []func(){
		func() { managementCall(t, "GET", pluginName+statusPath) },
		func() { managementCall(t, "POST", pluginName+refreshPath) },
		func() {
			call(t, pluginabi.MethodPluginReconfigure, lifecycleRequest{ConfigYAML: []byte("interval: 15m")})
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
