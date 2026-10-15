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
new beads; a filer who adds it by habit changes nothing.

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

The required `## ` sections are the template file's when that file exists:
`daemon.patrols.spec_dispatch.template` in `settings/config.json`, else
`~/.claude/docs/agents/spec-template.md`. With no template file, or one
carrying no `## ` heading, the built-in Goal, Constraints, Out of scope, Gate
and Size are checked instead.

## `gt spec dispatch`

One tick. The daemon runs it with `--json` when the ticker is on; `--dry-run`
decides and reports without slinging, labeling or commenting.

1. The operator hold file (`<town>/seat-refill.hold`) or ESTOP stops the tick.
2. Candidates are ready, open, unassigned **task, bug or feature** beads at or
   above `polecat_pool.max_priority` (default 2), in every rig that is not
   parked or docked. Epics, agent beads, wisps, the other runtime families and
   the non-work types (`chore`, `docs`, ...) are never candidates, and the
   retired label `spec` and type `feature` are not required — the candidate
   filter does not read them (gt-mmsr2). A bead held off the dispatch path is
   skipped: labeled `gt:ready-to-land`, `spec-dispatch-failed`, or `crew-only`
   (a person hand-labels a bead only they land),
   or carrying a dispatch hold. Before writing or reading a hold marker, read
   [Dispatch holds](../concepts/dispatch-holds.md): it lists them and where each
   is written. The beads the `spec-dispatch-failed` and `crew-only` labels hold
   ride the board so the tick names them in its skip line. Order is priority,
   then created_at, then id.
3. Each candidate is linted, and `polecat_pool.shape_gate` decides what the
   verdict does:
   - `off` runs no lint at all;
   - `warn` (the default) holds a bead the lint refuses: it is skipped with the
     reason `unshaped: <fields>`, spends no seat, and wears one `SHAPE: ...`
     comment, never repeated for the same verdict. It also holds a bead that
     needs planning — nothing is slung for it, it wears the `needs-planning`
     label, and the tick reports its planning state, `needs plan` while it
     waits for a proposal or `plan proposed` once a plan job has written its
     `PLAN PROPOSAL` block for the operator to file (gt-4k3fj.14, gt-tod3q).
     A bead labeled `spec-shape-waived` waives the lint and is slung anyway;
   - `refuse` holds a refused bead the same way and labels it `needs-shape`;
     a bead that needs planning is held exactly as under `warn` (gt-tod3q).
     Under either gate the tick spawns no planner (gt-4rizh), so a held bead
     waits for the operator.
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

## A bead can declare its formula

A bead labeled `formula:<name>` is slung with that formula instead of the
rig's default, through the same seat choice, holds and caps as any other bead
(gt-j5cr2). Its variables are the lines of a `## Formula vars` section in the
description, one `key=value` per line up to the next `## ` heading. Blank
lines and lines starting with `#` are ignored, and a repeated key keeps its
last value.

The tick skips, and does not label failed, a bead whose declaration it cannot
run: `bad formula vars: <line>` for a line without `=` or with an empty key,
`bad formula label: formula:` or `conflicting formula labels: a, b` for the
label, and `unknown formula: <name>` when the rig has no such formula. The
bead stays ready, so fixing the bead clears the skip. A bead that also wears
`scheduled:<name>` is slung with the actor `daemon/scheduled:<name>`, so
usage attribution and the overrun guard's scheduled-run limit still see a
scheduled run. `gt spec dispatch --dry-run` and the dispatched line print
`, formula <name>` after the seat.

A scheduled sling entry with `"dispatch": "spec"` in `daemon.patrols.scheduled_slings` (`settings/config.json`) hands its due
run to this dispatcher (gt-z5g3c): the `scheduled_slings` patrol creates one
shaped, report-only bead labeled `scheduled:<name>` and `formula:<formula>`, with
the entry's `vars` sorted in a `## Formula vars` section, and does not sling. The
entry may name an `agent` (an agent from `agents`, e.g. `claude-sonnet`): the
patrol then also writes an `agent:<name>` label on the bead, and the dispatcher
slings it only onto a seat of that agent. A full seat is a wait (`waiting for a
<agent> seat`), an agent with no seat in the pool is a louder skip, and neither
ever falls back to another agent's seat, so a review approved for a Claude model
cannot land on a cheaper seat because that seat is free. With no `agent` the
dispatcher picks any seat, as before. The open
bead is the queue: the entry's skip-open rule holds the next run, and the patrol
never retires it as stalled however long a full pool keeps it waiting. The bead's
priority is the entry's `priority` (3 when unset), held at
`polecat_pool.max_priority`, so a default entry is created at P2 and the
dispatcher accepts it. A run still open and unhooked after the entry's `interval`
(at least 6 hours) counts as a failed tick, so the third in a row escalates
instead of the entry skipping the open run forever.

