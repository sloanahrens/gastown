#!/usr/bin/env bash
# Tests for compactor-dog/run.sh helper functions.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
FAILURES=0
# Per-run log dirs, space-separated and cleaned by the EXIT trap. A variable
# set inside a $(...) command substitution never reaches the parent shell
# (macOS bash 3.2), so the runner appends each dir to this list in the parent
# where it is visible — RUN_LOG alone is not reliable for cleanup.
RUN_LOG_DIRS=""
RUN_LOG_DIR=""
run_test_cleanup() { rm -rf ${RUN_LOG_DIRS:-}; }
trap run_test_cleanup EXIT

# Source just the helper functions from run.sh by extracting them.
# We can't source the whole script (it runs immediately), so redefine here.
log() { echo "[test] $*"; }

# --- Copy validate_hash from run.sh (must stay in sync) ---
validate_hash() {
  local hash="$1"
  local context="$2"
  if [[ ! "$hash" =~ ^[a-v0-9]+$ ]]; then
    log "ERROR: Unsafe $context hash rejected: '$hash'"
    return 1
  fi
  return 0
}

# Verify our copy matches run.sh (guard against drift).
# Extract the regex from each file's validate_hash function with POSIX sed:
# grep -oP is GNU-only and fails on macOS/BSD grep, which is where the dog runs.
extract_hash_regex() {
  sed -n '/^validate_hash/,/^}/p' "$1" | sed -n 's/.*=~ \(.*\) \]\];.*/\1/p'
}
RUN_SH_REGEX=$(extract_hash_regex "$SCRIPT_DIR/run.sh")
TEST_REGEX=$(extract_hash_regex "$0")
if [[ -z "$RUN_SH_REGEX" || -z "$TEST_REGEX" ]]; then
  echo "FAIL: could not extract validate_hash regex (run.sh='$RUN_SH_REGEX' test='$TEST_REGEX')"
  echo "      Did the 'if [[ ! \"\$hash\" =~ ... ]]' line change shape?"
  exit 1
fi
if [[ "$RUN_SH_REGEX" != "$TEST_REGEX" ]]; then
  echo "FAIL: validate_hash regex in test ($TEST_REGEX) doesn't match run.sh ($RUN_SH_REGEX)"
  echo "      Update the test to match run.sh"
  exit 1
fi

# Verify compaction is not the default (gt-e14c). The destructive path
# (flatten + force-push) must require an explicit --compact flag; a plain
# `bash run.sh` has to stay monitor-only.
if ! grep -q '^CHECK_ONLY=true' "$SCRIPT_DIR/run.sh"; then
  echo "FAIL: run.sh no longer defaults CHECK_ONLY=true — destructive compaction is the default"
  FAILURES=$((FAILURES + 1))
fi
FALSE_SETTERS=$(grep -c 'CHECK_ONLY=false' "$SCRIPT_DIR/run.sh" || true)
COMPACT_FLAG=$(grep -c -- '--compact).*CHECK_ONLY=false' "$SCRIPT_DIR/run.sh" || true)
if [[ "$FALSE_SETTERS" != "$COMPACT_FLAG" ]]; then
  echo "FAIL: run.sh clears CHECK_ONLY on $FALSE_SETTERS line(s) but only $COMPACT_FLAG is the --compact flag"
  FAILURES=$((FAILURES + 1))
fi

assert_valid() {
  local hash="$1"
  if ! validate_hash "$hash" "test" >/dev/null 2>&1; then
    echo "FAIL: expected valid hash: '$hash'"
    FAILURES=$((FAILURES + 1))
  fi
}

assert_invalid() {
  local hash="$1"
  if validate_hash "$hash" "test" >/dev/null 2>&1; then
    echo "FAIL: expected invalid hash: '$hash'"
    FAILURES=$((FAILURES + 1))
  fi
}

# --- Tests ---

echo "=== validate_hash tests ==="

