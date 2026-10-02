# Steward jobs

The steward runner (gt-9bioi.1) is the daemon's side of the steward: it spawns
one headless agent session per landing-queue event and records what each one
did. The jobs themselves — the review prompt, the rejection handling, what a
job may push — are gt-9bioi.2; monitoring is gt-9bioi.3.

## Events

The scan runs every `interval` over each rig's open beads:

| Event | Trigger | The job gets |
|---|---|---|
| `review` | a bead carries `gt:ready-to-land` with a head no job has seen | the READY TO LAND block: branch, head, target, author |
| `rejection` | a bead carries `rework` and a MERGE REJECTION block | the refusal: kind, reason, findings, the rejected head |

A head a job has already run on is not run again, so a resubmission (a new
head) earns a new job and a failed fix does not loop. A bead assigned to a
crew member, or labeled `gt:needs-human`, is left alone.

While the patrol runs live it owns every rejected bead: a rework bead is the
steward's to settle, and `gt sling` refuses to dispatch one without `--force`
so the convoy feeders and seat-refill cannot spend a second polecat on work a
job is already settling (gt-28ibg). In shadow the patrol owns nothing — its
jobs change no branch, label or note — so rejections dispatch as they did
before it existed. A town whose steward scans land in a narrow `rigs` list
owns only those rigs' rejections.

## Configuration

`patrols.steward` in `mayor/daemon.json`, off unless `enabled` is true:

```json
{"enabled": true, "mode": "shadow", "interval": "60s", "max_jobs": 2, "job_timeout": "45m",
 "routine_agent": "deepseek-flash", "hard_agent": "deepseek-pro",
 "rigs": ["gastown"], "work_root": "/tmp/gt-steward"}
```

`mode` is `shadow` (the default) or `live`; see Rollout.

The concurrency cap and the one-job-per-bead rule hold across scans for the
lifetime of the daemon. `routine_agent` runs the first attempt; a job retries
once on `hard_agent` after a failure, and a conflict rejection starts there.
`work_root` must be outside the town: git refuses a worktree inside it.

## Rollout: shadow, then live

Turn the steward on in shadow, compare it with the overseer for a day, then
set `"mode": "live"` and restart the daemon. Run in shadow until its verdicts
match the overseer's on every event of a full day; a disagreement is a prompt
bug to fix before live.

| | shadow (default) | live |
|---|---|---|
| Review job | runs the five checks, comments `STEWARD (shadow) REVIEW PASS` or `FAIL` | comments `STEWARD REVIEW PASS` or `FAIL` |
| Rejection job | merges, repairs, lints and gates in its worktree, then comments `STEWARD (shadow) FIX`, `RESLING` or `REQUEUE` naming what it would have done | pushes the branch, requeues, re-slings |
| Cannot decide | comments `STEWARD (shadow) ESCALATE` | files the hq escalation |
| Labels, notes, remote branches | untouched | changed by the steps above |

A shadow job reports the outcome the live action would have had, so
`gt steward status` and the ledger count shadow and live verdicts in the same
words. The ledger's `mode` field tells them apart and `gt steward status` shows
the current mode and how many jobs in the window were shadow runs. A
`patrols.steward.mode` that is neither word runs as shadow and logs why.

The runner's own alerts do not depend on the mode: a hard-preset job, a stuck
job and a high error rate raise their escalations in shadow too.

**Comparing.** For each bead with a `STEWARD (shadow)` comment, set it beside
the overseer's own review or rejection handling of the same head: PASS against
pass, FAIL against fail, and for a rejection the kind of action (fix, re-sling,
requeue). The agreement rate is matching events over events both sides saw,
and the overseer's hourly report quotes it beside the `gt steward status
--json` counts.

**Switching.** Only jobs of the current mode spend an event. A head a shadow
job reviewed is reviewed again by a live job after the switch, and a rejection
still waiting then gets its live fix. A restart is needed because the daemon
reads the patrol config at startup.

## The job ledger

`<town>/.runtime/steward/jobs.jsonl`, one JSON object per line, written when a
job starts and again when it ends:

```json
{"id":"steward-2891","event":"review","bead":"gt-x","rig":"gastown",
 "branch":"polecat/emerald/gt-x","head":"c0ffee","model":"deepseek-flash",
 "started":"2026-10-01T18:42:00Z","ended":"2026-10-01T18:51:00Z",
 "outcome":"pass","summary":"reviewed","transcript":"/…/session.jsonl"}
```

A job reports its own `outcome` in `steward-result.json` in its worktree; the
runner records `error` when a job writes none. A row with no `ended` is a job
whose daemon died — the next daemon closes it at startup. The daemon logs one
`steward: start …` line and one `steward: end …` line per job, visible in
`gt tail`.

The ledger does not grow without bound: when the runner starts it drops every
job that ended more than seven days ago, whole. A job still running is kept
whatever its age, because a scan reads its row as the bead being busy.

A job whose preset's env names a `${VAR}` neither `settings/daemon.env` nor
the daemon's own environment defines fails before it starts, with the variable
named in the ledger's summary: an unresolved reference is an empty credential,
and the provider's auth error would look like a broken job.

## Monitoring

`gt steward status [--since 1h] [--last N] [--json]` summarizes the ledger: jobs
by outcome and by model, median and max duration, running and stuck jobs, the
escalations raised, the pro (hard-preset) job count, and the newest jobs with
bead and outcome. The overseer's hourly report quotes the `--json` object
(`jobs`, `outcomes`, `broke`, `pro`, `median_seconds`, `stuck`, `alerts`) next
to its audit agreement rate.

A job is *stuck* when it runs a minute past `job_timeout`: the runner kills at
the timeout, so an open row past it is a job the kill did not end. "Broke"
means error or timeout; a `fail` verdict is the steward working, and an
`interrupted` job (a daemon restart) is not an attempt.

`townhealth.json` carries the last hour's counters under `steward`, and the
`steward` field turns red for a stuck job and degraded when more than
`operational.health.steward_error_rate` (default 0.5) of the hour's attempted
jobs broke, once the hour holds `steward_min_jobs` (default 3) attempts.
`gt status --line` shows it like any other non-green field.

The daemon raises each of these escalations once, recorded in
`<town>/.runtime/steward/alerts.jsonl` (`gt steward status` counts them as
`alerts`):

| Escalation | Severity | When |
|---|---|---|
| `pro:<job>` | low | every job that ran on `hard_agent`, with why it was the hard one |
| `stuck:<job>` | medium | a job past its timeout plus the one-minute kill grace |
| `error-rate:<job>` | medium | the error rate over the threshold; keyed to the episode's first broken job |