The overrun guard (`daemon.patrols.overrun_guard`) does not apply its 12 and 20
minute rule to a seat on a `scheduled:` bead, because a deep review runs long by
design, but it does not leave one unbounded. `scheduled_stop_after` (a duration
such as `"90m"`, default `60m`) is the only limit on such a seat, counted from the
sling. Past it the guard stops the session with the same deliberate-stop record
`gt session stop` writes, so `patrol_scan` and the zombie detector leave the seat
down, logs `overrun_guard: stopped scheduled <rig>/<polecat> on <bead> after
<elapsed>` and adds one comment to the bead naming the limit. It then closes the
run bead (clearing its assignee first) and releases the seat the way an ordinary
overrun stop does: reset the agent bead, record the cleanup status, retire the
seat. It does not hand the bead back as work, label it `needs-split`, checkpoint
or push: a scheduled run is report-only, and the `scheduled_slings` patrol
creates the next one on the entry's own interval and gate (a run left hooked
would pin the seat against `max_total` and keep the entry in skip-open). A step
that fails is retried on the next tick without repeating the comment or the
close; a close that fails five times leaves the seat paused and the bead hooked
for an operator, as before. `"0"` never
stops a scheduled seat, which is how the guard behaved before the limit; a
negative or unparseable value is refused when the config loads. Add the key to
`settings/config.json` only after the running `gt` binary knows it, because the
strict parser parks every rig on an unknown key.

## Seats and budget

A seat is one agent with its own cap. Every agent runs the claude CLI with the
town's managed settings and guard hooks, so no seat is less guarded than
another.

Seats, in order:

| Seat | Cap | Takes |
|---|---|---|
| `polecat_pool.agent` | `polecat_pool.max_seats`, 2 when unset | every candidate |
| `polecat_pool.pro_agent` | `polecat_pool.pro_max`, 0 drops the seat | only beads carrying `polecat_pool.pro_label` |

The pro seat is reserved: a bead carrying `pro_label` is slung only there, and
the pool's own seat leaves it alone. A `spec_dispatch.max_hooked > 0` adds the
`hooked_agent` seat (default `claude-sonnet`) with that cap — off by default,
because a seat nobody asked for is a seat the dispatcher must not spend;
`prefer_hooked` moves it first.

`polecat_pool.max_total`, when set, caps the seats together: the pool's own cap
is per agent, so two agents each inside their own can still run more polecats
than the town wants. A town at its total is skipped whatever room the seats
show. Absent or 0 means no total cap.

To read or change the pool seat and the `hooked_agent` seat, run `gt pool show`
or `gt pool set <agent> <n>` instead of editing the files: `n` of 0 closes the
seat by removing its keys, where a hand-written `max_seats: 0` would mean
uncapped to sling and the default cap here. `gt pool set-total <n>` sets the
total the same way, 0 removing it. `gt pool set-second <agent> <n>` sets the
`hooked_agent` seat's agent and cap in one write, which `gt pool set` cannot: it
takes only an agent a seat already holds. The `hooked_agent` seat is read at
daemon start, so a change to it needs a daemon kickstart. The dispatcher keeps one
seat per agent: a second seat on the pool's agent never runs, and `gt pool show`
prints it as `not run`.

Occupancy is every live polecat session, the seats whose session is gone while
their work is still assigned (a crashed polecat waiting for its supervised
restart) or still landing, and the seat claims in-flight slings hold, counted
per agent from the session's `GT_AGENT`; `gt pool show` prints that count beside
each seat's cap, naming the seats no session holds. An empty `GT_AGENT`
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
content-overlap refusal: the guard refuses work in flight, and an unstarted peer
only while that peer comes first in dispatch order and the dispatcher would
sling it, so the refusal clears by itself once the peer is slung or closes and
the next tick takes the bead again with no label for anyone to remove
(gt-q6zoo, gt-nlkba, gt-00gee). Any other failure
leaves the bead unassigned, adds `spec-dispatch-failed` and one comment saying
the bead is out of the queue until the label is cleared. Remove the label to
retry.

## Turning the ticker off

The ticker is on by default (gt-1gnq9): the seat-refill plugin is gone, so it
is the only thing that fills a free seat from ready work, and a town that
dispatches nothing until an operator switches it on is the failure it exists to
prevent.

To park it, set `daemon.patrols.spec_dispatch.enabled` false in
`settings/config.json` and restart the daemon. Before
`gt config migrate` (historical layout) that patrol block lived in `mayor/daemon.json`. The
operator hold file (`<town>/seat-refill.hold`) and ESTOP already stop the tick
without a restart.
The seat budget comes from `polecat_pool`, not from this block; `max_hooked` is
the only seat key here, and it is opt-in.

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
