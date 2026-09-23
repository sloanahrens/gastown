#!/usr/bin/env bash
# quality-review/run.sh — per-worker om editorial quality trends.
#
# Source is refs/notes/om, the editorial gate's own proof: the gate writes the
# note, pushes it, and the note outlives wisp GC and DB flattens
# (internal/refinery/editorial/note.go). A window this run cannot read is a
# failed measurement, never a clean one (gt-gs7g).
set -euo pipefail

PLUGIN="quality-review"

# Score bands on a worker's mean score, and the first-attempt bar beside them.
OK_SCORE=0.60
WARN_SCORE=0.45
FIRST_ATTEMPT_WARN=0.50

# MR ids quoted in a breach alert's evidence list, bounded so one prolific
# worker cannot turn an alert into a wall.
EVIDENCE_MRS=10

WINDOW_HOURS="${GT_QUALITY_REVIEW_WINDOW_HOURS:-24}"
case "$WINDOW_HOURS" in ''|*[!0-9]*) WINDOW_HOURS=24 ;; esac

log() { echo "[$PLUGIN] $*"; }

TOWN_ROOT="${GT_TOWN_ROOT:-}"
if [ -z "$TOWN_ROOT" ]; then
  if ! TOWN_ROOT=$(gt town root 2>/dev/null); then
    echo "[$PLUGIN] ERROR: could not resolve town root" >&2
    exit 1
  fi
fi

# record_run — the receipt the acceptance criteria ask for. A receipt that
# cannot be written is logged, not fatal: the numbers on stdout are the
# measurement, and the daemon records the run's own exit status regardless.
record_run() {
  local result="$1" title="$2" description="$3"
  gt plugin record-run --plugin "$PLUGIN" --result "$result" \
    --title "$title" --description "$description" >/dev/null 2>&1 \
    || log "WARN: could not record a run receipt"
}

# fail — every unreadable input lands here, so no path that could not measure
# anything can reach the success receipt. Exit 1; exit 2 is never used.
fail() {
  local msg="$1"
  log "ERROR: $msg"
  record_run failure "$PLUGIN: FAILED" "$msg"
  gt escalate "Plugin FAILED: $PLUGIN" -s medium \
    --source "plugin:$PLUGIN" --fingerprint "$PLUGIN:failed" \
    --reason "$msg" >/dev/null 2>&1 || log "WARN: could not escalate the failure"
  exit 1
}

for tool in jq git; do
  command -v "$tool" >/dev/null 2>&1 || fail "required tool '$tool' is not on PATH"
done

# fetch_notes — bring the rig's notes ref up to origin's. A force refspec, so a
# stale local ref cannot mask what the writers published (git.Git.FetchNotes).
# Returns 1 with the git output in GIT_ERR; a remote that never had the ref is
# "no verdicts recorded here", not a failure.
GIT_ERR=""
fetch_notes() {
  local repo="$1" out=""
  if out=$(git -C "$repo" fetch --quiet origin "+refs/notes/om:refs/notes/om" 2>&1 < /dev/null); then
    return 0
  fi
  case "$out" in
    *"couldn't find remote ref"*) return 0 ;;
  esac
  GIT_ERR="$out"
  return 1
}

# --- Enumerate the rigs to measure -------------------------------------------
#
# The registry is the live parked/docked filter, and its repo_path is the clone
# this plugin runs git against. A registry that errors or will not parse is a
# failed measurement; an empty operational set is not.

RIGS_JSON=$(gt rig list --json 2>/dev/null) || fail "gt rig list --json failed"

