package controller

import (
	"fmt"
	"maps"
	"strings"
	"sync"
	"time"

	"github.com/duanzhenyu/cpa-codex-weight-balancer/internal/balancer"
)

// RecheckGap bounds how often one credential is probed off the timer. A 429
// that is not about quota leaves the account serving, and the host goes on
// sending it requests that go on being rejected.
const RecheckGap = time.Minute

// RecoveryGrace delays the recheck of an exclusion past its reset. Codex reports
// resets to the second and can lag them, and a probe that lands too early costs
// a whole RecheckGap.
const RecoveryGrace = 30 * time.Second

// rechecks holds the credentials the host saw rejected. The usage hook writes it
// from the host's dispatch queue while the worker drains it.
type rechecks struct {
	mu       sync.Mutex
	rejected map[string]struct{}
	last     map[string]time.Time
	lastAt   time.Time
	// retried holds the credentials whose failed recheck was already re-queued.
	retried map[string]struct{}
	// listed is what the last listing left in service. Recheck skips an
	// exclusion anywhere else, so scheduling one would leave the worker's timer
	// permanently overdue.
	listed map[string]bool
}

func (r *rechecks) list(listed map[string]bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.listed = listed
}

func (r *rechecks) queueLocked(authIndex string) {
	if r.rejected == nil {
		r.rejected = make(map[string]struct{})
	}
	r.rejected[authIndex] = struct{}{}
}

// requeue puts a rejection back without counting it as a new one.
func (r *rechecks) requeue(authIndex string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.queueLocked(authIndex)
}

// retry re-queues a rejection whose probe failed, once: a probe endpoint that
// stays down must not be probed every gap without end.
func (r *rechecks) retry(authIndex string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, done := r.retried[authIndex]; done {
		delete(r.retried, authIndex)
		return
	}
	if r.retried == nil {
		r.retried = make(map[string]struct{})
	}
	r.retried[authIndex] = struct{}{}
	r.queueLocked(authIndex)
}

func (r *rechecks) drain() map[string]struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.rejected
	r.rejected = nil
	return out
}

// due reports whether the credential may be probed now, and books the attempt
// if so: a probe that fails still counts, or a broken endpoint is hammered.
func (r *rechecks) due(authIndex string, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if last, ok := r.last[authIndex]; ok && now.Sub(last) < RecheckGap {
		return false
	}
	if r.last == nil {
		r.last = make(map[string]time.Time)
	}
	r.last[authIndex] = now
	r.lastAt = now
	return true
}

// NoteRejected records that the host saw a 429 on this credential. It does no
// I/O: the hook runs on the host's usage queue, and a probe there would stall
// every record behind it. It reports whether the rejection was recorded.
func (c *Controller) NoteRejected(authIndex string) bool {
	authIndex = strings.TrimSpace(authIndex)
	if authIndex == "" {
		return false
	}
	c.rechecks.mu.Lock()
	defer c.rechecks.mu.Unlock()
	c.rechecks.queueLocked(authIndex)
	// A fresh rejection is fresh evidence and earns its own retry.
	delete(c.rechecks.retried, authIndex)
	return true
}

