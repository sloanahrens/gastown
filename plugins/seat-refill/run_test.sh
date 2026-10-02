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
printf '%s|%s|%s\n' "$1" "$2" "${3:-}" >> "$TEST_STATE/timeout.log"
if [ -f "$TEST_STATE/timeout_expires" ]; then
  want=$(cat "$TEST_STATE/timeout_expires")
  case "$*" in
    *"$want"*) exit 124 ;;
  esac
fi
shift
exec "$@"
SH
  chmod +x "$bin_dir/timeout"
}

# --- Fake town ------------------------------------------------------------
# The gt calls the plugin makes, each backed by a fixture file so a case is
# pure data:
#   polecat list --all --json   $TEST_STATE/polecats.json   (absent -> [])
#   rig list --json             $TEST_STATE/rigs.json
#   ready --rig <rig> --json    $TEST_STATE/ready/<rig>.json
#   nudge <target> <message>    appends to $TEST_STATE/nudge.log
#   escalate <desc> ...         appends to $TEST_STATE/escalate.log
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
  mayor)
    if [ "${2:-}" = "status" ]; then
      if [ -f "$TEST_STATE/mayor_down" ]; then echo false; else echo true; fi
      exit 0
    fi
    exit 1
    ;;
  sling)
    shift
    printf 'SLING|%s\n' "$*" >> "$TEST_STATE/sling.log"
    if [ -f "$TEST_STATE/sling_fails" ]; then
      echo "Error: sling refused: merge queue over max_ready_for_dispatch" >&2
      exit 1
    fi
    if [ -f "$TEST_STATE/sling_errors" ]; then
      echo "Error: dolt connection refused" >&2
      exit 1
    fi
    exit 0
    ;;
  escalate)
    shift
    printf 'ESCALATE|%s\n' "$*" >> "$TEST_STATE/escalate.log"
    if [ -f "$TEST_STATE/escalate_fails" ]; then
      echo "gt: could not reach the town store" >&2
      exit 1
    fi
    exit 0
    ;;
  nudge)
    shift
    target="${1:-}"
    shift || true
    printf 'NUDGE|%s|%s\n' "$target" "$*" >> "$TEST_STATE/nudge.log"
    if [ -f "$TEST_STATE/nudge_notfound" ]; then
      echo "gt: command not found" >&2
      exit 1
    fi
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

  unset GT_SEAT_REFILL_PRO_MAX GT_SEAT_REFILL_PRO_AGENT GT_SEAT_REFILL_PRO_LABEL
  unset GT_SEAT_REFILL_EMPTY_SECONDS GT_SEAT_REFILL_NUDGE_SECONDS GT_SEAT_REFILL_MAYOR
  unset GT_SEAT_REFILL_DRY_RUN GT_SEAT_REFILL_DISPATCH_EMPTY_SECONDS
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
  : > "$TEST_STATE/sling.log"
  : > "$TEST_STATE/escalate.log"
  : > "$TEST_STATE/unexpected.log"

  write_fake_gt "$TEST_STATE/bin"
  write_fake_timeout "$TEST_STATE/bin"
  export PATH="$TEST_STATE/bin:$ORIGINAL_PATH"
  export TEST_STATE
  export GT_TOWN_ROOT="$CASE_DIR"
  export GT_SEAT_REFILL_MODE=nudge
  export GT_SEAT_REFILL_CONFIG="$CASE_DIR/settings.json"
  export GT_SEAT_REFILL_STATE="$CASE_DIR/state.json"
}

# run_plugin <now-epoch>: runs the plugin with a pinned clock, leaving stdout in
# $TEST_STATE/stdout.log and stderr in $TEST_STATE/stderr.log, and EXIT set.
run_plugin() {
  set +e
  GT_SEAT_REFILL_NOW="$1" bash "$SCRIPT" >"$TEST_STATE/stdout.log" 2>"$TEST_STATE/stderr.log"
  EXIT=$?
  set -e
}

