# CPA Codex Weight Balancer

**English** | [简体中文](README.zh-CN.md)

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
this is not on the request path, and the hook never writes a weight. Readings move weights
only on the timer; a 429 queues a probe that can exclude the account at once (see Known
limits).

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

Codex reports each rate-limit window as a bare `used_percent` plus `limit_window_seconds`
and a reset time. There is **no plan capacity anywhere in the payload**, so a weekly
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

## Banked reset credits

Codex grants accounts reset credits, each expiring 30 days after it was granted. Each is
titled "Full reset (Weekly + 5 hr)", so redeeming one should restore both windows — no
credit has been redeemed here to confirm that. A credit is a whole window of quota with a
deadline of its own, so with `reset_credits: true` it is priced exactly like one and added
to the weekly rate:

```
rate = weekly_remaining / max(time_to_reset, t_floor) ^ alpha
     + Σ full_window   / max(time_to_expiry, t_floor) ^ alpha    (each unexpired credit)
```

An account with no credits prices as before; each credit adds its own term, and a credit
nearer expiry adds more. The aim is to spend the account down while its credits are still
redeemable. **The plugin never redeems one** — that is the operator's call, in the CPA
management page, once the account is spent. Until then a spent window still answers 429,
so the credits are added only after the exclusion check: credits never keep a spent
account serving, and never take one to `weight: 0`.

`/wham/usage` carries only the count and no response header carries them at all, so the
switch costs one more token-bearing `GET /wham/rate-limit-reset-credits` per credential per
cycle, whether or not the account was probed. Only credits that are `available`,
supported by the plan, and of type `codex_rate_limits` count; one of those without a
usable `expires_at` fails the whole fetch. A failed fetch keeps the last list rather than
reading as none, which would reprice the account. Rechecks price
from that list without fetching, and it survives a configuration reload.

## Configuration