# Dolt base32 hashes (real examples)
assert_valid "aecqtmbdbabpalqnamq8atfv86ehjf7r"
assert_valid "0123456789abcdefghijklmnopqrstuv"
assert_valid "abc123"
assert_valid "00000000"

# Hex-only hashes should still pass (subset of base32)
assert_valid "deadbeef"
assert_valid "abcdef0123456789"

# Invalid: characters outside base32 range
assert_invalid "xyz"
assert_invalid "ABCDEF"
assert_invalid "hash-with-dashes"
assert_invalid "hash_with_underscores"
assert_invalid "hash with spaces"
assert_invalid ""
assert_invalid "../../../etc/passwd"
assert_invalid "'; DROP TABLE issues; --"

# --- Last-run check finds existing receipts (gt-idwq) ---
#
# Receipts are ephemeral wisps (`gt plugin record-run` -> bd create
# --ephemeral). A real `bd list` hides ephemeral beads, so the query in
# plugin.md returns [] and reports "never" unless it passes --include-infra.
# The fake bd below reproduces that filtering, with a receipt on hand: the
# query has to find it.
echo ""
echo "=== last-run query tests ==="

PLUGIN_MD="$SCRIPT_DIR/plugin.md"
QUERY_OK=true

# Extract the shipped query instead of copying it, so plugin.md stays the
# single source of truth. Breaking its shape fails the test rather than
# silently testing a stale copy.
LAST_RUN_QUERY=$(sed -n '/^RECENT_RUNS=/,/^  | jq/p' "$PLUGIN_MD" || true)
QUERY_LINES=$(printf '%s\n' "$LAST_RUN_QUERY" | grep -c . || true)
if [[ "$QUERY_LINES" != "2" || "$LAST_RUN_QUERY" != *"jq"* ]]; then
  echo "FAIL: expected a 2-line 'RECENT_RUNS=(bd list ... | jq' query in $PLUGIN_MD,"
  echo "      got $QUERY_LINES line(s). Did the last-run check change shape?"
  echo "      (A sed range that misses its end pattern runs to EOF, so the shape"
  echo "      check also keeps this test from eval'ing the rest of the file.)"
  QUERY_OK=false
  FAILURES=$((FAILURES + 1))
fi

if ! command -v jq >/dev/null 2>&1; then
  echo "SKIP: jq not installed (the query pipes through jq)"
elif $QUERY_OK; then
  FAKE_DIR=$(mktemp -d)
  # Replaces the top-level cleanup trap: this block owns FAKE_DIR, so the EXIT
  # trap now covers it alongside the per-run log dirs.
  trap 'rm -rf ${RUN_LOG_DIRS:-} "${FAKE_DIR:-}"' EXIT

  # Run the extracted query under whichever fake bd is installed in FAKE_DIR.
  # The subshell keeps PATH changes from leaking into later tests.
  run_last_run_query() {
    (
      export PATH="$FAKE_DIR:$PATH"
      export FAKE_RECEIPT="$1"
      RECENT_RUNS=""
      eval "$LAST_RUN_QUERY"
      printf '%s' "$RECENT_RUNS"
    )
  }

  write_fake_bd() {
    cat > "$FAKE_DIR/bd"
    chmod +x "$FAKE_DIR/bd"
  }

  # Receipt visible only when --include-infra is passed, as real bd behaves.
  write_fake_bd <<'FAKE_BD'
#!/usr/bin/env bash
for arg in "$@"; do
  if [[ "$arg" == "--include-infra" ]]; then
    printf '%s\n' "$FAKE_RECEIPT"
    exit 0
  fi
done
printf '[]\n'
FAKE_BD

  RECEIPT='[{"id":"hq-wisp-test1","created_at":"2026-09-22T10:00:00Z","labels":["type:plugin-run","plugin:compactor-dog","result:success"]}]'
  FOUND=$(run_last_run_query "$RECEIPT")
  if [[ "$FOUND" != "2026-09-22T10:00:00Z" ]]; then
    echo "FAIL: last-run query missed an existing receipt (got '$FOUND', want '2026-09-22T10:00:00Z')"
    echo "      The query must pass --include-infra; receipts are ephemeral wisps."
    FAILURES=$((FAILURES + 1))
  fi

  # Negative control: against a bd that returns [] no matter what, the same
  # query must report "never". Without this, a query that never ran (empty
  # extraction, missing jq) would pass as "found nothing" above.
  write_fake_bd <<'FAKE_BD_EMPTY'
