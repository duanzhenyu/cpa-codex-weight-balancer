package balancer

import (
	"cmp"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/duanzhenyu/cpa-codex-weight-balancer/internal/quota"
)

// Burn is the consumption rate observed between two probes. It measures the
// account's *total* drain — sticky sessions included — which is why it cannot
// be inferred from the weight the plugin set. See doc §5.4 constraint 3.
type Burn struct {
	Rate  float64
	Known bool
}

// Decision is the engine's verdict for one account in one cycle.
type Decision struct {
	AuthID   string
	State    State
	Weight   int64
	Write    bool // the weight differs from the last one written back
	Degraded bool // the score is a bare percentage, not comparable across tiers
	Burn     Burn
	// Error is why this account carried no usable snapshot this cycle. A
	// frozen account and one still warming up both report state=pending at
	// weight 0, so without this they cannot be told apart.
	Error string
}

// Report is one full cycle.
type Report struct {
	Decisions []Decision
	// Changed counts the weights the engine wants moved.
	Changed int
	// Wrote counts the ones the host confirmed. Only a confirmed write wipes
	// the host's smooth-WRR accumulator for the whole fleet — see doc §5.4
	// constraint 1 point 2, which is why the two are reported apart.
	Wrote int
}

// Engine holds the state that must survive across cycles and restarts.
//
// The ticker and the management routes reach it concurrently, so every field
// below is guarded: a status page rendering while a cycle folds in new
// observations must not iterate a map that is being written.
type Engine struct {
	Policy Policy

	mu       sync.Mutex
	accounts map[string]Account
	written  map[string]int64
	previous map[string]quota.Snapshot
}

// NewEngine returns an engine driven by p.
func NewEngine(p Policy) *Engine {
	return &Engine{
		Policy:   p,
		accounts: map[string]Account{},
		written:  map[string]int64{},
		previous: map[string]quota.Snapshot{},
	}
}

// Accounts copies out the per-account state, for persistence and status pages.
func (e *Engine) Accounts() map[string]Account {
	e.mu.Lock()
	defer e.mu.Unlock()
	return maps.Clone(e.accounts)
}

// Written copies out the last weight written back for each account. Restoring
// it alongside Accounts is what stops a reload from rewriting every credential
// and wiping the host's routing accumulator.
func (e *Engine) Written() map[string]int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return maps.Clone(e.written)
}

// Restore reinstates persisted state so a restart neither re-prices every
// account nor rewrites weights that did not move.
func (e *Engine) Restore(accounts map[string]Account, written map[string]int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if accounts != nil {
		e.accounts = maps.Clone(accounts)
	}
	if written != nil {
		e.written = maps.Clone(written)
	}
}

// Commit records a weight the host confirmed on disk. Until it is called the
// weight stays outstanding, so a rejected save is retried on the next cycle
// instead of being silently dropped.
func (e *Engine) Commit(authID string, weight int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.written[authID] = weight
}

// Cycle folds one round of observations into new weights.
func (e *Engine) Cycle(obs []Observation, now time.Time) Report {
	e.mu.Lock()
	defer e.mu.Unlock()
	rep := Report{Decisions: make([]Decision, 0, len(obs))}
	for _, o := range obs {
		id := o.Snapshot.AuthID
		next, rate, err := e.Policy.evaluate(e.accounts[id], o, now)
		e.accounts[id] = next

		d := Decision{AuthID: id, State: next.State, Weight: next.Weight, Degraded: rate.Degraded}
		if err != nil {
			d.Error = err.Error()
		}
		if err == nil {
			if prev, ok := e.previous[id]; ok {
				d.Burn = observedBurn(prev, o.Snapshot)
			}
			e.previous[id] = o.Snapshot
		}
		// Pending accounts are not ours to touch yet.
		if next.State != StatePending {
			if last, ok := e.written[id]; !ok || last != next.Weight {
				d.Write = true
				rep.Changed++
			}
		}
		rep.Decisions = append(rep.Decisions, d)
	}
	slices.SortFunc(rep.Decisions, func(a, b Decision) int { return cmp.Compare(a.AuthID, b.AuthID) })
	return rep
}

// observedBurn derives the weekly drain rate between two snapshots. A window
// that rolled over in between carries no measurable delta.
func observedBurn(prev, cur quota.Snapshot) Burn {
	dt := cur.ObservedAt.Sub(prev.ObservedAt).Seconds()
	if dt <= 0 || !cur.Weekly.ResetAt.Equal(prev.Weekly.ResetAt) {
		return Burn{}
	}
	drained := prev.Weekly.Remaining - cur.Weekly.Remaining
	if drained < 0 {
		return Burn{}
	}
	return Burn{Rate: drained / dt, Known: true}
}