// Recheck probes the credentials rejected since it last ran and the exclusions
// whose reset has passed. Only the probe is evidence here: a rejection's own
// headers are the host's to act on, and a served request's reading may predate
// the budget running out — which is how a spent account kept its full weight.
func (c *Controller) Recheck(now time.Time) (balancer.Report, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	entries, err := c.host.AuthList()
	if err != nil {
		return balancer.Report{}, fmt.Errorf("list credentials: %w", err)
	}
	rejected := c.rechecks.drain()
	accounts := c.engine.Accounts()
	listed := make(map[string]bool, len(entries))
	for _, entry := range entries {
		if serviceable(entry) {
			listed[entry.AuthIndex] = true
		}
	}
	c.rechecks.list(listed)
	c.resets.keep(listed)
	var report balancer.Report
	for _, entry := range entries {
		id := entry.AuthIndex
		if !listed[id] {
			continue
		}
		_, flagged := rejected[id]
		account := accounts[id]
		if account.State == balancer.StateUnavailable {
			// Rejections are expected while excluded and say nothing new.
			if account.RecoverAt.IsZero() || now.Before(account.RecoverAt.Add(RecoveryGrace)) {
				continue
			}
		} else if !flagged {
			continue
		}
		if !c.rechecks.due(id, now) {
			// The last rejection of a burst matters most, so it waits out the gap.
			if flagged {
				c.rechecks.requeue(id)
			}
			continue
		}

		// A host that cools the account after its 429 may send no further
		// rejection to re-queue it, so a failed attempt gets one retry.
		retryable := flagged && account.State != balancer.StateUnavailable
		name, raw, errGet := c.host.AuthGet(id)
		if errGet != nil {
			c.host.Log("warn", fmt.Sprintf("recheck re-read failed for %s: %v", id, errGet))
			if retryable {
				c.rechecks.retry(id)
			}
			continue
		}
		observation, _ := c.probe(id, raw, now)
		if observation.Err != nil {
			c.host.Log("warn", fmt.Sprintf("recheck probe failed for %s: %v", id, observation.Err))
			if retryable {
				c.rechecks.retry(id)
			}
		}
		observation.Disk = diskWeight(raw)
		observation.Resets = c.resetWindows(id)
		decision, moved := c.engine.Recheck(observation, now)
		if !moved {
			continue
		}
		report.Decisions = append(report.Decisions, decision)
		if !decision.Write {
			continue
		}
		report.Changed++
		if !c.commit(name, decision) {
			continue
		}
		report.Wrote++
		if decision.State == balancer.StateUnavailable {
			c.host.Log("info", fmt.Sprintf("%s was rejected with its quota spent: excluded until %s",
				id, c.engine.Accounts()[id].RecoverAt.Format(time.RFC3339)))
		} else {
			c.host.Log("info", fmt.Sprintf("%s re-admitted at weight %d", id, decision.Weight))
		}
	}
	return report, nil
}

// NextRecheck is when Recheck next has work: the earliest exclusion falling due,
// or a pending rejection, due now unless the gap defers it. A rejection that
// outlives the wake which carried it — the listing failed — is otherwise never
// retried.
func (c *Controller) NextRecheck(now time.Time) (time.Time, bool) {
	accounts := c.engine.Accounts()
	c.rechecks.mu.Lock()
	defer c.rechecks.mu.Unlock()
	var next time.Time
	consider := func(id string, at time.Time) {
		if last, ok := c.rechecks.last[id]; ok && at.Before(last.Add(RecheckGap)) {
			at = last.Add(RecheckGap)
		}
		if !at.IsZero() && (next.IsZero() || at.Before(next)) {
			next = at
		}
	}
	for id, account := range accounts {
		if c.rechecks.listed[id] && account.State == balancer.StateUnavailable && !account.RecoverAt.IsZero() {
			consider(id, account.RecoverAt.Add(RecoveryGrace))
		}
	}
	for id := range c.rechecks.rejected {
		if accounts[id].State != balancer.StateUnavailable {
			consider(id, now)
		}
	}
	return next, !next.IsZero()
}

// RecheckState is what survives a configuration reload: a queued rejection would
// otherwise wait for the ticker, a lost gap lets the new worker probe the same
// credential again at once, and without the last listing a failed first listing
// leaves every exclusion unscheduled.
type RecheckState struct {
	Rejected map[string]struct{}
	Last     map[string]time.Time
	Listed   map[string]bool
	LastAt   time.Time
}

// Rechecks copies the state out.
func (c *Controller) Rechecks() RecheckState {
	c.rechecks.mu.Lock()
	defer c.rechecks.mu.Unlock()
	return RecheckState{
		Rejected: maps.Clone(c.rechecks.rejected),
		Last:     maps.Clone(c.rechecks.last),
		Listed:   maps.Clone(c.rechecks.listed),
		LastAt:   c.rechecks.lastAt,
	}
}

// RestoreRechecks hands the state to the worker replacing this one.
func (c *Controller) RestoreRechecks(s RecheckState) {
	c.rechecks.mu.Lock()
	defer c.rechecks.mu.Unlock()
	c.rechecks.rejected = maps.Clone(s.Rejected)
	c.rechecks.last = maps.Clone(s.Last)
	c.rechecks.listed = maps.Clone(s.Listed)
	c.rechecks.lastAt = s.LastAt
}

// Rejections reports how many credentials have a rejection waiting on a probe.
// It tells a usage hook that never fires apart from a fleet nobody rejects.
func (c *Controller) Rejections() int {
	c.rechecks.mu.Lock()
	defer c.rechecks.mu.Unlock()
	return len(c.rechecks.rejected)
}

// LastRecheck is when a credential was last probed off the timer.
func (c *Controller) LastRecheck() time.Time {
	c.rechecks.mu.Lock()
	defer c.rechecks.mu.Unlock()
	return c.rechecks.lastAt
}
