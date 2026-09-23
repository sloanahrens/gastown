#!/usr/bin/env bash
# Tests for quality-review/run.sh.
#
# Real git and real jq against repos built here; only `gt` is stubbed, so the
# notes plumbing under test is the same code the daemon runs. Every case also
# pins the exit code, which is what keeps "never 2 for no data" a property of
# the suite rather than of one test.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="$SCRIPT_DIR/run.sh"
ORIGINAL_PATH="$PATH"
PASS=0
FAIL=0
CLEANUP_DIRS=()

cleanup() {
  for dir in "${CLEANUP_DIRS[@]}"; do
    rm -rf "$dir"
  done
}
trap cleanup EXIT

record_pass() {
  PASS=$((PASS + 1))
  printf 'PASS: %s\n' "$1"
}

record_fail() {
  FAIL=$((FAIL + 1))
  printf 'FAIL: %s\n' "$1"
}

assert_eq() {
  local actual="$1" expected="$2" label="$3"
  if [ "$actual" = "$expected" ]; then
    record_pass "$label"
  else
    record_fail "$label"
    printf '  expected: %s\n' "$expected"
    printf '  actual:   %s\n' "$actual"
  fi
}

assert_file_contains() {
  local file="$1" needle="$2" label="$3"
  if grep -Fq -- "$needle" "$file"; then
    record_pass "$label"
  else
    record_fail "$label"
    printf '  expected %q in %s:\n' "$needle" "$file"
    sed 's/^/    /' "$file" 2>/dev/null || true
  fi
}

assert_file_not_contains() {
  local file="$1" needle="$2" label="$3"
  if grep -Fq -- "$needle" "$file" 2>/dev/null; then
    record_fail "$label"
    printf '  did not expect %q in %s:\n' "$needle" "$file"
    sed 's/^/    /' "$file"
  else
    record_pass "$label"
  fi
}

# --- Fixture ---------------------------------------------------------------

write_fake_gt() {
  cat > "$BIN_DIR/gt" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
case "${1:-}" in
  rig)
    if [ "${2:-}" = "list" ]; then
      [ -f "$TEST_STATE/rigs_fail" ] && exit 1
      cat "$TEST_STATE/rigs.json"
      exit 0
    fi
    ;;
  plugin)
    if [ "${2:-}" = "record-run" ]; then
      printf '%s\n' "$*" >> "$TEST_STATE/receipts.log"
      exit 0
    fi
    ;;
  escalate)
    if [ "${2:-}" = "clear" ]; then
      printf '%s\n' "$*" >> "$TEST_STATE/clears.log"
      exit 0
    fi
    printf '%s\n' "$*" >> "$TEST_STATE/escalations.log"
    exit 0
    ;;
  mail)
    printf '%s\n' "$*" >> "$TEST_STATE/mail.log"
    cat >> "$TEST_STATE/mail.log"
    exit 0
    ;;
  town)
    if [ "${2:-}" = "root" ]; then
      printf '%s\n' "$GT_TOWN_ROOT"
      exit 0
    fi
    ;;
esac
exit 0
SH
  chmod +x "$BIN_DIR/gt"
}

new_case() {
  CASE_DIR=$(mktemp -d)
  CLEANUP_DIRS+=("$CASE_DIR")
  BIN_DIR="$CASE_DIR/bin"
  TEST_STATE="$CASE_DIR/state"
  mkdir -p "$BIN_DIR" "$TEST_STATE"
  : > "$TEST_STATE/receipts.log"
  : > "$TEST_STATE/escalations.log"
  : > "$TEST_STATE/mail.log"
  : > "$TEST_STATE/clears.log"
  write_fake_gt
  export TEST_STATE
  export GT_TOWN_ROOT="$CASE_DIR/town"
  mkdir -p "$GT_TOWN_ROOT"
  PATH="$BIN_DIR:$ORIGINAL_PATH"
}

# make_repo <dir> — a rig checkout plus the origin it pushes to. No notes ref
# yet; publish_notes is what puts one on the remote.
make_repo() {
  local dir="$1"
  mkdir -p "$dir"
  git init -q --bare "$dir/origin.git"
  git init -q "$dir/work"
  git -C "$dir/work" config user.email test@example.com
  git -C "$dir/work" config user.name Test
  git -C "$dir/work" remote add origin "$dir/origin.git"
  echo seed > "$dir/work/seed"
  git -C "$dir/work" add seed
  git -C "$dir/work" commit -qm seed
  git -C "$dir/work" push -q origin HEAD:main
}

