package codex

import (
	"fmt"
	"math"
	"testing"
	"time"
)

var at = time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)

func parse(t *testing.T, body string) Usage {
	t.Helper()
	u, err := ParseUsage([]byte(body), "acct", 7, at)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return u
}

func near(t *testing.T, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("got %g, want %g", got, want)
	}
}

const full = `{
  "plan_type": "pro",
  "rate_limits": {
    "allowed": true,
    "limit_reached": false,
    "primary":   {"used_percent": 12.5, "window_minutes": 300,   "reset_after_seconds": 4200},
    "secondary": {"used_percent": 40,   "window_minutes": 10080, "reset_after_seconds": 300000}
  }
}`

func TestParseFullPayload(t *testing.T) {
	u := parse(t, full)
	if u.Snapshot.AuthID != "acct" || u.Snapshot.Generation != 7 || !u.Snapshot.ObservedAt.Equal(at) {
		t.Fatalf("snapshot identity wrong: %+v", u.Snapshot)
	}
	if u.PlanType != "pro" || u.LimitReached {
		t.Fatalf("got plan %q limitReached=%v", u.PlanType, u.LimitReached)
	}
	// secondary is the 7-day window, so it must land in Weekly.
	near(t, u.Snapshot.Weekly.Remaining, 60)
	if !u.Snapshot.Weekly.ResetAt.Equal(at.Add(300000 * time.Second)) {
		t.Fatalf("weekly reset %v", u.Snapshot.Weekly.ResetAt)
	}
	near(t, u.Snapshot.FiveHour.Remaining, 87.5)
	if !u.Snapshot.FiveHour.ResetAt.Equal(at.Add(4200 * time.Second)) {
		t.Fatalf("5h reset %v", u.Snapshot.FiveHour.ResetAt)
	}
	if u.Snapshot.Weekly.Absolute || u.Snapshot.FiveHour.Absolute {
		t.Fatal("used_percent carries no capacity; Absolute must stay false")
	}
}

func TestParseClassifiesByWindowMinutesNotKeyName(t *testing.T) {
	// Nothing guarantees primary is the short window; only the duration does.
	u := parse(t, `{"rate_limits":{
	  "primary":   {"used_percent": 40, "window_minutes": 10080, "reset_after_seconds": 300000},
	  "secondary": {"used_percent": 10, "window_minutes": 300,   "reset_after_seconds": 600}
	}}`)
	near(t, u.Snapshot.Weekly.Remaining, 60)
	near(t, u.Snapshot.FiveHour.Remaining, 90)
}

func TestParseCamelCaseKeys(t *testing.T) {
	u := parse(t, `{"planType":"plus","rateLimit":{"limitReached":true,
	  "primary":{"usedPercent":25,"windowMinutes":300,"resetAfterSeconds":900},
	  "secondary":{"usedPercent":50,"windowMinutes":10080,"resetAfterSeconds":100000}}}`)
	if u.PlanType != "plus" || !u.LimitReached {
		t.Fatalf("got plan %q limitReached=%v", u.PlanType, u.LimitReached)
	}
	near(t, u.Snapshot.Weekly.Remaining, 50)
	near(t, u.Snapshot.FiveHour.Remaining, 75)
}

func TestParseResetAtFallback(t *testing.T) {
	resetAt := at.Add(2 * time.Hour).Unix()
	u := parse(t, fmt.Sprintf(`{"rate_limits":{
	  "secondary":{"used_percent":10,"window_minutes":10080,"reset_at":%d}}}`, resetAt))
	if !u.Snapshot.Weekly.ResetAt.Equal(time.Unix(resetAt, 0).UTC()) {
		t.Fatalf("got %v", u.Snapshot.Weekly.ResetAt)
	}
}

func TestParsePrefersRelativeResetOverAbsolute(t *testing.T) {
	// reset_after_seconds is immune to clock skew between us and upstream.
	u := parse(t, fmt.Sprintf(`{"rate_limits":{"secondary":{
	  "used_percent":10,"window_minutes":10080,
	  "reset_after_seconds":3600,"reset_at":%d}}}`, at.Add(99*time.Hour).Unix()))
	if !u.Snapshot.Weekly.ResetAt.Equal(at.Add(time.Hour)) {
		t.Fatalf("got %v, want the relative reset", u.Snapshot.Weekly.ResetAt)
	}
}

func TestParseExhaustedWindow(t *testing.T) {
	u := parse(t, `{"rate_limits":{
	  "primary":{"used_percent":100,"window_minutes":300,"reset_after_seconds":1200},
	  "secondary":{"used_percent":30,"window_minutes":10080,"reset_after_seconds":200000}}}`)
	near(t, u.Snapshot.FiveHour.Remaining, 0)
	near(t, u.Snapshot.Weekly.Remaining, 70)
}

func TestParseRejectsMalformedWindows(t *testing.T) {
	for name, body := range map[string]string{
		"used percent above 100": `{"rate_limits":{"secondary":{"used_percent":101,"window_minutes":10080,"reset_after_seconds":10}}}`,
		"negative used percent":  `{"rate_limits":{"secondary":{"used_percent":-1,"window_minutes":10080,"reset_after_seconds":10}}}`,
		"zero window minutes":    `{"rate_limits":{"secondary":{"used_percent":10,"window_minutes":0,"reset_after_seconds":10}}}`,
		"no reset field":         `{"rate_limits":{"secondary":{"used_percent":10,"window_minutes":10080}}}`,
		"no windows":             `{"rate_limits":{"allowed":true}}`,
		"no rate limits":         `{"plan_type":"pro"}`,
		"not json":               `nope`,
	} {
		if _, err := ParseUsage([]byte(body), "acct", 1, at); err == nil {
			t.Fatalf("%s: want an error", name)
		}
	}
}

func TestParseSingleWindowLeavesShortWindowInert(t *testing.T) {
	// One window only: it is the weekly budget, and the absent short window
	// must not be able to block the account.
	u := parse(t, `{"rate_limits":{"secondary":{"used_percent":10,"window_minutes":10080,"reset_after_seconds":10}}}`)
	near(t, u.Snapshot.Weekly.Remaining, 90)
	if !u.Snapshot.FiveHour.Expired(at) {
		t.Fatal("an absent short window must read as expired, never as exhausted")
	}
}

func TestAbsurdResetIsRejectedNotOverflowed(t *testing.T) {
	// time.Duration(n) * time.Second overflows int64 well before n does, so a
	// huge reset_after_seconds wraps to a negative duration and the window
	// reads as already expired.
	for _, body := range []string{
		`{"rate_limits":{"primary":{"used_percent":10,"window_minutes":10080,"reset_after_seconds":9223372036854775807}}}`,
		`{"rate_limits":{"primary":{"used_percent":10,"window_minutes":10080,"reset_after_seconds":1000000000000}}}`,
		`{"rate_limits":{"primary":{"used_percent":10,"window_minutes":10080,"reset_at":9223372036854775807}}}`,
	} {
		if _, err := ParseUsage([]byte(body), "a", 1, at); err == nil {
			t.Fatalf("accepted an unrepresentable reset: %s", body)
		}
	}
}
