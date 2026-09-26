#!/usr/bin/env bash
# stuck-work-dog/run.sh — Detects stuck WORK, not stuck workers.
#
# stuck-agent-dog watches for crashed/zombie polecats. Nothing watched for
# the other failure mode: a bead sitting open and unassigned in a blocking
# position, quietly stalling the merge queue while every agent looks
# healthy. Filed from gt-ck0c: an MR sat blocked on an unassigned open bead
# for 2h while the rig idled, and nothing raised a signal about it.
#
# This plugin is read-only. It never assigns, restarts, or dispatches
# anything — it only escalates so a human or the mayor can act.

set -euo pipefail

log() { echo "[stuck-work-dog] $*"; }

# An escalation is this plugin's only action. The daemon runs it directly with
# no agent reading along, so a failed one must not serialize as success: count
# it, keep gt's error text, and exit 1 so the daemon hands the output to a dog.
ESCALATE_FAILURES=0
escalate_failed() {
  log "  ESCALATION FAILED: $*"
  ESCALATE_FAILURES=$((ESCALATE_FAILURES + 1))
}

TOWN_ROOT="${GT_TOWN_ROOT:-}"
if [ -z "$TOWN_ROOT" ]; then
  if ! TOWN_ROOT=$(gt town root 2>/dev/null); then
    log "SKIP: could not resolve town root"
    exit 0
  fi
fi

integer_or_default() {
  local value="$1"
  local default="$2"

  case "$value" in
    ''|*[!0-9]*) echo "$default" ;;
    *) echo "$value" ;;
  esac
}

# How long a bead may sit open+unassigned in a blocking position before we
# escalate (default 2h — the exact window that motivated gt-ck0c).
BLOCKER_STALE_SECONDS=$(integer_or_default "${GT_STUCK_WORK_DOG_BLOCKER_STALE_SECONDS:-}" 7200)
# How long a merge queue may sit with entries but none progressing, and no
# polecat working, before we escalate (default 30m).
QUEUE_STALL_SECONDS=$(integer_or_default "${GT_STUCK_WORK_DOG_QUEUE_STALL_SECONDS:-}" 1800)

# --- Time helpers --------------------------------------------------------

iso8601_epoch() {
  local iso="$1"
  [ -n "$iso" ] || return 1
  jq -rn --arg ts "$iso" '$ts | sub("\\.[0-9]+Z$"; "Z") | fromdateiso8601? // empty' 2>/dev/null
}

# --- Beads resolution helpers ---------------------------------------------
# Mirrors plugins/stuck-agent-dog/run.sh: rig workdir resolution fails soft
# so one bad/missing rig never aborts the whole run under `set -e`.

rig_workdir() {
  local rig="$1"

  if [ -d "$TOWN_ROOT/$rig/mayor/rig" ]; then
    printf '%s\n' "$TOWN_ROOT/$rig/mayor/rig"
    return 0
  fi

  if [ -d "$TOWN_ROOT/$rig" ]; then
    printf '%s\n' "$TOWN_ROOT/$rig"
    return 0
  fi

  return 1
}

