#!/usr/bin/env bash
# Tests for plugins/town-metrics/run.sh (gt-tctk4): a fake gt on a temporary
# PATH records what it was asked to run and answers with the exit code and
# output the case sets, and the town root is a temporary directory, so the
# receipt, the exit code, the single-flight lock and its crash takeover are
# exercised without appending a real snapshot. Run directly or via the shell
# tier: scripts/test-makefile.sh calls it.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
RUN="$SCRIPT_DIR/run.sh"
BASH_BIN="$(command -v bash)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
PASS=0
FAIL=0

pass() { echo "  PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "  FAIL: $1"; FAIL=$((FAIL + 1)); }

STATE="$TMP/state"
mkdir -p "$STATE"

# --- Stubs and fixtures -------------------------------------------------------

# gt appends the argv it got to the calls file, then answers with the output
# and exit code its subcommand's STUB_* variables set (STUB_OUT_SNAPSHOT,
# STUB_RC_ANALYSE, ...), falling back to STUB_OUT/STUB_ERR/STUB_RC, so a test
# can fail one step and not the other. With STUB_HOLD set it holds the run open
# until the test releases it: it opens the release FIFO read-write (so no open
# blocks), tells the test it started through the started FIFO, and waits, with a
# bound, for one line on the release FIFO. Nothing here polls.
mkdir -p "$TMP/bin" "$TMP/empty"
cat >"$TMP/bin/gt" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_STATE/calls"
if [ -n "${STUB_HOLD:-}" ]; then
  exec 4<>"$STUB_STATE/release"
  printf 'started\n' >"$STUB_STATE/started"
  read -r -t 30 -u 4 _ || true
fi
case "$1 ${2:-}" in
  "metrics snapshot")
    out="${STUB_OUT_SNAPSHOT-${STUB_OUT:-}}"
    err="${STUB_ERR_SNAPSHOT-${STUB_ERR:-}}"
    rc="${STUB_RC_SNAPSHOT-${STUB_RC:-0}}"
    ;;
  "metrics analyse")
    out="${STUB_OUT_ANALYSE-${STUB_OUT:-}}"
    err="${STUB_ERR_ANALYSE-${STUB_ERR:-}}"
    rc="${STUB_RC_ANALYSE-${STUB_RC:-0}}"
    ;;
  *)
    out="${STUB_OUT:-}"
    err="${STUB_ERR:-}"
    rc="${STUB_RC:-0}"
    ;;
esac
printf '%s' "$out"
printf '%s' "$err" >&2
exit "$rc"
STUB
chmod +x "$TMP/bin/gt"

TOWN="$TMP/town"
mkdir -p "$TOWN/.runtime"
LOCK="$TOWN/.runtime/town-metrics.lock"
RECEIPT="snapshot 2026-10-24T21:00:00Z appended to $TOWN/.runtime/metrics/snapshots.jsonl (8 groups, 0 errors)"
ANALYSIS="analyse 2026-10-25T02:00:00Z: 44 landings, 3 targets -> $TOWN/.runtime/metrics/latest/panel.json"

# want_calls is the two commands an unsplit run gives the stub, one per line.
want_calls() { printf '%s\n%s' "metrics snapshot" "metrics analyse"; }

# run_wrapper OUT [GT_RC GT_OUT GT_ERR TOWN PATH ANALYSE_RC ANALYSE_OUT] runs
# run.sh against the stubs; rc holds its exit code and the whole output (stdout
# and stderr together) lands in OUT. The analysis answers with ANALYSIS and exit
# 0 unless ANALYSE_RC or ANALYSE_OUT says otherwise.
run_wrapper() {
  local out=$1 gt_rc=${2:-0} gt_out=${3:-} gt_err=${4:-} town=${5:-$TOWN} path=${6:-$TMP/bin:$PATH}
  local analyse_rc=${7:-0} analyse_out=${8:-$ANALYSIS}
  : >"$STATE/calls"
  rc=0
  PATH="$path" GT_TOWN_ROOT="$town" STUB_STATE="$STATE" \
    STUB_RC_SNAPSHOT="$gt_rc" STUB_OUT_SNAPSHOT="$gt_out" STUB_ERR_SNAPSHOT="$gt_err" \
    STUB_RC_ANALYSE="$analyse_rc" STUB_OUT_ANALYSE="$analyse_out" \
    "$BASH_BIN" "$RUN" >"$out" 2>&1 || rc=$?
}

