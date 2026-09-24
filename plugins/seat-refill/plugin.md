+++
name = "seat-refill"
description = "Nudges the mayor when a polecat pool seat sits empty for 5m with slingable work ready"
version = 1

[gate]
type = "cooldown"
duration = "2m"

[tracking]
labels = ["plugin:seat-refill", "category:dispatch"]
digest = true

[execution]
type = "script"
timeout = "3m"
notify_on_failure = true
severity = "medium"
+++

# Seat Refill

Nudges the mayor when a polecat seat has been empty for five minutes and there
is work a sling could take. The daemon heartbeat runs `run.sh` in-process; no
dog reads this file unless the script exits nonzero.

## Why

The mayor is event-driven. Declining to dispatch opens no slot, so the
`SLOT_OPEN` that would wake it never arrives, and a seat that empties while it
sleeps stays empty. gt-59o9 measured the result: 5.5 h idle with 362 beads in
`bd ready`, ended by an operator typing into the pane.

The daemon's `mayor_dispatch` patrol closed the coarse half of that hole — one
seat number for the whole pool, every 30m (gt-59o9). This plugin is the
per-seat half. It watches each seat separately, so a seat that empties between
patrols is named within one, with the beads that could fill it attached.

Refilling is a noticing problem, not a judgment problem. The nudge carries no
judgment: it names a seat that is empty and work that exists, and the mayor
decides whether to sling it, which bead, and on which model.

## What counts as a seat

A seat is one capped agent class, read from `polecat_pool` in the town
settings:

- `local` — `local_agent`, capped at `max_local`.
- `overflow` — `overflow_agent`, capped at `max_overflow` when set. An
  unset or zero `max_overflow` leaves the overflow seat **uncapped**, so it is
  never empty and never watched.
- `sonnet` — `claude-sonnet`, capped at one. This third class is not
  expressible in `polecat_pool` today: a sonnet polecat reaches a seat only
  through an explicit `gt sling --agent claude-sonnet`, so nothing counts it
  (gt-xmsqb). Until that bead gives the pool N tiers, the mayor's interim
  policy — hold itself to one live sonnet — is modeled here. Set the knob to
  zero when the policy changes.

A `max_local` of zero means the local tier is **closed**, which is a decision
rather than an empty seat, so it is not watched either.

Occupancy comes from the pool's own count: live polecat sessions whose
`GT_AGENT` is the seat's agent, plus the in-flight seat claims a sling writes
before its session exists (gt-t8q5). A nudge that named room the next sling
would refuse is worse than no nudge (gt-59o9), so this deliberately borrows the
pool's accounting instead of inventing a second one.

## What counts as slingable work

A ready bead that is a `task`, `bug`, or `feature`, unassigned, at P0-P2, in a
rig that is operational. Epics, agent and infra beads, and the town's
bookkeeping families are gone before this point — `gt ready` excludes them, and
the type whitelist excludes the rest.

Two further exclusions, both shared with the daemon's dispatch check
(gt-59o9): notification envelopes (`STATE_COLLAPSE`, an escalation, a
`main_branch_test:` diagnosis) are titles *about* work rather than work; and a
parked or docked rig is not a dispatch target at all, so its backlog is not a
reason to nudge.

The priority ceiling is the town directive's: a nudge that leads with a P3
backlog is a nudge the mayor learns to ignore. The types are narrower than the
board — `docs` and `chore` beads are real work but are not in the bead's list,
and the mayor's own patrol still surfaces them.

The `sonnet` seat is the exception: its emptiness is only news when work
*asks* for it, so it fires only on a bead carrying the `needs-sonnet` label
(gt-tq6l). Without one, an empty sonnet seat is the resting state.

## When it fires

Each seat carries an empty episode: the time it was first seen empty, and the
time it was last nudged about.

- A seat observed empty for the first time starts its episode now. The plugin
  measures what it observes; a seat long empty before the plugin was installed
  is nudged about five minutes after the first run, not immediately.
- At five minutes of emptiness **and** at least one candidate bead, it fires —
  once.
- The same episode may fire again every 15 minutes, and no more often.
- A seat that fills, or that stops existing, drops its episode. The next empty
  episode starts a fresh clock.

Episode state lives in `.runtime/seat-refill.json` under the town root. Deleting
it loses only the current episodes; the next run starts new ones.

Every firing goes to the mayor as one nudge naming each empty seat, how long it
has been empty, its live/cap count, and the top three candidate bead ids by
priority. Delivery is `gt nudge` in its default wait-idle mode — the same path
the daemon's own patrol uses: it reaches an idle mayor directly, queues behind a
busy one, and honors the mayor's DND.

## Stopping it

**An operator hold.** Create a file at `<town-root>/seat-refill.hold` and the
plugin stops firing; delete it to resume. The gate stays untouched, so the hold
is visible in `gt plugin history` as skipped runs rather than as a plugin that
went quiet. A town-wide or per-rig `ESTOP` is respected the same way.

The same file is the town's automatic-dispatch hold. The following all refuse
to sling while it exists and log why (`internal/dispatch`):

- the deacon's RECOVERED_BEAD redispatch and its convoy feed dogs
- the gated-molecule step in the deacon patrol
- the daemon's convoy feeders
- the daemon's `gt scheduler run` heartbeat step
- the `scheduled_slings` patrol

`GT_SEAT_REFILL_HOLD` relocates the file for all of them. A rig's
`ESTOP.<rig>` holds only that rig's dispatch. An explicit `gt sling` typed by
the operator or the mayor still works.

**Disabling the gate.** `gt plugin` has no pause; a plugin whose gate is a
cooldown runs whenever that cooldown has elapsed. To stop it for longer than a
hold, change this file's gate type to `manual` and run `gt plugin sync`.

Before changing what fires, read the seat and threshold inputs first: every
input in `run.sh` is overridable by environment variable so `run_test.sh` can
drive them without a town, and those defaults are the live behavior.

## Boundaries

It never slings, never assigns, and never reorders work. It does not read the
merge queue, so it can name a rig whose queue is deeper than the dispatch rule
allows; `gt sling`'s own backpressure guard refuses that dispatch, and the
nudge asks the mayor to decide, not to comply.

A seat count it cannot read is never reported as zero. If `gt polecat list`
fails, the run fails loudly rather than nudging about a seat that may be
occupied. A single rig whose `gt ready` read fails is skipped with a log line
and no nudge, which is a real blind spot — it is logged in the run receipt and
in `daemon/plugin-runs/seat-refill.log`.