operational_rig_names() {
  local rig_json="" rows=""

  if ! rig_json=$(cd "$TOWN_ROOT" 2>/dev/null && gt rig list --json 2>/dev/null); then
    log "SKIP: gt rig list --json unavailable; cannot verify operational rig state" >&2
    return 0
  fi

  if ! rows=$(printf '%s' "$rig_json" | jq -r '
    if type == "array" then .[] else empty end
    | select((.status // "" | ascii_downcase) == "operational")
    | select((.name // "") != "")
    | .name
  ' 2>/dev/null); then
    log "SKIP: gt rig list --json not parseable; cannot verify operational rig state" >&2
    return 0
  fi

  printf '%s\n' "$rows" | awk 'NF >= 1 && $1 != ""'
}

# True (exit 0) if any bead is in_progress anywhere in the rig's workspace.
# Cheap, coarse proxy for "is a polecat actively working" without needing
# tmux/session access (this plugin only reads beads + the merge queue).
has_in_progress_work() {
  local rig="$1" dir="" output="" count=""

  if ! dir=$(rig_workdir "$rig"); then
    return 1
  fi

  output=$(cd "$dir" && bd list --status=in_progress --json --limit=1 2>/dev/null) || return 1
  count=$(printf '%s' "$output" | jq 'length' 2>/dev/null || echo 0)
  [ "${count:-0}" -gt 0 ]
}

# True (exit 0) if the rig has a 'gt mq batch run' in flight.
#
# A batch stacks several MRs and lands them in one push, claiming none of them
# — and no polecat bead — until they land. Mid-batch the rig therefore reads
# as entries, none in_progress, nobody working: the exact signature detector 2
# escalates on, and the source of 9 false escalations in 5 hours before this
# check existed (gt-lhaum). The run holds an in-flight marker for its whole
# duration; 'gt slot status' lists it beside the gate slots, and its name and
# role are defined in internal/cmd/mq_batch.go (batchMarkerName,
# batchMarkerRole).
#
# A reading that cannot be taken counts as not-in-flight: this detector exists
# to speak up about a queue nobody is moving, and a marker it cannot read must
# not silence it.
batch_in_flight() {
  local rig="$1" reading=""

  reading=$(gt slot status --json 2>/dev/null) || return 1
  [ -n "$reading" ] || return 1

  printf '%s' "$reading" | jq -e --arg name "mq-batch-$rig" \
    'any(.slots[]?; .marker == true and .name == $name)' >/dev/null 2>&1
}

# Fetch a single bead as a normalized single JSON object (bd show --json
# returns a one-element array).
bead_json() {
  local rig="$1" id="$2" dir=""

  if ! dir=$(rig_workdir "$rig"); then
    return 1
  fi

  (cd "$dir" && bd show "$id" --json 2>/dev/null) | jq -c '(if type == "array" then .[0] else . end)' 2>/dev/null
}

# --- Main ------------------------------------------------------------------

NOW=$(date +%s)
RIG_LIST=$(operational_rig_names)
if [ -z "$RIG_LIST" ]; then
  log "SKIP: no operational rigs found"
fi

CHECKED_RIGS=0
ESCALATIONS=0

while IFS= read -r RIG; do
  [ -z "$RIG" ] && continue
  CHECKED_RIGS=$((CHECKED_RIGS + 1))

  MR_JSON=$(gt mq list "$RIG" --json --status=all 2>/dev/null) || {
    log "SKIP $RIG: gt mq list failed"
    continue
  }

  # Drop closed MRs — only active queue entries count as "work in flight".
  ACTIVE_JSON=$(printf '%s' "$MR_JSON" | jq -c '[.[] | select(.status != "closed")]' 2>/dev/null) || ACTIVE_JSON='[]'
  MR_COUNT=$(printf '%s' "$ACTIVE_JSON" | jq 'length' 2>/dev/null || echo 0)
  [ "${MR_COUNT:-0}" -gt 0 ] || continue

  log "$RIG: $MR_COUNT active merge request(s)"

  # --- Detector 1: an open MR blocked on an open, unassigned dependency ---
  # This is the narrow, exact-target case from gt-ck0c: the blocked MR
  # already knows what it waits on, so the join is trivial.
  BLOCKED_MRS=$(printf '%s' "$ACTIVE_JSON" | jq -c '
    .[] | select(.status == "open") | select((.blocked_by // []) | length > 0)
  ' 2>/dev/null) || BLOCKED_MRS=""

  if [ -n "$BLOCKED_MRS" ]; then
    while IFS= read -r MR; do
      [ -z "$MR" ] && continue
      MR_ID=$(printf '%s' "$MR" | jq -r '.id')
      BLOCKER_IDS=$(printf '%s' "$MR" | jq -r '.blocked_by[]' 2>/dev/null) || BLOCKER_IDS=""

      while IFS= read -r BLOCKER_ID; do
        [ -z "$BLOCKER_ID" ] && continue

        BLOCKER=$(bead_json "$RIG" "$BLOCKER_ID") || continue
        [ -n "$BLOCKER" ] || continue

        BSTATUS=$(printf '%s' "$BLOCKER" | jq -r '.status // empty' 2>/dev/null)
        ASSIGNEE=$(printf '%s' "$BLOCKER" | jq -r '.assignee // empty' 2>/dev/null)
        CREATED=$(printf '%s' "$BLOCKER" | jq -r '.created_at // empty' 2>/dev/null)

        [ "$BSTATUS" = "open" ] || continue
        [ -z "$ASSIGNEE" ] || continue

        CREATED_EPOCH=$(iso8601_epoch "$CREATED") || continue
        [ -n "$CREATED_EPOCH" ] || continue

        AGE=$(( NOW - CREATED_EPOCH ))
        [ "$AGE" -ge "$BLOCKER_STALE_SECONDS" ] || continue

        AGE_MIN=$(( AGE / 60 ))
        log "  STUCK WORK: $RIG/$MR_ID blocked on $BLOCKER_ID (open, unassigned, ${AGE_MIN}m)"
        ESCALATIONS=$((ESCALATIONS + 1))

        gt escalate "MR $MR_ID ($RIG) blocked on $BLOCKER_ID — open and unassigned for ${AGE_MIN}m" \
          -s medium \
          --source "plugin:stuck-work-dog" \
          --fingerprint "stuck-work-dog:blocked-mr:$RIG:$BLOCKER_ID" \
          --related "$BLOCKER_ID" \
          --reason "Merge request $MR_ID cannot progress because its dependency $BLOCKER_ID is open with no assignee. Nothing dispatches an unassigned bead on its own; assign or close $BLOCKER_ID to unblock the queue." \
          || escalate_failed "$RIG/$BLOCKER_ID"
      done <<< "$BLOCKER_IDS"
    done <<< "$BLOCKED_MRS"
  fi

  # --- Detector 2: queue has entries but nothing is progressing -----------
  # Catches the broader symptom from gt-ck0c: the rig idled for ~30 minutes
  # with a non-empty queue and zero polecats working, and nothing said so.
  IN_PROGRESS_COUNT=$(printf '%s' "$ACTIVE_JSON" | jq '[.[] | select(.status == "in_progress")] | length' 2>/dev/null || echo 0)
  if [ "${IN_PROGRESS_COUNT:-0}" -eq 0 ]; then
    OLDEST_CREATED=$(printf '%s' "$ACTIVE_JSON" | jq -r '[.[].created_at] | sort | first // empty' 2>/dev/null)
    OLDEST_EPOCH=$(iso8601_epoch "$OLDEST_CREATED" 2>/dev/null || true)

    if [ -n "$OLDEST_EPOCH" ]; then
      QUEUE_AGE=$(( NOW - OLDEST_EPOCH ))

      if [ "$QUEUE_AGE" -ge "$QUEUE_STALL_SECONDS" ] && ! has_in_progress_work "$RIG"; then
        if batch_in_flight "$RIG"; then
          log "  $RIG: 'gt mq batch run' in flight — $MR_COUNT entries unclaimed until it lands them"
        else
          QUEUE_AGE_MIN=$(( QUEUE_AGE / 60 ))
          log "  STUCK WORK: $RIG merge queue stalled — $MR_COUNT entries, none progressing, no polecat working, oldest ${QUEUE_AGE_MIN}m"
          ESCALATIONS=$((ESCALATIONS + 1))

          gt escalate "$RIG merge queue stalled — $MR_COUNT entries, none in_progress, no polecat working (oldest ${QUEUE_AGE_MIN}m)" \
            -s medium \
            --source "plugin:stuck-work-dog" \
            --fingerprint "stuck-work-dog:queue-stall:$RIG" \
            --reason "The merge queue has entries but none are progressing and no polecat in $RIG has in_progress work. Check for a blocked-mr escalation above (an unassigned blocker is the usual cause) or a stuck refinery." \
            || escalate_failed "queue-stall:$RIG"
        fi
      fi
    fi
  fi
done <<< "$RIG_LIST"

echo ""
SUMMARY="Checked $CHECKED_RIGS rig(s), raised $ESCALATIONS stuck-work escalation(s)"
echo "=== $SUMMARY ==="

if [ "$ESCALATE_FAILURES" -gt 0 ]; then
  SUMMARY="$SUMMARY, $ESCALATE_FAILURES FAILED to send"
  gt plugin record-run --plugin stuck-work-dog --result failure \
    --title "stuck-work-dog: $SUMMARY" --description "$SUMMARY" >/dev/null 2>&1 || true
  exit 1
fi

gt plugin record-run --plugin stuck-work-dog --result success \
  --title "stuck-work-dog: $SUMMARY" --description "$SUMMARY" >/dev/null 2>&1 || true
