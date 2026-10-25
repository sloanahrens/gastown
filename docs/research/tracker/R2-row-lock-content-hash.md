> Status: historical (2026-10). Merged in: gt-p85pw.2 (R2). Not maintained.

# R2: what `row_lock` and `content_hash` guard, and what the tracker import does with them

**Bead:** gt-p85pw.2 (epic gt-p85pw, track R). **Completed by:** polecat nux, 2026-10-25.
**Read at:** beads `main` 7570bb92 (every beads line number below is that commit's); gastown `origin/main` d81c4c48.
**Method:** read-only research. The beads tree was read through a bare partial clone of the beads remote made outside any rig; no live database was queried and no schema changed.

## Answer

`row_lock` is live and load-bearing; `content_hash` is inert.

- **`row_lock`** is `BIGINT NOT NULL DEFAULT 0` on `issues` and `wisps`, added by migration **`0054`**. Every create mints a random non-zero int64 and every status/ownership write rewrites it. Dolt has no row locks and merges concurrent commits cell by cell, so this one shared cell is what turns two writers touching different columns of one row into a serialization conflict the retry loop replays. The same value is read back as `types.Issue.RowVersion` and published to clients as `revision`, where `expected_version` guards compare it for equality. **Import rule: copy it verbatim, as a signed 64-bit integer, including the value 0.** The new store must keep minting a fresh value on every status/assignee/`started_at` write.
- **`content_hash`** is a nullable `VARCHAR(64)` on `issues` (since migration **`0001`**) and `wisps` (since **`0020`**). It is computed only when a row is first inserted, and is never recomputed, compared, or serialized. **Import rule: copy it verbatim, or drop it; copy is the default because it keeps the import a faithful row-for-row copy and costs one nullable column.** Nothing in beads or gastown makes a decision on it.
- **No caller in gastown's `internal/` or `cmd/` reads either column.** The one gastown reader is `tracker/migrate`, which reads both off `SELECT *` and stashes them in `issues.metadata.legacy`; it makes no decision on them either, and `tracker.issues` has no column for the revision token yet.

## 1. `row_lock`

### Where the column was added

| Table | Migration | Notes |
|---|---|---|
| `issues` | `internal/storage/schema/migrations/0054_add_lease_columns.up.sql:48-60` | `BIGINT NOT NULL DEFAULT 0`; guarded ALTER |
| `wisps` | `internal/storage/schema/migrations/0054_add_lease_columns.up.sql:105-116` | same DDL, guarded on the wisps table existing |
| both | `internal/storage/schema/migrations/0055_move_leases_to_table.up.sql:16,91` | 0055 moves leases out of the row into an ephemeral table and drops the lease columns; `row_lock` stays because it is the general write-serialization cell, not a lease column |
| `wisps`, fresh clones | `internal/storage/schema/migrations/ignored/0013_add_wisp_row_lock.up.sql:24-37` | `wisps` is `dolt_ignored`; a clone whose cursor is already past 0054 never runs 0054, so the ignored series materializes the column (bd-rotq2) |
| CLI fresh bundle | `internal/storage/schema/cli_migrations.go:182,186` | direct DDL for the `dolt sql` bundle route; see the comment at `:105` |

The primary spec is the migration comment at `0054_add_lease_columns.up.sql:9-14`; the invariant every writer must obey is restated at `internal/storage/issueops/lease.go:64-105` and enforced at build time by `TestAllIssueRowWritesStampRowLock` (`internal/storage/issueops/row_lock_guard_test.go:1-49` for the rule, `:160` for the check).

### What it protects against

Two transactions on one row that touch *different* columns would otherwise cell-merge silently in Dolt — a reclaim writing `status` over a close writing `closed_at` is exactly-once versus a lost close — so every status/ownership writer rewrites this one cell to force a serialization conflict the retry loop replays; the same random value doubles as the client-visible optimistic-concurrency token (`revision`/`expected_version`).

### Writers — every path that mints a value

| Verb / path | Function | Write site (beads) |
|---|---|---|
| create (`bd create`, batch create, import) | `executeIssueInsert` | `internal/storage/issueops/helpers.go:121` (column list), `:154` (`freshRowLock()`) |
| create / upsert, proxied-server (uow) route | `insertIssueRow` | `internal/storage/domain/db/issue.go:765,788,806,817` |
| update (`bd update`) | `updateIssueInTx` | `internal/storage/issueops/update.go:482` |
| update, proxied-server route | `(*issueSQLRepositoryImpl).Update` | `internal/storage/domain/db/issue.go:266` |
| claim (`bd claim`) | `ClaimIssueInTx` | `internal/storage/issueops/claim.go:58` via `RowLockClause()` (`issueops/lease.go:126`) |
| claim, proxied-server route | `(*issueSQLRepositoryImpl).Claim` | `internal/storage/domain/db/issue.go:417` |
| close (`bd close`) | `closeIssue*` | `internal/storage/issueops/close.go:343` |
| reopen (`bd reopen`) | `reopenIssueInTx` | `internal/storage/issueops/reopen.go:68` |
| unclaim (`bd unclaim`, both entry points) | `UnclaimIssueInTx`, `UnclaimIssueIfAssigneeInTx` | `internal/storage/issueops/unclaim.go:82-84`, `:199-201` |
| reclaim expired lease (`bd reclaim`) | `ReclaimExpiredLeasesInTx` | `internal/storage/issueops/lease.go:781` |
| wake expired defers | `wakeExpiredDefersInTable` | `internal/storage/issueops/wake_defers.go:111-120` |
| persistence-plane move (promote/demote a wisp) | `MoveIssuePersistenceInTx` | `internal/storage/issueops/persistence.go:44` |
| Dolt merge settle (automatic conflict resolution) | row classifier | `internal/storage/versioncontrolops/automerge.go:471-501` |

The mint itself is `freshRowLock` (`internal/storage/issueops/lease.go:107`): `crypto/rand` over 8 bytes, forced non-zero, **signed** (a token can be negative).

### Readers — every path that consumes it

| Read | Function | Site (beads) |
|---|---|---|
| hydrated on every get/list/ready/search | full scan / lite scan | `internal/storage/issueops/scan.go:225`, `:397` → `types.Issue.RowVersion` |
| CAS precondition for close/update/delete | `CheckVersionInTx` | `internal/storage/issueops/version.go:36` |
| CAS predicate inside claim/unclaim UPDATEs | — | `internal/storage/domain/db/issue.go:471,481`; `internal/storage/issueops/unclaim.go:83,200` |
| merge settle (needs a token distinct from both parents) | — | `internal/storage/versioncontrolops/automerge.go:354-355,368,500` |
| published to clients | `IssueDetails.Revision` ← `RowVersion` | `internal/types/types.go:1174-1206`; `internal/workapi/detail.go:117`; `internal/httpapi/update.go:107` (and the close/reopen/release equivalents) |

### Contract facts an importer must not break

- The token is **opaque, equality-only**, and its coverage is **partial**: label, dependency, rename, `is_blocked` and compaction writes deliberately do *not* bump it (`internal/storage/issueops/lease.go:91-105`). It is not a change-detection key on its own.
- **0 is a legal token** (a migration-0054 row never mutated since), and the detail view emits it rather than omitting it — see `internal/types/types.go:1185-1194` and `TestGetIssuePublishesTheRevisionToken` (`internal/httpapi/get_issue_test.go:436`).
- It is **never serialized generically**: `json:"-"` on the issue (`internal/types/types.go:22,76-84`), so `bd export` JSONL carries neither `row_lock` nor `content_hash`, even though `test/conformance/roundtrip_test.go:159` strips `row_lock` defensively. A JSONL round trip therefore **cannot** preserve revisions; only a direct SQL copy can.
- The wire spelling is `revision` (int64) and the guard spelling is `expected_version`; guards exist on update, close, reopen, delete, release and batch apply (`internal/httpapi/apigen/types.gen.go:386-391,701-708,871-878,1166-1177,1726-1731,1910-1915`).

## 2. `content_hash`

### Where the column was added

| Table | Migration | Notes |
|---|---|---|
| `issues` | `internal/storage/schema/migrations/0001_create_issues.up.sql:3` | nullable `VARCHAR(64)`, present in the initial schema |
| `wisps` | `internal/storage/schema/migrations/0020_create_wisps.up.sql:3` | same shape |
| clone-local `issues` copy | `internal/storage/schema/migrations/ignored/0001_create_local_state_tables.up.sql:4` | ignored series materializes the same table shape |

No later migration alters it; the only other SQL that mentions it copies it (`0053_repair_rig_wisps.up.sql:57`, `cli_migrations.go:274,285`).

### What it protects against

Nothing today: it was introduced as a canonical-content fingerprint for dedupe and change detection in the SQLite/JSONL auto-import era (`CHANGELOG.md:8613`, `:7068`, and the still-standing claim at `docs/architecture/index.md:85`), but the current code neither recomputes it after an edit nor compares it anywhere.

### Writers

| Verb / path | Function | Site (beads) |
|---|---|---|
| create (any plane) | `PrepareIssueForInsert` | `internal/storage/issueops/create.go:678-680` — computes only when empty |
| create, proxied-server route | `(*issueSQLRepositoryImpl).Insert` | `internal/storage/domain/db/issue.go:61-63` |
| insert / upsert | `executeIssueInsert`, `insertIssueRow` | `internal/storage/issueops/helpers.go:121,145`; `internal/storage/domain/db/issue.go:765,788,808` |
| JSONL import | insert path above | export omits the column, so import necessarily recomputes at `create.go:678` |
| migration 0053 repair (issues ← wisps) | SQL copy | `internal/storage/schema/migrations/0053_repair_rig_wisps.up.sql:57` |
| legacy SQLite migration | projection | `internal/migration/legacysqlite/reader.go:45,396` |

No update path writes it: `allowedUpdateFields` excludes it (`internal/storage/domain/db/issue.go:40-49`) and `issueops/update.go` never references it. An edited issue keeps the hash computed at creation, so the column is stale by construction today.

### Readers

| Read | Function | Site (beads) |
|---|---|---|
| hydrated on every get/list/ready/search | full scan / lite scan | `internal/storage/issueops/scan.go:117`, `:296` |
| historical read (`AS OF`) | `AsOfInTx` | `internal/storage/issueops/as_of.go:48,73` |
| legacy SQLite import projection | — | `internal/migration/legacysqlite/reader.go:396` |
| conformance fingerprint (test-only) | `readCommenterAnchorRow` | `backend/conformance/commenter_contract.go:798` |

Nothing compares it to a stored or recomputed value. `ComputeContentHash` (`internal/types/types.go:195-248`) has exactly two non-test callers, both the insert-time fill above; its only consumer of the field is serialization, and the field is `json:"-"` (`internal/types/types.go:22`), so it never appears in `bd show`, `bd list`, or `bd export`.

## 3. Import rules for the tracker (M2)

**`row_lock` — copy.** Copy the Dolt value verbatim into the tracker's own storage for every `issues` and `wisps` row, as a signed 64-bit integer, preserving 0. Reasons:

- It is the `revision` a client may already hold; re-minting at import would turn every outstanding `expected_version` guard into a spurious `409 precondition_failed`, and dropping it to 0 would make stale guards *succeed* against rows they never read.
- It is random, not derivable: there is nothing to recompute.
- The source row and the imported row must be the same row to the client, or "verify" (M3) cannot compare them field-for-field.
- After import the **service** must mint fresh values on every status/assignee/`started_at` write, including wisps; the import is the one moment the value is preserved rather than replaced.

**`content_hash` — copy; dropping it is also safe.** Copy the nullable string as it stands. Reasons: it is cheap (one nullable column or one metadata key), it is the only record of what the row hashed to when written, and nothing reads it, so a recompute buys no behavior while silently rewriting historical values written by older algorithms. If M2 would rather have a smaller schema, dropping it breaks nothing today — but then say so explicitly in the verify report rather than leaving it half-migrated.

### What the gastown importer does today, and the two gaps

`tracker/migrate` already carries both: `readTable` selects every column (`tracker/migrate/read.go:37-41`, `Read` at `:74`), `issueFromRow` puts every column without its own field into `IssueRow.Legacy` (`tracker/migrate/rows.go:95-155`), and `copyIssues` writes `legacyMetadata(is.Legacy)` into `tracker.issues.metadata` (`tracker/migrate/import.go:140`). Tests pin the behavior: `tracker/migrate/import_integration_test.go:27,106-107`, `tracker/migrate/mapping_test.go:70-77,100-113`.

Two gaps against the rules above:

1. **The zero-drop loses live data.** `issueFromRow` skips `int64` values equal to 0 (`tracker/migrate/rows.go:143-146`), and `legacyMetadata` applies the same rule (`tracker/migrate/mapping.go:62-88`). `row_lock = 0` is a real token on legacy rows, and a client can be holding exactly that value; the copy rule must carry 0.
2. **Metadata is a stash, not the revision column.** `tracker.issues` has a `metadata jsonb` (`tracker/store/schema/0001_core.sql:41-43`) but no revision column in the table (`:11-45`), and nothing in `tracker/` serves `revision` today (`git grep revision origin/main -- tracker` is empty). If the shim/V-track path must answer `expected_version` the way the v0 API does, the value needs a typed `bigint` column read with a 64-bit decoder — a JSON number in `metadata` parsed by a float64 decoder corrupts tokens past 2^53, which is exactly the failure `internal/httpapi/get_issue_test.go:429-435` was written to catch in beads.

## 4. Does a caller in gastown read either column?

No caller in `internal/` or `cmd/` reads either column at `origin/main` d81c4c48:

```
git grep -n -E "row_lock|content_hash" origin/main -- internal cmd
# internal/beads/beads.go:3902  (a comment about bd's dep remove maintaining row_lock)
```

The only gastown reader is the tracker importer, described in §3: it reads both from Dolt and stores them in `metadata.legacy` without acting on them. The tracker's server and client packages contain no reference to either name.

## Re-check

```bash
# be fresh: the beads tree is read from a clone outside any rig
git clone --depth 1 http://control-plane.openshell.svc.cluster.local:3000/sloan/beads.git /tmp/beads
git -C /tmp/beads grep -n -E "row_lock|content_hash" -- internal cmd | rg -v _test.go
# gastown side
git grep -n -E "row_lock|content_hash|revision" origin/main -- internal cmd tracker
```
