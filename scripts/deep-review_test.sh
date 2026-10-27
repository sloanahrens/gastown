#!/usr/bin/env bash
# Tests for scripts/deep-review.sh: quiet detection, a quiet verdict that fails
# closed on unreadable input, timestamp ordering, the cursor ledger, the
# risk-ranked rotation slice (score, 21-day exclusion, count and byte caps,
# reviewed lines, cursor) on a production-sized tree, fingerprint lookup, and
# the queued ledger line's severity and its skipped malformed lines, the P3
# batches (threshold, age, filed, cap, seen, skipped malformed lines),
# plan --mode slice|landings|both, the gate subcommand
# (count and age thresholds, the green-sweep gate and the sweep records it
# refuses, both bd --json shapes, untrusted input), the
# docs-audit subcommand (dead paths and commands, code_changed, score order,
# which docs are audited), plan's docs.txt (slice pairing, stale docs, caps)
# and plan's review-targeting battery (called only when enabled, and the slice
# output byte-identical with it on, failing and absent).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
DR="$SCRIPT_DIR/deep-review.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
PASS=0
FAIL=0

assert_eq() {
  local name="$1" expected="$2" actual="$3"
  if [[ "$expected" == "$actual" ]]; then
    echo "  PASS: $name"; PASS=$((PASS + 1))
  else
    echo "  FAIL: $name"; echo "    expected: $expected"; echo "    actual:   $actual"; FAIL=$((FAIL + 1))
  fi
}

# plan_rc runs plan and prints "<exit code>"; stdout goes to $TMP/plan.out
plan_rc() {
  local rc=0
  "$DR" plan "$@" > "$TMP/plan.out" 2> "$TMP/plan.err" || rc=$?
  echo "$rc"
}
field() { grep "^$1=" "$TMP/plan.out" | cut -d= -f2-; }
joined() { tr '\n' ' ' < "$1" | sed 's/ $//'; }

mkrepo() { # dir file... : a git repo whose internal/ holds the given files
  local dir="$1"; shift
  mkdir -p "$dir"
  git -C "$dir" init -q
  git -C "$dir" config user.email t@t; git -C "$dir" config user.name t
  local f
  for f in "$@"; do mkdir -p "$dir/$(dirname "$f")"; echo "package x" > "$dir/$f"; done
  git -C "$dir" add -A; git -C "$dir" commit -q -m init
}

REPO="$TMP/repo"
mkrepo "$REPO" internal/a/one.go internal/a/two.go internal/a/two_test.go internal/b/three.go internal/b/four.go cmd/main.go

# The plan fixtures are the files internal/deepreview/plan_test.go plans from, so
# the shell and the Go rows plan the same bytes rather than two copies.
PLAN_FIXTURES="$SCRIPT_DIR/../internal/deepreview/testdata/plan"
LANDINGS="$PLAN_FIXTURES/ledger.jsonl"
INSTANTS="$PLAN_FIXTURES/instants.jsonl"
: > "$TMP/none.txt"
NOW="2026-10-10T10:00:00Z"

echo "first run: 24h window, risky landings only, torn last line tolerated"
rc="$(plan_rc --landings "$LANDINGS" --prev-notes "$TMP/none.txt" --repo "$REPO" --now "$NOW" --out "$TMP/o1")"
assert_eq "exit 0" "0" "$rc"
assert_eq "not quiet" "false" "$(field quiet)"
assert_eq "two risky landings" "2" "$(field landings)"
assert_eq "torn line counted" "1" "$(field unparsed)"
assert_eq "cursor is the newest landing, fraction kept" "2026-10-10T03:00:00.123456Z" "$(field reviewed_until_new)"
assert_eq "window start" "2026-10-09T10:00:00Z" "$(field reviewed_until_old)"
assert_eq "landings oldest first" "gt-1 gt-3" "$(jq -r .bead "$TMP/o1/landings.jsonl" | tr '\n' ' ' | sed 's/ $//')"
assert_eq "slice skips tests and non-internal; equal scores tie by path, the tested file ranks last" "internal/a/one.go internal/b/four.go internal/b/three.go internal/a/two.go" "$(joined "$TMP/o1/slice.txt")"

echo "ledger cursor: nothing after it is quiet"
printf 'cursor: 2026-10-10T03:00:00.123456Z internal/a/two.go\n' > "$TMP/n2.txt"
rc="$(plan_rc --landings "$LANDINGS" --prev-notes "$TMP/n2.txt" --repo "$REPO" --now "$NOW" --out "$TMP/o2")"
assert_eq "exit 0" "0" "$rc"
assert_eq "quiet" "true" "$(field quiet)"
assert_eq "no slice on a quiet day" "0" "$(field slice_files)"
assert_eq "no slice bytes either" "0" "$(field slice_bytes)"
assert_eq "cursor stays" "2026-10-10T03:00:00.123456Z" "$(field reviewed_until_new)"

echo "ledger cursor: the newest timestamp wins whatever the order; its path no longer picks files"
printf 'cursor: 2026-10-01T00:00:00Z internal/a/one.go\ncursor: 2026-10-10T01:00:00.5Z internal/b/four.go\ncursor: 2026-10-05T00:00:00Z internal/a/two.go\n' > "$TMP/n3.txt"
rc="$(plan_rc --landings "$LANDINGS" --prev-notes "$TMP/n3.txt" --repo "$REPO" --slice 2 --now "$NOW" --out "$TMP/o3")"
assert_eq "one landing after cursor" "1" "$(field landings)"
assert_eq "slice is the top of the risk rank" "internal/a/one.go internal/b/four.go" "$(joined "$TMP/o3/slice.txt")"
assert_eq "old rotation cursor is the newest cursor's path" "internal/b/four.go" "$(field rotation_cursor_old)"
assert_eq "new rotation cursor is the last slice path" "internal/b/four.go" "$(field rotation_cursor_new)"

echo "slice larger than the tree is the whole tree once"
rc="$(plan_rc --landings "$LANDINGS" --prev-notes "$TMP/n3.txt" --repo "$REPO" --slice 50 --now "$NOW" --out "$TMP/o3b")"
assert_eq "four files, no repeats" "internal/a/one.go internal/b/four.go internal/b/three.go internal/a/two.go" "$(joined "$TMP/o3b/slice.txt")"

echo "a cursor path that no longer exists changes nothing"
printf 'cursor: 2026-10-01T00:00:00Z internal/a/gone.go\n' > "$TMP/n3c.txt"
rc="$(plan_rc --landings "$LANDINGS" --prev-notes "$TMP/n3c.txt" --repo "$REPO" --slice 2 --now "$NOW" --out "$TMP/o3c")"
assert_eq "same slice" "internal/a/one.go internal/b/four.go" "$(joined "$TMP/o3c/slice.txt")"
assert_eq "old cursor echoed" "internal/a/gone.go" "$(field rotation_cursor_old)"

echo "timestamps compare as instants, not strings"
printf 'cursor: 2026-10-10T05:00:00Z internal/a/one.go\n' > "$TMP/n5.txt"
rc="$(plan_rc --landings "$INSTANTS" --prev-notes "$TMP/n5.txt" --repo "$REPO" --now "$NOW" --out "$TMP/o5")"
assert_eq "fraction after a whole-second cursor is newer; +02:00 is 04:30Z, before it" "gt-a gt-b" "$(jq -r .bead "$TMP/o5/landings.jsonl" | tr '\n' ' ' | sed 's/ $//')"
assert_eq "newest by instant" "2026-10-10T05:00:00.9Z" "$(field reviewed_until_new)"
printf 'cursor: 2026-10-10T05:00:00.9Z internal/a/one.go\n' > "$TMP/n5b.txt"
rc="$(plan_rc --landings "$INSTANTS" --prev-notes "$TMP/n5b.txt" --repo "$REPO" --now "$NOW" --out "$TMP/o5b")"
assert_eq "a landing at the cursor instant is not reviewed again" "true" "$(field quiet)"

echo "the quiet verdict fails closed"
: > "$TMP/empty.jsonl"
assert_eq "empty landings file" "3" "$(plan_rc --landings "$TMP/empty.jsonl" --prev-notes "$TMP/none.txt" --repo "$REPO" --now "$NOW" --out "$TMP/e1")"
printf '{"bead":"gt-1","landed_ts":"2026-10-10T01:00:00Z","risk_paths":["x"]}\n' > "$TMP/drift.jsonl"
assert_eq "landed_at renamed" "3" "$(plan_rc --landings "$TMP/drift.jsonl" --prev-notes "$TMP/none.txt" --repo "$REPO" --now "$NOW" --out "$TMP/e2")"
{ cat "$INSTANTS"; printf '{"bead":"gt-9","landed_at":"yesterday","risk_paths":["x"]}\n'; } > "$TMP/drift2.jsonl"
assert_eq "one drifted line among good ones" "3" "$(plan_rc --landings "$TMP/drift2.jsonl" --prev-notes "$TMP/none.txt" --repo "$REPO" --now "$NOW" --out "$TMP/e3")"
{ cat "$INSTANTS"; printf '{"bead":"gt-9","landed_at":"2026-10-10T06:00:00Z","risk_paths":"x"}\n'; } > "$TMP/drift3.jsonl"
assert_eq "risk_paths is not a list" "3" "$(plan_rc --landings "$TMP/drift3.jsonl" --prev-notes "$TMP/none.txt" --repo "$REPO" --now "$NOW" --out "$TMP/e4")"
{ printf 'garbage\n'; cat "$INSTANTS"; } > "$TMP/mid.jsonl"
assert_eq "a non-JSON line before the last" "3" "$(plan_rc --landings "$TMP/mid.jsonl" --prev-notes "$TMP/none.txt" --repo "$REPO" --now "$NOW" --out "$TMP/e5")"
printf '[1,2]\n' > "$TMP/arr.jsonl"
assert_eq "a JSON line that is not an object" "3" "$(plan_rc --landings "$TMP/arr.jsonl" --prev-notes "$TMP/none.txt" --repo "$REPO" --now "$NOW" --out "$TMP/e6")"
printf 'cursor: not-a-time internal/a/one.go\n' > "$TMP/badcur.txt"
assert_eq "cursor with a bad timestamp" "3" "$(plan_rc --landings "$LANDINGS" --prev-notes "$TMP/badcur.txt" --repo "$REPO" --now "$NOW" --out "$TMP/e7")"
printf 'cursor: \n' > "$TMP/badcur2.txt"
assert_eq "cursor with no timestamp" "3" "$(plan_rc --landings "$LANDINGS" --prev-notes "$TMP/badcur2.txt" --repo "$REPO" --now "$NOW" --out "$TMP/e8")"
echo "a cursor line without a path (an empty slice wrote it) is read, not refused"
# slice mode always prints rotation_cursor_old=, so an empty path is a present, empty line
oldcur() { grep '^rotation_cursor_old=' "$TMP/plan.out" || echo "(no rotation_cursor_old line)"; }
for spelling in 'cursor: 2026-10-10T03:00:00.5Z \n' 'cursor: 2026-10-10T03:00:00.5Z\n'; do
  # shellcheck disable=SC2059
  printf "$spelling" > "$TMP/nopath.txt"
  rc="$(plan_rc --mode slice --landings "$LANDINGS" --prev-notes "$TMP/nopath.txt" --repo "$REPO" --now "$NOW" --out "$TMP/np")"
  assert_eq "pathless cursor [$spelling] exits 0" "0" "$rc"
  assert_eq "pathless cursor [$spelling] timestamp" "2026-10-10T03:00:00.5Z" "$(field reviewed_until_old)"
  assert_eq "pathless cursor [$spelling] path is empty" "rotation_cursor_old=" "$(oldcur)"
