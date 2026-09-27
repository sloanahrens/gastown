#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
SCRIPT="$ROOT_DIR/plugins/stuck-work-dog/run.sh"
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

assert_file_empty() {
  local file="$1"
  local label="$2"
  if [ ! -s "$file" ]; then
    record_pass "$label"
  else
    record_fail "$label"
    printf '  unexpected contents of %s:\n' "$file"
    sed 's/^/    /' "$file"
  fi
}

assert_file_contains() {
  local file="$1"
  local needle="$2"
  local label="$3"
  if [ -f "$file" ] && grep -Fq -- "$needle" "$file"; then
    record_pass "$label"
  else
    record_fail "$label"
    printf '  expected %q in %s\n' "$needle" "$file"
    sed 's/^/    /' "$file" 2>/dev/null || printf '  (file does not exist)\n'
  fi
}

assert_file_not_contains() {
  local file="$1"
  local needle="$2"
  local label="$3"
  if ! grep -Fq -- "$needle" "$file" 2>/dev/null; then
    record_pass "$label"
  else
    record_fail "$label"
    printf '  did not expect %q in %s\n' "$needle" "$file"
    sed 's/^/    /' "$file" 2>/dev/null || true
  fi
}

assert_line_count() {
  local file="$1"
  local expected="$2"
  local label="$3"
  local actual=0

  if [ -f "$file" ]; then
    actual=$(wc -l < "$file" | tr -d ' ')
  fi
  if [ "$actual" = "$expected" ]; then
    record_pass "$label"
  else
    record_fail "$label"
    printf '  expected %s lines in %s, got %s\n' "$expected" "$file" "$actual"
    sed 's/^/    /' "$file" 2>/dev/null || true
  fi
}

# ISO8601 UTC timestamp `seconds_ago` seconds in the past. Portable across
# GNU date (-d @epoch) and BSD/macOS date (-r epoch).
iso_ago() {
  local seconds_ago="$1"
  local epoch=$(( $(date +%s) - seconds_ago ))
  date -u -d "@$epoch" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null \
    || date -u -r "$epoch" +%Y-%m-%dT%H:%M:%SZ
}

write_fake_commands() {
  local bin_dir="$1"

  cat > "$bin_dir/gt" <<'SH'
#!/usr/bin/env bash
set -euo pipefail

case "${1:-}" in
  town)
    if [ "${2:-}" = "root" ]; then
      printf '%s\n' "$GT_TOWN_ROOT"
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
  mq)
    if [ "${2:-}" = "list" ]; then
      rig="${3:-}"
      printf '%s\n' "$rig" >> "$TEST_STATE/mq_calls.log"
      file="$TEST_STATE/mq/$rig.json"
      if [ -f "$file" ]; then
        cat "$file"
      else
        echo '[]'
      fi
      exit 0
    fi
    exit 1
    ;;
  slot)
    if [ "${2:-}" = "status" ]; then
      if [ -f "$TEST_STATE/slot_status_fail" ]; then
        printf 'gt: cannot read the container-gate lock directory\n' >&2
        exit 1
      fi
      file="$TEST_STATE/slot_status.json"
      if [ -f "$file" ]; then
        cat "$file"
      else
        echo '{"slots":[]}'
      fi
      exit 0
    fi
    exit 1
    ;;
  escalate)
    shift
    desc="${1:-}"
    shift || true
    fingerprint=""
    related=""
    while [ $# -gt 0 ]; do
      case "$1" in
        --fingerprint) fingerprint="${2:-}"; shift 2 ;;
        --related) related="${2:-}"; shift 2 ;;
        -s|--severity) shift 2 ;;
        --source) shift 2 ;;
        --reason) shift 2 ;;
        *) shift ;;
      esac
    done
    printf 'ESCALATE|%s|%s|%s\n' "$fingerprint" "$related" "$desc" >> "$TEST_STATE/escalate.log"
    if [ -f "$TEST_STATE/escalate_fail" ]; then
      printf 'escalate: dolt unreachable\n' >&2
      exit 1
    fi
    exit 0
    ;;
  plugin)
    if [ "${2:-}" = "record-run" ]; then
      shift 2
      plugin=""
      result=""
      while [ $# -gt 0 ]; do
        case "$1" in
          --plugin) plugin="${2:-}"; shift 2 ;;
          --result) result="${2:-}"; shift 2 ;;
          *) shift ;;
        esac
      done
      printf 'RECORD|%s|%s\n' "$plugin" "$result" >> "$TEST_STATE/record.log"
      exit 0
    fi
    exit 1
    ;;
  *)
    printf 'UNEXPECTED gt call: %s\n' "$*" >> "$TEST_STATE/unexpected.log"
    exit 1
    ;;
