#!/usr/bin/env bash
# deep-review.sh - deterministic helper for the daily mol-deep-review run.
#
# The formula's orchestrator decides nothing this script can decide:
#   plan    whether the day is quiet, which landings to review, which rotation
#           slice to read, from the ledger the previous runs left in bead notes
#   seen    whether a finding fingerprint is already in the ledger
#   queued  confirmed findings an earlier run recorded but never filed
#   batches unfiled P3 findings grouped per package, ready to file as one bead
#
# Ledger lines, written into the run bead's notes by the formula:
#   cursor: <RFC3339 landed_at of the newest landing covered> <last slice path>
#   reviewed: <YYYY-MM-DD> <path>
#   finding: <fingerprint> <bead id | digest-only>
#   queued: <fingerprint> <P1|P2|P3> <file:line> <claim>
#   p3: <YYYY-MM-DD> <fingerprint> <file:line> <claim>
# --prev-notes is the notes of recent run beads in any order. The cursor line
# with the newest timestamp wins, so the order and status of the run beads do
# not matter. The rotation slice is ranked by risk, not by the cursor path: a
# file with a reviewed: line from the last 21 days is skipped, and the newest
# date wins when a path has several. A reviewed: line that is malformed is
# ignored, because the worst outcome is a file read twice. A queued finding stays pending until a finding line names it.
# A queued line that does not carry a severity and a claim stops queued with
# exit 3: the run that would file it reads a truncated queue as a short one,
# and a lost finding is worse than a run that stops and escalates. A p3: line
# is a P3 finding held back so a run does not file one bead per P3. batches
# groups the ones no finding: line names yet by package directory (the dirname
# of the file in the fingerprint), and lists a package once it holds enough of
# them or its oldest has waited long enough. A malformed p3: line stops
# batches with exit 3 for the same reason.
#
# Exit codes: 2 usage, 3 an input the script cannot trust. Never read a
# non-zero exit as a quiet day.
set -euo pipefail

DEFAULT_WINDOW_SECONDS=86400
DEFAULT_SLICE=15
DEFAULT_SLICE_BYTES=150000
DEFAULT_BATCH_MIN=3
DEFAULT_BATCH_MAX_AGE_DAYS=7
DEFAULT_BATCH_MAX=6
REVIEW_COOLDOWN_DAYS=21
RISK_HISTORY="60.days.ago"

usage() {
  cat >&2 <<'EOF'
usage:
  deep-review.sh plan --landings FILE --prev-notes FILE --repo DIR
                      [--ref REF] [--slice N] [--slice-bytes N] [--now RFC3339]
                      [--out DIR]
  deep-review.sh seen --prev-notes FILE FINGERPRINT
  deep-review.sh queued --prev-notes FILE
  deep-review.sh batches --prev-notes FILE [--now RFC3339] [--min N]
                         [--max-age-days D] [--max-per-batch M]
EOF
  exit 2
}

die() { echo "deep-review.sh: $*" >&2; exit 3; }