#!/usr/bin/env bash
printf '[]\n'
FAKE_BD_EMPTY

  EMPTY=$(run_last_run_query "$RECEIPT")
  if [[ "$EMPTY" != "never" ]]; then
    echo "FAIL: negative control expected 'never' from an always-empty bd, got '$EMPTY'"
    echo "      The harness is not exercising the query in plugin.md."
    FAILURES=$((FAILURES + 1))
  fi
fi

# --- Check-only escalation loop (gt-hrt9) ---
#
# Monitor-only is a steady state: the daemon records a receipt and exits 0,
# so the old doc contract (a dog reads plugin.md and escalates on any
# over-threshold signal) could never fire — the plugin raised nothing. The
# escalation loop now lives in run.sh's check-only branch: one gt escalate
# per candidate DB, a warning receipt when any candidate was escalated, a
# failure receipt when none was, and exit 0 either way. Drive the real
# script with faked dolt and gt commands and assert the branch's behavior.
echo ""
echo "=== check-only escalation tests ==="

# Returns a fresh per-run log dir on stdout. The value only reaches the
# caller through the $(...) subshell, so any variable set inside this
# subshell is gone the moment it returns — the caller is responsible for
# recording the dir (see run_with_fakes / RUN_LOG_DIRS).
run_log_dir() {
  printf '%s' "$(mktemp -d /tmp/compactor-dog-run.XXXXXX)"
}

write_fakes() {
  local dir="$1" esc_rc="${2:-0}"
  cat > "$dir/dolt" <<'FAKE_DOLT'
#!/usr/bin/env bash
# Stand-in for the `dolt sql` CLI as run.sh calls it:
# `dolt --host H --port P --no-tls -u U -p "" [--use-db DB] sql -q QUERY --result-format csv`
# (run.sh puts global flags BEFORE the sql subcommand). It serves two
# databases: hq over the threshold (600), beads below it (10).
while [[ $# -gt 0 ]]; do
  case "$1" in
    -q) q="${2:-}"; shift ;;
    --use-db) db="${2:-}"; shift ;;
    sql) sub="sql" ;;
    *) : ;;
  esac
  shift
done
if [[ "${sub:-}" == "sql" ]]; then
  if [[ "$q" == "SHOW DATABASES" ]]; then
    printf 'Database\nhq\nbeads\n'
  elif [[ "$q" == *"dolt_log"* ]]; then
    if [[ "$db" == "hq" ]]; then
      printf 'cnt\n600\n'
    else
      printf 'cnt\n10\n'
    fi
  fi
fi
exit 0
FAKE_DOLT
  cat > "$dir/gt" <<FAKE_GT
#!/usr/bin/env bash
LOG="\${RUN_LOG:-/dev/null}"
case "\${1:-}" in
escalate)
  printf 'ESCALATE %s\n' "\$*" >> "\$LOG"
  exit "$esc_rc"
  ;;
plugin)
  printf 'RECORD-RUN %s\n' "\$*" >> "\$LOG"
  exit 0
  ;;
*)
  exit 0
  ;;
esac
FAKE_GT
  chmod +x "$dir/dolt" "$dir/gt"
}

