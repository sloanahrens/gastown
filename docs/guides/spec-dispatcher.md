# Spec dispatcher

The spec dispatcher (gt-4k3fj.5) turns a spec bead into a running polecat
without an operator or mayor in the loop. A spec is a work bead written against
the D10 template (`~/.claude/docs/agents/spec-template.md`), usually by
`/workorder`.

## Shape is a property of every work bead

The lint checks shape, not bookkeeping (gt-mmsr2). It reads the five `## `
sections and the acceptance list of a task, bug or feature the same way; a
bead's type and labels do not decide whether it is checked.

The label `spec` is **retired: accepted and ignored** (gt-mmsr2). Leave it off
new beads; a filer who adds it by habit changes nothing. The spec-template doc
(`~/.claude/docs/agents/spec-template.md`) and `gt spec lint --help` say the
same.

## `gt spec lint <id>`

Checks one work bead the way the dispatcher does before it allocates a seat,
and prints one line naming the first failure. `--json` prints
`{id, ok, needs_planning, refusals[]}` instead, listing **every** shape failure
in check order, so a shell caller can act on all of them at once.

| Check | Refusal names |
|---|---|
| the bead is work, not an epic, an agent bead, a wisp or another runtime family | `not a work bead` |
| every `## ` section in the template, present and non-empty (Goal, Constraints, Out of scope, Gate, Size) | `## <section>` |
| 1-6 acceptance items (3-6 preferred) | `acceptance` |
| Size says one worker, one MR | `size` |

A non-work bead is refused before any shape is read: no sections are required
of an epic, and the reason names what the bead is (`type epic`, `label
gt:agent`, `type wisp`).

Exit 0 means clean. Exit 1 means refused. Exit 2 means the spec needs planning:
label `needs-planning`, a Size that says planning, or more than six
acceptance items. `--json` does not change the exit codes.

The template path is daemon.json `patrols.spec_dispatch.template`, else the
file above, else the five sections built in. `/workorder` runs `gt spec lint <id>` after filing when
`gt spec lint --help` succeeds, so both read one shape.

## `gt spec dispatch`

One tick. The daemon runs it with `--json` when the ticker is on; `--dry-run`
decides and reports without slinging, labeling or commenting.

1. The operator hold file (`<town>/seat-refill.hold`) or ESTOP stops the tick.
2. Candidates are ready, open, unassigned **task, bug or feature** beads at or
   above `polecat_pool.max_priority` (default 2), in every rig that is not
   parked or docked. Epics, agent beads, wisps, the other runtime families and
   the non-work types (`chore`, `docs`, ...) are never candidates, and the
   retired label `spec` and type `feature` are not required — the candidate
   filter does not read them (gt-mmsr2). Beads labeled `gt:ready-to-land`,
   `needs-human`, `needs-mayor-review` or `spec-dispatch-failed`, or deferred,
   are skipped. Order is priority, then created_at, then id.
3. Each candidate is linted, and `polecat_pool.shape_gate` decides what the
   verdict does:
   - `off` runs no lint at all;
   - `warn` (the default) holds a bead the lint refuses: it is skipped with the
     reason `unshaped: <fields>`, spends no seat, and wears one `SHAPE: ...`
     comment, never repeated for the same verdict. A bead labeled
     `spec-shape-waived` waives the lint and is slung anyway;
   - `refuse` also holds it, and labels it `needs-shape` (or `needs-planning`
     when that is the verdict) with one comment. Nothing is spawned for a bead
     that needs planning (the planner, gt-4k3fj.7, is not built yet).
4. A spec that clears the shape gate takes the first free seat and is slung
   through the shared rig-dispatch path in-process, with no auto-convoy.

## A red main defers to its revert

A bead the red-main owner filed (label `red-main`) is held while that owner is
undoing the breakage it was filed for. Otherwise the dispatcher spends a seat
on the fix forward and the two race over the same package (gt-zkdwt).

The owner records the revert it is building or has queued in the rig's red-main
state file, `.runtime/red-main/<rig>.json`, under `revert` (`culprit`, and the
revert bead once it is filed). While that is set, that rig's `red-main` beads
are skipped and stay ready; every other bead in the rig still dispatches. The
hold is per rig and per label, and it lifts — so the very next tick takes the
bead again — when the revert lands, when it is rejected, or when the owner
never filed one. A state that is missing, malformed or has no `revert` key is
no revert at all: reading silence as a hold would keep a rig's red-main beads
from a seat for good.

## A recovered bead resumes its branch

