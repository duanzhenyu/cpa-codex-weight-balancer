package codex

import (
	"strings"
	"testing"
	"time"
)

// liveResetCredits is a real /wham/rate-limit-reset-credits response with the
// credit ids and the presentation fields removed.
const liveResetCredits = `{
  "credits": [
    {"id": "c1", "reset_type": "codex_rate_limits", "is_supported_by_plan": true, "status": "available",
     "granted_at": "2026-09-29T18:21:24.313601Z", "expires_at": "2026-10-29T18:21:24.313601Z",
     "redeem_started_at": null, "redeemed_at": null, "title": "Full reset (Weekly + 5 hr)"},
    {"id": "c2", "reset_type": "codex_rate_limits", "is_supported_by_plan": true, "status": "available",
     "granted_at": "2026-09-22T20:43:24.652893Z", "expires_at": "2026-10-22T20:43:24.652893Z",
     "redeem_started_at": null, "redeemed_at": null, "title": "Full reset (Weekly + 5 hr)"}
  ],
  "available_count": 2,
  "total_earned_count": 0,
  "immediate_reset_purchase_eligible": false,
  "history_enabled": true
}`

func TestParseResetCreditsReadsTheLivePayloadSoonestFirst(t *testing.T) {
	got, err := ParseResetCredits([]byte(liveResetCredits))
	if err != nil {
		t.Fatalf("live payload rejected: %v", err)
	}
	want := []time.Time{
		time.Date(2026, 10, 22, 20, 43, 24, 652893000, time.UTC),
		time.Date(2026, 10, 29, 18, 21, 24, 313601000, time.UTC),
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if !got[i].Equal(want[i]) {
			t.Fatalf("credit %d expires %v, want %v", i, got[i], want[i])
		}
	}
}

func TestParseResetCreditsAcceptsAnAccountWithNone(t *testing.T) {
	got, err := ParseResetCredits([]byte(`{"credits": [], "available_count": 0}`))
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v", got, err)
	}
}

// Only a credit that can still be redeemed against these limits is extra quota.
func TestParseResetCreditsSkipsCreditsThatCannotBeRedeemed(t *testing.T) {
	cases := map[string]string{
		"redeemed":            `{"reset_type": "codex_rate_limits", "is_supported_by_plan": true, "status": "redeemed", "expires_at": "2026-10-22T20:43:24Z"}`,
		"unsupported by plan": `{"reset_type": "codex_rate_limits", "is_supported_by_plan": false, "status": "available", "expires_at": "2026-10-22T20:43:24Z"}`,
		"other limit":         `{"reset_type": "code_review", "is_supported_by_plan": true, "status": "available", "expires_at": "2026-10-22T20:43:24Z"}`,
	}
	for name, credit := range cases {
		got, err := ParseResetCredits([]byte(`{"credits": [` + credit + `]}`))
		if err != nil || len(got) != 0 {
			t.Errorf("%s: got %v, %v", name, got, err)
		}
	}
}

// A redeemable credit that cannot be dated is a payload this parser does not
// understand. Dropping just that credit would hand back a shorter list that
// replaces the last trusted one and reprices the account.
func TestParseResetCreditsRejectsAnUndatedRedeemableCredit(t *testing.T) {
	base := `{"reset_type": "codex_rate_limits", "is_supported_by_plan": true, "status": "available"`
	valid := base + `, "expires_at": "2026-10-22T20:43:24Z"}`
	for name, bad := range map[string]string{
		"no expiry":          base + `}`,
		"null expiry":        base + `, "expires_at": null}`,
		"unparseable expiry": base + `, "expires_at": "soon"}`,
	} {
		for _, credits := range []string{bad, valid + "," + bad} {
			if got, err := ParseResetCredits([]byte(`{"credits": [` + credits + `]}`)); err == nil {
				t.Errorf("%s: accepted as %v", name, got)
			}
		}
	}
}

// The error reaches the status route, which must not echo payload fields.
func TestParseResetCreditsDoesNotEchoTheExpiry(t *testing.T) {
	raw := `{"credits": [{"reset_type": "codex_rate_limits", "is_supported_by_plan": true, "status": "available", "expires_at": "payload-text"}]}`
	if _, err := ParseResetCredits([]byte(raw)); err == nil || strings.Contains(err.Error(), "payload-text") {
		t.Fatalf("got %v, want an error that does not echo the payload", err)
	}
}

// An unrecognised payload must fail rather than read as "no credits": the
// caller keeps the last list it trusted on error, and an empty list would
// silently drop every account's credits.
func TestParseResetCreditsRejectsAnUnrecognisedPayload(t *testing.T) {
	for _, raw := range []string{``, `not json`, `{}`, `{"credits": null}`, `{"available_count": 3}`, `[]`} {
		if got, err := ParseResetCredits([]byte(raw)); err == nil {
			t.Errorf("%q: accepted as %v", raw, got)
		}
	}
}
