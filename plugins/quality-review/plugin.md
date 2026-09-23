+++
name = "quality-review"
description = "Per-worker om editorial quality trends from refs/notes/om"
version = 2

[gate]
type = "cooldown"
duration = "6h"

[tracking]
labels = ["plugin:quality-review", "category:quality"]
digest = true

[execution]
type = "script"
timeout = "5m"
notify_on_failure = true
severity = "medium"
+++

# Quality Review

Per-worker trends from the om editorial gate's own verdicts, read from
`refs/notes/om`: the note the gate writes on each reviewed head and pushes.

The notes rather than the `quality-review-result` receipt wisps, because the
note is the gate's durable proof — it survives wisp GC, DB flattens and MR-bead
deletion, and it carries the whole verdict (score, findings, attempts,
follow-ups) rather than a summary of it. A prose plugin had to re-derive the
numbers from the receipt stream on every dispatch, and the source of those
numbers drifted between runs (gt-gs7g).

This is a script plugin. The daemon runs `run.sh` in-process; no dog interprets
these instructions.

## What it reports

Per worker, over a rolling window (24h by default,
`GT_QUALITY_REVIEW_WINDOW_HOURS`): reviews, first-attempt approve rate, mean
score, mean findings count, major findings, request-changes verdicts, and a
trend from the window's first half against its second.

Status bands on the mean score: OK at 0.60 and above, WARN from 0.45, BREACH
below it. A first-attempt approve rate under 50% caps an OK worker at WARN —
the score band measures the reviewer's number, not the rework the gate absorbed
before the diff landed.

A BREACH mails the deacon and escalates under the stable per-worker key
`quality-review:breach:<rig>/<worker>`; a later window where that worker is not
in breach closes the key.

Past the table, one JSON object per worker goes to stdout, so the model-trial
scoring reads the same numbers this run reported.

Only operational rigs are read, and only ones that have recorded a verdict: a
parked rig, a rig with no editorial gate, and a rig whose window is empty all
contribute nothing and say so.

## Exit codes

| exit | when |
|------|------|
| 0 | measured, or the window is genuinely empty — either way a receipt is recorded |
| 1 | a read failed: `jq` or `git` missing, the rig registry unusable, a checkout missing, or a `git fetch`/`notes list`/`notes show` call erroring |

2 is never an outcome. An empty window is a result and cannot be mistaken for
a failure; a window that could not be read is never reported as empty.

## Run

```bash
./run.sh        # measure, report, alert
./run_test.sh   # fixture and failure-path tests
```
