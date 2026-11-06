> Status: current (2026-11). The guide a filer reads before writing a work bead: the five required sections, the `## Files` and `## Interfaces` syntax, and the six lessons the gt update epic taught.

# Writing a bead

`gt spec lint <id>` checks one work bead before the dispatcher spends a seat. It
reads the five sections, the acceptance list and the Size, and checks the repo
files the bead names; the bead's type and labels do not decide whether it is
checked. A refusal names the field to fix. For the command, its exit codes and
when the dispatcher holds a bead, read
[Spec dispatcher](spec-dispatcher.md).

## The five sections

| Section | What it is for |
|---|---|
| Goal | What the bead must achieve, and the exact names other beads may rely on. |
| Constraints | The repo files the bead creates and modifies, and the behaviour and limits the work must respect. Without a `## Files` section, the path check reads the files named here. |
| Out of scope | What this bead leaves to another bead, so a reader does not wander into it. |
| Gate | The command that proves this bead's acceptance boxes. |
| Size | `One worker, one landing, about 20 minutes.` The lint reads the words "one worker"; a bead labeled `overseer-only` or `operator-only` may instead say "one session of the overseer" or "one session of the operator". |

The acceptance list carries one to six boxes, three to six preferred, each a
claim a polecat can check alone. An item names what to observe, usually the
test that observes it. The lint refuses an item that names a container or a
live town (gt-hgt9j.3); those steps belong in the Gate. More than six items
route the bead to the planner instead of a seat.

## `## Files`

A bead that changes repo files lists them under the section, one `verb: path`
per line. The verbs are lower case:

```markdown
## Files
create: internal/update/state.go
create: internal/update/state_test.go
modify: internal/specdispatch/lint.go
read: docs/guides/spec-dispatcher.md
```

`create` adds a path and needs nothing on the base branch. `modify` and `read`
name a path the base branch holds, or one an open blocker of this bead creates.
A line that is neither blank nor a heading nor `verb: path` is refused with its
line number and text, so a typo is never read as a missing path. A leading
`- ` bullet and backticks around the path are stripped.

When the section is present it is the only source for the path check. A bead
without it keeps the cue grammar in its Constraints, described in
[Spec dispatcher](spec-dispatcher.md).

## `## Interfaces`

A bead lists the identifiers it defines and the identifiers it uses from the
beads it depends on, each list comma-separated:

```markdown
## Interfaces
Defines: Store, Store.Mutate, ErrNoState
Uses: Clock, Dir
```

An identifier is letters, digits, underscore and dot, starting with a letter or
underscore, so `Store.Mutate` is one identifier. A leading `- ` bullet and
backticks around an item are stripped, and repeated `Defines:` or `Uses:` lines
merge in order. A bead's own `Defines:` do not satisfy its `Uses:`; only a bead
it depends on does.

## Check an epic's beads together

`gt spec lint --graph <epic>` reads the epic's children and reports the mistakes
that only show across beads: a name two beads define, a name a bead uses that no
bead it depends on defines, and two independent beads that modify the same file.
It exits 1 on any of those. A test name that two beads list is an advisory and
does not change the exit code. The check reads `Defines:` and `Uses:` from
`## Interfaces`, the `create:` and `modify:` paths from `## Files`, and the
`Test...:` name leading each acceptance box.

## The six lessons

### Define every shared name in exactly one bead

A name another bead imports has one defining bead; every other bead lists it
under `Uses:`. B8 (gt-o4sk8.9) defines `RequestFileName`, `ResultFileName`,
`RequestFile`, `MaintenanceResult` and `MaintFiles`, and B11 (gt-o4sk8.12)
imports them and defines only its own `requestFiles`, `readMaintenanceRequest`
and `finishMaintenanceRequest`. B4 and B8 had each carried a file-operation
interface; B8 kept its own small `MaintFiles`, so the hold's interface has one
owner and the maintenance protocol has another, and no later bead has to choose
between two spellings of the same operation.

### Name the test seam for every behaviour a box tests

A box that says "a probe that always errors" is untestable until the bead names
the seam that injects the error. B9a (gt-o4sk8.10) carries every dependency in
`Deps` — `Clock`, `Sleep`, `Store`, `Probes` — so
`TestRunBlockedOrUnreadableDrainAborts` drives an erroring seat probe with no
process and no sleep, and B1 (gt-o4sk8.2) gives `Store` a `Rename` field so
`TestStoreWriteIsAtomic` replaces it with a failing function and checks the
previous state survives. An acceptance box opens with the `Test...:` name where
a test is the claim, and the Gate's `-run` filter selects it.

### State what happens on error

For any probe, guard, hold, drain, install or gc step, say what a failure does,
not only what a success does. B9a states that a drain whose seat probe keeps
erroring ends the run `aborted`, puts the probe error in the drain step's
`Detail`, and attempts no install. B11 states that a write failure of
`maintenance-result.json` leaves the request file in place and is logged with
the run id, so the next tick answers again instead of rerunning gc. Leaving the
error path unstated has a cost: B2's heartbeat and lock edge cases had to be
guessed by the polecat, and an unreadable seat list read as "no seats" was a
gap the reviewer found in the drain plan.

### Check the dependency edges by listing the names each bead uses

The `Uses:` line is the list to check: every name there comes from a bead this
bead depends on. B11 uses `update.RequestFileName`, `update.RequestFile` and
`update.MaintenanceResult`, so it depends on B8, which defines them. That edge
was once missing — B11 on B8 was a defect that only hand reading found — and
`gt spec lint --graph <epic>` now refuses the used name that no blocker
defines.

### Split a bead at about six behaviours

Count the distinct behaviours rather than the files: a bead with seven
behaviours is more than one worker can land in one small MR. B9 carried seven,
so it was cut into B9a (gt-o4sk8.10) — plan, dry run, drain, install and lift —
and B9b (gt-o4sk8.21) — maintenance, restart, verify and the partial outcome.
B10 was cut too, into B10a to B10e. The lint routes more than six acceptance
items to the planner, so the boxes make the count visible.

### Write the Gate as a command that selects the tests the boxes name

The Gate is the command a reviewer runs to see the boxes pass, so it names the
tests they open with. B11 edits a container-suite package, and its Gate is
`go test ./internal/daemon -run 'MaintenanceRequest|ScheduledMaintenance'
-count=1`, the narrow filter that selects its boxes rather than the package;
B9a, in the new non-container package, runs `go test ./internal/update
-count=1`. The landing gate runs the repo's full gate and belongs to `gt done`;
the bead's Gate selects what this diff's boxes need.

## Where the lessons come from

The bead-linting session record — the gt update beads, the refusals the lint
made, and the defects only hand reading found — is in git history (see
CHANGELOG).
