package codex

import (
	"net/http"
	"testing"
	"time"
)

// live is the header set observed on this fleet's CPA at 2026-09-12T13:15+08:00,
// copied verbatim from GET /v0/management/auth-files.
func live() http.Header {
	return http.Header{
		"X-Codex-Active-Limit":                         {"premium"},
		"X-Codex-Credits-Balance":                      {"0"},
		"X-Codex-Credits-Has-Credits":                  {"false"},
		"X-Codex-Plan-Type":                            {"plus"},
		"X-Codex-Primary-Over-Secondary-Limit-Percent": {"0"},
		"X-Codex-Primary-Reset-After-Seconds":          {"11112"},
		"X-Codex-Primary-Reset-At":                     {"1789201212"},
		"X-Codex-Primary-Used-Percent":                 {"30"},
		"X-Codex-Primary-Window-Minutes":               {"300"},
		"X-Codex-Secondary-Reset-After-Seconds":        {"256112"},
		"X-Codex-Secondary-Reset-At":                   {"1789446211"},
		"X-Codex-Secondary-Used-Percent":               {"44"},
		"X-Codex-Secondary-Window-Minutes":             {"10080"},
	}
}

func parseHeaders(t *testing.T, h http.Header) Usage {
	t.Helper()
	u, err := ParseHeaders(h, "acct", 7, at)
	if err != nil {
		t.Fatalf("parse headers: %v", err)
	}
	return u
}

func TestParseHeadersReadsTheLiveObservedSet(t *testing.T) {
	u := parseHeaders(t, live())
	if u.Snapshot.AuthID != "acct" || u.Snapshot.Generation != 7 || !u.Snapshot.ObservedAt.Equal(at) {
		t.Fatalf("snapshot identity wrong: %+v", u.Snapshot)
	}
	if u.PlanType != "plus" || u.LimitReached {
		t.Fatalf("got plan %q limitReached=%v", u.PlanType, u.LimitReached)
	}
	// secondary is 10080 minutes, so it is the weekly budget.
	near(t, u.Snapshot.Weekly.Remaining, 56)
	if !u.Snapshot.Weekly.ResetAt.Equal(at.Add(256112 * time.Second)) {
		t.Fatalf("weekly reset %v", u.Snapshot.Weekly.ResetAt)
	}
	near(t, u.Snapshot.FiveHour.Remaining, 70)
	if !u.Snapshot.FiveHour.ResetAt.Equal(at.Add(11112 * time.Second)) {
		t.Fatalf("5h reset %v", u.Snapshot.FiveHour.ResetAt)
	}
	if u.Snapshot.Weekly.Absolute || u.Snapshot.FiveHour.Absolute {
		t.Fatal("used_percent carries no capacity; Absolute must stay false")
	}
}

func TestParseHeadersClassifiesByWindowLengthNotKeyName(t *testing.T) {
	h := http.Header{
		"X-Codex-Primary-Used-Percent":          {"40"},
		"X-Codex-Primary-Window-Minutes":        {"10080"},
		"X-Codex-Primary-Reset-After-Seconds":   {"300000"},
		"X-Codex-Secondary-Used-Percent":        {"10"},
		"X-Codex-Secondary-Window-Minutes":      {"300"},
		"X-Codex-Secondary-Reset-After-Seconds": {"600"},
	}
	u := parseHeaders(t, h)
	near(t, u.Snapshot.Weekly.Remaining, 60)
	near(t, u.Snapshot.FiveHour.Remaining, 90)
}

func TestParseHeadersPrefersRelativeResetOverAbsolute(t *testing.T) {
	u := parseHeaders(t, live())
	// Reset-At on this fixture is a different instant than Reset-After; the
	// relative one is immune to clock skew and must win.
	if !u.Snapshot.FiveHour.ResetAt.Equal(at.Add(11112 * time.Second)) {
		t.Fatalf("5h reset %v", u.Snapshot.FiveHour.ResetAt)
	}
}

func TestParseHeadersFallsBackToResetAt(t *testing.T) {
	h := live()
	h.Del("X-Codex-Primary-Reset-After-Seconds")
	h.Del("X-Codex-Secondary-Reset-After-Seconds")
	u := parseHeaders(t, h)
	if !u.Snapshot.FiveHour.ResetAt.Equal(time.Unix(1789201212, 0).UTC()) {
		t.Fatalf("5h reset %v", u.Snapshot.FiveHour.ResetAt)
	}
	if !u.Snapshot.Weekly.ResetAt.Equal(time.Unix(1789446211, 0).UTC()) {
		t.Fatalf("weekly reset %v", u.Snapshot.Weekly.ResetAt)
	}
}

