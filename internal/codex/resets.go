package codex

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"
)

// ParseResetCredits reads a /wham/rate-limit-reset-credits payload into the
// expiry of every credit that can still be redeemed against the Codex limits,
// soonest first. A redeemed credit restores both windows at once, so it is one
// more full window of quota, and it is worth anything only if redeemed before it
// expires. /wham/usage carries the count alone; only this endpoint dates them.
func ParseResetCredits(raw []byte) ([]time.Time, error) {
	var doc struct {
		Credits *[]struct {
			ResetType string `json:"reset_type"`
			Supported bool   `json:"is_supported_by_plan"`
			Status    string `json:"status"`
			ExpiresAt string `json:"expires_at"`
		} `json:"credits"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("decode codex reset credits: %w", err)
	}
	if doc.Credits == nil {
		return nil, fmt.Errorf("codex reset credits payload carries no credits list")
	}
	expiries := []time.Time{}
	for _, c := range *doc.Credits {
		if c.ResetType != "codex_rate_limits" || !c.Supported || c.Status != "available" {
			continue
		}
		// Skipping it would return a shorter list that replaces the last trusted
		// one and reprices the account.
		expiry, err := time.Parse(time.RFC3339Nano, c.ExpiresAt)
		if err != nil {
			return nil, errors.New("codex reset credit carries no usable expiry")
		}
		expiries = append(expiries, expiry.UTC())
	}
	slices.SortFunc(expiries, time.Time.Compare)
	return expiries, nil
}
