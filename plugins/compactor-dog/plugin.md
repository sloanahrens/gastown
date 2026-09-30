+++
name = "compactor-dog"
description = "Monitor Dolt commit growth across production DBs and escalate when compaction is needed"
version = 1

[gate]
type = "cooldown"
duration = "30m"

[tracking]
labels = ["plugin:compactor-dog", "category:maintenance"]
digest = true

[execution]
type = "script"
timeout = "5m"
notify_on_failure = true
severity = "medium"
+++

# Compactor Dog

Monitors Dolt commit growth across all production databases and escalates to
the Mayor when history compaction or flatten is needed. This is a judgment
call, not a hard threshold trigger.

The daemon runs only `run.sh`. The steps below are a manual procedure for
whoever picks up a failed run or a warning: gather the data, then use judgment
to decide if maintenance is needed. Consider:

- Commit count per DB (absolute size)
- Growth rate (commits per hour since last check)
- Time since last flatten or compaction
- Current swarm activity (more polecats = faster growth)
- Whether growth is "normal busy" or "runaway"

## How this runs

The daemon runs `run.sh` directly in its default monitor-only mode
(`[execution] type = "script"`, claude-l5w). The script records per-DB commit
counts; when a DB exceeds the threshold it raises one MEDIUM `gt escalate`
per candidate and records a `warning` receipt. A run with no candidates
records `check-only`; a run whose escalations all fail records `failure`,
so "found work, could not report it" never reads as "found nothing" (the
outcome list under Record Result has the detail). The 30-minute cooldown gate
deduplicates: a steady over-threshold DB re-warns once per 30 minutes, not
per daemon heartbeat.
The daemon's `compactor_dog` patrol (`internal/daemon/compactor_dog.go`,
threshold 2000 by default) owns the hard line. `run.sh` reads that same
threshold from `mayor/daemon.json`'s `patrols.compactor_dog.threshold`
(falling back to 2000) and defers — logs, doesn't escalate — any candidate at
or above it, since the daemon raises it on its own cadence; the script's
warnings cover only the band below, no double alert at the top.

In `gc` mode (`patrols.scheduled_maintenance.mode`), commit count below the
daemon threshold isn't a disk signal either, so those candidates defer too —
otherwise a gc-mode town with the threshold raised to 20000 (this town's
setting) re-escalates every DB from 500 to 20000 each 30-minute cycle (gt-124a6).

Nothing runs the judgment steps below; a nonzero `run.sh` exit (Dolt
unreachable, no databases) is escalated (see Record Result).

**First, check the maintenance mode.** The judgment table in Step 6 depends
on it:

```bash
MAINT_MODE=$(jq -r '.patrols.scheduled_maintenance.mode // "monitor"' "$HOME/gt/mayor/daemon.json" 2>/dev/null)
echo "scheduled_maintenance mode: ${MAINT_MODE:-monitor}"
```

- `monitor` or `flatten`: commit count is the maintenance signal. Use the
  Step 6 table as written.
- `gc`: the daemon runs a history-preserving `dolt_gc('--full')` by database
  size and never flattens. Commit count is no longer a disk signal: history
  grows between gc runs by design, and flatten is not the fix. Use the gc-mode
  rules in Step 6. The daemon's `compactor_dog.threshold` is raised to 20000
  in gc mode.

## Compaction is operator-only

`run.sh` defaults to **monitor-only** (check-only) mode. No data is modified.

Compaction requires the explicit `--compact` flag and is **never** the default:
it rewrites commit history (flatten). Nothing is pushed: Dolt remote sync was
removed (ADR 0002). No automated run passes `--compact` — the plugin
escalates, and the operator decides.

```bash
# Monitor only (default, safe to run automatically)
bash plugins/compactor-dog/run.sh

# Monitor with a different recommendation threshold
bash plugins/compactor-dog/run.sh --threshold 1000

# Preview what compaction would do — dry-run only takes effect with --compact
bash plugins/compactor-dog/run.sh --compact --dry-run

# Operator-only: actually compact (DESTRUCTIVE — flatten)
bash plugins/compactor-dog/run.sh --compact
```

In `monitor` or `flatten` mode, `run.sh`'s default threshold (500) is the
"Escalate" column in Step 6: no candidates means every DB is under 500 and no
escalation is due. A DB over 1000 is always a candidate. Do not pass
`--threshold` to quiet a real signal. In `gc` mode, 500 still marks a
"candidate" for the report, but the script doesn't escalate it (see "How this
runs").

## Steps 1-5: Gather the data

`bash plugins/compactor-dog/run.sh` (monitor-only) lists the production
databases and each one's commit count. Its database filter is a name-pattern
heuristic: `gt dolt list` derives the production set from each rig's
`metadata.json`, so cross-check the two when a rig is added or renamed (that is
how `beads`, since renamed `be`, was miscounted).

