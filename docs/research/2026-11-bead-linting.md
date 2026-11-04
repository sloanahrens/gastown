> Status: historical (2026-11). Merged in: gt-62yz9. Not maintained.

# Bead linting: how it works, what it did to the gt update specs, and how it could work better

Written by the overseer on 2026-11-03 for the operator, who asked for it after watching the lint drive the shaping of 31 beads. The sources are named inline; the history comes from history-man and the code, and the experience comes from this session.

## Summary

- `gt spec lint` is a shape check, not a quality check. It makes a bead carry five named sections, one to six acceptance boxes, a size that says one worker, and, for any file the Constraints name, a file that exists on main or is introduced by the bead. The spec dispatcher runs the same check before it spends a seat, and holds a bead that fails.
- It exists because polecats "finish what a bead pins down and wander where it does not" (operator, 2026-10-02, gt-f8ppx), and because a bead once named files that never landed and was slung anyway (gt-7t1im).
- In this session the lint was the discipline surface the operator describes. A machine-generated batch of 20 beads passed it 4 times in 20. Hand-shaping 31 beads produced 9 refusals, and each one made me read the bead again.
- The lint is the floor. The defects that would have cost the most were all invisible to it: a lost-update race between two beads, a type defined by two parallel beads, a missing dependency edge, a heartbeat design that overwrote step results, a loop that starved the last databases. Hand reading found every one.
- No record I found measures whether lint-clean beads land first time more often. The one adjacent measurement says shape does not prevent overruns (see "What the records do not say").
- Recommended first changes, in order: structured file verbs and a dependency-aware file inventory (removes most of the friction), refusals that print the exact accepted rewrite, a check that the description and the acceptance field agree, static checks on the Gate and boxes, and an epic-level lint across beads.

## How it works today

**Where the rules live.** The checks are in internal/specdispatch (lint.go, target.go). The required sections come from a template file, `daemon.patrols.spec_dispatch.template` in the town config or else `~/.claude/docs/agents/spec-template.md`, with the built-in Goal, Constraints, Out of scope, Gate and Size as the fallback (docs/guides/spec-dispatcher.md). The daemon's own tier sweep bead shape used to hardcode the same list, which let the sweep file beads the dispatcher then skipped; gt-uzkic made them share one source.

**The command.** `gt spec lint <id>` prints the first failure; `--json` lists every failure in check order. Exit 0 is clean, 1 is refused, 2 means the bead needs planning (label needs-planning, a Size that says planning, or more than six boxes). A refusal prints the required sections and the re-lint command (gt-ngtev).

| Check | Refusal names |
|---|---|
| the bead is work, not an epic, agent bead, wisp | `not a work bead` |
| each template `## ` section present and non-empty | `## <section>` |
| one to six acceptance items | `acceptance` |
| every repo file the Constraints names as one it changes exists on the target branch | `## Constraints` |
| Size matches `\bone\s+worker\b` | `size` |

