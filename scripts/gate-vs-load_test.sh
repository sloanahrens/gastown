#!/usr/bin/env bash
# Tests for scripts/gate-vs-load.py, the read-only report that reads the
# landing stages lines out of daemon.log and buckets the gate and lint wall
# times by the host load1 beside them (gt-lkhim).
#
# Fixture logs pin every property the spec asks for: a landing in each load
# bucket, a landing with no load suffix (reported on its own row, not dropped),
# Go-style durations (18s, 1m45s, 5m2s), an empty bucket that must not print a
# row, and a --since run that reads the log line's date.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPORT="$SCRIPT_DIR/gate-vs-load.py"

command -v python3 >/dev/null 2>&1 || {
  echo "gate-vs-load_test: python3 not found on PATH" >&2
  exit 1
}

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
PASS=0
FAIL=0

pass() { echo "  PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "  FAIL: $1"; FAIL=$((FAIL + 1)); }

# run ARGS...: executes the report with the fixtures' log and captures OUT/RC.
run() {
  set +e
  OUT="$(python3 "$REPORT" "$@" 2>&1)"
  RC=$?
  set -e
}

assert_rc() {
  if [[ "$RC" -eq "$2" ]]; then
    pass "$1"
  else
    fail "$1 (exit $RC, want $2)"
    echo "$OUT" | sed 's/^/    | /'
  fi
}

# assert_has NAME LINE NEEDLE: LINE contains NEEDLE (fixed string).
assert_has() {
  if grep -qF -- "$3" <<<"$2"; then
    pass "$1"
  else
    fail "$1 (no [$3] in [$2])"
  fi
}

assert_lacks() {
  if grep -qF -- "$3" <<<"$2"; then
    fail "$1 (unexpected [$3] in [$2])"
  else
    pass "$1"
  fi
}

# row NAME LABEL: OUT contains a table row for LABEL, echoed into ROW.
row() {
  ROW="$(grep -E "^${2} " <<<"$OUT" || true)"
  if [[ -n "$ROW" ]]; then
    pass "$1"
  else
    fail "$1 (no row for [$2])"
  fi
}

# A landing per load bucket, one with no load reading, and two Go-style
# durations the spec names (1m45s in "under 10", 5m2s in "over 45").
cat >"$TMP/buckets.log" <<'LOG'
2026/10/03 20:00:00 landing_worker: [land] aaa: stages: lint 10s, gate 20s, om 30s (load1 5.0)
2026/10/03 20:01:00 landing_worker: [land] bbb: stages: gate 1m45s, om 30s (load1 5.0)
2026/10/03 20:02:00 landing_worker: [land] ccc: stages: lint 12s, gate 30s, om 30s (load1 12.0)
2026/10/03 20:03:00 landing_worker: [land] ddd: stages: lint 14s, gate 40s, om 30s (load1 25.0)
2026/10/03 20:04:00 landing_worker: [land] eee: stages: lint 16s, gate 50s, om 30s (load1 33.0)
2026/10/03 20:05:00 landing_worker: [land] fff: stages: lint 18s, gate 5m2s, om 30s (load1 52.0)
2026/10/03 20:06:00 landing_worker: [land] ggg: stages: lint 11s, gate 22s, om 30s
LOG

run --log "$TMP/buckets.log"
assert_rc "a readable log exits 0" 0

# under 10: two landings (aaa, bbb), gate median of 20s and 1m45s -> 1m2.5s.
row "bucket under 10 gets a row" "under 10"
assert_has "under 10 counts its two landings" "$ROW" "2"
assert_has "under 10 gate median is the Go-style 1m2.5s" "$ROW" "1m2.5s"
assert_has "under 10 gate max is 1m45s" "$ROW" "1m45s"
assert_has "under 10 lint comes from the one landing that ran it" "$ROW" "10s"

row "bucket 10-20 gets a row" "10-20"
assert_has "10-20 gate is 30s" "$ROW" "30s"
assert_has "10-20 lint is 12s" "$ROW" "12s"

row "bucket 20-30 gets a row" "20-30"
assert_has "20-30 gate is 40s" "$ROW" "40s"

row "bucket 30-45 gets a row" "30-45"
assert_has "30-45 gate is 50s" "$ROW" "50s"

row "bucket over 45 gets a row" "over 45"
assert_has "over 45 gate max is the Go-style 5m2s" "$ROW" "5m2s"
assert_has "over 45 lint is 18s" "$ROW" "18s"

row "the no-load line is its own row" "no load"
assert_has "no load counts the one landing without a suffix" "$ROW" "1"
assert_has "no load still reports its gate" "$ROW" "22s"
assert_has "no load still reports its lint" "$ROW" "11s"

assert_has "totals separate the load and no-load landings" "$OUT" "6 landings under load, 1 with no load reading"

# A bucket with no landing prints no row.
cat >"$TMP/one.log" <<'LOG'
2026/10/03 20:00:00 landing_worker: [land] aaa: stages: lint 10s, gate 20s, om 30s (load1 5.0)
LOG
run --log "$TMP/one.log"
row "the only populated bucket gets a row" "under 10"
assert_lacks "an empty bucket prints no row" "$OUT" "over 45"
assert_has "an empty bucket is not the totals" "$OUT" "1 landings under load, 0 with no load reading"

# --since reads the date on the log line: the 2026/10/02 landing is filtered,
# so the 16m39s gate never reaches a bucket.
cat >"$TMP/dates.log" <<'LOG'
2026/10/02 10:00:00 landing_worker: [land] old: stages: lint 1s, gate 16m39s, om 30s (load1 5.0)
2026/10/03 10:00:00 landing_worker: [land] new: stages: lint 10s, gate 20s, om 30s (load1 5.0)
LOG
run --log "$TMP/dates.log" --since 2026-10-03
assert_rc "--since exits 0" 0
assert_lacks "--since drops the earlier date" "$OUT" "16m39s"
row "--since keeps the later date in its bucket" "under 10"
assert_has "--since leaves one landing" "$OUT" "1 landings under load, 0 with no load reading"

run --log "$TMP/dates.log" --since 2026-10-04
assert_rc "a --since after every landing exits 0" 0
assert_has "a --since after every landing says so" "$OUT" "(no landings with a stages line)"

# Input errors are exit 2, not a traceback.
run --log "$TMP/does-not-exist.log"
assert_rc "a missing log is refused" 2
assert_has "a missing log names itself" "$OUT" "no such log file"

run --log "$TMP/dates.log" --since 2026-10
assert_rc "a malformed --since is refused" 2
assert_has "a malformed --since says the format" "$OUT" "YYYY-MM-DD"

echo
echo "Results: $PASS passed, $FAIL failed"
[[ "$FAIL" -eq 0 ]]