done
printf 'cursor: 2026-10-10T01:00:00Z internal/a/one.go\ncursor: 2026-10-10T03:00:00Z\ncursor: 2026-10-10T02:00:00Z internal/b/two.go\n' > "$TMP/nopath2.txt"
rc="$(plan_rc --mode slice --landings "$LANDINGS" --prev-notes "$TMP/nopath2.txt" --repo "$REPO" --now "$NOW" --out "$TMP/np2")"
assert_eq "the newest cursor wins when it has no path" "2026-10-10T03:00:00Z" "$(field reviewed_until_old)"
assert_eq "and its path is empty, not an older line's" "rotation_cursor_old=" "$(oldcur)"
printf 'cursor: 2026-10-10T01:00:00Z internal/a/one.go\ncursor: 2026-10-10T03:00:00Z\ncursor: 2026-10-10T04:00:00Z internal/b/two.go\n' > "$TMP/nopath3.txt"
rc="$(plan_rc --mode slice --landings "$LANDINGS" --prev-notes "$TMP/nopath3.txt" --repo "$REPO" --now "$NOW" --out "$TMP/np3")"
assert_eq "a newer cursor with a path still wins" "2026-10-10T04:00:00Z|rotation_cursor_old=internal/b/two.go" "$(field reviewed_until_old)|$(oldcur)"
assert_eq "bad --now" "3" "$(plan_rc --landings "$LANDINGS" --prev-notes "$TMP/none.txt" --repo "$REPO" --now tomorrow --out "$TMP/e9")"
mkrepo "$TMP/norepo" cmd/main.go
assert_eq "no Go files under internal/" "3" "$(plan_rc --landings "$LANDINGS" --prev-notes "$TMP/none.txt" --repo "$TMP/norepo" --now "$NOW" --out "$TMP/e10")"
assert_eq "missing landings file is usage" "2" "$(plan_rc --landings "$TMP/absent" --prev-notes "$TMP/none.txt" --repo "$REPO" --now "$NOW" --out "$TMP/e11")"
assert_eq "non-numeric slice is usage" "2" "$(plan_rc --landings "$LANDINGS" --prev-notes "$TMP/none.txt" --repo "$REPO" --slice many --now "$NOW" --out "$TMP/e12")"
assert_eq "flag without a value is usage" "2" "$(plan_rc --landings "$LANDINGS" --prev-notes)"

echo "a tree the size of the real one: no SIGPIPE under pipefail"
BIG="$TMP/big"
mkdir -p "$BIG"
git -C "$BIG" init -q
git -C "$BIG" config user.email t@t; git -C "$BIG" config user.name t
for d in $(seq 1 40); do
  mkdir -p "$BIG/internal/pkg$d"
  for f in $(seq 1 100); do echo "package x" > "$BIG/internal/pkg$d/file$f.go"; done
done
git -C "$BIG" add -A; git -C "$BIG" commit -q -m big
rc="$(plan_rc --landings "$LANDINGS" --prev-notes "$TMP/none.txt" --repo "$BIG" --slice 15 --now "$NOW" --out "$TMP/o6")"
assert_eq "exit 0" "0" "$rc"
assert_eq "slice of 15" "15" "$(field slice_files)"
assert_eq "all scores tie, so the slice starts at the first path in byte order" "internal/pkg1/file1.go" "$(head -n 1 "$TMP/o6/slice.txt")"
assert_eq "every file is ranked" "4000" "$(awk 'END { print NR }' "$TMP/o6/ranked.txt")"
rc="$(plan_rc --landings "$LANDINGS" --prev-notes "$TMP/none.txt" --repo "$BIG" --slice 3 --now "$NOW" --out "$TMP/o7")"
assert_eq "byte order, not numeric" "internal/pkg1/file1.go internal/pkg1/file10.go internal/pkg1/file100.go" "$(joined "$TMP/o7/slice.txt")"

echo "the --ref tree is read, not the working tree"
echo "package x" > "$REPO/internal/a/untracked.go"
git -C "$REPO" add -A; git -C "$REPO" commit -q -m more
rc="$(plan_rc --landings "$LANDINGS" --prev-notes "$TMP/none.txt" --repo "$REPO" --ref HEAD~1 --slice 50 --now "$NOW" --out "$TMP/o8")"
assert_eq "file added after the ref is absent" "0" "$(grep -c untracked "$TMP/o8/slice.txt" || true)"

echo "risk ranking: fix commits, churn, size and a missing sibling test, from 60 days of history"
RISK="$TMP/risk"
mkdir -p "$RISK"
git -C "$RISK" init -q
git -C "$RISK" config user.email t@t; git -C "$RISK" config user.name t
gen() { # file lines : "package x" plus comment lines up to the given length
  mkdir -p "$(dirname "$RISK/$1")"
  { echo "package x"; if [ "$2" -gt 1 ]; then seq 2 "$2" | sed 's|^|// |'; fi; } > "$RISK/$1"
}
bump() { echo "// edit" >> "$RISK/$1"; }
cm() { # message [date] : commit the working tree, optionally at a fixed date
  git -C "$RISK" add -A
  if [ -n "${2:-}" ]; then
    GIT_AUTHOR_DATE="$2" GIT_COMMITTER_DATE="$2" git -C "$RISK" commit -q -m "$1"
  else
    git -C "$RISK" commit -q -m "$1"
  fi
}
gen internal/r/stale.go 1; cm "fix: old one" 2020-01-01T00:00:00Z
for i in 2 3 4; do bump internal/r/stale.go; cm "fix: old $i" 2020-01-0${i}T00:00:00Z; done
gen internal/r/fixed.go 1; cm "fix: create"
bump internal/r/fixed.go; cm "fix(scope): second"
bump internal/r/fixed.go; cm "Fix: third"
gen internal/r/busy.go 1; cm "feat: a"
for m in "prefix: b" "refix: c" "refactor: d" "feat: e"; do bump internal/r/busy.go; cm "$m"; done
gen internal/r/large.go 1200; cm "feat: large"
gen internal/r/mid.go 400; cm "feat: mid"
gen internal/r/plain.go 1; cm "feat: plain"
gen internal/r/tested.go 1; gen internal/r/tested_test.go 1; cm "feat: tested"
sz() { wc -c < "$RISK/$1" | tr -d ' '; }
score() { awk -F'\t' -v p="$2" '$5 == p { print $3 }' "$1/ranked.txt"; }
R="internal/r"
rc="$(plan_rc --landings "$LANDINGS" --prev-notes "$TMP/none.txt" --repo "$RISK" --slice 50 --now "$NOW" --out "$TMP/r1")"
assert_eq "exit 0" "0" "$rc"
assert_eq "highest score first; old fixes outside the window add nothing" \
  "$R/fixed.go $R/large.go $R/busy.go $R/mid.go $R/plain.go $R/stale.go $R/tested.go" "$(joined "$TMP/r1/slice.txt")"
assert_eq "3 fix commits (any case, with scope) + 3 commits + 0.015 + no test" "16.0150" "$(score "$TMP/r1" $R/fixed.go)"
assert_eq "prefix:, refix: and refactor: are not fixes" "9.0250" "$(score "$TMP/r1" $R/busy.go)"
assert_eq "lines/200 caps at 5" "10.0000" "$(score "$TMP/r1" $R/large.go)"
assert_eq "400 lines is 2" "7.0000" "$(score "$TMP/r1" $R/mid.go)"
assert_eq "a sibling test drops the 4" "1.0050" "$(score "$TMP/r1" $R/tested.go)"
assert_eq "history older than 60 days counts nothing" "4.0200" "$(score "$TMP/r1" $R/stale.go)"
assert_eq "test files are not ranked" "0" "$(grep -c '_test.go' "$TMP/r1/ranked.txt" || true)"

echo "reviewed within 21 days of --now are skipped; the newest date per path wins"
cat > "$TMP/rv1.txt" <<'N'
reviewed: 2026-10-05 internal/r/fixed.go
reviewed: 2026-09-19 internal/r/large.go
reviewed: 2026-09-18 internal/r/busy.go
reviewed: 2026-10-09 internal/r/mid.go
reviewed: 2026-08-01 internal/r/mid.go
reviewed: yesterday internal/r/plain.go
finding: x gt-1 reviewed: 2026-10-09 internal/r/stale.go
reviewed: 2026-10-09 internal/r/gone.go
N
rc="$(plan_rc --landings "$LANDINGS" --prev-notes "$TMP/rv1.txt" --repo "$RISK" --slice 50 --now "$NOW" --out "$TMP/r2")"
assert_eq "exit 0" "0" "$rc"
assert_eq "21 days exactly is skipped, 22 ranks again, malformed and mid-line text ignored" \
  "$R/busy.go $R/plain.go $R/stale.go $R/tested.go" "$(joined "$TMP/r2/slice.txt")"
printf '{"bead":"gt-5","head":"h5","landed_commit":"c5","landed_at":"2026-10-11T23:00:00Z","risk_paths":["internal/z.go"]}\n' > "$TMP/late.jsonl"
rc="$(plan_rc --landings "$TMP/late.jsonl" --prev-notes "$TMP/rv1.txt" --repo "$RISK" --slice 50 --now 2026-10-12T00:00:00Z --out "$TMP/r2b")"
assert_eq "a later --now ages the 09-19 review out of the window" \
  "$R/large.go $R/busy.go $R/plain.go $R/stale.go $R/tested.go" "$(joined "$TMP/r2b/slice.txt")"
cat > "$TMP/rv2.txt" <<'N'
reviewed: 2026-10-01 internal/r/tested.go
reviewed: 2026-10-07 internal/r/fixed.go
reviewed: 2026-10-06 internal/r/large.go
reviewed: 2026-10-04 internal/r/busy.go
reviewed: 2026-10-05 internal/r/mid.go
reviewed: 2026-10-03 internal/r/plain.go
reviewed: 2026-10-03 internal/r/stale.go
N
rc="$(plan_rc --landings "$LANDINGS" --prev-notes "$TMP/rv2.txt" --repo "$RISK" --slice 50 --now "$NOW" --out "$TMP/r3")"
assert_eq "every file reviewed: least recently reviewed first, equal dates by score" \
  "$R/tested.go $R/plain.go $R/stale.go $R/busy.go $R/mid.go $R/large.go $R/fixed.go" "$(joined "$TMP/r3/slice.txt")"
grep -v tested.go "$TMP/rv2.txt" > "$TMP/rv3.txt"
rc="$(plan_rc --landings "$LANDINGS" --prev-notes "$TMP/rv3.txt" --repo "$RISK" --slice 50 --now "$NOW" --out "$TMP/r4")"
assert_eq "one candidate left: only it, no fallback fill" "$R/tested.go" "$(joined "$TMP/r4/slice.txt")"

echo "the file-count and byte caps: whichever is hit first ends the slice"
SF="$(sz $R/fixed.go)"; SL="$(sz $R/large.go)"; SB="$(sz $R/busy.go)"
run_caps() { # name slice bytes
  plan_rc --landings "$LANDINGS" --prev-notes "$TMP/none.txt" --repo "$RISK" --slice "$2" --slice-bytes "$3" --now "$NOW" --out "$TMP/$1" > /dev/null
}
run_caps c1 50 $((SF + SL + SB))
assert_eq "budget fits three exactly" "$R/fixed.go $R/large.go $R/busy.go" "$(joined "$TMP/c1/slice.txt")"
assert_eq "slice_bytes is their sum" "$((SF + SL + SB))" "$(field slice_bytes)"
assert_eq "slice_files beside it" "3" "$(field slice_files)"
run_caps c2 50 $((SF + SL + SB - 1))
assert_eq "one byte short drops the third" "$R/fixed.go $R/large.go" "$(joined "$TMP/c2/slice.txt")"
assert_eq "slice_bytes" "$((SF + SL))" "$(field slice_bytes)"
run_caps c3 50 $((SF + SB))
assert_eq "the first file that does not fit ends the slice; smaller ones behind it are not pulled in" "$R/fixed.go" "$(joined "$TMP/c3/slice.txt")"
run_caps c4 50 1
assert_eq "the first file is kept even when it exceeds the budget" "$R/fixed.go" "$(joined "$TMP/c4/slice.txt")"
assert_eq "slice_bytes reports the overshoot" "$SF" "$(field slice_bytes)"
run_caps c5 2 1000000
assert_eq "count cap hit first" "$R/fixed.go $R/large.go" "$(joined "$TMP/c5/slice.txt")"
run_caps c6 3 $((SF + SL))
assert_eq "byte cap hit first" "$R/fixed.go $R/large.go" "$(joined "$TMP/c6/slice.txt")"
DEFB="$TMP/defb"
mkdir -p "$DEFB/internal"
git -C "$DEFB" init -q
git -C "$DEFB" config user.email t@t; git -C "$DEFB" config user.name t
for f in a b c; do awk 'BEGIN { for (i = 0; i < 6000; i++) print "123456789" }' > "$DEFB/internal/$f.go"; done
git -C "$DEFB" add -A; git -C "$DEFB" commit -q -m big
rc="$(plan_rc --landings "$LANDINGS" --prev-notes "$TMP/none.txt" --repo "$DEFB" --now "$NOW" --out "$TMP/c7")"
assert_eq "default budget is 150000 bytes: two 60000-byte files fit, three do not" "internal/a.go internal/b.go" "$(joined "$TMP/c7/slice.txt")"
assert_eq "slice_bytes" "120000" "$(field slice_bytes)"
rc="$(plan_rc --landings "$LANDINGS" --prev-notes "$TMP/none.txt" --repo "$DEFB" --slice-bytes 180000 --now "$NOW" --out "$TMP/c8")"
assert_eq "--slice-bytes raises it" "3" "$(field slice_files)"
assert_eq "slice-bytes 0 is usage" "2" "$(plan_rc --landings "$LANDINGS" --prev-notes "$TMP/none.txt" --repo "$RISK" --slice-bytes 0 --now "$NOW" --out "$TMP/c9")"
assert_eq "non-numeric slice-bytes is usage" "2" "$(plan_rc --landings "$LANDINGS" --prev-notes "$TMP/none.txt" --repo "$RISK" --slice-bytes lots --now "$NOW" --out "$TMP/c10")"