slings() { grep -c 'SLING' "$TEST_STATE/sling.log" || true; }
escalations() { grep -c 'ESCALATE' "$TEST_STATE/escalate.log" || true; }
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
touch "$CASE_DIR/seat-refill.hold"
run_plugin 6000600
assert_eq "$(nudges)" "0" "hold flag: no nudge while the flag is set"
assert_contains "$TEST_STATE/stdout.log" "hold flag" "hold flag: the run says why it was skipped"
rm -f "$CASE_DIR/seat-refill.hold"
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
export GT_SEAT_REFILL_PRO_MAX=0
write_polecats "$LIVE_NONE"
ready_bug gastown
run_plugin 8000000
run_plugin 8000600
assert_eq "$EXIT" "0" "uncapped: exits 0"
assert_eq "$(nudges)" "0" \
  "uncapped: a closed local tier and an uncapped overflow mean no seat can be empty"
assert_contains "$TEST_STATE/stdout.log" "[plugin-result skipped]" "uncapped: skipped, not a failure"

# --- Case 12: the pro seat fires only on work that asks for it ------------
setup_case
write_polecats "$LIVE_NONE"
cat > "$TEST_STATE/ready/gastown.json" <<'JSON'
{"sources":[{"name":"gastown","issues":[
  {"id":"gt-flow","title":"Ordinary work","status":"open","priority":1,"issue_type":"task"}
]}],"summary":{},"town_root":"/town"}
JSON
run_plugin 9000000
run_plugin 9000600
assert_eq "$(nudges_of 'seat pro')" "0" \
  "pro seat: an empty pro seat with no needs-pro work is silent"
cat > "$TEST_STATE/ready/gastown.json" <<'JSON'
{"sources":[{"name":"gastown","issues":[
  {"id":"gt-hard","title":"Design work","status":"open","priority":1,"issue_type":"task","labels":["needs-pro"]}
]}],"summary":{},"town_root":"/town"}
JSON
run_plugin 9001200
assert_eq "$(nudges_of 'seat pro')" "1" \
  "pro seat: a needs-pro bead fires the pro seat"

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
assert_eq "$EXIT" "1" "failed nudge: exits nonzero so the daemon escalates it"
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
printf nudge > "$TEST_STATE/timeout_expires"
run_plugin 15000300
assert_eq "$EXIT" "1" "nudge bound fired: still exits nonzero"
assert_contains "$TEST_STATE/stderr.log" "timed out" "nudge bound fired: the failure names the timeout"
assert_not_contains "$TEST_STATE/stderr.log" "was not reported" \
  "nudge bound fired: does not claim the seat went unreported"
assert_eq "$(jq -r '.episodes.local.last_nudge' "$GT_SEAT_REFILL_STATE")" "0" \
  "nudge bound fired: unconfirmed delivery is not recorded as sent"
rm -f "$TEST_STATE/timeout_expires"

# --- Case 19: the operator label is not fillable work ---------------------
# A bead the operator reserved is theirs to do by hand. Asking the mayor to
# sling it is what put gt-nj23.9 back on a polecat after the operator had taken
# it (gt-21pl0), so the label keeps it off the candidate list entirely — however
# the label was typed, since a label is written by hand.
setup_case
write_polecats "$LIVE_NONE"
cat > "$TEST_STATE/ready/gastown.json" <<'JSON'
{"sources":[{"name":"gastown","issues":[
  {"id":"gt-op","title":"Hand-run audit","status":"open","priority":1,"issue_type":"task","labels":["Operator"]}
]}],"summary":{},"town_root":"/town"}
JSON
run_plugin 16000000
run_plugin 16000300
assert_eq "$(nudges)" "0" \
  "operator label: an empty seat with only operator work stays silent"

