+++
name = "seat-refill"
description = "Slings the top eligible ready bead into an empty polecat pool seat (mayor-free); mode nudge restores the mayor nudge"
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

Fills an empty polecat seat by slinging the best eligible ready bead into it
(see Direct dispatch below); with `polecat_pool.mode` set to `nudge` it instead
nudges the mayor when a seat has been empty for five minutes and there is work a
sling could take. The daemon heartbeat runs `run.sh` in-process; no agent reads this
file. A nonzero exit is logged and escalated by the daemon.

## Direct dispatch (default, gt-qvs0b)

With no mayor running, nothing else fills an empty seat, so the default mode
slings. Each run, for every empty seat, it runs
`gt sling <bead> <rig> --agent <seat agent>` for the best candidate (lowest
priority number, then id), one bead per free slot, never the same bead twice.
`gt sling` still enforces its own refusals (backpressure, hold, rig estop); a
refusal (`sling refused:`, `refusing to re-sling`) is logged and is not a plugin
failure; any other sling failure, including a timeout, is an error, and if every
sling in a run fails the run exits nonzero (escalates) and names each bead and
why. Every `gt` call, slings included, is bounded under one run budget inside the
plugin's 3m timeout (gt-d6rse). A seat fills at once unless
`polecat_pool.dispatch_empty_seconds` says otherwise. The pro seat takes only
`pro_label` beads (default `needs-pro`), and the other seats leave those alone.

Every bead the pro seat dispatches is recorded as it is slung: a log line plus
one low-severity escalation naming the bead (source `plugin:seat-refill`, keyed
on the bead id and the dispatch time). A record that cannot be written logs a
warning and never fails the dispatch, which has already happened.

Candidates skip: `gt:ready-to-land` and `gt:needs-human` beads, `in_progress`
or assigned beads (crew included), epics, molecules and agent beads (type
whitelist), the `operator` label, and any bead named by a live polecat session
or an in-flight pool claim (gt-inu1y).

Each candidate is shape-linted just before its sling
(`gt spec lint <id> --json`, gt-mmsr2), because a vague bead is the main cause
of om rejections. `polecat_pool.shape_gate` picks the response. `off` runs no
lint. `warn` (the default, the observe-first half of the rollout) slings the
bead anyway and leaves the verdict on it as one comment naming the refusals
(`SHAPE: <refusals>`, never repeated for the same verdict). `refuse` skips the
bead, labels it `needs-shape` — or `needs-planning` when that is the verdict —
and moves on to the next candidate in the same run. A lint that cannot be read
is not a clean verdict: `warn` slings and logs it, `refuse` skips the bead and
fails the run. A labeled bead stays a candidate, so reshaping it is what puts
it back within reach. Nudge mode runs no lint; the mayor decides there.

`GT_SEAT_REFILL_DRY_RUN=1` logs `DRY-RUN: would sling ...` and neither slings
nor writes state. The hold file, ESTOP, parked rigs and seat caps apply in both
modes; `polecat_pool.mode` `nudge` restores the mayor nudge described below,
and skips the run when `gt mayor status --running` reports no mayor.

## Why

The mayor is event-driven. Declining to dispatch opens no slot, so the
`SLOT_OPEN` that would wake it never arrives, and a seat that empties while it
sleeps stays empty. gt-59o9 measured the result: 5.5 h idle with 362 beads in
`bd ready`, ended by an operator typing into the pane.

The daemon's `mayor_dispatch` patrol closed the coarse half of that hole — one
seat number for the whole pool, every 30m (gt-59o9). This plugin is the
per-seat half. It watches each seat separately, so a seat that empties between
patrols is named within one, with the beads that could fill it attached.

Refilling is a noticing problem: the nudge names an empty seat and work that
exists, and the mayor decides whether to sling it, which bead, and on which
model.

## What counts as a seat

A seat is one capped agent class, read from `polecat_pool` in the town
settings:

- `local` — `local_agent`, capped at `max_local`.
- `overflow` — `overflow_agent`, capped at `max_overflow` when set. An
  unset or zero `max_overflow` leaves the overflow seat **uncapped**, so it is
  never empty and never watched.
- `pro` — `pro_agent`, capped at `pro_max`, taking only beads carrying
  `pro_label`. The pool has no tier for this class: only an explicit
  `gt sling --agent <pro>` reaches it (gt-xmsqb). `pro_max` 0 drops the seat.

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

Two further exclusions, both shared with the daemon's dispatch check (gt-59o9):
notification envelopes (`STATE_COLLAPSE`, an escalation, a `main_branch_test:`
diagnosis) are titles *about* work; and a parked rig is not a dispatch target,
so its backlog is not a reason to nudge.

The priority ceiling is `max_priority` (default P2): a nudge that leads with a
P3 backlog is a nudge the mayor learns to ignore. The types are narrower than
the board — `docs` and `chore` beads are real work but are not in the bead's
list, and the mayor's own patrol still surfaces them.

The `pro` seat is the exception: its emptiness is only news when work *asks*
for it, so it fires only on a bead carrying `pro_label` (gt-tq6l). Without one,
an empty pro seat is the resting state.

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

Every firing goes to the mayor as one `gt nudge` in its default wait-idle mode —
the same path the daemon's patrol uses, honoring the mayor's DND — naming each
empty seat, how long it has been empty, its live/cap count, and the top three
candidates by priority.

## Stopping it

**An operator hold.** Create a file at `<town-root>/seat-refill.hold` and the
plugin stops firing; delete it to resume. The gate stays untouched, so the hold
is visible in `gt plugin history` as skipped runs rather than as a plugin that
went quiet. A town-wide or per-rig `ESTOP` is respected the same way.

The file is also the town's automatic-dispatch hold: the convoy feeders,
`gt scheduler run` and the `scheduled_slings` patrol refuse to sling while it
exists (`internal/dispatch`), so an operator hold parks more than this plugin.
A rig's `ESTOP.<rig>` holds only that rig's dispatch.

**Disabling the gate.** A cooldown gate runs whenever the cooldown has elapsed;
to stop the plugin for longer than a hold, change this file's gate type to
`manual` and run `gt plugin sync`.

Before changing what fires, read "Seat Refill" in `docs/reference.md`: the seats
and thresholds are `polecat_pool` keys there, and a `GT_SEAT_REFILL_*` variable
overrides the file for `run_test.sh` alone.

## Boundaries

In nudge mode it never slings, assigns or reorders work. It does not read the
merge queue, so it can name a rig whose queue is deeper than the dispatch rule
allows; `gt sling`'s backpressure guard refuses that dispatch.

A seat count it cannot read is never reported as zero. If `gt polecat list`
fails, the run fails loudly rather than nudging about a seat that may be
occupied. A single rig whose `gt ready` read fails is skipped with a log line
and no nudge, which is a real blind spot — it is logged in the run receipt and
in `daemon/plugin-runs/seat-refill.log`.
