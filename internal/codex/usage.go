// Package codex parses Codex rate-limit payloads into quota snapshots.
//
// The shape below was verified against a live /wham/usage response, not
// extrapolated: the object is rate_limit (singular), its windows are
// primary_window / secondary_window, and each window sizes itself with
// limit_window_seconds. There is no plan capacity anywhere in the payload, so
// remaining quota is only ever a percentage of that one window.
//
// additional_rate_limits carries per-model sub-limits in the same shape and is
// deliberately ignored: folding a per-model budget into the account-wide one
// would double-count it.
package codex

import (
	"cmp"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/duanzhenyu/cpa-codex-weight-balancer/internal/quota"
)

// Usage is one parsed rate-limit payload.
type Usage struct {
	Snapshot quota.Snapshot
	// LimitReached mirrors the upstream flag: the account is rate limited right
	// now regardless of what the window percentages say.
	LimitReached bool
	Dormant      Dormancy
	PlanType     string
}

// Dormancy reports which of an account's budgets have never started. Codex
// begins a window at the account's first metered request, and until then
// reports a reset exactly one window away — a value that slides with
// wall-clock instead of counting down.
//
// The two budgets are reported separately because they cost differently to
// start. A weekly clock is started once and then runs for a week; a short one
// expires every few hours and reads untouched again each time the account falls
// idle, so paying to start it recurs for as long as the account stays quiet.
type Dormancy struct {
	// Weekly is the budget the score is computed from: while it is dormant the
	// account is priced forever as though it had a whole week left to spend.
	Weekly bool
	// Short is the availability gate only — it never moves a weight. Starting
	// it early moves its reset earlier relative to a later burst, which shortens
	// the exclusion when the short budget is spent.
	Short bool
}

// Any reports whether either budget reads dormant, which is the one decision
// both flags share: Codex stamps its quota headers before metering the request
// that carries them, so any reading that calls a window dormant describes the
// account as it was before that request and is already stale.
func (d Dormancy) Any() bool { return d.Weekly || d.Short }

// ParseUsage reads a Codex rate-limit payload. Windows are classified by
// limit_window_seconds rather than by the primary/secondary key, because only the
// duration says which one is the weekly budget.
func ParseUsage(raw []byte, authID string, generation uint64, observedAt time.Time) (Usage, error) {
	var doc usageDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return Usage{}, fmt.Errorf("decode codex usage: %w", err)
	}
	limits := pick(doc.RateLimit, doc.RateLimitCamel)
	if limits == nil {
		return Usage{}, fmt.Errorf("codex usage carries no rate_limit object")
	}

	snapshot, dormant, err := fold(
		pick(limits.Primary, limits.PrimaryCamel),
		pick(limits.Secondary, limits.SecondaryCamel),
		authID, generation, observedAt,
	)
	if err != nil {
		return Usage{}, err
	}
	return Usage{
		Snapshot:     snapshot,
		LimitReached: limits.LimitReached || limits.LimitReachedCamel,
		Dormant:      dormant,
		PlanType:     first(doc.PlanType, doc.PlanTypeCamel),
	}, nil
}

// fold classifies the account-wide windows by length and folds them into a
// snapshot. Both transports — the probe body and the response headers — carry
// the same two windows under the same rules, so the validation lives here once.
//
// It also reports which budgets are dormant. Each end of the sorted list is
// asked about its own budget, so a window only ever speaks for the one its
// length identifies.
func fold(primary, secondary *windowDoc, authID string, generation uint64, observedAt time.Time) (quota.Snapshot, Dormancy, error) {
	var windows []sizedWindow
	for _, w := range []*windowDoc{primary, secondary} {
		if parsed, ok := w.window(observedAt); ok {
			windows = append(windows, parsed)
		}
	}
	if len(windows) == 0 {
		return quota.Snapshot{}, Dormancy{}, fmt.Errorf("codex quota carries no usable rate-limit window")
	}
	slices.SortFunc(windows, func(a, b sizedWindow) int { return cmp.Compare(a.span, b.span) })

	weekly := windows[len(windows)-1]
	s := quota.Snapshot{
		AuthID:     authID,
		Generation: generation,
		ObservedAt: observedAt,
		Weekly:     weekly.window,
	}
	// With a single window there is nothing shorter to gate on, and the zero
	// value reads as Expired — never as exhausted — so it cannot block.
	if len(windows) > 1 {
		s.FiveHour = windows[0].window
	}
	// fold promotes a lone window into the weekly slot so the account can still
	// be priced, so the slot does not say which budget the window is — only its
	// length does. Asking each end about its own side keeps a lone five-hour
	// window from being reported as a dormant week, and vice versa.
	shortest := windows[0]
	// The sort is not stable, so two windows of one length put an arbitrary one
	// at each end and read an arbitrary dormancy off it. Neither budget is
	// identified then, and a coin flip must not spend a request.
	named := len(windows) == 1 || shortest.span != weekly.span
	return s, Dormancy{
		Weekly: named && weekly.dormant && weekly.span >= minDormantSpan,
		Short:  named && shortest.dormant && shortest.span < minDormantSpan,
	}, nil
}

