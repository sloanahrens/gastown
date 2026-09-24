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

Brings the installed `gt` binary in force from main: the daemon and every
session it spawns keep executing the binary at their install path, so a stale
binary leaves merged fixes inert for as long as detection goes unrepaired
(gt-oqbw). This plugin installs, it does not just report.

The daemon runs it in-process as an execution-type script; no dog performs
these steps.

## Exit codes

The plugin's contract with the daemon. Before changing it, read
`internal/daemon/plugin_script.go`. Both sides must agree, and each row has a
test in `run_test.sh` (recorded there per case, not per exit code, since
several exit-0 and exit-1 cases differ in what they escalate).

| exit | when | recorded | escalates |
|------|------|----------|-----------|
| 0 | did the work (installed, or already fresh) — or refused safely: dirty checkout, wrong branch, diverged local main, not safe to rebuild, install-gt refused (exit 2), no rig root | yes, as success or skipped — except no rig root, which records nothing | on a refusal, only while the binary is due and past `REBUILD_GT_STARVE_MINUTES` (Starvation below) |
| 3 | deferred: nothing accomplished this run (gate busy, MR in flight, under the install threshold, an unreadable staleness check, the install lock or the container-gate slot not free — install-gt exit 3, or the lock busy before this plugin's own sync) — retry next heartbeat | no | the same starvation clock |
| 1 | failed: install-gt.sh failed (build, install, or smoke check — it rolls back and escalates under `install-gt:*`), or the rig has no `scripts/install-gt.sh` | yes, as failure | install-gt's own fingerprint, or `rebuild-gt:no-installer` |

Exit 3 means deferral only because this plugin's `[execution]` block sets
`allow_deferred_exit = true`; a script plugin without that opt-in has exit 3
read as an ordinary failure (`internal/daemon/plugin_script.go`), so a real
failure elsewhere can never be silently swallowed as "nothing to see here".

A refusal (exit 0, recorded as skipped) is not a failure (exit 1, recorded as
failure and escalated): waiting cannot fix a failure, but most refusals clear
on their own — a human commits the dirty checkout, main catches up. The run
that first hits one therefore escalates nothing; a due binary it leaves out of
force escalates on the starvation clock below, and the drift check alarms on
the states that never became a block at all.

A run record is what satisfies the cooldown gate, so exit 3 writing none is
what holds the retry to one heartbeat (3 min) instead of one cooldown (1 h).
The install waits for a window that opens and closes on its own, and every
hourly tick landing inside a busy window is what starved it before.

## Gate Check

The daemon heartbeat evaluates this gate and runs `run.sh` in-process; the
Deacon does not dispatch it (see `plugins/README.md` Scheduling, gt-o1z7).

## Drift Escalation

Every skip path below (dirty repo, wrong branch, diverged local main, an
unreadable staleness check, "not safe to rebuild") is a normal, expected
outcome on its own — but any one of them can persist for hours while the
binary quietly falls behind `origin/main`. The starvation clock catches the
skips that leave a *due* binary out of force; this check catches the rest,
including the runs where no staleness reading was possible at all.

Before any pre-flight check runs, check drift directly and escalate on the
outcome that matters — commits behind `origin/main` — rather than on which
skip reason fired:

```bash
gt stale --json   # commits_behind is meaningful regardless of RIG_ROOT's
                   # working-tree state — it only inspects git history
```

If `commits_behind` exceeds a threshold (default 20, override with
`REBUILD_GT_MAX_COMMITS_BEHIND`), escalate with a stable fingerprint so
repeated runs don't spam duplicate escalations while the condition persists:

```bash
gt escalate "rebuild-gt: binary is $N commits behind origin/main and has not been rebuilt" \
  -s medium \
  --source "plugin:rebuild-gt" \
  --fingerprint "rebuild-gt:drift" >/dev/null 2>&1 || true
```

`commits_behind` missing or `null` (the count could not be determined) while
`stale` is `true` is not "0 behind" — it means the drift could be 1 commit or
1000, so it escalates too, under `rebuild-gt:drift-unknown`. Reading an
unknown count as 0 is what silently retired this alarm the one time it
mattered (gt-oqbw).

## Starvation

A deferral writes no run record, so a block that repeats is invisible to the
rest of the town (gt-kox0).

Every path that leaves a *due* binary out of force — a busy gate, a merge in
flight, a refusal to build — records the block in
`daemon/rebuild-gt-state.json`: when it opened, and how many runs it has
lasted. Past `REBUILD_GT_STARVE_MINUTES` (default 30, above the longest gate
hold measured on 2026-09-22) the run escalates at high severity under the
stable fingerprint `rebuild-gt:starved`. The escalation goes out before the
block is marked escalated, so a call that never reached the town is retried
next run rather than lost for the rest of the block.

When the container-gate slot is what blocks, the run waits for it and then
has `install-gt.sh` build inside it (`--slot-role gastown/rebuild-gt`), which
runs:

```bash
gt slot run --role gastown/rebuild-gt --timeout "<what is left of REBUILD_GT_RESERVE_WAIT>s" -- make build
```

The waiting is the point: `gt slot run` alone does not queue behind a gate on
a pool with more than one slot — it takes a free slot, and the build starts
beside the load-sensitive suite the yield exists to avoid (gt-htx3). So the
run polls `gt slot status` until no gate-class role holds a slot, and acquires
after that. `REBUILD_GT_RESERVE_WAIT` (default 10m) bounds the two together,
which is why `[execution] timeout` is 25m. The install is a temp-file rename
outside the hold. A wait that gets nothing defers — nothing was built, so
nothing failed.

Reaching force — a fresh binary, or a completed install — closes the block and
clears the keys this plugin owns, under `gt escalate clear` (gt-vwry). So does
a run that finds the binary not due: a block must not outlive the condition it
measured.

## Detection

Check binary staleness:

```bash
gt stale --json
```

Parse the JSON output and check these fields:
- If `"stale": false` → record success wisp and exit early (binary is fresh)
- If `"safe_to_rebuild": false` → **DO NOT REBUILD**. Record a skip wisp and exit.
  This means the repo is on a non-main branch or HEAD is not a descendant of the
  binary commit (would be a downgrade).
- If `"safe_to_rebuild": true` → continue

At or past `REBUILD_GT_INSTALL_THRESHOLD` commits behind (default 1), install
at the first quiet moment: a merged commit that is not in force is a live
defect, not a rounding error (gt-oqbw, gt-ww20, gt-rbfj). Under the threshold
(strictly fewer commits behind than it), defer. The refinery's post-merge hook
(`scripts/install-after-merge.sh`) installs most merges within a minute of
landing; this plugin is the backstop for merges that bypass it.

An unknown `commits_behind` here is treated as *at* the threshold, not under
it — proceed with the install rather than defer. Reading "unknown" as "0
behind" left a stale, safe, quiet binary deferred every heartbeat forever: the
threshold gate could never be satisfied by a count `gt stale` couldn't
produce, and nothing else in the run would ever install it (the drift
escalation above still fires independently, but firing an alarm is not the
same as fixing the staleness).

## Pre-flight Checks

Before building, verify the source repo is clean and on main:

```bash
cd ~/gt/gastown/mayor/rig
git status --porcelain --untracked-files=no -- . ':(exclude).beads'  # No tracked changes that affect the build
git branch --show-current  # Must be "main"
```

If either check fails, skip the rebuild and record a wisp.

## Sync with origin/main

The rig checkout has no self-serve pull otherwise: without this step the
build uses whatever commit a human last checked out, and `make safe-install`
fails its `check-up-to-date` gate against `origin/main` on every run until a
human pulls manually.

```bash
cd ~/gt/gastown/mayor/rig
git fetch origin --quiet
git merge --ff-only origin/main --quiet
```

`--ff-only` is load-bearing: if local `main` has diverged from
`origin/main`, the merge fails and the plugin must skip the rebuild and
record a skip wisp with reason "local main diverged from origin/main" —
**never** `git reset --hard` to force it.

The fetch and fast-forward are writes to `mayor/rig`, so they run under
`install-gt.sh`'s flock (`daemon/install-gt.lock`, claude-7fc) and cannot move
the tree under a build the post-merge hook is running. The plugin waits up to
`REBUILD_GT_LOCK_WAIT` seconds (default 30) for it; a lock still busy means an
install is running right now, so the run defers (exit 3). The lock is released
before `install-gt.sh` runs, which takes it again itself.

## Quiet gate

`make build` competes for CPU with a gate suite whose tests are load-sensitive
(gt-htx3), so the build waits for a town with nothing in flight:

- no gate-class role holding a container-gate slot, no container running
  outside the gate, and no saturated pool (`gt slot status --json`), and
- no MR a refinery is mid-merge on
  (`gt mq list gastown --status=in_progress --json`).

Under `REBUILD_GT_STARVE_MINUTES` either reading defers the run; past it the
first is waited out (Starvation above).

There is no second reading before the install. It existed because the install
ended in a daemon restart; `install-gt.sh` only renames the binary and leaves
the restart to the daemon's idle point (claude-7fc).

Two readings that are deliberately not deferrals. An MR merely ready in the
queue consumes nothing, and at this town's merge rate the queue is never
empty, so requiring an empty queue would leave the install waiting forever.
A `docker_unknown` status means the cross-check could not tell, and a VM that
is down runs no suite for a build to compete with.

## Action

Run the rig's own `scripts/install-gt.sh --sha <HEAD> --source rebuild-gt`
(plus `--slot-role gastown/rebuild-gt` past the starvation threshold), the one
install path the refinery's post-merge hook also uses. Under one flock it
builds, installs atomically (`scripts/install-binary.sh`, gt-0het), keeps the
previous binary as `gt.prev`, and verifies the commit in force the way this
plugin used to — the short commit the binary reports is resolved to a full
hash inside the rig before comparing (gt-oqbw, gt-b5mpe). On a failed check it
restores `gt.prev` and escalates (`install-gt:smoke-failed`,
`install-gt:rollback-failed`). Then `gt formula sync` and `gt plugin sync`
(non-fatal), then `daemon/restart-pending.json`.

The daemon is **not** restarted here. It reads the marker at each heartbeat
and exits (code 75, restarted by launchd) at a moment with no plugin, dog or
main-branch gate in flight, so an install never kills in-flight work.

## Daemon in force (backstop)

Each run also checks that the daemon came into force: a restart-pending
marker older than `REBUILD_GT_DAEMON_LAG_MINUTES` (default 30), or a daemon
whose `state.json` commit is not a descendant of the installed binary's while
that binary has been in place that long, escalates
`rebuild-gt:daemon-not-in-force` (HIGH). It clears once both readings are
healthy.

## Record Result

The receipt names the commits brought into force — `in force <old> -> <new>
(N commits)` plus their subjects. install-gt also appends a line to
`daemon/install-receipts.jsonl`. A refusal escalates only through the
starvation check's `rebuild-gt:starved`; reaching force — or a run that finds
the binary not due — clears `:starved`, `:drift`, `:drift-unknown`,
`:unverified` and `:no-installer` together.