```yaml
plugins:
  enabled: true
  configs:
    cpa-codex-weight-balancer:
      enabled: true          # the instance switch; the host defaults it off
      interval: 15m          # reweight cycle; minimum 1m
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
      error_penalty: false   # discount by the account's own error rate; never below weight 1
      error_window: 12h      # how far back that rate looks; at least interval
      reset_credits: false   # price banked reset credits as extra windows; one more request per account per cycle
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

A failed probe freezes a row's state and weight and sets its `error`; `stale` turns true
once the frozen weight is older than `freshness`. A credential never priced yet therefore
reads `pending` at 0 both while warming up and while every probe fails — `error` is what
tells them apart.

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

`rejections_pending` counts the credentials a 429 has queued for a probe, and
`last_recheck` is when one was last probed off the timer. Rows show each account as it
stands now, so an exclusion a recheck made appears before the next cycle;
`weights_pending` and `weights_written` still describe the last cycle only.

`error_penalty` and `error_window` echo those settings. With the switch on, each row carries
`error_rate`: `failures / max(requests, 10)` over that credential's window, counting only
failures for reasons of its own, as of the last scheduled cycle.

`reset_credits` echoes that switch. With it on, each row carries `reset_credits`, the
unexpired credits held, `reset_credit_next_expiry` when there is one, and
`reset_credits_error` while the last fetch for that credential failed. Credit ids are
never shown.

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
  probed as before, and so is an excluded one: a request that began before its budget
  ran out can report quota left after the exclusion.
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
- **A 429 triggers a probe, not an exclusion.** A failed request is still never read as
  quota: a 429 answers with a spent window, and pricing off that would write
  `weight: 0` — a hard exclusion that evicts bound sticky sessions — at request rate.
  Instead the credential is queued and probed at once, off the timer, and only that
  probe can exclude it. Off the timer only moves into or out of `weight: 0` are applied;
  re-pricing a serving account still waits for the ticker. Rejections of one credential
  are coalesced and probed at most once a minute, and a further 429 on an excluded
  credential triggers no probe of its own; the ticker still probes it every cycle. On
  top of that, a timer rechecks the exclusion 30 seconds after the recorded reset and
  lifts it if the probe agrees; one that still reads spent is retried a minute later,
  since whether Codex has actually reset at the instant it reported is not verified.
  A 429 the probe does not confirm — budget left — changes nothing, and routing
  to that account is left to the host, which derives its own recovery time from
  `Retry-After` or blind exponential backoff capped at 30 minutes, never from the
  `Reset-After-Seconds` sitting in that same response. A recheck whose probe fails is
  retried once, a minute later; a probe endpoint that stays down is left to the ticker.
- **`error_penalty` discounts; it never excludes.** With it on, every request the host
  reports on a Codex credential counts toward that credential's error rate, except a
  failure that says nothing about the account: a 429 (the recheck path's), a client that
  hung up (499), and a fault of the request itself — 400/409/413/422, or a body naming
  `context_length_exceeded`, `previous_response_not_found` and the like, following the
  host's own `IsRequestFault`. An image-generation tool call, which the host reports as
  two served records of one attempt, counts once — unless another request's record on the
  same credential lands between the two, which dilutes the rate rather than inflating it.
  Each scheduled cycle folds its tally, stamped with the cycle's time, into a sliding
  window (`error_window`: default 12h, or `interval` if that is longer; never shorter
  than `interval`) and sets the rate to
  `failures / max(requests, 10)` summed over the window. A tally drops out at the first
  scheduled cycle a full window after it was folded. `POST /refresh` and the cycle a reload
  starts with price with the rate as it stands; only the ticker folds. The weekly score is
  multiplied by `1 − rate` before bucketing, so hysteresis absorbs a stray failure; an
  account failing everything sinks to weight 1 and keeps its sticky sessions. A penalized
  account draws less traffic and so less evidence: clean requests dilute a burst only
  gradually, and the penalty lifts in full once the burst leaves the window. The long
  memory costs writes. In a simulation at the defaults with 20 requests a cycle, four
  cycles of nothing but failures followed by clean traffic took 11 writes on the way back
  up, four of them in the first hour. An idle account moves only as samples expire: once
  for a burst of nothing but failures, up to once per expiring sample when the window holds
  fewer than 10 requests. The window lives in memory: a reload carries it over but drops
  the counts not yet folded, and a restart of the host clears it. An upstream outage
  penalizes the whole fleet alike — shares barely move, but every weight does. The window
  length and the floor of 10 requests are unmeasured placeholders, and which failures Codex
  actually produces has not been observed live.
- **`reset_credits` has an endgame.** At `alpha = 1` one credit alone prices at 2.3× a
  fresh week three days before expiry, 7× a day before, 168× an hour before, and 672× at
  the default `t_floor`. In that last day the account's bucket moves often, and every
  move wipes the host's routing accumulator fleet-wide; the account may also spend its
  five-hour window and be excluded, sticky sessions with it. The weekly window has the
  same shape near its reset — this adds one such endgame per credit. A redeemed credit
  lingers in the price for up to one cycle. What `applicable_available_count` in
  `/wham/usage` means is unknown, so it is not used.

## Requirements

- CLIProxyAPI built with plugin support — check for `X-Cpa-Support-Plugin: 1` on any
  management response.
- `routing.strategy: "weighted-round-robin"`. Under any other strategy the host never
  reads the weights and this plugin does nothing.
- One of the released platforms:

  | Platform | Needs | Loaded before publishing by | Also run in |
  |---|---|---|---|
  | `linux/amd64` | glibc ≥ 2.17 | `tools/loadcheck` | CPA 8.0.27 |
  | `linux/arm64` | glibc ≥ 2.17 | `tools/loadcheck` on an arm64 runner | — |
  | `windows/amd64` | — | `tools/loadcheck` on a Windows runner | — |
  | `darwin/arm64` | macOS ≥ 12 (CPA itself needs 15) | `tools/loadcheck` | CPA 8.0.27 |
  | `darwin/amd64` | macOS ≥ 12 (CPA itself needs 15) | `tools/loadcheck` under Rosetta and on an Intel runner, CPA 8.0.27 on the runner | CPA 8.0.27 under Rosetta |

  glibc 2.17 is the baseline CPA's own Linux build targets; measure it inside the
  container that runs CPA, not on the Docker host. `tools/loadcheck` loads the library
  the way the host does, registers it, runs one dry-run cycle whose calls back into the
  host land in Go and grow the stack there, and shuts it down; on Linux and macOS it
  then calls `dlclose` and checks that the image is still mapped. It does all of that
  twice, the second time on the image the first left behind, as re-enabling a plugin
  does — the ABI crossings, without the rest of the host.
- **`darwin/amd64` is built with a patched Go toolchain.** Stock Go keeps the current
  goroutine in TLS slot 6 (`0x30(GS)`) in every runtime there, so a stock-built Go plugin
  and the Go host overwrite each other's: CPA 8.0.27 under Rosetta crashes on the first
  plugin call that grows a stack, with this plugin and with others from the store, and
  `tools/loadcheck` crashes the same way on a native Intel Mac. This library is built
  with [`scripts/go-tls-slot.patch`](scripts/go-tls-slot.patch), which moves the plugin's
  runtime to slot 11 (`0x58(GS)`), the other slot Apple reserves, and the build fails if
  its disassembly still addresses slot 6. Every library built this way shares slot 11, so an Intel
  CPA can load only one of them; quota-reset-router, claude-seat-pacer and
  kpatdev-quota-router in the store use it too. That follows from the mechanism and has
  not been tried.
- **On macOS the library stays loaded until CPA exits.** CPA calls `dlclose` on a
  disabled plugin, but a Go runtime cannot be unloaded. On Linux Go's linker marks the
  library so that `dlclose` leaves it mapped; on macOS nothing does, and dyld unmaps it
  under the runtime's running threads. With 0.7.1's plugin code, CPA 8.0.27 then used a
  whole core on Apple Silicon, and more under Rosetta, for the minute measured after the
  plugin was disabled; with `tools/loadcheck` as the host lingering 150 seconds after
  `dlclose`, it hung on exit in two of three Rosetta runs. From 0.7.2 the plugin pins its
  own image when initialised, so disabling it only stops it, as on Linux and Windows.
  `tools/loadcheck` fails a library that `dlclose` unmaps, and the Intel job in
  `verify-release` fails if CPA uses half a core or more in the minute after the plugin
  is disabled.
- Go does not support loading a `c-shared` Go library into a Go program on Darwin or
  Windows at all (golang/go#29061, #49457), so for those platforms the table is the
  whole claim.

## Install

From the plugin store: Management Center → Plugins → install `cpa-codex-weight-balancer`.

Manually: download your platform's zip and `checksums.txt` from
[Releases](https://github.com/duanzhenyu/cpa-codex-weight-balancer/releases), run
`sha256sum --ignore-missing -c checksums.txt`, then unzip the library (`.so`, `.dll` or
`.dylib`) into your `plugins.dir`, optionally under `<goos>/<goarch>/`. The plugin ID is
the filename minus the extension and an optional `-v<version>` suffix, so
`cpa-codex-weight-balancer.so` and `cpa-codex-weight-balancer-v0.7.2.so` both work —
anything else renames the plugin.
Under Docker, put `plugins.dir` on a mounted volume: the default `"plugins"` is
relative to the container's working directory, which does not survive a rebuild.
Replacing a library CPA has loaded needs a restart, even after disabling the plugin: the
library stays loaded until CPA exits.

Then enable it and give it a first, write-free run with `dry_run: true`.

## Build

```sh
make            # vet, test, build for the host platform
make release    # zip named <id>_<version>_<goos>_<goarch>.zip, library at the zip root
make dist       # release zips for every platform above, plus checksums.txt
```

Requires CGO. `make dist` also needs GNU userland and a Docker that can run
`linux/amd64` images. It builds Linux and Windows in Docker (`scripts/Dockerfile`; zig
pins the glibc baseline) and both Darwin libraries on an Apple Silicon Mac over SSH
(`MAC_HOST`, default `macbook`; `MAC_PROXY` if downloads there need one), and loads
each library it can run, `darwin/amd64` under Rosetta. That one needs Go built from source
with `scripts/go-tls-slot.patch`, which the Mac does once and keeps in
`~/Library/Caches/cpa-wb-go-tls`;
`make build` refuses it. Upload them to a draft release, then run
`.github/workflows/verify-release.yml` on it to load `linux/arm64` and `windows/amd64`,
and `darwin/amd64` on a native Intel Mac, there in CPA as well
(`scripts/cpa-load-darwin-amd64.sh`).

## License

MIT. See [LICENSE](LICENSE).
