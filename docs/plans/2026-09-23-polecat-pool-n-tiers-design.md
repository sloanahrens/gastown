# polecat_pool: N agent tiers

**Bead:** gt-xmsqb (P2)
**Status:** design, awaiting implementation plan

## Summary

`polecat_pool` can cap exactly two agent classes. An agent it does not own is
passed through uncapped — and, more consequentially, uncounted. This design
generalizes the pool to an ordered list of tiers, separates *capping* from
*counting*, and adds one new concept (`request_only`) without which the change
would make spend worse rather than better.

The driver is concrete: the operator wants a hard three-polecat cap — one
`local-coder-polecat`, one `deepseek-flash`, one `claude-sonnet` — and only two
of the three are expressible today. The interim measure is `max_overflow: 2 → 1`
plus a mayor self-policy for sonnet, which is an intention rather than a limit.

## The problem

`config.PolecatPool` (`internal/config/types.go:2066-2085`) has two tiers:
`LocalAgent`/`MaxLocal` and `OverflowAgent`/`MaxOverflow`. The only enforcement
points in the tree are those two fields, read in `internal/cmd/sling_pool.go`
and `internal/cmd/daemon_dispatch.go`.

An agent the pool does not own is passed through untouched. This is deliberate
and documented at `sling_pool.go:174-177`:

> An agent the pool does not own leaves it nothing to admit, and the request
> stands untouched (gt-4lbz).

That was a sound rule when the pool was local-plus-overflow and everything else
resolved to the `role_agents` default. It now means `claude-sonnet` — which
reaches a seat only via an explicit `gt sling --agent claude-sonnet` for
needs-sonnet beads — cannot be capped at all.

### The half that matters more

Those slings are also **uncounted**. `poolSeatPicture`
(`daemon_dispatch.go:300-325`) computes:

```go
capacity := pool.MaxLocal
if capped {
    capacity += pool.MaxOverflow
}
seats.Occupied = local + overflow
```

An explicitly-slung polecat consumes a real seat, real money and real host load,
and appears in neither number. The town's actual concurrency is therefore
`MaxLocal + MaxOverflow + (unbounded explicit slings)`, while every capacity
reading — dispatch decisions and the dashboard's Local Pool panel — reports only
the first two terms.

Observed 2026-09-23 11:37 with `max_local=1`, `max_overflow=2`: four polecats
working, two of them `claude-sonnet` (marble on gt-6gsfv, quartz on gt-o1z7).
Nothing was misbehaving. The second sonnet was simply outside the accounting.

**Capping and counting have been conflated. Separating them is most of the fix.**

## Architecture

One new type, one normalization point, and every consumer reads the normalized
list.

```
settings/config.json ──┐
  tiers: [...]         ├─► EffectiveTiers() ──► []PoolTier ──┬─► choosePoolAgent  (admission)
  (or legacy 4 fields) ─┘   (single adapter)                  ├─► poolSeatCounts   (counting)
                                                              ├─► claimFor         (reservation)
                                                              └─► poolSeatPicture  (reporting)
```

Legacy configuration is adapted once at the boundary and never branched on
again. A legacy town and a tiered town cannot diverge in behaviour, because they
differ only in what `EffectiveTiers()` returns.

## Components

### `PoolTier`

In `internal/config/types.go`:

- `Agent string` — the agent alias this tier admits.
- `Max int` — live sessions allowed on it.
- `RequestOnly bool` — when true, reachable only by an explicit `--agent`.

`PolecatPool` gains `Tiers []PoolTier`.

`idle_fill` and `min_spawn_gap` stay **pool-level**. `min_spawn_gap` continues to
apply to the **first tier only**, which reproduces today's local-seat prefill
guard (every fresh local session spends 20-25k tokens of prefill during which
the decoding slots starve). This is a deliberate non-generalization, recorded
here so it does not read as an oversight: there is one GPU seat that needs a
stagger, and `gt-nn7n` built `idle_fill` for that same single seat.

### `EffectiveTiers()`

Returns `Tiers` when non-empty. Otherwise synthesizes
`[{local_agent, max_local}, {overflow_agent, max_overflow}]`, omitting the
overflow tier when `overflow_agent` is empty and treating `max_overflow == 0` as
uncapped exactly as `OverflowCapped()` does today.

### `internal/cmd/pool_tiers.go` (new file)

Holds the tier lookup, per-tier counting and the cascade-order helper.
`sling_pool.go` is already 971 lines; adding the tier model inline would push it
past 1,100, and the tier model should be testable without reaching through
routing. This is a boundary improvement in code the change already touches, not
unrelated refactoring.

### Changed signatures

`poolSeatCounts` returns per-tier counts plus an untiered count:

```go
func poolSeatCounts(tiers []config.PoolTier, sessions []poolSession) (perTier []int, untiered int, newestFirstTier time.Time)
```

`claimFor`'s single two-tier line:

```go
capped := agent == pool.LocalAgent || (agent == pool.OverflowAgent && pool.OverflowCapped())
```

becomes "the agent is in a tier with a cap". `poolSeatClaim` already carries an
`Agent` field, so per-tier claims need no new state.

`choosePoolAgent` stays **pure** — its one side effect, attaching
`localAttemptLabel`, remains the caller's.

## Data flow

`choosePoolAgent` keeps its existing structure — a cascade of cases ending in
refusal — but iterates tiers instead of two named fields:

