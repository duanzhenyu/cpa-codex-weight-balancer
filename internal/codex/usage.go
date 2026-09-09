// Package codex parses Codex rate-limit payloads into quota snapshots.
//
// The upstream shape is fixed by CPA's own parser
// (internal/runtime/executor/helps/codex_quota.go): each window reports
// used_percent, window_minutes and one of reset_after_seconds / reset_at.
// There is no plan capacity anywhere in the payload, so remaining quota is only
// ever a percentage of that one window.
package codex

import (
	"cmp"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"gitlab.duanzhenyu.top/dzy/cpa-codex-weight-balancer/internal/quota"
)

// Usage is one parsed rate-limit payload.
type Usage struct {
	Snapshot quota.Snapshot
	// LimitReached mirrors the upstream flag: the account is rate limited right
	// now regardless of what the window percentages say.
	LimitReached bool
	PlanType     string
}

// ParseUsage reads a Codex rate-limit payload. Windows are classified by
// window_minutes rather than by the primary/secondary key, because only the
// duration says which one is the weekly budget.
func ParseUsage(raw []byte, authID string, generation uint64, observedAt time.Time) (Usage, error) {
	var doc usageDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return Usage{}, fmt.Errorf("decode codex usage: %w", err)
	}
	limits := pick(doc.RateLimits, doc.RateLimitsCamel)
	if limits == nil {
		return Usage{}, fmt.Errorf("codex usage carries no rate_limits object")
	}

	var windows []sizedWindow
	for _, w := range []*windowDoc{limits.Primary, limits.Secondary} {
		if parsed, ok := w.window(observedAt); ok {
			windows = append(windows, parsed)
		}
	}
	if len(windows) == 0 {
		return Usage{}, fmt.Errorf("codex usage carries no usable rate-limit window")
	}
	slices.SortFunc(windows, func(a, b sizedWindow) int { return cmp.Compare(a.minutes, b.minutes) })

	u := Usage{
		Snapshot: quota.Snapshot{
			AuthID:     authID,
			Generation: generation,
			ObservedAt: observedAt,
			Weekly:     windows[len(windows)-1].window,
		},
		LimitReached: limits.LimitReached || limits.LimitReachedCamel,
		PlanType:     first(doc.PlanType, doc.PlanTypeCamel),
	}
	// With a single window there is nothing shorter to gate on, and the zero
	// value reads as Expired — never as exhausted — so it cannot block.
	if len(windows) > 1 {
		u.Snapshot.FiveHour = windows[0].window
	}
	return u, nil
}

type usageDoc struct {
	RateLimits      *limitsDoc `json:"rate_limits"`
	RateLimitsCamel *limitsDoc `json:"rateLimit"`
	PlanType        string     `json:"plan_type"`
	PlanTypeCamel   string     `json:"planType"`
}

type limitsDoc struct {
	LimitReached      bool       `json:"limit_reached"`
	LimitReachedCamel bool       `json:"limitReached"`
	Primary           *windowDoc `json:"primary"`
	Secondary         *windowDoc `json:"secondary"`
}

type windowDoc struct {
	UsedPercent      *float64 `json:"used_percent"`
	UsedPercentCamel *float64 `json:"usedPercent"`
	Minutes          *int64   `json:"window_minutes"`
	MinutesCamel     *int64   `json:"windowMinutes"`
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

// sizedWindow pairs a parsed window with its duration, which is the only thing
// that says whether it is the weekly budget or the short one.
type sizedWindow struct {
	window  quota.Window
	minutes int64
}

// window validates one rate-limit window the same way the host does: a usable
// window needs a percentage in [0,100], a positive duration, and some reset.
func (w *windowDoc) window(observedAt time.Time) (sizedWindow, bool) {
	if w == nil {
		return sizedWindow{}, false
	}
	used, minutes := pick(w.UsedPercent, w.UsedPercentCamel), pick(w.Minutes, w.MinutesCamel)
	if used == nil || minutes == nil || *used < 0 || *used > 100 || *minutes <= 0 {
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
	return sizedWindow{quota.Window{Remaining: 100 - *used, ResetAt: resetAt}, *minutes}, true
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