# add_note <dir> <n> <json> — a fresh commit carrying that om note.
add_note() {
  local dir="$1" n="$2" json="$3" sha=""
  echo "$n" > "$dir/work/commit-$n"
  git -C "$dir/work" add "commit-$n"
  git -C "$dir/work" commit -qm "commit $n"
  sha=$(git -C "$dir/work" rev-parse HEAD)
  git -C "$dir/work" notes --ref=om add -f -m "$json" "$sha"
}

# copy_note <dir> <n> — the gate's CopyNotesToLanded: a new commit (the landed
# one) carrying the same note blob as the commit below it. Every stacked or
# rebased merge does this, so the same verdict sits on two commits.
copy_note() {
  local dir="$1" n="$2" from_sha sha
  from_sha=$(git -C "$dir/work" rev-parse HEAD)
  echo "$n" > "$dir/work/commit-$n"
  git -C "$dir/work" add "commit-$n"
  git -C "$dir/work" commit -qm "commit $n (landed, note copied)"
  sha=$(git -C "$dir/work" rev-parse HEAD)
  git -C "$dir/work" notes --ref=om copy "$from_sha" "$sha"
}

publish_notes() {
  local dir="$1"
  git -C "$dir/work" push -q origin HEAD:main
  git -C "$dir/work" push -q origin refs/notes/om
}

# iso_at <seconds-ago> — RFC3339 UTC, as the gate writes reviewed_at.
iso_at() {
  local epoch=$(( $(date -u +%s) - $1 ))
  date -u -r "$epoch" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null \
    || date -u -d "@$epoch" +%Y-%m-%dT%H:%M:%SZ
}

# note_json <worker> <score> <verdict> <attempt> <findings_count> <seconds_ago> [extra-json]
# extra-json is merged in, which is how the attempts/findings/followups shapes
# get exercised.
note_json() {
  local worker="$1" score="$2" verdict="$3" attempt="$4" count="$5" ago="$6" extra="${7:-{\}}"
  jq -cn --arg rig "$8" --arg worker "$worker" --argjson score "$score" --arg verdict "$verdict" \
    --argjson attempt "$attempt" --argjson count "$count" --arg at "$(iso_at "$ago")" --argjson extra "$extra" '
    {rig: $rig, mr: ("gt-wisp-" + $worker), worker: $worker, score: $score, verdict: $verdict,
     findings_count: $count, attempt: $attempt, reviewed_at: $at} + $extra'
}

# --- Running the plugin ----------------------------------------------------

PLUGIN_RC=0

run_plugin() {
  local out="$1"
  PLUGIN_RC=0
  ( bash "$SCRIPT" ) > "$out" 2>&1 || PLUGIN_RC=$?
}

# json_lines <out> — the per-worker machine summaries, canonicalised so the
# comparison is on content rather than on key order or rounding noise. The
# window bounds are dropped: they are the wall clock, not the measurement.
json_lines() {
  sed -n '/^--- worker summaries/,$p' "$1" | grep '^{' \
    | jq -c -S 'del(.window_start, .window_end)' | sort
}

# table_row <out> <worker> — the worker's row of the human table, as the fields
# the table reports. Asserting fields keeps the padding free to change.
table_row() {
  awk -v w="$2" '$1 == w {print $3"|"$4"|"$5"|"$6"|"$7"|"$8"|"$9}' "$1"
}

# --- Tests -----------------------------------------------------------------

