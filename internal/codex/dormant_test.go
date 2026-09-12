package codex

import (
	"fmt"
	"net/http"
	"testing"
)

// A Codex window only starts counting on the account's first metered request.
// Until then the account reports a reset exactly one window away, and keeps
// reporting that forever — the value slides with wall-clock instead of
// counting down. The fixtures below are the live readings taken from this
// fleet's dormant credential on 2026-09-12T09:44Z, before and after the
// request that started it.
//
// One request starts both windows at the same instant: the same fleet's plus
// account read primary 7917/18000 and secondary 594717/604800 on 2026-09-12,
// both exactly 10083s into their spans. So the two budgets go dormant together
// on a fresh account, and only diverge once the short one has rolled over.

// windows builds a payload with both windows under explicit control.
func windows(shortUsed, shortLeft, weekUsed, weekLeft int) string {
	return fmt.Sprintf(`{"rate_limit":{
	  "primary_window":{"used_percent":%d,"limit_window_seconds":18000,"reset_after_seconds":%d},
	  "secondary_window":{"used_percent":%d,"limit_window_seconds":604800,"reset_after_seconds":%d}}}`,
		shortUsed, shortLeft, weekUsed, weekLeft)
}

// dormantBody holds the short window untouched and varies only the weekly one.
func dormantBody(usedPercent, resetAfter int) string {
	return windows(0, 18000, usedPercent, resetAfter)
}

func dormantHeaders(usedPercent, resetAfter string) http.Header {
	return headersFor("0", "18000", usedPercent, resetAfter)
}

func headersFor(shortUsed, shortLeft, weekUsed, weekLeft string) http.Header {
	return http.Header{
		"X-Codex-Plan-Type":                     {"team"},
		"X-Codex-Primary-Used-Percent":          {shortUsed},
		"X-Codex-Primary-Window-Minutes":        {"300"},
		"X-Codex-Primary-Reset-After-Seconds":   {shortLeft},
		"X-Codex-Secondary-Used-Percent":        {weekUsed},
		"X-Codex-Secondary-Window-Minutes":      {"10080"},
		"X-Codex-Secondary-Reset-After-Seconds": {weekLeft},
	}
}

func TestDormantWhenWeeklyWindowHasNotStarted(t *testing.T) {
	if !parse(t, dormantBody(0, 604800)).Dormant.Weekly {
		t.Fatal("reset_after == limit_window_seconds at 0% used is a window that never started")
	}
}

func TestNotDormantOnceTheWeeklyClockHasMoved(t *testing.T) {
	// 12 seconds after the kickstart request, measured live.
	if parse(t, dormantBody(0, 604788)).Dormant.Weekly {
		t.Fatal("a window 12s into its span is running, not dormant")
	}
}

// A fresh account has spent nothing on either budget, and one request starts
// both. Either switch must be able to see it.
func TestAFreshAccountIsDormantOnBothBudgets(t *testing.T) {
	got := parse(t, windows(0, 18000, 0, 604800)).Dormant
	if !got.Weekly || !got.Short {
		t.Fatalf("both budgets must read dormant on an untouched account: %+v", got)
	}
}

// The two budgets are reported separately because they cost differently: the
// weekly clock is started once and runs for a week, while the short one expires
// every few hours and reads untouched again each time the account falls idle.
func TestShortDormancyIsSeparateFromTheWeekly(t *testing.T) {
	got := parse(t, windows(0, 18000, 3, 604177)).Dormant
	if got.Weekly {
		t.Fatal("a fresh 5h window on a running weekly budget is not weekly dormancy")
	}
	if !got.Short {
		t.Fatal("the short window is untouched with a full span to go; that is short dormancy")
	}
}

func TestShortDormancyClearsOnceTheShortClockHasMoved(t *testing.T) {
	// 12s into the short window, the same instant the weekly one started.
	got := parse(t, windows(0, 17988, 0, 604788)).Dormant
	if got.Short || got.Weekly {
		t.Fatalf("both clocks are running: %+v", got)
	}
}

func TestShortDormancyNeedsAnUntouchedWindow(t *testing.T) {
	if parse(t, windows(4, 18000, 3, 604177)).Dormant.Short {
		t.Fatal("quota already spent means the window is running whatever the reset says")
	}
}

func TestDormancyNeedsAnUntouchedWindow(t *testing.T) {
	if parse(t, dormantBody(3, 604800)).Dormant.Weekly {
		t.Fatal("quota already spent means the window is running whatever the reset says")
	}
}

