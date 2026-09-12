package controller

import (
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/duanzhenyu/cpa-codex-weight-balancer/internal/codex"
)

// observations holds the quota each credential last reported on a request the
// host actually served. The host delivers usage records on its own queue, so
// these writes race every cycle.
type observations struct {
	mu   sync.Mutex
	seen map[string]codex.Usage
}

func (o *observations) put(authIndex string, u codex.Usage) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.seen == nil {
		o.seen = make(map[string]codex.Usage)
	}
	// The store holds each credential's current quota, so a record describing
	// an earlier response must not displace a later one.
	if held, ok := o.seen[authIndex]; ok && !u.Snapshot.ObservedAt.After(held.Snapshot.ObservedAt) {
		return
	}
	o.seen[authIndex] = u
}

// drain empties the store and returns what it held. A cycle prices the fleet
// from the observations that existed when it started: consuming them one at a
// time instead would let a record that landed mid-cycle be spent by the very
// cycle that could not have accounted for it, and would leave records for
// credentials the host no longer lists in the map forever.
func (o *observations) drain() map[string]codex.Usage {
	o.mu.Lock()
	defer o.mu.Unlock()
	seen := o.seen
	o.seen = nil
	return seen
}

func (o *observations) len() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.seen)
}

// Observe records the quota an upstream Codex response reported for one
// credential, to be used in place of that credential's probe on the next cycle.
// The response headers carry the same two account-wide windows the probe
// fetches, but they arrive with traffic the host was serving anyway instead of
// costing a request that has to carry the access token.
//
// It never writes a weight. Reacting here would move weights at request rate,
// and every weight change wipes the host's whole smooth-WRR accumulator.
//
// It reports whether the response carried a usable snapshot; anything else — a
// non-Codex response, an upstream error answered before quota accounting — is
// dropped, leaving the probe in charge of that credential.
func (c *Controller) Observe(authIndex string, headers http.Header, observedAt time.Time) bool {
	authIndex = strings.TrimSpace(authIndex)
	// The host lists credentials at registration before it has assigned their
	// runtime indices. Filing an observation under the empty id would collapse
	// the fleet onto a phantom account.
	if authIndex == "" {
		return false
	}
	// The generation is stamped by the cycle that consumes this, so that it
	// stays a single sequence shared with the probes.
	usage, err := codex.ParseHeaders(headers, authIndex, 0, observedAt)
	if err != nil {
		return false
	}
	c.observed.put(authIndex, usage)
	return true
}

// Pending reports how many credentials have reported quota since the last
// cycle and will therefore be priced without a probe.
func (c *Controller) Pending() int { return c.observed.len() }
