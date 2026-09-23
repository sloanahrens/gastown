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

Run `run.sh` exactly; its exit code and its stdout are the result. Read the rest
of this file only to interpret a failure, or before editing the script.

Per-worker trends from the om editorial gate's own verdicts, read from the notes
it publishes on `refs/notes/om`.

The notes rather than the `quality-review-result` receipt wisps, because the note
is the gate's durable proof: it survives wisp GC, DB flattens and MR-bead
deletion, and it carries the whole verdict — score, findings, attempts,
follow-ups — rather than a summary of it. A prose plugin had to re-derive the
numbers from the receipt stream on every dispatch, and the numbers drifted
between dispatches of the same window (gt-gs7g).

## What it reports

Per worker, over a rolling window (24h by default,
`GT_QUALITY_REVIEW_WINDOW_HOURS`): reviews, first-attempt approve rate, mean
score, mean findings count, major findings, request-changes verdicts, and a
trend from the window's first half against its second.

A review is one reviewed diff, not one note. The gate copies its note onto the
landed commit on every stacked or rebased merge, and a re-roll leaves the note it
replaced beside the new one, so the notes on the ref outnumber the verdicts
they record by about a third. The count is keyed on the gate's `patch_id`.

Status bands on the mean score: OK at 0.60 and above, WARN from 0.45, BREACH
below it. A first-attempt approve rate under 50% caps an OK worker at WARN —
the score band measures the reviewer's number, not the rework the gate absorbed
before the diff landed.

Past the table, one JSON object per worker goes to stdout, so the model-trial
scoring reads the same numbers this run reported.

Only operational rigs are read, and only ones that have recorded a verdict: a
parked rig and a rig with no editorial gate contribute nothing and say so.

## Alerts

A BREACH mails the deacon and escalates under the stable per-worker key
`quality-review:breach:<rig>/<worker>`. It mails the deacon rather than the
prose plugin's `mayor/` because the keyed escalation already routes to the
mayor, and two addresses for one condition get acknowledged twice and acted on
once.

A later run closes the key for any worker not in breach, including one with no
reviews in the window at all: the sweep covers every worker seen in the last
seven windows. A run that reports also closes `quality-review:failed`.

## Exit codes

| exit | when |
|------|------|
| 0 | measured, or the window is genuinely empty — either way a receipt is recorded |
| 1 | the run could not measure: `jq` or `git` missing, the town root or rig registry unusable, a checkout missing, a `git fetch`/`notes list`/`cat-file` call erroring, or notes were read and none of them could be placed in the window or attributed to a worker |

Exit 2 is never used. An empty window is a result and cannot be mistaken for a
failure; a window that could not be read is never reported as empty. Half a
window of unattributable notes is a schema that moved, not a quiet week, and it
exits 1 so the failure surfaces where the numbers used to.

A run with no operational rig to read prints `[plugin-result skipped]` and
records a `skipped` receipt rather than a success: it measured nothing, and a
no-op that records success green-checks the plugin's history and spends its
cooldown. An empty window keeps its success receipt, because an empty window is
a measurement — of a window that holds no verdicts.

## The gate's ref is read, never written

Rig clones are worktrees of one `.repo.git` and a notes ref is not per-worktree,
so `refs/notes/om` in a rig clone is the ref the refinery writes and publishes
from. A read-only report that fetched into it would overwrite the proof, and
could discard a verdict the gate has written but not yet pushed.

The script fetches origin's `refs/notes/om` into `refs/notes/quality-review-om`
and reads that.

## Run

```bash
./run.sh        # measure, report, alert
./run_test.sh   # fixture and failure-path tests
```