esac
SH
  chmod +x "$bin_dir/gt"

  cat > "$bin_dir/bd" <<'SH'
#!/usr/bin/env bash
set -euo pipefail

case "${1:-}" in
  show)
    id="${2:-}"
    file="$TEST_STATE/beads/$id.json"
    if [ -f "$file" ]; then
      printf '[%s]\n' "$(cat "$file")"
    else
      echo '[]'
    fi
    exit 0
    ;;
  list)
    # bd list --status=in_progress --json --limit=1, run from the rig
    # workdir: presence of .in_progress in cwd means "a bead is in_progress".
    if [ -f "./.in_progress" ]; then
      echo '[{"id":"marker"}]'
    else
      echo '[]'
    fi
    exit 0
    ;;
  *)
    printf 'UNEXPECTED bd call: %s\n' "$*" >> "$TEST_STATE/unexpected.log"
    exit 1
    ;;
esac
SH
  chmod +x "$bin_dir/bd"
}

setup_test() {
  local test_name="$1"
  local dir
  dir=$(mktemp -d "${TMPDIR:-/tmp}/stuck-work-dog-test.XXXXXX")
  CLEANUP_DIRS+=("$dir")

  export TEST_STATE="$dir/state"
  export GT_TOWN_ROOT="$dir/town"
  mkdir -p "$TEST_STATE/beads" "$TEST_STATE/mq" "$GT_TOWN_ROOT"

  local bin_dir="$dir/bin"
  mkdir -p "$bin_dir"
  write_fake_commands "$bin_dir"
  export PATH="$bin_dir:$ORIGINAL_PATH"
}

set_rigs() {
  local rigs_json="$1"
  printf '%s' "$rigs_json" > "$TEST_STATE/rigs.json"
}

set_mq() {
  local rig="$1" mq_json="$2"
  printf '%s' "$mq_json" > "$TEST_STATE/mq/$rig.json"
}

set_bead() {
  local id="$1" fields="$2"
  printf '%s' "$fields" > "$TEST_STATE/beads/$id.json"
}

mark_in_progress() {
  local rig="$1"
  touch "$GT_TOWN_ROOT/$rig/.in_progress"
}

# The slot-status reading 'gt slot status --json' returns: one in-flight batch
# marker for each rig named, exactly as internal/cmd/mq_batch.go writes it.
set_batch_markers() {
  local rigs=("$@") rig rows=()

  for rig in "${rigs[@]}"; do
    rows+=("{\"index\":1,\"held\":true,\"marker\":true,\"name\":\"mq-batch-$rig\",\"owner\":{\"role\":\"$rig/refinery-batch\",\"pid\":4242}}")
  done

  printf '{"held":false,"slots":[%s]}\n' "$(IFS=,; echo "${rows[*]}")" > "$TEST_STATE/slot_status.json"
}

fail_slot_status() {
  touch "$TEST_STATE/slot_status_fail"
}

run_plugin() {
  "$SCRIPT" > "$TEST_STATE/stdout.log" 2>"$TEST_STATE/stderr.log"
}

# --- Test 1: no operational rigs — skip cleanly, still records success ------
test_no_rigs() {
  setup_test "no_rigs"
  set_rigs '[]'

  run_plugin

  assert_file_contains "$TEST_STATE/stdout.log" "SKIP: no operational rigs found" "no_rigs: logs skip"
  assert_file_empty "$TEST_STATE/escalate.log" "no_rigs: no escalations"
  assert_file_contains "$TEST_STATE/record.log" "RECORD|stuck-work-dog|success" "no_rigs: records success"
}

# --- Test 2: rig with an empty merge queue — nothing happens ----------------
test_empty_queue() {
  setup_test "empty_queue"
  mkdir -p "$GT_TOWN_ROOT/gastown"
  set_rigs '[{"name":"gastown","status":"operational"}]'
  set_mq "gastown" '[]'

  run_plugin

  assert_file_empty "$TEST_STATE/escalate.log" "empty_queue: no escalations"
  assert_file_contains "$TEST_STATE/record.log" "RECORD|stuck-work-dog|success" "empty_queue: records success"
}