# RFC3339 -> [epoch seconds, nanoseconds padded to 9 digits]; arrays compare
# in order, so fractional seconds and UTC offsets of any width order correctly.
JQ_TS='
def ts:
  capture("^(?<d>[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2})(?:\\.(?<f>[0-9]+))?(?<z>Z|[+-][0-9]{2}:[0-9]{2})$")
  | ((.d + "Z") | fromdateiso8601) as $e
  | (if .z == "Z" then 0
     else ((.z[1:3] | tonumber) * 3600 + (.z[4:6] | tonumber) * 60) * (if .z[0:1] == "-" then -1 else 1 end)
     end) as $off
  | [$e - $off, (((.f // "") + "000000000")[0:9])];
# capture emits nothing on a non-match, so default to null
def tsok: if type == "string" then ((try ts catch null) // null) else null end;
'

valid_ts() { jq -n -e --arg t "$1" "$JQ_TS"'($t | tsok) != null' >/dev/null; }

cmd_seen() {
  local notes=""
  while [ $# -gt 0 ]; do
    case "$1" in
      --prev-notes) notes="${2:-}"; shift 2 || usage ;;
      *) break ;;
    esac
  done
  [ -f "$notes" ] && [ $# -eq 1 ] || usage
  FP="$1" awk '
    BEGIN { a = "finding: " ENVIRON["FP"] " "; b = "queued: " ENVIRON["FP"] " " }
    index($0, a) == 1 || index($0, b) == 1 { found = 1 }
    index($0, "p3: ") == 1 { split(substr($0, 5), f, " "); if (f[2] == ENVIRON["FP"]) found = 1 }
    END { exit !found }
  ' "$notes"
}

cmd_queued() {
  local notes=""
  while [ $# -gt 0 ]; do
    case "$1" in
      --prev-notes) notes="${2:-}"; shift 2 || usage ;;
      *) usage ;;
    esac
  done
  [ -f "$notes" ] || usage
  awk '
    NR == FNR { if (index($0, "finding: ") == 1) { split(substr($0, 10), f, " "); filed[f[1]] = 1 } next }
    index($0, "queued: ") == 1 {
      n = split(substr($0, 9), f, " ")
      if (n < 4 || f[2] !~ /^(P1|P2|P3)$/) {
        printf "deep-review.sh: malformed queued line: %s\n", $0 > "/dev/stderr"
        exit 3
      }
      if (!(f[1] in filed) && !(f[1] in shown)) { shown[f[1]] = 1; print }
    }
  ' "$notes" "$notes"
}

# batches: unfiled p3: lines grouped by package directory. A p3: line is filed
# once a finding: line names its fingerprint. A fingerprint on several p3: lines
# counts once, at its oldest date. A directory is listed when it holds MIN
# unfiled lines or its oldest is at least MAX_AGE days old (dates are UTC days,
# so "at least D days old" is a date on or before today minus D). Within a
# batch the oldest lines come first and only MAX_PER lines are printed; the rest
# wait for the next run. Blocks come out in directory byte order.
cmd_batches() {
  local notes="" now="" min="$DEFAULT_BATCH_MIN" max_age="$DEFAULT_BATCH_MAX_AGE_DAYS" max_per="$DEFAULT_BATCH_MAX"
  while [ $# -gt 0 ]; do
    case "$1" in
      --prev-notes) notes="${2:-}"; shift 2 || usage ;;
      --now) now="${2:-}"; shift 2 || usage ;;
      --min) min="${2:-}"; shift 2 || usage ;;
      --max-age-days) max_age="${2:-}"; shift 2 || usage ;;
      --max-per-batch) max_per="${2:-}"; shift 2 || usage ;;
      *) usage ;;
    esac
  done
  [ -f "$notes" ] || usage
  case "$min" in ''|*[!0-9]*) usage ;; esac
  case "$max_age" in ''|*[!0-9]*) usage ;; esac
  case "$max_per" in ''|*[!0-9]*) usage ;; esac
  [ "$min" -gt 0 ] && [ "$max_per" -gt 0 ] || usage
  [ -n "$now" ] || now="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  valid_ts "$now" || die "--now is not RFC3339: $now"

  local cutoff rows
  cutoff="$(jq -n -r --arg now "$now" "$JQ_TS"'(($now | ts)[0] - '"$max_age"' * 86400) | strftime("%Y-%m-%d")')"
  # Rows are held until the whole file has parsed, so a malformed line prints nothing.
  rows="$(awk '
    NR == FNR { if (index($0, "finding: ") == 1) { split(substr($0, 10), f, " "); filed[f[1]] = 1 } next }
    index($0, "p3: ") == 1 {
      n = split(substr($0, 5), f, " ")
      if (n < 4 || f[1] !~ /^[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]$/ || f[2] !~ /^[^:]+:.+:[^:]+$/) {
        printf "deep-review.sh: malformed p3 line: %s\n", $0 > "/dev/stderr"
        bad = 1; exit 3
      }
      fp = f[2]
      if (fp in filed) next
      if (!(fp in date)) order[++count] = fp
      if (!(fp in date) || f[1] < date[fp]) { date[fp] = f[1]; line[fp] = $0 }
    }
    END {
      if (bad) exit 3
      for (i = 1; i <= count; i++) {
        fp = order[i]
        file = substr(fp, 1, index(fp, ":") - 1)
        dir = (file ~ /\//) ? file : "./" file
        sub(/\/[^\/]*$/, "", dir)
        printf "%s\t%s\t%d\t%s\n", dir, date[fp], i, line[fp]
      }
    }
  ' "$notes" "$notes")" || exit 3
  [ -n "$rows" ] || return 0
  printf '%s\n' "$rows" | LC_ALL=C sort -t "$(printf '\t')" -k1,1 -k2,2 -k3,3n | awk -F'\t' -v min="$min" -v cap="$max_per" -v cutoff="$cutoff" '
    function flush(   i) {
      if (!have) return
      if (cnt >= min || oldest <= cutoff) {
        print "batch " dir
        for (i = 1; i <= cnt && i <= cap; i++) print kept[i]
      }
    }
    $1 != dir || !have { flush(); have = 1; dir = $1; oldest = $2; cnt = 0 }
    { cnt++; kept[cnt] = $0; sub(/^[^\t]*\t[^\t]*\t[^\t]*\t/, "", kept[cnt]) }
    END { flush() }
  '
}

