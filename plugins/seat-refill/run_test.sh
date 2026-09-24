#!/usr/bin/env bash
# run_test.sh — the seat and empty-episode logic in run.sh, driven against a
# fake town.
#
# The episode rules are the whole plugin, and every one of them is a decision
# about silence: fire once when a seat has been empty long enough with work
# ready, stay silent when it has not, when there is nothing to take the seat,
# when a rig is parked, and when the operator has pulled the hold. The fake
# `gt` below answers the four calls run.sh makes, so each case is one state
# file plus one fixture, and no tmux server, Dolt, or town is involved.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
SCRIPT="$ROOT_DIR/plugins/seat-refill/run.sh"
ORIGINAL_PATH="$PATH"

PASS=0
FAIL=0
CLEANUP_DIRS=()

cleanup() {
  local dir
  for dir in "${CLEANUP_DIRS[@]:-}"; do
    [ -n "$dir" ] && rm -rf "$dir"
  done
  return 0
}
trap cleanup EXIT

record_pass() { PASS=$((PASS + 1)); printf 'PASS: %s\n' "$1"; }
record_fail() { FAIL=$((FAIL + 1)); printf 'FAIL: %s\n' "$1"; }

assert_eq() {
  local got="$1" want="$2" label="$3"
  if [ "$got" = "$want" ]; then
    record_pass "$label"
  else
    record_fail "$label"
    printf '  got:  %s\n  want: %s\n' "$got" "$want"
  fi
}

assert_contains() {
  local file="$1" needle="$2" label="$3"
  if [ -f "$file" ] && grep -Fq -- "$needle" "$file"; then
    record_pass "$label"
  else
    record_fail "$label"
    printf '  expected %q in %s\n' "$needle" "$file"
    if [ -f "$file" ]; then sed 's/^/    /' "$file"; else printf '  (file does not exist)\n'; fi
  fi
}

assert_not_contains() {
  local file="$1" needle="$2" label="$3"
  if ! grep -Fq -- "$needle" "$file" 2>/dev/null; then
    record_pass "$label"
  else
    record_fail "$label"
    printf '  did not expect %q in %s\n' "$needle" "$file"
    sed 's/^/    /' "$file"
  fi
  return 0
}

# --- Fake timeout ---------------------------------------------------------
# Records the bound run.sh puts on each command, then runs the command — or,
# with $TEST_STATE/timeout_expires present, exits 124 as coreutils timeout does
# when the bound fires, without running it. The real `timeout` never runs, so
# no case waits on a clock.
write_fake_timeout() {
  local bin_dir="$1"
  cat > "$bin_dir/timeout" <<'SH'
#!/usr/bin/env bash
printf '%s|%s\n' "$1" "$2" >> "$TEST_STATE/timeout.log"
if [ -f "$TEST_STATE/timeout_expires" ]; then
  exit 124
fi
shift
exec "$@"
SH
  chmod +x "$bin_dir/timeout"
}

# --- Fake town ------------------------------------------------------------
# Four gt calls, each backed by a fixture file so a case is pure data:
#   polecat list --all --json   $TEST_STATE/polecats.json   (absent -> [])
#   rig list --json             $TEST_STATE/rigs.json
#   ready --rig <rig> --json    $TEST_STATE/ready/<rig>.json
#   nudge <target> <message>    appends to $TEST_STATE/nudge.log
write_fake_gt() {
  local bin_dir="$1"

  cat > "$bin_dir/gt" <<'SH'
#!/usr/bin/env bash
set -euo pipefail

case "${1:-}" in
  polecat)
    if [ "${2:-}" = "list" ]; then
      if [ -f "$TEST_STATE/polecat_list_fails" ]; then
        echo "bd: connection refused" >&2
        exit 1
      fi
      if [ -f "$TEST_STATE/polecats.json" ]; then
        cat "$TEST_STATE/polecats.json"
      else
        echo '[]'
      fi
      exit 0
    fi
    exit 1
    ;;
  rig)
    if [ "${2:-}" = "list" ]; then
      cat "$TEST_STATE/rigs.json"
      exit 0
    fi
    exit 1
    ;;
  ready)
    rig=""
    while [ $# -gt 0 ]; do
      case "$1" in
        --rig) rig="${2:-}"; shift 2 ;;
        *) shift ;;
      esac
    done
    if [ -f "$TEST_STATE/ready_fails" ]; then
      echo "bd: database not found" >&2
      exit 1
    fi
    if [ -f "$TEST_STATE/ready/$rig.json" ]; then
      cat "$TEST_STATE/ready/$rig.json"
    else
      printf '{"sources":[{"name":"%s","issues":[]}],"summary":{},"town_root":"/town"}\n' "$rig"
    fi
    exit 0
    ;;
  nudge)
    shift
    target="${1:-}"
    shift || true
    printf 'NUDGE|%s|%s\n' "$target" "$*" >> "$TEST_STATE/nudge.log"
    if [ -f "$TEST_STATE/nudge_fails" ]; then
      echo "nudge: could not reach target" >&2
      exit 1
    fi
    exit 0
    ;;
  *)
    printf 'UNEXPECTED gt call: %s\n' "$*" >> "$TEST_STATE/unexpected.log"
    exit 1
    ;;
