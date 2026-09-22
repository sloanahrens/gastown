+++
name = "quality-review"
description = "Review merge quality and track per-worker trends"
version = 1

[gate]
type = "cooldown"
duration = "6h"

[tracking]
labels = ["plugin:quality-review", "category:quality"]
digest = true

[execution]
timeout = "5m"
notify_on_failure = true
severity = "medium"
+++

# Quality Review — Trend Analysis

This plugin runs every 6h during Deacon patrol. It analyzes quality-review result
wisps recorded by the Refinery during merges, computes per-worker trends, and
alerts on quality breaches.

## Step 1: Query recent quality-review results

Fetch all quality-review result wisps from the last 24 hours.

**`--include-infra` is required.** These receipts are ephemeral wisps (recorded
via `gt plugin record-run`, see the reference section below), and `bd list`
hides ephemeral beads by design. Omitting the flag makes this query silently
return `[]` even when recent results exist — which is exactly what made every
run report "No results in last 24h" for days while breaches went unalerted.
The canonical recorder uses the same flag (`internal/plugin/recording.go`).

```bash
bd list --json --all --include-infra -l type:plugin-run,plugin:quality-review-result --created-after=-24h
```

If no results are found, do NOT immediately record a clean success. An empty
result is ambiguous: it can be a genuine "nothing in the window" OR a broken
query. Disambiguate with the independent history path:

```bash
gt plugin history quality-review-result --json
```

- **Both empty** — the window genuinely has no results. Record a run wisp and stop:

```bash
gt plugin record-run --plugin quality-review --result success \
  --title "quality-review: No results in last 24h" \
  --description "No quality-review results in last 24h. Nothing to analyze." >/dev/null 2>&1 || true
```

- **History non-empty, query empty** — the Step 1 query is broken. A failed
  measurement must NOT serialize as success. Record a failure and escalate; do
  not record a clean "success":

```bash
gt plugin record-run --plugin quality-review --result failure \
  --title "quality-review: Step 1 query returned [] but history is non-empty" \
  --description "Step 1 query returned empty while 'gt plugin history quality-review-result' shows recent runs. The query is broken." >/dev/null 2>&1 || true

gt escalate "quality-review: empty-result anomaly" \
  --severity medium \
  --reason "Step 1 query returned [] for window <start>..<end>, but 'gt plugin history quality-review-result' returned <n> runs (latest <wisp-id>); the query is broken."
```

## Step 2: Compute per-worker trends

Parse the wisp labels to extract per-worker data. Each result wisp has labels:
- `worker:<polecat-name>`
- `rig:<rig-name>`
- `score:<0.0-1.0>`
- `recommendation:<approve|request_changes>`

For each worker, compute:
- **Average score** across all results in window
- **Rejection rate**: count of `recommendation:request_changes` / total
- **Trend direction**: Compare first-half avg vs second-half avg of the window
  - Difference > 0.05: `improving`
  - Difference < -0.05: `declining`
  - Otherwise: `stable`

## Step 3: Classify worker status

Apply thresholds to each worker's average score:
- **OK**: avg >= 0.60
- **WARN**: 0.45 <= avg < 0.60
- **BREACH**: avg < 0.45

## Step 4: Alert on breaches

For each worker in BREACH status, send an alert:

```bash
gt mail send mayor/ -s "Quality BREACH: <worker>" -m "Worker: <worker>
Rig: <rig>
Avg Score: <avg>
Reviews: <count>
Rejection Rate: <rate>%
Trend: <improving|stable|declining>

Action: Review recent merges from this worker for quality issues."
```

Also escalate:

```bash
gt escalate "Quality BREACH: <worker> (avg: <avg>)" \
  --severity medium \
  --reason "Worker <worker> in rig <rig> has avg quality score <avg> over <count> reviews (threshold 0.45), window <start>..<end>, wisps: <wisp-id>,<wisp-id>"
```

## Step 5: Record run result

Record a summary wisp for this plugin run:

```bash
gt plugin record-run --plugin quality-review --result success \
  --title "quality-review: Analyzed <N> workers over <M> reviews" \
  --description "Analyzed <N> workers over <M> reviews. <B> breaches, <W> warnings." >/dev/null 2>&1 || true
```

If any step fails unexpectedly, record a failure wisp and escalate:

```bash
gt plugin record-run --plugin quality-review --result failure \
  --title "quality-review: FAILED" \
  --description "<error description>" >/dev/null 2>&1 || true

gt escalate "Plugin FAILED: quality-review" \
  --severity medium \
  --reason "Failed at <step> over window <start>..<end>, run wisp <wisp-id>: $ERROR"
```

## Escalation evidence requirements (REQUIRED)

This plugin files three escalations: `quality-review: empty-result anomaly`
(Step 1), `Quality BREACH: <worker>` (Step 4), and `Plugin FAILED:
quality-review` (Step 5). Each one must carry the evidence needed to falsify it
without re-running anything. Put in `--reason`:

- **Wisp IDs** you actually read — the result wisps and/or the run wisp, as IDs,
  not a summary of them.
- **Measurement window** — the concrete start and end timestamps scanned, not
  "last 24h".
- **The numbers the verdict rests on** — for a breach: worker, rig, avg score,
  review count, and the Step 3 threshold it crossed; for the empty-result
  anomaly: what the Step 1 query returned against what
  `gt plugin history quality-review-result` returned.

Rationale: a body that says only "suggests the harness is out of sync"
(hq-wisp-0g6094) makes the reader re-derive what the reporter already held. IDs,
a window, and the numbers make the claim checkable in one step, and separate a
non-reproducible transient from a live defect.

**Re-measure before re-filing.** The Step 1 window is a rolling 24 hours and the
Refinery writes new result wisps continuously, so a verdict computed earlier may
no longer hold. Before re-filing a breach or an empty-result anomaly for the same
worker or condition, re-run the Step 1 query over the CURRENT window. A finding
that reproduces in the current window is new evidence; the same finding quoted
from a window that has since moved on is not, and re-filing it unchanged is noise.

Done when the `--reason` names the wisps, the window, and the numbers.

Escalations about the pre-merge om gate itself (a `backend_timeout` or
`version_mismatch` from the refinery's review) are not this plugin's to file.
Those carry their own evidence requirements — see
`contrib/gastown/directives/refinery.md`.

---

## How scores get recorded (reference)

This plugin does NOT record scores itself. The Refinery records result wisps during
merges via the `quality-review` formula step. Each merge produces a wisp like:

```bash
gt plugin record-run --plugin quality-review-result --result success --rig <rig-name> \
  --label worker:<polecat-name> --label score:0.85 --label recommendation:approve \
  --title "quality-review: Score 0.85, approve" \
  --description "Score: 0.85, approve. Issues: 1 minor (style)" >/dev/null 2>&1 || true
```

This creates the data that Step 1 queries.