# An ordinary label is not the reservation: the same seat fires on it.
cat > "$TEST_STATE/ready/gastown.json" <<'JSON'
{"sources":[{"name":"gastown","issues":[
  {"id":"gt-plain","title":"Ordinary work","status":"open","priority":1,"issue_type":"task","labels":["run-blocker"]}
]}],"summary":{},"town_root":"/town"}
JSON
run_plugin 16000600
assert_eq "$(nudges)" "1" \
  "operator label: another label does not reserve the bead"

# === Direct dispatch (gt-qvs0b) and candidate selection (gt-inu1y) ==========
# Mode sling is the default: no mayor, the plugin slings itself.
direct_case() { setup_case; unset GT_SEAT_REFILL_MODE; export GT_SEAT_REFILL_PRO_MAX=0; }

# --- Case 20: an empty seat is filled at once, mayor never contacted -------
direct_case
touch "$TEST_STATE/mayor_down"
write_polecats "$LIVE_OVERFLOW_ONLY"
ready_bug gastown
run_plugin 20000000
assert_eq "$EXIT" "0" "direct: exits 0 with the mayor down"
assert_eq "$(slings)" "1" "direct: one sling for the one empty seat"
assert_contains "$TEST_STATE/sling.log" "SLING|gt-bug1 gastown --agent local-coder-polecat"   "direct: slings the bead into the rig on the seat's agent"
assert_eq "$(nudges)" "0" "direct: no nudge to the mayor"
assert_eq "$(escalations)" "0" "direct: an ordinary seat's dispatch files no record"

# --- Case 21: both seats empty take distinct beads, best first -------------
direct_case
write_polecats "$LIVE_NONE"
cat > "$TEST_STATE/ready/gastown.json" <<'JSON'
{"sources":[{"name":"gastown","issues":[
  {"id":"gt-b","title":"B","status":"open","priority":2,"issue_type":"task"},
  {"id":"gt-a","title":"A","status":"open","priority":1,"issue_type":"bug"},
  {"id":"gt-c","title":"C","status":"open","priority":2,"issue_type":"task"}
]}],"summary":{},"town_root":"/town"}
JSON
run_plugin 21000000
assert_eq "$(slings)" "2" "two seats: two slings"
assert_contains "$TEST_STATE/sling.log" "SLING|gt-a gastown --agent local-coder-polecat" "two seats: P1 to local"
assert_contains "$TEST_STATE/sling.log" "SLING|gt-b gastown --agent deepseek-flash" "two seats: next to overflow"

# --- Case 22: hold, estop, parked rig stop dispatch ------------------------
direct_case
write_polecats "$LIVE_NONE"
ready_bug gastown
touch "$CASE_DIR/seat-refill.hold"
run_plugin 22000000
assert_eq "$(slings)" "0" "direct: hold file blocks slinging"
rm -f "$CASE_DIR/seat-refill.hold"
touch "$CASE_DIR/ESTOP"
run_plugin 22000001
assert_eq "$(slings)" "0" "direct: town estop blocks slinging"
rm -f "$CASE_DIR/ESTOP"
touch "$CASE_DIR/ESTOP.gastown"
run_plugin 22000002
assert_eq "$(slings)" "0" "direct: rig estop blocks that rig"
rm -f "$CASE_DIR/ESTOP.gastown"
ready_bug beads
rm -f "$TEST_STATE/ready/gastown.json"
run_plugin 22000003
assert_eq "$(slings)" "0" "direct: parked rig is not a target"