RIG_ROWS=$(printf '%s' "$RIGS_JSON" | jq -r '
  if type == "array" then .[] else empty end
  | select((.status // "" | ascii_downcase) == "operational")
  | select((.name // "") != "" and (.repo_path // "") != "")
  | "\(.name)\t\(.repo_path)"') \
  || fail "gt rig list --json is not parseable"

if [ -z "$RIG_ROWS" ]; then
  log "SKIP: no operational rig with a checkout — nothing to measure"
  record_run success "$PLUGIN: no operational rigs" \
    "No operational rig with a checkout; no om notes to read."
  exit 0
fi

# --- Collect the notes -------------------------------------------------------

NOTES_FILE=$(mktemp -t quality-review-notes.XXXXXX)
trap 'rm -f "${NOTES_FILE:-}"' EXIT
: > "$NOTES_FILE"

NOTES_READ=0
NOTES_UNPARSEABLE=0
RIGS_READ=0

# Two registry entries can resolve to one checkout (a shared or symlinked
# clone). Reading it twice would count every note twice, so the second entry
# is skipped — the note's own rig field decides the row it lands in either way.
SEEN_REPOS=" "

while IFS=$'\t' read -r rig repo <&3; do
  [ -n "$rig" ] || continue

  if ! repo=$(cd "$repo" 2>/dev/null && pwd); then
    fail "$rig: repo_path is not a readable directory"
  fi

  case "$SEEN_REPOS" in
    *" $repo "*) log "  $rig: shares a checkout with an earlier rig — skipped"; continue ;;
  esac
  SEEN_REPOS="$SEEN_REPOS$repo "

  fetch_notes "$repo" || fail "$rig: git fetch of refs/notes/om failed: $GIT_ERR"

  if ! git -C "$repo" rev-parse --verify --quiet refs/notes/om >/dev/null 2>&1 < /dev/null; then
    log "  $rig: no refs/notes/om — recorded nothing"
    continue
  fi

  RIGS_READ=$((RIGS_READ + 1))

  NOTE_LIST=$(git -C "$repo" notes --ref=om list 2>&1 < /dev/null) \
    || fail "$rig: git notes --ref=om list failed: $NOTE_LIST"

  while read -r _note_sha annotated _rest <&4; do
    [ -n "${annotated:-}" ] || continue
    CONTENT=$(git -C "$repo" notes --ref=om show "$annotated" 2>&1 < /dev/null) \
      || fail "$rig: git notes --ref=om show $annotated failed: $CONTENT"

    if TAGGED=$(printf '%s' "$CONTENT" | jq -c --arg rig "$rig" '{src_rig: $rig, note: .}' 2>/dev/null); then
      printf '%s\n' "$TAGGED" >> "$NOTES_FILE"
      NOTES_READ=$((NOTES_READ + 1))
    else
      # The ref is shared with every writer that ever touched it, so one
      # unparseable note must not sink the rest — the same allowance
      # FindVerdictForDiff makes for a diff-scoped scan. It is counted and
      # logged, never dropped silently.
      NOTES_UNPARSEABLE=$((NOTES_UNPARSEABLE + 1))
      log "  $rig: SKIP unparseable note on $annotated"
    fi
  done 4<<< "$NOTE_LIST"
done 3<<< "$RIG_ROWS"

NOW_EPOCH=$(date -u +%s)
CUTOFF=$((NOW_EPOCH - WINDOW_HOURS * 3600))

# A note with no worker or rig, or with no readable reviewed_at, cannot be
# attributed to a row or placed in the window. That is a hole in the
# measurement rather than a note in it, so it is counted and reported instead
# of falling out of the totals unremarked. The predicate mirrors the
# aggregation's own, so this number is exactly what the totals excluded.
NOTES_UNATTRIBUTED=$(jq -s '[.[] | select(
  ((.note.worker // "") == "")
  or (((.note.rig // .src_rig) // "") == "")
  or (((.note.reviewed_at // "") | try (sub("\\.[0-9]+Z$"; "Z") | fromdateiso8601) catch null) == null)
)] | length' "$NOTES_FILE") \
  || fail "could not count unattributed notes"

# --- Aggregate ---------------------------------------------------------------

AGGREGATE_JQ=$(cat <<'JQ'
[ .[]
  | (.note.rig // .src_rig) as $rig
  | (.note.worker // "") as $worker
  | ((.note.reviewed_at // "") | try (sub("\\.[0-9]+Z$"; "Z") | fromdateiso8601) catch null) as $at
  | select($worker != "" and $rig != "" and $at != null and $at >= $cutoff)
  | {
      rig: $rig,
      worker: $worker,
      mr: (.note.mr // ""),
      score: ((.note.score // 0) | tonumber),
      at: $at,
      verdict: (.note.verdict // ""),
      # A re-review replaces a diff's verdict, so "first attempt" is the first
      # entry of the history the note carries — the top-level verdict is the
      # latest one, not the one the diff first met the gate with.
      first_verdict: (
        if ((.note.attempts // []) | length) > 0 then .note.attempts[0].verdict
        else (.note.verdict // "") end
      ),
      # findings[] is the detail behind findings_count; where a note predates
      # it, the follow-up beads filed for major findings are the count.
      majors: (
        if (.note | has("findings")) then
          ([.note.findings[]? | select(.severity == "major")] | length)
        else ((.note.followups // []) | length) end
      ),
      findings_count: ((.note.findings_count // 0) | tonumber)
    }
]
| group_by([.rig, .worker])
| map(
    . as $g
    | ($g | sort_by(.at)) as $s
    | ($s | length) as $n
    | ((($n + 1) / 2) | floor) as $h
    | $s[0:$h] as $a
    | (if ($s[$h:] | length) > 0 then $s[$h:] else $a end) as $b
    | (($a | map(.score) | add) / ($a | length)) as $mean_a
    | (($b | map(.score) | add) / ($b | length)) as $mean_b
    # Every mean is rounded once, here, so the table, the JSON line and the
    # score band all read the same number (0.325 is not 0.33 to one of them).
    | (($s | map(.score) | add) / $n | . * 10000 | round / 10000) as $mean_score
    | (($s | map(select(.first_verdict == "approve")) | length) / $n | . * 10000 | round / 10000) as $fa_rate
    | (if $mean_score >= $ok then "OK" elif $mean_score >= $warn then "WARN" else "BREACH" end) as $base
    | {
        rig: $s[0].rig,
        worker: $s[0].worker,
        reviews: $n,
        first_attempt_approve_rate: $fa_rate,
        mean_score: $mean_score,
        mean_findings: (($s | map(.findings_count) | add) / $n | . * 10000 | round / 10000),
        majors: ($s | map(.majors) | add),
        # share of diffs whose latest verdict the gate rejected
        request_changes: ($s | map(select(.verdict == "request_changes")) | length),
        # The later half minus the earlier one, so a positive delta means the
        # worker is getting better rather than that they started out ahead.
        trend: (
          if ($mean_b - $mean_a) > 0.05 then "improving"
          elif ($mean_b - $mean_a) < -0.05 then "declining"
          else "stable" end
        ),
        # A worker clearing the score band on a coin-flip first-attempt rate is
        # WARN, not OK: the band measures the reviewer's number, not the
        # rework the gate absorbed before landing the diff.
        status: (if ($base == "OK" and $fa_rate < $fa) then "WARN" else $base end),
        first_reviewed_at: ($s[0].at),
        last_reviewed_at: ($s[-1].at),
        # Every MR this worker's diffs came in on, capped for an alert body.
        # mrs_total is what makes the cap visible rather than silent.
        mrs: ([$s[] | .mr | select(. != "")] | unique | .[0:$fanout]),
        mrs_total: ([$s[] | .mr | select(. != "")] | unique | length)
      }
  )
| sort_by(.worker)
JQ
)

WORKERS=$(jq -s \
  --argjson cutoff "$CUTOFF" --argjson ok "$OK_SCORE" --argjson warn "$WARN_SCORE" \
  --argjson fa "$FIRST_ATTEMPT_WARN" --argjson fanout "$EVIDENCE_MRS" \
  "$AGGREGATE_JQ" "$NOTES_FILE") \
  || fail "could not aggregate the om notes (jq error above)"

WORKER_COUNT=$(printf '%s' "$WORKERS" | jq 'length')
REVIEW_COUNT=$(printf '%s' "$WORKERS" | jq '[.[].reviews] | add // 0')
BREACH_COUNT=$(printf '%s' "$WORKERS" | jq '[.[] | select(.status == "BREACH")] | length')
WARN_COUNT=$(printf '%s' "$WORKERS" | jq '[.[] | select(.status == "WARN")] | length')

WINDOW_START=$(jq -rn --argjson e "$CUTOFF" '$e | todateiso8601')
WINDOW_END=$(jq -rn --argjson e "$NOW_EPOCH" '$e | todateiso8601')
WINDOW_DESC="$WINDOW_START..$WINDOW_END"

SUMMARY="$WORKER_COUNT worker(s) over $REVIEW_COUNT review(s), window $WINDOW_DESC"

# An empty window is a result, not a failure: it is reported and recorded as a
# clean run (the acceptance criteria call this out — a rig with nothing
# recorded must not read as a broken measurement).
if [ "$WORKER_COUNT" -eq 0 ]; then
  log "no om notes in $WINDOW_DESC across $RIGS_READ rig(s) with a notes ref"
  SKIPPED="Read $RIGS_READ rig(s) with a notes ref; $NOTES_UNPARSEABLE unparseable, $NOTES_UNATTRIBUTED unattributable."
  if [ "$NOTES_UNPARSEABLE" -gt 0 ] || [ "$NOTES_UNATTRIBUTED" -gt 0 ]; then
    log "$SKIPPED"
  fi
  record_run success "$PLUGIN: nothing in window" "No om notes reviewed in $WINDOW_DESC. $SKIPPED"
  exit 0
fi

# --- Report ------------------------------------------------------------------

log "=== $SUMMARY ==="
printf '%s' "$WORKERS" | jq -r '
  (["WORKER","RIG","N","FA-APPROVE","MEAN","FINDINGS","MAJORS","TREND","STATUS"] | @tsv),
  (.[] | [
    .worker, .rig, (.reviews | tostring),
    (((.first_attempt_approve_rate * 100) | round | tostring) + "%"),
    (((.mean_score * 100) | round) / 100 | tostring),
    (((.mean_findings * 100) | round) / 100 | tostring),
    (.majors | tostring), .trend, .status
  ] | @tsv)' \
  | awk -F'\t' '{printf "  %-14s %-10s %4s %10s %6s %9s %7s  %-10s %s\n", $1, $2, $3, $4, $5, $6, $7, $8, $9}'

echo ""
echo "--- worker summaries (one JSON object per line) ---"
printf '%s' "$WORKERS" | jq -c \
  --argjson window_start "$CUTOFF" --argjson window_end "$NOW_EPOCH" '
  .[] | {
    rig, worker, reviews,
    first_attempt_approve_rate, mean_score, mean_findings,
    majors, request_changes, trend, status,
    window_start: ($window_start | todateiso8601),
    window_end: ($window_end | todateiso8601),
    mrs, mrs_total
  }'

# --- Alerts ------------------------------------------------------------------

BREACH_ROWS=$(printf '%s' "$WORKERS" | jq -r '
  .[] | select(.status == "BREACH")
  | [.rig, .worker, (.mean_score | tostring), (.reviews | tostring),
     (.first_attempt_approve_rate | tostring), (.majors | tostring),
     (.request_changes | tostring), .trend, (.mrs_total | tostring),
     (.mrs | join(", "))] | @tsv')

while IFS=$'\t' read -r rig worker mean reviews fa_rate majors rejects trend mrs_total mrs <&3; do
  [ -n "$worker" ] || continue
  log "  BREACH: $rig/$worker mean=$mean over $reviews review(s)"

  # Name the cap where it bites, so a shortened list is not read as the whole
  # population of MRs the verdict rests on.
  if [ "$mrs_total" -gt "$EVIDENCE_MRS" ]; then
    mrs="$mrs (+$((mrs_total - EVIDENCE_MRS)) more)"
  fi

  # The breach mail is the durable half of the alert: it has to survive the
  # session that would act on it, which a nudge does not.
  gt mail send deacon/ -s "Quality BREACH: $worker" --stdin <<BODY || log "  WARN: breach mail failed for $rig/$worker"
Worker: $worker
Rig: $rig
Mean score: $mean (BREACH below $WARN_SCORE)
Reviews: $reviews
First-attempt approve rate: $fa_rate
Request-changes verdicts: $rejects
Major findings: $majors
Trend: $trend
Window: $WINDOW_DESC
MRs: $mrs

Action: review recent merges from this worker for quality issues.
BODY

  gt escalate "Quality BREACH: $worker (mean $mean)" -s medium \
    --source "plugin:$PLUGIN" --fingerprint "$PLUGIN:breach:$rig/$worker" \
    --reason "Worker $worker in rig $rig: mean score $mean over $reviews review(s) in $WINDOW_DESC, below the $WARN_SCORE BREACH threshold; first-attempt approve rate $fa_rate, $rejects request_changes verdict(s), $majors major finding(s), trend $trend, MRs: $mrs" \
    >/dev/null 2>&1 || log "  WARN: breach escalation failed for $rig/$worker"
done 3<<< "$BREACH_ROWS"

# A fingerprint is an assertion the worker is in breach; once they are not, the
# producer closes it rather than leaving it for a human (gt-vwry). Clearing a
# key that matches nothing costs one read and writes nothing.
CLEAR_ARGS=()
while IFS=$'\t' read -r rig worker; do
  [ -n "$worker" ] || continue
  CLEAR_ARGS+=(--fingerprint "$PLUGIN:breach:$rig/$worker")
done < <(printf '%s' "$WORKERS" | jq -r '.[] | select(.status != "BREACH") | [.rig, .worker] | @tsv')

if [ "${#CLEAR_ARGS[@]}" -gt 0 ]; then
  gt escalate clear "${CLEAR_ARGS[@]}" \
    --reason "$PLUGIN: no worker is in breach as of $WINDOW_END" \
    >/dev/null 2>&1 || log "WARN: could not clear breach alerts"
fi

# --- Record ------------------------------------------------------------------

RECEIPT="$SUMMARY: $BREACH_COUNT breach(es), $WARN_COUNT warning(s)"
log "=== $RECEIPT ==="
record_run success "$PLUGIN: $WORKER_COUNT worker(s), $BREACH_COUNT breach(es)" \
  "$RECEIPT. $NOTES_READ note(s) read; $NOTES_UNPARSEABLE unparseable, $NOTES_UNATTRIBUTED unattributable."
exit 0