# --- Test 3: blocked MR on an old, open, unassigned dependency — escalates --
test_stale_unassigned_blocker() {
  setup_test "stale_unassigned_blocker"
  mkdir -p "$GT_TOWN_ROOT/gastown"
  set_rigs '[{"name":"gastown","status":"operational"}]'
  set_mq "gastown" '[
    {"id":"gt-mr-001","status":"open","created_at":"2026-01-01T00:00:00Z","blocked_by":["gt-blocker-1"]}
  ]'
  set_bead "gt-blocker-1" "{\"id\":\"gt-blocker-1\",\"status\":\"open\",\"assignee\":\"\",\"created_at\":\"$(iso_ago 10800)\"}"

  run_plugin

  assert_file_contains "$TEST_STATE/escalate.log" "stuck-work-dog:blocked-mr:gastown:gt-blocker-1" "stale_blocker: escalates with keyed fingerprint"
  assert_file_contains "$TEST_STATE/escalate.log" "ESCALATE|stuck-work-dog:blocked-mr:gastown:gt-blocker-1|gt-blocker-1|" "stale_blocker: --related is the blocker bead"
}

# --- Test 4: blocked MR, but blocker is assigned — no escalation -----------
test_assigned_blocker_no_escalation() {
  setup_test "assigned_blocker"
  mkdir -p "$GT_TOWN_ROOT/gastown"
  set_rigs '[{"name":"gastown","status":"operational"}]'
  set_mq "gastown" '[
    {"id":"gt-mr-001","status":"open","created_at":"2026-01-01T00:00:00Z","blocked_by":["gt-blocker-2"]}
  ]'
  set_bead "gt-blocker-2" "{\"id\":\"gt-blocker-2\",\"status\":\"open\",\"assignee\":\"someone\",\"created_at\":\"$(iso_ago 10800)\"}"

  run_plugin

  assert_file_not_contains "$TEST_STATE/escalate.log" "gt-blocker-2" "assigned_blocker: no escalation once assigned"
}

# --- Test 5: blocked MR, blocker too young — no escalation yet -------------
test_young_blocker_no_escalation() {
  setup_test "young_blocker"
  mkdir -p "$GT_TOWN_ROOT/gastown"
  set_rigs '[{"name":"gastown","status":"operational"}]'
  set_mq "gastown" '[
    {"id":"gt-mr-001","status":"open","created_at":"2026-01-01T00:00:00Z","blocked_by":["gt-blocker-3"]}
  ]'
  set_bead "gt-blocker-3" "{\"id\":\"gt-blocker-3\",\"status\":\"open\",\"assignee\":\"\",\"created_at\":\"$(iso_ago 60)\"}"

  run_plugin

  assert_file_not_contains "$TEST_STATE/escalate.log" "gt-blocker-3" "young_blocker: no escalation before threshold"
}

# --- Test 6: blocked_by references a bead that's already closed ------------
test_closed_blocker_no_escalation() {
  setup_test "closed_blocker"
  mkdir -p "$GT_TOWN_ROOT/gastown"
  set_rigs '[{"name":"gastown","status":"operational"}]'
  set_mq "gastown" '[
    {"id":"gt-mr-001","status":"open","created_at":"2026-01-01T00:00:00Z","blocked_by":["gt-blocker-4"]}
  ]'
  set_bead "gt-blocker-4" "{\"id\":\"gt-blocker-4\",\"status\":\"closed\",\"assignee\":\"\",\"created_at\":\"$(iso_ago 10800)\"}"

  run_plugin

  assert_file_not_contains "$TEST_STATE/escalate.log" "gt-blocker-4" "closed_blocker: no escalation once resolved"
}

