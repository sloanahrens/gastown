# Dolt Storage Architecture

> **Status**: Current reference for Gas Town agents
> **Updated**: 2026-02-28
> **Context**: Dolt is the sole storage backend for Beads and Gas Town

---

## Overview

Gas Town uses [Dolt](https://github.com/dolthub/dolt), an open-source
SQL database with Git-like versioning (Apache 2.0). One Dolt SQL server
per town serves all databases via MySQL protocol on port 3307. There is
no embedded mode and no SQLite. JSONL is used only for disaster-recovery
backups (the JSONL backup patrol exports scrubbed snapshots every 15 minutes to a
git-backed archive), not as a primary storage format.

The `gt daemon` manages the server lifecycle (auto-start, health checks
every 30s, crash restart with exponential backoff).

## Server Architecture

```
Dolt SQL Server (one per town, port 3307)
├── hq/       town-level beads  (hq-* prefix)
├── gastown/  rig beads         (gt-* prefix)
├── beads/    rig beads         (bd-* prefix)
├── wyvern/   rig beads         (wy-* prefix)
└── sky/      rig beads         (sky-* prefix)
```

**Data directory**: `~/gt/.dolt-data/` — each subdirectory is a database
accessible via `USE <name>` in SQL.

**Connection**: `root@tcp(<host>:3307)/<database>` (no password).

## Endpoint

The town's Dolt endpoint (host, port) lives in `mayor/town.json`:

```json
"dolt": {"host": "100.64.0.5", "port": 3307}
```

`gt install` writes it (`--dolt-port`, default 3307) and
`gt config set dolt.port <port>` changes it. A town whose `town.json`
predates the field reads the listener in `.dolt-data/config.yaml`. gt never
reads the endpoint from the environment (gt-y3pgh.3): `gt dolt start` writes
the server's `config.yaml` from it, `gt rig add` stamps it into each new
rig's `.beads/config.yaml` (`dolt.port`, `dolt.host`) through
`bd config set`, and gt exports it to the agents and bd processes it starts:

| bd (Beads) | Purpose |
|------------|---------|
| `BEADS_DOLT_SERVER_HOST` | Server host (bd defaults to `127.0.0.1` if unset) |
| `BEADS_DOLT_SERVER_PORT`, `BEADS_DOLT_PORT` | Server port |

The exported variables are outputs for bd; setting them in a shell does not
move gt. gt no longer exports `GT_DOLT_HOST` or `GT_DOLT_PORT` (gt-y3pgh.9):
nothing read them. The daemon ignores `patrols.dolt_server.port` and `.host`
in `daemon.json` for the same reason: the town's endpoint is the one source.

**Remote Dolt servers**: If Dolt runs on a different machine (e.g., over
Tailscale), set `"host"` in `town.json`'s `dolt` block. gt exports it as
`BEADS_DOLT_SERVER_HOST` to all bd subprocesses, overriding bd's hardcoded
`127.0.0.1` default.

## Commands

```bash
# Daemon manages server lifecycle (preferred)
gt daemon start

# Manual management
gt dolt start          # Start server
gt dolt stop           # Stop server
gt dolt status         # Health check, list databases
gt dolt logs           # View server logs
gt dolt sql            # Open SQL shell
gt dolt init-rig <X>   # Create a new rig database
gt dolt list           # List all databases
```

If the server isn't running, `bd` fails fast with a clear message
pointing to `gt dolt start`.

## Gas Town Scope vs `bd --global`

Gas Town's town-level beads are the `hq` database. Access them by running
direct `bd` commands from the town root (`~/gt`) or with `bd -C ~/gt ...`.
Direct `bd` commands from rig worktrees use that rig's `.beads` redirect and
database, so do not assume an `hq-*` ID will retarget the command.

Do not use `bd --global` for Gas Town town beads. In Beads, `--global`
means the standalone shared-server database named `beads_global`; it does
not mean Gas Town's `hq` database, and `BEADS_DOLT_DATABASE=hq` does not
retarget `--global`.

For Gas Town Dolt health, use `gt dolt status`. `bd dolt status` reports
the Beads client/runtime view and can say no Beads-managed server is running
even when the Gas Town Dolt server on port 3307 is healthy.

## Write Concurrency: All-on-Main

All agents — polecats, crew, mayor, the daemon — write directly
to `main`. Concurrency is managed through transaction discipline: every
write wraps `BEGIN` / `DOLT_COMMIT` / `COMMIT` atomically.

```
bd update <bead> --status=in_progress
  → BEGIN
  → UPDATE issues SET status='in_progress' ...
  → CALL DOLT_COMMIT('-Am', 'update status')
  → COMMIT
```

This eliminates the former branch-per-worker strategy (BD_BRANCH,
per-polecat Dolt branches, merge-at-done). All writes are immediately
visible to all agents — no cross-agent visibility gaps.

Multi-statement `bd` commands batch their writes inside a single
transaction to maintain atomicity.

## Schema

Schema version 6. The full schema lives in `beads/.../storage/dolt/schema.go`.
Key tables shown below; see source for indexes and full column lists.

```sql
-- Core: every bead is a row in issues (tasks, messages, agents, gates, etc.)
CREATE TABLE issues (
    id VARCHAR(255) PRIMARY KEY,
    title VARCHAR(500) NOT NULL,
    description TEXT NOT NULL,
    status VARCHAR(32) NOT NULL DEFAULT 'open',
    priority INT NOT NULL DEFAULT 2,
    issue_type VARCHAR(32) NOT NULL DEFAULT 'task',
    assignee VARCHAR(255),
    owner VARCHAR(255) DEFAULT '',
    sender VARCHAR(255) DEFAULT '',          -- messaging
    mol_type VARCHAR(32) DEFAULT '',         -- molecule type
    work_type VARCHAR(32) DEFAULT 'mutex',   -- mutex vs open_competition
    hook_bead VARCHAR(255) DEFAULT '',       -- agent hook
    role_bead VARCHAR(255) DEFAULT '',       -- agent role
    agent_state VARCHAR(32) DEFAULT '',      -- agent lifecycle
    wisp_type VARCHAR(32) DEFAULT '',        -- TTL-based compaction class
    metadata JSON DEFAULT (JSON_OBJECT()),   -- extensible metadata
    created_at DATETIME, updated_at DATETIME, closed_at DATETIME
    -- ... plus ~20 more columns (see schema.go)
);

-- Relationships between beads
CREATE TABLE dependencies (
    issue_id VARCHAR(255) NOT NULL,
    depends_on_id VARCHAR(255) NOT NULL,
    type VARCHAR(32) NOT NULL DEFAULT 'blocks',   -- blocks, parent-child, thread
    PRIMARY KEY (issue_id, depends_on_id)
);

-- Labels (many-to-many)
CREATE TABLE labels (
    issue_id VARCHAR(255) NOT NULL,
    label VARCHAR(255) NOT NULL,
    PRIMARY KEY (issue_id, label)
);

-- Audit trail
CREATE TABLE comments (id BIGINT AUTO_INCREMENT PRIMARY KEY, issue_id, author, text, created_at);
CREATE TABLE events   (id BIGINT AUTO_INCREMENT PRIMARY KEY, issue_id, event_type, actor, old_value, new_value, created_at);

-- Agent interaction log
CREATE TABLE interactions (id, kind, actor, issue_id, model, prompt, response, created_at);

-- Infrastructure
CREATE TABLE config          (key PRIMARY KEY, value);       -- runtime config knobs
CREATE TABLE metadata        (key PRIMARY KEY, value);       -- schema version, etc.
CREATE TABLE routes          (prefix PRIMARY KEY, path);     -- prefix→database routing
CREATE TABLE issue_counter   (prefix PRIMARY KEY, last_id);  -- sequential ID generation
CREATE TABLE child_counters  (parent_id PRIMARY KEY, last_child);
CREATE TABLE federation_peers (name PRIMARY KEY, remote_url, sovereignty, last_sync);

-- Compaction
CREATE TABLE issue_snapshots     (id, issue_id, compaction_level, original_content, ...);
CREATE TABLE compaction_snapshots (id, issue_id, compaction_level, snapshot_json, ...);
CREATE TABLE repo_mtimes         (repo_path PRIMARY KEY, mtime_ns, last_checked);
```

**Wisps** (ephemeral patrol data) reuse the same `issues` table with
`wisp_type` set. They are Dolt-ignored (`dolt_ignore` table) so wisp
mutations don't generate Dolt commits — only structural changes to the
ignore config itself are committed.

**Mail** is implemented as beads with `issue_type='message'` in the
issues table — there is no separate mail table. The `sender` field and
`dependencies` (type='thread') provide threading.

## Dolt-Specific Capabilities

These are available to agents via SQL and used throughout Gas Town:

| Feature | Usage |
|---------|-------|
| `dolt_history_*` tables | Full row-level history, queryable via SQL |
| `AS OF` queries | Time-travel: "what did this look like yesterday?" |
| `dolt_diff()` | "What changed between these two points?" |
| `DOLT_COMMIT` | Explicit commit with message (auto-commit is the default) |
| `DOLT_MERGE` | Merge branches (integration branches, federation) |
| `dolt_conflicts` table | Programmatic conflict resolution after merge |
| `DOLT_BRANCH` | Create/delete branches (integration branches) |

**Auto-commit** is on by default: every write gets a Dolt commit. Agents
can batch writes by disabling auto-commit temporarily.

**Conflict resolution** default: `newest` (most recent `updated_at` wins).
Arrays (labels): `union` merge. Counters: `max`.

## Three Data Planes

Beads data falls into three planes with different characteristics:

| Plane | What | Mutation | Durability | Transport | Status |
|-------|------|----------|------------|-----------|--------|
| **Operational** | Work in progress, status, assignments, heartbeats | High (seconds) | Days–weeks | Dolt SQL server (local) | **Live** |
| **Ledger** | Completed work, permanent record | Low (completion boundaries) | Permanent | JSONL export → git push to GitHub | **Live** |
| **Design** | Epics, RFCs, specs — ideas not yet claimed | Conversational | Until crystallized | DoltHub commons (shared) | **Planned** |

The operational plane lives entirely in the local Dolt server. The ledger
plane is currently served by the JSONL backup patrol, which exports scrubbed snapshots
to a git-backed archive every 15 minutes — this is the durable record that
survives disasters (proven in Clown Show #13).

## Data Lifecycle: Think Git, Not SQL (CRITICAL)

Dolt is git under the hood. **The commit graph IS the storage cost, not the
rows.** Every `bd create`, `bd update`, `bd close` generates a Dolt commit.
DELETE a row and the commit that wrote it still exists in history. `dolt gc`
reclaims unreferenced chunks, but the commit graph itself grows forever.

This is the key insight from Tim Sehn (Dolt founder, 2026-02-27):

> "Your Beads databases are small but your commit history is big."
>
> "If you delete a bead you want to rebase with the commit that wrote it
> so it just isn't there any more in history."

**Rebase** (`CALL DOLT_REBASE()`, available since v1.81.2) rewrites the
commit graph — it's the real cleanup mechanism. DELETE + gc is necessary
but insufficient. DELETE + rebase + gc is the full pipeline.

**Critical update** (Tim Sehn, 2026-02-28): All compaction operations —
`DOLT_RESET --soft`, `DOLT_REBASE()`, `dolt_gc()` — are **safe on a
running server**. Routine compaction does not need downtime. Auto-GC has
been ON by default since Dolt 1.75.0. Flatten is trivially cheap (pointer
moves, not data writes). Can run daily or more frequently.

Reference: https://www.dolthub.com/blog/2026-01-28-everybody-rebase/

### The Six-Stage Lifecycle

```
CREATE → LIVE → CLOSE → DECAY → COMPACT → FLATTEN
  │        │       │        │        │          │
  Dolt   active   done   DELETE   REBASE     SQUASH
  commit  work    bead    rows    commits    all history
                         >7-30d  together   to 1 commit
```

| Stage | Owner | Frequency | Mechanism |
|-------|-------|-----------|-----------|
| CREATE | Any agent | Continuous | `bd create`, `bd mol wisp create` |
| CLOSE | Agent or patrol | Per-task | `bd close`, `gt done` |
| DECAY | Reaper patrol | Daily | `DELETE FROM wisps WHERE status='closed' AND age > 7d` |
| COMPACT | Compactor patrol (monitor) | Daily | Counts commits, escalates at threshold — does not rewrite history |
| GC | `scheduled_maintenance` patrol, the one GC actor | Weekly, or on old-gen growth >20% | `CALL dolt_gc('--full')` under the pause marker; history kept |
| FLATTEN | Operator, offline | Only when a database passes 2 GB | [dolt-history-offline.md](../dolt-history-offline.md) — `DOLT_RESET --soft` + `DOLT_COMMIT` |

All six stages are implemented in code. DECAY runs in the Reaper patrol
(wisp_reaper.go). COMPACT runs in the Compactor patrol (compactor_dog.go), which
monitors and escalates. GC runs in scheduled_maintenance (maintenance_gc.go).
FLATTEN has no command: it is an offline operator procedure with one trigger,
a database past 2 GB (gt-8z769.3).
All lifecycle tickers are populated by `EnsureLifecycleDefaults()`
(lifecycle_defaults.go), which auto-writes missing patrol blocks to daemon.json
on `gt init` or `gt up` and never overwrites existing ones. There is no Dolt
remote push patrol any more (see "No Dolt Remote Sync" below).

### Two Data Streams

```
EPHEMERAL (wisps, patrol data)          PERMANENT (issues, molecules, agents)
  CREATE                                  CREATE
  → work                                  → work
  → CLOSE (>24h)                          → CLOSE
  → DELETE rows (Reaper)                  → JSONL export (scrubbed)
  → REBASE history (Operator, on          → git push to GitHub
    Compactor patrol escalation)             → COMPACT/FLATTEN on escalation
  → gc unreferenced chunks                  (no downtime)
```

**Ephemeral data** (wisps, wisp_events, wisp_labels, wisp_deps) is
high-volume patrol exhaust. Valuable in real-time, worthless after 24h.
The Reaper patrol DELETES the rows. History compaction flattens the commits
that wrote them out of history. Without both, storage grows without bound.

**Permanent data** (issues, molecules, agents, dependencies, labels) is
the ledger. Even permanent data benefits from history compaction — a bead
that was created, updated 5 times, and closed generates 7 commits that
can be rebased into 1. The data survives; the intermediate history doesn't.

### History Compaction Operations

**Who compacts.** Nobody, routinely. `gt maintain`, the compactor-dog plugin
and scheduled_maintenance's `monitor` and `flatten` modes were deleted
(gt-8z769.3): the 2026-09-29 measurement (gt-8z769) showed the databases'
size is commit history, not garbage, so a routine rewrite bought little and
risked the history `bd history` reads. Flatten is an offline operator
procedure with a single trigger, a database past 2 GB:
[dolt-history-offline.md](../dolt-history-offline.md). The Compactor patrol
counts commits and escalates as a history-query latency tripwire.

**The one GC actor.** `scheduled_maintenance` (scheduled_maintenance.go,
maintenance_gc.go) is the only path in gt that runs a manual gc. Dolt's own
auto-GC stays on. In the window (`maintenance.window`, one hour) it measures
each database under the Dolt data dir and runs `CALL dolt_gc('--full')` on one
when either holds:

- weekly: its last patrol gc is a week old (7 days less 4 hours of slack, so
  the run does not drift a window later each week), or none is recorded;
- old-gen growth: `.dolt/noms/oldgen` is more than 20% larger than right after
  its last patrol gc.

Old-gen is the trigger because it is where history accumulates; a total-size
trigger fires on growth gc cannot remove. The gt-kfzqa audit saw gt-DB read
latency break stuck-agent-dog at 863 MiB under the old 2x size trigger; 20%
of old-gen keeps a gc well inside that.

The databases are every directory with a `.dolt` subdirectory under the
Dolt data dir (discovered each run), not the `compactor_dog` /
`wisp_reaper` database lists, whose fallback is `hq` alone. The patrol is
skipped (logged) when the server is externally managed or not on a loopback
host: the trigger reads the data dir from this host's disk.

Per database, `daemon/maintenance_state.json` (atomic write) records
`last_gc`, `post_gc_bytes`, `post_gc_oldgen_bytes` (the growth baseline) and
`reclaimed_bytes`. If the size cannot be re-measured after a gc, no run is
recorded, so the next window gc's that database again.

Before any gc, the same cycle takes the night's backup (gt-8z769.5): per
database, under its own pause marker, `CALL dolt_backup('sync-url',
'file://~/gt-backups/dolt/<date>.partial/<db>')` through the running server,
then a `backup.json` manifest and a rename to `<date>`; rotation keeps seven
nights. A Dolt backup is the server's consistent snapshot of every branch and
the working set, dolt_ignored tables included, so the server never stops. The
backup comes first because the gc is the step that has correlated with a Dolt
panic: a failed backup escalates and skips that night's gc. A night already on
disk is not taken again when a deferred gc retries. If the daemon was down
across the window, the 5-minute check outside it takes a catch-up backup (the
backup only, never the gc) once the newest backup is older than 24h plus the
window length and the town is quiet (no daemon work, no gate slot held); a
failed catch-up escalates and is not retried for 6 hours (gt-wne04). Restore:
`docs/dolt-restore.md`.

Due databases run smallest first, one at a time, each bounded by 10 minutes.
Before each database the patrol re-checks a quiet-window guard: the daemon's
upgrade-idle predicate, no container-gate slot or in-flight marker held by
anyone, no polecat with a fresh `working` heartbeat, and no pause marker held
by another actor. If the town is busy it logs why and stops; the next 5-minute
tick in the window retries, and databases already gc'd drop out. A gc error
stops the run, logs, and escalates once; the patrol does not retry until the
next window. It never flattens, never pushes and never restarts Dolt.

Around each database's gc the patrol also:

- writes the pause marker `<town>/daemon/dolt.pause` (internal/doltpause;
  actor `daemon/scheduled_maintenance`, until = now + 15 minutes) before the
  call and removes it after, whatever the outcome. No marker, no gc: a write
  failure fails the run. A marker the daemon cannot remove, or one left by a
  daemon that died mid-gc, lapses at its until.
- takes the write side of the daemon's Dolt-task lock (non-blocking). The
  daemon's own Dolt tasks (wisp_reaper, doctor_dog,
  jsonl_git_backup, the compactor_dog cycle) take the read side and skip
  their tick with `<task>: skipped: gc in flight`; a task in flight makes the
  gc defer with `daemon Dolt task in flight`. Nothing blocks the select loop.
- holds off a health-driven Dolt restart while the gc call is in flight and
  under its 10-minute timeout (plus 2 minutes' grace). Past that it escalates
  once and lets the restart proceed. A dead server is still started.

A window that closes with gc still deferred on at least one due database
counts toward a streak kept in `maintenance_state.json`
(`consecutive_deferred_windows`, `last_deferral_reason`). The patrol
escalates once at 3 consecutive windows, with the waiting databases, their
sizes and the top deferral reasons, then at most every further 3 windows. A
completed or failed run resets the streak.

`--full` matters: Dolt's gc is generational, and auto-gc and plain
`dolt_gc()` only collect the new generation. Chunks promoted to the old
generation stay until a `--full` pass.

Settings (`patrols.scheduled_maintenance` in `mayor/daemon.json`):

```json
"scheduled_maintenance": { "enabled": true, "window": "03:00" }
```

or `gt config set maintenance.window 03:00`. The schedule and trigger are
policy, not settings. The retired keys (`interval`, `threshold`, `mode`,
`gc_min_bytes`, `gc_growth_ratio`) still parse, so a daemon.json carrying
them decodes strictly, and the daemon logs them as ignored.

`patrols.compactor_dog.threshold` is an escalation line only; the compactor
patrol never compacts and only guards history-walking query latency. This
town sets it to 20000 rather than the 2000 default.

All compaction operations are safe on a running server — no downtime
needed. Can also be wired as a Dolt scheduled event (MySQL-style cron):
https://www.dolthub.com/blog/2023-10-02-scheduled-events/

```sql
-- Simple daily flatten (squash everything older than working set)
SET @init = (SELECT commit_hash FROM dolt_log ORDER BY date ASC LIMIT 1);
CALL DOLT_RESET('--soft', @init);
CALL DOLT_COMMIT('-Am', 'daily compaction');
```

**Flatten** (safe on a running server — no downtime needed):

Flatten is trivially cheap. `dolt_reset --soft` doesn't write any data —
it moves the parent pointer of the working set to the referenced commit.
The subsequent commit writes a new commit and two small pointer writes.
Can be run daily or even more frequently (Tim Sehn, 2026-02-28).

```sql
-- Flatten via SQL on a running server (preferred)
-- Find the initial commit
SET @init = (SELECT commit_hash FROM dolt_log ORDER BY date ASC LIMIT 1);
CALL DOLT_RESET('--soft', @init);
CALL DOLT_COMMIT('-Am', 'flatten: squash history');
-- GC runs automatically when journal exceeds 50MB
```

Concurrent writes during a flatten are safe — the merge base becomes
the initial commit, but the diff is just what the transaction wrote,
so the merge succeeds.

**Surgical compaction** via interactive rebase (squash old, keep recent):

Unlike flatten (which squashes everything), interactive rebase lets you
keep recent individual commits while squashing old history. Runs on a
live server. Based on Jason Fulghum's rebase implementation. It is an
operator procedure with no CLI (see [dolt-history-offline.md](../dolt-history-offline.md));
the plugin's `--compact` flag and the daemon patrol do not implement it.

**Concurrent write hazard**: DOLT_REBASE is NOT safe with concurrent writes
(Tim Sehn, 2026-02-28). If agents commit to the database during rebase, Dolt
detects the graph change and errors; park the writers first. Flatten mode (DOLT_RESET --soft) is unaffected — concurrent writes
are safe there because the merge base shifts but the diff is just the txn.

```sql
-- 1. Create a branch at the initial commit (the rebase "upstream")
SET @init = (SELECT commit_hash FROM dolt_log ORDER BY date ASC LIMIT 1);
CALL DOLT_BRANCH('compact-base', @init);

-- 2. Create a work branch from main (never rebase main directly)
CALL DOLT_BRANCH('compact-work', 'main');
CALL DOLT_CHECKOUT('compact-work');

-- 3. Start interactive rebase — populates dolt_rebase system table
--    All commits between compact-base and compact-work go into the plan
CALL DOLT_REBASE('--interactive', 'compact-base');

-- 4. Modify the plan: squash old commits, keep recent ones
--    First commit must stay 'pick' (squash needs a parent to fold into).
--    Keep last N commits as 'pick', squash everything else.
SET @keep_recent = 50;  -- keep last 50 commits individual
UPDATE dolt_rebase SET action = 'squash'
WHERE rebase_order > (SELECT MIN(rebase_order) FROM dolt_rebase)
  AND rebase_order <= (SELECT MAX(rebase_order) FROM dolt_rebase) - @keep_recent;

-- 5. Execute the rebase plan
CALL DOLT_REBASE('--continue');

-- 6. Swap branches: make compact-work the new main
CALL DOLT_CHECKOUT('compact-work');
CALL DOLT_BRANCH('-D', 'main');
CALL DOLT_BRANCH('-m', 'compact-work', 'main');
CALL DOLT_BRANCH('-D', 'compact-base');
CALL DOLT_CHECKOUT('main');
-- GC runs automatically when journal exceeds 50MB
```

**Rebase actions** (from `dolt_rebase` table):
- `pick` — keep commit as-is
- `squash` — fold into previous commit, concatenate messages
- `fixup` — fold into previous commit, discard message
- `drop` — remove commit entirely
- `reword` — keep commit, change message

**Caveat**: Conflicts during rebase cause automatic abort (no manual
resolution yet). The simple flatten is more reliable for daily use;
surgical rebase is for cases where you need to preserve some history.

Reference: https://www.dolthub.com/blog/2024-01-03-announcing-dolt-rebase/

### Dolt GC

`dolt gc` compacts old chunk data AFTER rebase removes commits from the
graph. Run gc after rebase, not instead of it. Order matters: rebase
first, gc second.

**Automatic GC is ON by default** since Dolt 1.75.0 (October 2025). It
triggers when the journal file (`.dolt/noms/vvvv...`) reaches 50MB. No
manual gc or server stop is required — the server handles it.

Gas Town managed Dolt configs should keep `auto_gc_behavior` enabled with
`archive_level: 1` so the sql-server does not retain every old chunk index
and grow RSS indefinitely. The config is regenerated only when the managed
Dolt server starts; a deployed change takes effect on the next Dolt restart.
Set `operational.dolt.auto_gc` to `off` in `settings/config.json` before that
restart to emit `enable: false` and `archive_level: 0` as an operational
rollback switch.

If auto-GC was disabled long enough for a database to bloat, schedule one
explicit `dolt gc --full --archive-level=1` in a maintenance window to reset
the storage/RSS baseline. That one-time reclaim is an operator action, not a
hidden daemon task; after it completes, auto-GC handles ongoing chunk
reclamation.

```sql
-- Manual gc (safe on a running server, no need to stop)
CALL dolt_gc();
```

GC is memory-hungry but our databases are small, so no concern (Tim Sehn,
2026-02-28).

### Dolt Scheduled Events (Spike Results, 2026-02-28)

Dolt supports MySQL-style `CREATE EVENT` for server-maintained cron jobs.
Reference: https://www.dolthub.com/blog/2023-10-02-scheduled-events/

**Tested on Dolt 1.82.6:**
- `CREATE EVENT ... EVERY 1 DAY DO CALL dolt_gc()` — works
- Events persist in `dolt_schemas` table — survive server restart
- Events only fire on the `main` branch
- Stored procedures work (`CREATE PROCEDURE` with DECLARE, BEGIN...END)
- Events can call stored procedures
- Minimum interval: 30 seconds (Dolt enforces this floor)

**Can scheduled events replace the Compactor patrol?**

**No.** The Compactor patrol's job is monitoring and escalation, and escalation is
the part SQL events cannot provide:
- Threshold checking (only escalate when commit count exceeds N)
- Per-database iteration (one patrol covers all DBs)
- Escalation on breach (files a bead the Mayor sees)
- Daemon-level logging and observability

A stored procedure could count commits, but it has nowhere to escalate to and
no place in the daemon lifecycle.

**What scheduled events CAN do:**
- Supplement compaction with explicit `dolt_gc()` scheduling
- But auto-gc is already ON by default since Dolt 1.75.0, making this redundant

**Recommendation:** Keep the Compactor patrol for monitoring. Auto-gc handles chunk
reclamation. Scheduled events add no value beyond what we already have.

### Pollution Prevention

Pollution enters Dolt via four vectors:

1. **Commit graph growth**: Every mutation = a commit. Rebase compacts.
2. **Mail pollution**: Agents overuse `gt mail send` for routine comms.
   Use `gt nudge` (ephemeral, zero Dolt cost) instead. See mail-protocol.md.
3. **Test artifacts**: Test code creating issues on production server.
   Firewall in store.go refuses test-prefixed CREATE DATABASE on port 3307.
4. **Zombie processes**: Test dolt-server processes that outlive tests.
   Doctor patrol kills these. 45 zombies (7GB RAM) found and killed 2026-02-27.

Prevention is layered:
- **Prompting**: Agents prefer `gt nudge` over `gt mail send` (zero commits)
- **Firewall** (store.go): refuses test-prefixed CREATE DATABASE on port 3307
- **Reaper patrol**: DELETEs closed wisps, auto-closes stale issues
- **Compactor patrol**: monitors commit growth, escalates when a DB crosses threshold
- **Doctor patrol**: kills zombie servers, detects orphan DBs, monitors health
- **JSONL backup patrol**: scrubs exports, rejects pollution, spike-detects before commit

All these daemon patrols are enabled by default via `EnsureLifecycleDefaults()` in
lifecycle_defaults.go. The daemon auto-populates missing patrol entries
in daemon.json on startup (`gt init` / `gt up`). To disable a specific
patrol, set `"enabled": false` in its daemon.json section — the auto-populate
logic preserves explicitly configured entries.

### Communication Hygiene (Reducing Commit Volume)

Every `gt mail send` creates a bead + Dolt commit. Every `gt nudge`
creates nothing. The rule:

**Default to `gt nudge`. Only use `gt mail send` when the message MUST
survive the recipient's session death.**

| Role | Mail budget | Nudge for everything else |
|------|-------------|--------------------------|
| Polecat | 0-1 per session (HELP only) | Status, questions, updates |

## Standalone Beads Note

The `bd` CLI retains an embedded Dolt option for standalone use (outside
Gas Town). Server-only mode applies to Gas Town exclusively — standalone
users may not have a Dolt server running.

The Dolt team is working on improving embedded mode for single-process
use cases like standalone Beads. This would give solo `bd` users a
zero-config experience (no server to manage) while retaining Dolt's
versioning capabilities.

## No Dolt Remote Sync

Gas Town does not push, pull or fetch Dolt remotes (ADR 0002,
`docs/adr/0002-no-dolt-remote-sync.md`). The daemon's `dolt_remotes` patrol,
`gt dolt sync`, `gt dolt pull`, the DoltHub remote that `gt rig add` created,
`gt maintain`'s remote-divergence pre-flight, and the fetch and push steps in
the compactor-dog and dolt-archive plugins are all gone. Nothing syncs
`refs/dolt/data` any more.

The backup is local: scheduled_maintenance takes a nightly backup of every
database in `~/gt/.dolt-data` into `~/gt-backups/dolt/<date>/` before its gc,
keeping seven nights (gt-8z769.5; restore procedure in `docs/dolt-restore.md`),
and the JSONL export committed to each repo is the human-readable current
state. The 15-minute `dolt_backup` patrol (`dolt backup sync` into
`<town>/.dolt-backup` plus an iCloud rsync) is retired.

Why: a git-protocol remote kept a full second copy of history on disk
(`.dolt/git-remote-cache`, 456 MB of the gt database's 775 MB on 2026-09-29),
pushed on a timer, and turned every history rewrite into a force-push. The
2026-09-19 divergence of gt from its remote was that mechanism. The operator
has restored from a filesystem copy, never from the remote.

`gt doctor`'s `dolt-remote-leftovers` check warns while any of the old state is
still on disk. It reads files only (`.dolt/repo_state.json`,
`.dolt/git-remote-cache`, each routed `.beads/config.yaml`) and never connects
to Dolt.

### Removing Dolt remotes

An operator procedure, run once per town, with the operator's approval. It
writes to the live server, so the check above never runs it for you. The
Dolt server stays up throughout.

1. List what is left:

   ```bash
   gt doctor --check dolt-remote-leftovers
   ```

2. Remove every registered remote. The loop finds them itself, so it needs
   no database list. `DOLT_REMOTE('remove', ...)` changes repository state,
   not a table, so it makes no Dolt commit. The server listens on the
   town's Dolt port (`gt config get dolt.port`; 3307 by default).

   ```bash
   dq() { dolt --host 127.0.0.1 --port "${GT_DOLT_PORT:-3307}" --user root --password "" --no-tls "$@"; }
   safe() { [[ "$1" =~ ^[A-Za-z0-9_.-]+$ ]] || { echo "SKIP unsafe name: $1" >&2; return 1; }; }
   # -r csv prints a header row first; tail drops it.
   dq sql -r csv -q "SHOW DATABASES" | tail -n +2 | grep -v -E '^(information_schema|mysql)$' |
     while read -r DB; do
       safe "$DB" || continue
       dq --use-db "$DB" sql -r csv -q "SELECT name FROM dolt_remotes" </dev/null | tail -n +2 |
         while read -r NAME; do
           safe "$NAME" || continue
           echo "$DB: removing remote $NAME"
           dq --use-db "$DB" sql -q "CALL DOLT_REMOTE('remove', '$NAME')" </dev/null
         done
     done
   ```

   A name the loop skips as unsafe is handled by hand after a look at
   `SELECT name, url FROM dolt_remotes`.

3. Remove each database's `git-remote-cache`. The cache is only read or
   written by a push or fetch, and there is no remote left to push to. Move
   it out of the data directory first rather than deleting it in place, so a
   mistake is a `mv` back:

   ```bash
   TRASH=~/gt/.dolt-remote-cache-trash-$(date +%Y%m%d)
   mkdir -p "$TRASH"
   for CACHE in ~/gt/.dolt-data/*/.dolt/git-remote-cache; do
     [ -d "$CACHE" ] || continue
     DB=$(basename "$(dirname "$(dirname "$CACHE")")")
     [ -e "$TRASH/$DB" ] && { echo "$DB: $TRASH/$DB already exists, skipped"; continue; }
     du -sh "$CACHE"
     mv "$CACHE" "$TRASH/$DB"
     test ! -e "$CACHE" && echo "$DB: cache gone"
     du -sh "$(dirname "$CACHE")"
   done
   gt dolt status          # server healthy, latency normal
   bd list --limit 1       # from ~/gt/gastown: gt reads work
   ```

   Delete `$TRASH` after a clean day.

4. Delete `sync.remote` from every `.beads/config.yaml` the check names.
   The file is tracked in the rig's repository, so the change lands as a
   commit on that repo; the mayor clone picks it up on its next
   fast-forward. `bd` reads `sync.remote` when it decides where to push, so
   leaving it invites a later `bd` to add the remote back.

   ```bash
   grep -n '^sync.remote' ~/gt/*/mayor/rig/.beads/config.yaml ~/gt/.beads/config.yaml   # expect nothing
   ```

5. Confirm: `gt doctor --check dolt-remote-leftovers` reports OK, and for
   every database `SELECT COUNT(*) FROM dolt_remotes` is 0:

   ```bash
   dq() { dolt --host 127.0.0.1 --port "${GT_DOLT_PORT:-3307}" --user root --password "" --no-tls "$@"; }
   dq sql -r csv -q "SHOW DATABASES" | tail -n +2 | grep -v -E '^(information_schema|mysql)$' |
     while read -r DB; do
       printf '%s ' "$DB"; dq --use-db "$DB" sql -r csv -q "SELECT COUNT(*) FROM dolt_remotes" </dev/null | tail -1
     done   # expect every count to be 0
   ```

## File Layout

```
~/gt/                            Town root
├── .dolt-data/                  Centralized Dolt data directory
│   ├── hq/                      Town beads (hq-*)
│   ├── gastown/                 Gastown rig (gt-*)
│   ├── beads/                   Beads rig (bd-*)
│   ├── wyvern/                  Wyvern rig (wy-*)
│   └── sky/                     Sky rig (sky-*)
├── daemon/
│   ├── dolt.pid                 Server PID (daemon-managed)
│   ├── dolt.log                 Server log
│   └── dolt-state.json          Server state
└── mayor/
    └── daemon.json              Daemon config (dolt_server section)
```
