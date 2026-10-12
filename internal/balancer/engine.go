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
	Degraded bool // the score is a bare percentage, not an absolute quantity
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
	// Wrote counts the ones the host confirmed. A confirmed write is the only
	// thing that can reset the host's smooth-WRR credits, but it does not
	// always: the host compares only credentials present in both the old and
	// the new weight vector, and that vector holds positive weights only, so a
	// save dropping one to 0 removes it from the candidate set and resets
	// nothing. The plugin cannot observe which happened, which is why it
	// reports the two counts and claims nothing beyond them.
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
	// A zero window would keep only the latest tick and lift any penalty on the
	// next quiet one.
	if p.ErrorWindow <= 0 {
		p.ErrorWindow = DefaultErrorWindow
	}
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

// reconcile makes the file, not memory, the authority on what was written.
// Without it a weight removed behind the plugin's back — a re-login, an edit in
// the management UI — stays outstanding forever, because every later cycle sees
// its own record and skips the repair.
func (e *Engine) reconcile(id string, disk DiskWeight) {
	if !disk.Read {
		return
	}
	if disk.Present {
		e.written[id] = disk.Weight
		return
	}
	delete(e.written, id)
}

// Cycle folds one round of observations into new weights.
func (e *Engine) Cycle(obs []Observation, now time.Time) Report {
	e.mu.Lock()
	defer e.mu.Unlock()
	rep := Report{Decisions: make([]Decision, 0, len(obs))}
	for _, o := range obs {
		next, rate, err := e.Policy.evaluate(e.accounts[o.Snapshot.AuthID], o, now)
		d := e.apply(o, next, rate, err)
		if d.Write {
			rep.Changed++
		}
		rep.Decisions = append(rep.Decisions, d)
	}
	slices.SortFunc(rep.Decisions, func(a, b Decision) int { return cmp.Compare(a.AuthID, b.AuthID) })
	return rep
}

// Fold takes one tick's request outcomes into each listed account's error
// window, drops what has aged out of it, and recomputes the rate; a listed
// account that served nothing still ages. Only the ticker calls it, so a refresh
// or the cycle a reload starts with prices with the rate the last tick settled.
func (e *Engine) Fold(tally map[string]Outcomes, now time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	since := now.Add(-e.Policy.ErrorWindow)
	for id, o := range tally {
		a := e.accounts[id]
		var window []Sample
		for _, s := range a.Errors {
			if s.At.After(since) {
				window = append(window, s)
			}
		}
		if o.Requests > 0 {
			window = append(window, Sample{At: now, Outcomes: o})
		}
		a.Errors, a.ErrorRate = window, errorRate(window)
		e.accounts[id] = a
	}
}

// Recheck folds one probe taken off the timer — in answer to a rejected request,
// or because an exclusion fell due. Only exclusions may move there: re-pricing a
// serving account would commit a bucket the timer never chose, and the next
// cycle would write it, wiping the host's accumulator once per rejection. Unless
// the account is excluded before or after this probe, it reports false and
// leaves the account exactly as it was.
func (e *Engine) Recheck(o Observation, now time.Time) (Decision, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	prev := e.accounts[o.Snapshot.AuthID]
	next, rate, err := e.Policy.evaluate(prev, o, now)
	if prev.State != StateUnavailable && next.State != StateUnavailable {
		return Decision{}, false
	}
	return e.apply(o, next, rate, err), true
}

func (e *Engine) apply(o Observation, next Account, rate Rate, err error) Decision {
	id := o.Snapshot.AuthID
	e.reconcile(id, o.Disk)
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
		}
	}
	return d
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