# --- Test 7: queue stalled — entries, none in_progress, nobody working -----
test_queue_stall_escalates() {
  setup_test "queue_stall"
  mkdir -p "$GT_TOWN_ROOT/gastown"
  set_rigs '[{"name":"gastown","status":"operational"}]'
  set_mq "gastown" "[
    {\"id\":\"gt-mr-010\",\"status\":\"open\",\"created_at\":\"$(iso_ago 3600)\"},
    {\"id\":\"gt-mr-011\",\"status\":\"open\",\"created_at\":\"$(iso_ago 1900)\"}
  ]"

  run_plugin

  assert_file_contains "$TEST_STATE/escalate.log" "stuck-work-dog:queue-stall:gastown" "queue_stall: escalates with keyed fingerprint"
}

# --- Test 8: queue stalled but a polecat has in_progress work — skip -------
test_queue_stall_skipped_when_polecat_working() {
  setup_test "queue_stall_working"
  mkdir -p "$GT_TOWN_ROOT/gastown"
  set_rigs '[{"name":"gastown","status":"operational"}]'
  set_mq "gastown" "[
    {\"id\":\"gt-mr-020\",\"status\":\"open\",\"created_at\":\"$(iso_ago 3600)\"}
  ]"
  mark_in_progress "gastown"

  run_plugin

  assert_file_not_contains "$TEST_STATE/escalate.log" "queue-stall" "queue_stall_working: no escalation while a polecat works"
}

# --- Test 9: an in_progress MR means the queue is not stalled --------------
test_queue_not_stalled_when_mr_in_progress() {
  setup_test "queue_in_progress"
  mkdir -p "$GT_TOWN_ROOT/gastown"
  set_rigs '[{"name":"gastown","status":"operational"}]'
  set_mq "gastown" "[
    {\"id\":\"gt-mr-030\",\"status\":\"in_progress\",\"created_at\":\"$(iso_ago 3600)\"}
  ]"

  run_plugin

  assert_file_not_contains "$TEST_STATE/escalate.log" "queue-stall" "queue_in_progress: no escalation, an MR is progressing"
}

# --- Test 10: closed MRs don't count toward an active queue ----------------
test_closed_mrs_ignored() {
  setup_test "closed_mrs_ignored"
  mkdir -p "$GT_TOWN_ROOT/gastown"
  set_rigs '[{"name":"gastown","status":"operational"}]'
  set_mq "gastown" "[
    {\"id\":\"gt-mr-040\",\"status\":\"closed\",\"created_at\":\"$(iso_ago 999999)\"}
  ]"

  run_plugin

  assert_file_empty "$TEST_STATE/escalate.log" "closed_mrs_ignored: no escalations from a closed-only queue"
}

# --- Test 11: multiple rigs are each checked independently -----------------
test_multiple_rigs_checked() {
  setup_test "multiple_rigs"
  mkdir -p "$GT_TOWN_ROOT/gastown" "$GT_TOWN_ROOT/otherrig"
  set_rigs '[{"name":"gastown","status":"operational"},{"name":"otherrig","status":"operational"}]'
  set_mq "gastown" '[]'
  set_mq "otherrig" '[
    {"id":"gt-mr-050","status":"open","created_at":"2026-01-01T00:00:00Z","blocked_by":["gt-blocker-5"]}
  ]'
  set_bead "gt-blocker-5" "{\"id\":\"gt-blocker-5\",\"status\":\"open\",\"assignee\":\"\",\"created_at\":\"$(iso_ago 10800)\"}"

  run_plugin

  assert_file_contains "$TEST_STATE/mq_calls.log" "gastown" "multiple_rigs: checks gastown"
  assert_file_contains "$TEST_STATE/mq_calls.log" "otherrig" "multiple_rigs: checks otherrig"
  assert_file_contains "$TEST_STATE/escalate.log" "stuck-work-dog:blocked-mr:otherrig:gt-blocker-5" "multiple_rigs: escalates for the right rig"
}

# --- Test 12: a batch in flight means the queue is not stalled -------------
test_queue_stall_skipped_when_batch_in_flight() {
  setup_test "batch_in_flight"
  mkdir -p "$GT_TOWN_ROOT/gastown"
  set_rigs '[{"name":"gastown","status":"operational"}]'
  set_mq "gastown" "[
    {\"id\":\"gt-mr-070\",\"status\":\"open\",\"created_at\":\"$(iso_ago 3600)\"}
  ]"
  set_batch_markers "gastown"

  run_plugin

  assert_file_not_contains "$TEST_STATE/escalate.log" "queue-stall" "batch_in_flight: no escalation while the batch has not landed its MRs"
  assert_file_contains "$TEST_STATE/stdout.log" "gastown: 'gt mq batch run' in flight" "batch_in_flight: says why it stayed quiet"
}

