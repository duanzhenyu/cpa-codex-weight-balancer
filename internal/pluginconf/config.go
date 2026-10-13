// Package pluginconf decodes the plugin's block of the host config.
package pluginconf

import (
	"fmt"
	"math"
	"net/url"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/duanzhenyu/cpa-codex-weight-balancer/internal/balancer"
	"github.com/duanzhenyu/cpa-codex-weight-balancer/internal/controller"
)

// DefaultInterval matches the design doc: tens of minutes, not every minute.
const DefaultInterval = 15 * time.Minute

// MinInterval floors the reweight cadence. Every weight change wipes the host's
// smooth-WRR accumulator fleet-wide, so a fast cadence degrades routing into
// "always pick the largest weight" — see the design doc, §5.4 constraint 1.
const MinInterval = time.Minute

// Config is the YAML block under plugins.configs.cpa-codex-weight-balancer.
type Config struct {
	Interval string `yaml:"interval"`
	Endpoint string `yaml:"endpoint"`
	// Pointers distinguish "absent" from "explicitly zero": 0 is a meaningful
	// value for alpha and an invalid one for bucket_q and warmup.
	QuotaCapacity *float64 `yaml:"quota_capacity"`
	Alpha         *float64 `yaml:"alpha"`
	TFloor        string   `yaml:"t_floor"`
	BucketQ       *float64 `yaml:"bucket_q"`
	Hysteresis    *float64 `yaml:"hysteresis"`
	Warmup        *int     `yaml:"warmup"`
	Freshness     string   `yaml:"freshness"`
	DryRun        bool     `yaml:"dry_run"`
	// The kickstart switches are the only settings that spend quota, so they are
	// plain bools with no pointer games: absent must mean off.
	Kickstart            bool   `yaml:"kickstart"`
	KickstartShortWindow bool   `yaml:"kickstart_short_window"`
	KickstartModel       string `yaml:"kickstart_model"`
	KickstartCooldown    string `yaml:"kickstart_cooldown"`
	ErrorPenalty         bool   `yaml:"error_penalty"`
	ErrorWindow          string `yaml:"error_window"`
	ResetCredits         bool   `yaml:"reset_credits"`
}

// Resolved is a validated configuration.
type Resolved struct {
	Interval time.Duration
	DryRun   bool
	Options  controller.Options
}