esac
SH
  chmod +x "$bin_dir/gt"
}

# setup_case builds a fresh town in a temp dir. Every GT_SEAT_REFILL_* knob is
# cleared first, so a case that sets one can only affect itself.
TEST_STATE=""
CASE_DIR=""
setup_case() {
  CASE_DIR=$(mktemp -d)
  CLEANUP_DIRS+=("$CASE_DIR")
  TEST_STATE="$CASE_DIR/state"
  mkdir -p "$TEST_STATE/ready" "$TEST_STATE/bin"

  unset GT_SEAT_REFILL_SONNET_MAX GT_SEAT_REFILL_SONNET_AGENT GT_SEAT_REFILL_SONNET_LABEL
  unset GT_SEAT_REFILL_EMPTY_SECONDS GT_SEAT_REFILL_NUDGE_SECONDS GT_SEAT_REFILL_MAYOR
  unset GT_SEAT_REFILL_CLAIM_TTL GT_SEAT_REFILL_TOP_CANDIDATES GT_SEAT_REFILL_MAX_PRIORITY

  # The pool every case starts from: one local seat, one capped overflow seat.
  cat > "$CASE_DIR/settings.json" <<'JSON'
{
  "type": "town-settings",
  "polecat_pool": {
    "local_agent": "local-coder-polecat",
    "max_local": 1,
    "overflow_agent": "deepseek-flash",
    "max_overflow": 1
  }
}
JSON
  cat > "$TEST_STATE/rigs.json" <<'JSON'
[
  {"name": "gastown", "status": "operational"},
  {"name": "om", "status": "operational"},
  {"name": "beads", "status": "parked"}
]
JSON
  : > "$TEST_STATE/nudge.log"
  : > "$TEST_STATE/unexpected.log"

  write_fake_gt "$TEST_STATE/bin"
  write_fake_timeout "$TEST_STATE/bin"
  export PATH="$TEST_STATE/bin:$ORIGINAL_PATH"
  export TEST_STATE
  export GT_TOWN_ROOT="$CASE_DIR"
  export GT_SEAT_REFILL_CONFIG="$CASE_DIR/settings.json"
  export GT_SEAT_REFILL_STATE="$CASE_DIR/state.json"
  export GT_SEAT_REFILL_HOLD="$CASE_DIR/seat-refill.hold"
}

# run_plugin <now-epoch>: runs the plugin with a pinned clock, leaving stdout in
# $TEST_STATE/stdout.log and stderr in $TEST_STATE/stderr.log, and EXIT set.
run_plugin() {
  set +e
  GT_SEAT_REFILL_NOW="$1" bash "$SCRIPT" >"$TEST_STATE/stdout.log" 2>"$TEST_STATE/stderr.log"
  EXIT=$?
  set -e
}

nudges() { grep -c 'NUDGE' "$TEST_STATE/nudge.log" || true; }
nudges_of() { grep -c "$1" "$TEST_STATE/nudge.log" || true; }

# ready_bug <rig>: one P1 bug, ready and unassigned, in that rig.
ready_bug() {
  local rig="${1:-gastown}"
  cat > "$TEST_STATE/ready/$rig.json" <<JSON
{"sources":[{"name":"$rig","issues":[
  {"id":"gt-bug1","title":"A real bug","status":"open","priority":1,"issue_type":"bug"}
]}],"summary":{},"town_root":"/town"}
JSON
}

write_polecats() { printf '%s\n' "$1" > "$TEST_STATE/polecats.json"; }

# Two live polecats, both seats taken.
LIVE_BOTH='[{"rig":"gastown","name":"a","agent":"local-coder-polecat","session_running":true},
            {"rig":"gastown","name":"b","agent":"deepseek-flash","session_running":true}]'
