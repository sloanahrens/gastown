+++
name = "rebuild-gt"
description = "Bring the installed gt binary in force from main"
version = 3

[gate]
type = "cooldown"
duration = "1h"

[tracking]
labels = ["plugin:rebuild-gt", "rig:gastown", "category:maintenance"]
digest = true

[execution]
type = "script"
timeout = "25m"
notify_on_failure = true
severity = "medium"
allow_deferred_exit = true
+++

# Rebuild gt Binary

Brings the installed `gt` binary in force from main. The daemon and every
session it spawns run the binary at their install path, so a stale binary
leaves merged fixes inert until detection catches it (gt-oqbw). This plugin
installs; it does not just report.

The daemon runs it in-process as a script-type plugin; no agent performs these
steps.

## Exit codes

The plugin's contract with the daemon. Before changing it, read
`internal/daemon/plugin_script.go`; each row has a test in `run_test.sh`,
recorded per case rather than per exit code since several exit-0 and exit-1
cases differ in what they escalate.

| exit | when | recorded | escalates |
|------|------|----------|-----------|
| 0 | did the work (installed, or already fresh) — or refused safely: dirty checkout, wrong branch, diverged local main, not safe to rebuild, install-gt refused (exit 2), no rig root | yes: the daemon records every exit-0 run; a refusal prints the skip marker (`scriptSkippedMarker`) so it reads skipped, and all but no-rig-root also call `gt plugin record-run` | on a refusal, only while the binary is due and past `REBUILD_GT_STARVE_MINUTES` |
| 3 | deferred, nothing accomplished (gate busy, under the install threshold, an unreadable staleness check, the install lock or container-gate slot not free) — retry next heartbeat | no | the same starvation clock |
| 1 | failed: install-gt.sh failed (build, install, or smoke check — it rolls back and escalates under `install-gt:*`), or the rig has no `scripts/install-gt.sh` | yes, as failure | install-gt's own fingerprint, or `rebuild-gt:no-installer` |

Exit 3 means deferral only because `[execution]` sets
`allow_deferred_exit = true`; without that opt-in, a script plugin's exit 3
reads as an ordinary failure (`internal/daemon/plugin_script.go`), so a real
failure elsewhere is never silently swallowed as "nothing to see here".

Most refusals clear on their own (a human commits the dirty checkout, main
catches up), so a refusal escalates only on the starvation clock; waiting
cannot fix a failure, so a failure escalates at once. Exit 3 writes no run
record, so the retry comes the next heartbeat (3 min), not after the 1 h
cooldown: an hourly tick kept landing inside busy windows and starved it.

## Drift Escalation

Any skip path below can persist for hours while the binary falls behind
`origin/main`. Before any pre-flight check, the run escalates on the outcome
that matters, commits behind, rather than on which skip fired:

```bash
gt stale --json   # commits_behind is meaningful regardless of RIG_ROOT's
                   # working-tree state — it only inspects git history
```

Over `REBUILD_GT_MAX_COMMITS_BEHIND` (default 20) it escalates under a stable
fingerprint:

```bash
gt escalate "rebuild-gt: binary is $N commits behind origin/main and has not been rebuilt" \
  -s medium \
  --source "plugin:rebuild-gt" \
  --fingerprint "rebuild-gt:drift" >/dev/null 2>&1 || true
```

`commits_behind` missing or `null` while `stale` is `true` is not "0 behind"
— the drift could be 1 commit or 1000, so it escalates too, under
`rebuild-gt:drift-unknown`. Reading unknown as 0 is what silently retired
this alarm the one time it mattered (gt-oqbw).

## Starvation

A deferral writes no run record, so a block that repeats is invisible to the
rest of the town (gt-kox0).

Every path that leaves a *due* binary out of force — a busy gate, a merge in
flight, a refusal to build — records the block in
`daemon/rebuild-gt-state.json`: when it opened and how many runs it has
lasted. Past `REBUILD_GT_STARVE_MINUTES` (default 30, above the longest gate
hold measured on 2026-09-22) the run escalates at high severity under
`rebuild-gt:starved`. The escalation goes out before the block is marked
escalated, so a call that never reached the town retries next run instead of
being lost for the rest of the block.

When the container-gate slot blocks, the run waits for it, then has
`install-gt.sh` build inside it (`--slot-role gastown/rebuild-gt`):

```bash
gt slot run --role gastown/rebuild-gt --timeout "<what is left of REBUILD_GT_RESERVE_WAIT>s" -- make build
```

On a multi-slot pool `gt slot run` alone would take a free slot beside the
load-sensitive suite (gt-htx3), so the run first polls `gt slot status` until
no gate-class role holds a slot. `REBUILD_GT_RESERVE_WAIT` (10m) bounds the
poll and the slot wait together; the 25m `[execution] timeout` covers that,
the two lock waits, and the build. A wait that gets nothing defers.