# calls prints the commands the stub was given, one per line.
calls() { cat "$STATE/calls" 2>/dev/null || true; }

echo "town-metrics: a run appends one snapshot, analyses and releases its lock"
rm -f "$LOCK"
run_wrapper "$TMP/out" 0 "$RECEIPT"
if [ "$rc" == 0 ] && [ "$(calls)" == "$(want_calls)" ] \
  && [ "$(cat "$TMP/out")" == "$RECEIPT$ANALYSIS" ] && [ ! -e "$LOCK" ]; then
  pass "the snapshot then the analysis, their lines are the output in order, and the lock is gone"
else
  fail "the snapshot then the analysis (rc=$rc, calls=$(calls | tr '\n' '|'), $(cat "$TMP/out"))"
fi
run_wrapper "$TMP/out" 0 "$RECEIPT"
if [ "$rc" == 0 ] && [ "$(calls)" == "$(want_calls)" ]; then
  pass "the next run is not skipped by the previous run's released lock"
else
  fail "the next run is not skipped by the previous run's released lock (rc=$rc, $(cat "$TMP/out"))"
fi

echo "town-metrics: a failing gt metrics snapshot fails the run with its code"
run_wrapper "$TMP/out" 2 "" "Error: appending the snapshot: permission denied"
if [ "$rc" == 2 ] && grep -qF "appending the snapshot: permission denied" "$TMP/out"; then
  pass "exit 2 from the command is exit 2 from the plugin, with the cause"
else
  fail "exit 2 from the command is exit 2 from the plugin (rc=$rc, $(cat "$TMP/out"))"
fi
if [ ! -e "$LOCK" ]; then
  pass "a failed run releases its lock"
else
  fail "a failed run releases its lock"
fi

echo "town-metrics: a failing snapshot does not stop the analysis"
run_wrapper "$TMP/out" 2 "" "Error: appending the snapshot: permission denied"
if [ "$rc" == 2 ] && [ "$(calls)" == "$(want_calls)" ] && grep -qF "$ANALYSIS" "$TMP/out"; then
  pass "the analysis runs after a snapshot that failed, and the snapshot's code is the run's"
else
  fail "the analysis runs after a failed snapshot (rc=$rc, calls=$(calls | tr '\n' '|'), $(cat "$TMP/out"))"
fi

echo "town-metrics: a failing analysis fails the run with its code"
run_wrapper "$TMP/out" 0 "$RECEIPT" "" "$TOWN" "$TMP/bin:$PATH" 3 "analyse: extract: python3: exit status 1"
if [ "$rc" == 3 ] && [ "$(calls)" == "$(want_calls)" ] && grep -qF "exit status 1" "$TMP/out" \
  && grep -qF "$RECEIPT" "$TMP/out"; then
  pass "the snapshot's receipt is still the run's output and the analysis's code is the run's"
else
  fail "a failing analysis fails the run with its code (rc=$rc, calls=$(calls | tr '\n' '|'), $(cat "$TMP/out"))"
fi
if [ ! -e "$LOCK" ]; then
  pass "a run whose analysis failed releases its lock"
else
  fail "a run whose analysis failed releases its lock"
fi

echo "town-metrics: a lock a live run holds skips the hour"
printf '%s\n' "$$" >"$LOCK"
run_wrapper "$TMP/out"
if [ "$rc" == 0 ] && [ -z "$(calls)" ] && grep -qF "[plugin-result skipped]" "$TMP/out" \
  && grep -qF "another run holds" "$TMP/out"; then
  pass "a held lock: exit 0, a skipped line, and no snapshot"