# Only the overflow seat taken, so the local seat is the one that fires.
LIVE_OVERFLOW_ONLY='[{"rig":"gastown","name":"b","agent":"deepseek-flash","session_running":true}]'
LIVE_NONE='[]'

# --- Case 1: first observation starts an episode, and does not fire --------
setup_case
write_polecats "$LIVE_NONE"
ready_bug gastown
run_plugin 1000000
assert_eq "$EXIT" "0" "first sighting: exits 0"
assert_eq "$(nudges)" "0" "first sighting: no nudge at t=0"
assert_eq "$(jq -r '.episodes.local.empty_since' "$GT_SEAT_REFILL_STATE")" "1000000" \
  "first sighting: episode records empty_since"
assert_contains "$TEST_STATE/stdout.log" "[plugin-result skipped]" \
  "first sighting: the receipt says skipped, not success"

# --- Case 2: five minutes empty with work fires exactly one nudge ---------
# The overflow seat fills, so only the local seat is empty from here on.
write_polecats "$LIVE_OVERFLOW_ONLY"
run_plugin 1000300
assert_eq "$EXIT" "0" "threshold: exits 0"
assert_eq "$(nudges)" "1" "threshold: exactly one nudge at 5m"
assert_contains "$TEST_STATE/nudge.log" "NUDGE|mayor|" "threshold: addressed to the mayor"
assert_contains "$TEST_STATE/nudge.log" "seat local" "threshold: names the seat"
assert_contains "$TEST_STATE/nudge.log" "empty 300s" "threshold: names the empty duration"
assert_contains "$TEST_STATE/nudge.log" "gt-bug1 (P1 gastown)" "threshold: names the candidate bead"
assert_not_contains "$TEST_STATE/nudge.log" "seat overflow" \
  "threshold: the occupied seat is not named"
assert_not_contains "$TEST_STATE/stdout.log" "[plugin-result skipped]" \
  "threshold: a firing run is not recorded as skipped"

# --- Case 3: the same episode holds for 15m -------------------------------
run_plugin 1000600
assert_eq "$(nudges)" "1" "repeat cap: still one nudge at 10m"

# --- Case 4: and fires again once 15m have passed -------------------------
run_plugin 1001300
assert_eq "$(nudges)" "2" "repeat cap: a second nudge 15m after the first"

# --- Case 5: a filled seat ends the episode -------------------------------
setup_case
write_polecats "$LIVE_NONE"
ready_bug gastown
run_plugin 2000000
run_plugin 2000300
assert_eq "$(nudges)" "1" "fill: fires while empty"
write_polecats "$LIVE_BOTH"
run_plugin 2000600
assert_eq "$(jq -r '.episodes | has("local")' "$GT_SEAT_REFILL_STATE")" "false" \
  "fill: the episode is dropped when the seat fills"
run_plugin 2000900
assert_eq "$(nudges)" "1" "fill: the refilled seat does not fire again"

# --- Case 6: an empty seat with nothing to take it stays silent -----------
setup_case
write_polecats "$LIVE_NONE"
run_plugin 3000000
run_plugin 3000600
assert_eq "$(nudges)" "0" "no work: silent with empty rigs"
assert_eq "$(jq -r '.episodes.local.empty_since' "$GT_SEAT_REFILL_STATE")" "3000000" \
  "no work: the episode still runs, so work arriving later is nudged promptly"
ready_bug om
run_plugin 3000700
assert_eq "$(nudges)" "1" "no work: work arriving mid-episode fires without a fresh 5m wait"

# --- Case 7: the type and priority filters ---------------------------------
setup_case
write_polecats "$LIVE_NONE"
cat > "$TEST_STATE/ready/gastown.json" <<'JSON'
{"sources":[{"name":"gastown","issues":[
  {"id":"gt-p3","title":"Backlog","status":"open","priority":3,"issue_type":"bug"},
  {"id":"gt-chore","title":"A chore","status":"open","priority":1,"issue_type":"chore"},
  {"id":"gt-epic","title":"A container","status":"open","priority":1,"issue_type":"epic"},
  {"id":"gt-taken","title":"Already owned","status":"open","priority":1,"issue_type":"task","assignee":"gastown/polecats/x"},
  {"id":"gt-envelope","title":"STATE_COLLAPSE gastown","status":"open","priority":0,"issue_type":"bug"}
]}],"summary":{},"town_root":"/town"}
JSON
run_plugin 4000000
run_plugin 4000600
assert_eq "$(nudges)" "0" \
  "filters: P3, chore, epic, assigned, and envelope beads are not work"