**The path check** (gt-7t1im, three om rounds). It reads only the Constraints, and only tokens that look like a repo file: slash-separated, last segment with an extension. It compares them with origin/main (the rig's default branch). Cue words beside a path exempt it: "new file X", "adds X", "create X", "X is created", "X will be created", "X does not exist yet". A path the bead only reads can be passed with "see X", "per X", "as in X". The cue must sit directly beside the path on the same line. A bead naming a file with no inventory is refused, never passed.

**The dispatcher.** Each tick lints every candidate before it allocates a seat. `polecat_pool.shape_gate` decides what a refusal does: `off`, `warn` (the default, and what this town runs, since the key is unset) or `refuse`. Under `warn` the bead is skipped with the reason `unshaped: <fields>`, spends no seat and gets one `SHAPE:` comment; under `refuse` it also gets the `needs-shape` label. The label `spec-shape-waived` waives the lint. A bead that needs planning is held in every mode. A hand `gt sling` is not blocked by the lint (gt-f8ppx).

**Around it.** `gt spec lint` prints top history-man hits when a bead mentions slow or timed-out tests, as advice only (gt-p07bw); `gt prime` does the same for a polecat (gt-jamix). PRIME.md tells every agent to shape a work bead and run the lint (gt-z4m6g). The sling-time content-overlap guard is a separate mechanism (shared test names and files); since gt-e1ufo the dispatcher ignores overlap, and a hand sling only warns, as it did for three of today's slings.

**Usage today.** The waiver label has been used 0 times and `needs-shape` 0 times. Six beads have worn `needs-planning`. The dispatch dry runs this week listed the same five unshaped beads at every check (dv-wdrv, dv-0w7l.10, .12, .13, be-xu2.22).

## Why it exists

- Flash polecats wander where a bead does not pin the work down; the dispatcher therefore holds unshaped beads (gt-f8ppx, operator decision 2026-10-02).
- A spec written against code that never landed was slung and cost a session (gt-7t1im, evidence gt-08chr).
- The same five sections had been decided in two places (gt-uzkic) and the lint had been tied to bead type and a `spec` label that meant two things (gt-mmsr2, which made it a shape lint for every work bead).
- om rejects vague beads' work; the what-om-rejects checklist (gt-b9j3o.9) and the twenty-minute rule are the other halves of the same discipline.

## What the lint did in the gt update work

**The machine-generated batch.** I first parsed the landed plan into 20 beads with a throwaway script. 4 of 20 passed. The one refusal I read in full was the path check on B1 ("names internal/update/state.go, absent from main"); I did not read the other 15 reasons, because the operator stopped the script and asked me to shape each bead by hand. His point stands: a script that gets beads past the lint skips the reading the lint is there to force.

**The hand pass.** 31 beads (28 for polecats, 3 overseer-run) all pass now. On the way the lint refused 9 times:

| Refusals | What it taught |
|---|---|
| 4: a path I meant as "created by an earlier bead" (run.go, update_trigger.go, a docs file twice) | The cue must follow the path directly. A comma, semicolon or parenthesis between them breaks it. The accepted form is "<path> is created by bead X". This is learnable and unintuitive. |
| 2: `settings/config.json`, a launchd label, `mayor/town.json` | Any slash-and-dot token is read as a repo file, so runtime files and labels trip the check. I had to describe them in prose. |
| 3: the Size line of the overseer-run beads | The rule checks the words "one worker", not whether the bead is polecat work. "One worker (the overseer)" is true and passes. |

Two traps outside the lint's view: children created under an epic inherit its labels, so all 28 polecat beads inherited `overseer-only` and would never have dispatched; and the acceptance text lives in two places that must be edited together.

**What only reading found.** None of these is a lint check and none could be with today's rules:

| Defect | Where it showed |
|---|---|
| A heartbeat that rewrote the whole state file from a snapshot could overwrite a newer step result | B1, B2, B9a: fixed by one `Mutate` on the store |
| The same interface defined by two parallel beads | B4 and B8 (file operations); B8 got its own |
| A missing dependency edge (a bead imports names another bead defines) | B11 on B8 |
| A bead too big for twenty minutes (seven distinct behaviours) | B9 split into B9a and B9b; B10 into five |
| A fairness bug (one database's batches using the whole budget, starving the rest) | A1 |
| Behaviour left for the polecat to guess: what a heartbeat does on a write error, the shape of a history line, what a missing lock directory does | B2 |
| A probe whose data exists only as sentences, not numbers | B10b2 |
| A real fail-closed gap (an unreadable seat list read as "no seats") | om found it in the plan's drain step |
| A dry run that would overwrite the previous run's record | B9a |
| A needed daemon helper not exported (the image trim environment) | E1 |
| Wrong constants taken from memory (a lock wait of 60 s, not 120) | B6 |

The lesson I would write into the spec template: define every shared name in exactly one bead, name the test seam for every behaviour a box tests, state what happens on error, check the dependency edges by listing the names each bead uses, and split at about six behaviours.

## What the records do not say

I found no measurement of first-time-right rates or om rejection rates split by lint-clean versus not. The nearest data is gt-b9j3o.4: in mango's analysis a bead shape (shaped, task or bug, priority 0 to 2, one label, om threshold 0.6 to 0.75, haiku) stopped at the overrun guard 16.7% of the time against 2.8% for the rest, n=24, thin. It says shape does not prevent overruns and size does. The data to answer the real question exists: gt-2xee4 builds a per-bead change table from the events archive, and gt-y01io is the held mango analysis of what predicts rounds to land. Adding "passed lint first time" would let it be asked.

## Ideas to make it better

Cost is rough: S is one bead, M a few, L a programme.

1. **Structured file verbs (M).** A `Files:` block with `create:`, `modify:` and `read:` verbs replaces the cue grammar. It removes the comma trap and the false positives on runtime files, and it says what the bead does with each file, which the polecat needs anyway.
2. **A dependency-aware file inventory (S to M).** Pass a path that an open blocker creates. The lint already knows the dependency graph; chains of beads then need no wording tricks, and a path no blocker creates is correctly refused.
3. **Refusals that print the accepted rewrite (S).** gt-ngtev prints the required sections; extend it to print the exact phrase for a path refusal ("write `<path> is created by bead X` or `create <path>`") and the missing section's heading.
4. **Description and acceptance field agree (S).** The criteria live in the description and in a field, and gt done reads the field. Refuse when they differ. This is a trap the town has already documented.
5. **Static checks on the Gate and the boxes (M).** Refuse a Gate that runs a whole container-suite package without `-run` (the package list already exists in internal/cmd/tap_guard_container_suite.go), a box that says `make gate`, and a box that needs Docker, a cluster or a host path. These are the standing "tickable by the polecat alone" rules, today enforced by memory.
6. **A graph lint over an epic (M to L).** `gt spec lint --graph <epic>` would check that each type or function named in an Interfaces block is defined once, that every name a bead consumes comes from a bead it depends on, that independent beads do not edit the same file, and that test names are unique across the epic. This would have caught three of the table's defects.
7. **A failure-behaviour prompt for risk paths (S).** If the Goal mentions a probe, guard, hold, drain, install or gc, require a box naming the error behaviour. A heuristic and a warning, not a refusal.
8. **A guard for inherited labels (S).** Warn when a child inherits a skip-dispatch label from its parent. Better: do not inherit those labels.
9. **Size from content, not words (M).** Count distinct behaviours (boxes, verbs, files) and warn at the number where beads started overrunning. Calibrate it with the overrun data, once enough exists.
10. **Lint metrics (S).** Record each lint outcome and refusal reason. Join them to rounds-to-land in the mango dataset, to answer whether lint-clean predicts landing.
11. **Own the unshaped pile (S).** Five beads have sat unshaped for days. An age on the dashboard and an owner (the filer) would turn "held" into "being fixed".
12. **Lint at creation (S).** `bd create` for rig work beads could run the lint and print the refusals at once, so shaping happens while the filer still has the context.

My order: 3, then 1 and 2 together, then 4 and 5, then 6, then 10.

## Decisions for the operator

1. Which of the twelve ideas become beads? (The decision bead gt-62yz9 holds the answer.)
2. Should `shape_gate` move from `warn` to `refuse`, now that nobody uses the waiver?
3. Should the spec template gain the six lessons under "What only reading found"? The template is in ~/.claude, so I have not edited it.
4. Should beads meant for the overseer carry a kind the lint knows about, instead of satisfying the Size rule by wording?

## Sources

gt-f8ppx, gt-7t1im, gt-mmsr2, gt-uzkic, gt-ngtev, gt-p07bw, gt-jamix, gt-z4m6g, gt-e1ufo, gt-b9j3o.4, gt-b9j3o.9, gt-2xee4, gt-y01io; docs/guides/spec-dispatcher.md; internal/specdispatch/lint.go and target.go; the epic gt-o4sk8 and its 31 beads (the session record); the dispatch backlog log of 2026-11-02 to 11-03.
