#!/usr/bin/env bash
# Tests for scripts/deep-review.sh: quiet detection, a quiet verdict that fails
# closed on unreadable input, timestamp ordering, the cursor ledger, the
# risk-ranked rotation slice (score, 21-day exclusion, count and byte caps,
# reviewed lines, cursor) on a production-sized tree, fingerprint lookup, and
# the queued ledger line's severity.
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

LANDINGS="$TMP/landings.jsonl"
cat > "$LANDINGS" <<'J'
{"bead":"gt-1","head":"h1","landed_commit":"c1","landed_at":"2026-10-10T01:00:00.5Z","risk_paths":["internal/daemon/x.go"]}
{"bead":"gt-2","head":"h2","landed_commit":"c2","landed_at":"2026-10-10T02:00:00Z"}
{"bead":"gt-3","head":"h3","landed_commit":"c3","landed_at":"2026-10-10T03:00:00.123456Z","risk_paths":["internal/land/y.go"]}
{torn
J
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
cat > "$TMP/ts.jsonl" <<'J'
{"bead":"gt-a","head":"ha","landed_commit":"ca","landed_at":"2026-10-10T05:00:00.542005Z","risk_paths":["x"]}
{"bead":"gt-b","head":"hb","landed_commit":"cb","landed_at":"2026-10-10T05:00:00.9Z","risk_paths":["x"]}
{"bead":"gt-c","head":"hc","landed_commit":"cc","landed_at":"2026-10-10T06:30:00+02:00","risk_paths":["x"]}
J
printf 'cursor: 2026-10-10T05:00:00Z internal/a/one.go\n' > "$TMP/n5.txt"
rc="$(plan_rc --landings "$TMP/ts.jsonl" --prev-notes "$TMP/n5.txt" --repo "$REPO" --now "$NOW" --out "$TMP/o5")"
assert_eq "fraction after a whole-second cursor is newer; +02:00 is 04:30Z, before it" "gt-a gt-b" "$(jq -r .bead "$TMP/o5/landings.jsonl" | tr '\n' ' ' | sed 's/ $//')"
assert_eq "newest by instant" "2026-10-10T05:00:00.9Z" "$(field reviewed_until_new)"
printf 'cursor: 2026-10-10T05:00:00.9Z internal/a/one.go\n' > "$TMP/n5b.txt"
rc="$(plan_rc --landings "$TMP/ts.jsonl" --prev-notes "$TMP/n5b.txt" --repo "$REPO" --now "$NOW" --out "$TMP/o5b")"
assert_eq "a landing at the cursor instant is not reviewed again" "true" "$(field quiet)"

echo "the quiet verdict fails closed"
: > "$TMP/empty.jsonl"
assert_eq "empty landings file" "3" "$(plan_rc --landings "$TMP/empty.jsonl" --prev-notes "$TMP/none.txt" --repo "$REPO" --now "$NOW" --out "$TMP/e1")"
printf '{"bead":"gt-1","landed_ts":"2026-10-10T01:00:00Z","risk_paths":["x"]}\n' > "$TMP/drift.jsonl"
assert_eq "landed_at renamed" "3" "$(plan_rc --landings "$TMP/drift.jsonl" --prev-notes "$TMP/none.txt" --repo "$REPO" --now "$NOW" --out "$TMP/e2")"
{ cat "$TMP/ts.jsonl"; printf '{"bead":"gt-9","landed_at":"yesterday","risk_paths":["x"]}\n'; } > "$TMP/drift2.jsonl"
assert_eq "one drifted line among good ones" "3" "$(plan_rc --landings "$TMP/drift2.jsonl" --prev-notes "$TMP/none.txt" --repo "$REPO" --now "$NOW" --out "$TMP/e3")"
{ cat "$TMP/ts.jsonl"; printf '{"bead":"gt-9","landed_at":"2026-10-10T06:00:00Z","risk_paths":"x"}\n'; } > "$TMP/drift3.jsonl"
assert_eq "risk_paths is not a list" "3" "$(plan_rc --landings "$TMP/drift3.jsonl" --prev-notes "$TMP/none.txt" --repo "$REPO" --now "$NOW" --out "$TMP/e4")"
{ printf 'garbage\n'; cat "$TMP/ts.jsonl"; } > "$TMP/mid.jsonl"
assert_eq "a non-JSON line before the last" "3" "$(plan_rc --landings "$TMP/mid.jsonl" --prev-notes "$TMP/none.txt" --repo "$REPO" --now "$NOW" --out "$TMP/e5")"
printf '[1,2]\n' > "$TMP/arr.jsonl"
assert_eq "a JSON line that is not an object" "3" "$(plan_rc --landings "$TMP/arr.jsonl" --prev-notes "$TMP/none.txt" --repo "$REPO" --now "$NOW" --out "$TMP/e6")"
printf 'cursor: not-a-time internal/a/one.go\n' > "$TMP/badcur.txt"
assert_eq "cursor with a bad timestamp" "3" "$(plan_rc --landings "$LANDINGS" --prev-notes "$TMP/badcur.txt" --repo "$REPO" --now "$NOW" --out "$TMP/e7")"
printf 'cursor: 2026-10-10T03:00:00Z\n' > "$TMP/badcur2.txt"
assert_eq "cursor with no path" "3" "$(plan_rc --landings "$LANDINGS" --prev-notes "$TMP/badcur2.txt" --repo "$REPO" --now "$NOW" --out "$TMP/e8")"
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

echo "queued: P3 is a severity, a line without one fails closed"
printf 'queued: a.go:A:x P3 a.go:1 low claim\n' > "$TMP/q3.txt"
assert_eq "P3 carried through" "queued: a.go:A:x P3 a.go:1 low claim" "$("$DR" queued --prev-notes "$TMP/q3.txt")"
queued_rc() { local rc=0; "$DR" queued --prev-notes "$1" > "$TMP/queued.out" 2> "$TMP/queued.err" || rc=$?; echo "$rc"; }
printf 'queued: a.go:A:x a.go:1 no severity\n' > "$TMP/qbad1.txt"
assert_eq "severity missing" "3" "$(queued_rc "$TMP/qbad1.txt")"
assert_eq "the failure names the line" "1" "$(grep -c 'malformed queued line: queued: a.go:A:x a.go:1 no severity' "$TMP/queued.err" || true)"
printf 'queued: a.go:A:x P4 a.go:1 unknown severity\n' > "$TMP/qbad2.txt"
assert_eq "severity not P1-P3" "3" "$(queued_rc "$TMP/qbad2.txt")"
printf 'queued: a.go:A:x P2 a.go:1\n' > "$TMP/qbad3.txt"
assert_eq "claim missing" "3" "$(queued_rc "$TMP/qbad3.txt")"

echo ""
echo "passed: $PASS failed: $FAIL"
[[ "$FAIL" -eq 0 ]]
