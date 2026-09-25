# Dependency Satisfaction Is Merge-Aware

**Status:** implemented in gt-0r0z.
**Supersedes:** purely status-based blocker resolution.

## The bug this fixes

Blocker resolution used to be status-based: a dependency counted as satisfied
the moment the blocker bead's status read `closed`.

But a polecat closes its bead at **MR-creation time** (in `gt done`), not at
merge. A beads merge request spends roughly thirteen minutes in the gate. So
for that window `closed` means *submitted*, while every dependency check — and
every human reading `bd show` — takes it to mean *landed*.

The consequence is that a dependent bead is dispatched while the artifact it
depends on is still sitting in the merge queue. The downstream worker designs
against a world that does not exist.

This was observed, not theorized: on 2026-09-09 `be-suy` closed at ~13:38 when
its polecat submitted MR `be-wisp-b1z`; `gt-db8y` was auto-dispatched at ~13:41
branching off a `main` that did not yet contain `be-suy`'s work; `be-wisp-b1z`
did not merge until ~13:51. Its design notes said to call a target that
`be-suy` was adding, and to not hand-roll a fallback *if that target existed*.

## The rule

A blocking dependency is satisfied when the blocker is closed **and** it has no
**open** merge request.

| Blocker state | MR state | Dependency | Why |
|---|---|---|---|
| `closed` | open MR | **unmerged** — holds dependents | submitted, not landed |
| `closed` | no MR ever | landed — releases dependents | docs-only, decision bead, superseded |
| `closed` | MR rejected/superseded | landed — releases dependents | the MR is closed |
| `open` | any | open — blocks as before | unchanged status-based blocking |

Only blocking relation types gate readiness (`blocks`, `conditional-blocks`,
`waits-for`, `merge-blocks`). `tracks`, `parent-child`, `related`,
`discovered-from`, and `thread` never do, whether or not an MR is open.

## Why a rejected or abandoned MR cannot deadlock

The gate keys on a merge request being **open**, not on finding a merge commit.
Anything the refinery closes — merged, rejected, conflict, superseded —
drops out of the open-MR index and releases dependents. There is no state in
which a dependent waits for a merge commit that will never exist.

The residual risk is an MR that is neither merged nor closed. That is the
stranded-MR path, which the refinery and witness already own (the queue is
resolved before a branch is called stranded — gt-akap), not a new failure mode
introduced here.

## Failure posture

Two different postures, deliberately:

- **Gating fails open.** If the merge-state lookup errors, dispatch degrades to
  the old status-based behavior and prints a warning. Failing closed would
  turn a transient Dolt hiccup into a town-wide dispatch stall — a new failure
  class — whereas a missed gate merely reproduces the pre-existing bug.
- **Reporting fails honest.** An injected dependency status whose blocker could
  not be queried reads `unknown`, never `landed`. A gate that cannot act may
  degrade, but a report that cannot check must not reassure.

## Cost

Merge-queue lookups only happen for beads that declare a closed blocking
dependency. A bead with no dependencies, or whose blockers are all still open,
needs no lookup at all: the code checks whether any candidate has a
closed blocking edge *before* issuing a query. In the common case dispatch
timing is unchanged, and no merge-queue query is made.

## Where this lives

- `internal/beads/merge_pending.go` — the predicate (`DependencyMergeStatuses`,
  `UnmergedBlockerIDs`, `HasUnmergedBlockers`), the open-MR index
  (`OpenMRsBySourceIssue`), and the cross-rig resolver
  (`ResolveDependencyMergeStatuses`).
- `internal/cmd/capacity_dispatch.go` — the dispatch gate
  (`listUnmergedBlockedWorkBeadIDs`, `isScheduledWorkBeadMergeReady`).
- `internal/cmd/prime.go` — the starting-context line
  (`outputDependencyMergeStatus`), which covers every dispatch path including
  manual `gt sling`, not just the scheduler.
- `internal/cmd/scheduler.go` — `gt scheduler status` marks queue-held beads
  and names the blocking MR.

## Not this design

A more principled fix exists: change the close convention so the refinery closes
the source bead after merge. Then `closed` means what everyone assumes and the
status-based logic is correct for free, with no merge-aware special case
anywhere.

That was **explicitly rejected for gt-0r0z** — it touches the polecat exit path,
capacity accounting, and the done-state semantics that gt-uu6/gt-iljx had just
stabilized. It is recorded as follow-up work in **gt-pqqz**.

**Status: gt-pqqz has landed.** `gt done` no longer closes the source issue at
MR-submission time; the refinery's `closeMergedWorkBead`
(`internal/refinery/work_bead_close.go`) closes it once, at real merge
success. A source issue therefore no longer goes through a "closed but MR
still open" state on the normal path, which is the state this file's
merge-aware gate exists to detect.

The gate itself (`merge_pending.go` and its call sites listed above) has
deliberately not been removed: `IsPendingMergeCloseReason` still recognizes
the legacy closed-with-`pending_mr`-reason shape for any issue closed that
way before gt-pqqz rolled out, and the gate is harmless dead weight rather
than a correctness risk once that shape stops occurring. Removing it is a
follow-up, not a prerequisite. The injected dependency context stays useful
either way: it also covers the window from the worker's own point of view.
