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
| every repo file the Constraints names as a file it changes exists on the target branch | `## Constraints` |
| Size says one worker, one MR | `size` |

A non-work bead is refused before any shape is read: no sections are required
of an epic, and the reason names what the bead is (`type epic`, `label
gt:agent`, `type wisp`).

The path check reads its paths from the Constraints section (gt-7t1im), and
checks only the tokens that read as repo files — a slash-separated path whose
last segment carries an extension — so a directory, a package, a command, a
symbol, a glob and a URL are passed rather than guessed at. Two cues pass a
path the branch need not hold, and each reads the words directly beside the
path, on its own line, never a cue elsewhere on the line:

- a path the spec adds: "new file X", "adds X", "add X", "create X", or
  "X does not exist yet" / "X will be created" in the Goal or the Constraints;
- a path the spec only reads: "see X", "per X", "as in X", "like X", or "X is
  read-only", where every mention of it in the Constraints carries such a cue.
  A path the spec also changes, without a cue, is still checked.

A bead naming a file with no inventory supplied is refused, never passed: a
check that did not run is not a check that passed.

The inventory is the branch the rig's landings target, read from the rig's
`config.json` `default_branch` and `main` when that key is unset
(`land.RigDefaultBranch`). Of that branch, `origin/<branch>` is read when the
rig's repo has it and the local `<branch>` only when origin has none: the local
branch is one a worktree keeps up to date by hand, and it can lag the branch the
landing worker targets. A branch name git would read as something other than one
branch — a leading `-`, a `..` range — is held by name. A rig, a branch or a
git read that fails holds a bead naming a path with the error named, never as a
branch holding no files.

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
   skipped: labeled `gt:ready-to-land`, `spec-dispatch-failed`, `crew-only`,
   `split`, `overseer-only` (the overseer vets or releases it first) or
   `operator-only` (only the operator can settle it), or carrying a dispatch
   hold. The old `human-only` label still holds the same way until it is
   retired. Before writing or reading a hold marker, read
   [Dispatch holds](../concepts/dispatch-holds.md): it lists them and where each
   is written. The beads the `spec-dispatch-failed`, `crew-only`,
   `overseer-only` and `operator-only` labels hold ride the board so the tick
   names them in its skip line. Order is priority, then created_at, then id.

   `split` is the positive signal recorded at split time by whoever cuts a bead
   into sibling beads: the parent's scope now lives on them, so it is never
   slung, even after every child closes (hq-et8bc). The ready query filters it,
   so no skip line names one. A bead that merely has children is not split —
   the container rule skips it only while a child is open and releases it when
   the last one closes, and with no label it is dispatched as the candidate it
   was (gt-gektq). `needs-split` is a different label: the overrun guard's
   hand-back, a dispatch hold a person settles by cutting the bead smaller and
   removing it.
3. A candidate with an unresolved blocking dependency is skipped with the
   reason `blocked by <blocker> (<status>)`, even when the ready board listed
   it: bd's ready query reads a deferred blocker as resolved and the dispatcher
   does not (gt-ndad8). The edges come from the tick's one batched read per rig,
   so the check costs no per-candidate read, and a candidate whose edges could
   not be read waits for the next tick rather than dispatching blind.
4. Each candidate is linted, and `polecat_pool.shape_gate` decides what the
   verdict does:
   - `off` holds and labels nothing the lint refuses (the verdict is still read
     for the overrun-stop flag below);
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
5. A spec that clears the shape gate takes the first free seat and is slung
   through the shared rig-dispatch path in-process, with no auto-convoy. Content
   overlap does not hold a candidate. A candidate that names a path or a test
   another bead names is slung like any other, including one a polecat seat
   already has in flight, and the tick says nothing about a hold for it
   (gt-e1ufo). Landings are serial per rig and rebase, so a conflict costs a
   bounce, which is cheaper than an idle seat. The sling-time guard still runs
   for a dispatch and still prints its warning, but it admits the overlap
   rather than refusing it (`sling.Options.AdmitOverlap`). A manual `gt sling`
   keeps the refusal, and `--force` overrides it.
6. A bead that takes a seat and matches the overrun-stop box is slung anyway
   and wears one comment saying so (see below).

## The verdict ledger

Every real tick records each candidate it passed over, and why, in
`<town>/.runtime/spec_dispatch/verdicts.json` (gt-8thox). One entry per bead:
the class — `skipped`, `refused`, `planning`, `failed` or `deferred` — the
reason line the tick printed for it, `first_at` and `last_at`, and `ticks`,
the number of consecutive ticks the verdict has stood. A tick can pass over a
bead for more than one reason and list it under several of those classes; the
ledger keeps a single entry, under the class named last in that list, so a bead
both `skipped` and `deferred` is recorded as `deferred`. Before changing which
class wins, read `specTickVerdicts` (`internal/cmd/spec.go`), which fixes that
order. A bead the next tick does not pass over is dropped, because the file
states what the last tick saw.

A `--dry-run` records nothing: it decides without acting, and its reasons
would overwrite a real tick's. A ledger the tick cannot read is left as it is
rather than recorded onto empty, which would reset the first-seen time of every
verdict in it; a read or write that fails is one warning line on stderr and
does not fail the tick.

