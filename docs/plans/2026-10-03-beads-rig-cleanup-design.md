> Status: draft for Sloan's approval, 2026-10-03. Nothing here has been executed except the two crew-clone fast-forwards listed under "Already done". Historical once merged; not maintained.

# Cleaning up the beads rig, and the road to turning it on

Date: 2026-10-03. Handoff bead: claude-6r8 (town `claude` DB). Evidence base: beads `origin/main` at 406540b (fetched 2026-10-03), the `be` Dolt DB, the rig directory, and gastown `origin/main` 52545549.

## Problem

The beads rig is parked and nobody is working it through the town, yet it inflates the town's counts: 58 open `be-` beads (21 of them not work), 7 polecat seats (6 reusable or done, 1 stalled), stale relics from the embedded-Dolt era, and stale clones. Much of the queue is bookkeeping that landed without being closed, because the D-series program (wayfinder claude-1ey, all ten decisions resolved 2026-09-29) was built by crew clones landing directly onto beads `main`, not through the rig.

Sloan's intent: work toward a beads rig that works. Not ready to turn it on; unknown whether the (slow, expensive) gates make rig work worth it. So: clean up now regardless, measure the gate cost, then decide.

## Facts that shaped the plan

- The spec dispatcher is not single-rig. `gt spec dispatch` scans ready, unassigned task/bug/feature beads across operational rigs and skips parked or docked ones (`internal/cmd/spec.go:32`, `:1046`). `be-` is undispatched because the rig is parked. An earlier note said "reads only the gastown rig's beads"; that was wrong.
- Landed on beads `main`: be-3xa, be-h0k, be-u20 (closed); be-2gw.1/.2, be-b23.2/.3, be-xu2.1-.4 (still open).
- Unstarted D-series: be-c94.1-.6, be-2gw.3/.4/.5, be-b23.1, be-qm8.1/.2/.4, be-xu2.5-.8.
- Gate cost, measured 2026-09-29 on the be-b23 branch: full suite 945 s before the unit tier, 222 s after. `make test-integration` has a 45 m budget. Not re-measured on current main.
- Seats (verified with `git cherry` against real refs): chrome, guzzle, nitro, rust, vault have no patch missing from main. ghoul (be-4mx, b6cc32a) and mutant (be-1kk, 94d502b) each hold one.
- be-1kk is superseded: `Makefile:189` sets `BEADS_TEST_EMBEDDED_DOLT=1` for `test-integration` and no `skipUnlessEmbeddedDolt` remains.
- The orphan Dolt DB `beads` no longer exists (hq-9i5v2 is stale).

## Decisions (grilled with Sloan, 2026-10-03)

| # | Decision |
|---|----------|
| Q1 | Work toward a working beads rig. Cleanup now; gate-cost measurement; unpark only after both. |
| Q2 | One disposition table (live / landed / superseded / obsolete), each row with its evidence. Nothing is closed until Sloan approves the table. A bead counts as landed only if a commit on `origin/main` can be named. |
| Q3 | Keep no seats. Seats with nothing unmerged are nuked; seats with one unmerged commit are archived to a ref first. |
| Q4 | Agent and rig beads stay. Patrol molecules stop counting as queue. Non-work beads are hidden from queue views by filter, never deleted. |
| Q5 | Relics are archived out of the rig dir after a backup; nothing under `.dolt-data` is removed. |
| Q6 | crew/sloan is dormant. crew/sloan-be-fasttests is dormant, not dead; it stays as the clone for be-b23.1. |
| Q7 | One timed run of `make test` and one of `make test-integration` on a clean worktree, host-safety on, `docker ps` empty first. Rule of thumb: unit gate at or under ~4 min and integration post-merge only means unparking is worth it; a gate over ~15 min per MR means only gate-light work goes through the rig. Thresholds are Sloan's to change. |
| Q8 | Unpark readiness checklist (below). |
| Q9 | The pilot is a small isolated bug (be-t95 or be-hy2), not D-series code. D-series epics stay on crew until the pilot is clean. |
| Q11 | ghoul's be-4mx commit is probably moot (it served the refinery clone; D2 moves landing to the daemon). Archive it, drop the seat. |
| Q12 | Nuking is authorized as a slice that runs after plan approval, with the exact command and `git cherry` evidence per seat. |
| Q13 | mayor/rig: diffed read-only. `.claude/settings.json` is gt-injected. The five `.githooks/*` files are an older generation of `bd hooks` scripts, not gt-injected. Open decision below. |

