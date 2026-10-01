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

## Configuration

`patrols.steward` in `mayor/daemon.json`, off unless `enabled` is true:

```json
{"enabled": true, "interval": "60s", "max_jobs": 2, "job_timeout": "45m",
 "routine_agent": "deepseek-flash", "hard_agent": "claude-opus",
 "rigs": ["gastown"], "work_root": "/tmp/gt-steward"}
```

The concurrency cap and the one-job-per-bead rule hold across scans for the
lifetime of the daemon. `routine_agent` runs the first attempt; a job retries
once on `hard_agent` after a failure, and a conflict rejection starts there.
`work_root` must be outside the town: git refuses a worktree inside it.

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
