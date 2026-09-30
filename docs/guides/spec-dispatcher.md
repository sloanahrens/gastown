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
4. A clean spec takes a free seat of a class it may use and is slung through
   the shared rig-dispatch path in-process, with no auto-convoy.

## Seat classes and budget

- **hooked**: a Claude-backed agent (provider `claude`, no
  `ANTHROPIC_BASE_URL` override). It gets the town's settings and guard hooks.
  Agent `patrols.spec_dispatch.hooked_agent` (default `claude-sonnet`), cap
  `max_hooked` (default 2).
- **hookless**: every other agent, including a claude CLI pointed at DeepSeek
  or the local model. These run with zero guards until gt-be0z lands. Agent
  `hookless_agent`, else `polecat_pool.overflow_agent`. Cap
  `polecat_pool.max_overflow`, else `max_hookless` (default 2).

Occupancy is every live polecat session plus the seat claims in-flight slings
hold, classified by the session's `GT_AGENT`. `polecat_pool.min_spawn_gap`
applies to the newest polecat of either class. At most `max_per_tick`
(default 1) beads are slung per tick. A full class is a skip, never an
overflow, and the bead stays ready for the next tick.

**Host safety.** A spec whose title, description, notes, design or
acceptance mentions install, uninstall, `~/.local/bin`, `INSTALL_DIR`,
`make install`, `dolt cleanup` or `rm -rf` only goes to a hooked seat. Its
sling args tell the polecat to test install paths in a temporary
`INSTALL_DIR`. With the hooked class full it waits. This rule exists because a
hookless polecat ran the uninstall it was documenting on the host on
2026-09-30.

The dispatcher names its agent explicitly, and that choice outranks the bead's
`route:local` / `route:flash` labels. Every other sling path keeps gt-4lbz,
where the label wins.

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

## seat-refill and idle_fill

The ticker counts every polecat, whoever slung it, so it never exceeds a cap
that seat-refill nudges, mayor slings or the pool's `idle_fill` already
filled. It skips the tick instead. On 2026-09-30 seat-refill plus `idle_fill`
filled four flash seats on their own and the operator's slings were refused.
When the ticker is the town's dispatcher, turn the others off so the roster
is readable: set `polecat_pool.idle_fill` to `false` and disable the
seat-refill plugin. Do not park seat-refill with its hold file, because the
ticker honors that file too and would stop as well.
