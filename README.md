# CPA Codex Weight Balancer

A [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) plugin that keeps Codex
credential weights in step with each account's remaining weekly quota **and how soon
that quota expires**, so the host's `weighted-round-robin` selector spreads new sessions
across accounts instead of draining one at a time — without touching session affinity.

The plugin never takes part in per-request selection. It declares `management_api` and
`usage_plugin`, runs on a timer, and writes the `weight` field back into each
credential. Routing stays entirely with the host, so `session-affinity` keeps working.

`usage_plugin` subscribes to the host's usage records, which carry the upstream response
headers. Codex reports the same two account-wide windows there that the probe fetches,
so a credential that served a request since the last cycle is priced from that reading
instead of costing a probe. The host dispatches those records from its own queue, so
this is not on the request path, and the hook never writes a weight — weights still move
only on the timer.

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
plans comparable — that needs per-account capacity, which this version does not
model. It does not change routing either; the quantizer anchor scales with it, so
turning it on moves no weight. The status endpoint reports `capacity_scaled`, which
is the setting and not a verdict about plan tiers. Mixed Plus/Pro fleets stay
unsupported.

## Dormant accounts

A Codex rate-limit window starts at the account's **first metered request**, not on a
schedule. Until then the account reports a reset exactly one window away and keeps
reporting it: the value slides with wall-clock instead of counting down. A quota probe
does not start it — only inference does.

Such a credential is scored forever as `remaining 1.0 / t = one whole window`, a constant.
It can never accumulate the deadline urgency this plugin is built on, while every started
account's `t` shrinks toward its reset. A started-but-idle account a day from its reset
outranks a dormant one roughly sevenfold; the dormant one cannot get there at all.

`kickstart: true` spends one minimal inference request on such a credential to start its
clock, then leaves it to normal pricing. It is the only thing here that costs quota, so it
is off by default, and `dry_run` suppresses it. So does a credential the host has marked
unavailable, or one whose own quota says it is already rate limited. A credential is nudged
at most once per `kickstart_cooldown`, booked on the attempt rather than on success — a
request that fails in flight may still have been metered upstream — and that cooldown is
carried across configuration reloads, which the host performs far more often than it is
long. The credential is re-read immediately before the request, because the same cycle may
have written a weight to it in between.

### The five-hour window

`kickstart_short_window: true` applies the same nudge to the short budget, on its own
switch and also off by default. It buys something different, and it is worth being blunt
about what:

**It changes no weight this plugin computes.** The score comes from the weekly window
alone; the short one is a pure availability gate that only matters once it is *spent*
(`remaining <= 0`), and a window that has never started is full. What it buys is upstream:
a short window whose clock is already running resets earlier relative to a later burst. An
account that starts its window at the moment it begins working is blocked from that moment
plus five hours when it exhausts the budget; one whose clock was started four hours earlier
recovers in one. That block is a `weight 0`, which the host reads as a hard exclusion and
answers by evicting the credential's sticky sessions too, so shortening it is not nothing.

It also costs more often. The weekly clock is started once and then runs for a week; the
short one expires on its own, and an idle account reads untouched again afterwards. **How
often is not verified** — only a never-used window was observed to stay unstarted, not an
expired one. If an expired window does re-arm the same way, an idle account costs one
minimal request per `kickstart_cooldown`-bounded rollover, on the order of five a day. If
Codex instead rolls the window straight into the next one, this switch only ever fires on
an account that has never been used, and `kickstart` would have caught that anyway.

One request starts both windows at the same instant (verified 2026-09-12: a served account
read `7917/18000` and `594717/604800`, both exactly 10083s in), so a credential that
satisfies both switches still costs exactly one request — and turning on only the
short-window switch will start the weekly clock too on an account that has never been used.

Neither switch moves traffic. Weights only steer *new* sessions, so a warmed credential that
receives no cold binding still expires its week unused — as it would have anyway, minus the
chance to compete for one.

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
      kickstart: false       # spend one request to start a dormant weekly window
      kickstart_short_window: false  # same for the 5h window; changes no weight, recurs
      kickstart_model: gpt-5.6-sol   # Codex rejects its own smaller models here
      kickstart_cooldown: 1h # minimum 1m; shared by both switches
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

`observations_pending` counts the credentials that reported quota on a served request
since the last cycle and will therefore be priced without a probe. It is the one signal
that separates a usage hook the host never reaches from a fleet that is simply idle.

`dormant_credentials` lists the auth indices whose weekly window has never started and
`short_dormant_credentials` those whose five-hour window has not, and
`kickstarts_sent` / `last_kickstart_error` separate "no dormant accounts" from "dormancy
detected but every nudge is being rejected".

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
- **A header reading can be up to one cycle older than a probe would be.** An account is
  priced from the last response it served, not from the moment the cycle runs. The
  staleness is self-limiting rather than bounded: a credential quiet enough to have no
  recent reading is also one whose windows have barely moved. An idle credential is
  probed as before.
- **The response headers are Codex's to change too.** `X-Codex-Primary/Secondary-*` was
  captured from live traffic on 2026-09-12 and CPA retains it verbatim, but it is not a
  documented contract. A response that carries no usable window is dropped and that
  credential falls back to its probe, so the failure mode is a lost optimisation rather
  than a wrong weight.
- **A kickstart cannot be confirmed from the response it gets.** Codex fills the quota
  headers before metering the request that carries them, so the 200 answering a kickstart
  still reports the window as dormant. A *weekly*-dormant usage-hook reading is therefore
  discarded and that credential probed instead — it drives the score. A *short*-dormant one
  is kept: it prices the week perfectly well, and it is positive evidence that the request
  behind it has just started the short clock, so the kickstart it would trigger is already
  paid for. That decision waits for a cycle with no traffic behind it.
- **How often `kickstart_short_window` recurs is not verified.** Codex was observed to
  leave a *never-used* window unstarted indefinitely; whether an *expired* one re-arms the
  same way was not, because every account in the fleet had a running short window at the
  time. The switch is safe either way — the cooldown bounds it — but the worst case is one
  minimal request per idle account per rollover, roughly five a day.
- **`kickstart_short_window` moves no weight.** The score reads the weekly window alone.
  The short one only gates availability once it is spent, and a window that never started
  is full, so nudging it changes nothing this plugin writes. Its payoff is upstream: an
  earlier reset relative to a later burst.
- **The inference endpoint is not configurable.** `kickstart_model` is, because Codex
  rejects its own smaller models there (`gpt-5.3-codex-spark` answers "not supported when
  using Codex with a ChatGPT account"), but the URL is fixed to what CPA itself posts to. A
  fleet pointed at a non-default Codex base URL gets a failed kickstart, which is inert.
- **This is not a 429 circuit breaker.** Only responses the host actually served are
  read; a failed request is dropped even though its headers parse, because a 429
  answers with a spent window and pricing off that would write `weight: 0` — a hard
  exclusion that evicts bound sticky sessions — at request rate rather than on the
  timer. The account is excluded on the next cycle if the probe agrees it is out.
  Instant exclusion on 429 is the host's own job (`Quota.Exceeded` plus the affinity
  selector's reselect); note that the host derives the *recovery* time from
  `Retry-After` or blind exponential backoff capped at 30 minutes, and never from the
  `Reset-After-Seconds` sitting in that same response.

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
`cpa-codex-weight-balancer-v0.2.0.so` both work — anything else renames the plugin.
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
