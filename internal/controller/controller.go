// Package controller runs one probe-and-reweight cycle against the host.
package controller

import (
	"strings"
	"sync"
	"time"

	"github.com/duanzhenyu/cpa-codex-weight-balancer/internal/balancer"
)

// UsageEndpoint is the Codex quota probe. It comes from the design research,
// not from the CPA source tree — CPA parses this payload but never fetches it —
// so it stays configurable.
const UsageEndpoint = "https://chatgpt.com/backend-api/wham/usage"

// ResetCreditsEndpoint lists an account's banked reset credits with their
// expiries, verified live. /wham/usage carries only their count, and no response
// header carries them at all.
const ResetCreditsEndpoint = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits"

// Codex client identification, mirroring what the host sends on codex calls.
const (
	UserAgent  = "codex-tui/0.153.3 (Mac OS 26.5.1; arm64) iTerm.app/3.6.11 (codex-tui; 0.153.3)"
	Originator = "codex-tui"
)

// AuthEntry is one credential as the host lists it.
type AuthEntry struct {
	AuthIndex   string
	Name        string
	Type        string
	Disabled    bool
	Unavailable bool
}

// HTTPRequest is one outbound call: the quota probe, or the inference request
// that starts a dormant window.
type HTTPRequest struct {
	Method  string
	URL     string
	Headers map[string][]string
	Body    []byte
}

// HTTPResponse is the probe result.
type HTTPResponse struct {
	StatusCode int
	Body       []byte
}

// Host is the slice of the plugin host callbacks this controller needs.
type Host interface {
	AuthList() ([]AuthEntry, error)
	AuthGet(authIndex string) (name string, raw []byte, err error)
	AuthSave(name string, raw []byte) error
	HTTPDo(req HTTPRequest) (HTTPResponse, error)
	Log(level, message string)
}

// Options configures a controller.
type Options struct {
	Endpoint string
	Policy   balancer.Policy
	// Capacity expresses percentages and observed burn in one absolute unit. It
	// is a single number for the whole fleet, so it does not make accounts on
	// different plans comparable, and it does not change routing: the quantizer
	// anchor moves with it.
	Capacity float64
	// DryRun computes and reports weights without writing any back. Nothing is
	// recorded as written either, so turning it off later still writes.
	DryRun bool
	// Kickstart spends one minimal inference request on a credential whose
	// weekly window has never started. It is the only thing here that costs
	// quota, so it stays off unless the operator asks for it.
	Kickstart bool
	// KickstartShortWindow does the same for the five-hour budget. It is a
	// separate switch because it buys something else and costs more often: the
	// short window never enters the score, so starting it moves no weight —
	// it only brings the window's reset forward, shortening the exclusion when
	// a later burst spends that budget. And unlike the week, it expires on its
	// own, so an idle account reads dormant again every few hours.
	KickstartShortWindow bool
	KickstartModel       string
	KickstartCooldown    time.Duration
	// ResetCredits prices each banked reset credit as one more full window due
	// by its expiry. It costs one more token-bearing request per credential per
	// cycle, and it redeems nothing: the credit pays off only if someone
	// redeems it once the account is spent.
	ResetCredits bool
}

// Controller owns the engine and the host wiring.
type Controller struct {
	host   Host
	engine *balancer.Engine
	opts   Options

	// mu serializes cycles. The ticker and the management refresh route both
	// call RunCycle, and two overlapping cycles would double-probe every
	// account and interleave their write-backs.
	mu         sync.Mutex
	generation uint64

	// observed collects the quota served requests reported since the last
	// cycle, which spares those credentials a probe.
	observed observations
	// kicks remembers which dormant credentials have already been nudged.
	kicks kickstarts
	// rechecks queues the credentials the host saw rejected, for a probe off the
	// timer.
	rechecks rechecks
	// outcomes counts each credential's requests since the last tick.
	outcomes outcomes
	// resets holds each credential's banked reset credits.
	resets resetCredits
}

// New builds a controller.
func New(h Host, o Options) *Controller {
	if strings.TrimSpace(o.Endpoint) == "" {
		o.Endpoint = UsageEndpoint
	}
	if o.Policy.Warmup <= 0 {
		o.Policy = balancer.DefaultPolicy()
	}
	// Snapshot.Scaled rewrites Remaining into capacity units, and the quantizer
	// anchor is documented in the same unit as Remaining. Leaving it behind
	// shifts every bucket by one constant: a fleet-wide rewrite that changes no
	// account's share, and a saturation onto one weight at a large capacity. An
	// anchor that underflows keeps its old value, because a non-positive one
	// divides the score by zero.
	if o.Capacity > 0 {
		if anchor := o.Policy.Quantizer.R0 / 100 * o.Capacity; anchor > 0 {
			o.Policy.Quantizer.R0 = anchor
		}
	}
	return &Controller{host: h, engine: balancer.NewEngine(o.Policy), opts: o}
}

// Engine exposes the engine for status rendering and persistence.
func (c *Controller) Engine() *balancer.Engine { return c.engine }

// RunCycle probes every Codex credential and writes back the weights that moved.
// It prices with the error rates as they stand: only Tick may fold new outcomes.
func (c *Controller) RunCycle(now time.Time) (balancer.Report, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.run(now, false)
}

// Tick is the scheduled cycle: RunCycle after folding the request outcomes
// counted since the last tick.
func (c *Controller) Tick(now time.Time) (balancer.Report, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.run(now, true)
}