echo "reviewed-lines.txt and the rotation cursor feed the next run"
rc="$(plan_rc --landings "$LANDINGS" --prev-notes "$TMP/none.txt" --repo "$RISK" --slice 2 --now "$NOW" --out "$TMP/l1")"
assert_eq "one reviewed: line per slice file, dated --now's day, in slice order" \
  "reviewed: 2026-10-10 $R/fixed.go|reviewed: 2026-10-10 $R/large.go" "$(paste -sd'|' "$TMP/l1/reviewed-lines.txt")"
assert_eq "cursor old is empty on a first run" "" "$(field rotation_cursor_old)"
assert_eq "cursor new is the last slice path" "$R/large.go" "$(field rotation_cursor_new)"
{ printf 'cursor: %s %s\n' "$(field reviewed_until_new)" "$(field rotation_cursor_new)"; cat "$TMP/l1/reviewed-lines.txt"; } > "$TMP/l1-notes.txt"
sed '$d' "$LANDINGS" > "$TMP/landings2.jsonl"
printf '{"bead":"gt-4","head":"h4","landed_commit":"c4","landed_at":"2026-10-10T04:00:00Z","risk_paths":["internal/z.go"]}\n' >> "$TMP/landings2.jsonl"
rc="$(plan_rc --landings "$TMP/landings2.jsonl" --prev-notes "$TMP/l1-notes.txt" --repo "$RISK" --slice 2 --now "$NOW" --out "$TMP/l2")"
assert_eq "the written cursor line is read back as the old cursor" "$R/large.go" "$(field rotation_cursor_old)"
assert_eq "the written reviewed: lines rotate the slice onward" "$R/busy.go $R/mid.go" "$(joined "$TMP/l2/slice.txt")"
assert_eq "the landing window starts at the written cursor" "2026-10-10T03:00:00.123456Z" "$(field reviewed_until_old)"
rc="$(plan_rc --landings "$TMP/late.jsonl" --prev-notes "$TMP/none.txt" --repo "$RISK" --slice 2 --now 2026-10-10T23:30:00-05:00 --out "$TMP/l3")"
assert_eq "the date is the UTC day of the instant" "reviewed: 2026-10-11 $R/fixed.go" "$(head -n 1 "$TMP/l3/reviewed-lines.txt")"
assert_eq "a quiet day writes an empty reviewed-lines.txt" "0" "$(wc -c < "$TMP/o2/reviewed-lines.txt" | tr -d ' ')"
assert_eq "and slice_bytes=0" "0" "$(awk -F'\t' '{ s += $4 } END { print s + 0 }' "$TMP/o2/slice-rows.txt")"

echo "seen: exact fingerprint match against finding and queued lines"
cat > "$TMP/n4.txt" <<'N'
finding: daemon.go:Foo:swallowed-error gt-9
finding: other digest-only
queued: queued.go:Bar:race P2 queued.go:10 finding: spoof.go:Baz:x gt-1
N
seen() { if "$DR" seen --prev-notes "$TMP/n4.txt" "$1"; then echo yes; else echo no; fi; }
assert_eq "filed fingerprint" "yes" "$(seen daemon.go:Foo:swallowed-error)"
assert_eq "prefix is not a match" "no" "$(seen daemon.go:Foo)"
assert_eq "queued fingerprint" "yes" "$(seen queued.go:Bar:race)"
assert_eq "text inside a claim is not a ledger line" "no" "$(seen spoof.go:Baz:x)"
assert_eq "regex characters are literal" "no" "$(seen 'daemon.go:Foo:.*')"

echo "queued: recorded but never filed"
cat > "$TMP/n9.txt" <<'N'
queued: a.go:A:x P1 a.go:1 first claim
queued: b.go:B:y P2 b.go:2 second claim
finding: b.go:B:y gt-77
queued: a.go:A:x P1 a.go:1 first claim again
queued: c.go:C:z P2 c.go:3 third
N
assert_eq "filed ones and repeats drop out" "queued: a.go:A:x P1 a.go:1 first claim|queued: c.go:C:z P2 c.go:3 third" "$("$DR" queued --prev-notes "$TMP/n9.txt" | paste -sd'|' -)"
assert_eq "none queued" "" "$("$DR" queued --prev-notes "$TMP/none.txt")"

echo "queued: P3 is a severity, a malformed line is skipped with a warning"
printf 'queued: a.go:A:x P3 a.go:1 low claim\n' > "$TMP/q3.txt"
assert_eq "P3 carried through" "queued: a.go:A:x P3 a.go:1 low claim" "$("$DR" queued --prev-notes "$TMP/q3.txt")"
queued_rc() { local rc=0; "$DR" queued --prev-notes "$1" > "$TMP/queued.out" 2> "$TMP/queued.err" || rc=$?; echo "$rc"; }
cat > "$TMP/qbad1.txt" <<'N'
queued: a.go:A:x P1 a.go:1 good claim
queued: b.go:B:y a.go:2 no severity
queued: c.go:C:z P4 a.go:3 unknown severity
N
assert_eq "one bad line among good ones exits 0" "0" "$(queued_rc "$TMP/qbad1.txt")"
assert_eq "the good line is still listed" "queued: a.go:A:x P1 a.go:1 good claim" "$(paste -sd'|' - < "$TMP/queued.out")"
assert_eq "the warning names the bad line" "1" "$(grep -c 'malformed queued line: queued: b.go:B:y a.go:2 no severity' "$TMP/queued.err" || true)"
assert_eq "and the count names how many were skipped" "1" "$(grep -c 'skipped 2 malformed queued lines' "$TMP/queued.err" || true)"
printf 'queued: a.go:A:x P2 a.go:1\n' > "$TMP/qbad2.txt"
assert_eq "claim missing" "0" "$(queued_rc "$TMP/qbad2.txt")"
assert_eq "a lone bad line takes the singular count" "1" "$(grep -c 'skipped 1 malformed queued line; the ledger cannot be trusted' "$TMP/queued.err" || true)"
printf 'queued: a.go:A:x a.go:1 no severity\nqueued: b.go:B:y P4 a.go:2 unknown\n' > "$TMP/qbad3.txt"
assert_eq "an all-bad ledger exits 0" "0" "$(queued_rc "$TMP/qbad3.txt")"
assert_eq "with nothing on stdout" "0" "$(wc -c < "$TMP/queued.out" | tr -d ' ')"
assert_eq "and a count the run can report" "1" "$(grep -c 'skipped 2 malformed queued lines; the ledger cannot be trusted' "$TMP/queued.err" || true)"

echo "batches: a package is listed at MIN unfiled P3 lines or when its oldest is MAX_AGE days old"
batches() { "$DR" batches --prev-notes "$1" --now "$NOW" "${@:2}" | paste -sd'|' -; }
batches_rc() { local rc=0; "$DR" batches --prev-notes "$1" --now "$NOW" "${@:2}" > "$TMP/batches.out" 2> "$TMP/batches.err" || rc=$?; echo "$rc"; }
cat > "$TMP/b1.txt" <<'N'
p3: 2026-10-10 internal/daemon/a.go:A:x internal/daemon/a.go:1 claim a
p3: 2026-10-10 internal/daemon/b.go:B:y internal/daemon/b.go:2 claim b
p3: 2026-10-10 internal/daemon/c.go:C:z internal/daemon/c.go:3 claim c
p3: 2026-10-10 internal/land/d.go:D:x internal/land/d.go:4 claim d
p3: 2026-10-10 internal/land/e.go:E:x internal/land/e.go:5 claim e
N
assert_eq "three fresh lines in one package are a batch; two are not" \
  "batch internal/daemon|p3: 2026-10-10 internal/daemon/a.go:A:x internal/daemon/a.go:1 claim a|p3: 2026-10-10 internal/daemon/b.go:B:y internal/daemon/b.go:2 claim b|p3: 2026-10-10 internal/daemon/c.go:C:z internal/daemon/c.go:3 claim c" "$(batches "$TMP/b1.txt")"
assert_eq "--min 2 lists both packages" "batch internal/daemon|batch internal/land" "$("$DR" batches --prev-notes "$TMP/b1.txt" --now "$NOW" --min 2 | grep '^batch ' | paste -sd'|' -)"
assert_eq "--min 2: five p3 lines under the two headers" "7" "$("$DR" batches --prev-notes "$TMP/b1.txt" --now "$NOW" --min 2 | awk 'END { print NR }')"
assert_eq "--min 4 lists none" "" "$(batches "$TMP/b1.txt" --min 4)"
assert_eq "no p3 lines, nothing" "" "$(batches "$TMP/none.txt")"
assert_eq "unrelated ledger lines are ignored" "" "$(batches "$TMP/n9.txt")"
cat > "$TMP/b2.txt" <<'N'
p3: 2026-10-03 internal/old/a.go:A:x internal/old/a.go:1 seven days
p3: 2026-10-04 internal/six/b.go:B:y internal/six/b.go:2 six days
N
assert_eq "oldest exactly 7 days old lists a lone line; 6 days does not" \
  "batch internal/old|p3: 2026-10-03 internal/old/a.go:A:x internal/old/a.go:1 seven days" "$(batches "$TMP/b2.txt")"
assert_eq "--max-age-days 6 lists both" \
  "batch internal/old|p3: 2026-10-03 internal/old/a.go:A:x internal/old/a.go:1 seven days|batch internal/six|p3: 2026-10-04 internal/six/b.go:B:y internal/six/b.go:2 six days" "$(batches "$TMP/b2.txt" --max-age-days 6)"
assert_eq "--max-age-days 8 lists none" "" "$(batches "$TMP/b2.txt" --max-age-days 8)"
assert_eq "a later --now ages the six-day line" \
  "batch internal/old|p3: 2026-10-03 internal/old/a.go:A:x internal/old/a.go:1 seven days|batch internal/six|p3: 2026-10-04 internal/six/b.go:B:y internal/six/b.go:2 six days" \
  "$("$DR" batches --prev-notes "$TMP/b2.txt" --now 2026-10-11T00:00:00Z | paste -sd'|' -)"
printf 'p3: 2026-10-03 main.go:M:x main.go:1 file at the repo root\n' > "$TMP/b2r.txt"
assert_eq "a file at the repo root is package ." "batch .|p3: 2026-10-03 main.go:M:x main.go:1 file at the repo root" "$(batches "$TMP/b2r.txt")"

echo "batches: a finding line files a P3, which is then no longer listed"
cp "$TMP/b1.txt" "$TMP/b3.txt"
printf 'finding: internal/daemon/b.go:B:y gt-55\n' >> "$TMP/b3.txt"
assert_eq "two left in the package is below the threshold" "" "$(batches "$TMP/b3.txt")"
printf 'finding: internal/land/d.go:D:x digest-only\nfinding: nothing:here:x gt-1\n' >> "$TMP/b3.txt"
assert_eq "digest-only also names it; an unrelated finding changes nothing" "batch internal/daemon" "$("$DR" batches --prev-notes "$TMP/b3.txt" --now "$NOW" --min 2 | grep '^batch ' | paste -sd'|' -)"
printf 'p3: 2026-10-10 internal/daemon/f.go:F:x internal/daemon/f.go:6 claim f\n' >> "$TMP/b3.txt"
assert_eq "a new one restores the count without the filed one" \
  "batch internal/daemon|p3: 2026-10-10 internal/daemon/a.go:A:x internal/daemon/a.go:1 claim a|p3: 2026-10-10 internal/daemon/c.go:C:z internal/daemon/c.go:3 claim c|p3: 2026-10-10 internal/daemon/f.go:F:x internal/daemon/f.go:6 claim f" "$(batches "$TMP/b3.txt")"
