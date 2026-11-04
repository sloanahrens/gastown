> Status: current (2026-11). The operator's runbook for `gt update`: what the job does, how to run and read it, how to switch the daemon's trigger, and what to do when a run dies or ends badly.

# gt update: the operator's runbook

`gt update` installs the latest gt build and its sandbox image, runs the town's
maintenance, waits for the daemon to restart, verifies the town, and lifts the
dispatch hold — one job, in its own process. A run holds the update lock under
`~/gt/.runtime/update/`, so two runs cannot overlap; a second one exits at once
and names the run holding it.

Before the first hand run on a town, read [Installing gt](../INSTALLING.md): it
covers installing the binary and the daemon, and the restart command used
below.

## What a run does

The run decides a plan from the town's state — the installed build against
main, whether the image is published, whether the archive is behind, whether
today's backup exists, whether a gc is due — and prints one line per step
saying whether it will run and why. `--dry-run` stops after the plan.

| Step | What it does | When it skips |
|------|--------------|---------------|
| drain | Takes the dispatch hold if no hold exists, then waits up to 30 minutes for working polecats to finish. A working seat silent for 5 minutes counts as drained; no seat is ever stopped. | No install is planned. |
| maintenance | Asks the daemon for one maintenance cycle (history archive, backup, gc) and waits up to 30 minutes for its answer. | Nothing is due: the archive is current, today's backup exists, and no gc is due. |
| install | Builds and installs the target build and publishes its sandbox image. | The installed build is main and its image is published. |
| restart | Waits up to 15 minutes for the daemon to report the new build. | No install is planned. |
| verify | Checks the installed gt, the running daemon and the image label against the target build, and that the town health line reads green. | No install is planned, or the restart timed out. |
| lift | Releases the dispatch hold if this run created it. | No install is planned; a hold someone else set is left in place. |

A run ends:

- **ok** when the run finished with nothing failed, deferred or aborted; work
  that was not due was skipped.
- **partial** when a step fell short without stopping the run: a maintenance
  cycle that deferred or failed, a restart that timed out, or a verify check
  that failed. An install the plan made still happened.
- **aborted** when the run stopped without installing: the drain did not
  finish, the hold could not be taken, or the installer was busy.
- **failed** when the install failed — the town keeps running on the old
  binary — or when a hold the run owned could not be released, which leaves
  dispatch stopped until someone removes the file.
- **interrupted** when the run received Ctrl-C or SIGTERM. A hold the run owns
  is released on the way out.

## Running it by hand

Print the plan first; it writes no record and runs no step:

```bash
gt update --dry-run
```

Then run it:

```bash
gt update
```

```bash
gt update --skip maintenance
gt update --only install
gt update --force
```

- `--skip <step>` skips a step and is repeatable. Skipping install also skips
  drain, restart, verify and lift; skipping drain also skips lift.
- `--only <step>` keeps only the named steps and is repeatable. A named step
  the plan found no work for still skips; `--force` is what makes an install
  run.
- The selectable steps are `drain`, `maintenance`, `install`, `restart`,
  `verify` and `lift`. The plan itself is always decided before them.
- `--force` plans an install even when the installed build is main and its
  image is published.
- `--json` prints the final record as JSON instead of the text report.
- `--from-daemon` records the run's source as daemon. The daemon's trigger
  passes it; a hand run leaves it off.

| Exit | Meaning |
|------|---------|
| 0 | the run ended ok, or `--dry-run` printed the plan |
| 1 | the run failed, or it could not make its plan or write its record |
| 3 | the run ended partial |
| 4 | the run was aborted |
| 5 | another run holds the update lock; nothing ran |
| 130 | the run was interrupted |

## Reading a run

`gt update status` prints the current or last run's record. It takes no lock,
writes nothing and runs nothing.

```bash
gt update status
```

```text
run 20261103T195300Z-1a2b3c4d (manual): running
started 2026-11-03T19:53:00Z, last heartbeat 1m30s ago
target 01234567 (installed fedcba98)
hold: gt update 20261103T195300Z-1a2b3c4d: draining
  drain: ok
  install: running
```

