---
status: accepted
date: 2026-09-29
---

# gastown reaches beads only through the bd CLI

gastown used to reach beads three ways at once: the upstream `steveyegge/beads` Go library
pinned at v1.0.5 and linked into `gt`, the fork's `bd` binary on PATH, and raw SQL against the
shared Dolt server. The library knew schema 49 while the fork had migrated production to 66; it
opened those databases read-write with no drift check, its inserts failed on columns whose
defaults the fork had removed and gastown swallowed the failures, one of its writes recomputed
`is_blocked` with a predicate the fork had fixed, and its store open could create a rig database
at the old schema. Nobody could see any of this because the daemon's skew guard read a table the
fork never writes.

We decided that `bd` is the single owner of schema and invariants, and that gastown talks to it
only through the typed `beads.Client` seam over a subprocess. The in-process library, its go.mod
entry, and with them embedded Dolt, four cloud SDKs and cgo ICU leave the `gt` binary. Raw SQL
from gastown is reads only, through one package that declares the schema version it was written
against; gastown never issues DML and never migrates. `gt` refuses to start unless the `bd` on
PATH is a fork build whose schema level matches the database and whose JSON contract version it
knows. gastown never installs `bd`.

## Considered options

- **Re-pin the library to the fork.** One `replace` line; every used method is signature-identical.
  Rejected: it keeps a second schema owner and a second migrator, keeps the cgo build, and makes two
  processes that must stay at the same storage version forever. The speed it buys is on a handful
  of reads.
- **Library for reads, CLI for writes.** Rejected for the same reasons at half the benefit. It
  remains the escape hatch for a single read that stays too slow after batching.
- **bd's HTTP API.** Rejected: a network hop on one host buys no isolation.

## Consequences

- The fork's `bd` grows a machine mode (`BD_MACHINE=1`: no TTY, UI, metrics, plugins, prose or
  stdin), a default JSON envelope with a contract version and typed error kinds, and
  `bd capabilities --json`. gastown sets machine mode on every call: the environment policy
  (`SuppressBDSideEffects`) adds `BD_MACHINE=1`, and callers read the payload through
  `beads.LegacyPayload`. The calls that run outside it (`bd sql`, whose machine output loses column
  order, and the two terminal passthroughs) are listed in `internal/beads/machine_policy_test.go`.
- Every `beads.Client` method lands with its in-memory fake and a contract-suite case in the same
  change, enforced by a test that diffs the interface against the fake.
- Agents may call only a read-only `bd` allowlist from formulas and templates; every mutation goes
  through a `gt` verb.
- Hot reads (convoy event polling, dispatch board, ready work) move to `bd events tail --since`
  and `bd ready --json` and are measured after the move.

Decision record: wayfinder map claude-1ey, ticket claude-1ey.1, in the operator's `~/.claude`
tracker; evidence in `~/.claude/docs/research/deep-review/synthesis.md` (theme T1).
