#!/usr/bin/env bash
# quality-review/run.sh — per-worker om editorial quality trends.
#
# Source is refs/notes/om, the editorial gate's own proof: the gate writes the
# note, pushes it, and the note outlives wisp GC and DB flattens
# (internal/refinery/editorial/note.go). A window this run cannot read is a
# failed measurement, never a clean one (gt-gs7g).
set -euo pipefail

# C for two reasons: git's diagnostics stay in the language the case statement
# in fetch_notes matches on, and ${#s} below is a byte count rather than a
# character count.
export LC_ALL=C

PLUGIN="quality-review"

# The gate's ref, and the private ref this run reads it through. Every rig
# clone is a worktree of one .repo.git and a notes ref is not per-worktree, so
# refs/notes/om in a rig clone is the ref the refinery writes with WriteNote and
# publishes with PushNotes. A read-only report that force-fetched into it would
# overwrite the gate's proof, and could discard a verdict the gate has written
# but not yet pushed (gt-2rcx). Fetch the same objects into a ref of our own and
# read that; the gate's ref is never touched.
GATE_NOTES_REF="refs/notes/om"
NOTES_REF="refs/notes/${PLUGIN}-om"

# Score bands on a worker's mean score, and the first-attempt bar beside them.
OK_SCORE=0.60
WARN_SCORE=0.45
FIRST_ATTEMPT_WARN=0.50

# Share of the notes a run read that may be unusable — unparseable, undatable,
# or carrying nothing to attribute them to — before the run is a failed
# measurement rather than a quiet one. A ref whose fields have moved is a
# measurement that cannot see its own data, and reporting that as a clean
# window is how the prose plugin's --include-infra bug went unnoticed for days
# (gt-gs7g). A share rather than a count, so one foreign note beside healthy
# ones stays a skip; the guard fires only when the data is not there to read.
UNUSABLE_FAIL_PCT=50

# MR ids quoted in a breach alert's evidence list, bounded so one prolific
# worker cannot turn an alert into a wall.
EVIDENCE_MRS=10

# How far back the alert-key sweep looks, in windows. A worker who breached and
# then stopped reviewing never reappears in a window, so clearing only the
# workers the current window saw leaves their key open forever — the gt-vwry
# problem escalate clear exists to solve. The plugin runs every 6h, so a
# horizon of seven windows (seven days by default) covers every such worker for
# many runs after their last review, without a key list that grows without end.
CLEAR_HORIZON_WINDOWS=7

WINDOW_HOURS="${GT_QUALITY_REVIEW_WINDOW_HOURS:-24}"
case "$WINDOW_HOURS" in ''|*[!0-9]*) WINDOW_HOURS=24 ;; esac

log() { echo "[$PLUGIN] $*"; }

# hex40 / digits — the shape of a cat-file frame header, used to tell a frame
# from a line of the note inside one. Written as tests rather than as a glob
# because bash 3.2 (the bash on macOS) has no regex test.
hex40() {
  [ "${#1}" -eq 40 ] || return 1
  case "$1" in *[!0-9a-f]*) return 1 ;; esac
  return 0
}

digits() {
  case "$1" in ''|*[!0-9]*) return 1 ;; esac
  return 0
}

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

# Every gt call below — the receipt, the escalations, the breach mail — resolves
# a town from its cwd or from GT_TOWN_ROOT, and the daemon's cwd is not a
# contract this script can lean on. Resolve the town once and pin it for those
# calls (gt takes GT_TOWN_ROOT as an override); an unresolvable town root is a
# failed run, not a partial one that files its receipt against another town.
TOWN_ROOT="${GT_TOWN_ROOT:-}"
if [ -z "$TOWN_ROOT" ]; then
  if ! TOWN_ROOT=$(gt town root 2>/dev/null); then
    fail "could not resolve the town root: GT_TOWN_ROOT is unset and 'gt town root' failed"
  fi