cat > "$TMP/b4.txt" <<'N'
p3: 2026-10-10 internal/q/a.go:A:x internal/q/a.go:1 mentions finding: internal/q/b.go:B:y gt-1 in its claim
p3: 2026-10-10 internal/q/b.go:B:y internal/q/b.go:2 claim b
p3: 2026-10-10 internal/q/c.go:C:z internal/q/c.go:3 claim c
finding: internal/q/a.go:A:xx gt-2
N
assert_eq "text inside a claim and a longer fingerprint do not file anything" "3" "$(batches "$TMP/b4.txt" | awk -F'|' '{ print NF - 1 }')"
cat > "$TMP/b5.txt" <<'N'
p3: 2026-10-10 internal/q/a.go:A:x internal/q/a.go:1 first
p3: 2026-10-08 internal/q/a.go:A:x internal/q/a.go:1 older record
p3: 2026-10-10 internal/q/a.go:A:x internal/q/a.go:1 third
p3: 2026-10-10 internal/q/b.go:B:y internal/q/b.go:2 claim b
N
assert_eq "a repeated fingerprint counts once, at its oldest line, so the package stays below 3" "" "$(batches "$TMP/b5.txt")"
assert_eq "and below the age line" "batch internal/q|p3: 2026-10-08 internal/q/a.go:A:x internal/q/a.go:1 older record|p3: 2026-10-10 internal/q/b.go:B:y internal/q/b.go:2 claim b" "$(batches "$TMP/b5.txt" --max-age-days 2)"

echo "batches: at most MAX_PER lines per batch, oldest first; the rest wait"
{ for i in 1 2 3 4 5 6 7 8; do printf 'p3: 2026-10-0%d internal/cap/f%d.go:F:x internal/cap/f%d.go:%d claim %d\n' "$((10 - i))" "$i" "$i" "$i" "$i"; done; } > "$TMP/b6.txt"
assert_eq "default cap is 6, oldest dates first (f8 is the oldest, f1 and f2 wait)" \
  "batch internal/cap|f8 f7 f6 f5 f4 f3" "$("$DR" batches --prev-notes "$TMP/b6.txt" --now "$NOW" | awk 'NR == 1 { printf "%s|", $0; next } { split($3, a, ":"); sub(/.*\//, "", a[1]); sub(/\.go$/, "", a[1]); printf "%s%s", sep, a[1]; sep = " " }')"
assert_eq "--max-per-batch 2" "3" "$("$DR" batches --prev-notes "$TMP/b6.txt" --now "$NOW" --max-per-batch 2 | awk 'END { print NR }')"
assert_eq "--max-per-batch 9 lists all eight" "9" "$("$DR" batches --prev-notes "$TMP/b6.txt" --now "$NOW" --max-per-batch 9 | awk 'END { print NR }')"
{ cat "$TMP/b6.txt"; printf 'finding: internal/cap/f8.go:F:x gt-1\n'; } > "$TMP/b6b.txt"
assert_eq "a filed line frees its slot for the one that waited" "1" "$("$DR" batches --prev-notes "$TMP/b6b.txt" --now "$NOW" | grep -c 'f2.go:F:x internal/cap/f2.go:2' || true)"
{ cat "$TMP/b1.txt"; for i in 1 2 3; do printf 'p3: 2026-10-10 cmd/gt/f%d.go:F:x cmd/gt/f%d.go:%d claim\n' "$i" "$i" "$i"; done; } > "$TMP/b7.txt"
assert_eq "blocks come out in directory order" "batch cmd/gt|batch internal/daemon" "$("$DR" batches --prev-notes "$TMP/b7.txt" --now "$NOW" | grep '^batch ' | paste -sd'|' -)"

echo "seen: a fingerprint on a p3 line is seen"
printf 'p3: 2026-10-10 p3.go:P:x p3.go:1 claim finding: inner.go:I:x gt-1\n' > "$TMP/n10.txt"
seen_p3() { if "$DR" seen --prev-notes "$TMP/n10.txt" "$1"; then echo yes; else echo no; fi; }
assert_eq "p3 fingerprint" "yes" "$(seen_p3 p3.go:P:x)"
assert_eq "prefix of it" "no" "$(seen_p3 p3.go:P)"
assert_eq "the file:line field" "no" "$(seen_p3 p3.go:1)"
assert_eq "the date" "no" "$(seen_p3 2026-10-10)"
assert_eq "text inside the claim" "no" "$(seen_p3 inner.go:I:x)"

echo "batches: a malformed p3 line is skipped with a warning; the rest is listed"
for bad in 'p3: 2026-10-10 a/b.go:B:x a/b.go:1' 'p3: yesterday a/b.go:B:x a/b.go:1 claim' 'p3: 2026-10-10 a/b.go a/b.go:1 claim' 'p3: 2026-10-10 a/b.go:B a/b.go:1 claim' 'p3: 2026-10-10' 'p3: 2026-10-10 :B:x a/b.go:1 claim'; do
  { cat "$TMP/b1.txt"; printf '%s\n' "$bad"; } > "$TMP/bbad.txt"
  assert_eq "exit 0: $bad" "0" "$(batches_rc "$TMP/bbad.txt")"
  assert_eq "the rest of the ledger is unchanged: $bad" "$(batches "$TMP/b1.txt")" "$(paste -sd'|' - < "$TMP/batches.out")"
done
assert_eq "the warning names the line" "1" "$(grep -c 'malformed p3 line: p3: 2026-10-10 :B:x a/b.go:1 claim' "$TMP/batches.err" || true)"
assert_eq "with the count of skipped lines" "1" "$(grep -c 'skipped 1 malformed p3 line; the ledger cannot be trusted' "$TMP/batches.err" || true)"
printf 'p3: 2026-10-10\np3: 2026-10-10 a/b.go:B a/b.go:1 claim\n' > "$TMP/ballbad.txt"
assert_eq "an all-bad ledger exits 0" "0" "$(batches_rc "$TMP/ballbad.txt")"
assert_eq "prints no rows" "0" "$(wc -c < "$TMP/batches.out" | tr -d ' ')"
assert_eq "and a count the run can report" "1" "$(grep -c 'skipped 2 malformed p3 lines; the ledger cannot be trusted' "$TMP/batches.err" || true)"
assert_eq "bad --now" "3" "$(batches_rc "$TMP/b1.txt" --now tomorrow)"
assert_eq "missing notes file is usage" "2" "$("$DR" batches --prev-notes "$TMP/absent" > /dev/null 2>&1 || echo $?)"
assert_eq "no --prev-notes is usage" "2" "$("$DR" batches > /dev/null 2>&1 || echo $?)"
assert_eq "--min 0 is usage" "2" "$(batches_rc "$TMP/b1.txt" --min 0)"
assert_eq "--max-per-batch 0 is usage" "2" "$(batches_rc "$TMP/b1.txt" --max-per-batch 0)"
assert_eq "non-numeric --max-age-days is usage" "2" "$(batches_rc "$TMP/b1.txt" --max-age-days soon)"
assert_eq "unknown flag is usage" "2" "$(batches_rc "$TMP/b1.txt" --bogus 1)"

# earlier tests dirtied $REPO, so the mode and gate tests run on a clean one
REPO="$TMP/mrepo"
mkrepo "$REPO" internal/a/one.go internal/a/two.go internal/a/two_test.go internal/b/three.go internal/b/four.go cmd/main.go

echo "plan --mode slice: only the slice, never the landings, no landing cursor moves"
printf 'cursor: 2026-10-10T03:00:00.123456Z internal/a/two.go\n' > "$TMP/n20.txt"
rc="$(plan_rc --mode slice --prev-notes "$TMP/n20.txt" --repo "$REPO" --slice 2 --now "$NOW" --out "$TMP/m1")"
assert_eq "exit 0 without a landings file" "0" "$rc"
assert_eq "mode echoed" "slice" "$(field mode)"
assert_eq "a slice is never a quiet day" "false" "$(field quiet)"
assert_eq "no landings" "0" "$(field landings)"
assert_eq "landings file empty" "0" "$(wc -c < "$TMP/m1/landings.jsonl" | tr -d ' ')"
assert_eq "reviewed_until_old is the cursor" "2026-10-10T03:00:00.123456Z" "$(field reviewed_until_old)"
assert_eq "reviewed_until_new is unchanged" "2026-10-10T03:00:00.123456Z" "$(field reviewed_until_new)"
assert_eq "the slice is the risk-ranked top" "internal/a/one.go internal/b/four.go" "$(joined "$TMP/m1/slice.txt")"
assert_eq "slice_files" "2" "$(field slice_files)"
assert_eq "reviewed lines written" "2" "$(awk 'END { print NR }' "$TMP/m1/reviewed-lines.txt")"
assert_eq "rotation cursor echoed" "internal/b/four.go" "$(field rotation_cursor_new)"
rc="$(plan_rc --mode slice --prev-notes "$TMP/none.txt" --repo "$REPO" --now "$NOW" --out "$TMP/m2")"
assert_eq "no cursor: both ends are the window start" "2026-10-09T10:00:00Z/2026-10-09T10:00:00Z" "$(field reviewed_until_old)/$(field reviewed_until_new)"
rc="$(plan_rc --mode slice --landings "$TMP/drift.jsonl" --prev-notes "$TMP/none.txt" --repo "$REPO" --now "$NOW" --out "$TMP/m3")"
assert_eq "a drifted landings file is not read by the slice job" "0" "$rc"
assert_eq "slice mode needs the repo" "2" "$(plan_rc --mode slice --prev-notes "$TMP/none.txt" --now "$NOW" --out "$TMP/m4")"
assert_eq "bad cursor still stops the slice job" "3" "$(plan_rc --mode slice --prev-notes "$TMP/badcur.txt" --repo "$REPO" --now "$NOW" --out "$TMP/m5")"

echo "plan --mode landings: only the risky landings, an empty slice"
rc="$(plan_rc --mode landings --landings "$LANDINGS" --prev-notes "$TMP/none.txt" --now "$NOW" --out "$TMP/m6")"
assert_eq "exit 0 without a repo" "0" "$rc"
assert_eq "mode echoed" "landings" "$(field mode)"
assert_eq "not quiet" "false" "$(field quiet)"
assert_eq "two risky landings" "2" "$(field landings)"
assert_eq "landings oldest first" "gt-1 gt-3" "$(jq -r .bead "$TMP/m6/landings.jsonl" | tr '\n' ' ' | sed 's/ $//')"
assert_eq "cursor moves to the newest landing" "2026-10-10T03:00:00.123456Z" "$(field reviewed_until_new)"
assert_eq "empty slice" "0" "$(field slice_files)"
assert_eq "no slice bytes" "0" "$(field slice_bytes)"
assert_eq "slice file empty" "0" "$(wc -c < "$TMP/m6/slice.txt" | tr -d ' ')"
assert_eq "no rotation cursor" "" "$(grep '^rotation_cursor' "$TMP/plan.out" || true)"
rc="$(plan_rc --mode landings --landings "$LANDINGS" --prev-notes "$TMP/n2.txt" --now "$NOW" --out "$TMP/m7")"
assert_eq "nothing after the cursor is quiet" "true" "$(field quiet)"
assert_eq "cursor stays" "2026-10-10T03:00:00.123456Z" "$(field reviewed_until_new)"
assert_eq "drifted landings fail closed" "3" "$(plan_rc --mode landings --landings "$TMP/drift.jsonl" --prev-notes "$TMP/none.txt" --now "$NOW" --out "$TMP/m8")"
assert_eq "landings mode needs the landings file" "2" "$(plan_rc --mode landings --prev-notes "$TMP/none.txt" --now "$NOW" --out "$TMP/m9")"

