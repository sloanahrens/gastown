#!/usr/bin/env bash
# deep-review.sh - deterministic helper for the daily mol-deep-review run.
#
# The formula's orchestrator decides nothing this script can decide:
#   plan    whether the day is quiet, which landings to review, which rotation
#           slice to read, from the ledger the previous runs left in bead notes
#   seen    whether a finding fingerprint is already in the ledger
#   queued  confirmed findings an earlier run recorded but never filed
#
# Ledger lines, written into the run bead's notes by the formula:
#   cursor: <RFC3339 landed_at of the newest landing covered> <last slice path>
#   finding: <fingerprint> <bead id | digest-only>
#   queued: <fingerprint> <P1|P2> <file:line> <claim>
# --prev-notes is the notes of recent run beads in any order. The cursor line
# with the newest timestamp wins, so the order and status of the run beads do
# not matter. A queued finding stays pending until a finding line names it.
#
# Exit codes: 2 usage, 3 an input the script cannot trust. Never read a
# non-zero exit as a quiet day.
set -euo pipefail

DEFAULT_WINDOW_SECONDS=86400
DEFAULT_SLICE=15

usage() {
  cat >&2 <<'EOF'
usage:
  deep-review.sh plan --landings FILE --prev-notes FILE --repo DIR
                      [--ref REF] [--slice N] [--now RFC3339] [--out DIR]
  deep-review.sh seen --prev-notes FILE FINGERPRINT
  deep-review.sh queued --prev-notes FILE
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
      split(substr($0, 9), f, " ")
      if (!(f[1] in filed) && !(f[1] in shown)) { shown[f[1]] = 1; print }
    }
  ' "$notes" "$notes"
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

cmd_plan() {
  local landings="" notes="" repo="" ref="HEAD" slice="$DEFAULT_SLICE" now="" out=""
  while [ $# -gt 0 ]; do
    case "$1" in
      --landings) landings="${2:-}"; shift 2 || usage ;;
      --prev-notes) notes="${2:-}"; shift 2 || usage ;;
      --repo) repo="${2:-}"; shift 2 || usage ;;
      --ref) ref="${2:-}"; shift 2 || usage ;;
      --slice) slice="${2:-}"; shift 2 || usage ;;
      --now) now="${2:-}"; shift 2 || usage ;;
      --out) out="${2:-}"; shift 2 || usage ;;
      *) usage ;;
    esac
  done
  [ -f "$landings" ] && [ -f "$notes" ] && [ -d "$repo" ] || usage
  case "$slice" in ''|*[!0-9]*) usage ;; esac
  [ "$slice" -gt 0 ] || usage
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

  # Rotation slice: the next N non-test Go files under internal/ after the
  # cursor path, wrapping. Skipped on a quiet day. One awk pass, no head in a
  # pipe: head closing early would SIGPIPE the writer and pipefail would abort
  # on a file list the size of the real tree.
  : > "$out/slice.txt"
  if [ "$n" -gt 0 ]; then
    git -C "$repo" ls-tree -r --name-only "$ref" -- internal \
      | awk '/\.go$/ && !/_test\.go$/' | LC_ALL=C sort > "$out/all-files.txt"
    [ -s "$out/all-files.txt" ] || die "no non-test Go files under internal/ at $ref"
    LC_ALL=C awk -v c="$cursor" -v n="$slice" '
      { a[NR] = $0 }
      END {
        for (i = 1; i <= NR && out < n; i++) if (c == "" || (a[i] "") > (c "")) { print a[i]; out++ }
        for (i = 1; i <= NR && out < n; i++) if (!(c == "" || (a[i] "") > (c ""))) { print a[i]; out++ }
      }
    ' "$out/all-files.txt" > "$out/slice.txt"
    echo "rotation_cursor_old=$cursor"
    echo "rotation_cursor_new=$(tail -n 1 "$out/slice.txt")"
  fi
  echo "slice_files=$(awk 'END { print NR }' "$out/slice.txt")"
  echo "out=$out"
}

[ $# -ge 1 ] || usage
sub="$1"; shift
case "$sub" in
  plan) cmd_plan "$@" ;;
  seen) cmd_seen "$@" ;;
  queued) cmd_queued "$@" ;;
  *) usage ;;
esac