fi
export GT_TOWN_ROOT="$TOWN_ROOT"

# fetch_notes — bring the gate's ref up to origin's, into our own ref. A force
# refspec, so a stale copy of the private ref cannot mask what the writers
# published (git.Git.FetchNotes). Returns 1 with the git output in GIT_ERR; a
# remote that never had the ref is "no verdicts recorded here", not a failure.
GIT_ERR=""
fetch_notes() {
  local repo="$1" out=""
  if out=$(git -C "$repo" fetch --quiet origin "+$GATE_NOTES_REF:$NOTES_REF" 2>&1 < /dev/null); then
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
  # The daemon's marker for an exit-0 run that accomplished nothing
  # (scriptSkippedMarker, gt-chqi): a no-op that records success green-checks
  # the plugin's history and satisfies its cooldown for a run that measured
  # nothing.
  echo "[plugin-result skipped]"
  record_run skipped "$PLUGIN: no operational rigs" \
    "No operational rig with a checkout; no om notes to read."
  exit 0
fi

# --- Collect the notes -------------------------------------------------------

NOTES_FILE=$(mktemp -t quality-review-notes.XXXXXX)
trap 'rm -f "${NOTES_FILE:-}"' EXIT
: > "$NOTES_FILE"

NOTES_SEEN=0
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

  fetch_notes "$repo" || fail "$rig: git fetch of $GATE_NOTES_REF failed: $GIT_ERR"

  if ! git -C "$repo" rev-parse --verify --quiet "$NOTES_REF" >/dev/null 2>&1 < /dev/null; then
    log "  $rig: no $GATE_NOTES_REF — recorded nothing"
    continue
  fi

  RIGS_READ=$((RIGS_READ + 1))

  NOTE_LIST=$(git -C "$repo" notes --ref="$NOTES_REF" list 2>&1 < /dev/null) \
    || fail "$rig: git notes --ref=$NOTES_REF list failed: $NOTE_LIST"
  [ -n "$NOTE_LIST" ] || continue

  # One cat-file for the whole ref, not a `git notes show` per note. The ref
  # holds every review ever recorded, rehearsal commits included, and grows
  # without bound, so a process per note is a per-run cost that climbs toward
  # the 5m timeout for no gain — 358 notes cost 5.5s of pure process spawn, and
  # this run is 2.5s end to end. %(rest) echoes the annotated commit into the
  # frame header, so a note stays paired with the commit it is attached to; the
  # frame is read as lines rather than by its byte count because bash has no
  # read -N before 4.1 and macOS ships 3.2. Lines also keep a multi-line note
  # whole.
  PENDING_COMMIT=""
  PENDING_NOTE=""

  # Frame headers come one per note, so a header closes the note before it.
  store_pending() {
    if [ -n "$PENDING_COMMIT" ]; then
      # A verdict is a JSON object with a numeric score. Anything else on this
      # ref came from another writer, so it is skipped per note rather than
      # failing the whole report — a bare `42` or a string score used to pass
      # this test and take the aggregate jq down with it.
      if TAGGED=$(printf '%s' "$PENDING_NOTE" | jq -e -c --arg rig "$rig" '
            select(type == "object")
            | select((has("score") | not) or (.score | type == "number"))
            | {src_rig: $rig, note: .}' 2>/dev/null); then
        printf '%s\n' "$TAGGED" >> "$NOTES_FILE"
        NOTES_READ=$((NOTES_READ + 1))
      else
        # The ref is shared with every writer that ever touched it, so one
        # unparseable note must not sink the rest — the same allowance
        # FindVerdictForDiff makes for a diff-scoped scan. It is counted and
        # logged, never dropped silently; a ref where most notes read like this
        # is a failed measurement (UNUSABLE_FAIL_PCT), not a quiet one.
        NOTES_UNPARSEABLE=$((NOTES_UNPARSEABLE + 1))
        log "  $rig: SKIP unparseable note on $PENDING_COMMIT"
      fi
    fi
    PENDING_COMMIT=""
    PENDING_NOTE=""
  }

  while IFS= read -r line <&4; do
    f_blob="${line%% *}"
    f_rest="${line#* }"
    f_size="${f_rest%% *}"
    f_commit="${f_rest#* }"

    if hex40 "$f_blob" && digits "$f_size" && hex40 "$f_commit"; then
      store_pending
      PENDING_COMMIT="$f_commit"
      NOTES_SEEN=$((NOTES_SEEN + 1))
    elif hex40 "$f_blob" && [ "$f_size" = "missing" ]; then
      # cat-file could not produce the blob. The note has no content, so the
      # next header closes it as an unparseable one.
      store_pending
      PENDING_COMMIT="$f_blob"
      NOTES_SEEN=$((NOTES_SEEN + 1))
    elif [ -n "$PENDING_COMMIT" ]; then
      PENDING_NOTE+="$line"$'\n'
    fi
  done 4< <(printf '%s\n' "$NOTE_LIST" \
      | git -C "$repo" cat-file --batch='%(objectname) %(objectsize) %(rest)' 2>/dev/null)
  store_pending
done 3<<< "$RIG_ROWS"

NOW_EPOCH=$(date -u +%s)
CUTOFF=$((NOW_EPOCH - WINDOW_HOURS * 3600))
CLEAR_CUTOFF=$((NOW_EPOCH - WINDOW_HOURS * CLEAR_HORIZON_WINDOWS * 3600))

WINDOW_START=$(jq -rn --argjson e "$CUTOFF" '$e | todateiso8601')
WINDOW_END=$(jq -rn --argjson e "$NOW_EPOCH" '$e | todateiso8601')
WINDOW_DESC="$WINDOW_START..$WINDOW_END"

# What the window actually holds. A note with no readable reviewed_at cannot be
# placed in it, and a note with no worker cannot be attributed to a row; both
# are holes in the measurement rather than notes in it, and the guards below
# decide on these numbers.
WINDOW_STATS=$(jq -s --argjson cutoff "$CUTOFF" '
  def at: ((.note.reviewed_at // "") | try (sub("\\.[0-9]+Z$"; "Z") | fromdateiso8601) catch null);
  def attributed: (((.note.worker // "") != "") and (((.note.rig // .src_rig) // "") != ""));
  [ .[] | select(at != null) ] as $dated
  | [ $dated[] | select(at >= $cutoff) ] as $win
  | { dated: ($dated | length),
      in_window: ($win | length),
      attributed: ([ $win[] | select(attributed) ] | length) }' "$NOTES_FILE") \
  || fail "could not window the notes read from $NOTES_REF"

read -r NOTES_DATED NOTES_IN_WINDOW ATTR_IN_WINDOW \
  < <(printf '%s' "$WINDOW_STATS" | jq -r '[.dated, .in_window, .attributed] | @tsv') \
  || fail "could not window the notes read from $NOTES_REF"

NOTES_UNATTRIBUTED=$(( NOTES_READ - NOTES_DATED + NOTES_IN_WINDOW - ATTR_IN_WINDOW ))
NOTES_OFF_WINDOW=$(( NOTES_DATED - NOTES_IN_WINDOW ))

# --- Did this run measure anything? ------------------------------------------
#
# Three shapes of "nothing to report" are not the same thing, and only two of
# them are results:
#
#   nothing on the ref                    -> nothing recorded yet: a no-op
#   dated notes, all older than the window -> the window is empty: a result
#   notes read, none parsable, datable or
#   attributable                           -> a failed measurement
#
# The third is the shape the prose plugin shipped with --include-infra: a query
# that could not see its own data, recorded as a clean window while breaches
# went unalerted (gt-gs7g). Success is reserved for a window that is genuinely
# empty; a ref where the data this plugin groups by is not there to read exits
# 1 and escalates instead.
unusable_share() { [ $(( $1 * 100 )) -ge $(( $2 * UNUSABLE_FAIL_PCT )) ]; }

if [ "$NOTES_SEEN" -gt 0 ] && [ "$NOTES_READ" -eq 0 ]; then
  fail "$NOTES_SEEN note(s) on $GATE_NOTES_REF across $RIGS_READ rig(s), none parsable as a verdict"
fi

if [ "$NOTES_READ" -gt 0 ] && unusable_share $(( NOTES_READ - NOTES_DATED )) "$NOTES_READ"; then
  fail "$NOTES_READ note(s) read from $GATE_NOTES_REF, $(( NOTES_READ - NOTES_DATED )) with no readable reviewed_at"
fi

if [ "$NOTES_IN_WINDOW" -gt 0 ] && unusable_share $(( NOTES_IN_WINDOW - ATTR_IN_WINDOW )) "$NOTES_IN_WINDOW"; then
  fail "$(( NOTES_IN_WINDOW - ATTR_IN_WINDOW )) of $NOTES_IN_WINDOW note(s) in $WINDOW_DESC carry no worker to attribute them to"
fi

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
      # patch_id is the gate's own identity for a reviewed diff, and the key
      # that keeps one verdict to one row. The gate copies its note onto the
      # landed commit whenever the reviewed head differs from it
      # (editorial.CopyNotesToLanded — every stacked or rebased merge), and a
      # re-roll writes a new note on a fresh rehearsal commit while the note it
      # replaces stays on the old one (gt-bveg). One row per annotated commit
      # therefore counts a copied MR's approval twice and a re-rolled MR's
      # first request_changes twice, moving reviews, every mean, and both alert
      # thresholds. A note predating patch_id falls back to its own content,
      # which still collapses a copy — identical bytes, two commits — while
      # keeping two genuinely different diffs apart.
      dkey: (if (.note.patch_id // "") != "" then .note.patch_id else (.note | tojson) end),
      score: ((.note.score // 0) | (try tonumber catch 0)),
      at: $at,
      attempt: ((.note.attempt // 0) | (try tonumber catch 0)),
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
      findings_count: ((.note.findings_count // 0) | (try tonumber catch 0))
    }
]
# The latest verdict per diff survives, so a re-roll's note — the one carrying
# the attempt history — is the row that reaches the totals and the note it
# replaced is not counted beside it. sort_by is stable, and the composite key
# (which puts .at and .attempt last) leaves the winner last in each group.
| sort_by(.rig, .worker, .mr, .dkey, .at, .attempt)
| group_by([.rig, .worker, .mr, .dkey])
| map(.[-1])
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
        # count of the reviewed diffs whose latest verdict the gate rejected
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

SUMMARY="$WORKER_COUNT worker(s) over $REVIEW_COUNT review(s), window $WINDOW_DESC"

# --- Clear the alert keys whose condition is gone ----------------------------
#
# A fingerprint is an assertion; one that outlives its condition is what
# escalate clear exists to retract (gt-vwry). Two details make that harder
# here. A breaching worker who then stops reviewing never reappears in a
# window, so the sweep covers every worker seen within CLEAR_HORIZON_WINDOWS,
# not just the ones this window saw. And the reason recorded against a closure
# has to be true while other workers ARE still in breach, so it names the count
# instead of denying that anyone is in breach. Clearing a key that matches no
# open escalation costs one read and writes nothing.
clear_alerts() {
  local breaches="$1" keys="" cleared=0 rig worker
  local -a args=()

  if ! keys=$(jq -r -s --argjson horizon "$CLEAR_CUTOFF" --arg breaches "$breaches" '
        ($breaches | split("\n") | map(select(. != ""))) as $in_breach
        | [ .[]
            | (.note.rig // .src_rig) as $rig
            | (.note.worker // "") as $worker
            | ((.note.reviewed_at // "") | try (sub("\\.[0-9]+Z$"; "Z") | fromdateiso8601) catch null) as $at
            | select($worker != "" and $rig != "" and $at != null and $at >= $horizon)
            | "\($rig)\t\($worker)" ]
        | unique
        | .[] | select(. as $k | ($in_breach | index($k)) == null)' "$NOTES_FILE"); then
    log "  WARN: could not read the swept worker set; no breach keys are cleared this run"
    keys=""
  fi

  while IFS=$'\t' read -r rig worker; do
    [ -n "$worker" ] || continue
    args+=(--fingerprint "$PLUGIN:breach:$rig/$worker")
    cleared=$((cleared + 1))
  done <<< "$keys"

  # The failure key is closed by any run that got far enough to report: this
  # run measured the window, so whatever the last failure was, it is not the
  # current state.
  gt escalate clear --fingerprint "$PLUGIN:failed" \
    --reason "$PLUGIN: the window was read and reported; the last failure is not the current state" \
    >/dev/null 2>&1 || log "  WARN: could not clear the $PLUGIN:failed key"

  if [ "${#args[@]}" -gt 0 ]; then
    gt escalate clear "${args[@]}" \
      --reason "$PLUGIN: $cleared worker key(s) not in breach in $WINDOW_DESC (${BREACH_COUNT:-0} still in breach)" \
      >/dev/null 2>&1 || log "WARN: could not clear breach alerts"
  fi
}

# An empty window is a result, not a failure: the guards above have already
# ruled out a window that could not be read, so reaching here with no workers
# means the notes on the ref are simply older than the window (gt-gs7g).
if [ "$WORKER_COUNT" -eq 0 ]; then
  log "no om notes in $WINDOW_DESC across $RIGS_READ rig(s) with a notes ref"
  clear_alerts ""
  SKIPPED="Read $RIGS_READ rig(s) with a notes ref; $NOTES_UNPARSEABLE unparseable, $NOTES_UNATTRIBUTED unattributable, $NOTES_OFF_WINDOW older than the window."
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
  # session that would act on it, which a nudge does not. It goes to the deacon
  # rather than the prose plugin's mayor/ because this plugin is dispatched by
  # the deacon's patrol and the keyed escalation already routes to the mayor
  # (settings/escalation.json) — two addresses for one condition is how a
  # breach gets acknowledged twice and acted on once.
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

  # The reason's leading sentence is a contract for a human reader, not a
  # parsing surface: the sweep above finds open keys from the notes ref, so the
  # key does not have to be recovered from this text.
  gt escalate "Quality BREACH: $worker (mean $mean)" -s medium \
    --source "plugin:$PLUGIN" --fingerprint "$PLUGIN:breach:$rig/$worker" \
    --reason "Worker $worker in rig $rig: mean score $mean over $reviews review(s) in $WINDOW_DESC, below the $WARN_SCORE BREACH threshold; first-attempt approve rate $fa_rate, $rejects request_changes verdict(s), $majors major finding(s), trend $trend, MRs: $mrs" \
    >/dev/null 2>&1 || log "  WARN: breach escalation failed for $rig/$worker"
done 3<<< "$BREACH_ROWS"

clear_alerts "$(printf '%s' "$WORKERS" | jq -r '.[] | select(.status == "BREACH") | "\(.rig)\t\(.worker)"')"

# --- Record ------------------------------------------------------------------

RECEIPT="$SUMMARY: $BREACH_COUNT breach(es), $WARN_COUNT warning(s)"
log "=== $RECEIPT ==="
record_run success "$PLUGIN: $WORKER_COUNT worker(s), $BREACH_COUNT breach(es)" \
  "$RECEIPT. $NOTES_READ note(s) read; $NOTES_UNPARSEABLE unparseable, $NOTES_UNATTRIBUTED unattributable, $NOTES_OFF_WINDOW older than the window."
exit 0
