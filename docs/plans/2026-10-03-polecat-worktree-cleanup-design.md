# Automatic cleanup of finished polecat worktrees (patrol_scan)

Date: 2026-10-03. Status: draft for review. Handoff bead: claude-0bg.
Verified against `origin/main` (fetched 2026-10-03), not the stale crew/sloan checkout.

## Problem

Nothing removes a polecat worktree once its bead lands. The deacon, dogs,
witness and refinery that used to are gone. On 2026-10-03 the operator and the
overseer hand-nuked 9 leftovers (amber, emerald, garnet: dirty or unpushed with
closed beads; flint, granite, jade, lapis, obsidian, onyx: parked with landed
beads). `patrol_scan` restarts, reopens, idles and closes molecules but never
removes a worktree. `gt polecat nuke` already preserves the branch to origin
before deleting and refuses unsafe cases without `--force`; nothing calls it.

## Decisions (grilled with Sloan, 2026-10-03)

| # | Decision |
|---|----------|
| Q1 | v1 reaps only seats that can never be reused. Reuse-eligible `done` seats are capacity and are never touched. (Today: 12 seats, all `done`, clean, reuse-eligible, so the reaper would do nothing on them.) |
| Q2 | Grace 30m for ordinary terminal seats, 24h for parked seats. Both configurable. |
| Q3 | Compute git state live. `AgentRecord.CleanupStatus` is advisory only. The patrol writes nothing but the removal. |
| Q4 | Veto on: non-terminal assigned bead, non-terminal bead referencing the branch or `resume_branch`, open MR, live or recent session, in-flight intent. |
| Q5 | gastown only: `worktree_cleanup.rigs` defaults to `["gastown"]`. |
| Q6 | Extend the tick summary line, one log line per decision, deduped alerts for blocked seats, one threshold alert. |
| Q7 | `enabled=false`, `dry_run=true` by default. Removal goes through `gt polecat nuke`, never `--force`. Per-tick cap (default 2). A non-safe verdict is surfaced, never forced. |

