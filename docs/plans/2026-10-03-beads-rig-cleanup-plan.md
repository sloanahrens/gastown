> Status: approved 2026-10-03; implemented by the slice beads be-1r0.1 to be-1r0.12 (be rig). Historical once merged; not maintained. Design: `2026-10-03-beads-rig-cleanup-design.md`.

# Beads rig cleanup: implementation plan

Handoff bead: claude-6r8. Every slice works in `~/gt/beads` (bd) or its crew clones; none changes gastown code. Each slice is a bead in the `be` rig (or hq for town-level items) shaped per `/gastown` (Goal, Constraints, Out of scope, Gate, Size, Acceptance). The rig stays parked throughout; slices are executed by the operator or a crew session, not dispatched.

Order: A1, A3 and A4 are independent. A2 follows A1. A5 follows A4. B1 follows A1's first table. B2 is independent but must not overlap other test suites. B3 follows B1. B4 is independent. B5 needs B2, B3 and B4. B6 needs B5.

| Slice | Does | Depends on | Size | Acceptance (checkable) |
|-------|------|-----------|------|------------------------|
| A1 | Disposition table for all 58 `be-` beads, then close landed/superseded ones with a commit comment each | Sloan approves table | M | Every closed bead has a `commit:` comment naming a hash on `origin/main`; `bd list --status=open` count matches the table's live rows |
| A2 | Patrol molecules be-aaz, be-cnr, be-v51 stop counting as queue; dashboard and `gt rig list` counts verified | A1 | S | Counts before/after recorded with the ephemeral-aware query |
| A3 | Archive ghoul and mutant commits as refs; re-check the other five with `git cherry`; nuke seats one at a time | Sloan approves plan | S | `refs/archive/ghoul-be-4mx` and `refs/archive/mutant-be-1kk` exist; `gt rig list` shows beads Polecats 0; no `--force` used |
| A4 | Backup, then archive embedded-Dolt relics out of the rig dir; close hq-9i5v2 | Backup taken | S | Backup path recorded; rig dir lacks the four relics; `gt dolt status` unchanged; nothing under `.dolt-data` touched |
| A5 | Check for executable `bd` binaries in rig worktrees; resolve mayor/rig (Sloan's decision); close hq-4exu7 | A4, Sloan's mayor/rig answer | S | No executable named `bd` under `~/gt/beads/**` except the quarantined copy (non-executable); mayor/rig at `origin/main`, clean |
| B1 | Targeted runs/checks for be-128, be-bv1, be-y7o, be-x3h, be-hy2, be-96h, be-8ff, be-a89, be-olm | A1 table | M | Each row moves to live/landed/obsolete with evidence |
| B2 | Time `make test` and `make test-integration` once each on a clean worktree | `docker ps` empty, no other suite | M | Seconds, container count and peak load on a bead; thresholds applied |
| B3 | Shape live beads and wire deps | B1 | M | `gt spec lint <id>` exits 0 for each live task/bug; `bd dep add` edges recorded |
| B4 | Schema-level startup self-check for the fork `bd` | none | M | A mismatch makes the check fail loudly; `BD_IGNORE_SCHEMA_SKEW` unset |
| B5 | Pilot one shaped bug (be-t95 or be-hy2) on flash, Sloan watching | B2, B3, B4 | M | Bead landed on `main` or the failure recorded with its log |
| B6 | Unpark or keep crew-only | B5 | S | Decision recorded with B2/B5 numbers |

## Stop rules

- A seat is never nuked without a `git cherry` run, on real refs, in the same sitting.
- A bead is never closed as landed without a named commit on a freshly fetched `origin/main`.
- Any Dolt trouble: follow the town CLAUDE.md (diagnostics before any restart), not these slices.
- No slice runs `bd sync`, removes anything under `.dolt-data`, or sets `BD_IGNORE_SCHEMA_SKEW`.

## Added after approval

- be-1r0.12 (P1): the `cmd/bd` integration tier does not compile (a test imports `internal/storage/backends`, deleted by be-xu2.2). B2 (be-1r0.7) depends on it.
- B5's acceptance carries one extra line: cherry-pick `refs/archive/ghoul-be-4mx` only if the pilot shows the pre-push hook blocks `refs/notes/om`.
- The Q7 thresholds in the design were approved as written.
