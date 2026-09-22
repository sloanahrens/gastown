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
  --reason "Step 1 query returned [] but plugin history shows recent result wisps; the query is broken."
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
  --reason "Worker <worker> in rig <rig> has avg quality score <avg> over <count> reviews"
```

## Escalation evidence requirements (REQUIRED)

Every escalation this plugin files carries the evidence needed to falsify it
without re-running anything. Put in `--reason`:

- **Wisp IDs** you actually read (the result wisps and/or the run wisp), not a summary of them.
- **Versions you observed**: for the `om` rig, the installed om binary sha and the manifest's
  `om_binary.sha256`; for any rig, its rubric pin against the rubric sha seen.
- **Measurement window**: the start and end timestamps scanned.

Rationale: a body that says only "suggests the harness is out of sync" (hq-wisp-0g6094) makes the
reader re-derive what the reporter already held. Wisp IDs and shas make the claim checkable in one
step, and separate a non-reproducible transient from a live defect.

**Re-measure before re-filing.** Before filing a `backend_timeout` or `version_mismatch`
escalation, re-check the CURRENT budget and the CURRENT shas: the installed sha may already match
the manifest, and a rig's budget may already have been raised (om's review timeout went
300s -> 900s, om-cwy). A failure at the CURRENT setting is new evidence; the same failure at a
setting that has since changed is not, and re-filing it unchanged is noise.

Done when the `--reason` names the wisps, the shas, and the window.

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
  --reason "$ERROR"
```

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
