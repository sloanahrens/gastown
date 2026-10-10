#!/usr/bin/env bash
# Tests for scripts/deep-review.sh: quiet detection, a quiet verdict that fails
# closed on unreadable input, timestamp ordering, the cursor ledger, the
# rotation slice on a production-sized tree, and fingerprint lookup.
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
assert_eq "slice skips tests and non-internal" "internal/a/one.go internal/a/two.go internal/b/four.go internal/b/three.go" "$(joined "$TMP/o1/slice.txt")"

echo "ledger cursor: nothing after it is quiet"
printf 'cursor: 2026-10-10T03:00:00.123456Z internal/a/two.go\n' > "$TMP/n2.txt"
rc="$(plan_rc --landings "$LANDINGS" --prev-notes "$TMP/n2.txt" --repo "$REPO" --now "$NOW" --out "$TMP/o2")"
assert_eq "exit 0" "0" "$rc"
assert_eq "quiet" "true" "$(field quiet)"
assert_eq "no slice on a quiet day" "0" "$(field slice_files)"
assert_eq "cursor stays" "2026-10-10T03:00:00.123456Z" "$(field reviewed_until_new)"

echo "ledger cursor: the newest timestamp wins whatever the order, slice wraps"
printf 'cursor: 2026-10-01T00:00:00Z internal/a/one.go\ncursor: 2026-10-10T01:00:00.5Z internal/b/four.go\ncursor: 2026-10-05T00:00:00Z internal/a/two.go\n' > "$TMP/n3.txt"
rc="$(plan_rc --landings "$LANDINGS" --prev-notes "$TMP/n3.txt" --repo "$REPO" --slice 2 --now "$NOW" --out "$TMP/o3")"
assert_eq "one landing after cursor" "1" "$(field landings)"
assert_eq "slice after cursor, wrapping" "internal/b/three.go internal/a/one.go" "$(joined "$TMP/o3/slice.txt")"
assert_eq "new rotation cursor" "internal/a/one.go" "$(field rotation_cursor_new)"

echo "slice larger than the tree is the whole tree once"
rc="$(plan_rc --landings "$LANDINGS" --prev-notes "$TMP/n3.txt" --repo "$REPO" --slice 50 --now "$NOW" --out "$TMP/o3b")"
assert_eq "four files, no repeats" "internal/b/three.go internal/a/one.go internal/a/two.go internal/b/four.go" "$(joined "$TMP/o3b/slice.txt")"

echo "a rotation cursor whose file was deleted still resumes after its position"
printf 'cursor: 2026-10-01T00:00:00Z internal/a/gone.go\n' > "$TMP/n3c.txt"
rc="$(plan_rc --landings "$LANDINGS" --prev-notes "$TMP/n3c.txt" --repo "$REPO" --slice 2 --now "$NOW" --out "$TMP/o3c")"
assert_eq "resumes at the next path" "internal/a/one.go internal/a/two.go" "$(joined "$TMP/o3c/slice.txt")"

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
assert_eq "slice starts at the first path" "internal/pkg1/file1.go" "$(head -n 1 "$TMP/o6/slice.txt")"
printf 'cursor: 2026-10-01T00:00:00Z internal/pkg9/file99.go\n' > "$TMP/n6.txt"
rc="$(plan_rc --landings "$LANDINGS" --prev-notes "$TMP/n6.txt" --repo "$BIG" --slice 3 --now "$NOW" --out "$TMP/o7")"
assert_eq "the last path in byte order wraps to the first" "internal/pkg1/file1.go internal/pkg1/file10.go internal/pkg1/file100.go" "$(joined "$TMP/o7/slice.txt")"

echo "the --ref tree is read, not the working tree"
echo "package x" > "$REPO/internal/a/untracked.go"
git -C "$REPO" add -A; git -C "$REPO" commit -q -m more
rc="$(plan_rc --landings "$LANDINGS" --prev-notes "$TMP/none.txt" --repo "$REPO" --ref HEAD~1 --slice 50 --now "$NOW" --out "$TMP/o8")"
assert_eq "file added after the ref is absent" "0" "$(grep -c untracked "$TMP/o8/slice.txt" || true)"

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

echo ""
echo "passed: $PASS failed: $FAIL"
[[ "$FAIL" -eq 0 ]]