echo "plan --mode both is the default"
rc="$(plan_rc --mode both --landings "$LANDINGS" --prev-notes "$TMP/n3.txt" --repo "$REPO" --slice 2 --now "$NOW" --out "$TMP/m10")"
assert_eq "same slice as the default" "internal/a/one.go internal/b/four.go" "$(joined "$TMP/m10/slice.txt")"
assert_eq "one landing" "1" "$(field landings)"
rc="$(plan_rc --mode both --landings "$LANDINGS" --prev-notes "$TMP/n2.txt" --repo "$REPO" --now "$NOW" --out "$TMP/m11")"
assert_eq "both mode still skips the slice on a quiet day" "true/0" "$(field quiet)/$(field slice_files)"
assert_eq "unknown mode is usage" "2" "$(plan_rc --mode hourly --landings "$LANDINGS" --prev-notes "$TMP/none.txt" --repo "$REPO" --now "$NOW" --out "$TMP/m12")"

echo "gate: a stub bd feeds the run history"
mkdir -p "$TMP/bin" "$TMP/rig"
cat > "$TMP/bin/bd" <<'STUB'
#!/usr/bin/env bash
echo "$(pwd -P) $*" > "$BD_STUB_ARGS"
cat "$BD_STUB_OUT"
exit "${BD_STUB_RC:-0}"
STUB
chmod +x "$TMP/bin/bd"
export BD_STUB_ARGS="$TMP/bd.args" BD_STUB_OUT="$TMP/bd.out"
gate_rc() {
  local rc=0
  PATH="$TMP/bin:$PATH" "$DR" gate "$@" > "$TMP/gate.out" 2> "$TMP/gate.err" || rc=$?
  echo "$rc"
}
gate_out() { cat "$TMP/gate.out"; }
LBL="scheduled:deep-review"
echo '[]' > "$BD_STUB_OUT"

echo "gate: no history, two landings in the 24h window, the oldest is old enough"
assert_eq "exit 0 (run)" "0" "$(gate_rc --landings "$LANDINGS" --label "$LBL" --now "$NOW")"
assert_eq "output" "gate: run n=2 oldest=539m" "$(gate_out)"
assert_eq "bd is asked for the label's whole history" "list -l scheduled:deep-review --all --limit 60 --json" "$(cut -d' ' -f2- "$BD_STUB_ARGS")"
assert_eq "too few and too young: skip" "1" "$(gate_rc --landings "$LANDINGS" --label "$LBL" --now "$NOW" --min-age-min 540)"
assert_eq "skip output" "gate: skip n=2 oldest=539m" "$(gate_out)"
assert_eq "enough of them is enough" "0" "$(gate_rc --landings "$LANDINGS" --label "$LBL" --now "$NOW" --min-age-min 540 --min-count 2)"
assert_eq "--ref and --repo are accepted; bd runs in --repo" "0" "$(gate_rc --landings "$LANDINGS" --label "$LBL" --now "$NOW" --ref origin/main --repo "$TMP/rig")"
assert_eq "bd's working directory" "$(cd "$TMP/rig" && pwd -P)" "$(cut -d' ' -f1 "$BD_STUB_ARGS")"

echo "gate: the newest cursor in any run's notes decides what is new"
printf '[{"id":"gt-r1","notes":"cursor: 2026-10-01T00:00:00Z internal/a/one.go"},{"id":"gt-r2","notes":"reviewed: 2026-10-10 internal/a/one.go\\ncursor: 2026-10-10T03:00:00.123456Z internal/a/two.go"},{"id":"gt-r3"},{"id":"gt-r4","notes":null}]\n' > "$BD_STUB_OUT"
assert_eq "nothing after the cursor: skip" "1" "$(gate_rc --landings "$LANDINGS" --label "$LBL" --now "$NOW")"
assert_eq "output" "gate: skip n=0 oldest=0m" "$(gate_out)"
printf '{"data":[{"id":"gt-r1","notes":"cursor: 2026-10-10T01:00:00.5Z internal/a/one.go"}]}\n' > "$BD_STUB_OUT"
assert_eq "BD_MACHINE envelope: one landing, 419m old" "0" "$(gate_rc --landings "$LANDINGS" --label "$LBL" --now "$NOW")"
assert_eq "output" "gate: run n=1 oldest=419m" "$(gate_out)"
assert_eq "19m 59s old is 19m: skip" "1" "$(gate_rc --landings "$LANDINGS" --label "$LBL" --now 2026-10-10T03:19:59.123456Z)"
assert_eq "skip output" "gate: skip n=1 oldest=19m" "$(gate_out)"
assert_eq "exactly 20m: run" "0" "$(gate_rc --landings "$LANDINGS" --label "$LBL" --now 2026-10-10T03:20:00.123456Z)"
assert_eq "output" "gate: run n=1 oldest=20m" "$(gate_out)"
assert_eq "a landing in the future is 0m old, not negative" "gate: skip n=1 oldest=0m" "$(gate_rc --landings "$LANDINGS" --label "$LBL" --now 2026-10-10T02:00:00Z >/dev/null; gate_out)"

echo "gate: three young landings are enough"
cat > "$TMP/young.jsonl" <<'J'
{"bead":"gt-a","landed_at":"2026-10-10T09:50:00Z","risk_paths":["x"]}
{"bead":"gt-b","landed_at":"2026-10-10T09:52:00Z","risk_paths":["x"]}
{"bead":"gt-c","landed_at":"2026-10-10T09:55:00Z","risk_paths":["x"]}
{"bead":"gt-d","landed_at":"2026-10-10T09:56:00Z"}
J
echo '[]' > "$BD_STUB_OUT"
assert_eq "three at 10m: run" "0" "$(gate_rc --landings "$TMP/young.jsonl" --label "$LBL" --now "$NOW")"
assert_eq "output (the landing without risk paths is not counted)" "gate: run n=3 oldest=10m" "$(gate_out)"
assert_eq "--min-count 4: skip" "1" "$(gate_rc --landings "$TMP/young.jsonl" --label "$LBL" --now "$NOW" --min-count 4)"
assert_eq "skip output" "gate: skip n=3 oldest=10m" "$(gate_out)"
rc="$(plan_rc --mode landings --landings "$TMP/young.jsonl" --prev-notes "$TMP/none.txt" --now "$NOW" --out "$TMP/g1")"
assert_eq "plan --mode landings counts the same three" "3" "$(field landings)"

echo "gate --green-state: only landings a full green sweep covered count"
GREPO="$TMP/grepo"
mkrepo "$GREPO" internal/a/one.go
G1="$(git -C "$GREPO" rev-parse HEAD)"
echo "package y" > "$GREPO/internal/a/two.go"
git -C "$GREPO" add -A; git -C "$GREPO" commit -q -m second
G2="$(git -C "$GREPO" rev-parse HEAD)"
GL="$TMP/green-landings.jsonl"
cat > "$GL" <<J
{"bead":"gt-g1","landed_commit":"$G1","landed_at":"2026-10-10T01:00:00Z","risk_paths":["internal/a/one.go"]}
{"bead":"gt-g2","landed_commit":"$G2","landed_at":"2026-10-10T02:00:00Z","risk_paths":["internal/a/two.go"]}
J
green_state() { # sha shell integration race -> the record at $TMP/gs.json
  printf '{"last_green_sha":"%s","tiers":{"shell":{"verdict":"%s"},"integration":{"verdict":"%s"},"race":{"verdict":"%s"}}}\n' "$1" "$2" "$3" "$4" > "$TMP/gs.json"
}
green_state "$G1" GREEN GREEN GREEN
assert_eq "a sweep covering the older landing is enough on its own" "0" "$(gate_rc --landings "$GL" --label "$LBL" --repo "$GREPO" --now "$NOW" --green-state "$TMP/gs.json")"
assert_eq "the run line names the tip it gated on" "gate: run n=1 oldest=540m green=${G1:0:8}" "$(gate_out)"
green_state "$G2" GREEN GREEN GREEN
assert_eq "a sweep at the newer tip covers both" "0" "$(gate_rc --landings "$GL" --label "$LBL" --repo "$GREPO" --now "$NOW" --green-state "$TMP/gs.json")"
assert_eq "output" "gate: run n=2 oldest=540m green=${G2:0:8}" "$(gate_out)"
assert_eq "the gate still reads the run history" "list -l scheduled:deep-review --all --limit 60 --json" "$(cut -d' ' -f2- "$BD_STUB_ARGS")"
printf '{"bead":"gt-g3","landed_commit":"%s","landed_at":"2026-10-10T02:57:00Z","risk_paths":["internal/a/two.go"]}\n' "$G2" > "$TMP/green-young.jsonl"
assert_eq "one landing minutes old is enough (no count of three, no 20m wait)" "0" "$(gate_rc --landings "$TMP/green-young.jsonl" --label "$LBL" --repo "$GREPO" --now 2026-10-10T03:00:00Z --green-state "$TMP/gs.json")"
assert_eq "output" "gate: run n=1 oldest=3m green=${G2:0:8}" "$(gate_out)"
assert_eq "an explicit threshold still wins" "1" "$(gate_rc --landings "$GL" --label "$LBL" --repo "$GREPO" --now "$NOW" --green-state "$TMP/gs.json" --min-count 5 --min-age-min 600)"
assert_eq "skip output" "gate: skip n=2 oldest=540m green=${G2:0:8}" "$(gate_out)"

printf '{"bead":"gt-g2","landed_commit":"%s","landed_at":"2026-10-10T02:00:00Z","risk_paths":["internal/a/two.go"]}\n' "$G2" > "$TMP/green-newer.jsonl"
green_state "$G1" GREEN GREEN GREEN
assert_eq "a landing newer than the tip is not covered" "1" "$(gate_rc --landings "$TMP/green-newer.jsonl" --label "$LBL" --repo "$GREPO" --now "$NOW" --green-state "$TMP/gs.json")"
assert_eq "skip output" "gate: skip n=0 oldest=0m green=${G1:0:8}" "$(gate_out)"
printf '{"bead":"gt-gx","landed_commit":"deadbeefdeadbeefdeadbeefdeadbeefdeadbeef","landed_at":"2026-10-10T02:00:00Z","risk_paths":["internal/a/two.go"]}\n' > "$TMP/green-unknown.jsonl"
assert_eq "a commit the repo does not know does not count" "1" "$(gate_rc --landings "$TMP/green-unknown.jsonl" --label "$LBL" --repo "$GREPO" --now "$NOW" --green-state "$TMP/gs.json")"

echo "gate --green-state: a sweep that is not green is a skip, never a run"
green_state "$G1" GREEN GREEN RED
assert_eq "a red race tier" "1" "$(gate_rc --landings "$GL" --label "$LBL" --repo "$GREPO" --now "$NOW" --green-state "$TMP/gs.json")"
assert_eq "names the tier" "gate: skip (no green sweep: race tier is RED)" "$(gate_out)"
green_state "$G1" GREEN RED GREEN
assert_eq "a red integration tier" "gate: skip (no green sweep: integration tier is RED)" "$(gate_rc --landings "$GL" --label "$LBL" --repo "$GREPO" --now "$NOW" --green-state "$TMP/gs.json" >/dev/null; gate_out)"
printf '{"last_green_sha":"%s","tiers":{"integration":{"verdict":"GREEN"},"race":{"verdict":"GREEN"}}}\n' "$G1" > "$TMP/gs.json"
assert_eq "a tier the record does not carry" "gate: skip (no green sweep: shell tier is missing)" "$(gate_rc --landings "$GL" --label "$LBL" --repo "$GREPO" --now "$NOW" --green-state "$TMP/gs.json" >/dev/null; gate_out)"
printf '{"last_run":"2026-10-10T02:30:00Z","last_sha":"%s","last_green_sha":"%s","tiers":{"shell":{"verdict":"GREEN","passed":12,"failed":0,"failed_names":[],"log":"/tmp/s.log"},"integration":{"verdict":"GREEN","passed":4,"failed":0,"log":"/tmp/i.log"},"race":{"verdict":"GREEN","passed":1,"failed":0}}}\n' "$G1" "$G1" > "$TMP/gs.json"
assert_eq "the record's other fields are no obstacle" "0" "$(gate_rc --landings "$GL" --label "$LBL" --repo "$GREPO" --now "$NOW" --green-state "$TMP/gs.json")"
printf '{"last_green_sha":"","tiers":{"shell":{"verdict":"GREEN"},"integration":{"verdict":"GREEN"},"race":{"verdict":"GREEN"}}}' > "$TMP/gs.json"
assert_eq "no last_green_sha yet" "gate: skip (no green sweep: no last_green_sha)" "$(gate_rc --landings "$GL" --label "$LBL" --repo "$GREPO" --now "$NOW" --green-state "$TMP/gs.json" >/dev/null; gate_out)"
printf '{"last_green_sha":"0000000000000000000000000000000000000000","tiers":{"shell":{"verdict":"GREEN"},"integration":{"verdict":"GREEN"},"race":{"verdict":"GREEN"}}}' > "$TMP/gs.json"
assert_eq "a tip this repo does not hold" "gate: skip (no green sweep: last_green_sha 00000000 is not a commit in $GREPO)" "$(gate_rc --landings "$GL" --label "$LBL" --repo "$GREPO" --now "$NOW" --green-state "$TMP/gs.json" >/dev/null; gate_out)"
assert_eq "a record that is not there" "gate: skip (no green sweep: no state file)" "$(gate_rc --landings "$GL" --label "$LBL" --repo "$GREPO" --green-state "$TMP/absent.json" >/dev/null; gate_out)"
printf '{"last_green_sha":' > "$TMP/gsbad.json"
assert_eq "a record that is not JSON" "gate: skip (no green sweep: $TMP/gsbad.json is not readable JSON)" "$(gate_rc --landings "$GL" --label "$LBL" --repo "$GREPO" --green-state "$TMP/gsbad.json" >/dev/null; gate_out)"
printf '[]' > "$TMP/gsarr.json"
assert_eq "a record that is not an object" "gate: skip (no green sweep: $TMP/gsarr.json is not readable JSON)" "$(gate_rc --landings "$GL" --label "$LBL" --repo "$GREPO" --green-state "$TMP/gsarr.json" >/dev/null; gate_out)"
assert_eq "and all of them are one line on stdout" "1" "$(wc -l < "$TMP/gate.out" | tr -d ' ')"
assert_eq "--green-state with no value is usage" "2" "$(gate_rc --landings "$GL" --label "$LBL" --repo "$GREPO" --green-state)"