# --- Case 23: candidate selection skips landed, in-flight, claimed ---------
direct_case
write_polecats '[{"rig":"gastown","name":"p","agent":"x","session_running":false,"issue":"gt-held"}]'
mkdir -p "$GT_TOWN_ROOT/.runtime/polecat-pool-claims"
cat > "$GT_TOWN_ROOT/.runtime/polecat-pool-claims/c.json" <<JSON
{"id":"c","pid":$$,"agent":"x","bead":"gt-claimed","created_at":"$(date -u +%Y-%m-%dT%H:%M:%SZ)"}
JSON
cat > "$TEST_STATE/ready/gastown.json" <<'JSON'
{"sources":[{"name":"gastown","issues":[
  {"id":"gt-landing","title":"x","status":"open","priority":0,"issue_type":"task","labels":["gt:ready-to-land"]},
  {"id":"gt-human","title":"x","status":"open","priority":0,"issue_type":"task","labels":["gt:needs-human"]},
  {"id":"gt-wip","title":"x","status":"in_progress","priority":0,"issue_type":"task"},
  {"id":"gt-crew","title":"x","status":"open","priority":0,"issue_type":"task","assignee":"gastown/crew/sloan"},
  {"id":"gt-mol","title":"x","status":"open","priority":0,"issue_type":"molecule"},
  {"id":"gt-agent","title":"x","status":"open","priority":0,"issue_type":"agent"},
  {"id":"gt-epic","title":"x","status":"open","priority":0,"issue_type":"epic"},
  {"id":"gt-held","title":"x","status":"open","priority":0,"issue_type":"task"},
  {"id":"gt-claimed","title":"x","status":"open","priority":0,"issue_type":"task"},
  {"id":"gt-ok","title":"fine","status":"open","priority":2,"issue_type":"task"}
]}],"summary":{},"town_root":"/town"}
JSON
run_plugin "$(date +%s)"
assert_eq "$(slings)" "1" "selection: only the one eligible bead is slung"
assert_not_contains "$TEST_STATE/sling.log" "gt-landing" "selection: gt:ready-to-land skipped"
for b in gt-human gt-wip gt-crew gt-mol gt-agent gt-epic gt-held gt-claimed; do
  assert_not_contains "$TEST_STATE/sling.log" "$b" "selection: $b skipped"
done
assert_contains "$TEST_STATE/sling.log" "SLING|gt-ok" "selection: gt-ok slung"

# --- Case 24: dry run decides but touches nothing --------------------------
direct_case
write_polecats "$LIVE_NONE"
ready_bug gastown
GT_SEAT_REFILL_DRY_RUN=1 run_plugin 24000000
assert_eq "$(slings)" "0" "dry run: no sling"
assert_contains "$TEST_STATE/stdout.log" "DRY-RUN: would sling gt-bug1" "dry run: says what it would do"
assert_contains "$TEST_STATE/stdout.log" "would dispatch" "dry run: summary says would dispatch"
assert_not_contains "$TEST_STATE/stdout.log" "dispatched 1" "dry run: never says dispatched"
assert_eq "$([ -e "$GT_SEAT_REFILL_STATE" ] && echo yes || echo no)" "no" "dry run: no state written"

# --- Case 25: the pro seat takes only needs-pro work, others skip it ------
direct_case
unset GT_SEAT_REFILL_PRO_MAX
write_polecats "$LIVE_BOTH"
cat > "$TEST_STATE/ready/gastown.json" <<'JSON'
{"sources":[{"name":"gastown","issues":[
  {"id":"gt-hard","title":"x","status":"open","priority":1,"issue_type":"task","labels":["needs-pro"]}
]}],"summary":{},"town_root":"/town"}
JSON
run_plugin 25000000
assert_contains "$TEST_STATE/sling.log" "SLING|gt-hard gastown --agent deepseek-pro" "pro: needs-pro bead to the pro seat"
assert_eq "$(escalations)" "1" "pro: the dispatch files exactly one record"
assert_contains "$TEST_STATE/escalate.log" "seat-refill: pro seat dispatched gt-hard" "pro: the record names the bead"
assert_contains "$TEST_STATE/escalate.log" "--severity low" "pro: the record is low severity"
assert_contains "$TEST_STATE/escalate.log" "--fingerprint seat-refill:pro:gt-hard:25000000" \
  "pro: the record is keyed per bead and dispatch time"