// Decode parses and validates the plugin configuration block. Every unset field
// falls back to the documented default; every set field is validated, because a
// nonsensical value here silently mis-prices the whole fleet.
func Decode(raw []byte) (Resolved, error) {
	var cfg Config
	if len(raw) > 0 {
		if err := yaml.Unmarshal(raw, &cfg); err != nil {
			return Resolved{}, fmt.Errorf("decode plugin config: %w", err)
		}
	}

	policy := balancer.DefaultPolicy()
	out := Resolved{Interval: DefaultInterval, DryRun: cfg.DryRun}
	out.Options.Endpoint = controller.UsageEndpoint
	out.Options.DryRun = cfg.DryRun
	interval, err := duration(cfg.Interval, DefaultInterval, "interval")
	if err != nil {
		return Resolved{}, err
	}
	if interval < MinInterval {
		return Resolved{}, fmt.Errorf("interval %s is below the %s floor: a faster cadence wipes the host routing accumulator every cycle", interval, MinInterval)
	}
	out.Interval = interval

	if endpoint := strings.TrimSpace(cfg.Endpoint); endpoint != "" {
		if err = checkEndpoint(endpoint); err != nil {
			return Resolved{}, err
		}
		out.Options.Endpoint = endpoint
	}
	out.Options.Kickstart = cfg.Kickstart
	out.Options.KickstartShortWindow = cfg.KickstartShortWindow
	out.Options.KickstartModel = controller.DefaultKickstartModel
	if model := strings.TrimSpace(cfg.KickstartModel); model != "" {
		out.Options.KickstartModel = model
	}
	if out.Options.KickstartCooldown, err = duration(cfg.KickstartCooldown, controller.DefaultKickstartCooldown, "kickstart_cooldown"); err != nil {
		return Resolved{}, err
	}
	// Dormancy normally clears itself on the next cycle, so this floor only
	// governs a kickstart that answers 200 and starts nothing. Left unfloored,
	// that case bills the account once per cycle forever.
	if out.Options.KickstartCooldown < MinInterval {
		return Resolved{}, fmt.Errorf("kickstart_cooldown %s is below the %s floor: a shorter one re-bills an account whose window refuses to start", out.Options.KickstartCooldown, MinInterval)
	}

	if cfg.QuotaCapacity != nil {
		if err = finite(*cfg.QuotaCapacity, "quota_capacity"); err != nil {
			return Resolved{}, err
		}
		if *cfg.QuotaCapacity < 0 {
			return Resolved{}, fmt.Errorf("quota_capacity must not be negative")
		}
		out.Options.Capacity = *cfg.QuotaCapacity
	}

	if cfg.Alpha != nil {
		if err = finite(*cfg.Alpha, "alpha"); err != nil {
			return Resolved{}, err
		}
		if *cfg.Alpha < 0 {
			return Resolved{}, fmt.Errorf("alpha must not be negative")
		}
		// alpha 0 means "ignore the deadline, weight by remaining quota alone".
		// The scorer reads a non-positive alpha as "unset", so the smallest
		// positive float stands in: x^5e-324 and x^0 are both 1.
		policy.Params.Alpha = max(*cfg.Alpha, math.SmallestNonzeroFloat64)
	}
	if policy.Params.TFloor, err = duration(cfg.TFloor, balancer.DefaultTFloor, "t_floor"); err != nil {
		return Resolved{}, err
	}
	if policy.Params.TFloor <= 0 {
		return Resolved{}, fmt.Errorf("t_floor must be positive")
	}
	if policy.Freshness, err = duration(cfg.Freshness, balancer.DefaultFreshness, "freshness"); err != nil {
		return Resolved{}, err
	}

	if cfg.BucketQ != nil {
		if err = finite(*cfg.BucketQ, "bucket_q"); err != nil {
			return Resolved{}, err
		}
		if *cfg.BucketQ <= 0 {
			return Resolved{}, fmt.Errorf("bucket_q must be positive")
		}
		policy.Quantizer.Q = *cfg.BucketQ
	}
	if cfg.Hysteresis != nil {
		if err = finite(*cfg.Hysteresis, "hysteresis"); err != nil {
			return Resolved{}, err
		}
		if *cfg.Hysteresis < 0 {
			return Resolved{}, fmt.Errorf("hysteresis must not be negative")
		}
		policy.Quantizer.Margin = *cfg.Hysteresis
	}
	if cfg.Warmup != nil {
		if *cfg.Warmup < 1 {
			return Resolved{}, fmt.Errorf("warmup must be at least 1")
		}
		policy.Warmup = *cfg.Warmup
	}
	policy.ErrorPenalty = cfg.ErrorPenalty
	// The default stretches to a long interval so configs that predate the
	// window keep loading.
	if policy.ErrorWindow, err = duration(cfg.ErrorWindow, max(balancer.DefaultErrorWindow, out.Interval), "error_window"); err != nil {
		return Resolved{}, err
	}
	// A shorter window holds only the latest tick's tally, and a rate that swings
	// with every tick moves a bucket — and wipes the host's accumulator — far
	// more often.
	if policy.ErrorWindow < out.Interval {
		return Resolved{}, fmt.Errorf("error_window %s is shorter than interval %s: it would hold only the latest cycle's requests", policy.ErrorWindow, out.Interval)
	}
	out.Options.ResetCredits = cfg.ResetCredits

	out.Options.Policy = policy
	return out, nil
}

// finite rejects the YAML literals .nan and .inf. Every other check here is a
// comparison, and NaN passes all of them: an infinite alpha drives every score
// to zero, which the state machine reads as exhausted quota and answers by
// zeroing the whole fleet's weights.
func finite(v float64, field string) error {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return fmt.Errorf("%s must be a finite number", field)
	}
	return nil
}

// checkEndpoint refuses anything that would put a bearer token on the wire in
// the clear. It does not restrict the host: the endpoint is configurable
// precisely because Codex can move it, and pinning a hostname here would defeat
// that. The operator therefore owns this value — whatever host it names
// receives every account's access token.
func checkEndpoint(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("endpoint: %w", err)
	}
	if parsed.Scheme != "https" {
		return fmt.Errorf("endpoint must be https: every probe carries an access token")
	}
	if parsed.Host == "" {
		return fmt.Errorf("endpoint has no host")
	}
	return nil
}

func duration(raw string, fallback time.Duration, field string) (time.Duration, error) {
	if strings.TrimSpace(raw) == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil {
		return 0, fmt.Errorf("%s: %w", field, err)
	}
	return parsed, nil
}
