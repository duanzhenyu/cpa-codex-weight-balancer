package codex

import (
	"fmt"
	"net/http"
	"testing"
)

// A Codex window only starts counting on the account's first metered request.
// Until then the account reports a reset exactly one window away, and keeps
// reporting that forever — the value slides with wall-clock instead of
// counting down. Both fixtures below are the live readings taken from this
// fleet's dormant credential on 2026-09-12T09:44Z, before and after the
// request that started it.

func dormantBody(usedPercent, resetAfter int) []byte {
	return []byte(fmt.Sprintf(`{"rate_limit":{
	  "primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_after_seconds":18000},
	  "secondary_window":{"used_percent":%d,"limit_window_seconds":604800,"reset_after_seconds":%d}}}`,
		usedPercent, resetAfter))
}

func dormantHeaders(usedPercent, resetAfter string) http.Header {
	return http.Header{
		"X-Codex-Plan-Type":                     {"team"},
		"X-Codex-Primary-Used-Percent":          {"0"},
		"X-Codex-Primary-Window-Minutes":        {"300"},
		"X-Codex-Primary-Reset-After-Seconds":   {"18000"},
		"X-Codex-Secondary-Used-Percent":        {usedPercent},
		"X-Codex-Secondary-Window-Minutes":      {"10080"},
		"X-Codex-Secondary-Reset-After-Seconds": {resetAfter},
	}
}

func TestDormantWhenWeeklyWindowHasNotStarted(t *testing.T) {
	u, err := ParseUsage(dormantBody(0, 604800), "acct", 1, at)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !u.Dormant {
		t.Fatal("reset_after == limit_window_seconds at 0% used is a window that never started")
	}
}

func TestNotDormantOnceTheWeeklyClockHasMoved(t *testing.T) {
	// 12 seconds after the kickstart request, measured live.
	u, err := ParseUsage(dormantBody(0, 604788), "acct", 1, at)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if u.Dormant {
		t.Fatal("a window 12s into its span is running, not dormant")
	}
}

// Dormancy is a fact about the weekly budget. The short window resets on its
// own every five hours, so its state says nothing about whether the account's
// week has begun.
func TestDormancyReadsTheWeeklyWindowNotTheShortOne(t *testing.T) {
	running := []byte(`{"rate_limit":{
	  "primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_after_seconds":18000},
	  "secondary_window":{"used_percent":3,"limit_window_seconds":604800,"reset_after_seconds":604177}}}`)
	u, err := ParseUsage(running, "acct", 1, at)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if u.Dormant {
		t.Fatal("a fresh 5h window on a running weekly budget is not dormancy")
	}
}

func TestDormancyNeedsAnUntouchedWindow(t *testing.T) {
	u, err := ParseUsage(dormantBody(3, 604800), "acct", 1, at)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if u.Dormant {
		t.Fatal("quota already spent means the window is running whatever the reset says")
	}
}

// reset_at cannot stand in for reset_after here. It is an absolute instant, so
// deciding dormancy from it would compare our clock against Codex's and call
// any account with a week to go dormant.
func TestDormancyIsNotInferredFromAbsoluteResetAlone(t *testing.T) {
	body := fmt.Sprintf(`{"rate_limit":{
	  "secondary_window":{"used_percent":0,"limit_window_seconds":604800,"reset_at":%d}}}`,
		at.Add(604800e9).Unix())
	u, err := ParseUsage([]byte(body), "acct", 1, at)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if u.Dormant {
		t.Fatal("without reset_after there is no evidence the window never started")
	}
}

func TestDormancyFromResponseHeaders(t *testing.T) {
	// Verbatim from the 200 the kickstart request itself returned: Codex
	// reports the pre-request state, so a served response can still read dormant.
	u, err := ParseHeaders(dormantHeaders("0", "604800"), "acct", 1, at)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !u.Dormant {
		t.Fatal("headers carry the same window; dormancy must be detected there too")
	}
}

func TestHeaderAndBodyTransportsAgreeOnDormancy(t *testing.T) {
	for _, tc := range []struct {
		name       string
		used, left int
		want       bool
	}{
		{"untouched", 0, 604800, true},
		{"started", 0, 604788, false},
		{"spent", 3, 604177, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, errBody := ParseUsage(dormantBody(tc.used, tc.left), "acct", 1, at)
			if errBody != nil {
				t.Fatalf("body: %v", errBody)
			}
			headers, errHeaders := ParseHeaders(
				dormantHeaders(fmt.Sprint(tc.used), fmt.Sprint(tc.left)), "acct", 1, at)
			if errHeaders != nil {
				t.Fatalf("headers: %v", errHeaders)
			}
			if body.Dormant != tc.want || headers.Dormant != tc.want {
				t.Fatalf("want %v, body=%v headers=%v", tc.want, body.Dormant, headers.Dormant)
			}
		})
	}
}

// Only the weekly budget can justify a paid kickstart. When the weekly window
// is missing, fold still promotes the short one so the account can be priced —
// but a five-hour window that restarts on its own every five hours is dormant
// half the time, and paying to start it would recur forever.
func TestALoneShortWindowIsNeverDormant(t *testing.T) {
	body := []byte(`{"rate_limit":{
	  "primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_after_seconds":18000}}}`)
	u, err := ParseUsage(body, "acct", 1, at)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if u.Snapshot.Weekly.Remaining != 100 {
		t.Fatalf("the lone window must still price the account: %v", u.Snapshot.Weekly)
	}
	if u.Dormant {
		t.Fatal("a five-hour window cannot justify spending an inference request")
	}
}