echo "gate --green-from-landings: a rig with no tier sweep gates on its last passing landing"
FL="$TMP/fl.jsonl"
cat > "$FL" <<J
{"bead":"al-1","target":"main","gate_result":"pass (ci exit 0 16s)","landed_commit":"$G1","landed_at":"2026-10-10T01:00:00Z"}
{"bead":"al-2","target":"main","gate_result":"pass (ci exit 0 16s)","landed_commit":"$G2","landed_at":"2026-10-10T02:00:00Z","risk_paths":["internal/a/two.go"]}
J
assert_eq "one risk-path landing the last green gate contains is enough" "0" "$(gate_rc --landings "$FL" --label "$LBL" --repo "$GREPO" --now "$NOW" --green-from-landings)"
assert_eq "output names the green tip; the landing without risk paths is not counted" "gate: run n=1 oldest=480m green=${G2:0:8}" "$(gate_out)"
assert_eq "no sweep file is read or needed" "0" "$(gate_rc --landings "$FL" --label "$LBL" --repo "$GREPO" --now 2026-10-10T02:03:00Z --green-from-landings)"
assert_eq "a landing minutes old runs (no count of three, no 20m wait)" "gate: run n=1 oldest=3m green=${G2:0:8}" "$(gate_out)"
assert_eq "an explicit threshold still wins" "1" "$(gate_rc --landings "$FL" --label "$LBL" --repo "$GREPO" --now "$NOW" --green-from-landings --min-count 5 --min-age-min 600)"
cat > "$FL" <<J
{"bead":"al-1","target":"main","gate_result":"pass (ci exit 0 16s)","landed_commit":"$G1","landed_at":"2026-10-10T01:00:00Z","risk_paths":["internal/a/one.go"]}
{"bead":"al-2","target":"main","gate_result":"fail (ci exit 1 16s)","landed_commit":"$G2","landed_at":"2026-10-10T02:00:00Z","risk_paths":["internal/a/two.go"]}
{"bead":"al-3","target":"release","gate_result":"pass (ci exit 0 16s)","landed_commit":"$G2","landed_at":"2026-10-10T02:30:00Z","risk_paths":["internal/a/two.go"]}
J
assert_eq "the last green gate on main is the newest passing record on main" "0" "$(gate_rc --landings "$FL" --label "$LBL" --repo "$GREPO" --now "$NOW" --green-from-landings)"
assert_eq "landings past that tip are not covered" "gate: run n=1 oldest=540m green=${G1:0:8}" "$(gate_out)"
printf '{"bead":"al-1","target":"main","gate_result":"fail (ci exit 1 16s)","landed_commit":"%s","landed_at":"2026-10-10T01:00:00Z","risk_paths":["x"]}\n' "$G1" > "$FL"
assert_eq "no passing record is a skip, never a run" "1" "$(gate_rc --landings "$FL" --label "$LBL" --repo "$GREPO" --now "$NOW" --green-from-landings)"
assert_eq "and says so" "gate: skip (no green gate: no landing record with a passing gate on main)" "$(gate_out)"
printf '{"bead":"al-1","gate_result":"pass (ci exit 0 1s)","landed_commit":"0000000000000000000000000000000000000000","landed_at":"2026-10-10T01:00:00Z","risk_paths":["x"]}\n' > "$FL"
assert_eq "a green tip the repo does not hold is a skip" "gate: skip (no green gate: last green landing 00000000 is not a commit in $GREPO)" "$(gate_rc --landings "$FL" --label "$LBL" --repo "$GREPO" --now "$NOW" --green-from-landings >/dev/null; gate_out)"
assert_eq "a landing the tip does not contain does not count" "1" "$(gate_rc --landings "$FL" --label "$LBL" --repo "$GREPO" --now "$NOW" --green-from-landings)"
assert_eq "--green-state and --green-from-landings together are usage" "2" "$(gate_rc --landings "$GL" --label "$LBL" --repo "$GREPO" --green-state "$TMP/gs.json" --green-from-landings)"

echo "gate: an input it cannot trust is exit 3, never a skip"
assert_eq "bd fails" "3" "$(BD_STUB_RC=1 gate_rc --landings "$LANDINGS" --label "$LBL" --now "$NOW")"
echo 'not json' > "$BD_STUB_OUT"
assert_eq "bd prints something that is not JSON" "3" "$(gate_rc --landings "$LANDINGS" --label "$LBL" --now "$NOW")"
: > "$BD_STUB_OUT"
assert_eq "bd prints nothing" "3" "$(gate_rc --landings "$LANDINGS" --label "$LBL" --now "$NOW")"
echo '{"error":"x"}' > "$BD_STUB_OUT"
assert_eq "an object without a data list" "3" "$(gate_rc --landings "$LANDINGS" --label "$LBL" --now "$NOW")"
echo '{"data":{"id":"gt-r1"}}' > "$BD_STUB_OUT"
assert_eq "data is not a list" "3" "$(gate_rc --landings "$LANDINGS" --label "$LBL" --now "$NOW")"
echo '[1,2]' > "$BD_STUB_OUT"
assert_eq "rows are not objects" "3" "$(gate_rc --landings "$LANDINGS" --label "$LBL" --now "$NOW")"
echo '[{"id":"gt-r1","notes":7}]' > "$BD_STUB_OUT"
assert_eq "notes is not a string" "3" "$(gate_rc --landings "$LANDINGS" --label "$LBL" --now "$NOW")"
echo '[{"id":"gt-r1","notes":"cursor: not-a-time internal/a/one.go"}]' > "$BD_STUB_OUT"
assert_eq "cursor with a bad timestamp" "3" "$(gate_rc --landings "$LANDINGS" --label "$LBL" --now "$NOW")"
assert_eq "nothing on stdout for any of them" "" "$(gate_out)"
echo '[]' > "$BD_STUB_OUT"
assert_eq "empty landings file" "3" "$(gate_rc --landings "$TMP/empty.jsonl" --label "$LBL" --now "$NOW")"
assert_eq "landed_at renamed" "3" "$(gate_rc --landings "$TMP/drift.jsonl" --label "$LBL" --now "$NOW")"
assert_eq "risk_paths is not a list" "3" "$(gate_rc --landings "$TMP/drift3.jsonl" --label "$LBL" --now "$NOW")"
assert_eq "a non-JSON line before the last" "3" "$(gate_rc --landings "$TMP/mid.jsonl" --label "$LBL" --now "$NOW")"
assert_eq "bad --now" "3" "$(gate_rc --landings "$LANDINGS" --label "$LBL" --now tomorrow)"
assert_eq "the failure says why" "1" "$(grep -c 'is not RFC3339' "$TMP/gate.err" || true)"
assert_eq "a torn last line is tolerated" "0" "$(gate_rc --landings "$LANDINGS" --label "$LBL" --now "$NOW")"

echo "gate: usage"
assert_eq "missing landings file" "2" "$(gate_rc --landings "$TMP/absent" --label "$LBL")"
assert_eq "no --landings" "2" "$(gate_rc --label "$LBL")"
assert_eq "no --label" "2" "$(gate_rc --landings "$LANDINGS")"
assert_eq "missing --repo directory" "2" "$(gate_rc --landings "$LANDINGS" --label "$LBL" --repo "$TMP/absent")"
assert_eq "non-numeric --min-count" "2" "$(gate_rc --landings "$LANDINGS" --label "$LBL" --min-count few)"
assert_eq "non-numeric --min-age-min" "2" "$(gate_rc --landings "$LANDINGS" --label "$LBL" --min-age-min soon)"
assert_eq "unknown flag" "2" "$(gate_rc --landings "$LANDINGS" --label "$LBL" --bogus 1)"
assert_eq "flag without a value" "2" "$(gate_rc --landings "$LANDINGS" --label)"

# ---- retired subcommands --------------------------------------------------

echo "om-queued: retired (gt-lw7yc), so it is an unknown subcommand and the usage and header omit it"
assert_eq "om-queued is usage" "2" "$("$DR" om-queued > /dev/null 2>&1 || echo $?)"
assert_eq "the usage text does not list it" "0" "$("$DR" 2>&1 >/dev/null | grep -c 'om-queued' || true)"
assert_eq "the header does not name it" "0" "$(grep -c 'om-queued' "$DR" || true)"

# ---- docs-audit -----------------------------------------------------------

audit_rc() { # prints the exit code; stdout to $TMP/audit.out, stderr to $TMP/audit.err
  local rc=0
  "$DR" docs-audit "$@" > "$TMP/audit.out" 2> "$TMP/audit.err" || rc=$?
  echo "$rc"
}

# Stub gt and bd, cobra-like. Bare --help prints the top-level command list.
# "<word> --help" prints help for the words the stub knows but does not list
# (gt unhook, bd stats), prints nothing and fails for "weird", and says
# unknown command for any other word. Every call is appended to $STUB_LOG, and
# any call that is not a lone --help or "<word> --help" is a failure.
mkdir -p "$TMP/dbin"
cat > "$TMP/dbin/gt" <<'STUB'
#!/usr/bin/env bash
[ -z "${STUB_LOG:-}" ] || echo "gt $*" >> "$STUB_LOG"
if [ "$*" = "--help" ]; then
  printf 'Gas Town stub\n\nUsage:\n  gt [command]\n\nWork Management:\n  done         Submit work\n  hook         Show the hook\n\nAdditional Commands:\n  help         Help about any command\n\nFlags:\n  -h, --help   help for gt\n'
  exit 0
fi
if [ $# -eq 2 ] && [ "$2" = "--help" ]; then
  case "$1" in
    unhook) printf 'Detach work from the hook\n\nUsage:\n  gt unhook [flags]\n'; exit 0 ;;
    weird) exit 1 ;;
  esac
  printf 'Error: unknown command "%s" for "gt"\nRun '"'"'gt --help'"'"' for usage.\n' "$1" >&2
  exit 1
fi
exit 1
STUB
cat > "$TMP/dbin/bd" <<'STUB'
#!/usr/bin/env bash
[ -z "${STUB_LOG:-}" ] || echo "bd $*" >> "$STUB_LOG"
if [ "$*" = "--help" ]; then
  printf 'Issues stub\n\nUsage:\n  bd [flags]\n  bd [command]\n\nWorking With Issues:\n  list              List issues\n  show              Show an issue\n\nFlags:\n  -h, --help   help for bd\n'
  exit 0
fi
if [ $# -eq 2 ] && [ "$2" = "--help" ]; then
  case "$1" in
    stats) printf 'Show statistics\n\nUsage:\n  bd stats [flags]\n'; exit 0 ;;
  esac
  printf 'Error: unknown command "%s" for "bd"\nRun '"'"'bd --help'"'"' for usage.\n' "$1" >&2
  exit 1
