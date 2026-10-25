---
status: accepted
date: 2026-10-25
---

# A town-owned tracker on Postgres replaces Dolt-backed beads

ADR 0002 kept Dolt with a policy fix and named SQLite as the fallback. Three things changed that.
Measurements on 2026-10-25 showed the cost is structural: roughly 80,000 `bd` calls in session
transcripts, reads ten times writes, a daemon spawning about six `bd` processes a second, and one
Dolt commit per write that no policy removes. The offline flatten we ran that day halved the
server's memory and left CPU unchanged. SQLite cannot serve concurrent agents and does not move to
Kubernetes, which is where the whole system is going. And the town uses a small part of beads: of
56 issue columns, 22 hold data in the gt database, and nine tables are empty.

We decided to build a tracker the town owns: a long-lived Go service (`trackerd`) in front of
Postgres 17, in a nested Go module `tracker/` inside this repository, with a thin Go client behind
the existing `beads.Client` seam of ADR 0001. Rigs move one at a time through a small `bd` shim
that routes by id prefix to old `bd` or the service; there is no dual write. Every rig's data is
imported from Dolt in one transaction and verified row for row before its flip. Dolt-style history
is replaced by an append-only events table and ordinary Postgres backups; `AS OF` and `DOLT_REVERT`
are gone, and so are `gt history` and the compaction safety net built on them.

This supersedes the SQLite trigger in ADR 0002. The 30-day evaluation (claude-1ey.12) becomes a
checkpoint on the tracker program. The rest of ADR 0002 stands for as long as any rig still runs on
Dolt: no remote sync, no routine history rewrite, owned garbage collection.

## Considered options

- **Fix Dolt in place, or run it in Kubernetes (claude-wkb.4).** Rejected as the destination: it
  keeps the per-write commit and the memory profile, and only moves them. Kept as a fallback if the
  cutover slips.
- **Port the beads fork to Postgres.** Rejected: an 87,000-line storage layer with a test suite we
  do not trust, to keep features the town does not use.
- **SQLite.** Rejected: single writer, and not a Kubernetes database.
- **A separate repository.** Rejected for now: the tracker is a nested module with its own `go.mod`
  and a test that it imports nothing from `gt`, so extraction later is a move, not a rewrite.

Design: `docs/plans/2026-10-25-town-tracker-design.md`. Plan: `docs/plans/2026-10-25-town-tracker-plan.md`.
Program: epic gt-p85pw; handoff bead claude-pwu; decision record claude-477.