direct_case
write_polecats "$LIVE_NONE"
cat > "$TEST_STATE/ready/gastown.json" <<'JSON'
{"sources":[{"name":"gastown","issues":[
  {"id":"gt-hard","title":"x","status":"open","priority":1,"issue_type":"task","labels":["needs-pro"]}
]}],"summary":{},"town_root":"/town"}
JSON
run_plugin 25000001
assert_eq "$(slings)" "0" "pro: local/overflow seats leave needs-pro work alone"
assert_eq "$(escalations)" "0" "pro: nothing dispatched, so no record"

# --- Case 26: a refused sling is logged, not escalated ---------------------
direct_case
write_polecats "$LIVE_NONE"
ready_bug gastown
touch "$TEST_STATE/sling_fails"
run_plugin 26000000
assert_eq "$EXIT" "0" "refused sling: exits 0"
assert_contains "$TEST_STATE/stdout.log" "refused: merge queue" "refused sling: logged with its reason"

# --- Case 27: nudge mode with the mayor down skips instead of failing ------
setup_case
touch "$TEST_STATE/mayor_down"
write_polecats "$LIVE_NONE"
ready_bug gastown
run_plugin 27000000
run_plugin 27000300
assert_eq "$EXIT" "0" "mayor down (nudge mode): not a failure"
assert_contains "$TEST_STATE/stdout.log" "not running" "mayor down (nudge mode): says why"

# --- Case 28: a dead or expired claim does not strand its bead -------------
direct_case
write_polecats "$LIVE_NONE"
ready_bug gastown
mkdir -p "$GT_TOWN_ROOT/.runtime/polecat-pool-claims"
# pid 999999 is not running: the claim is dead, so gt-bug1 is not held.
cat > "$GT_TOWN_ROOT/.runtime/polecat-pool-claims/dead.json" <<JSON
{"id":"dead","pid":999999,"agent":"x","bead":"gt-bug1","created_at":"$(date -u +%Y-%m-%dT%H:%M:%SZ)"}
JSON
# live pid but created long ago: expired.
cat > "$GT_TOWN_ROOT/.runtime/polecat-pool-claims/old.json" <<JSON
{"id":"old","pid":$$,"agent":"x","bead":"gt-bug1","created_at":"2020-01-01T00:00:00Z"}
JSON
run_plugin "$(date +%s)"
assert_contains "$TEST_STATE/sling.log" "SLING|gt-bug1" "stale claim: dead and expired claims do not hold the bead"

# --- Case 29: every sling erroring exits nonzero and names the bead --------
direct_case
write_polecats "$LIVE_NONE"
ready_bug gastown
touch "$TEST_STATE/sling_errors"
run_plugin 29000000
assert_eq "$EXIT" "1" "all slings fail: exits nonzero so the daemon escalates"
assert_contains "$TEST_STATE/stderr.log" "gt-bug1" "all slings fail: names the bead"
assert_contains "$TEST_STATE/stderr.log" "dolt connection refused" "all slings fail: names why"
assert_not_contains "$TEST_STATE/stdout.log" "[plugin-result skipped]" "all slings fail: not recorded as skipped"

# --- Case 30: a sling timeout is an error, not a refusal -------------------
direct_case
write_polecats "$LIVE_NONE"
ready_bug gastown
printf 'sling' > "$TEST_STATE/timeout_expires"
run_plugin 30000000
assert_eq "$EXIT" "1" "sling timeout: exits nonzero"
assert_contains "$TEST_STATE/stderr.log" "timed out" "sling timeout: named as a timeout"

# --- Case 31: only refusals is quiet, and says so --------------------------
direct_case
write_polecats "$LIVE_NONE"
ready_bug gastown
touch "$TEST_STATE/sling_fails"
run_plugin 31000000
assert_eq "$EXIT" "0" "all refused: exits 0"
assert_contains "$TEST_STATE/stdout.log" "refusal(s)" "all refused: receipt counts the refusals"