1. **Explicit `--agent X`**
   - X is in a tier → that tier's rules alone. Full ⇒ **refuse**, naming the
     tier. Never cascade (see Error handling).
   - X is in no tier → **untouched**, evaluated ahead of the `local-attempt:1`
     rule (gt-gcrk).
2. **No explicit agent** — `shape()` picks a preferred tier among
   **shape-routed tiers only**. `request_only` tiers are skipped entirely.
3. **Cascade** — full or staggered ⇒ the next shape-routed tier; past the last
   ⇒ refuse.

`shape()` is unchanged: `task`/`chore`/`docs` prefer the first tier,
`bug`/`feature` the overflow tier, anything else has no opinion and the seat
count decides.

### Why `request_only` is load-bearing

It is the one new idea here, and the design fails without it. With a plain
cascade, a chore that finds the local tier full walks to the next tier, and then
the next — onto `claude-sonnet`, the most expensive seat in the town. A cap
introduced to reduce spend would *increase* it, in exactly the circumstances
(local seat busy) that the cap makes more common.

`request_only` tiers are capped and counted like any other. They are simply not
reachable by shape or by cascade — only by a caller naming them.

## Error handling

### Uncountable sessions stay fail-open

When sessions cannot be listed, routing falls back to the last shape-routed tier
and never refuses. This preserves a property `poolRoute` chose deliberately and
documents: refusing here "would otherwise stop every sling on a tmux hiccup".

**This is the design's known soft edge and is stated plainly rather than buried:
the cap is hard under normal operation and soft under infrastructure failure.**
It is a deliberate trade, accepted by the operator during design. The mitigation
is volume, not silence — count consecutive fallbacks and escalate past a
threshold, so a lapsing cap is loud.

### Untiered sessions never refuse

`dispatchSeats` gains `Untiered int` as its own field. Folding untiered sessions
into `Occupied` would drive `Free` toward zero, and `dispatchDecision`
(`daemon_dispatch.go:209-210`) gates on `seats.Free < 1` — so a single untiered
one-off `--agent claude-opus` session would refuse every tiered sling and convert
an observability fix into a town-wide dispatch outage.

Reporting therefore reads as tiered-plus-untiered separately (for example
`3/3 tiered, +1 untiered`). A single combined figure would drive free seats
negative, which the existing code clamps to zero — hiding precisely the overage
this bead exists to expose.

### An explicit request for a full tier is refused, never cascaded

Cascading would reintroduce gt-x40u: a request for a full seat silently served
by the other seat, spending on a paid provider the caller never asked for. With
more tiers there would be more ways to do it. The existing `refuseRequest`
already produces the right message shape — a line naming the seat the caller
asked for.

### `max: 0` means closed, not uncapped

A tier with `max: 0` is **closed**, preserving today's `max_local: 0` semantics
("no local dispatch", gt-4lbz). An absent `max_overflow` remains **uncapped**.
This asymmetry exists today; it is recorded here rather than silently changed,
because a config-schema change is exactly where such a rule gets "tidied" by
accident.

## Testing

| Test | Asserts |
|---|---|
| **Three-tier cap** | local 1 / flash 1 / sonnet 1 ⇒ a fourth concurrent sling is refused, not spawned |
| **Explicit full tier** | `--agent claude-sonnet` with that tier full is refused, naming the tier, and never served by another tier |
| **request_only isolation** | a chore with the first tier full cascades to flash and **never** to sonnet |
| **Untiered counted, not capping** | an untiered `--agent claude-opus` session moves `Untiered` while leaving `Free` and admission unchanged |
| **Legacy parity** | today's four-field config produces identical routing decisions, case by case |
| **Fail-open** | an unlistable session set routes to the last tier and never refuses; N consecutive fallbacks escalate |
| **`max: 0`** | a zero-max tier is closed, and an absent overflow max is uncapped |

**The legacy-parity test is load-bearing.** It is what proves the normalization
did not quietly reorder a case that gt-x40u, gt-4lbz, gt-nn7n or gt-gcrk each
paid a bug to establish. It should enumerate those four scenarios by name.

## Prior art — settled, not to be re-litigated

Each of these orderings was won by a specific bug. The cascade's *sequence* is
the asset; this design changes what it iterates over, not when it refuses.

- **gt-x40u** — an explicit `--agent` must be honoured or refused, never
  silently swapped to the paid provider.
- **gt-4lbz** — the convoy feeder and the deacon's RECOVERED_BEAD redispatch both
  spawned past `max_local`/`max_overflow`; also established that a non-pool agent
  passes through untouched.
- **gt-nn7n** — the `idle_fill` knob.
- **gt-gcrk** — the `local-attempt:1` label must not override an explicit
  `--agent` for a non-pool agent.

## Out of scope

- **Dashboard UI** beyond the corrected numbers falling out of `dispatchSeats`.
- **File migration.** `settings/config.json` carries API keys and must never be
  committed or mechanically rewritten. Both config forms are supported
  indefinitely.
- **A separate total cap.** The total is the sum of tier maxima. A second knob
  could disagree with the tiers and would be a second source of truth for one
  number.
- **Per-tier `idle_fill`.** One idle seat class matters; `gt-nn7n` built the knob
  for it.

## Target configuration

The three-tier config this design exists to make expressible:

```json
"polecat_pool": {
  "tiers": [
    { "agent": "local-coder-polecat", "max": 1 },
    { "agent": "deepseek-flash",      "max": 1 },
    { "agent": "claude-sonnet",       "max": 1, "request_only": true }
  ],
  "idle_fill": true,
  "min_spawn_gap": "4m"
}
```
