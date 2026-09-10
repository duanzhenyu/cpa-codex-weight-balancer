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

Codex reports each rate-limit window as a bare `used_percent` plus
`limit_window_seconds` and a reset time. There is **no plan capacity anywhere in the payload**, so a weekly
percentage and a 5-hour percentage are different units and cannot be combined.

Therefore:

- the **weekly window drives the weight** (largest `limit_window_seconds`)
- the **short window is an availability gate only** — spent means `weight: 0` until its
  own reset, then automatic recovery

Percentages are comparable *between accounts on the same window* only while every
account is on the same plan. `quota_capacity` is a single fleet-wide number: it puts
quota and observed burn in an absolute unit, but it cannot make accounts on different
plans comparable — that needs per-account capacity, which 0.1.0 does not model. It does
not change routing either; the quantizer anchor scales with it, so turning it on moves
no weight. The status endpoint reports `capacity_scaled`, which is the setting and not
a verdict about plan tiers. Mixed Plus/Pro fleets stay unsupported.

## Configuration

```yaml
plugins:
  enabled: true
  configs:
    cpa-codex-weight-balancer:
      enabled: true          # the instance switch; the host defaults it off
      interval: 15m          # probe cadence; minimum 1m
      alpha: 1               # urgency exponent
      t_floor: 15m           # lower clamp on time-to-reset
      bucket_q: 0.08         # relative width of one weight bucket
      hysteresis: 0.35       # overshoot, in buckets, before a weight moves
      warmup: 2              # good probes before taking over a weight
      freshness: 90m         # after this, a frozen weight is flagged stale
      quota_capacity: 0      # >0 puts quota and burn in one fleet-wide absolute unit
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

The `weight` on each row is the plugin's own target, not necessarily what is on disk:
during warmup it reads 0 while the credential keeps whatever weight it already had.

`weights_pending` counts the weights the plugin wants to move; `weights_written` counts
the saves that actually reached the host. The plugin does not claim to know whether the
host reset its routing credits — it cannot observe that. They differ when a save is
rejected, and when the re-read just before saving finds another writer already put the
target there. A rejected save is retried on the next cycle rather than being recorded as
done.

A restart re-reads each credential's weight from disk, so a value already at its target
is not proposed. Buckets are not persisted, though, so a restart can still legitimately
re-price an account whose score sits where the old hysteresis band was holding it.

## Known limits

- **Session affinity caps the leverage.** A binding hit never reaches weighted
  round-robin, so weights only steer *new* sessions. If a few long sessions dominate
  consumption, the plugin can only nudge; it cannot pull load off an account. The
  status endpoint reports `observed_burn_per_hour` measured from consecutive quota
  snapshots so you can see how much of the drain is actually controllable.
- **Repricing a credential resets the host's accumulated routing credits** for every
  candidate in that selector state, not just the one that moved. The host compares only
  credentials present in both the old and the new weight vector, and that vector holds
  positive weights only, so dropping one to 0 removes it from the candidate set instead
  of counting as a change. Per-account log bucketing with hysteresis reduces how often
  a reset happens but cannot prevent it: with tens of accounts, at least one crosses a
  bucket most cycles. Keep `interval` coarse.
- **The endpoint is trusted as configured.** Validation requires `https` and nothing
  else, because pinning a hostname would defeat the reason the field exists. Whatever
  host you name receives every account's access token, so treat a typo here as a
  credential leak.
- **A probe has no overall deadline.** The host's request API carries no timeout field,
  so a peer that connects and then never finishes a response holds that cycle open, and
  a shutdown or reconfigure waits behind it.
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
- **The probe endpoint is Codex's to change.** The default URL and its payload shape
  were verified against live accounts on 2026-09-09, but nothing contractual keeps them
  stable, so the endpoint is configurable — and must be `https`, since every probe
  carries a bearer token. Run with `dry_run: true` after any upgrade on either side.

## Requirements

- CLIProxyAPI built with plugin support — check for `X-Cpa-Support-Plugin: 1` on any
  management response.
- `routing.strategy: "weighted-round-robin"`. Under any other strategy the host never
  reads the weights and this plugin does nothing.
- Released binaries are `linux/amd64` and need **glibc ≥ 2.34**, measured inside the
  container that runs CPA rather than on the Docker host. Other platforms and older
  glibc must build from source.

## Install

From the plugin store: Management Center → Plugins → install `cpa-codex-weight-balancer`.

Manually: download the zip and `checksums.txt` from
[Releases](https://github.com/duanzhenyu/cpa-codex-weight-balancer/releases), run
`sha256sum -c checksums.txt`, then unzip the `.so` into your `plugins.dir`, optionally
under `<goos>/<goarch>/`. The plugin ID is the filename minus the extension and an
optional `-v<version>` suffix, so `cpa-codex-weight-balancer.so` and
`cpa-codex-weight-balancer-v0.1.0.so` both work — anything else renames the plugin.
Under Docker, put `plugins.dir` on a mounted volume: the default `"plugins"` is
relative to the container's working directory, which does not survive a rebuild.
Overwriting a library already loaded at the same path needs a restart.

Then enable it and give it a first, write-free run with `dry_run: true`.

## Build

```sh
make            # vet, test, build for the host platform
make release    # zip named <id>_<version>_<goos>_<goarch>.zip, library at the zip root
make checksums
```

Requires CGO. Cross-compiling needs a matching C toolchain per target.

## License

MIT. See [LICENSE](LICENSE).
