package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// handleUsage records the quota an upstream Codex response reported, sparing
// that credential a probe on the next cycle. It writes nothing: weights move
// only on the ticker, because every weight change wipes the host's whole
// smooth-WRR accumulator.
//
// Anything it cannot use is dropped rather than reported. The host fuses a
// plugin that fails on this hook, and there is no failure here worth losing
// the whole plugin over — every provider's records arrive on it.
func handleUsage(request []byte) ([]byte, error) {
	var record pluginapi.UsageRecord
	if len(request) > 0 {
		if err := json.Unmarshal(request, &record); err != nil {
			return nil, fmt.Errorf("decode usage record: %w", err)
		}
	}
	if !strings.EqualFold(strings.TrimSpace(record.Provider), "codex") {
		return okEnvelope(map[string]any{})
	}
	// A rejected request answers with well-formed headers of its own, and a 429
	// answers with headers saying the account is spent. Pricing off those would
	// run the credential through rate.Blocked to weight 0 — a hard exclusion
	// that evicts its bound sticky sessions too — at request rate instead of on
	// the timer. Reacting to a 429 is the host's job. The probe reaches the same
	// verdict on the next cycle if the account really is out.
	if record.Failed || record.Failure.StatusCode >= 400 {
		return okEnvelope(map[string]any{})
	}

	mu.Lock()
	worker := current
	mu.Unlock()
	// The host can deliver a record between shutdown and the next register.
	if worker == nil {
		return okEnvelope(map[string]any{})
	}

	worker.ctl.Observe(record.AuthIndex, record.ResponseHeaders, usageObservedAt(record))
	return okEnvelope(map[string]any{})
}

// usageObservedAt is when the response that carried these headers arrived. The
// host dispatches usage records off its own queue, so the clock read in this
// hook can trail the response by however long that queue was backed up — and
// the engine divides the quota drained between two snapshots by exactly this
// interval to report the observed burn rate.
func usageObservedAt(record pluginapi.UsageRecord) time.Time {
	now := time.Now().UTC()
	if record.RequestedAt.IsZero() || record.Latency <= 0 {
		return now
	}
	// Never stamp the future whatever the record claims: Compute rejects a
	// snapshot observed after the cycle that consumes it.
	if completed := record.RequestedAt.Add(record.Latency).UTC(); completed.Before(now) {
		return completed
	}
	return now
}