test_five_note_fixture() {
  echo ""
  echo "=== 5-note fixture: per-worker table ==="
  new_case

  make_repo "$CASE_DIR/alpha"
  add_note "$CASE_DIR/alpha" 1 "$(note_json alpha 0.80 approve 2 1 21600 \
    '{"findings":[{"id":"a","severity":"minor"}],"attempts":[{"score":0.55,"verdict":"request_changes","attempt":1,"findings_count":2},{"score":0.80,"verdict":"approve","attempt":2,"findings_count":1}]}' alpha-rig)"
  add_note "$CASE_DIR/alpha" 2 "$(note_json alpha 0.85 approve 1 0 18000 \
    '{"findings":[]}' alpha-rig)"
  add_note "$CASE_DIR/alpha" 3 "$(note_json alpha 0.75 approve 2 2 14400 \
    '{"findings":[{"id":"b","severity":"major"},{"id":"c","severity":"minor"}],"attempts":[{"score":0.60,"verdict":"request_changes","attempt":1,"findings_count":3},{"score":0.75,"verdict":"approve","attempt":2,"findings_count":2}]}' alpha-rig)"
  publish_notes "$CASE_DIR/alpha"

  make_repo "$CASE_DIR/beta"
  # No findings array: the major count has to come from the follow-up beads.
  add_note "$CASE_DIR/beta" 1 "$(note_json beta 0.30 request_changes 1 2 18000 \
    '{"followups":["gt-f1","gt-f2"]}' beta-rig)"
  add_note "$CASE_DIR/beta" 2 "$(note_json beta 0.35 request_changes 2 13 14400 \
    '{"findings":[{"id":"d","severity":"major"}],"attempts":[{"score":0.30,"verdict":"request_changes","attempt":1,"findings_count":2}]}' beta-rig)"
  publish_notes "$CASE_DIR/beta"

  cat > "$TEST_STATE/rigs.json" <<JSON
[{"name":"alpha-rig","status":"operational","repo_path":"$CASE_DIR/alpha/work"},
 {"name":"beta-rig","status":"operational","repo_path":"$CASE_DIR/beta/work"}]
JSON

  run_plugin "$CASE_DIR/out"
  assert_eq "$PLUGIN_RC" "0" "5 notes: exits 0"

  assert_eq "$(table_row "$CASE_DIR/out" alpha)" "3|33%|0.8|1|1|declining|WARN" \
    "alpha row: 3 reviews, 1/3 first-attempt approves, mean 0.8, declining, WARN"
  assert_eq "$(table_row "$CASE_DIR/out" beta)" "2|0%|0.33|7.5|3|stable|BREACH" \
    "beta row: 2 reviews, mean 0.325, 3 majors from findings, BREACH"

  assert_eq "$(json_lines "$CASE_DIR/out" | jq -c -S 'select(.worker=="alpha")')" \
    '{"first_attempt_approve_rate":0.3333,"majors":1,"mean_findings":1,"mean_score":0.8,"mrs":["gt-wisp-alpha"],"mrs_total":1,"request_changes":0,"reviews":3,"rig":"alpha-rig","status":"WARN","trend":"declining","worker":"alpha"}' \
    "alpha JSON summary carries the same numbers"
  assert_eq "$(json_lines "$CASE_DIR/out" | jq -c -S 'select(.worker=="beta")')" \
    '{"first_attempt_approve_rate":0,"majors":3,"mean_findings":7.5,"mean_score":0.325,"mrs":["gt-wisp-beta"],"mrs_total":1,"request_changes":2,"reviews":2,"rig":"beta-rig","status":"BREACH","trend":"stable","worker":"beta"}' \
    "beta JSON summary carries the same numbers"

  assert_file_not_contains "$TEST_STATE/receipts.log" '"' "5 notes: the window in the receipt is unquoted"
  assert_file_not_contains "$TEST_STATE/mail.log" 'window "' "5 notes: the window in the alert is unquoted"

  assert_file_contains "$TEST_STATE/receipts.log" "--result success" "5 notes: success receipt recorded"
  assert_file_contains "$TEST_STATE/receipts.log" "quality-review: 2 worker(s), 1 breach(es)" \
    "5 notes: receipt names the worker and breach counts"

  # BREACH alerts: a durable mail plus a keyed escalation, and only for beta.
  assert_file_contains "$TEST_STATE/mail.log" "Quality BREACH: beta" "5 notes: beta breach mailed to the deacon"
  assert_file_contains "$TEST_STATE/mail.log" "Mean score: 0.325" "5 notes: breach mail carries the numbers"
  assert_file_not_contains "$TEST_STATE/mail.log" "alpha" "5 notes: no alert for a non-breaching worker"
  assert_file_contains "$TEST_STATE/escalations.log" "--fingerprint quality-review:breach:beta-rig/beta" \
    "5 notes: beta escalation carries a stable per-worker key"
  assert_file_contains "$TEST_STATE/clears.log" "--fingerprint quality-review:breach:alpha-rig/alpha" \
    "5 notes: alpha's key is closed rather than left for a human"
  assert_file_not_contains "$TEST_STATE/clears.log" "beta-rig/beta" \
    "5 notes: the breaching worker's key is not cleared"
}