# --- Case 32: "command not found" is not a mayor-down skip -----------------
setup_case
write_polecats "$LIVE_NONE"
ready_bug gastown
touch "$TEST_STATE/nudge_notfound"
run_plugin 32000000
run_plugin 32000300
assert_eq "$EXIT" "1" "nudge error text: 'command not found' fails, it is not mayor-down"

# --- Case 33: an unknown MODE is rejected ----------------------------------
setup_case
write_polecats "$LIVE_NONE"
GT_SEAT_REFILL_MODE=bogus run_plugin 33000000
assert_eq "$EXIT" "1" "bad mode: exits nonzero"
assert_contains "$TEST_STATE/stderr.log" "mode must be sling or nudge" "bad mode: names the knob"

# --- Case 34: needs-pro label matches case-insensitively -------------------
direct_case
unset GT_SEAT_REFILL_PRO_MAX
write_polecats "$LIVE_BOTH"
cat > "$TEST_STATE/ready/gastown.json" <<'JSON'
{"sources":[{"name":"gastown","issues":[
  {"id":"gt-hard","title":"x","status":"open","priority":1,"issue_type":"task","labels":["Needs-Pro"]}
]}],"summary":{},"town_root":"/town"}
JSON
run_plugin 34000000
assert_contains "$TEST_STATE/sling.log" "SLING|gt-hard gastown --agent deepseek-pro" "pro label: case-insensitive"
assert_eq "$(escalations)" "1" "pro label: the dispatch is recorded too"

# --- Case 35: a wedged pool read is named ----------------------------------
direct_case
write_polecats "$LIVE_NONE"
printf 'polecat list' > "$TEST_STATE/timeout_expires"
run_plugin 35000000
assert_eq "$EXIT" "1" "pool wedge: exits nonzero"
assert_contains "$TEST_STATE/stderr.log" "gt polecat list --all --json timed out" "pool wedge: named (gt-d6rse)"

# --- Case 36: a record that cannot be written never fails the dispatch -----
direct_case
unset GT_SEAT_REFILL_PRO_MAX
write_polecats "$LIVE_BOTH"
cat > "$TEST_STATE/ready/gastown.json" <<'JSON'
{"sources":[{"name":"gastown","issues":[
  {"id":"gt-hard","title":"x","status":"open","priority":1,"issue_type":"task","labels":["needs-pro"]}
]}],"summary":{},"town_root":"/town"}
JSON
touch "$TEST_STATE/escalate_fails"
run_plugin 36000000
assert_eq "$EXIT" "0" "pro record failure: the run still exits 0"
assert_contains "$TEST_STATE/sling.log" "SLING|gt-hard gastown --agent deepseek-pro" \
  "pro record failure: the bead is already slung and stays slung"
assert_contains "$TEST_STATE/stdout.log" "could not record pro dispatch of gt-hard" \
  "pro record failure: named as a warning"

# === Dispatch policy from the town settings (gt-y3pgh.12) ==================
# The policy keys live in polecat_pool, beside the seats; a GT_SEAT_REFILL_*
# variable still overrides both the file and the default, for run_test.sh.

# write_pool <json>: rewrites the case's settings file with this polecat_pool.
write_pool() {
  printf '{"type":"town-settings","polecat_pool":%s}\n' "$1" > "$GT_SEAT_REFILL_CONFIG"
}

# --- Case 37: max_priority in the file decides what is dispatched ----------
# The ceiling is the knob an operator raises to reach a deeper backlog, and it
# had to be an env var in the daemon config's env map before (gt-y3pgh.12).
setup_case
write_pool '{"local_agent":"local-coder-polecat","max_local":1,"overflow_agent":"deepseek-flash","max_overflow":1,"max_priority":0}'
write_polecats "$LIVE_NONE"
ready_bug gastown   # a P1 bug: above the ceiling in the file
run_plugin 37000000
run_plugin 37000300
assert_eq "$(nudges)" "0" "config ceiling: a P1 bead is not dispatched at P0"
assert_contains "$TEST_STATE/stdout.log" "0 candidate(s)" "config ceiling: the seat sees no candidate"

