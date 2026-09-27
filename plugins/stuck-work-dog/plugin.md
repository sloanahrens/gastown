+++
name = "stuck-work-dog"
description = "Detects unassigned beads stalling the merge queue and blocked MRs, and escalates"
version = 1

[gate]
type = "cooldown"
duration = "10m"

[tracking]
labels = ["plugin:stuck-work-dog", "category:health"]
digest = true

[execution]
type = "script"
timeout = "5m"
notify_on_failure = true
severity = "medium"
+++

# Stuck Work Dog

Detects stuck **WORK**, as distinct from stuck **workers**. `stuck-agent-dog`
watches for crashed or zombie polecats; nothing watched for the other
failure mode. This plugin is read-only: it only escalates. It never
assigns, restarts, or dispatches anything.

## How this runs

The daemon runs `run.sh` directly (`[execution] type = "script"`, claude-l5w):
both detectors are exact joins with fixed thresholds, so an agent adds nothing
to the all-clear case. A dog reads these steps only when `run.sh` exits
nonzero, including when an escalation it raised failed to send
(`ESCALATION FAILED` in the output).

## Origin

Filed from `gt-ck0c`. A merge request sat blocked for two hours on a
dependency bead that was open and unassigned. The rig idled behind it —
zero polecats working, merge queue not progressing — and nothing raised a
signal. In the same window, an unrelated false-positive detector fired
thirty restart requests about a healthy, idle polecat that needed nothing.
The asymmetry is the point: a loud detector that watches workers, and total
silence on the condition that actually stopped the rig.

Two cheap, read-only checks close that gap:

1. **Blocked MR → open + unassigned dependency.** The narrowest, most exact
   signal. A blocked MR already records what it's waiting on, so the join
   is trivial: walk each open MR's `blocked_by` list and check whether the
   blocker itself is `open` with no `assignee`.
2. **Merge queue stalled.** The broader symptom: the queue has entries, none
   are `in_progress`, and no polecat in the rig has any `in_progress` work
   either. This catches the same incident even when the specific blocker
   bead isn't itself the direct dependency of a queued MR.

A bead in a blocking position with an empty assignee for a long time is
itself always worth surfacing; detector 1 is the special case of that which
is cheapest to compute and gives the sharpest target (an MR id and a bead
id), so it is checked first.

## Step 1: Resolve town and enumerate operational rigs

```bash
TOWN_ROOT="${GT_TOWN_ROOT:-$(gt town root)}"
RIG_LIST=$(gt rig list --json | jq -r '
  .[] | select((.status // "" | ascii_downcase) == "operational") | .name
')
```

If `gt rig list --json` is unavailable or unparseable, skip the run rather
than guessing at rig state.

## Step 2: For each rig, pull the active merge queue

```bash
MR_JSON=$(gt mq list "$RIG" --json --status=all)
ACTIVE_JSON=$(printf '%s' "$MR_JSON" | jq -c '[.[] | select(.status != "closed")]')
```

`--status=all` is required here — the default `gt mq list` view only shows
`open` entries, which would hide `in_progress` MRs and make the queue look
perpetually stalled. Closed MRs are dropped locally since they are no
longer "work in flight".

If there are zero active entries for a rig, there is nothing to check —
move on.

## Step 3: Detector 1 — blocked MR on an unassigned open dependency

For each `open` MR with a non-empty `blocked_by` list, look up each
blocker bead (`bd show <id> --json`) and check:

- `status == "open"`
- `assignee` is empty
- `created_at` is older than `GT_STUCK_WORK_DOG_BLOCKER_STALE_SECONDS`
  (default `7200` — 2 hours, the exact window from `gt-ck0c`)

When all three hold, escalate with a fingerprint keyed on the blocker bead
(`stuck-work-dog:blocked-mr:<rig>:<blocker-id>`), so repeated firings across
patrol cycles bump the same escalation instead of creating duplicates. Set
`--related <blocker-id>` so the escalation links straight to the bead that
needs an owner.

## Step 4: Detector 2 — merge queue stalled with no one working

If no MR in the rig's active queue is `in_progress`:

- Find the oldest `created_at` among active entries.
- If that age exceeds `GT_STUCK_WORK_DOG_QUEUE_STALL_SECONDS` (default
  `1800` — 30 minutes) **and** no bead anywhere in the rig is
  `in_progress` (`bd list --status=in_progress --json --limit=1`) **and** no
  in-flight marker `mq-batch-<rig>` is held, escalate with fingerprint
  `stuck-work-dog:queue-stall:<rig>`.

The `bd list --status=in_progress` check is a coarse, cheap proxy for "is a
polecat actually working" — it doesn't require tmux/session access, only
beads.

The batch check is what keeps the queue's own silence from reading as a
refinery that died. `gt mq batch run` claims none of its MRs until it lands
them, so for the whole of a run — assembly, editorial review, gate, bisection,
merge and the post-merge chores — the rig is entries, none `in_progress`,
nobody assigned: the exact shape this detector fires on, which cost 9 false
escalations in 5 hours (gt-lhaum). The run holds an in-flight marker for its
whole duration; `gt slot status` lists it beside the gate slots, and
`internal/cmd/mq_batch.go` defines its name and role. A `gt slot status` this
plugin cannot read counts as *no* batch — the detector exists to speak up
about a queue nobody is moving, and a command it cannot parse must not
silence it.

## Step 5: Escalate

```bash
gt escalate "<description>" \
  -s medium \
  --source "plugin:stuck-work-dog" \
  --fingerprint "<stable key from Step 3 or 4>" \
  --reason "<what to do about it>"
```

Severity is `medium`, not `high` or `critical`: this plugin surfaces a
*condition worth a human's attention*, not an outage. A signal with a bad
true/false ratio trains people to ignore it — keep the bar for firing
narrow (a real dependency, a real stall) and let the description explain
exactly what to unblock.

## Record Result

```bash
gt plugin record-run --plugin stuck-work-dog --result success \
  --title "stuck-work-dog: <summary>" --description "<summary>" >/dev/null 2>&1 || true
```

On failure, `gt escalate "Plugin FAILED: stuck-work-dog" --severity high --reason "$ERROR"`.