For growth rate and time since flatten, query each database:

```bash
dolt sql -q "SELECT count(*) AS total,
  SUM(date > DATE_SUB(NOW(), INTERVAL 1 HOUR)) AS last_1h,
  SUM(date > DATE_SUB(NOW(), INTERVAL 24 HOUR)) AS last_24h,
  MIN(date) AS oldest_commit FROM dolt_log" \
  --host "${GT_DOLT_HOST:-127.0.0.1}" --port "${GT_DOLT_PORT:-3307}" -u root -d "$DB"
```

A database with five or fewer commits was recently flattened. `gt polecat list`
shows the swarm that explains a fast-growing one. The last run, from its
receipt (receipts are ephemeral wisps, hidden from bd without --include-infra:
drop the flag and this reports "never" even when runs exist, gt-idwq, so read a
"never" as a failed measurement; `gt plugin history compactor-dog` cross-checks):

```bash
RECENT_RUNS=$(bd list --label plugin:compactor-dog --status closed --include-infra --json 2>/dev/null \
  | jq -r '.[0].created_at // "never"' 2>/dev/null || echo "unknown")
echo "  Last compactor run: $RECENT_RUNS"
```

## Step 6: Make the judgment call

**This is where you use judgment.** Review all the data
gathered above and decide whether to escalate.

**Guidelines for judgment** (not rules — context matters):

| Signal | Comfortable | Getting warm | Escalate |
|--------|------------|--------------|----------|
| Total commits (per DB) | <200 | 200-500 | >500 |
| Hourly growth rate | <10/hr | 10-30/hr | >30/hr |
| Daily growth rate | <100/day | 100-300/day | >300/day |
| Time since flatten | <2 weeks | 2-4 weeks | >4 weeks |

**Hard escalate line (no judgment override): any DB over 1000 commits.**
The script's default threshold (500) matches the "Escalate" column above.
The "Getting warm" band (200-500) is informational — you may monitor
without escalating if context justifies it.

The table above is for `monitor` and `flatten` modes. **In `gc` mode:**

- Do not escalate on commit count, growth rate, or time since flatten — growth
  between daily gc runs is expected and gc does not reduce it.
- Escalate on commit count only above 20000 (the daemon's gc-mode
  `compactor_dog.threshold`) — a history-query latency concern, not a disk
  one. The daemon already escalates it; check `gt escalate list` first.
- Runaway growth (e.g. >300/hr with no swarm) is still worth escalating, as a
  runaway writer, not a compaction request.
- gc failures and skipped windows are escalated by the daemon's own
  scheduled_maintenance patrol, not by this plugin.
- Report what you saw (database, commit count or growth rate, whether a swarm
  explains it) and leave the remedy to the operator — never recommend
  compaction or flatten in gc mode.

**But override the table if context warrants it:**
- 400 commits after a 10-polecat swarm = normal, will settle
- 200 commits growing at 50/hr with no swarm = something's wrong

**If you judge maintenance is needed** (monitor or flatten mode; in gc mode
replace the Recommendation line with what you actually saw, e.g. runaway
growth, and do not recommend compaction):

```bash
gt escalate "Dolt compaction recommended" -s MEDIUM \
  --reason "<per-DB counts and growth; the swarm size; which DBs exceed comfort>.
See dolt-storage.md for procedure."
```

If everything looks comfortable, no action is needed.

## Record Result

Receipt outcomes, with who records them:

- `run.sh` check-only, no candidates: records `check-only` itself.
- `run.sh` check-only, every candidate deferred (see "How this runs" — at/above
  the daemon threshold, or gc mode): raises nothing, records `check-only`, but
  the description still names the deferred count, not "nothing found".
- `run.sh` check-only, one or more escalatable candidates: raises the per-DB
  `gt escalate` calls itself and records `warning`, or `failure` when every
  call failed — the script still exits 0, so `failure` means the checks found
  work and could not report it, not a crash.
- `run.sh --compact`: records `success` or `warning` (the compaction-error
  escalation) itself.
- `run.sh` exits nonzero: the daemon records `failure` and raises
  `gt escalate` (fingerprint `plugin:compactor-dog:failed`) with the output
  tail. No agent is dispatched; the next good run closes the escalation.

A run the daemon starts leaves two receipts: the daemon's, from the script's
exit status, and the script's own from the list above. A `failure` from the
script beside a `success` from the daemon means the process finished and the
run still failed to deliver its signal.

Manual escalation (only after investigating a failed run by hand):
```bash
gt plugin record-run --plugin compactor-dog --result failure \
  --title "compactor-dog: FAILED" \
  --description "Compactor check failed: $ERROR" >/dev/null 2>&1 || true

gt escalate "Plugin FAILED: compactor-dog" \
  --severity medium \
  --reason "$ERROR"
```
