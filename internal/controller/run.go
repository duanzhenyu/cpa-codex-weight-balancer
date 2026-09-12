package controller

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/duanzhenyu/cpa-codex-weight-balancer/internal/authfile"
	"github.com/duanzhenyu/cpa-codex-weight-balancer/internal/balancer"
	"github.com/duanzhenyu/cpa-codex-weight-balancer/internal/codex"
	"github.com/duanzhenyu/cpa-codex-weight-balancer/internal/quota"
)

func (c *Controller) run(now time.Time) (balancer.Report, error) {
	entries, err := c.host.AuthList()
	if err != nil {
		return balancer.Report{}, fmt.Errorf("list credentials: %w", err)
	}

	observed := c.observed.drain()
	names := make(map[string]string, len(entries))
	observations := make([]balancer.Observation, 0, len(entries))
	for _, entry := range entries {
		// The host lists credentials at registration before it has assigned
		// their runtime indices. Acting on one files its observation under the
		// empty id, collapsing the fleet onto a phantom account.
		if strings.TrimSpace(entry.AuthIndex) == "" {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(entry.Type), "codex") || entry.Disabled {
			continue
		}
		name, raw, errGet := c.host.AuthGet(entry.AuthIndex)
		if errGet != nil {
			observations = append(observations, failed(entry.AuthIndex, errGet))
			continue
		}
		names[entry.AuthIndex] = name
		observation := c.price(entry.AuthIndex, raw, observed, now)
		observation.Disk = diskWeight(raw)
		observations = append(observations, observation)
	}

	report := c.engine.Cycle(observations, now)
	for _, decision := range report.Decisions {
		if decision.Error != "" {
			c.host.Log("warn", fmt.Sprintf("probe failed for %s: %s", decision.AuthID, decision.Error))
		}
		if !decision.Write {
			continue
		}
		if c.opts.DryRun {
			c.host.Log("info", fmt.Sprintf("dry run: %s would move to weight %d", decision.AuthID, decision.Weight))
			continue
		}
		saved, errWrite := c.write(names[decision.AuthID], decision.AuthID, decision.Weight)
		if errWrite != nil {
			c.host.Log("warn", fmt.Sprintf("weight write-back failed for %s: %v", decision.AuthID, errWrite))
			continue
		}
		// Only a confirmed save may be recorded. Committing on intent leaves a
		// rejected write permanently outstanding, because every later cycle
		// would see the weight as already on disk and skip it.
		c.engine.Commit(decision.AuthID, decision.Weight)
		if saved {
			report.Wrote++
		}
	}
	return report, nil
}

// price turns one credential into an observation, preferring the quota a
// request the host already served reported over spending a probe on it. Only a
// credential nothing has used since the last cycle is probed, and staleness
// there is self-limiting: an account quiet enough to have no fresh reading is
// also one whose windows have barely moved.
func (c *Controller) price(authIndex string, raw []byte, observed map[string]codex.Usage, now time.Time) balancer.Observation {
	if usage, ok := observed[authIndex]; ok {
		// A cycle reads its clock before it drains, so a record that landed in
		// between is stamped after this cycle's now and Compute rejects it as
		// observed in the future — freezing an account that just reported
		// perfectly good quota. Clamping loses nothing a deadline depends on:
		// every reset in the snapshot is an absolute instant. It does shorten
		// the interval the engine divides by for the observed burn rate, but
		// only by the width of one drain.
		if usage.Snapshot.ObservedAt.After(now) {
			usage.Snapshot.ObservedAt = now
		}
		return c.observation(usage)
	}
	return c.probe(authIndex, raw, now)
}

// probe fetches and parses one account's quota. Every failure path yields a
// frozen observation rather than an error: one unreachable account must not
// stop the fleet from being repriced.
func (c *Controller) probe(authIndex string, raw []byte, now time.Time) balancer.Observation {
	token := credentialString(raw, "access_token")
	if token == "" {
		return failed(authIndex, errors.New("credential carries no access token"))
	}
	headers := map[string][]string{
		"Authorization": {"Bearer " + token},
		"Accept":        {"application/json"},
		"Originator":    {Originator},
		"User-Agent":    {UserAgent},
	}
	if accountID := credentialString(raw, "account_id"); accountID != "" {
		headers["Chatgpt-Account-Id"] = []string{accountID}
	}

	resp, err := c.host.HTTPDo(HTTPRequest{Method: "GET", URL: c.opts.Endpoint, Headers: headers})
	if err != nil {
		return failed(authIndex, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return failed(authIndex, fmt.Errorf("usage probe returned %d", resp.StatusCode))
	}

	usage, err := codex.ParseUsage(resp.Body, authIndex, 0, now)
	if err != nil {
		return failed(authIndex, err)
	}
	return c.observation(usage)
}

// observation stamps a parsed quota with this cycle's generation and puts it in
// the fleet's unit. Both transports pass through here so a header-fed account
// and a probed one are scored on the same axis.
func (c *Controller) observation(u codex.Usage) balancer.Observation {
	c.generation++
	u.Snapshot.Generation = c.generation
	return balancer.Observation{
		Snapshot:      u.Snapshot.Scaled(c.opts.Capacity),
		CircuitBroken: u.LimitReached,
	}
}

// write re-reads the credential immediately before saving. host.auth.save
// replaces the whole document with no compare-and-swap, so patching bytes read
// earlier in the cycle would silently undo anything written in between —
// a token refresh, most importantly.
//
// It reports whether a save actually reached the host: a weight already on
// disk is skipped, and counting that skip as a write would tell the operator
// the routing accumulator had been wiped when nothing moved. A restart hits
// this path for every credential, having lost its record of what it wrote.
func (c *Controller) write(name, authIndex string, weight int64) (bool, error) {
	fresh, raw, err := c.host.AuthGet(authIndex)
	if err != nil {
		return false, fmt.Errorf("re-read before save: %w", err)
	}
	// The re-read is also the authority on where the credential lives: CPA may
	// have re-created the file under a different name since the probe.
	if fresh != "" {
		name = fresh
	}
	if name == "" {
		return false, fmt.Errorf("no auth file name for %s", authIndex)
	}
	if current, ok := authfile.Weight(raw); ok && current == weight {
		return false, nil
	}
	patched, err := authfile.SetWeight(raw, weight)
	if err != nil {
		return false, err
	}
	if err = c.host.AuthSave(name, patched); err != nil {
		return false, err
	}
	return true, nil
}

// diskWeight reports what the credential file holds right now. The probe
// already has the bytes, so telling the engine costs nothing — and without it
// the engine trusts its own record of what it wrote, which anything else may
// have overwritten since.
func diskWeight(raw []byte) balancer.DiskWeight {
	weight, present := authfile.Weight(raw)
	return balancer.DiskWeight{Weight: weight, Present: present, Read: true}
}

func failed(authIndex string, err error) balancer.Observation {
	return balancer.Observation{Snapshot: quota.Snapshot{AuthID: authIndex}, Err: err}
}

// credentialString reads a top-level string field, falling back to the nested
// token object that some credential layouts use.
func credentialString(raw []byte, key string) string {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return ""
	}
	if value, ok := stringOf(fields[key]); ok {
		return value
	}
	var nested struct {
		Token map[string]json.RawMessage `json:"token"`
	}
	if json.Unmarshal(raw, &nested) == nil {
		if value, ok := stringOf(nested.Token[key]); ok {
			return value
		}
	}
	return ""
}

func stringOf(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var out string
	if json.Unmarshal(raw, &out) != nil {
		return "", false
	}
	return strings.TrimSpace(out), strings.TrimSpace(out) != ""
}