Reaching force — a fresh binary or a completed install — closes the block and
clears the keys this plugin owns (`gt escalate clear`, gt-vwry); so does a run
that finds the binary not due.

## Detection

Check binary staleness:

```bash
gt stale --json
```

`"stale": false` exits early (fresh). `"safe_to_rebuild": false` (non-main
branch, or a rebuild would be a downgrade) is a refusal: **DO NOT REBUILD**.

At or past `REBUILD_GT_INSTALL_THRESHOLD` commits behind (default 1), install
at the first quiet moment: a merged commit that is not in force is a live
defect, not a rounding error (gt-oqbw, gt-ww20, gt-rbfj). Under the threshold
(strictly fewer commits behind), defer. This plugin is the backstop for
landings nobody installed.

An unknown `commits_behind` counts as *at* the threshold: install. Reading it
as 0 left a stale, safe binary deferred every heartbeat forever.

## Pre-flight Checks

Before building, verify the source repo is clean and on main:

```bash
cd ~/gt/gastown/mayor/rig
git status --porcelain --untracked-files=no -- . ':(exclude).beads'  # No tracked changes that affect the build
git branch --show-current  # Must be "main"
```

If either check fails, skip the rebuild and record a wisp.

## Sync with origin/main

Without this, the build uses whatever commit a human last checked out.

```bash
cd ~/gt/gastown/mayor/rig
git fetch origin --quiet
git merge --ff-only origin/main --quiet
```

`--ff-only` is load-bearing: if local `main` has diverged from
`origin/main`, the merge fails and the plugin must skip the rebuild and
record a skip wisp with reason "local main diverged from origin/main" —
**never** `git reset --hard` to force it.

The fetch and fast-forward run under `install-gt.sh`'s flock
(`daemon/install-gt.lock`, claude-7fc), so they never move the tree under a
running `make install`. The plugin waits `REBUILD_GT_LOCK_WAIT` (30s), then
defers (exit 3). It releases the lock before `install-gt.sh`, which retakes it
with `REBUILD_GT_INSTALL_LOCK_WAIT` (60s, passed as `INSTALL_GT_LOCK_WAIT`) and
defers too when still busy: a long wait would keep the plugin running and hold
off the daemon's idle-point upgrade restart.

## Quiet gate

`make build` competes for CPU with a gate suite whose tests are load-sensitive
(gt-htx3), so the build waits for a town with nothing in flight:

- no gate-class role holding a container-gate slot, no container running
  outside the gate, and no saturated pool (`gt slot status --json`).

Under `REBUILD_GT_STARVE_MINUTES` that reading defers the run; past it the
slot is waited out (Starvation above). The merge-queue in-flight reading was
deleted with the merge queue (gt-v4ssj.6).

There is no second reading before the install, which only renames the binary
(claude-7fc). A `docker_unknown` status does not defer: the cross-check could
not tell, and a VM that is down runs no suite to compete with.

## Action

Run the rig's own `scripts/install-gt.sh --sha <HEAD> --source rebuild-gt`
(plus `--slot-role gastown/rebuild-gt` past the starvation threshold), the
path `make install` runs. Under one flock it builds, installs atomically
(gt-0het), keeps `gt.prev`, and verifies the commit in force (gt-oqbw,
gt-b5mpe). On a failed check it restores
`gt.prev` and escalates (`install-gt:smoke-failed`,
`install-gt:rollback-failed`). A failure install-gt does not escalate itself —
its `unexpected` trap, `no-rig`, or no RESULT line — is escalated here as
`rebuild-gt:install-failed` (MEDIUM). Then `gt formula sync` and
`gt plugin sync` (non-fatal), then `daemon/restart-pending.json`.

The daemon is **not** restarted here. It reads the marker at each heartbeat
and exits (code 75, restarted by launchd) at a moment with no plugin or
main-branch gate in flight, so an install never kills in-flight work.

## Daemon in force (backstop)

Each run also checks that the daemon came into force: a restart-pending
marker older than `REBUILD_GT_DAEMON_LAG_MINUTES` (default 30), or a daemon
whose `state.json` commit is not a descendant of the installed binary's while
that binary has sat that long, escalates
`rebuild-gt:daemon-not-in-force` (HIGH). It clears once both readings are
healthy: no marker pending and the daemon's commit a descendant of the
installed binary's. A reading that could not be taken (no `gt stale`, no
commit in `state.json`, a commit that does not resolve) leaves the alert as
is.

## Record Result

The receipt names the commits brought into force — `in force <old> -> <new>
(N commits)` plus their subjects. install-gt also appends a line to
`daemon/install-receipts.jsonl`. A refusal escalates only through the
starvation check's `rebuild-gt:starved`; reaching force — or a run that finds
the binary not due — clears `:starved`, `:drift`, `:drift-unknown`,
`:unverified`, `:no-installer` and `:install-failed` together.