// reset_at cannot stand in for reset_after here. It is an absolute instant, so
// deciding dormancy from it would compare our clock against Codex's and call
// any account with a week to go dormant.
func TestDormancyIsNotInferredFromAbsoluteResetAlone(t *testing.T) {
	lone := fmt.Sprintf(`{"rate_limit":{
	  "secondary_window":{"used_percent":0,"limit_window_seconds":604800,"reset_at":%d}}}`,
		at.Add(604800e9).Unix())
	if parse(t, lone).Dormant.Weekly {
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
	if !u.Dormant.Weekly {
		t.Fatal("headers carry the same window; dormancy must be detected there too")
	}
}

func TestHeaderAndBodyTransportsAgreeOnDormancy(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		shortUsed, shortLeft  int
		weekUsed, weekLeft    int
		wantWeekly, wantShort bool
	}{
		{"untouched", 0, 18000, 0, 604800, true, true},
		{"started", 0, 17988, 0, 604788, false, false},
		{"weekly running, short rolled over", 0, 18000, 3, 604177, false, true},
		{"both spent", 4, 9000, 3, 604177, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fromBody, errBody := ParseUsage([]byte(windows(tc.shortUsed, tc.shortLeft, tc.weekUsed, tc.weekLeft)), "acct", 1, at)
			if errBody != nil {
				t.Fatalf("body: %v", errBody)
			}
			fromHeaders, errHeaders := ParseHeaders(headersFor(
				fmt.Sprint(tc.shortUsed), fmt.Sprint(tc.shortLeft),
				fmt.Sprint(tc.weekUsed), fmt.Sprint(tc.weekLeft)), "acct", 1, at)
			if errHeaders != nil {
				t.Fatalf("headers: %v", errHeaders)
			}
			want := Dormancy{Weekly: tc.wantWeekly, Short: tc.wantShort}
			if fromBody.Dormant != want || fromHeaders.Dormant != want {
				t.Fatalf("want %+v, body=%+v headers=%+v", want, fromBody.Dormant, fromHeaders.Dormant)
			}
		})
	}
}

// When one window is missing, fold promotes whatever is left so the account can
// still be priced. Its span is the only thing that says which budget it is, and
// a lone short window must not be mistaken for a dormant week.
func TestALoneWindowIsClassifiedByItsSpan(t *testing.T) {
	short := parse(t, `{"rate_limit":{
	  "primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_after_seconds":18000}}}`)
	if short.Snapshot.Weekly.Remaining != 100 {
		t.Fatalf("the lone window must still price the account: %v", short.Snapshot.Weekly)
	}
	if short.Dormant.Weekly || !short.Dormant.Short {
		t.Fatalf("a five-hour window is the short budget, whatever slot it fills: %+v", short.Dormant)
	}

	weekly := parse(t, `{"rate_limit":{
	  "secondary_window":{"used_percent":0,"limit_window_seconds":604800,"reset_after_seconds":604800}}}`)
	if !weekly.Dormant.Weekly || weekly.Dormant.Short {
		t.Fatalf("a lone weekly window says nothing about a short budget: %+v", weekly.Dormant)
	}
}

// slices.SortFunc is not stable, so when two windows report the same length
// which one lands at each end is arbitrary — and with it, whose dormancy is
// read. Nothing is worth spending a request on under that coin flip.
func TestEqualLengthWindowsIdentifyNoBudget(t *testing.T) {
	for _, span := range []int{18000, 604800} {
		raw := fmt.Sprintf(`{"rate_limit":{
		  "primary_window":{"used_percent":0,"limit_window_seconds":%d,"reset_after_seconds":%d},
		  "secondary_window":{"used_percent":30,"limit_window_seconds":%d,"reset_after_seconds":9000}}}`,
			span, span, span)
		if got := parse(t, raw).Dormant; got.Any() {
			t.Fatalf("span %d: two windows of one length name no budget: %+v", span, got)
		}
	}
}

// maxResetSeconds bounds the reset already; the span it is compared against has
// to be bounded by the same rule, or a nonsense length sorts itself into the
// weekly end and the account is priced off the wrong window. The header
// transport rejects this via maxWindowMinutes, and fold promises both
// transports share one definition of usable.
func TestAnImpossibleWindowLengthIsNotUsable(t *testing.T) {
	raw := `{"rate_limit":{
	  "secondary_window":{"used_percent":0,"limit_window_seconds":99999999999,"reset_after_seconds":604800}}}`
	if _, err := ParseUsage([]byte(raw), "acct", 1, at); err == nil {
		t.Fatal("a window longer than any reset we accept must not be usable")
	}
}

// Any drives the one decision both flags share: a reading that calls either
// budget dormant was stamped before its own request was metered, so it is
// always stale.
func TestAnyReportsEitherBudget(t *testing.T) {
	for _, tc := range []struct {
		d    Dormancy
		want bool
	}{
		{Dormancy{}, false},
		{Dormancy{Weekly: true}, true},
		{Dormancy{Short: true}, true},
		{Dormancy{Weekly: true, Short: true}, true},
	} {
		if got := tc.d.Any(); got != tc.want {
			t.Fatalf("%+v.Any() = %v", tc.d, got)
		}
	}
}
