> Status: approved in sections by Sloan (grilling rounds 2 and 3, 2026-10-25). Decision record: hq-3afdo.

# Deep review: a Go daemon job, bounded slices for sandbox seats

## Problem

The scheduled deep review's plan step reads the landings ledger on the host
(`~/gt/.runtime/landings/<rig>.jsonl`). The schedule runs `scripts/deep-review.sh plan` inside a
`mol-deep-review` run bead, which a sandbox seat executes. A sandbox seat cannot read the ledger, so
the plan step fails closed (hq-wisp-nj731) and the schedule entries deep-review-slice and
deep-review-risk are held (gt-q86qy). The bash script is 907 lines and is not tested in Go.

## Decision

The review is a Go job in the gt daemon's scheduled-sling patrol. The daemon reads the ledger on the
host, writes a bounded slice onto the run bead, and the dispatcher slings the bead to a seat. The seat
reviews the slice and never reads the ledger. Logic lives in Go with unit tests; no new scripts.

Rejected alternatives (recorded for the decision trail):
- **B, a standalone binary on its own launchd job.** Same logic, but a second scheduler to run and
  monitor, and it duplicates the daemon's sling and bead code.
- **E, an agent seat on the host that does the plan.** Puts a deterministic job on agent tokens.
- **Derive landings from git.** Loses the gate result, om verdict and score that the ledger holds.
- **Move records into the database now.** No landings table is planned (dipper's tracker) and there
  is no Postgres target date.

## Components

- **`internal/deepreview`** (gastown): plan, gate and classify logic, ported from
  `scripts/deep-review.sh` as pure functions, plus the cursor parsing. Unit tests use fakes.
- **Landings reader:** the existing read-only `internal/landings` package. The package does not write.
  The landing worker (`internal/land`, `land.go:2153`) stays the only writer of the ledger.
- **Daemon patrol:** the scheduled-sling path runs the plan on the host, writes the slice and the new
  cursor onto the run bead's notes, and leaves the sling to the dispatcher.
- **Seat:** reviews the slice on its bead. Its formula carries no ledger path.

## Data flow, one run

1. The schedule fires. The daemon reads the cursor from the newest `cursor:` line in the last run
   bead's notes; with none, it uses the default window.
2. It reads the ledger from the cursor through the reader and classifies the landings with the rules
   the bash uses.
3. It builds the slice in rotation order, stopping at 250000 bytes or 40 files.
4. It writes the slice and the new cursor (`cursor: <RFC3339 landed_at> [<slice path>]`) onto the new run
   bead, then the dispatcher slings the bead.
5. If nothing needs review, no bead is created (as the gate does today).

## Errors (all fail closed)

- **Ledger missing or unreadable:** no bead; escalate (the hq-wisp-nj731 class). Never an empty slice.
- **Malformed line:** skipped and counted; the count goes into the run bead's notes (as the bash does).
- **Unreadable cursor:** refuse and escalate; never guess a window.
- **Slice over the bound:** cut at the bound; the cursor stops at the last record included, so no
  landing is skipped.
- **Cursor moves only after the bead is written.** A crash between steps re-reads the same landings.
- **One run per schedule at a time:** the daemon checks for an open run bead before it creates one.

## Testing

- Unit tests in `internal/deepreview` with fake ledger and bead clients: plan, classify, slice bound,
  cursor placement, and a table row for every error path above.
- **Parity before deletion:** a fixture ledger and fixture run notes go through the bash plan and the Go
  plan; the slice, cursor and skipped count must match. The bash is deleted only after this passes.
- **Mutation check:** each decision gets the `&& false` mutant; results go on the bead and in the commit body.
- **Seat test:** the formula carries no ledger path.
- **One host integration test:** the job against a copy of the real ledger, inside the 30-second cap.

## Cutover

1. The Go package and its unit tests land; the bash still runs.
2. The parity test lands.
3. The daemon job runs in shadow mode: it writes its result to the notes and slings nothing, and is
   compared with the bash for a day.
4. The schedule switches to the Go job and the held entries come off hold (overseer owns the config).
5. `scripts/deep-review.sh` and its tests are deleted in the same chain.

Rollback: restore the bash entry in the schedule. Nothing is deleted before step 5 is green.

## Open items (not decided here)

- The exact formula change for the seat (robin owns deep-review-run.formula.toml step 1). It waits for
  the slice format in this spec.
- The slice format (fields and how the seat reads it) is to be fixed in the plan, before the formula
  change.

## Facts this design rests on (origin/main 9b8cbad1)

- `scripts/deep-review.sh`: 907 lines; `plan` takes `--landings`, `--prev-notes`, `--repo`; `gate` takes
  `--landings`, `--label`, `--green-state` or `--green-from-landings`.
- The cursor is a `cursor:` line in run-bead notes.
- Schedule (`settings/config.json`): `deep-review-slice` (3h, `slice_bytes` 250000, `slice_files` 40)
  and `deep-review-risk` (15m); both held. `deep-review-risk-agentlab` was removed by the overseer.
- The ledger is written only by `internal/land` (`land.go:2153`); the other `Append` in the tree is
  unrelated (jev battery log).
- Bead description and notes are LONGTEXT (migration 0049); the 255 limit covers short fields only.
