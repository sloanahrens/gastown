# Spec dispatcher

The spec dispatcher (gt-4k3fj.5) turns a spec bead into a running polecat
without an operator or mayor in the loop. A spec is a bead written against the
D10 template (`~/.claude/docs/agents/spec-template.md`), usually by
`/workorder`.

## `gt spec lint <id>`

Checks one bead the way the dispatcher does before it allocates a seat, and
prints one line.

| Check | Refusal names |
|---|---|
| type `feature` | `type` |
| label `spec` | `label spec` |
| every `## ` section in the template, present and non-empty (Goal, Constraints, Out of scope, Gate, Size) | `## <section>` |
| at least one acceptance item (3-6 preferred) | `acceptance` |
| Size says one worker, one MR | `size` |

Exit 0 means clean. Exit 1 means refused. Exit 2 means the spec needs planning:
label `needs-planning`, a Size that says planning, or more than six
acceptance items.

The template path is `$GT_SPEC_TEMPLATE`, else the file above, else the five
sections built in. `/workorder` runs `gt spec lint <id>` after filing when
`gt spec lint --help` succeeds, so both read one shape.

## `gt spec dispatch`

One tick. The daemon runs it with `--json` when the ticker is on; `--dry-run`
decides and reports without slinging, labeling or commenting.

1. The operator hold file (`<town>/seat-refill.hold`) or ESTOP stops the tick.
2. Candidates are ready, open, unassigned beads with label `spec` and type
   `feature` in every rig that is not parked or docked. Beads labeled
   `gt:ready-to-land`, `needs-human`, `needs-mayor-review` or
   `spec-dispatch-failed`, or deferred, are skipped. Order is priority, then
   created_at, then id.
3. Each candidate is linted. A refusal is one line in the report and one
   comment on the bead, never repeated. A spec that needs planning gets the
   `needs-planning` label and one comment; nothing is spawned (the planner,
   gt-4k3fj.7, is not built yet).
4. A clean spec takes the first free seat and is slung through the shared
   rig-dispatch path in-process, with no auto-convoy.

## Seats and budget

A seat is one agent with its own cap. Every agent runs the claude CLI with the
town's managed settings and guard hooks, so no seat is less guarded than
another. The `hooked_*` key names predate that and are kept for compatibility;
`hookless_agent` and `max_hookless` are retired and ignored.

Seats, in order:

| Seat | Cap |
|---|---|
| `polecat_pool.overflow_agent` | `polecat_pool.max_overflow`, 2 when unset |
| `patrols.spec_dispatch.hooked_agent`, default `claude-sonnet` | `max_hooked`, default 2 |

`prefer_hooked` moves the hooked_agent seat first.

Occupancy is every live polecat session plus the seat claims in-flight slings
hold, counted per agent from the session's `GT_AGENT`. An empty `GT_AGENT`
counts as the polecat role default. `polecat_pool.min_spawn_gap` applies to the
newest polecat on any seat. At most `max_per_tick` (default 1) beads are slung
per tick. A full seat is skipped, never overfilled, and the bead stays ready
for the next tick.

## Host safety

Every dispatch carries sling args telling the polecat that install and
uninstall tests use a temporary `INSTALL_DIR` and never the real
`~/.local/bin`. The 2026-09-30 04:14 host wipe came from a guard blind spot,
filed as gt-tt8sg.

## Failures

A Dolt serialization failure (Error 1213) retries the whole dispatch up to
four times with jittered backoff; the bond step inside the sling already
retries on its own. Pool and merge-queue refusals are skips. Any other failure
leaves the bead unassigned, adds `spec-dispatch-failed` and one comment. Remove
the label to retry.

## Enabling the ticker

In `mayor/daemon.json`:

```json
{
  "patrols": {
    "spec_dispatch": {
      "enabled": true,
      "interval": "60s",
      "hooked_agent": "claude-sonnet",
      "max_hooked": 2,
      "max_per_tick": 1
    }
  }
}
```

Then restart the daemon.

## seat-refill

The ticker counts every polecat, whoever slung it, so it never exceeds a cap
that seat-refill nudges or mayor slings already filled. It skips the tick
instead. On 2026-09-30 seat-refill filled four flash seats on its own and the
operator's slings were refused. When the ticker is the town's dispatcher, turn
seat-refill off so the roster is readable: disable the seat-refill plugin. Do not park seat-refill with its hold file, because the
ticker honors that file too and would stop as well.