# --- Case 8: a parked rig is not a dispatch target -------------------------
setup_case
write_polecats "$LIVE_NONE"
ready_bug beads
run_plugin 5000000
run_plugin 5000600
assert_eq "$(nudges)" "0" "parked rig: its ready work is not a reason to nudge"

# --- Case 9: the operator hold flag ---------------------------------------
setup_case
write_polecats "$LIVE_NONE"
ready_bug gastown
run_plugin 6000000
touch "$GT_SEAT_REFILL_HOLD"
run_plugin 6000600
assert_eq "$(nudges)" "0" "hold flag: no nudge while the flag is set"
assert_contains "$TEST_STATE/stdout.log" "hold flag" "hold flag: the run says why it was skipped"
rm -f "$GT_SEAT_REFILL_HOLD"
run_plugin 6000700
assert_eq "$(nudges)" "1" "hold flag: removing the flag resumes the episode"

# --- Case 10: estop ------------------------------------------------------
setup_case
write_polecats "$LIVE_NONE"
ready_bug gastown
run_plugin 7000000
touch "$GT_TOWN_ROOT/ESTOP"
run_plugin 7000600
assert_eq "$(nudges)" "0" "estop: no nudge while the town is frozen"
rm -f "$GT_TOWN_ROOT/ESTOP"
touch "$GT_TOWN_ROOT/ESTOP.om"
run_plugin 7000700
assert_eq "$(nudges)" "1" "estop: a rig estop does not stop the other rigs"

# --- Case 11: a closed local tier and an uncapped overflow -----------------
setup_case
cat > "$CASE_DIR/settings.json" <<'JSON'
{"type":"town-settings","polecat_pool":{"local_agent":"local-coder-polecat","max_local":0,"overflow_agent":"deepseek-flash"}}
JSON
export GT_SEAT_REFILL_SONNET_MAX=0
write_polecats "$LIVE_NONE"
ready_bug gastown
run_plugin 8000000
run_plugin 8000600
assert_eq "$EXIT" "0" "uncapped: exits 0"
assert_eq "$(nudges)" "0" \
  "uncapped: a closed local tier and an uncapped overflow mean no seat can be empty"
assert_contains "$TEST_STATE/stdout.log" "[plugin-result skipped]" "uncapped: skipped, not a failure"

# --- Case 12: the sonnet seat fires only on work that asks for it ----------
setup_case
write_polecats "$LIVE_NONE"
cat > "$TEST_STATE/ready/gastown.json" <<'JSON'
{"sources":[{"name":"gastown","issues":[
  {"id":"gt-flow","title":"Ordinary work","status":"open","priority":1,"issue_type":"task"}
]}],"summary":{},"town_root":"/town"}
JSON
run_plugin 9000000
run_plugin 9000600
assert_eq "$(nudges_of 'seat sonnet')" "0" \
  "sonnet seat: an empty sonnet seat with no needs-sonnet work is silent"
cat > "$TEST_STATE/ready/gastown.json" <<'JSON'
{"sources":[{"name":"gastown","issues":[
  {"id":"gt-hard","title":"Design work","status":"open","priority":1,"issue_type":"task","labels":["needs-sonnet"]}
]}],"summary":{},"town_root":"/town"}
JSON
run_plugin 9001200
assert_eq "$(nudges_of 'seat sonnet')" "1" \
  "sonnet seat: a needs-sonnet bead fires the sonnet seat"

# --- Case 13: a live claim holds its seat ---------------------------------
setup_case
write_polecats "$LIVE_NONE"
ready_bug gastown
mkdir -p "$GT_TOWN_ROOT/.runtime/polecat-pool-claims"
# The test's own PID is alive, so this claim reads as an in-flight spawn.
cat > "$GT_TOWN_ROOT/.runtime/polecat-pool-claims/claim1.json" <<JSON
{"id":"claim1","pid":$$,"agent":"local-coder-polecat","bead":"gt-live","created_at":"$(date -u +%Y-%m-%dT%H:%M:%SZ)"}
JSON
NOW_REAL=$(date +%s)
run_plugin "$NOW_REAL"
run_plugin "$((NOW_REAL + 300))"
assert_eq "$(nudges_of 'seat local')" "0" \
  "claim: a seat held by a live in-flight spawn is not empty"
