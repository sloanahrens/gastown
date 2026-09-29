---
status: accepted
date: 2026-09-29
---

# Dolt stays for now, with no remote sync and no routine history rewrite

Measured on 2026-09-29 with the town stopped: an offline `dolt gc --full` reclaimed almost nothing
(gt 769 MB to 775 MB, hq 227 MB to 190 MB). gt's size was 456 MB of Dolt's `git-remote-cache`, a
local clone of the `refs/dolt/data` remote pushed to the gastown GitHub repo, plus 307 MB of history
for 19,746 commits in three weeks (about 940 a day, one per write) over a few megabytes of live
rows. Growth is commits, not garbage, and the only tools that shrink history rewrite every hash,
which is what diverged gt from its remote on 2026-09-19.

We decided to keep Dolt for now, shrunk to the one backend the town uses, and to change the policy
around it instead of the store: bd makes one Dolt commit per invocation instead of one per write;
history is never rewritten routinely (an offline rewrite is allowed only when a database passes
2 GB); gastown owns the Dolt server process, runs a weekly or size-triggered full GC, and writes a
pause marker that bd honors before any write; the Dolt remotes and the daemon's push ticker are
removed, and the backup is a nightly filesystem copy of the data directory plus the JSONL export
committed to each repo, extended to include events; the events table keeps its dolt-ignored
status but loses the delete cascade; only `bd migrate` migrates.

A written trigger decides the alternative: if, after commit batching and owned GC have run for 30
days, any week still has a Dolt-caused town stoppage, or the beads unit tier still cannot run
without a server, beads moves to SQLite (one file per database, WAL, JSONL export as backup). The
evaluation is a scheduled ticket, not a mood.

## Considered options

- **Move to SQLite now.** Rejected for now: a storage rewrite of an 87k-line layer while the
  landing path and supervisor are also being redesigned, when policy changes are deletion-only
  and may be enough. The trigger above keeps it live.
- **Keep the Dolt remote sync as the backup.** Rejected: it doubles history on disk, pushes on a
  timer, and turns every rewrite into a force-push and re-clone; the operator has restored from a
  filesystem copy, never from the remote.
- **Nightly flatten.** Rejected: it is the divergence mechanism, and it races live writers on the
  single shared server.

Decision record: wayfinder map claude-1ey, ticket claude-1ey.3; measurements in that ticket's
comments; evidence in `~/.claude/docs/research/deep-review/beads-storage.md`.
