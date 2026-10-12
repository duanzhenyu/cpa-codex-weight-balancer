package main

import (
	"slices"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/duanzhenyu/cpa-codex-weight-balancer/internal/balancer"
	"github.com/duanzhenyu/cpa-codex-weight-balancer/internal/controller"
)

func TestResetCreditsAreAdvertisedAndReported(t *testing.T) {
	if !slices.ContainsFunc(pluginRegistration().Metadata.ConfigFields, func(f pluginapi.ConfigField) bool {
		return f.Name == "reset_credits" && f.Type == pluginapi.ConfigFieldTypeBoolean
	}) {
		t.Fatal("reset_credits is not advertised")
	}

	soon := time.Now().UTC().Add(48 * time.Hour).Truncate(time.Second)
	keys := []string{"reset_credits", "reset_credit_next_expiry", "reset_credits_error"}
	for yaml, on := range map[string]bool{"interval: 15m\nreset_credits: true": true, "interval: 15m": false} {
		cfg := decode(t, yaml)
		w := &worker{cfg: cfg, ctl: controller.New(hostBridge{}, cfg.Options)}
		w.lastRep = balancer.Report{Decisions: []balancer.Decision{{AuthID: "a", State: balancer.StateActive, Weight: 900}}}
		w.ctl.RestoreResetCredits(controller.ResetCreditsState{
			Held:   map[string][]time.Time{"a": {time.Now().UTC().Add(-time.Hour), soon, soon.Add(24 * time.Hour)}},
			Errors: map[string]string{"a": "reset credits fetch returned 503"},
		})

		out := w.status()
		if out["reset_credits"] != on {
			t.Fatalf("%q: reset_credits = %v", yaml, out["reset_credits"])
		}
		row := out["credentials"].([]map[string]any)[0]
		if !on {
			for _, k := range keys {
				if v, ok := row[k]; ok {
					t.Fatalf("%q: %s = %v shown with the switch off", yaml, k, v)
				}
			}
			continue
		}
		// An expired credit can no longer be redeemed, so it is not held.
		if row["reset_credits"] != 2 || row["reset_credit_next_expiry"] != soon.Format(time.RFC3339) {
			t.Fatalf("row %+v, want 2 credits, the next expiring at %s", row, soon.Format(time.RFC3339))
		}
		if row["reset_credits_error"] != "reset credits fetch returned 503" {
			t.Fatalf("row %+v: a failed fetch must be visible", row)
		}
	}
}

// The host reconfigures on every auth-file change, its own write-back included.
// A rebuild that drops the credits leaves rechecks pricing without them until
// the next tick.
func TestReconfigureCarriesResetCreditsAcross(t *testing.T) {
	configure(t, "interval: 15m\nreset_credits: true\ndry_run: true")
	mu.Lock()
	ctl := current.ctl
	mu.Unlock()
	ctl.RestoreResetCredits(controller.ResetCreditsState{
		Held: map[string][]time.Time{"abc": {time.Now().UTC().Add(48 * time.Hour)}},
	})

	configure(t, "interval: 16m\nreset_credits: true\ndry_run: true")
	mu.Lock()
	got := current.ctl.ResetCredits()
	mu.Unlock()
	if len(got.Held["abc"]) != 1 {
		t.Fatalf("reload dropped the credits: %+v", got)
	}
}
