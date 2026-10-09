package controller

import (
	"maps"
	"strings"
	"sync"
	"time"

	"github.com/duanzhenyu/cpa-codex-weight-balancer/internal/balancer"
)

// outcomes tallies each credential's requests between ticks. The usage hook
// writes it from the host's dispatch queue while ticks drain it.
type outcomes struct {
	mu    sync.Mutex
	tally map[string]balancer.Outcomes
	// served is when the last counted success on each credential started. It
	// outlives the drain, because a duplicate can land after the tick.
	served map[string]time.Time
}

func (o *outcomes) drain() map[string]balancer.Outcomes {
	o.mu.Lock()
	defer o.mu.Unlock()
	tally := o.tally
	o.tally = nil
	return tally
}

// NoteOutcome counts one request the host served on this credential, and
// whether it failed for a reason of the credential's own. Like NoteRejected it
// does no I/O and moves no weight: the count waits for the ticker. It reports
// whether the request was counted, which it never is with the switch off.
func (c *Controller) NoteOutcome(authIndex string, failed bool, requestedAt time.Time) bool {
	authIndex = strings.TrimSpace(authIndex)
	if !c.opts.Policy.ErrorPenalty || authIndex == "" {
		return false
	}
	c.outcomes.mu.Lock()
	defer c.outcomes.mu.Unlock()
	// The host reports an image-generation tool call as two served records of one
	// attempt, both stamped with its start. Counting both dilutes the rate.
	if !failed && !requestedAt.IsZero() {
		if c.outcomes.served[authIndex].Equal(requestedAt) {
			return false
		}
		if c.outcomes.served == nil {
			c.outcomes.served = make(map[string]time.Time)
		}
		c.outcomes.served[authIndex] = requestedAt
	}
	if c.outcomes.tally == nil {
		c.outcomes.tally = make(map[string]balancer.Outcomes)
	}
	t := c.outcomes.tally[authIndex]
	t.Requests++
	if failed {
		t.Failures++
	}
	c.outcomes.tally[authIndex] = t
	return true
}

// Outcomes copies out the tallies awaiting the next tick.
func (c *Controller) Outcomes() map[string]balancer.Outcomes {
	c.outcomes.mu.Lock()
	defer c.outcomes.mu.Unlock()
	return maps.Clone(c.outcomes.tally)
}