# --- Test 13: another rig's batch does not silence this rig ----------------
test_queue_stall_fires_when_only_another_rig_has_a_batch() {
  setup_test "batch_other_rig"
  mkdir -p "$GT_TOWN_ROOT/gastown" "$GT_TOWN_ROOT/otherrig"
  set_rigs '[{"name":"gastown","status":"operational"}]'
  set_mq "gastown" "[
    {\"id\":\"gt-mr-080\",\"status\":\"open\",\"created_at\":\"$(iso_ago 3600)\"}
  ]"
  set_batch_markers "otherrig"

  run_plugin

  assert_file_contains "$TEST_STATE/escalate.log" "stuck-work-dog:queue-stall:gastown" "batch_other_rig: gastown's stall still escalates"
}

# --- Test 14: an unreadable slot status must not silence the detector ------
test_queue_stall_fires_when_slot_status_unreadable() {
  setup_test "slot_status_unreadable"
  mkdir -p "$GT_TOWN_ROOT/gastown"
  set_rigs '[{"name":"gastown","status":"operational"}]'
  set_mq "gastown" "[
    {\"id\":\"gt-mr-090\",\"status\":\"open\",\"created_at\":\"$(iso_ago 3600)\"}
  ]"
  fail_slot_status

  run_plugin

  assert_file_contains "$TEST_STATE/escalate.log" "stuck-work-dog:queue-stall:gastown" "slot_status_unreadable: escalates rather than assuming a batch holds the queue"
}

# --- Test 15: no unexpected gt/bd calls in a representative run ------------
test_no_unexpected_calls() {
  setup_test "no_unexpected_calls"
  mkdir -p "$GT_TOWN_ROOT/gastown"
  set_rigs '[{"name":"gastown","status":"operational"}]'
  set_mq "gastown" '[
    {"id":"gt-mr-060","status":"open","created_at":"2026-01-01T00:00:00Z","blocked_by":["gt-blocker-6"]}
  ]'
  set_bead "gt-blocker-6" "{\"id\":\"gt-blocker-6\",\"status\":\"open\",\"assignee\":\"\",\"created_at\":\"$(iso_ago 10800)\"}"

  run_plugin

  assert_file_empty "$TEST_STATE/unexpected.log" "no_unexpected_calls: only known gt/bd subcommands were invoked"
}


# --- Failed escalation: exit 1 so the daemon hands the run to a dog ---------
test_failed_escalation_exits_nonzero() {
  local rc=0
  setup_test "failed_escalation"
  mkdir -p "$GT_TOWN_ROOT/gastown"
  set_rigs '[{"name":"gastown","status":"operational"}]'
  set_mq "gastown" "[
    {\"id\":\"gt-mr-010\",\"status\":\"open\",\"created_at\":\"$(iso_ago 3600)\"}
  ]"
  touch "$TEST_STATE/escalate_fail"

  run_plugin || rc=$?

  if [ "$rc" -eq 1 ]; then
    record_pass "failed_escalation: exit 1"
  else
    record_fail "failed_escalation: exit 1 (got $rc)"
  fi
  assert_file_contains "$TEST_STATE/stdout.log" "ESCALATION FAILED: queue-stall:gastown" "failed_escalation: named in output"
  assert_file_contains "$TEST_STATE/stderr.log" "dolt unreachable" "failed_escalation: gt error kept"
  assert_file_contains "$TEST_STATE/record.log" "RECORD|stuck-work-dog|failure" "failed_escalation: records failure"
}

test_no_rigs
test_empty_queue
test_stale_unassigned_blocker
test_assigned_blocker_no_escalation
test_young_blocker_no_escalation
test_closed_blocker_no_escalation
test_queue_stall_escalates
test_queue_stall_skipped_when_polecat_working
test_queue_not_stalled_when_mr_in_progress
test_closed_mrs_ignored
test_multiple_rigs_checked
test_queue_stall_skipped_when_batch_in_flight
test_queue_stall_fires_when_only_another_rig_has_a_batch
test_queue_stall_fires_when_slot_status_unreadable
test_no_unexpected_calls
test_failed_escalation_exits_nonzero

echo ""
echo "=== $PASS passed, $FAIL failed ==="
[ "$FAIL" -eq 0 ]