The dashboard's ready list reads the ledger, so an operator learns why a
well-formed, unassigned bead is waiting without opening the daemon log. A row
the ledger holds gains the verdict's fields: the dim line under it names the
class, the time the verdict was first seen, how many ticks it has stood and the
tick's reason, and the ready list's `skipped by dispatcher` chip counts those
rows and filters to them. A row the lint called well formed, but which the
ledger classes `skipped`, `refused` or `deferred`, stops reading ready and
wears the `skipped by dispatcher` tag instead; a `planning` or `failed` entry
fills the fields and leaves the row's shape as the lint gave it.

A ledger older than five minutes, or one the page cannot read, is stale, and
the page presents no verdict from it: no chip, tag or row line claims a skip,
and one note above the rows gives the ledger's age or names the failed read.
A held or stopped dispatcher stops writing, so its last tick's verdicts read
as current would be a claim nothing backs (gt-ckncx).

## The overrun-stop flag

Some bead shapes ran past the overrun guard more often than others on the rig
the box was measured on (gt-b9j3o.4). The dispatcher therefore notes a
candidate that matches that box as it slings it: the comment names the seat's
model and the rig's review threshold, the two facts of the box the bead itself
does not show.

The flag is a note, not a hold. A matching bead is dispatched exactly as it
would have been: no seat is withheld, nothing is labeled, and the bead's
priority and order are unchanged. A note is written only when the sling
dispatches the bead, so a bead whose sling deferred carries no flag.

Before changing the box, read `specdispatch.InOverrunBox`
(`internal/specdispatch/overrun.go`): the facts it reads, the band the rig's
`.om.json` threshold has to fall in, and the seat model are defined there and
nowhere else. A rig with no `.om.json`, or one whose threshold falls outside the
measured band, is outside the box. A threshold that cannot be read is not a
threshold of 0: the tick reports the failed read once for the rig and flags
nothing that tick.