# newest cursor line in the notes: prints "<timestamp>\t<path>", or nothing
newest_cursor() {
  jq -R -s -r "$JQ_TS"'
    split("\n") | map(select(startswith("cursor: ")))
    | map((capture("^cursor: (?<t>\\S+) (?<p>.+)$") // error("malformed cursor line: " + .))
          | if (.t | tsok) == null then error("bad timestamp in cursor line: " + .t) else . end)
    | if length == 0 then empty else max_by(.t | ts) | "\(.t)\t\(.p)" end
  ' "$1"
}

# build_slice REPO REF NOTES NOW OUT SLICE BYTES: writes $OUT/slice.txt (paths),
# slice-rows.txt (tier, review date, score, bytes, path) and reviewed-lines.txt.
#
# Score per non-test Go file under internal/ at REF, from one git log pass and
# two tree-wide calls, never one call per file (the tree has ~930 files):
#   3*fix_commits + commits + min(5, lines/200) + 4 if no sibling _test.go
# fix_commits are commits since RISK_HISTORY whose subject starts with fix.
# Files reviewed within REVIEW_COOLDOWN_DAYS of NOW (UTC date) form tier 1 and
# are skipped; only when every file is tier 1 does the slice fall back to it,
# least recently reviewed first. The slice stops at the first file that would
# push it past BYTES or SLICE, and always keeps its first file so one file
# larger than the budget cannot empty a non-quiet day's slice. Temp files, not
# pipes into head: head closing early would SIGPIPE the writer and pipefail
# would abort on a file list the size of the real tree.
build_slice() {
  local repo="$1" ref="$2" notes="$3" now="$4" out="$5" slice="$6" budget="$7"
  local cutoff today rc=0
  cutoff="$(jq -n -r --arg now "$now" "$JQ_TS"'(($now | ts)[0] - '"$REVIEW_COOLDOWN_DAYS"' * 86400) | strftime("%Y-%m-%d")')"
  today="$(jq -n -r --arg now "$now" "$JQ_TS"'($now | ts)[0] | strftime("%Y-%m-%d")')"

  git -C "$repo" ls-tree -r -l "$ref" -- internal > "$out/tree.txt" || die "cannot list internal/ at $ref"
  awk -F'\t' '$2 ~ /\.go$/ && $2 !~ /_test\.go$/ { print $2 }' "$out/tree.txt" | LC_ALL=C sort > "$out/all-files.txt"
  [ -s "$out/all-files.txt" ] || die "no non-test Go files under internal/ at $ref"
  # git grep exits 1 when no line matches, which is a tree of empty files
  git -C "$repo" grep -c '' "$ref" -- internal > "$out/line-counts.txt" || rc=$?
  [ "$rc" -le 1 ] || die "cannot count lines under internal/ at $ref"
  git -C "$repo" log --since="$RISK_HISTORY" --name-only --format=@%s "$ref" -- internal > "$out/history.txt" \
    || die "cannot read the history of internal/ at $ref"

  awk -F'\t' -v tree="$out/tree.txt" -v lines="$out/line-counts.txt" -v hist="$out/history.txt" -v cutoff="$cutoff" '
    FILENAME == tree {
      if ($2 !~ /\.go$/) next
      isgo[$2] = 1
      if ($2 !~ /_test\.go$/) { k = split($1, m, " "); size[$2] = m[k] + 0 }
      next
    }
    FILENAME == lines {
      r = substr($0, index($0, ":") + 1)
      if (match(r, /:[0-9]+$/)) nlines[substr(r, 1, RSTART - 1)] = substr(r, RSTART + 1) + 0
      next
    }
    FILENAME == hist {
      if (substr($0, 1, 1) == "@") { fix = (tolower(substr($0, 2, 3)) == "fix"); next }
      if ($0 == "") next
      commits[$0]++
      if (fix) fixes[$0]++
      next
    }
    /^reviewed: [0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9] ./ {
      d = substr($0, 11, 10); p = substr($0, 22)
      if (!(p in rev) || d > rev[p]) rev[p] = d
    }
    END {
      for (p in size) {
        t = substr(p, 1, length(p) - 3) "_test.go"
        s = 3 * fixes[p] + commits[p] + (nlines[p] / 200 > 5 ? 5 : nlines[p] / 200) + ((t in isgo) ? 0 : 4)
        recent = (p in rev) && ((rev[p] "") >= (cutoff ""))
        printf "%d\t%s\t%.4f\t%d\t%s\n", recent ? 1 : 0, recent ? rev[p] : "0000-00-00", s, size[p], p
      }
    }
  ' "$out/tree.txt" "$out/line-counts.txt" "$out/history.txt" "$notes" \
    | LC_ALL=C sort -t "$(printf '\t')" -k1,1n -k2,2 -k3,3nr -k5 > "$out/ranked.txt"

  awk -F'\t' -v n="$slice" -v budget="$budget" '
    NR == 1 { tier = $1 }
    $1 != tier || taken >= n || (taken > 0 && bytes + $4 > budget) { exit }
    { print; bytes += $4; taken++ }
  ' "$out/ranked.txt" > "$out/slice-rows.txt"
  cut -f5 "$out/slice-rows.txt" > "$out/slice.txt"
  awk -v today="$today" '{ print "reviewed: " today " " $0 }' "$out/slice.txt" > "$out/reviewed-lines.txt"
}

cmd_plan() {
  local landings="" notes="" repo="" ref="HEAD" slice="$DEFAULT_SLICE" slice_bytes="$DEFAULT_SLICE_BYTES" now="" out=""
  while [ $# -gt 0 ]; do
    case "$1" in
      --landings) landings="${2:-}"; shift 2 || usage ;;
      --prev-notes) notes="${2:-}"; shift 2 || usage ;;
      --repo) repo="${2:-}"; shift 2 || usage ;;
      --ref) ref="${2:-}"; shift 2 || usage ;;
      --slice) slice="${2:-}"; shift 2 || usage ;;
      --slice-bytes) slice_bytes="${2:-}"; shift 2 || usage ;;
      --now) now="${2:-}"; shift 2 || usage ;;
      --out) out="${2:-}"; shift 2 || usage ;;
      *) usage ;;
    esac
  done
  [ -f "$landings" ] && [ -f "$notes" ] && [ -d "$repo" ] || usage
  case "$slice" in ''|*[!0-9]*) usage ;; esac
  [ "$slice" -gt 0 ] || usage
  case "$slice_bytes" in ''|*[!0-9]*) usage ;; esac
  [ "$slice_bytes" -gt 0 ] || usage
  [ -n "$now" ] || now="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  valid_ts "$now" || die "--now is not RFC3339: $now"
  [ -n "$out" ] || out="$(mktemp -d)"
  mkdir -p "$out"

  local entry since cursor
  entry="$(newest_cursor "$notes")" || die "unreadable cursor in $notes"
  if [ -n "$entry" ]; then
    since="${entry%%$'\t'*}"
    cursor="${entry#*$'\t'}"
  else
    since="$(jq -n -r --arg now "$now" "$JQ_TS"'($now | ts)[0] - '"$DEFAULT_WINDOW_SECONDS"' | todate')"
    cursor=""
  fi

  # Classify every landings line. A quiet verdict is only as good as the
  # parse behind it, so a line that is JSON but lacks a usable landed_at (the
  # record shape changed) or has a malformed risk_paths stops the plan. A line
  # that is not JSON is tolerated only as the last line, a torn append.
  jq -R -c "$JQ_TS"'
    input_line_number as $n
    | if test("^[[:space:]]*$") then empty
      else (try fromjson catch null) as $o
      | if $o == null then {k: "bad", n: $n}
        elif ($o | type) != "object" then {k: "drift", n: $n}
        elif ($o.landed_at | tsok) == null then {k: "drift", n: $n}
        elif ($o | has("risk_paths")) and (($o.risk_paths | type) != "array") and ($o.risk_paths != null) then {k: "drift", n: $n}
        else {k: "ok", n: $n, bead: $o.bead, head: $o.head, landed_commit: $o.landed_commit,
              landed_at: $o.landed_at, risk_paths: ($o.risk_paths // [])}
        end
      end
  ' "$landings" > "$out/classified.jsonl" || die "cannot parse $landings"

  local total ok drift bad stray
  total="$(awk 'END { print NR }' "$landings")"
  ok="$(jq -s '[.[] | select(.k == "ok")] | length' "$out/classified.jsonl")"
  drift="$(jq -s '[.[] | select(.k == "drift")] | length' "$out/classified.jsonl")"
  bad="$(jq -s '[.[] | select(.k == "bad")] | length' "$out/classified.jsonl")"
  stray="$(jq -s --argjson last "$total" '[.[] | select(.k == "bad" and .n != $last)] | length' "$out/classified.jsonl")"
  [ "$drift" -eq 0 ] || die "$drift line(s) of $landings are JSON without a usable landed_at or risk_paths; the record shape may have changed"
  [ "$stray" -eq 0 ] || die "$stray line(s) of $landings before the last are not JSON"
  [ "$ok" -gt 0 ] || die "$landings holds no readable landing record"

  # Risky landings after the cursor, oldest first.
  jq -c --arg since "$since" "$JQ_TS"'
    select(.k == "ok" and (.risk_paths | length) > 0 and (.landed_at | ts) > ($since | ts))
    | {bead, head, landed_commit, landed_at, risk_paths}
  ' "$out/classified.jsonl" | jq -s -c "$JQ_TS"'sort_by(.landed_at | ts) | .[]' > "$out/landings.jsonl"

  local n newest
  n="$(awk 'END { print NR }' "$out/landings.jsonl")"
  if [ "$n" -gt 0 ]; then
    newest="$(jq -s -r "$JQ_TS"'max_by(.landed_at | ts) | .landed_at' "$out/landings.jsonl")"
    echo "quiet=false"
  else
    newest="$since"
    echo "quiet=true"
  fi
  echo "landings=$n"
  echo "records=$ok"
  echo "unparsed=$bad"
  echo "reviewed_until_old=$since"
  echo "reviewed_until_new=$newest"

  # Rotation slice, skipped on a quiet day. The cursor path no longer picks
  # files; it is echoed so the ledger's cursor: line keeps its shape.
  : > "$out/slice.txt"
  : > "$out/slice-rows.txt"
  : > "$out/reviewed-lines.txt"
  if [ "$n" -gt 0 ]; then
    build_slice "$repo" "$ref" "$notes" "$now" "$out" "$slice" "$slice_bytes"
    echo "rotation_cursor_old=$cursor"
    echo "rotation_cursor_new=$(tail -n 1 "$out/slice.txt")"
  fi
  echo "slice_files=$(awk 'END { print NR }' "$out/slice.txt")"
  echo "slice_bytes=$(awk -F'\t' '{ s += $4 } END { print s + 0 }' "$out/slice-rows.txt")"
  echo "out=$out"
}

[ $# -ge 1 ] || usage
sub="$1"; shift
case "$sub" in
  plan) cmd_plan "$@" ;;
  seen) cmd_seen "$@" ;;
  queued) cmd_queued "$@" ;;
  batches) cmd_batches "$@" ;;
  *) usage ;;
esac