The patrol tick readies a dead polecat's bead with the branch its unlanded work
survives on, in the bead's notes (gt-gzhin.2). The dispatcher reads the newest
`resume_branch: <branch>` line and slings the bead onto that branch — the
equivalent of `gt sling --branch <branch>` — so reopened work continues where
it stopped instead of a second polecat starting from main over it. The tick's
dispatched line names the resumed branch.

A recorded branch that no longer exists on the rig's origin cannot be resumed:
the bead is slung fresh and one comment records the branch that was gone. A
remote that cannot be read is unknown, not gone — the candidate waits for the
next tick rather than handing preserved work to a fresh start (ADR 0005).

## Seats and budget

A seat is one agent with its own cap. Every agent runs the claude CLI with the
town's managed settings and guard hooks, so no seat is less guarded than
another.

Seats, in order:

| Seat | Cap | Takes |
|---|---|---|
| `polecat_pool.overflow_agent` | `polecat_pool.max_overflow`, 2 when unset | every candidate |
| `polecat_pool.pro_agent` | `polecat_pool.pro_max`, 0 drops the seat | only beads carrying `polecat_pool.pro_label` |

The pro seat is reserved: a bead carrying `pro_label` is slung only there, and
the overflow seat leaves it alone. A `spec_dispatch.max_hooked > 0` adds the
`hooked_agent` seat (default `claude-sonnet`) with that cap — off by default,
because a seat nobody asked for is a seat the dispatcher must not spend;
`prefer_hooked` moves it first.

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
retries on its own. Pool and merge-queue refusals are skips, and so is a
content-overlap refusal: the guard refuses only live work, so the refusal
clears by itself when the overlapping bead closes and the next tick takes the
bead again with no label for anyone to remove (gt-q6zoo). Any other failure
leaves the bead unassigned, adds `spec-dispatch-failed` and one comment saying
the bead is out of the queue until the label is cleared. Remove the label to
retry.

## Turning the ticker off

The ticker is on by default (gt-1gnq9): the seat-refill plugin is gone, so it
is the only thing that fills a free seat from ready work, and a town that
dispatches nothing until an operator switches it on is the failure it exists to
prevent.

To park it, set `patrols.spec_dispatch.enabled` false in `mayor/daemon.json` and
restart the daemon. The operator hold file (`<town>/seat-refill.hold`) and ESTOP
already stop the tick without a restart. The seat budget comes from
`polecat_pool`, not from this block; `max_hooked` is the only seat key here, and
it is opt-in.

`gt status --line` and the health tick report `dispatch=off[R]` while the
ticker is off, `dispatch=stalled[R]` when every tick in its ten-minute window
saw candidates waiting and a free seat, slung none and turned none away on
purpose, and `dispatch=silent[R]` when the daemon has been up longer than the
window with the ticker on and no tick landed in it, so a town with no working
dispatcher says so from the one signal an operator reads. A tick that
refused, replanned or skipped its candidates was working, not stalled. A tick
whose roster the daemon cannot read is `dispatch[?]`: an unreadable roster is
not a full town (gt-xiw7o).

The field counts the ready beads carrying `spec-dispatch-failed` as well, so a
bead a failed dispatch left out of the queue is visible even while the
dispatcher itself is healthy: `dispatch=ok_(3_failed)[R]` is three beads
waiting for someone to clear the label (gt-q6zoo). The count is the newest
tick's, so it comes off the line on the next tick after the label is cleared;
it rides whichever state the field already reads (`off_(2_failed)`,
`stalled_(1_failed)`), and a `dispatch[?]` or `dispatch=silent[R]` field shows
it in the detail only — a field with no answer, or no tick in the window, has
no count to show on the line.

The count is read from the tick's own ready board, not a second query: the
`bd ready --json` the dispatcher runs hydrates each issue's `labels` array
(verified against a live board, gt-q6zoo), so the beads the label holds are in
the same snapshot as the candidates and the tick counts them in one pass.

## seat-refill

The plugin this ticker replaced is deleted (gt-4k3fj.8.8): the ticker is the
town's automatic dispatch path, and its policy is the same `polecat_pool` keys
the plugin read. Deleting the directory from the repo does not remove the
runtime copy at `<town>/plugins` — `make install` runs `gt plugin sync`, which
copies and updates but does not remove. After this lands, run `gt plugin sync
--clean` once (or delete `<town>/plugins/seat-refill` by hand) or the deleted
plugin keeps dispatching beside the ticker.

The ticker counts every polecat, whoever slung it, so it never exceeds a cap
a mayor sling already filled. It skips the tick instead.