# Drive the real run.sh with the faked gt/dolt on PATH. $1 = fake bin dir,
# remaining args go to run.sh. The runner creates a fresh log dir (the fake
# gt appends ops there), records it in RUN_LOG_DIR (global, set in the
# parent) and RUN_LOG_DIRS (for the EXIT trap), then runs run.sh in a
# subshell. Do NOT call this from $(...) — the function must run in the
# parent shell for RUN_LOG_DIRS to reach the EXIT trap.
run_with_fakes() {
  local dir="$1" d
  shift
  d=$(run_log_dir)
  RUN_LOG_DIR="$d"
  RUN_LOG_DIRS="$RUN_LOG_DIRS $d"
  # The run sits in an `if` so its status cannot end the harness through
  # set -e: the caller asserts on RUN_RC instead.
  if (
    export PATH="$dir:$PATH"
    export RUN_LOG="$d/ops"
    # Pin monitor mode: these tests assert monitor/flatten escalation
    # behavior and must not pick up this machine's real
    # mayor/daemon.json scheduled_maintenance.mode (gt-124a6).
    export COMPACTOR_MAINT_MODE="monitor"
    bash "$SCRIPT_DIR/run.sh" "$@"
  ) >/dev/null 2>&1; then
    RUN_RC=0
  else
    RUN_RC=$?
  fi
  if [[ $RUN_RC -ne 0 ]]; then
    echo "HARNESS: run.sh exited $RUN_RC (see ops in $d)" >&2
  fi
  return 0
}

FAKE_DIR=$(mktemp -d)
FAKE_DIR_RC1=$(mktemp -d)
trap 'rm -rf ${RUN_LOG_DIRS:-} "$FAKE_DIR" "$FAKE_DIR_RC1"' EXIT
write_fakes "$FAKE_DIR" 0
write_fakes "$FAKE_DIR_RC1" 1

# (a) One over-threshold DB: the check-only branch escalates exactly one
# DB, records a warning receipt, and exits 0 (the run stays a success —
# escalation is the happy path, not a failure).
run_with_fakes "$FAKE_DIR"
RC=$RUN_RC
LOG="$RUN_LOG_DIR"
if [[ "$RC" -ne 0 ]]; then
  echo "FAIL: check-only with a candidate exited $RC, want 0"
  FAILURES=$((FAILURES + 1))
fi
ESC_COUNT=$( (grep -c '^ESCALATE ' "$LOG/ops" 2>/dev/null) || true )
ESC_COUNT=${ESC_COUNT:-0}
ESC_COUNT="${ESC_COUNT//$'\n'/}"
REC_COUNT=$( (grep -c '^RECORD-RUN ' "$LOG/ops" 2>/dev/null) || true )
REC_COUNT=${REC_COUNT:-0}
REC_COUNT="${REC_COUNT//$'\n'/}"
if [[ "$ESC_COUNT" != "1" ]]; then
  echo "FAIL: expected 1 escalation (one per candidate DB), got $ESC_COUNT"
  FAILURES=$((FAILURES + 1))
fi
if [[ "$ESC_COUNT" -gt 0 ]] && ! grep -q 'ESCALATE .*hq' "$LOG/ops"; then
  echo "FAIL: escalation does not name the over-threshold DB (hq)"
  FAILURES=$((FAILURES + 1))
fi
if ! grep -q -- '--result warning' "$LOG/ops"; then
  echo "FAIL: escalated check-only run must record a warning receipt"
  FAILURES=$((FAILURES + 1))
fi
if [[ "$REC_COUNT" -lt 1 ]]; then
  echo "FAIL: check-only run recorded no receipt"
  FAILURES=$((FAILURES + 1))
fi

# (b) gt escalate fails for every candidate: one HIGH failover escalation
# covers the run, the receipt records `failure` — distinct from the
# `check-only` a run with nothing to report writes — and the script still
# exits 0, because the run itself completed and only its signal path failed.
run_with_fakes "$FAKE_DIR_RC1"
RC=$RUN_RC
LOG="$RUN_LOG_DIR"
if [[ "$RC" -ne 0 ]]; then
  echo "FAIL: check-only with failed escalations exited $RC, want 0"
  FAILURES=$((FAILURES + 1))
fi
if ! grep -q -- '-s HIGH' "$LOG/ops"; then
  echo "FAIL: failed per-DB escalations must trigger the HIGH failover escalation"
  FAILURES=$((FAILURES + 1))