test_empty_window_is_success() {
  echo ""
  echo "=== empty window ==="
  new_case

  make_repo "$CASE_DIR/old"
  add_note "$CASE_DIR/old" 1 "$(note_json alpha 0.90 approve 1 0 259200 '{"findings":[]}' alpha-rig)"
  publish_notes "$CASE_DIR/old"

  cat > "$TEST_STATE/rigs.json" <<JSON
[{"name":"alpha-rig","status":"operational","repo_path":"$CASE_DIR/old/work"}]
JSON

  run_plugin "$CASE_DIR/out"
  assert_eq "$PLUGIN_RC" "0" "empty window: exits 0, not 2"
  assert_file_contains "$CASE_DIR/out" "no om notes in" "empty window: says so"
  assert_eq "$(json_lines "$CASE_DIR/out" | wc -l | tr -d ' ')" "0" "empty window: no worker summaries"
  assert_file_contains "$TEST_STATE/receipts.log" "--result success" "empty window: still records a receipt"
}

test_no_notes_ref() {
  echo ""
  echo "=== rig with no om notes ==="
  new_case

  make_repo "$CASE_DIR/bare"
  cat > "$TEST_STATE/rigs.json" <<JSON
[{"name":"bare-rig","status":"operational","repo_path":"$CASE_DIR/bare/work"}]
JSON

  run_plugin "$CASE_DIR/out"
  assert_eq "$PLUGIN_RC" "0" "no notes: exits 0"
  assert_file_contains "$CASE_DIR/out" "no refs/notes/om" "no notes: reports the empty notes ref"
  assert_file_contains "$TEST_STATE/receipts.log" "--result success" "no notes: records a receipt"
}

test_unparseable_note_is_skipped() {
  echo ""
  echo "=== note that is not JSON ==="
  new_case

  make_repo "$CASE_DIR/mixed"
  add_note "$CASE_DIR/mixed" 1 'this is not json'
  add_note "$CASE_DIR/mixed" 2 "$(note_json alpha 0.90 approve 1 0 3600 '{"findings":[]}' alpha-rig)"
  publish_notes "$CASE_DIR/mixed"

  cat > "$TEST_STATE/rigs.json" <<JSON
[{"name":"alpha-rig","status":"operational","repo_path":"$CASE_DIR/mixed/work"}]
JSON

  run_plugin "$CASE_DIR/out"
  assert_eq "$PLUGIN_RC" "0" "unparseable note: exits 0"
  assert_file_contains "$CASE_DIR/out" "SKIP unparseable note" "unparseable note: logged, not silent"
  assert_file_contains "$TEST_STATE/receipts.log" "1 unparseable" "unparseable note: counted in the receipt"
  assert_eq "$(table_row "$CASE_DIR/out" alpha)" "1|100%|0.9|0|0|stable|OK" \
    "unparseable note: the readable note still counts"
}

test_undatable_note_alone_is_fatal() {
  echo ""
  echo "=== a ref this run cannot place is not a clean window ==="
  new_case

  make_repo "$CASE_DIR/anon"
  # Nothing to window it by and nothing to attribute it to: notes were read and
  # the run learned nothing. A renamed reviewed_at or worker field looks exactly
  # like this, and reporting it as an empty window is the fail-open branch the
  # prose plugin shipped with --include-infra (gt-gs7g).
  add_note "$CASE_DIR/anon" 1 '{"verdict":"approve","score":0.7}'
  publish_notes "$CASE_DIR/anon"

  cat > "$TEST_STATE/rigs.json" <<JSON
[{"name":"alpha-rig","status":"operational","repo_path":"$CASE_DIR/anon/work"}]
JSON

  run_plugin "$CASE_DIR/out"
  assert_eq "$PLUGIN_RC" "1" "undatable note: exits 1, never 0"
  assert_file_contains "$CASE_DIR/out" "no readable reviewed_at" "undatable note: names the unreadable field"
  assert_file_contains "$TEST_STATE/receipts.log" "--result failure" "undatable note: records a failure"
  assert_file_not_contains "$TEST_STATE/receipts.log" "--result success" "undatable note: never records success"
  assert_file_contains "$TEST_STATE/escalations.log" "--fingerprint quality-review:failed" "undatable note: escalates"
}