A running record prints its start and heartbeat age; a finished one prints when
it ended. The hold line appears only when the run owns the hold. With no
record, the command prints `no gt update has run`.

```bash
gt update status --json
```

`--json` prints the whole recorded state as indented JSON — the record the
daemon and the dashboard read.

The dashboard draws the run as a chip in the header beside the hold chip. While
a run is in flight the chip is amber and reads `update: install 8m`, or
`update: starting 5s` before the first step names itself. Past three minutes
without a heartbeat it reads `update stalled`. A finished run that ended
failed, partial or aborted keeps an amber chip naming the outcome; an
interrupted run is quiet; an ok run, and a town with no update record, shows no
chip. Hovering a chip lists the run id and source, then one line per step with
its result and detail; the stalled chip leads with the time since the last
heartbeat.

Files under `~/gt/.runtime/update/`:

| File | Holds |
|------|-------|
| `state.json` | The current run's record: run id, source, start, heartbeat and end times, target and installed builds, each step's plan, result and detail, the current step, the hold and its note, and the outcome. Rewritten atomically at every step; `gt update status` and the dashboard read it. |
| `state.json.next` | The staging file of a write in progress; absent except during a publish. |
| `history.jsonl` | One JSON line per finished run: the same record plus `durations_ms` for each step. |
| `lock` | The exclusive lock a running job holds. The kernel drops it when the process exits, so a dead job leaves no lock. |
| `maintenance-request` | The job's request for a maintenance cycle, with the run id. The daemon removes it when it answers completed or failed; a deferred or interrupted cycle leaves it for the next tick. |
| `maintenance-result.json` | The daemon's answer — completed, deferred or failed — with the detail. Written before the request it answers is removed; a deferred or interrupted cycle leaves it in place. |
| `launch-<timestamp>.log` | The output of a run the daemon launched, one file per launch. A hand run writes its output to the terminal. |

## The daemon trigger

The daemon decides when an update is due. The trigger is off unless
`patrols.update.enabled` is true. The block is in the town's
`settings/config.json`, under `daemon` — operator-owned runtime settings, not a
file in this repository. A town not yet on the two-file layout keeps the same
document in `~/gt/mayor/daemon.json`.

```json
{
  "daemon": {
    "patrols": {
      "update": {
        "enabled": true,
        "window": "03:00",
        "min_interval": "3h",
        "max_runtime": "2h"
      }
    }
  }
}
```

| Key | Default | Meaning |
|-----|---------|---------|
| `enabled` | `false` | Whether the daemon decides when an update is due. Absent means off. |
| `window` | `03:00` | The daily start, `HH:MM` local time with two-digit hour and minute. The window stays open for one hour. |
| `min_interval` | `3h` | How long after a run ends before main moving with a landing waiting makes the next run due. A Go duration from `30m` to `24h`. |
| `max_runtime` | `2h` | How long a run may last before the daemon escalates `update:overrun`. A Go duration from `10m` to `12h`. |

An update is due once a day inside the window that opens at `window`, when no
run has started since it opened; or at any time when main differs from the
installed build, at least one landing waits to deploy, and `min_interval` has
passed since the last run ended. The daemon launches nothing while a run is in
progress, while an install marker is pending, or in standby — the town ESTOP or
a waned Ring.

The daemon reads the block at start, so a change needs a daemon restart. Restart
it between landing passes:

```bash
gt daemon restart
```

Switching `enabled` to false hands the decision back: `rebuild_gt` makes install
decisions again, and the maintenance window tick starts the nightly cycle in its
own window (`patrols.scheduled_maintenance.window`) — the old `03:00` schedule.

## When it goes wrong

The daemon raises three escalations at MEDIUM, under source `update`. Each key
is stable, so a repeat lands on the escalation already open, and the clear names
the same key. They are checked on every daemon heartbeat even with the trigger
switched off — a hand-started run can die too — and the town ESTOP and a waned
Ring do not silence them.