func TestParseHeadersReportsLimitReached(t *testing.T) {
	h := live()
	h.Set("X-Codex-Limit-Reached", "true")
	if !parseHeaders(t, h).LimitReached {
		t.Fatal("want limit reached")
	}
	// Absence is not exhaustion: a response without the flag must not blocked
	// the account.
	if parseHeaders(t, live()).LimitReached {
		t.Fatal("absent flag must read as false")
	}
}

func TestParseHeadersRejectsAQuotaFreeResponse(t *testing.T) {
	for name, h := range map[string]http.Header{
		"empty":          {},
		"unrelated only": {"Content-Type": {"application/json"}, "X-Request-Id": {"abc"}},
		"no window length": {
			"X-Codex-Primary-Used-Percent":        {"30"},
			"X-Codex-Primary-Reset-After-Seconds": {"11112"},
		},
		"no reset": {
			"X-Codex-Primary-Used-Percent":   {"30"},
			"X-Codex-Primary-Window-Minutes": {"300"},
		},
		"percent out of range": {
			"X-Codex-Primary-Used-Percent":        {"101"},
			"X-Codex-Primary-Window-Minutes":      {"300"},
			"X-Codex-Primary-Reset-After-Seconds": {"11112"},
		},
		"unparsable percent": {
			"X-Codex-Primary-Used-Percent":        {"n/a"},
			"X-Codex-Primary-Window-Minutes":      {"300"},
			"X-Codex-Primary-Reset-After-Seconds": {"11112"},
		},
	} {
		if _, err := ParseHeaders(h, "acct", 1, at); err == nil {
			t.Fatalf("%s: want error", name)
		}
	}
}

func TestParseHeadersRejectsAnAbsurdWindowLength(t *testing.T) {
	// Minutes are multiplied by 60 before they are compared, and that overflows
	// int64 long before the header field does. This value wraps to a positive
	// 4 seconds, so a missing bound would not be caught by the existing
	// non-positive check — it would sort the weekly budget into the short
	// window and gate the account on it.
	h := live()
	h.Set("X-Codex-Secondary-Window-Minutes", "3381903080180084463")
	u := parseHeaders(t, h)
	near(t, u.Snapshot.Weekly.Remaining, 70)
	if u.Snapshot.FiveHour.Usable(at) {
		t.Fatal("the rejected window must leave nothing behind to gate on")
	}
}

func TestParseHeadersSingleWindowLeavesShortWindowInert(t *testing.T) {
	h := http.Header{
		"X-Codex-Secondary-Used-Percent":        {"44"},
		"X-Codex-Secondary-Window-Minutes":      {"10080"},
		"X-Codex-Secondary-Reset-After-Seconds": {"256112"},
	}
	u := parseHeaders(t, h)
	near(t, u.Snapshot.Weekly.Remaining, 56)
	if u.Snapshot.FiveHour.Usable(at) {
		t.Fatal("a missing short window must not gate anything")
	}
}

func TestParseHeadersIgnoresPerLimitVariants(t *testing.T) {
	// The websocket path emits x-codex-additional-<limit>-* and code-review
	// windows. Folding a per-model budget into the account-wide one would
	// double-count it, exactly as additional_rate_limits is ignored in the body.
	h := live()
	h.Set("X-Codex-Additional-Gpt5-Primary-Used-Percent", "99")
	h.Set("X-Codex-Additional-Gpt5-Primary-Window-Minutes", "300")
	h.Set("X-Codex-Additional-Gpt5-Primary-Reset-After-Seconds", "60")
	h.Set("X-Codex-Code-Review-Primary-Used-Percent", "98")
	h.Set("X-Codex-Code-Review-Primary-Window-Minutes", "300")
	h.Set("X-Codex-Code-Review-Primary-Reset-After-Seconds", "60")
	u := parseHeaders(t, h)
	near(t, u.Snapshot.FiveHour.Remaining, 70)
	near(t, u.Snapshot.Weekly.Remaining, 56)
}