fi
exit 1
STUB
chmod +x "$TMP/dbin/gt" "$TMP/dbin/bd"

# A PATH without gt and bd: only the tools the audit runs.
mkdir -p "$TMP/nobin"
for t in git jq awk sort mkdir mktemp date; do ln -sf "$(command -v "$t")" "$TMP/nobin/$t"; done

DREPO="$TMP/drepo"
mkdir -p "$DREPO"
git -C "$DREPO" init -q
git -C "$DREPO" config user.email t@t; git -C "$DREPO" config user.name t
git -C "$DREPO" config commit.gpgsign false
dcommit() { # date message: commit everything staged-or-not at that date
  git -C "$DREPO" add -A
  GIT_AUTHOR_DATE="$1" GIT_COMMITTER_DATE="$1" git -C "$DREPO" commit -q -m "$2"
}
dwrite() { mkdir -p "$DREPO/$(dirname "$1")"; cat > "$DREPO/$1"; }
dtouch() { mkdir -p "$DREPO/$(dirname "$1")"; echo "change $RANDOM" >> "$DREPO/$1"; }

for f in internal/a/one.go internal/a/two.go internal/b/three.go internal/a/busy.go cmd/main.go scripts/tool.sh tests/t.sh; do dwrite "$f" <<< "package x"; done
dwrite README.md <<'MD'
# Demo
Code lives in `internal/a/one.go` and `internal/a/one.go:12`, see `internal/a/` too.
Gone: `internal/gone.go` and `docs/old.md#frag`; kept: `internal/b/three.go:3-9`.
Skipped: `cmd/*.go`, `internal/<pkg>/x.go`, `internal/...`, `scripts/{a,b}.sh`.
Run `gt done` then `gt zap now`; use `bd list -l x` and `bd nope`. Also `gt --version`,
`gt <cmd>`, the bead id `gt-4ysxr` and `go test ./internal/...` name nothing.

```
`internal/fenced.go` and `gt fenced` sit in a fence
```
MD
dwrite docs/stale.md <<'MD'
Covers `internal/a/one.go`, `internal/a/two.go` and `internal/b/three.go`.
MD
dwrite docs/fresh.md <<'MD'
Covers `internal/a/two.go`.
MD
dwrite docs/mid.md <<'MD'
Covers `internal/a/one.go`.
MD
dwrite docs/busy.md <<'MD'
Covers `internal/a/busy.go`.
MD
dwrite docs/concepts/c.md <<'MD'
Uses `scripts/tool.sh` and `scripts/nope.sh`.
MD
dwrite docs/guides/g.md <<'MD'
Uses `tests/t.sh` and `cmd/main.go`.
MD
dwrite internal/a/README.md <<'MD'
Lines `internal/a/one.go:5-9`.
MD
dwrite .beads/PRIME.md <<'MD'
Run `gt done`.
MD
dwrite AGENTS.md <<'MD'
Nothing cited.
MD
dwrite CLAUDE.md <<'MD'
Gone: `internal/claude-gone.go`.
MD
for f in docs/plans/p.md docs/design/d.md docs/adr/a.md docs/research/r.md docs/sub/deep.md; do
  dwrite "$f" <<'MD'
Gone: `internal/dead.go` and `gt maintain`.
MD
done
dcommit 2026-06-01T12:00:00Z c0
dtouch internal/a/one.go; dtouch internal/a/two.go
dcommit 2026-09-01T12:00:00Z c1
dtouch internal/a/two.go; dtouch docs/fresh.md
dcommit 2026-09-05T12:00:00Z c2
dtouch docs/mid.md
dcommit 2026-09-06T12:00:00Z c2b
dtouch internal/a/one.go; dtouch internal/b/three.go
dcommit 2026-09-08T12:00:00Z c3
for i in $(seq 1 12); do dtouch internal/a/busy.go; dcommit "2026-09-$((10 + i))T12:00:00Z" "busy $i"; done
git -C "$DREPO" tag c3
dwrite internal/gone.go <<< "package x"
dcommit 2026-09-30T12:00:00Z c4

echo "docs-audit: dead paths, dead commands, code_changed, score order, living docs only"
cat > "$TMP/audit.want" <<'W'
22 README.md dead_paths=2 dead_cmds=2 code_changed=2
10 docs/busy.md dead_paths=0 dead_cmds=0 code_changed=12
5 CLAUDE.md dead_paths=1 dead_cmds=0 code_changed=0
5 docs/concepts/c.md dead_paths=1 dead_cmds=0 code_changed=0
3 docs/stale.md dead_paths=0 dead_cmds=0 code_changed=3
2 internal/a/README.md dead_paths=0 dead_cmds=0 code_changed=2
1 docs/mid.md dead_paths=0 dead_cmds=0 code_changed=1
0 .beads/PRIME.md dead_paths=0 dead_cmds=0 code_changed=0
0 AGENTS.md dead_paths=0 dead_cmds=0 code_changed=0
0 docs/fresh.md dead_paths=0 dead_cmds=0 code_changed=0
0 docs/guides/g.md dead_paths=0 dead_cmds=0 code_changed=0
W
rc="$(PATH="$TMP/dbin:$PATH" audit_rc --repo "$DREPO" --ref c3 --now "$NOW" --out "$TMP/da1")"
assert_eq "exit 0" "0" "$rc"
assert_eq "docs counted on stdout" "11" "$(grep '^docs=' "$TMP/audit.out" | cut -d= -f2)"
assert_eq "scores, counts and order; plans, design, adr, research and docs/sub are not audited" "$(cat "$TMP/audit.want")" "$(cat "$TMP/da1/docs-audit.txt")"
assert_eq "a span inside a fence is not a citation" "0" "$(grep -c 'fenced' "$TMP/da1/doc-refs.tsv" || true)"

echo "docs-audit: code_changed counts a commit once, after the doc's own commit, inside 60 days, at most 10"
assert_eq "stale.md: c1, c2 and c3 touch cited files, c3 touches two of them" "3" "$(grep ' docs/stale.md ' "$TMP/da1/docs-audit.txt" | sed 's/.*code_changed=//')"
assert_eq "fresh.md changed in the commit that changed its code: nothing after it" "0" "$(grep ' docs/fresh.md ' "$TMP/da1/docs-audit.txt" | sed 's/.*code_changed=//')"
assert_eq "mid.md: the code commit before its own commit does not count, the one after does" "1" "$(grep ' docs/mid.md ' "$TMP/da1/docs-audit.txt" | sed 's/.*code_changed=//')"
assert_eq "busy.md: 12 commits are reported as 12 and cap at 10 in the score" "10" "$(grep ' docs/busy.md ' "$TMP/da1/docs-audit.txt" | cut -d' ' -f1)"
rc="$(PATH="$TMP/dbin:$PATH" audit_rc --repo "$DREPO" --ref c3 --now 2026-12-01T00:00:00Z --out "$TMP/da2")"
assert_eq "everything older than 60 days before --now is outside the window" "0" "$(awk '{ s += substr($5, 14) } END { print s + 0 }' "$TMP/da2/docs-audit.txt")"

echo "docs-audit: --ref picks the tree"
rc="$(PATH="$TMP/dbin:$PATH" audit_rc --repo "$DREPO" --now "$NOW" --out "$TMP/da3")"
assert_eq "at HEAD internal/gone.go exists and its commit counts" "18 README.md dead_paths=1 dead_cmds=2 code_changed=3" "$(head -n 1 "$TMP/da3/docs-audit.txt")"

echo "docs-audit: a binary that is not on PATH is not judged"
rc=0
PATH="$TMP/nobin" "$BASH" "$DR" docs-audit --repo "$DREPO" --ref c3 --now "$NOW" --out "$TMP/da4" > "$TMP/audit.out" 2> "$TMP/audit.err" || rc=$?
assert_eq "exit 0" "0" "$rc"
assert_eq "no dead_cmds without gt and bd" "12 README.md dead_paths=2 dead_cmds=0 code_changed=2" "$(head -n 1 "$TMP/da4/docs-audit.txt")"

echo "docs-audit: a command the listing lacks is confirmed with --help"
CREPO1="$TMP/crepo1"
mkrepo "$CREPO1" internal/a/one.go
mkdir -p "$CREPO1/docs"
cat > "$CREPO1/docs/cmds.md" <<'MD'
Listed: `gt done` and `bd list`. Not listed but they run: `gt unhook` and `bd stats`.
Unknown: `gt zap` and `bd nope`. Fails without saying unknown: `gt weird`. Again: `gt zap`.
MD
cat > "$CREPO1/docs/more.md" <<'MD'
Same words in a second doc: `gt unhook`, `gt zap`.
MD
git -C "$CREPO1" add -A; git -C "$CREPO1" commit -q -m docs
: > "$TMP/stub1.log"
rc="$(STUB_LOG="$TMP/stub1.log" PATH="$TMP/dbin:$PATH" audit_rc --repo "$CREPO1" --now "$NOW" --out "$TMP/dc1")"
assert_eq "exit 0" "0" "$rc"
assert_eq "gt unhook, bd stats and gt weird answer --help, so only gt zap and bd nope are dead" "10 docs/cmds.md dead_paths=0 dead_cmds=2 code_changed=0" "$(grep ' docs/cmds.md ' "$TMP/dc1/docs-audit.txt")"
assert_eq "the same candidate in a second doc is judged the same way" "5 docs/more.md dead_paths=0 dead_cmds=1 code_changed=0" "$(grep ' docs/more.md ' "$TMP/dc1/docs-audit.txt")"
assert_eq "each candidate is asked once and judged by its answer" "bd nope dead
bd stats alive
gt unhook alive
gt weird alive
gt zap dead" "$(LC_ALL=C sort "$TMP/dc1/cmd-checks.txt")"
assert_eq "the only calls are the two listings and one <word> --help per candidate" "bd --help
bd nope --help
bd stats --help
gt --help
gt unhook --help
gt weird --help
gt zap --help" "$(LC_ALL=C sort "$TMP/stub1.log")"

echo "docs-audit: at most 20 confirming --help calls, best-scoring docs first"
CREPO2="$TMP/crepo2"
mkrepo "$CREPO2" internal/a/one.go
mkdir -p "$CREPO2/docs"
for n in $(seq -w 1 15); do printf '`gt yy%s` ' "$n"; done > "$CREPO2/docs/high.md"
for n in $(seq -w 1 10); do printf '`gt zz%s` ' "$n"; done > "$CREPO2/docs/low.md"
git -C "$CREPO2" add -A; git -C "$CREPO2" commit -q -m docs
: > "$TMP/stub2.log"
rc="$(STUB_LOG="$TMP/stub2.log" PATH="$TMP/dbin:$PATH" audit_rc --repo "$CREPO2" --now "$NOW" --out "$TMP/dc2")"
assert_eq "exit 0" "0" "$rc"
assert_eq "20 calls beyond the two listings" "20" "$(grep -c -v -x -e 'gt --help' -e 'bd --help' "$TMP/stub2.log")"
assert_eq "the higher-scoring doc is asked about first: all 15 of its commands count dead" "75 docs/high.md dead_paths=0 dead_cmds=15 code_changed=0" "$(grep ' docs/high.md ' "$TMP/dc2/docs-audit.txt")"
assert_eq "the lower one gets the 5 calls left; the other 5 are not counted dead" "25 docs/low.md dead_paths=0 dead_cmds=5 code_changed=0" "$(grep ' docs/low.md ' "$TMP/dc2/docs-audit.txt")"
assert_eq "the 5 past the cap are recorded as skipped" "5" "$(grep -c ' skipped$' "$TMP/dc2/cmd-checks.txt")"

echo "docs-audit: the confirming calls pass --help and nothing else"
assert_eq "no call without --help" "0" "$(cat "$TMP/stub1.log" "$TMP/stub2.log" | grep -c -v -e '--help$' || true)"
assert_eq "no call with more than one word before --help" "0" "$(cat "$TMP/stub1.log" "$TMP/stub2.log" | awk 'NF > 3' | wc -l | tr -d ' ')"

