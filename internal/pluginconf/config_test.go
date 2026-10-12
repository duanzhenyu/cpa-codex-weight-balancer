package pluginconf

import (
	"math"
	"testing"
	"time"

	"github.com/duanzhenyu/cpa-codex-weight-balancer/internal/balancer"
	"github.com/duanzhenyu/cpa-codex-weight-balancer/internal/controller"
)

func TestDecodeDefaults(t *testing.T) {
	got, err := Decode(nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Interval != 15*time.Minute {
		t.Fatalf("interval %v", got.Interval)
	}
	if got.Options.Endpoint != controller.UsageEndpoint {
		t.Fatalf("endpoint %q", got.Options.Endpoint)
	}
	if got.Options.Capacity != 0 {
		t.Fatalf("capacity %v, want bare percentages by default", got.Options.Capacity)
	}
	p := got.Options.Policy
	if p.Params.Alpha != balancer.DefaultAlpha || p.Params.TFloor != balancer.DefaultTFloor {
		t.Fatalf("params %+v", p.Params)
	}
	if p.Quantizer.Q != balancer.DefaultQ || p.Quantizer.Margin != balancer.DefaultMargin {
		t.Fatalf("quantizer %+v", p.Quantizer)
	}
	if p.Warmup != balancer.DefaultWarmup || p.Freshness != balancer.DefaultFreshness {
		t.Fatalf("policy %+v", p)
	}
	if got.DryRun {
		t.Fatal("dry run must default off")
	}
}

func TestDecodeAllFields(t *testing.T) {
	got, err := Decode([]byte(`
interval: 30m
endpoint: https://example.invalid/usage
quota_capacity: 5000
alpha: 1.5
t_floor: 20m
bucket_q: 0.05
hysteresis: 0.4
warmup: 3
freshness: 2h
dry_run: true
`))
	if err != nil {
		t.Fatal(err)
	}
	if got.Interval != 30*time.Minute || !got.DryRun {
		t.Fatalf("got %+v", got)
	}
	if got.Options.Endpoint != "https://example.invalid/usage" || got.Options.Capacity != 5000 {
		t.Fatalf("options %+v", got.Options)
	}
	p := got.Options.Policy
	if p.Params.Alpha != 1.5 || p.Params.TFloor != 20*time.Minute {
		t.Fatalf("params %+v", p.Params)
	}
	if p.Quantizer.Q != 0.05 || p.Quantizer.Margin != 0.4 {
		t.Fatalf("quantizer %+v", p.Quantizer)
	}
	if p.Warmup != 3 || p.Freshness != 2*time.Hour {
		t.Fatalf("policy %+v", p)
	}
	// Bounds the author did not expose must keep their defaults.
	if p.Quantizer.MinWeight != balancer.DefaultMinWeight || p.Quantizer.MaxWeight != balancer.DefaultMaxWeight {
		t.Fatalf("weight bounds %+v", p.Quantizer)
	}
}

func TestDecodeRejectsBadInput(t *testing.T) {
	for name, body := range map[string]string{
		"invalid yaml":        "interval: [",
		"bad interval":        "interval: soon",
		"bad t_floor":         "t_floor: later",
		"bad freshness":       "freshness: whenever",
		"interval too fast":   "interval: 10s",
		"negative alpha":      "alpha: -1",
		"zero bucket_q":       "bucket_q: 0",
		"negative bucket_q":   "bucket_q: -0.1",
		"negative hysteresis": "hysteresis: -0.2",
		"warmup below one":    "warmup: 0",
		"negative capacity":   "quota_capacity: -5",
		"zero t_floor":        "t_floor: 0s",
	} {
		if _, err := Decode([]byte(body)); err == nil {
			t.Fatalf("%s: want an error", name)
		}
	}
}

func TestDecodeAlphaZeroIsHonoured(t *testing.T) {
	// alpha 0 is a documented mode — weight by remaining quota, ignore the
	// deadline — and must not be mistaken for an absent field.
	got, err := Decode([]byte("alpha: 0"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Options.Policy.Params.Alpha != math.SmallestNonzeroFloat64 {
		t.Fatalf("alpha %v, want the smallest positive float", got.Options.Policy.Params.Alpha)
	}
}

func TestDecodeCommentMentioningAFieldIsNotTheField(t *testing.T) {
	got, err := Decode([]byte("# bucket_q and warmup are left at their defaults\ninterval: 20m"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Options.Policy.Quantizer.Q != balancer.DefaultQ || got.Options.Policy.Warmup != balancer.DefaultWarmup {
		t.Fatalf("a comment must not change anything: %+v", got.Options.Policy)
	}
}

func TestDecodeIntervalFloorIsInclusive(t *testing.T) {
	if _, err := Decode([]byte("interval: 1m")); err != nil {
		t.Fatalf("the floor itself must be accepted: %v", err)
	}
}

func TestRejectsNonFiniteNumbers(t *testing.T) {
	// YAML has literals for these, and every validation here is a comparison
	// that NaN silently passes. An infinite alpha drives every score to zero,
	// which the state machine reads as "quota exhausted" and writes 0 weight
	// across the whole fleet.
	for _, yaml := range []string{
		"alpha: .nan",
		"alpha: .inf",
		"bucket_q: .nan",
		"bucket_q: .inf",
		"hysteresis: .nan",
		"quota_capacity: .inf",
	} {
		if _, err := Decode([]byte(yaml)); err == nil {
			t.Fatalf("%q was accepted", yaml)
		}
	}
}

func TestRejectsEndpointThatWouldLeakTokens(t *testing.T) {
	// Every probe carries a bearer token in the Authorization header, so a
	// mistyped endpoint is a fleet-wide credential leak.
	for _, yaml := range []string{
		"endpoint: http://chatgpt.com/backend-api/wham/usage",
		"endpoint: ftp://example.com/usage",
		"endpoint: not a url at all",
		"endpoint: https://",
	} {
		if _, err := Decode([]byte(yaml)); err == nil {
			t.Fatalf("%q was accepted", yaml)
		}
	}
	if _, err := Decode([]byte("endpoint: https://example.internal/usage")); err != nil {
		t.Fatalf("a plain https endpoint must stay configurable: %v", err)
	}
}

func TestErrorPenaltySwitch(t *testing.T) {
	if got := decode(t, ""); got.Options.Policy.ErrorPenalty {
		t.Fatal("error_penalty must default off")
	}
	if got := decode(t, "error_penalty: true"); !got.Options.Policy.ErrorPenalty {
		t.Fatal("error_penalty: true was not applied")
	}
}

func TestErrorWindow(t *testing.T) {
	if got := decode(t, "").Options.Policy.ErrorWindow; got != balancer.DefaultErrorWindow {
		t.Fatalf("error_window %v, want %v by default", got, balancer.DefaultErrorWindow)
	}
	if got := decode(t, "error_window: 6h").Options.Policy.ErrorWindow; got != 6*time.Hour {
		t.Fatalf("error_window %v, want 6h", got)
	}
	if got := decode(t, "interval: 30m\nerror_window: 30m").Options.Policy.ErrorWindow; got != 30*time.Minute {
		t.Fatalf("error_window %v: a window of one interval must be accepted", got)
	}
	// Configs written before the window existed must keep loading.
	if got := decode(t, "interval: 24h").Options.Policy.ErrorWindow; got != 24*time.Hour {
		t.Fatalf("error_window %v with a 24h interval, want it stretched to one interval", got)
	}
	for _, yaml := range []string{"error_window: soon", "error_window: 10m", "interval: 30m\nerror_window: 20m"} {
		if _, err := Decode([]byte(yaml)); err == nil {
			t.Fatalf("%q was accepted", yaml)
		}
	}
}

func TestResetCreditsSwitch(t *testing.T) {
	if got := decode(t, ""); got.Options.ResetCredits {
		t.Fatal("reset_credits must default off")
	}
	if got := decode(t, "reset_credits: true"); !got.Options.ResetCredits {
		t.Fatal("reset_credits: true was not applied")
	}
}