test_all_in_window_notes_unattributable_is_fatal() {
  echo ""
  echo "=== notes in the window, none with a worker ==="
  new_case

  make_repo "$CASE_DIR/anon"
  add_note "$CASE_DIR/anon" 1 \
    "{\"rig\":\"alpha-rig\",\"mr\":\"m1\",\"score\":0.5,\"verdict\":\"approve\",\"reviewed_at\":\"$(iso_at 3600)\"}"
  add_note "$CASE_DIR/anon" 2 \
    "{\"rig\":\"alpha-rig\",\"mr\":\"m2\",\"score\":0.5,\"verdict\":\"approve\",\"reviewed_at\":\"$(iso_at 1800)\"}"
  publish_notes "$CASE_DIR/anon"

  cat > "$TEST_STATE/rigs.json" <<JSON
[{"name":"alpha-rig","status":"operational","repo_path":"$CASE_DIR/anon/work"}]
JSON

  run_plugin "$CASE_DIR/out"
  assert_eq "$PLUGIN_RC" "1" "unattributable window: exits 1"
  assert_file_contains "$CASE_DIR/out" "2 of 2 note(s) in" "unattributable window: names the share"
  assert_file_contains "$TEST_STATE/receipts.log" "--result failure" "unattributable window: records a failure"
  assert_file_not_contains "$TEST_STATE/receipts.log" "--result success" "unattributable window: never records success"
}

test_unattributable_minority_is_tolerated() {
  echo ""
  echo "=== one foreign note beside healthy ones ==="
  new_case

  make_repo "$CASE_DIR/mixed"
  # A note with a window-placed date but no worker: a hole in the measurement,
  # counted and reported — but one hole in four is not a blind run.
  add_note "$CASE_DIR/mixed" 1 \
    "{\"rig\":\"alpha-rig\",\"mr\":\"m0\",\"score\":0.5,\"verdict\":\"approve\",\"reviewed_at\":\"$(iso_at 5400)\"}"
  add_note "$CASE_DIR/mixed" 2 "$(note_json alpha 0.90 approve 1 0 3600 '{"findings":[]}' alpha-rig)"
  add_note "$CASE_DIR/mixed" 3 "$(note_json alpha 0.80 approve 1 0 3000 '{"findings":[]}' alpha-rig)"
  add_note "$CASE_DIR/mixed" 4 "$(note_json alpha 0.85 approve 1 0 2400 '{"findings":[]}' alpha-rig)"
  publish_notes "$CASE_DIR/mixed"

  cat > "$TEST_STATE/rigs.json" <<JSON
[{"name":"alpha-rig","status":"operational","repo_path":"$CASE_DIR/mixed/work"}]
JSON

  run_plugin "$CASE_DIR/out"
  assert_eq "$PLUGIN_RC" "0" "minority unattributable: exits 0"
  assert_file_contains "$TEST_STATE/receipts.log" "1 unattributable" \
    "minority unattributable: counted in the receipt, not dropped silently"
  assert_eq "$(table_row "$CASE_DIR/out" alpha)" "3|100%|0.85|0|0|stable|OK" \
    "minority unattributable: the readable notes still count"
}

test_copied_note_counts_once() {
  echo ""
  echo "=== the gate copies a note onto the landed commit ==="
  new_case

  make_repo "$CASE_DIR/alpha"
  add_note "$CASE_DIR/alpha" 1 "$(note_json alpha 0.90 approve 1 0 3600 '{"findings":[]}' alpha-rig)"
  copy_note "$CASE_DIR/alpha" 2
  publish_notes "$CASE_DIR/alpha"

  cat > "$TEST_STATE/rigs.json" <<JSON
[{"name":"alpha-rig","status":"operational","repo_path":"$CASE_DIR/alpha/work"}]
JSON

  run_plugin "$CASE_DIR/out"
  assert_eq "$PLUGIN_RC" "0" "copied note: exits 0"
  assert_eq "$(table_row "$CASE_DIR/out" alpha)" "1|100%|0.9|0|0|stable|OK" \
    "copied note: one verdict on two commits is one review"
  assert_eq "$(json_lines "$CASE_DIR/out" | jq -c -S 'select(.worker=="alpha") | .reviews')" "1" \
    "copied note: the JSON summary agrees"
}