fi
if ! grep -q -- '--result failure' "$LOG/ops"; then
  echo "FAIL: a run whose escalations all failed must record a failure receipt"
  FAILURES=$((FAILURES + 1))
fi
if grep -q -- '--result check-only' "$LOG/ops"; then
  echo "FAIL: total escalation failure recorded check-only — indistinguishable from a run with nothing to report"
  FAILURES=$((FAILURES + 1))
fi

# (c) --compact must not go through the check-only escalation branch:
# the destructive path escalates only on integrity/compaction errors,
# never per candidate.
# The fake dolt serves the same counts in every mode, so --compact goes
# through the full flatten cycle; the branch distinguisher is that the
# per-candidate escalation message never appears (that wording belongs to
# the check-only branch only).
run_with_fakes "$FAKE_DIR" --compact
LOG="$RUN_LOG_DIR"
# grep -c prints "0" on no match but exits 1, which under set -e would kill
# the harness mid-test: run it in a guarded subshell so the assignment always
# succeeds, then trim the count to a single bare number.
CANDIDATE_ESC=$( (grep -c 'ESCALATE .*commits (threshold' "$LOG/ops" 2>/dev/null) || true )
CANDIDATE_ESC=${CANDIDATE_ESC:-0}
CANDIDATE_ESC="${CANDIDATE_ESC//$'\n'/}"
if [[ "$CANDIDATE_ESC" != "0" ]]; then
  echo "FAIL: --compact path raised '$CANDIDATE_ESC' per-candidate escalation(s); that branch is check-only's"
  FAILURES=$((FAILURES + 1))
fi

# --- Daemon threshold coordination (gt-hrt9 attempt 3) ---
#
# The daemon's own compactor_dog patrol escalates independently once a DB
# crosses its configured threshold (default 2000). Without a matching upper
# bound here, run.sh would escalate the same DB again on every check-only
# cycle between the plugin's threshold and the daemon's — a double alert.
# COMPACTOR_DAEMON_THRESHOLD lets these tests set that line without depending
# on mayor/daemon.json or jq's real output.
echo ""
echo "=== daemon threshold coordination tests ==="

write_fake_dolt_counts() {
  local dir="$1" hq_count="$2" beads_count="$3"
  cat > "$dir/dolt" <<FAKE_DOLT
#!/usr/bin/env bash
while [[ \$# -gt 0 ]]; do
  case "\$1" in
    -q) q="\${2:-}"; shift ;;
    --use-db) db="\${2:-}"; shift ;;
    sql) sub="sql" ;;
    *) : ;;
  esac
  shift
done
if [[ "\${sub:-}" == "sql" ]]; then
  if [[ "\$q" == "SHOW DATABASES" ]]; then
    printf 'Database\nhq\nbeads\n'
  elif [[ "\$q" == *"dolt_log"* ]]; then
    if [[ "\$db" == "hq" ]]; then
      printf 'cnt\n$hq_count\n'
    else
      printf 'cnt\n$beads_count\n'
    fi
  fi
fi
exit 0
FAKE_DOLT
  cat > "$dir/gt" <<'FAKE_GT'
#!/usr/bin/env bash
LOG="${RUN_LOG:-/dev/null}"
case "${1:-}" in
escalate)
  printf 'ESCALATE %s\n' "$*" >> "$LOG"
  exit 0
  ;;
plugin)
  printf 'RECORD-RUN %s\n' "$*" >> "$LOG"
  exit 0
  ;;
*)
  exit 0
  ;;
esac
FAKE_GT
  chmod +x "$dir/dolt" "$dir/gt"
}

