package controller

import (
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/duanzhenyu/cpa-codex-weight-balancer/internal/codex"
	"github.com/duanzhenyu/cpa-codex-weight-balancer/internal/quota"
)

// resetCredits holds each credential's banked reset credits as last fetched.
// The status route reads it while a cycle writes it.
type resetCredits struct {
	mu   sync.Mutex
	held map[string][]time.Time
	errs map[string]string
}

func (r *resetCredits) keep(listed map[string]bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	maps.DeleteFunc(r.held, func(id string, _ []time.Time) bool { return !listed[id] })
	maps.DeleteFunc(r.errs, func(id string, _ string) bool { return !listed[id] })
}

// fetchResets refreshes one credential's credits. A failure keeps the last list:
// it says nothing about the credits, and reading it as none would reprice the
// account — wiping the host's routing accumulator — on every failed fetch.
func (c *Controller) fetchResets(authIndex string, raw []byte) error {
	expiries, err := c.readResets(raw)
	c.resets.mu.Lock()
	defer c.resets.mu.Unlock()
	if err != nil {
		if c.resets.errs == nil {
			c.resets.errs = make(map[string]string)
		}
		c.resets.errs[authIndex] = err.Error()
		return err
	}
	if c.resets.held == nil {
		c.resets.held = make(map[string][]time.Time)
	}
	c.resets.held[authIndex] = expiries
	delete(c.resets.errs, authIndex)
	return nil
}

func (c *Controller) readResets(raw []byte) ([]time.Time, error) {
	headers, err := codexHeaders(raw)
	if err != nil {
		return nil, err
	}
	headers["OpenAI-Beta"] = []string{"codex-1"}
	resp, err := c.host.HTTPDo(HTTPRequest{Method: "GET", URL: ResetCreditsEndpoint, Headers: headers})
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("reset credits fetch returned %d", resp.StatusCode)
	}
	return codex.ParseResetCredits(resp.Body)
}

// resetWindows prices each held credit as one full window in the unit the
// snapshot was scaled to, due by its expiry. A list handed over from a worker
// that had the switch on is ignored once it is off: nothing refreshes it then.
func (c *Controller) resetWindows(authIndex string) []quota.Window {
	if !c.opts.ResetCredits {
		return nil
	}
	full, absolute := 100.0, false
	if c.opts.Capacity > 0 {
		full, absolute = c.opts.Capacity, true
	}
	c.resets.mu.Lock()
	defer c.resets.mu.Unlock()
	held := c.resets.held[authIndex]
	out := make([]quota.Window, 0, len(held))
	for _, expiry := range held {
		out = append(out, quota.Window{Remaining: full, Absolute: absolute, ResetAt: expiry})
	}
	return out
}

// ResetCreditsState is what the status route renders and what survives a
// configuration reload: a recheck prices from it, and the first cycle after a
// reload may fail to fetch.
type ResetCreditsState struct {
	Held   map[string][]time.Time
	Errors map[string]string
}

// ResetCredits copies the state out.
func (c *Controller) ResetCredits() ResetCreditsState {
	c.resets.mu.Lock()
	defer c.resets.mu.Unlock()
	return ResetCreditsState{Held: cloneHeld(c.resets.held), Errors: maps.Clone(c.resets.errs)}
}

// RestoreResetCredits hands the state to the worker replacing this one.
func (c *Controller) RestoreResetCredits(s ResetCreditsState) {
	c.resets.mu.Lock()
	defer c.resets.mu.Unlock()
	c.resets.held = cloneHeld(s.Held)
	c.resets.errs = maps.Clone(s.Errors)
}

func cloneHeld(held map[string][]time.Time) map[string][]time.Time {
	out := maps.Clone(held)
	for id, expiries := range out {
		out[id] = slices.Clone(expiries)
	}
	return out
}