## Already done (before this doc)

- crew/sloan (beads): main fast-forwarded 9177014 to 406540b; the 99 MB executable `bd` build in it set to mode 644 (hq-4exu7 precedent). All 13 local branches have zero unmerged patches. No branches deleted.
- crew/sloan-be-fasttests: fast-forwarded 6494999 to 406540b before Sloan had confirmed it was dormant. Reversible (reflog HEAD@{1}); the branch had no unmerged patches and a clean tree.

## Open decision

mayor/rig is 96 behind and carries older-generation `.githooks/*` text. Restoring main's version (`git checkout -- .githooks .claude/settings.json`, then `git merge --ff-only origin/main`) is very likely right but changes that clone's hooks. Sloan to confirm. `bd.QUARANTINED-v66-do-not-run` stays quarantined until Sloan says otherwise.

## Design

Two tracks, ordered. Track A needs no dispatch and changes no running system. Track B only measures until its last slice.

### Track A: cleanup (rig stays parked)

- **A1 Reconcile the queue.** Produce the disposition table with a named commit per landed row (preliminary table is in the handoff bead). On approval, close landed and superseded beads with `bd close --reason` plus `bd comments add <id> "commit: <hash> ..."`. Run from `~/gt/beads`.
- **A2 Stop non-work beads counting.** Close or label the three patrol molecules (be-aaz, be-cnr, be-v51) as infra; verify, not assume, how the dashboard and `gt rig list` count (`bd list` hides ephemeral beads by default, so a zero there proves nothing).
- **A3 Seats.** Archive ghoul and mutant commits as refs (`refs/archive/<seat>-<bead>`); re-run `git cherry` on the other five immediately before acting; nuke one seat at a time with `gt polecat nuke` (never `--force`). Done when `gt rig list` shows beads with 0 polecats.
- **A4 Relics.** Backup first. Move `embeddeddolt/`, `embeddeddolt.gate.lock`, rig-root `metadata.json` and `config.json.pre-vkcbi-b` to a dated archive dir in the rig. Close hq-9i5v2 (DB gone) and hq-4exu7 (once A5 lands).
- **A5 Binary guard.** `/bd` is already in main's `.gitignore`. Add a check that flags any executable named `bd` in rig worktrees, because a polecat-built `bd` once overwrote production. Decide the mayor/rig item.

### Track B: toward unparking

- **B1 Verify the unverified rows** (be-128, be-bv1, be-y7o, be-x3h, be-hy2, be-96h, be-8ff, be-a89, be-olm) with targeted runs, then fold into the A1 table.
- **B2 Measure the gate** (Q7). Record seconds, container count and peak load on a bead.
- **B3 Shape the live beads** to the dispatcher standard (`gt spec lint <id>` exit 0) and wire `bd dep add` where beads touch hot files.
- **B4 Startup self-check** that the fork's `bd` schema level matches what the town runs; `BD_IGNORE_SCHEMA_SKEW` is never set.
- **B5 Pilot** one shaped bead (be-t95 or be-hy2) on DeepSeek flash with Sloan watching, using the HOST SAFETY args the dispatcher attaches.
- **B6 Decide.** Unpark, or keep the rig as crew-only, using B2 and B5.

### Unpark readiness checklist

1. Queue reconciled and live beads shaped.
2. A1-A5 done; mayor/rig current and clean.
3. B2 numbers inside the thresholds.
4. B4 self-check in place.
5. B5 pilot landed clean.

## Risks

- Closing a bead as landed on a stale origin: fetch first, name the commit.
- Reading code presence as bug fixed: the B1 slice exists for this.
- Nuking a seat with unmerged work: the archive ref and the second `git cherry` are mandatory.
- A `bd` build overwriting the production binary: A5 guard, and no dispatch before B4.
- Gate runs contend with Docker/Dolt: B2 runs only when `docker ps` is empty.

## Not in scope

Unparking, dispatching `be-` beads, the D-series build itself, deleting branches, anything under `.dolt-data`, and the quarantined `bd` binary.