assert_eq "$(nudges_of 'seat overflow')" "1" \
  "claim: the seat with no claim still fires"

# --- Case 14: an unreadable seat count fails loudly, never as zero --------
setup_case
write_polecats "$LIVE_NONE"
ready_bug gastown
touch "$TEST_STATE/polecat_list_fails"
run_plugin 11000000
assert_eq "$EXIT" "1" "unreadable seats: exits nonzero"
assert_eq "$(nudges)" "0" "unreadable seats: no nudge about a seat that may be occupied"
assert_contains "$TEST_STATE/stderr.log" "occupancy is unknown" \
  "unreadable seats: the failure says what was unknown"

# --- Case 15: no pool configured -----------------------------------------
setup_case
cat > "$CASE_DIR/settings.json" <<'JSON'
{"type":"town-settings","role_agents":{"polecat":"deepseek-flash"}}
JSON
write_polecats "$LIVE_NONE"
ready_bug gastown
run_plugin 12000000
run_plugin 12000600
assert_eq "$EXIT" "0" "no pool: exits 0"
assert_eq "$(nudges)" "0" "no pool: no seats to watch"
assert_contains "$TEST_STATE/stdout.log" "no polecat_pool" "no pool: the run says why it was skipped"

# --- Case 16: a nudge that fails is not recorded as sent ------------------
setup_case
write_polecats "$LIVE_NONE"
ready_bug gastown
run_plugin 13000000
touch "$TEST_STATE/nudge_fails"
run_plugin 13000300
assert_eq "$EXIT" "1" "failed nudge: exits nonzero so the daemon hands it to a dog"
assert_eq "$(jq -r '.episodes.local.last_nudge' "$GT_SEAT_REFILL_STATE")" "0" \
  "failed nudge: the episode does not record a nudge that never landed"
rm -f "$TEST_STATE/nudge_fails"
run_plugin 13000320
assert_eq "$(jq -r '.episodes.local.last_nudge' "$GT_SEAT_REFILL_STATE")" "13000320" \
  "failed nudge: the next run retries immediately instead of after 15m"

# --- Case 17: the nudge bound outlasts gt nudge's own wait-idle budget -----
# gt nudge in wait-idle mode polls for idle for 15s, queues, then watches for
# idle for up to 60s (internal/cmd/nudge.go waitIdleTimeout,
# idleWatcherTimeout). A bound at or under 75s kills a nudge that is working
# normally against a busy mayor and reports it as lost (gt-hen4o).
setup_case
write_polecats "$LIVE_NONE"
ready_bug gastown
: > "$TEST_STATE/timeout.log"
run_plugin 14000000
run_plugin 14000300
assert_eq "$(nudges)" "1" "nudge bound: the nudge ran"
bound=$(awk -F'|' '$2 == "gt" { print $1 }' "$TEST_STATE/timeout.log" | tail -1)
bound_seconds=${bound%s}
if [[ "$bound_seconds" =~ ^[0-9]+$ ]] && [ "$bound_seconds" -gt 75 ]; then
  record_pass "nudge bound: ${bound} exceeds gt nudge's 15s+60s wait-idle budget"
else
  record_fail "nudge bound: ${bound:-<none>} does not exceed gt nudge's 15s+60s wait-idle budget"
fi

# --- Case 18: a nudge killed by the bound says so, not "was not reported" ---
# Past its whole budget gt nudge is wedged, so this is still a failure — but
# wait-idle queues before it watches, so the message must not claim the seat
# went unreported.
setup_case
write_polecats "$LIVE_NONE"
ready_bug gastown
run_plugin 15000000
touch "$TEST_STATE/timeout_expires"
run_plugin 15000300
assert_eq "$EXIT" "1" "nudge bound fired: still exits nonzero"
assert_contains "$TEST_STATE/stderr.log" "timed out" "nudge bound fired: the failure names the timeout"
assert_not_contains "$TEST_STATE/stderr.log" "was not reported" \
  "nudge bound fired: does not claim the seat went unreported"
assert_eq "$(jq -r '.episodes.local.last_nudge' "$GT_SEAT_REFILL_STATE")" "0" \
  "nudge bound fired: unconfirmed delivery is not recorded as sent"
rm -f "$TEST_STATE/timeout_expires"

echo ""
if [ "$FAIL" -gt 0 ]; then
  printf '=== %d passed, %d FAILED ===\n' "$PASS" "$FAIL"
  exit 1
fi
printf '=== %d passed, 0 failed ===\n' "$PASS"