else
  fail "a held lock: exit 0, a skipped line, and no snapshot (rc=$rc, calls=$(calls | tr '\n' '|'), $(cat "$TMP/out"))"
fi
if [ "$(cat "$LOCK")" == "$$" ]; then
  pass "the skipping run leaves the holder's lock in place"
else
  fail "the skipping run leaves the holder's lock in place ($(cat "$LOCK" 2>&1))"
fi

echo "town-metrics: a lock whose run is gone is taken over"
# A reaped child's pid is a pid with nothing behind it: exactly what a killed
# run leaves, and kill -0 says so.
"$BASH_BIN" -c 'exit 0' &
dead=$!
wait "$dead" 2>/dev/null || true
printf '%s\n' "$dead" >"$LOCK"
run_wrapper "$TMP/out" 0 "$RECEIPT"
if [ "$rc" == 0 ] && [ "$(calls)" == "$(want_calls)" ] && [ ! -e "$LOCK" ]; then
  pass "a stale lock does not block the next run, and is not left behind"
else
  fail "a stale lock does not block the next run (rc=$rc, calls=$(calls | tr '\n' '|'), lock=$(cat "$LOCK" 2>&1))"
fi

echo "town-metrics: a run that overlaps another skips instead of snapshotting twice"
: >"$STATE/calls"
# The test holds the started FIFO read-write, so the stub's write never blocks
# and a read with -t is bounded; the stub does the same on the release FIFO.
mkfifo "$STATE/started" "$STATE/release"
exec 3<>"$STATE/started"
PATH="$TMP/bin:$PATH" GT_TOWN_ROOT="$TOWN" STUB_STATE="$STATE" STUB_HOLD=1 \
  "$BASH_BIN" "$RUN" >"$TMP/first" 2>&1 &
first_pid=$!
if read -r -t 20 -u 3 _; then
  second_rc=0
  PATH="$TMP/bin:$PATH" GT_TOWN_ROOT="$TOWN" STUB_STATE="$STATE" \
    "$BASH_BIN" "$RUN" >"$TMP/second" 2>&1 || second_rc=$?
  printf 'go\n' >"$STATE/release"
  first_rc=0
  wait "$first_pid" || first_rc=$?
  n_calls=$(wc -l <"$STATE/calls" | tr -d ' ')
  if [ "$first_rc" == 0 ] && [ "$second_rc" == 0 ] \
    && grep -qF "[plugin-result skipped]" "$TMP/second" \
    && [ "$n_calls" == 2 ]; then
    pass "the second run skips while the first holds the lock, and only one run's two steps execute"
  else
    fail "the second run skips while the first holds the lock (first=$first_rc, second=$second_rc, calls=$(calls | tr '\n' '|'), $(cat "$TMP/second"))"
  fi
else
  kill "$first_pid" 2>/dev/null || true
  wait "$first_pid" 2>/dev/null || true
  fail "the first run never reached gt within 20 s"
fi
exec 3>&-
rm -f "$STATE/started" "$STATE/release"

echo "town-metrics: a run with no gt on PATH fails closed"
run_wrapper "$TMP/out" 0 "" "" "$TOWN" "$TMP/empty"
if [ "$rc" == 1 ] && grep -qF "is not on PATH" "$TMP/out" && [ ! -e "$LOCK" ]; then
  pass "no gt: exit 1, the reason is named, and no lock is left"
else
  fail "no gt: exit 1, the reason is named (rc=$rc, $(cat "$TMP/out"))"
fi

echo "town-metrics: a run with no town root fails closed"
run_wrapper "$TMP/out" 0 "" "" "$TMP/no-such-town"
if [ "$rc" == 1 ] && grep -qF "no town root at" "$TMP/out" && [ -z "$(calls)" ]; then
  pass "no town root: exit 1, the path is named, and no snapshot runs"
else
  fail "no town root: exit 1, the path is named (rc=$rc, calls=$(calls | tr '\n' '|'), $(cat "$TMP/out"))"
fi

echo "town-metrics: $PASS passed, $FAIL failed"
[[ "$FAIL" -eq 0 ]]
