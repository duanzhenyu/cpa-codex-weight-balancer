# CPA Codex Weight Balancer

A [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) plugin that keeps Codex
credential weights in step with each account's remaining weekly quota **and how soon
that quota expires**, so the host's `weighted-round-robin` selector spreads new sessions
across accounts instead of draining one at a time — without touching session affinity.

The plugin never takes part in per-request selection. It declares only
`management_api`, runs on a timer, and writes the `weight` field back into each
credential. Routing stays entirely with the host, so `session-affinity` keeps working.

## Why time-to-reset, not just remaining quota

Quota does not roll over. Whatever is left when a window resets evaporates. So the
weight basis is a rate, not a level:

```
rate = remaining / max(time_to_reset, t_floor) ^ alpha
```

At `alpha = 1` this is the burn rate an account must sustain to finish its weekly
budget exactly at its own reset. Accounts whose reset is nearer therefore get more
traffic. `alpha = 0` ignores deadlines and weights by remaining quota alone; above 1
leans harder toward whichever account is closest to losing its quota.

**This is a heuristic, not an optimum.** When traffic cannot fill the fleet's total
capacity, proportional allocation still wastes quota — earliest-deadline-first would
waste less. The rate is a good urgency signal; it is not a zero-evaporation proof.

## What the two Codex windows can and cannot do

Codex reports each rate-limit window as a bare `used_percent` plus `window_minutes`
and a reset time. There is **no plan capacity anywhere in the payload**, so a weekly
percentage and a 5-hour percentage are different units and cannot be combined.

Therefore:

- the **weekly window drives the weight** (largest `window_minutes`)
- the **short window is an availability gate only** — spent means `weight: 0` until its
  own reset, then automatic recovery

Percentages are comparable *between accounts on the same window* only while every
account is on the same plan. Mixed Plus/Pro fleets need `quota_capacity` per account to
be meaningful; until then the status endpoint reports `tier_comparable: false`.

## Configuration

```yaml
plugins:
  enabled: true
  configs:
    cpa-codex-weight-balancer:
      interval: 15m          # probe cadence; minimum 1m
      alpha: 1               # urgency exponent
      t_floor: 15m           # lower clamp on time-to-reset
      bucket_q: 0.08         # relative width of one weight bucket
      hysteresis: 0.35       # overshoot, in buckets, before a weight moves
      warmup: 2              # good probes before taking over a weight
      freshness: 90m         # how long a frozen weight stays trusted
      quota_capacity: 0      # >0 scales percentages into a tier-comparable unit
      dry_run: false         # compute and report, write nothing
      # endpoint: https://chatgpt.com/backend-api/wham/usage   # must be https
```

And the host must actually use weights:

```yaml
routing:
  strategy: "weighted-round-robin"
  session-affinity: true
```

## Status

Both routes are Management-API authenticated, not public resources — per-credential
state is not something to expose unauthenticated.

```
GET  /v0/management/cpa-codex-weight-balancer/status
POST /v0/management/cpa-codex-weight-balancer/refresh
```

The status payload carries auth indices, states, weights and observed burn. It carries
no tokens, no credential JSON and no file names.

`weights_pending` counts the weights the plugin wants to move; `weights_written` counts
what the host confirmed. They differ when a save is rejected, and only the second one
means the routing accumulator was actually wiped. A pending write stays pending — a
rejected save is retried on the next cycle rather than being recorded as done.

## Known limits

- **Session affinity caps the leverage.** A binding hit never reaches weighted
  round-robin, so weights only steer *new* sessions. If a few long sessions dominate
  consumption, the plugin can only nudge; it cannot pull load off an account. The
  status endpoint reports `observed_burn_per_hour` measured from consecutive quota
  snapshots so you can see how much of the drain is actually controllable.
- **Every weight change wipes the host's routing accumulator** for the whole fleet
  (`weightsConfigChanged` compares the entire map). Per-account log bucketing with
  hysteresis reduces how often that happens but cannot prevent it: with tens of
  accounts, at least one crosses a bucket most cycles. Keep `interval` coarse.
- **`weight: 1` is not "no new sessions".** Against a peer at 100 it still takes
  roughly 1% of cold bindings. It means *smallest positive share*.
- **`host.auth.save` replaces the whole document with no compare-and-swap.** Writes
  re-read immediately beforehand and preserve every other key, which narrows the race
  against a concurrent token refresh but does not close it.
- **A spent short window recovers blind if the probe stays down.** `weight: 0` lifts at
  the recorded reset even without a fresh snapshot, re-admitting the account at the
  minimum weight. Leaving it excluded until a probe succeeds would strand it for the
  whole outage; re-admitting it at full weight would be a guess. The next good probe
  prices it properly.
- **An HTTP error from the probe is treated as transient, never as exclusion.** A 429
  on the usage endpoint says the probe was throttled, not that the account cannot serve
  inference, so the last good weight is frozen rather than zeroed. There is no backoff:
  a persistently failing endpoint is re-probed every `interval`.
- **The probe endpoint is unverified.** CPA parses this payload but never fetches it;
  the URL comes from the design research. It is configurable for that reason — it must
  be `https`, since every probe carries a bearer token. Run with `dry_run: true` first.

## Build

```sh
make            # vet, test, build for the host platform
make release    # zip named <id>_<version>_<goos>_<goarch>.zip, library at the zip root
make checksums
```

Requires CGO. Cross-compiling needs a matching C toolchain per target.