test_reroll_counts_once() {
  echo ""
  echo "=== a re-rolled verdict replaces the one it was re-rolled from ==="
  new_case

  make_repo "$CASE_DIR/alpha"
  # A re-roll writes its new note on a fresh rehearsal commit and leaves the
  # note it replaced on the old one (gt-bveg). Both carry the same patch_id.
  add_note "$CASE_DIR/alpha" 1 "$(note_json alpha 0.40 request_changes 1 3 5400 \
    '{"patch_id":"p1","findings":[{"id":"a","severity":"major"}]}' alpha-rig)"
  add_note "$CASE_DIR/alpha" 2 "$(note_json alpha 0.72 approve 2 1 3600 \
    '{"patch_id":"p1","findings":[{"id":"a","severity":"major"}],"attempts":[{"score":0.40,"verdict":"request_changes","attempt":1,"findings_count":3},{"score":0.72,"verdict":"approve","attempt":2,"findings_count":1}]}' alpha-rig)"
  publish_notes "$CASE_DIR/alpha"

  cat > "$TEST_STATE/rigs.json" <<JSON
[{"name":"alpha-rig","status":"operational","repo_path":"$CASE_DIR/alpha/work"}]
JSON

  run_plugin "$CASE_DIR/out"
  assert_eq "$PLUGIN_RC" "0" "re-roll: exits 0"
  # Two rows here would read 2 reviews, mean 0.56, 2 majors, BREACH — the
  # re-roll counted as a second review of the same diff.
  assert_eq "$(table_row "$CASE_DIR/out" alpha)" "1|0%|0.72|1|1|stable|WARN" \
    "re-roll: the surviving note is the latest, carrying the attempt history"
}

test_gate_notes_ref_is_not_written() {
  echo ""
  echo "=== the gate's own ref is not written by a report ==="
  new_case

  make_repo "$CASE_DIR/alpha"
  add_note "$CASE_DIR/alpha" 1 "$(note_json alpha 0.90 approve 1 0 3600 '{"findings":[]}' alpha-rig)"
  publish_notes "$CASE_DIR/alpha"
  # A verdict the gate has written locally but not pushed yet (gt-2rcx): a
  # force-fetch into refs/notes/om would replace the ref that holds it.
  add_note "$CASE_DIR/alpha" 2 "$(note_json alpha 0.20 request_changes 1 9 1800 '{"findings":[]}' alpha-rig)"
  local before
  before=$(git -C "$CASE_DIR/alpha/work" rev-parse refs/notes/om)

  cat > "$TEST_STATE/rigs.json" <<JSON
[{"name":"alpha-rig","status":"operational","repo_path":"$CASE_DIR/alpha/work"}]
JSON

  run_plugin "$CASE_DIR/out"
  assert_eq "$PLUGIN_RC" "0" "gate ref: exits 0"
  assert_eq "$(git -C "$CASE_DIR/alpha/work" rev-parse refs/notes/om)" "$before" \
    "gate ref: the local ref is byte-for-byte where it was"
  assert_eq "$(table_row "$CASE_DIR/out" alpha)" "1|100%|0.9|0|0|stable|OK" \
    "gate ref: only what origin published is measured"
}

test_non_object_note_is_skipped() {
  echo ""
  echo "=== note that is valid JSON but not an object ==="
  new_case

  make_repo "$CASE_DIR/odd"
  add_note "$CASE_DIR/odd" 1 '42'
  add_note "$CASE_DIR/odd" 2 "$(note_json alpha 0.90 approve 1 0 3600 '{"findings":[]}' alpha-rig)"
  publish_notes "$CASE_DIR/odd"

  cat > "$TEST_STATE/rigs.json" <<JSON
[{"name":"alpha-rig","status":"operational","repo_path":"$CASE_DIR/odd/work"}]
JSON

  run_plugin "$CASE_DIR/out"
  assert_eq "$PLUGIN_RC" "0" "non-object note: exits 0 — one foreign writer does not sink the report"
  assert_file_contains "$TEST_STATE/receipts.log" "1 unparseable" "non-object note: counted"
  assert_eq "$(table_row "$CASE_DIR/out" alpha)" "1|100%|0.9|0|0|stable|OK" \
    "non-object note: the verdict beside it still counts"
}