run_with_daemon_threshold() {
  local dir="$1" threshold="$2" d
  shift 2
  d=$(run_log_dir)
  RUN_LOG_DIR="$d"
  RUN_LOG_DIRS="$RUN_LOG_DIRS $d"
  if (
    export PATH="$dir:$PATH"
    export RUN_LOG="$d/ops"
    export COMPACTOR_DAEMON_THRESHOLD="$threshold"
    # Pin monitor mode for the same reason as run_with_fakes above — these
    # tests predate the gc-mode gate and assert monitor/flatten behavior.
    export COMPACTOR_MAINT_MODE="monitor"
    bash "$SCRIPT_DIR/run.sh" "$@"
  ) >/dev/null 2>&1; then
    RUN_RC=0
  else
    RUN_RC=$?
  fi
  if [[ $RUN_RC -ne 0 ]]; then
    echo "HARNESS: run.sh exited $RUN_RC (see ops in $d)" >&2
  fi
  return 0
}

# Same as run_with_daemon_threshold, but also pins scheduled_maintenance.mode
# (gt-124a6: gc mode changes whether commit count below the daemon threshold
# is an escalation signal at all).
run_with_daemon_threshold_and_mode() {
  local dir="$1" threshold="$2" mode="$3" d
  shift 3
  d=$(run_log_dir)
  RUN_LOG_DIR="$d"
  RUN_LOG_DIRS="$RUN_LOG_DIRS $d"
  if (
    export PATH="$dir:$PATH"
    export RUN_LOG="$d/ops"
    export COMPACTOR_DAEMON_THRESHOLD="$threshold"
    export COMPACTOR_MAINT_MODE="$mode"
    bash "$SCRIPT_DIR/run.sh" "$@"
  ) >/dev/null 2>&1; then
    RUN_RC=0
  else
    RUN_RC=$?
  fi
  if [[ $RUN_RC -ne 0 ]]; then
    echo "HARNESS: run.sh exited $RUN_RC (see ops in $d)" >&2
  fi
  return 0
}

# (d) One candidate below the daemon threshold, one at/above it: the script
# escalates only the sub-threshold candidate, defers the other to the
# daemon's own patrol, and still records a warning (one real signal was
# raised).
FAKE_DIR_DEFER=$(mktemp -d)
trap 'rm -rf ${RUN_LOG_DIRS:-} "$FAKE_DIR" "$FAKE_DIR_RC1" "$FAKE_DIR_DEFER"' EXIT
write_fake_dolt_counts "$FAKE_DIR_DEFER" 600 3000

run_with_daemon_threshold "$FAKE_DIR_DEFER" 2000
LOG="$RUN_LOG_DIR"
if [[ "$RUN_RC" -ne 0 ]]; then
  echo "FAIL: check-only with a deferred candidate exited $RUN_RC, want 0"
  FAILURES=$((FAILURES + 1))
fi
if grep -q 'ESCALATE .*beads' "$LOG/ops" 2>/dev/null; then
  echo "FAIL: candidate at/above the daemon threshold (beads, 3000) was escalated — the daemon already owns this band"
  FAILURES=$((FAILURES + 1))
fi
if ! grep -q 'ESCALATE .*hq' "$LOG/ops" 2>/dev/null; then
  echo "FAIL: candidate below the daemon threshold (hq, 600) was not escalated"
  FAILURES=$((FAILURES + 1))
fi
if ! grep -q -- '--result warning' "$LOG/ops" 2>/dev/null; then
  echo "FAIL: one escalatable plus one deferred candidate must still record a warning receipt"
  FAILURES=$((FAILURES + 1))
fi

# (e) Every candidate at/above the daemon threshold: nothing is escalated by
# the script (all deferred), so the receipt is check-only, not warning or
# failure — but the description still names the deferred count so this
# doesn't read as "nothing found".
FAKE_DIR_ALL_DEFER=$(mktemp -d)
trap 'rm -rf ${RUN_LOG_DIRS:-} "$FAKE_DIR" "$FAKE_DIR_RC1" "$FAKE_DIR_DEFER" "$FAKE_DIR_ALL_DEFER"' EXIT
write_fake_dolt_counts "$FAKE_DIR_ALL_DEFER" 2500 3000

run_with_daemon_threshold "$FAKE_DIR_ALL_DEFER" 2000
LOG="$RUN_LOG_DIR"
if [[ "$RUN_RC" -ne 0 ]]; then
  echo "FAIL: check-only with all candidates deferred exited $RUN_RC, want 0"
  FAILURES=$((FAILURES + 1))