// minDormantSpan divides the two budgets. Codex's windows are five hours and a
// week, so anything in between resolves the same way.
const minDormantSpan = 24 * 60 * 60

type usageDoc struct {
	RateLimit      *limitsDoc `json:"rate_limit"`
	RateLimitCamel *limitsDoc `json:"rateLimit"`
	PlanType       string     `json:"plan_type"`
	PlanTypeCamel  string     `json:"planType"`
}

type limitsDoc struct {
	LimitReached      bool       `json:"limit_reached"`
	LimitReachedCamel bool       `json:"limitReached"`
	Primary           *windowDoc `json:"primary_window"`
	PrimaryCamel      *windowDoc `json:"primaryWindow"`
	Secondary         *windowDoc `json:"secondary_window"`
	SecondaryCamel    *windowDoc `json:"secondaryWindow"`
}

type windowDoc struct {
	UsedPercent      *float64 `json:"used_percent"`
	UsedPercentCamel *float64 `json:"usedPercent"`
	Span             *int64   `json:"limit_window_seconds"`
	SpanCamel        *int64   `json:"limitWindowSeconds"`
	ResetAfter       *int64   `json:"reset_after_seconds"`
	ResetAfterCamel  *int64   `json:"resetAfterSeconds"`
	ResetAt          *int64   `json:"reset_at"`
	ResetAtCamel     *int64   `json:"resetAt"`
}

// maxResetSeconds bounds a reset before it is turned into a duration. The
// multiplication by time.Second overflows int64 long before the field does,
// wrapping a far-future reset into a negative one that reads as already
// expired. No Codex window is longer than a week; 400 days is slack, not a
// semantic limit.
const maxResetSeconds = 400 * 24 * 60 * 60

// sizedWindow pairs a parsed window with its length in seconds, which is the
// only thing that says whether it is the weekly budget or the short one.
type sizedWindow struct {
	window quota.Window
	span   int64
	// dormant reports a window that has never started: nothing spent, and a
	// reset exactly one span away.
	dormant bool
}

// window validates one rate-limit window: a usable one needs a percentage in
// [0,100], a positive length, and some reset.
func (w *windowDoc) window(observedAt time.Time) (sizedWindow, bool) {
	if w == nil {
		return sizedWindow{}, false
	}
	used, span := pick(w.UsedPercent, w.UsedPercentCamel), pick(w.Span, w.SpanCamel)
	// The span is bounded by the same rule as the reset it is compared against:
	// a nonsense length is not merely useless, it sorts itself into the weekly
	// end and prices the account off the wrong window.
	if used == nil || span == nil || *used < 0 || *used > 100 || *span <= 0 || *span > maxResetSeconds {
		return sizedWindow{}, false
	}
	// A relative reset is immune to clock skew between us and upstream, so it
	// wins whenever both forms are present.
	var resetAt time.Time
	switch after, absolute := pick(w.ResetAfter, w.ResetAfterCamel), pick(w.ResetAt, w.ResetAtCamel); {
	case after != nil && *after >= 0 && *after <= maxResetSeconds:
		resetAt = observedAt.Add(time.Duration(*after) * time.Second)
	case absolute != nil && *absolute > 0 && *absolute-observedAt.Unix() <= maxResetSeconds:
		resetAt = time.Unix(*absolute, 0).UTC()
	default:
		return sizedWindow{}, false
	}
	// Dormancy is read from the relative reset only. reset_at is an absolute
	// instant, so measuring the span against it would compare our clock with
	// Codex's and call any account with a full week left dormant.
	after := pick(w.ResetAfter, w.ResetAfterCamel)
	dormant := *used == 0 && after != nil && *after == *span
	return sizedWindow{quota.Window{Remaining: 100 - *used, ResetAt: resetAt}, *span, dormant}, true
}

func pick[T any](a, b *T) *T {
	if a != nil {
		return a
	}
	return b
}

func first(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