| Key | When it is raised | What it means |
|-----|-------------------|---------------|
| `update:stale` | A record still says `running` and its heartbeat is more than 3 minutes old, or it has none. | The job is dead, not slow: it refreshes the heartbeat every 15 seconds. The message names the run, its current step, the heartbeat age, the hold note if the run owns the hold, and the run log. It clears when the heartbeat resumes or the run ends. |
| `update:overrun` | A running run has lasted longer than `max_runtime`. | The run may still be alive — check the heartbeat. A long install or maintenance step trips it. The message names the step, the elapsed time, the limit and the log. It clears when the run ends. |
| `update:last-run-failed` | The current record ended failed, partial or aborted within the last two hours. | Names the run, its outcome and every failed or deferred step with the detail. It clears when a later run replaces the record, or when the two hours pass. An ok or interrupted run never raises it. |

The stale and overrun messages name `.runtime/update/run-<run-id>.log`, a file no
run writes. A daemon-launched run writes its output to `launch-<timestamp>.log`
in the same directory, stamped at launch: the latest one before the run's start
time in `gt update status` is the run's log. A hand run writes to its terminal.

### A dead job or a slow step

`gt update status` shows the heartbeat age while a run is in flight. The
heartbeat comes from a 15-second timer that runs for the whole run, not from the
steps, so a heartbeat that stops advancing means the process is gone or wedged.
A heartbeat no more than three minutes old means the job is alive, possibly on a
slow step; the `update:overrun` escalation covers a run that is merely long. A
heartbeat older than three minutes has missed twelve ticks: the job is dead or
wedged. To see whether a process is still there:

```bash
pgrep -fl 'gt update'
```

### Removing the hold a dead run left

A run that owned the dispatch hold and died leaves the file behind, so dispatch
stays stopped. The stale alert names the run and the hold note. Read the note,
then remove the file:

```bash
cat ~/gt/seat-refill.hold
rm ~/gt/seat-refill.hold
```

Remove a hold only when its note names a dead `gt update <run id>: draining`
run. A hold with any other note is someone else's; leave it to its author.

Removing the file lets dispatch resume but does not clear the stale alert: the
record still says `running`. Run the job again. It takes the lock the dead
process dropped, replans from the town's current state so finished work is
skipped, and replaces the record, which clears the alert.

A hold set by hand is never cleared by the job: the job releases only the file
it created itself. If a hold already exists when the run starts, the run drains
and installs under it, records the hold as not its own, and leaves it in place.
Lift your own hold when its work is done.

### "installed, restart pending"

When the daemon does not report the new build within 15 minutes of the install,
the restart step records `installed, restart pending`, verify is recorded
skipped, and the run ends partial. The new binary is installed; the daemon
still runs the old build until it restarts. The install leaves a restart marker
for the daemon, and the daemon restarts itself once it is idle. The hold is
released, and the partial run raises
`update:last-run-failed` naming the deferred restart step.

Nothing needs doing while the marker stands. If the marker has waited 30 minutes
for an idle daemon, the daemon escalates the wait and keeps waiting. Then restart
it by hand at a quiet moment, and confirm the town is on the target build:

```bash
gt daemon restart
gt version
gt update status
```

The next run's verify checks gt, the daemon, the image label and the health line
together.

### Other bad endings

- An aborted drain names the seats still working, or the seat probe's error;
  run it again when the seats are idle.
- An aborted install when the installer is busy means another process holds the
  install lock or a container-gate slot; wait, then run it again.
- A failed install leaves the old binary running; the step's detail carries the
  installer's reason.
- A partial verify names the check that did not match; the town is not fully on
  the target build until they all pass.
- A run that could not take or release the dispatch hold names the hold in its
  step detail. Check `~/gt/seat-refill.hold`: fix its permissions, or remove it
  if it is stale. Dispatch stays stopped while the file is there.
- A partial maintenance means the maintenance cycle fell short; an install the
  plan made still went ahead, and a later maintenance cycle picks the work up.
  The step's detail says what stopped.