The threshold is the rig checkout's own `.om.json` — the tree its landings are
reviewed from — not the review config of a landing in flight.

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
entry takes no `agent`: the review is an ordinary bead and draws a seat from the
[seat map](#seats-and-budget) like any other (gt-mukgk). The config load refuses
an `agent` on a spec entry, and the error names the replacement. To keep a
scheduled job on one model (deep reviews never run on DeepSeek, gt-co54d), set
`"run_agent": "<agent>"` on the entry: the patrol labels each run bead
`agent:<agent>`, so only that agent's seats take it (gt-tplg6). The agent needs a
seat in `polecat_pool.seats`; the config load refuses an entry without one, and
the patrol fails the tick rather than create a bead no seat can take. It also
refuses `run_agent` on an entry whose `dispatch` is not `"spec"`, which the
patrol could only skip on every tick (gt-nosod). The open
bead is the queue: the entry's skip-open rule holds the next run, and the patrol
never retires it as stalled however long a full pool keeps it waiting. The bead's
priority is the entry's `priority` (3 when unset), held at
`polecat_pool.max_priority`, so a default entry is created at P2 and the
dispatcher accepts it. A run still open and unhooked after the entry's `interval`
(at least 6 hours) counts as a failed tick, so the third in a row escalates
instead of the entry skipping the open run forever.

The overrun guard times a bead, not a seat. Its clock is the bead's first
dispatch since the bead's last run end — the seat's `gt done`, the guard's own
stop, or a hand close through `gt bead close` — so a bead handed from one seat to
another keeps the dispatch it first went out on and cannot restart its clock by
moving, and a bead whose work was finished, closed or stopped starts over when it
is dispatched again. A landing needs no end of its own: it follows the seat's own
`gt done`, whose event ends the run first. The dashboard's polecat Age is this
same clock, labelled as the bead's dispatch time (gt-tzbtf).

An overrun stop records that run end on the bead (`overrun_stop`, stamped with
the session stop's time) before it hands the bead back, so a dispatch after a
stop is a new run judged on its own 12 and 20 minute rule. The record is written
once: a stop retried on later ticks — while a hand-back keeps failing — reuses
the saved end instead of writing a later one each time, which would creep past
the dispatch it ends. It is written only while the bead is still the stopped
seat's, so a bead closed or handed to another polecat in the meantime keeps that
holder's clock.

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

The seat map, `polecat_pool.seats` in `settings/config.json`, is the one place
that says which models run, how many of each and when. It holds any number of
agents. Every reader goes through one loader, `config.Seats`, so the dispatcher,
`gt sling`, the roster and `gt pool show` cannot disagree about it (gt-mukgk).

Each key of `seats` is an agent name, and each agent has one seat; a `cap` of 0
closes it. The agent's command, arguments and environment stay in the `agents`
map; the built-in presets stand in for a name the town does not define. Every
agent runs the claude CLI with the town's managed settings and guard hooks, so
no seat is less guarded than another.

`polecat_pool.max_total`, when set, caps live polecats across every seat: a
seat's cap is per agent, so seats each inside their own cap can still run more
polecats than the town wants. A town at its total is skipped whatever room the
seats show. Absent means no total cap.

The loader refuses, with a message naming the key, an agent that is neither in
`agents` nor a preset, a negative cap, a `closed` window that does not parse or
has no valid `timezone`, and a name listed in two seats. A tick that meets a
rejected map fails with `reading the seat map: <loader message>` and dispatches
nothing, and the daemon refuses to start on one. It never runs on part of the
list.

### Choosing a seat

A bead with no `agent:<name>` label draws uniformly at random among the agents
with a free, open seat. The draw is per agent, not per free slot, so a seat with
a small cap still gets work and list order prefers no model. A bead labeled
`agent:<name>` draws only that agent's seat. A seat marked `"pinned_only": true`
in `polecat_pool.seats` is never drawn by an unlabelled bead, only by a bead
labeled for its agent, within its cap and window; an unlabelled bead facing only
such seats skips with `seats full`. When the seat is full the tick
reports `waiting for a <agent> seat`; when it is inside its window, `waiting for
a <agent> seat, closed until 08:00`; and an agent with no seat in the map is a
louder skip, `pinned to <agent>, which has no seat in the pool`. None of them
falls back to another agent's seat, so work approved for one model cannot land
on a cheaper one because that seat is free. A full town is skipped with `seats
full`, and the bead stays ready for the next tick.

### Closed windows

A seat inside its `closed` window stays in the map and still counts the
polecats it already has, but takes no new spawn. The window repeats daily and is
read at the tick's clock in `polecat_pool.timezone`. The start is inclusive and
the end exclusive, and an end before the start wraps past midnight, so
`20:00-08:00` is open at 19:59 and 08:00, closed at 20:00 and 07:59. `gt sling` honors the same window.

### Occupancy

Occupancy is every live polecat session, the seats whose session is gone while
their work is still assigned (a crashed polecat waiting for its supervised
restart) or still landing, and the seat claims in-flight slings hold. Each is
counted on the agent its seat was spawned on, which the agent bead records, not
on a role default. A polecat that records no agent fills no seat but counts
toward `max_total`, and the tick names it: `roster: N polecat(s) record no agent
and count under "(unrecorded)", on no seat: <names>`.
`polecat_pool.min_spawn_gap` applies to the newest polecat on any seat. A tick
slings as many beads as there are free seats, unless `max_per_tick` sets a
lower ceiling. A full seat is skipped, never overfilled.

### Names

A name drawn for a seat is bound to that seat's agent and is never handed to
another (gt-m9dty). A seat with a `names` list draws from it first; a seat with
no list, or whose list is spent, draws from the themed default list. A name
bound before a list named it keeps its binding.

### `gt pool`

Change seats with `gt pool`, not by editing the file. `gt pool set` validates
the whole resulting map before it writes, so an unknown agent or a bad window
writes nothing. `gt pool --help` lists the verbs.

### Taking effect

Every seat key is read on every tick, so a seat added, moved or closed applies
at the next tick: no daemon restart and no kickstart, which cut the tick then in
flight the last time one was run (gt-jdnab). The daemon logs `spec_dispatch:
seats now ...` on the tick that reads a map different from the last one, and a
seat file that does not parse parks the ticker with one line per distinct error,
leaving the seats in force the ones the last good read named rather than ticking
with a seat silently dropped. The patrol's `enabled` key is still read at daemon
start, so turning the ticker off and on again is the one key here that needs a
restart.

## Host safety

Every dispatch carries sling args telling the polecat that install and
uninstall tests use a temporary `INSTALL_DIR` and never the real
`~/.local/bin`. The 2026-09-30 04:14 host wipe came from a guard blind spot,
filed as gt-tt8sg.

## Failures

A Dolt serialization failure (Error 1213) retries the whole dispatch up to
four times with jittered backoff; the bond step inside the sling already
retries on its own. Contention that outlasts the last retry is a skip, not a
failure: Dolt rolled the attempt back before reporting it, so the next tick
repeats the dispatch with no label for anyone to remove (gt-9x16z). Pool and
merge-queue refusals are skips. A content overlap is never a refusal on the
dispatch path (gt-e1ufo), so it defers nothing. A skip also hands back the seat the tick charged
for the candidate, so a deferral does not leave the tick counting a seat no
polecat took. Any other failure
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
The seat budget comes from `polecat_pool.seats`, not from this block.

`gt status --line` and the health tick report `dispatch=off[R]` while the
ticker is off, `dispatch=stalled[R]` when every tick in its ten-minute window
saw candidates waiting and a free seat, slung none and turned none away on
purpose, and `dispatch=silent[R]` when the daemon has been up longer than the
window with the ticker on and no tick landed in it, so a town with no working
dispatcher says so from the one signal an operator reads. A tick that
refused, replanned or skipped its candidates was working, not stalled. A tick
whose roster the daemon cannot read is `dispatch[?]`: an unreadable roster is
not a full town (gt-xiw7o). A seat file that does not parse parks the ticker
outright, and it reads `dispatch=parked[R]` with the read error as its detail:
the ticker runs no tick while parked, so without the reason it would look
silent (gt-jdnab).

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