test_non_numeric_score_is_skipped() {
  echo ""
  echo "=== note whose score is not a number ==="
  new_case

  make_repo "$CASE_DIR/scored"
  add_note "$CASE_DIR/scored" 1 \
    "{\"rig\":\"alpha-rig\",\"mr\":\"m0\",\"worker\":\"junk\",\"score\":\"high\",\"verdict\":\"approve\",\"reviewed_at\":\"$(iso_at 3600)\"}"
  add_note "$CASE_DIR/scored" 2 "$(note_json alpha 0.90 approve 1 0 3000 '{"findings":[]}' alpha-rig)"
  publish_notes "$CASE_DIR/scored"

  cat > "$TEST_STATE/rigs.json" <<JSON
[{"name":"alpha-rig","status":"operational","repo_path":"$CASE_DIR/scored/work"}]
JSON

  run_plugin "$CASE_DIR/out"
  assert_eq "$PLUGIN_RC" "0" "string score: exits 0"
  assert_file_contains "$TEST_STATE/receipts.log" "1 unparseable" "string score: counted, not averaged in as a zero"
  assert_eq "$(table_row "$CASE_DIR/out" alpha)" "1|100%|0.9|0|0|stable|OK" \
    "string score: the good note is untouched"
}

test_absent_worker_key_is_cleared() {
  echo ""
  echo "=== a worker with no reviews this window is still cleared ==="
  new_case

  make_repo "$CASE_DIR/alpha"
  # Quiet: reviewed three days ago and never since — inside the sweep's horizon,
  # outside the window. Their breach key is the one escalate clear cannot reach
  # by looking at the window alone.
  add_note "$CASE_DIR/alpha" 1 "$(note_json quiet 0.30 request_changes 1 2 259200 '{"findings":[]}' alpha-rig)"
  add_note "$CASE_DIR/alpha" 2 "$(note_json alpha 0.90 approve 1 0 3600 '{"findings":[]}' alpha-rig)"
  publish_notes "$CASE_DIR/alpha"

  cat > "$TEST_STATE/rigs.json" <<JSON
[{"name":"alpha-rig","status":"operational","repo_path":"$CASE_DIR/alpha/work"}]
JSON

  run_plugin "$CASE_DIR/out"
  assert_eq "$PLUGIN_RC" "0" "absent worker: exits 0"
  assert_eq "$(json_lines "$CASE_DIR/out" | wc -l | tr -d ' ')" "1" "absent worker: not a row in this window"
  assert_file_contains "$TEST_STATE/clears.log" "--fingerprint quality-review:breach:alpha-rig/quiet" \
    "absent worker: the stale key is closed rather than left for a human"
  assert_file_contains "$TEST_STATE/clears.log" "--fingerprint quality-review:failed" \
    "a run that reported closes its own failure key"
}

test_failed_git_fetch_is_fatal() {
  echo ""
  echo "=== git fetch failure ==="
  new_case

  make_repo "$CASE_DIR/broken"
  git -C "$CASE_DIR/broken/work" remote set-url origin "$CASE_DIR/does-not-exist.git"
  cat > "$TEST_STATE/rigs.json" <<JSON
[{"name":"broken-rig","status":"operational","repo_path":"$CASE_DIR/broken/work"}]
JSON

  run_plugin "$CASE_DIR/out"
  assert_eq "$PLUGIN_RC" "1" "fetch failure: exits 1"
  assert_file_contains "$CASE_DIR/out" "git fetch of refs/notes/om failed" "fetch failure: names the failing call"
  assert_file_contains "$TEST_STATE/receipts.log" "--result failure" "fetch failure: records a failure, not a clean window"
  assert_file_contains "$TEST_STATE/escalations.log" "--fingerprint quality-review:failed" \
    "fetch failure: escalates"
  assert_file_not_contains "$TEST_STATE/receipts.log" "--result success" \
    "fetch failure: never records success"
}

