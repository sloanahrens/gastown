# Deacon self-probe: reporting, budget, and backlog control

**Bead:** gt-ecqx0 (P1) · **Supersedes the unbuilt half of:** gt-jmy3
**Depends on:** gt-wisp-6ups (pyrite, commit `d8e2c39d`) landing first
**Status:** design, awaiting implementation plan

## Summary

The deacon self-probe is a supervision signal: the daemon injects a known event
into the deacon's own mail inbox, the deacon's patrol loop acknowledges it, and
the daemon reads back whether the acknowledgement arrived in time. It exists
because no component in Gas Town fed a known input through its own detection
path, and because three separate STUCK DEACON incidents (09-08, 09-09, 09-10)
were only ever visible as *loss of signal*, which is indistinguishable from
health.

The probe shipped in `61e224d` on 2026-09-22 and **has never once succeeded**.
This document covers the half of the repair that remains after pyrite's
enumeration fix: making the verdict reachable, giving it a budget that an agent
patrol loop can actually meet, and bounding the probe backlog.

## Why the original shipped broken

`61e224d` did three things in one commit:

1. sent `DEACON_SELF_PROBE` mail to the deacon each doctor-dog cycle;
2. added `filterDeaconSelfProbes()` (`internal/cmd/mail_inbox.go:126`), which
   strips exactly those messages from `gt mail inbox`;
3. registered a doctor check that hard-fails unless the deacon acks them.

`mol-deacon-patrol.formula.toml:215-218` then instructed the deacon: *"For each
DEACON_SELF_PROBE message: `gt mail read <id>`"* — but `gt mail inbox` was its
only way to enumerate messages, and step 2 removed them from it. The deacon
could not obtain the IDs, so it acked none.

This was not a regression. Evidence: of the probes that had already left the
system, **every one still carried `delivery:pending` with no `delivery:acked`,
`delivery-acked-by` or `delivery-acked-at`** — they were reaped unacked by wisp
GC, not acknowledged. There is no before-and-after and no bisect window.

Two further facts make the failure invisible rather than merely present:

- **Nothing automated runs a full `gt doctor`.** Both `mol-dog-doctor` and
  `mol-deacon-patrol` only ever invoke `gt doctor --check <name>`, and
  `mol-deacon-patrol:1091` explicitly forbids the full run ("takes 60+ seconds
  and blocks the patrol loop"). The `deacon-self-probe` check therefore has no
  automated consumer at all.
- **The doc comment described behaviour that was never written.** Lines 18-24 of
  `internal/daemon/deacon_self_probe.go` asserted that "the deacon patrol's
  inbox-hygiene step recognizes this prefix and acknowledges the message
  mechanically". Prose stating intent as fact is how this shipped looking
  finished. (Corrected by pyrite.)

gt-jmy3's own acceptance criteria already required *"mol-deacon-patrol inbox
hygiene step acks probes without an LLM decision (mechanical step or gt
command)"*. That command was never built. This work finishes an unmet criterion;
it does not reverse a decision.

## What pyrite's MR already delivers

`gt-wisp-6ups` / `d8e2c39d` (9 files, +436/-23) covers the enumeration half:

- **`gt deacon ack-probes`** (`internal/cmd/deacon_ack_probes.go`) reads the
  deacon's mailbox directly, bypassing the inbox filter, finds unacked probes
  and acks each exactly as `gt mail read <id>` would. Probes are left in place
  after acking so the next doctor-dog cycle can read the ack back.
- **One mechanical formula step** replaces the unusable prose;
  `mol-deacon-patrol` goes v19 → v20.
- **Probes excluded from `gt status` unread counts**, via a constant re-exported
  through `internal/constants` so `internal/mail` can share it.
- **The stale doc comment corrected.**

This design assumes that MR merges. If it is rejected, the enumeration work
returns to scope and this document must be re-read as the second of two parts.

## What pyrite's MR does not deliver, and why that is not cosmetic

Grepping the full diff for `self_probe_budget`, `escalateAlert` and
`ConsecutiveErrors` returns **zero matches** for all three.

The consequence is specific and easy to miss. The budget is still
`ping_timeout = "30s"` from `config/roles/deacon.toml` — a value sized for a
network health-check ping — while the deacon's patrol loop cycles in minutes and
doctor-dog fires every 5 minutes. Once the deacon can ack, it will ack *late*,
and `EvaluateDeaconSelfProbe` will return:

```
Error: deacon patrol acked the probe late: 7m12s after send (budget 30s)
```

The verdict flips from "did not ack the probe" to "acked the probe late". It is
still a permanent `Error`, and it is still consumed by nobody. **The check will
appear fixed and will not be.**

## Architecture

Three components, split along the boundary that failed:

| Component | Responsibility | Layer |
|---|---|---|
| `SendDeaconSelfProbe` | inject a known event into the deacon's real inbox | daemon |
| `gt deacon ack-probes` | find and ack outstanding probes, mechanically | cmd (landed) |
| `EvaluateDeaconSelfProbe` + escalation | judge the previous probe; escalate on repeat failure | daemon (this work) |

### The load-bearing invariant

**The daemon may send and judge. It must never ack.**

The acknowledgement has to cross the process boundary into the deacon's own
patrol loop, because that crossing *is* the measurement. A daemon that acked its
own probe would be marking its own homework: it would prove the mail library
works and nothing about whether the deacon is alive and draining its inbox.

This is recorded here because the call looks like a redundant subprocess to
anyone optimising the daemon, and removing it would silently convert the check
into a no-op that still reports green. The wall-clock gap between send and ack
is not overhead — it is the patrol-loop cycle time, which is the quantity being
measured.

## Components

### Budget (`self_probe_budget`)

Add `self_probe_budget = "15m"` to `[health]` in
`internal/config/roles/deacon.toml`, and a `SelfProbeBudget Duration` field to
`RoleHealthConfig` (`internal/config/roles.go`). `deaconSelfProbeBudget()` reads
the new key and stops borrowing `ping_timeout`. The default constant moves from
`30 * time.Second` to `15 * time.Minute`.

The budget must exceed the doctor-dog interval (5m). Below that, a probe is
judged before the deacon could plausibly have run, and the check measures
scheduler skew rather than deacon health.

### State (`ConsecutiveErrors`)

`deaconSelfProbeBaseline` (persisted at `.runtime/deacon-self-probe.json`) gains
`ConsecutiveErrors int`. The existing `Nonce`, `SentAt` and `LastSendError`
fields are unchanged.

### Evaluation and escalation

Escalation reuses `d.escalateAlert(key, source, message)`
(`internal/daemon/jsonl_git_backup.go:885`), which already sends to the mayor via
`gt escalate`, deduplicates on `--fingerprint`, retries up to 3 times at a 60s
per-attempt timeout, and logs the full message to the feed as a last-resort
fallback. Its counterpart `clearAlerts(reason, keys...)` clears the alert on
recovery.

No new scheduling is required: `daemon.go:972` already calls
`runDeaconSelfProbe()` on the doctor-dog tick.

## Data flow

`runDeaconSelfProbe` becomes **evaluate-then-send**, per tick:

1. **Gate on pause.** If `deacon.IsPaused(townRoot)` reports paused, or the pause
   state cannot be read, do nothing and return.
2. **Evaluate** the outstanding probe:
   - `OK` → reset `ConsecutiveErrors` to 0; `clearAlerts` on the probe's key.
   - `Error` → increment `ConsecutiveErrors`.
   - `Skipped` → leave `ConsecutiveErrors` untouched. A skip is not evidence in
     either direction.
3. **Escalate** when `ConsecutiveErrors >= 3` (≈45 minutes at a 15m budget),
   under a stable fingerprint so repeats deduplicate.
4. **Sweep** probes older than two budgets.
5. **Send** exactly one new probe; record nonce and `SentAt`.

Sending *after* evaluating is what bounds the backlog at one outstanding probe.
The observed accumulation happened because step 5 ran unconditionally while
steps 1-4 did not exist.

Aggressive GC is deliberately avoided: `EvaluateDeaconSelfProbe` returns
`Skipped` when the probe is absent ("no longer present in the deacon inbox,
could not verify ack"). Sweeping too eagerly would silently degrade a hard
`Error` into an unknown, which is the failure mode this whole document exists to
eliminate.

## Error handling

**A paused deacon must not raise an alarm.** `gt deacon pause` exists and a
paused deacon legitimately will not ack. Evaluation is gated on
`deacon.IsPaused(townRoot)`, following the precedent already set at
`internal/deacon/heartbeat.go:156`, which fails closed on an unreadable pause
state for exactly this reason. Without this gate the redesign ships a guaranteed
false alarm the first time an operator pauses the deacon.

**`Skipped` is never `OK`.** An unreadable inbox, a failed send, or a missing
probe all stay `Skipped` and never reset the counter. The current code already
gets this right; it is recorded here so a later refactor does not "simplify" a
three-state verdict into a boolean.

**Escalation targets the mayor, never the deacon.** Escalating a deacon fault to
the deacon repeats the bug class being fixed: routing a fault report to the
component under test.

**Escalation is best-effort under a Dolt outage.** `escalateAlert` shells out to
`gt escalate`, which can fail under the same transient Dolt unavailability that
causes dog-molecule pours to fail (tracked separately as gt-i3rpw). After three
attempts it logs to the feed. This is a known and accepted degradation: a log
line is strictly better than today, where the verdict reaches nobody at all.
This design does not depend on gt-i3rpw landing.

## Testing

The alarming branch is the deliverable. gt-jmy3 shipped with green tests over a
feature that never worked once; these requirements exist to make that
specific outcome impossible.

| Test | Asserts |
|---|---|
| **Escalation integration** | probe sent, ack deliberately withheld, three evaluations advanced → `escalateAlert` fires with the expected fingerprint. **Must fail against current `main`.** |
| **Vacuous-pass guard** | probes-examined > 0, so "no probes, therefore no failures" cannot read green |
| **Paused deacon** | paused + unacked → no escalation |
| **Recovery** | `Error`, `Error`, then `OK` → counter resets and `clearAlerts` is called |
| **Backlog bound** | N ticks produce at most one outstanding probe |
| **Budget source** | `self_probe_budget` is read; `ping_timeout` is not consulted |

Existing fakes make this cheap: `deaconInboxLister` is a narrow interface
(`List() ([]*mail.Message, error)`) that already exists so tests can inject a
fake inbox instead of shelling out to `bd`, and `evaluateDeaconSelfProbeWith` /
`sendDeaconSelfProbeWith` take their dependencies as parameters.

## Out of scope

- **Dog-molecule pour failures** — `internal/daemon/dog_molecule.go:91-94` drops
  an entire patrol cycle on any transient `bd`/Dolt failure, with no retry,
  counter or escalation. Filed as **gt-i3rpw** (P1) and routed separately. This
  design deliberately does not depend on a molecule pour, which is why the two
  can proceed independently.
- **Checks with no automated consumer.** `deacon-self-probe` is one of an unknown
  number of checks registered in `gt doctor` that nothing automated ever runs.
  Counting the rest is worth its own investigation and is not attempted here.
- **Merge-queue throughput.** Unrelated to this bead, tracked under hq-ryrh and
  hq-7sejs.

## Open risk

If `gt-wisp-6ups` is rejected rather than merged, the enumeration half returns to
scope and this design becomes part two of two. Nothing else in this document
changes; the ordering does.