fi
if grep -q '^ESCALATE ' "$LOG/ops" 2>/dev/null; then
  echo "FAIL: no per-DB escalation should fire when every candidate is at/above the daemon threshold"
  FAILURES=$((FAILURES + 1))
fi
if ! grep -q -- '--result check-only' "$LOG/ops" 2>/dev/null; then
  echo "FAIL: a run with every candidate deferred to the daemon must record check-only, not warning or failure"
  FAILURES=$((FAILURES + 1))
fi
if ! grep -q "deferred to the daemon's own patrol" "$LOG/ops" 2>/dev/null; then
  echo "FAIL: the check-only receipt must say candidates were deferred, not silently drop them"
  FAILURES=$((FAILURES + 1))
fi

# (f) gc mode: a candidate between the plugin's 500 floor and the daemon
# threshold must NOT be escalated — commit count isn't a disk signal in gc
# mode (plugin.md Step 6), so the script defers it exactly like the
# at/above-threshold band. This is the gt-124a6 regression: monitor mode
# still escalates hq (600, below a 20000 daemon threshold); gc mode must not.
FAKE_DIR_GC=$(mktemp -d)
trap 'rm -rf ${RUN_LOG_DIRS:-} "$FAKE_DIR" "$FAKE_DIR_RC1" "$FAKE_DIR_DEFER" "$FAKE_DIR_ALL_DEFER" "$FAKE_DIR_GC"' EXIT
write_fake_dolt_counts "$FAKE_DIR_GC" 600 3000

run_with_daemon_threshold_and_mode "$FAKE_DIR_GC" 20000 "gc"
LOG="$RUN_LOG_DIR"
if [[ "$RUN_RC" -ne 0 ]]; then
  echo "FAIL: gc-mode check-only exited $RUN_RC, want 0"
  FAILURES=$((FAILURES + 1))
fi
if grep -q '^ESCALATE ' "$LOG/ops" 2>/dev/null; then
  echo "FAIL: gc mode escalated a candidate (hq, 600) below the daemon threshold (20000) — commit count isn't a disk signal in gc mode"
  FAILURES=$((FAILURES + 1))
fi
if ! grep -q -- '--result check-only' "$LOG/ops" 2>/dev/null; then
  echo "FAIL: gc mode with every candidate deferred must record check-only, not warning"
  FAILURES=$((FAILURES + 1))
fi

# (g) monitor mode (the default) with the same fixture still escalates the
# sub-threshold candidate — the gc-mode gate must not suppress the existing
# monitor/flatten behavior.
FAKE_DIR_MONITOR=$(mktemp -d)
trap 'rm -rf ${RUN_LOG_DIRS:-} "$FAKE_DIR" "$FAKE_DIR_RC1" "$FAKE_DIR_DEFER" "$FAKE_DIR_ALL_DEFER" "$FAKE_DIR_GC" "$FAKE_DIR_MONITOR"' EXIT
write_fake_dolt_counts "$FAKE_DIR_MONITOR" 600 3000

run_with_daemon_threshold_and_mode "$FAKE_DIR_MONITOR" 20000 "monitor"
LOG="$RUN_LOG_DIR"
if [[ "$RUN_RC" -ne 0 ]]; then
  echo "FAIL: monitor-mode check-only exited $RUN_RC, want 0"
  FAILURES=$((FAILURES + 1))
fi
if ! grep -q 'ESCALATE .*hq' "$LOG/ops" 2>/dev/null; then
  echo "FAIL: monitor mode must still escalate a sub-daemon-threshold candidate (hq, 600) — the gc-mode gate should not apply here"
  FAILURES=$((FAILURES + 1))
fi

echo ""
if [[ $FAILURES -gt 0 ]]; then
  echo "FAILED: $FAILURES test(s) failed"
  exit 1
else
  echo "PASSED: all tests passed"
fi
