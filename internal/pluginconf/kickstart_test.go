package pluginconf

import (
	"strings"
	"testing"
	"time"

	"github.com/duanzhenyu/cpa-codex-weight-balancer/internal/controller"
)

func decode(t *testing.T, yaml string) Resolved {
	t.Helper()
	got, err := Decode([]byte(yaml))
	if err != nil {
		t.Fatalf("decode %q: %v", yaml, err)
	}
	return got
}

// Kickstart is the only setting that spends the operator's quota, so an absent
// config block must never enable it.
func TestKickstartDefaultsOff(t *testing.T) {
	got := decode(t, "")
	if got.Options.Kickstart {
		t.Fatal("kickstart must default off")
	}
	if got.Options.KickstartModel != controller.DefaultKickstartModel {
		t.Fatalf("model %q", got.Options.KickstartModel)
	}
	if got.Options.KickstartCooldown != controller.DefaultKickstartCooldown {
		t.Fatalf("cooldown %v", got.Options.KickstartCooldown)
	}
}

func TestKickstartModelOverride(t *testing.T) {
	got := decode(t, "kickstart: true\nkickstart_model: gpt-5.6-luna\nkickstart_cooldown: 30m\n")
	if !got.Options.Kickstart {
		t.Fatal("kickstart not enabled")
	}
	if got.Options.KickstartModel != "gpt-5.6-luna" {
		t.Fatalf("model %q", got.Options.KickstartModel)
	}
	if got.Options.KickstartCooldown != 30*time.Minute {
		t.Fatalf("cooldown %v", got.Options.KickstartCooldown)
	}
}

func TestBlankKickstartModelFallsBackToTheDefault(t *testing.T) {
	got := decode(t, "kickstart: true\nkickstart_model: \"   \"\n")
	if got.Options.KickstartModel != controller.DefaultKickstartModel {
		t.Fatalf("model %q", got.Options.KickstartModel)
	}
}

// The cooldown is the only thing standing between a kickstart that keeps
// failing and an account billed every cycle.
func TestKickstartCooldownIsFloored(t *testing.T) {
	for _, raw := range []string{"kickstart_cooldown: 0s\n", "kickstart_cooldown: 10s\n", "kickstart_cooldown: -1h\n"} {
		if _, err := Decode([]byte(raw)); err == nil {
			t.Fatalf("%q was accepted", raw)
		} else if !strings.Contains(err.Error(), "kickstart_cooldown") {
			t.Fatalf("%q: %v", raw, err)
		}
	}
}

func TestKickstartCooldownRejectsGarbage(t *testing.T) {
	if _, err := Decode([]byte("kickstart_cooldown: soon\n")); err == nil {
		t.Fatal("garbage duration accepted")
	}
}
