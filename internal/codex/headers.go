package codex

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Codex reports the same two account-wide windows on every response it serves.
// The names below were captured from this fleet's live traffic on 2026-09-12;
// CPA retains them verbatim (sdk/cliproxy/auth/quota_signals.go) and then uses
// them for nothing but its management page.
const (
	headerPrefix    = "X-Codex-"
	headerPrimary   = headerPrefix + "Primary-"
	headerSecondary = headerPrefix + "Secondary-"
)

// maxWindowMinutes bounds a window length before it is converted to seconds.
// The multiplication overflows int64 far below the field's range, and a value
// that wraps to a small positive span passes every later check while sorting
// the weekly budget into the short window.
const maxWindowMinutes = maxResetSeconds / 60

// ParseHeaders reads an upstream Codex response's quota headers. It reports an
// error whenever the response carried no usable window, which is the normal
// case for anything that is not a served Codex call.
func ParseHeaders(h http.Header, authID string, generation uint64, observedAt time.Time) (Usage, error) {
	if h == nil {
		return Usage{}, fmt.Errorf("codex quota carries no usable rate-limit window")
	}
	snapshot, dormant, err := fold(
		headerWindow(h, headerPrimary),
		headerWindow(h, headerSecondary),
		authID, generation, observedAt,
	)
	if err != nil {
		return Usage{}, err
	}
	return Usage{
		Snapshot: snapshot,
		Dormant:  dormant,
		// A response that omits the flag says nothing about exhaustion, so an
		// unparsable or absent value must read as false, never as blocked.
		LimitReached: strings.EqualFold(strings.TrimSpace(h.Get(headerPrefix+"Limit-Reached")), "true"),
		PlanType:     strings.TrimSpace(h.Get(headerPrefix + "Plan-Type")),
	}, nil
}

// headerWindow reshapes one window's headers into the document the body parser
// already validates, so both transports share one definition of "usable".
//
// Only the two bare prefixes are read. The per-model (X-Codex-Additional-*) and
// code-review windows repeat this shape, and folding a sub-limit into the
// account-wide budget would double-count it.
func headerWindow(h http.Header, prefix string) *windowDoc {
	doc := &windowDoc{
		UsedPercent: headerFloat(h, prefix+"Used-Percent"),
		ResetAfter:  headerInt(h, prefix+"Reset-After-Seconds"),
		ResetAt:     headerInt(h, prefix+"Reset-At"),
	}
	// The body sizes a window in seconds and the headers in minutes; fold
	// reads the seconds, so convert here rather than teaching it both units.
	if minutes := headerInt(h, prefix+"Window-Minutes"); minutes != nil && *minutes > 0 && *minutes <= maxWindowMinutes {
		seconds := *minutes * 60
		doc.Span = &seconds
	}
	return doc
}

func headerFloat(h http.Header, name string) *float64 {
	raw := strings.TrimSpace(h.Get(name))
	if raw == "" {
		return nil
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return nil
	}
	return &value
}

func headerInt(h http.Header, name string) *int64 {
	raw := strings.TrimSpace(h.Get(name))
	if raw == "" {
		return nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return nil
	}
	return &value
}