Approach: a new per-seat pass inside `patrolscan.Scanner.Tick`; the host shells
out to `gt polecat nuke`. The daemon cannot import `internal/cmd` (cycle), and
`Restart` already shells out to `gt session restart` the same way. Not chosen:
a separate patrol (duplicates seat enumeration and splits one seat's decision
across two ticks); extracting nuke into a shared package (a 400-line
safety-critical refactor, justified only if a second caller appears; the swap
would touch only the host's `Reap`).

## Design

### Placement in the tick

`seat()` cannot host this. It returns silently for a held (parked) seat and
for a dead seat with no work, which are exactly where reap candidates sit.
`Tick` gains `reap(rig, name)` as a second per-seat pass, run after the `seat()`
pass and before `orphans()`. Gated by `Options.Reap` (nil = off).

Ordering matters: removing a directory makes dead-holder recovery
(`orphans()` -> `recover()`: no session and no directory) eligible to reopen the
seat's bead. The Q4 vetoes keep any seat with a non-terminal bead out of the
reaper, so recovery never sees a reaped seat's live work. A closed bead's
molecule is closed by `orphans()` in the same tick.

### Decision

Vetoes run in this order, cheapest first. The first to fire wins and returns
`skipped`; a failed read returns `unknown`. Unknown never acts.

1. Intent record unreadable, or Submitted (the landing worker owns it).
2. A session exists or the heartbeat is fresh.
3. A non-terminal bead is assigned to the seat, or references its branch or
   `resume_branch`.
4. An open MR exists for the seat.
5. The seat is reuse-eligible per `polecat.DecideSlotReuse`.

A seat that survives is a dead end. Grace is measured from the later of the
bead's `closed_at` and the agent bead's last update; an unknown timestamp is
not eligible. Then the live git check: no unique commits per
`git cherry origin/main HEAD`, clean tree, no stash. Then:

- dry-run: `would-reap`
- real run, within the cap: `Env.Reap`, then `reaped`
- git check fails, or `nuke` refuses: `blocked`, with the blockers as detail

New outcomes: `reaped`, `would-reap`, `blocked`.

### Env surface (superseded by Revision 1 below)

```go
// ReapFacts reads the seat's live state. Any failed read is an error, never "clean".
ReapFacts(rig, polecat string) (ReapFacts, error)
// Reap removes the seat by running `gt polecat nuke <rig>/<name>` (no --force).
Reap(rig, polecat string) error
```

`ReapFacts` carries seat state (agent state, parked, session, heartbeat), work
(assigned bead with status and `ClosedAt`, branch references from non-terminal
beads, open MR), git (branch, unique commits, dirty, stashes, a check-failed
marker) and `Reusable`. The host assembles it from `internal/polecat`
(`LiveGitState`, `DecideSlotReuse`) and the beads readers it already holds.

The daemon's verdict and `nuke`'s own safety check are two implementations of
"is this safe". They are not unified here. When the daemon says eligible and
`nuke` refuses, `IsRefusal` maps it to `blocked` with nuke's reason, so a
disagreement fails safe and is logged. A counter makes a persistent
disagreement visible. Unifying them is a follow-up.

### Config

Nested under `PatrolScanConfig`, so it inherits the tick's `enabled` and
`interval`:

```json
"worktree_cleanup": {
  "enabled": false, "dry_run": true, "rigs": ["gastown"],
  "grace": "30m", "parked_grace": "24h",
  "max_per_tick": 2, "blocked_alert_threshold": 5 }
```

Missing or zero `max_per_tick` means 2, never unlimited.

### Observability

The summary line gains `N reaped, M would-reap, K blocked`. Every decision gets
one line with seat, verdict and reason; quiet vetoes emit nothing.

Blocked seats are not silent. `Daemon.escalateAlert(key, source, message)`
(already used by the rogue-bd check) raises a per-seat alert keyed on
`reap-blocked:<rig>/<name>:<blockers-hash>`, and one threshold alert when the
blocked count reaches `blocked_alert_threshold` (default 5).

### Rollout

1. Ship disabled.
2. `enabled=true, dry_run=true` on gastown for a few days. Every `would-reap`
   seat must be one an operator would have nuked by hand, and no reusable
   `done` seat may appear.
3. `dry_run=false`, `max_per_tick=2`.

Kill switch: `enabled=false`. No state to unwind; the only side effect is the
nuke, which preserves the branch to origin first.

## Testing

Unit (`internal/patrolscan/scan_test.go`), extending `fakeEnv`:
- veto table, one row per veto; each asserts `skipped` and `Reap` never called,
  with a baseline row that is reaped
- fail-closed table: every erroring read gives `unknown` and removes nothing
  (the gt-evdg class)
- grace boundaries (29m/31m; parked 23h/25h; unknown `closed_at`)
- git-safe: unique commits, dirty, stash, check-failed give `blocked`
- dry-run never calls `Reap`; per-tick cap; unset cap means 2
- `nuke` refusal gives `blocked`, other errors give `failed`
- alert dedupe; summary-line counters
- regression: 12 `done`, clean, reuse-eligible seats give zero `would-reap`

Daemon (`internal/daemon/patrol_scan_test.go`): config defaults, `ReapFacts`
assembly from fakes, and a literal assertion that `Reap`'s argv is
`gt polecat nuke <rig>/<name>` with no `--force`. One real-git integration test
for the cherry/dirty/stash probes against a temp origin. Hermetic: no live tmux
socket or Dolt (gt-yav3). End-to-end nuke coverage is gt-bxrji, out of scope.

## To verify in the first implementation slice

These were not read to the end, so the plan must confirm them rather than
assume them:
- `escalateAlert` dedupes by key (per-seat alerts depend on it).
- Where `patrol_scan` config is actually loaded from now that the latest commit
  retired `daemon.json` for `mayor/town.json`; the new block goes beside it.
- That `rec.Held()` covers every parked case, and how `MarkIdle`'s `stop`
  record interacts with the reap vetoes.
- The `closed_at` and agent-bead update sources for the grace clock.

## Revision 1 (2026-10-03, found while planning; needs Sloan's confirmation)

Reading `origin/main` for the plan showed that `gt polecat check-recovery
<rig>/<name> --json` already returns the safety verdict this design was going to
rebuild: `verdict`, `reusable`, `safe_to_nuke`, `blockers`, `git_state_source`,
`active_mr`, `branch`, `issue`. It is the verdict the nuke path and the Witness
use, and it is assembled in `internal/cmd` from the live git probe, hook-bead
classification (`ClassifyHookBead`) and active-MR landed evidence, none of which
the daemon can import. Rebuilding it in the daemon would be a second copy of the
logic gt-eqiid and gt-hsg were fixed for. `DecideSlotReuse` also has no
non-test callers on main, so it is not the reuse source this spec assumed.

Changes to the design above:

1. **`ReapFacts` is dropped.** The decision reads `Env` methods it already has
   (`Intent`, `SessionExists`, `Heartbeat`, `AssignedWork`) plus a new optional
   interface:

   ```go
   type ReapEnv interface {
       Recovery(rig, polecat string) (Recovery, error)   // parses check-recovery --json
       IdleSince(rig, polecat string) (time.Time, error) // grace clock
       BranchClaimed(rig, branch string) (string, error) // non-terminal bead naming branch/resume_branch
       Reap(rig, polecat string) error                   // gt polecat nuke, never --force
   }
   ```

   `Scanner` finds it by type assertion, so the pure package builds and tests
   with no host change, and a host that lacks it simply never reaps.
2. **Safe means the CLI said so, measured live:** `safe_to_nuke` true AND
   `verdict == "SAFE_TO_NUKE"` AND `git_state_source == "live"`. A recorded or
   unknown git source is `blocked`, never acted on (the gt-7kr/gt-14a class).
3. **Verdict mapping:** `WORKING`, `SUBMITTED`, `PENDING_MR` are quiet skips
   (someone owns the seat); `reusable` is a quiet skip (Q1); `NEEDS_RECOVERY` and
   `NEEDS_MQ_SUBMIT` are `blocked`. A seat that is `SAFE_TO_NUKE` yet not
   `reusable` is the parked-and-landed case, the real v1 target.
4. **Frozen seats are never reaped.** `Held()` is `Frozen || Desired==park`; only
   a park gets the 24h grace, a freeze is skipped silently.
5. **Grace is quiet.** A seat inside its grace emits no line (a line per seat per
   2-minute tick would flood the log); `would-reap` and `reaped` carry the
   signal.
6. **The nuke refusal needs a distinct exit status.** `gt polecat nuke` refuses
   with exit 1, the same as any failure. The plan adds a dedicated exit code for
   a safety refusal so the host maps it to `blocked` without matching text.
7. The real-git integration test is dropped: the daemon no longer runs git
   probes, and `check-recovery` has its own tests.

Verified while planning: `escalateAlert` dedupes (`gt escalate --fingerprint`
records a repeat onto the open escalation; `gt escalate clear --fingerprint`
closes it). `PatrolScanConfig` still lives in `internal/config/daemon_config.go`
as a section of `mayor/town.json`. `check-recovery --json` exits 0 whatever the
verdict, and `--reconcile-cleanup` (the only write) defaults off and is never
passed.

## Non-goals

Trimming idle reusable seats by age or count (later, separate option);
unifying the two safety verdicts; extracting nuke into a shared package; other
rigs; any LLM or steward job.