write_pool '{"local_agent":"local-coder-polecat","max_local":1,"overflow_agent":"deepseek-flash","max_overflow":1,"max_priority":1}'
run_plugin 37000600
assert_eq "$(nudges)" "1" "config ceiling: raising it to P1 in the file reaches the same bead"

# --- Case 38: an env var still overrides the file -------------------------
# The case fixtures drive the policy through GT_SEAT_REFILL_*; the override must
# win over the file rather than be ignored.
setup_case
write_pool '{"local_agent":"local-coder-polecat","max_local":1,"overflow_agent":"deepseek-flash","max_overflow":1,"max_priority":0}'
write_polecats "$LIVE_NONE"
ready_bug gastown
GT_SEAT_REFILL_MAX_PRIORITY=2 run_plugin 38000000
GT_SEAT_REFILL_MAX_PRIORITY=2 run_plugin 38000300
assert_eq "$(nudges)" "1" "env override: GT_SEAT_REFILL_MAX_PRIORITY beats the file's 0"

# --- Case 39: the pro seat's cap, agent and label come from the file -------
direct_case
unset GT_SEAT_REFILL_PRO_MAX
write_pool '{"local_agent":"local-coder-polecat","max_local":1,"overflow_agent":"deepseek-flash","max_overflow":1,"pro_agent":"deepseek-reasoner","pro_label":"hard"}'
write_polecats "$LIVE_BOTH"
cat > "$TEST_STATE/ready/gastown.json" <<'JSON'
{"sources":[{"name":"gastown","issues":[
  {"id":"gt-hard","title":"x","status":"open","priority":1,"issue_type":"task","labels":["hard"]}
]}],"summary":{},"town_root":"/town"}
JSON
run_plugin 39000000
assert_contains "$TEST_STATE/sling.log" "SLING|gt-hard gastown --agent deepseek-reasoner" \
  "pro config: the file's pro_agent takes the bead the file's pro_label marks"

# --- Case 40: pro_max 0 in the file drops the pro seat --------------------
direct_case
unset GT_SEAT_REFILL_PRO_MAX
write_pool '{"local_agent":"local-coder-polecat","max_local":1,"overflow_agent":"deepseek-flash","max_overflow":1,"pro_max":0}'
write_polecats "$LIVE_NONE"
ready_bug gastown
run_plugin 40000000
assert_eq "$(slings)" "1" "pro_max 0: the local seat still fills"
assert_not_contains "$TEST_STATE/sling.log" "--agent deepseek-pro" "pro_max 0: nothing runs on the pro seat"

# --- Case 41: mode in the file decides sling or nudge ---------------------
setup_case
unset GT_SEAT_REFILL_MODE
write_polecats "$LIVE_NONE"
ready_bug gastown
write_pool '{"local_agent":"local-coder-polecat","max_local":1,"overflow_agent":"deepseek-flash","max_overflow":1,"mode":"nudge"}'
run_plugin 41000000
run_plugin 41000300
assert_eq "$(nudges)" "1" "config mode: nudge in the file asks the mayor"
assert_eq "$(slings)" "0" "config mode: nudge in the file slings nothing"

write_pool '{"local_agent":"local-coder-polecat","max_local":1,"overflow_agent":"deepseek-flash","max_overflow":1,"mode":"sling"}'
run_plugin 41000600
assert_eq "$(slings)" "1" "config mode: sling in the file dispatches the bead"

echo ""
if [ "$FAIL" -gt 0 ]; then
  printf '=== %d passed, %d FAILED ===\n' "$PASS" "$FAIL"
  exit 1
fi
printf '=== %d passed, 0 failed ===\n' "$PASS"