echo "docs-audit: usage and untrusted input"
assert_eq "no --repo" "2" "$(audit_rc --out "$TMP/du1")"
assert_eq "missing --repo directory" "2" "$(audit_rc --repo "$TMP/absent")"
assert_eq "unknown flag" "2" "$(audit_rc --repo "$DREPO" --bogus 1)"
assert_eq "flag without a value" "2" "$(audit_rc --repo "$DREPO" --ref)"
assert_eq "missing --prev-notes file" "2" "$(audit_rc --repo "$DREPO" --prev-notes "$TMP/absent")"
assert_eq "--prev-notes is accepted" "0" "$(audit_rc --repo "$DREPO" --ref c3 --now "$NOW" --prev-notes "$TMP/none.txt" --out "$TMP/du2")"
assert_eq "bad --now" "3" "$(audit_rc --repo "$DREPO" --now tomorrow --out "$TMP/du3")"
assert_eq "unknown ref" "3" "$(audit_rc --repo "$DREPO" --ref no-such-ref --now "$NOW" --out "$TMP/du4")"

# ---- plan: docs.txt -------------------------------------------------------

echo "plan docs.txt: docs that cite a slice file or its package directory, then the stalest docs"
PREPO="$TMP/prepo"
mkrepo "$PREPO" internal/a/one.go internal/a/two.go internal/b/three.go internal/c/four.go
dead() { # n prefix: n backticked paths the tree does not hold
  local i
  for i in $(seq 1 "$1"); do printf '`internal/dead/%s-%s.go` ' "$2" "$i"; done
  echo
}
{ echo '`internal/a/one.go` `internal/x/gone.go`'; } > "$PREPO/README.md"
mkdir -p "$PREPO/docs"
echo 'Package `internal/b/`.' > "$PREPO/docs/p1.md"
echo '`internal/c/four.go` `internal/d/1.go` `internal/d/2.go`' > "$PREPO/docs/p2.md"
dead 3 p3 > "$PREPO/docs/p3.md"
echo 'Nothing cited.' > "$PREPO/docs/p4.md"
echo '`internal/a/two.go`' > "$PREPO/docs/q.md"
git -C "$PREPO" add -A; git -C "$PREPO" commit -q -m docs

rc="$(plan_rc --mode slice --prev-notes "$TMP/none.txt" --repo "$PREPO" --slice 3 --now "$NOW" --out "$TMP/pd1")"
assert_eq "exit 0" "0" "$rc"
assert_eq "slice is the first three files by path" "internal/a/one.go internal/a/two.go internal/b/three.go" "$(joined "$TMP/pd1/slice.txt")"
assert_eq "paired by score then path, then the two highest scorers not paired already" "README.md docs/p1.md docs/q.md docs/p3.md docs/p2.md" "$(joined "$TMP/pd1/docs.txt")"
assert_eq "docs= is the line count" "5" "$(field docs)"
assert_eq "the audit lands beside it, best score first" "15 docs/p3.md dead_paths=3 dead_cmds=0 code_changed=0" "$(head -n 1 "$TMP/pd1/docs-audit.txt")"
assert_eq "four.go is outside the slice, so p2 is there for its score only" "docs/p2.md" "$(grep -x 'docs/p2.md' "$TMP/pd1/docs.txt")"

printf 'reviewed: 2026-10-05 docs/p3.md\nreviewed: 2026-01-01 docs/p3.md\nreviewed: 2026-09-01 docs/p2.md\nreviewed: yesterday docs/p1.md\n' > "$TMP/dn1.txt"
rc="$(plan_rc --mode slice --prev-notes "$TMP/dn1.txt" --repo "$PREPO" --slice 3 --now "$NOW" --out "$TMP/pd2")"
assert_eq "a doc reviewed 5 days ago (newest date wins) drops out; one reviewed 39 days ago stays eligible" "README.md docs/p1.md docs/q.md docs/p2.md" "$(joined "$TMP/pd2/docs.txt")"
assert_eq "docs= follows" "4" "$(field docs)"

rc="$(plan_rc --mode slice --prev-notes "$TMP/none.txt" --repo "$PREPO" --slice 1 --now "$NOW" --out "$TMP/pd3")"
assert_eq "a slice of one file pairs only the docs that cite it" "README.md docs/p3.md docs/p2.md" "$(joined "$TMP/pd3/docs.txt")"

echo "plan docs.txt: 6 paired docs and 8 in all"
CREPO="$TMP/crepo"
mkrepo "$CREPO" internal/a/one.go
mkdir -p "$CREPO/docs"
for n in 1 2 3 4 5 6 7 8 9; do { printf '`internal/a/one.go` '; dead "$n" "d$n"; } > "$CREPO/docs/d$n.md"; done
dead 30 ha > "$CREPO/docs/hot-a.md"; dead 20 hb > "$CREPO/docs/hot-b.md"; dead 10 hc > "$CREPO/docs/hot-c.md"
git -C "$CREPO" add -A; git -C "$CREPO" commit -q -m docs
rc="$(plan_rc --mode slice --prev-notes "$TMP/none.txt" --repo "$CREPO" --now "$NOW" --out "$TMP/pc1")"
assert_eq "six paired by score, then hot-a and hot-b; hot-c is cut by the cap of 8" "docs/d9.md docs/d8.md docs/d7.md docs/d6.md docs/d5.md docs/d4.md docs/hot-a.md docs/hot-b.md" "$(joined "$TMP/pc1/docs.txt")"
assert_eq "docs=8" "8" "$(field docs)"

echo "plan docs.txt: the docs go where the slice goes"
rc="$(plan_rc --mode landings --landings "$LANDINGS" --prev-notes "$TMP/none.txt" --now "$NOW" --out "$TMP/pl1")"
assert_eq "landings mode: no docs" "0" "$(field docs)"
assert_eq "landings mode: docs.txt empty" "0" "$(wc -c < "$TMP/pl1/docs.txt" | tr -d ' ')"
rc="$(plan_rc --mode both --landings "$LANDINGS" --prev-notes "$TMP/n2.txt" --repo "$PREPO" --slice 3 --now "$NOW" --out "$TMP/pl2")"
assert_eq "both mode on a quiet day: no slice, no docs" "true/0/0" "$(field quiet)/$(field slice_files)/$(field docs)"
rc="$(plan_rc --mode both --landings "$LANDINGS" --prev-notes "$TMP/none.txt" --repo "$PREPO" --slice 3 --now "$NOW" --out "$TMP/pl3")"
assert_eq "both mode with landings: the same docs as slice mode" "README.md docs/p1.md docs/q.md docs/p3.md docs/p2.md" "$(joined "$TMP/pl3/docs.txt")"
rc="$(plan_rc --landings "$LANDINGS" --prev-notes "$TMP/none.txt" --repo "$REPO" --now "$NOW" --out "$TMP/pl4")"
assert_eq "a tree with no living docs plans none" "0" "$(field docs)"

# ---- plan: review-targeting battery ---------------------------------------
echo ""
echo "plan slice: the review-targeting battery changes none of the slice output"

TOWN_ON="$TMP/town-on"
mkdir -p "$TOWN_ON/mayor"
printf '{"type":"daemon-patrol-config","version":1,"patrols":{"jev_targets":{"enabled":true,"usd_per_billion_tokens":3}}}\n' > "$TOWN_ON/mayor/daemon.json"
TOWN_HOST="$TMP/town-host"
mkdir -p "$TOWN_HOST/settings"
printf '{"daemon":{"patrols":{"jev_targets":{"enabled":true,"usd_per_billion_tokens":3}}}}\n' > "$TOWN_HOST/settings/config.json"
TOWN_OFF="$TMP/town-off"
mkdir -p "$TOWN_OFF/mayor"
printf '{"type":"daemon-patrol-config","version":1,"patrols":{"jev_targets":{"enabled":false}}}\n' > "$TOWN_OFF/mayor/daemon.json"

GTBIN="$TMP/gt-bin"; mkdir -p "$GTBIN"
cat > "$GTBIN/gt" <<'G'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "${GT_CALLS:-/dev/null}"
echo "JEV-STDOUT"
echo "JEV-STDERR" >&2
exit "${GT_EXIT:-0}"
G
chmod +x "$GTBIN/gt"

# A PATH holding everything plan needs except gt, so the absent case is a
# command that is genuinely not there.
SHIM="$TMP/shim"; mkdir -p "$SHIM"
for tool in bash env git jq awk sort cut grep sed tr cat date mkdir mktemp rm dirname basename head tail wc ln; do
  src="$(command -v "$tool" 2>/dev/null || true)"
  [ -z "$src" ] || ln -sf "$src" "$SHIM/$tool"
done

# battery_plan TOWN OUT GT_PATH RUN_BEAD [VAR=value...]: runs plan --mode slice
# and prints the exit code. A gt on GT_PATH records its arguments.
battery_plan() {
  local town="$1" out="$2" gtpath="$3" run="$4"; shift 4
  local runflag=""
  [ -z "$run" ] || runflag="--run $run"
  local rc=0
  # shellcheck disable=SC2086
  env GT_TOWN_ROOT="$town" GT_CALLS="$TMP/gt-calls" PATH="$gtpath" "$@" \
    "$DR" plan --mode slice --prev-notes "$TMP/none.txt" --repo "$REPO" \
    --now "$NOW" $runflag --out "$out" > "$TMP/plan.out" 2> "$TMP/plan.err" || rc=$?
  echo "$rc"
}

# battery_case NAME TOWN OUT GT_PATH RUN_BEAD [VAR=value...]: runs one case
# and keeps everything it could change: plan's streams, the slice files, and
# the fake gt's calls.
battery_case() {
  local name="$1"; shift
  : > "$TMP/gt-calls"
  battery_plan "$@" > "$TMP/rc-$name"
  cp "$TMP/plan.out" "$TMP/planout-$name"
  cp "$TMP/plan.err" "$TMP/planerr-$name"
  cp "$TMP/gt-calls" "$TMP/calls-$name"
  {
    grep -v '^out=' "$TMP/planout-$name"
    printf -- '--slice--\n'; cat "$TMP/bo-$name/slice.txt"
    printf -- '--rows--\n'; cat "$TMP/bo-$name/slice-rows.txt"
    printf -- '--reviewed--\n'; cat "$TMP/bo-$name/reviewed-lines.txt"
    printf -- '--history--\n'; cat "$TMP/bo-$name/history.txt"
  } > "$TMP/sig-$name"
}

battery_case off "$TOWN_OFF" "$TMP/bo-off" "$GTBIN:$PATH" gt-run1
battery_case on "$TOWN_ON" "$TMP/bo-on" "$GTBIN:$PATH" gt-run1
battery_case fail "$TOWN_ON" "$TMP/bo-fail" "$GTBIN:$PATH" gt-run1 GT_EXIT=9
battery_case absent "$TOWN_ON" "$TMP/bo-absent" "$SHIM" gt-run1
battery_case norun "$TOWN_ON" "$TMP/bo-norun" "$GTBIN:$PATH" ""
battery_case host "$TOWN_HOST" "$TMP/bo-host" "$GTBIN:$PATH" gt-run1

for c in on fail absent norun host; do
  assert_eq "the slice output is byte-identical with the battery $c" "same" \
    "$(cmp -s "$TMP/sig-off" "$TMP/sig-$c" && echo same || echo different)"
done
assert_eq "not enabled: the battery is never called" "0" "$(wc -l < "$TMP/calls-off" | tr -d ' ')"
assert_eq "enabled: the battery is called once" "1" "$(wc -l < "$TMP/calls-on" | tr -d ' ')"
assert_eq "enabled: the call names the run and the slice rows" "gt-run1|$TMP/bo-on/slice-rows.txt" \
  "$(awk '{ print $4 "|" $6 }' "$TMP/calls-on")"
assert_eq "enabled in settings/config.json's daemon section: called once" "1" "$(wc -l < "$TMP/calls-host" | tr -d ' ')"
assert_eq "no --run: the battery is never called" "0" "$(wc -l < "$TMP/calls-norun" | tr -d ' ')"
assert_eq "the absent PATH really holds no gt" "no" \
  "$(env PATH="$SHIM" bash -c 'command -v gt >/dev/null && echo yes || echo no')"
assert_eq "absent: nothing was called" "0" "$(wc -l < "$TMP/calls-absent" | tr -d ' ')"
assert_eq "the battery's stdout stays out of plan's stdout" "0" "$(grep -c 'JEV-STDOUT' "$TMP/planout-on" || true)"
assert_eq "the battery's output is reported on stderr" "1" "$(grep -c 'JEV-STDERR' "$TMP/planerr-on" || true)"
assert_eq "a failing battery leaves plan's exit at 0" "0" "$(cat "$TMP/rc-fail")"

echo ""
echo "passed: $PASS failed: $FAIL"
[[ "$FAIL" -eq 0 ]]
