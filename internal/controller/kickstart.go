package controller

import (
	"errors"
	"fmt"
	"maps"
	"sync"
	"time"

	"github.com/duanzhenyu/cpa-codex-weight-balancer/internal/codex"
)

// KickstartEndpoint is Codex's inference endpoint, the one CPA itself posts to
// (internal/runtime/executor/codex_executor_execute.go). A Codex rate-limit
// window starts at the account's first metered request and not before: verified
// live on 2026-09-12, where a credential probed every cycle for a day still
// reported a reset exactly one window away. Only inference starts the clock.
const KickstartEndpoint = "https://chatgpt.com/backend-api/codex/responses"

// DefaultKickstartModel is the cheapest model this fleet's accounts accept.
// Codex rejects the small ones outright — gpt-5.3-codex-spark answers "not
// supported when using Codex with a ChatGPT account" — so this is not a
// free choice, and it stays configurable for when the catalogue moves.
const DefaultKickstartModel = "gpt-5.6-sol"

// DefaultKickstartCooldown bounds the damage when a kickstart returns 200 and
// the window stays dormant anyway. Dormancy normally clears itself on the next
// cycle, so this only ever governs the case where it does not.
const DefaultKickstartCooldown = time.Hour

// kickstartBody is the smallest request Codex meters. instructions must be
// present, and stream must be true: the endpoint answers SSE and rejects a
// non-streaming call.
const kickstartBody = `{"model":%q,"instructions":"","input":[{"type":"message","role":"user",` +
	`"content":[{"type":"input_text","text":"ok"}]}],"stream":true,"store":false}`

// kickstarts records which credentials have been nudged and when. The status
// route reads it while a cycle writes it.
type kickstarts struct {
	mu      sync.Mutex
	last    map[string]time.Time
	dormant map[string]codex.Dormancy
	sent    int
	lastErr string
}

// due reports whether this credential may be nudged now, and books the attempt
// if so. Booking on the attempt rather than on success is deliberate: a
// kickstart that fails mid-flight may still have been metered upstream.
func (k *kickstarts) due(authIndex string, now time.Time, cooldown time.Duration) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	if last, ok := k.last[authIndex]; ok && now.Sub(last) < cooldown {
		return false
	}
	if k.last == nil {
		k.last = make(map[string]time.Time)
	}
	k.last[authIndex] = now
	return true
}

// observe replaces the whole dormancy map with what this cycle saw. Updating it
// per key instead would go on listing a credential the host has stopped
// serving, and the status route is the only diagnostic there is.
//
// The cooldown map is deliberately left alone: a credential that vanishes for
// one cycle — an unreadable document, say — must not come back nudgeable.
func (k *kickstarts) observe(dormant map[string]codex.Dormancy) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.dormant = dormant
}

func (k *kickstarts) record(err error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if err != nil {
		k.lastErr = err.Error()
		return
	}
	k.sent++
	k.lastErr = ""
}

// KickstartState is what a cycle has learned about dormancy and spent on it.
// It is both what the status route renders and what survives a configuration
// reload: the host rebuilds the worker, and a lost cooldown permits an
// immediate repeat of a request that may already have been metered upstream.
type KickstartState struct {
	// Last is when each credential was last nudged, keyed by auth index.
	Last    map[string]time.Time
	Dormant map[string]codex.Dormancy
	Sent    int
	LastErr string
}

// Kickstarts copies the state out. The status route reads it while a cycle
// writes it.
func (c *Controller) Kickstarts() KickstartState {
	c.kicks.mu.Lock()
	defer c.kicks.mu.Unlock()
	return KickstartState{
		Last:    maps.Clone(c.kicks.last),
		Dormant: maps.Clone(c.kicks.dormant),
		Sent:    c.kicks.sent,
		LastErr: c.kicks.lastErr,
	}
}

// RestoreKickstarts hands the state to the worker replacing this one.
func (c *Controller) RestoreKickstarts(s KickstartState) {
	c.kicks.mu.Lock()
	defer c.kicks.mu.Unlock()
	c.kicks.last = maps.Clone(s.Last)
	c.kicks.dormant = maps.Clone(s.Dormant)
	c.kicks.sent = s.Sent
	c.kicks.lastErr = s.LastErr
}

// kickstart spends one minimal inference request on a credential with a dormant
// window. One request is all it takes for either budget: Codex starts both of an
// account's windows at the same instant, verified live on 2026-09-12 where a
// served account read 7917/18000 and 594717/604800, both exactly 10083s in.
//
// It deliberately ignores the response body, and cannot confirm that anything
// started: the quota headers it answers with describe the account as it was
// *before* this request, so they read as dormant all over again.
func (c *Controller) kickstart(authIndex string, now time.Time) {
	if !c.kicks.due(authIndex, now, c.kickstartCooldown()) {
		return
	}
	// The cycle wrote weights between pricing this credential and reaching
	// here, and host.auth.save replaces the whole document with no CAS — so the
	// bytes read at the top of the cycle may already name a refreshed token.
	_, raw, err := c.host.AuthGet(authIndex)
	if err != nil {
		// record feeds the status route, which is an HTTP endpoint and carries
		// no file names. The host's error is free to name the document it could
		// not read, so the cause goes to the log instead.
		c.kicks.record(errors.New("re-read before kickstart failed"))
		c.host.Log("warn", fmt.Sprintf("kickstart re-read failed for %s: %v", authIndex, err))
		return
	}
	token := credentialString(raw, "access_token")
	if token == "" {
		c.kicks.record(errors.New("credential carries no access token"))
		return
	}
	headers := map[string][]string{
		"Authorization": {"Bearer " + token},
		"Content-Type":  {"application/json"},
		"Accept":        {"text/event-stream"},
		"Originator":    {Originator},
		"User-Agent":    {UserAgent},
	}
	if accountID := credentialString(raw, "account_id"); accountID != "" {
		headers["Chatgpt-Account-Id"] = []string{accountID}
	}

	resp, err := c.host.HTTPDo(HTTPRequest{
		Method:  "POST",
		URL:     KickstartEndpoint,
		Headers: headers,
		Body:    []byte(fmt.Sprintf(kickstartBody, c.kickstartModel())),
	})
	if err == nil && (resp.StatusCode < 200 || resp.StatusCode > 299) {
		err = fmt.Errorf("kickstart returned %d", resp.StatusCode)
	}
	c.kicks.record(err)
	if err != nil {
		c.host.Log("warn", fmt.Sprintf("kickstart failed for %s: %v", authIndex, err))
		return
	}
	c.host.Log("info", fmt.Sprintf("kickstart spent one inference request on %s", authIndex))
}

func (c *Controller) kickstartModel() string {
	if c.opts.KickstartModel != "" {
		return c.opts.KickstartModel
	}
	return DefaultKickstartModel
}

func (c *Controller) kickstartCooldown() time.Duration {
	if c.opts.KickstartCooldown > 0 {
		return c.opts.KickstartCooldown
	}
	return DefaultKickstartCooldown
}