test_registry_failure_is_fatal() {
  echo ""
  echo "=== rig registry unavailable ==="
  new_case
  : > "$TEST_STATE/rigs_fail"

  run_plugin "$CASE_DIR/out"
  assert_eq "$PLUGIN_RC" "1" "registry failure: exits 1"
  assert_file_contains "$TEST_STATE/receipts.log" "--result failure" "registry failure: records a failure"
  assert_file_not_contains "$TEST_STATE/receipts.log" "--result success" "registry failure: never records success"
}

test_no_operational_rigs() {
  echo ""
  echo "=== only parked rigs ==="
  new_case

  cat > "$TEST_STATE/rigs.json" <<JSON
[{"name":"parked-rig","status":"parked","repo_path":"$CASE_DIR/nonexistent"}]
JSON

  run_plugin "$CASE_DIR/out"
  assert_eq "$PLUGIN_RC" "0" "parked rigs: exits 0 without touching the checkout"
  assert_file_contains "$CASE_DIR/out" "no operational rig" "parked rigs: reported as skipped"
  assert_file_contains "$CASE_DIR/out" "[plugin-result skipped]" \
    "parked rigs: prints the daemon's marker for a run that accomplished nothing"
  assert_file_contains "$TEST_STATE/receipts.log" "--result skipped" \
    "parked rigs: the receipt says skipped, not success"
  assert_file_not_contains "$TEST_STATE/receipts.log" "--result success" \
    "parked rigs: success is not recorded for a no-op"
}

test_missing_checkout_is_fatal() {
  echo ""
  echo "=== operational rig with no checkout ==="
  new_case

  cat > "$TEST_STATE/rigs.json" <<JSON
[{"name":"gone-rig","status":"operational","repo_path":"$CASE_DIR/absent"}]
JSON

  run_plugin "$CASE_DIR/out"
  assert_eq "$PLUGIN_RC" "1" "missing checkout: exits 1"
  assert_file_contains "$TEST_STATE/receipts.log" "--result failure" "missing checkout: records a failure"
}

test_shared_checkout_is_read_once() {
  echo ""
  echo "=== two rigs, one checkout ==="
  new_case

  make_repo "$CASE_DIR/shared"
  add_note "$CASE_DIR/shared" 1 "$(note_json alpha 0.90 approve 1 0 3600 '{"findings":[]}' alpha-rig)"
  publish_notes "$CASE_DIR/shared"

  cat > "$TEST_STATE/rigs.json" <<JSON
[{"name":"alpha-rig","status":"operational","repo_path":"$CASE_DIR/shared/work"},
 {"name":"alpha-alias","status":"operational","repo_path":"$CASE_DIR/shared/work"}]
JSON

  run_plugin "$CASE_DIR/out"
  assert_eq "$PLUGIN_RC" "0" "shared checkout: exits 0"
  assert_file_contains "$CASE_DIR/out" "shares a checkout" "shared checkout: second rig skipped"
  assert_eq "$(table_row "$CASE_DIR/out" alpha)" "1|100%|0.9|0|0|stable|OK" \
    "shared checkout: the note is counted once, not twice"
}

# --- Main ------------------------------------------------------------------

command -v jq >/dev/null 2>&1 || { echo "SKIP: jq not installed"; exit 0; }

test_five_note_fixture
test_copied_note_counts_once
test_reroll_counts_once
test_empty_window_is_success
test_no_notes_ref
test_unparseable_note_is_skipped
test_non_object_note_is_skipped
test_non_numeric_score_is_skipped
test_undatable_note_alone_is_fatal
test_all_in_window_notes_unattributable_is_fatal
test_unattributable_minority_is_tolerated
test_absent_worker_key_is_cleared
test_gate_notes_ref_is_not_written
test_failed_git_fetch_is_fatal
test_registry_failure_is_fatal
test_no_operational_rigs
test_missing_checkout_is_fatal
test_shared_checkout_is_read_once

echo ""
if [ "$FAIL" -gt 0 ]; then
  echo "FAILED: $FAIL test assertion(s) failed ($PASS passed)"
  exit 1
fi
echo "PASSED: all $PASS assertions passed"
