#!/usr/bin/env bash
# deep-review.sh - deterministic helper for the daily mol-deep-review run.
#
# The formula's orchestrator decides nothing this script can decide:
#   plan    whether the day is quiet, which landings to review, which rotation
#           slice to read, from the ledger the previous runs left in bead notes
#   seen    whether a finding fingerprint is already in the ledger
#   queued  confirmed findings an earlier run recorded but never filed
#   batches unfiled P3 findings grouped per package, ready to file as one bead
#   gate    whether the risky-landings job has anything to do (no LLM)
#   docs-audit  which living docs look out of date, scored without an LLM
#
# plan --mode splits the run into two jobs: slice (the hourly rotation slice,
# no landings, no cursor moved) and landings (the risky landings, no slice).
# The default, both, is the original single run.
#
# In slice mode, once the slice is built, plan runs the review-targeting
# battery when daemon.patrols.jev_targets.enabled is true: it runs
# `gt jev review-targets`, which scores each slice file and logs the answers
# beside the rank. Its output goes to stderr and its exit status is ignored, so
# the slice is the same whether the battery runs, fails or is off.
#
# docs-audit scores every living doc at a ref (README.md, CLAUDE.md, AGENTS.md,
# .beads/PRIME.md, docs/*.md, docs/concepts, docs/guides, internal/**/README.md;
# the plans, design, adr and research records are point-in-time and never
# audited) from three counts. dead_paths: backticked paths under internal, cmd,
# scripts, docs, test or tests that the tree no longer holds. dead_cmds:
# backticked `gt <word>` or `bd <word>` that the binary does not know (not
# counted when the binary is not on PATH). A command the top-level --help lists
# is known. One it does not list is asked `tool word --help` and is dead only
# when the answer says unknown command. At most 20 such calls run per audit,
# best-scoring docs first, and a candidate past the cap is not counted dead.
# code_changed: commits of
# the last 60 days that touched a file the doc cites, made after the doc's own
# last commit, so a doc committed with its code does not count. plan, whenever
# it builds the slice, then writes docs.txt: living docs that cite a slice file
# or its package directory (6 at most), then the 3 highest-scoring docs without a
# reviewed: line from the last 21 days, 8 docs at most. docs.txt is empty in
# landings mode and on a quiet day, like the slice.
#
# Ledger lines, written into the run bead's notes by the formula:
#   cursor: <RFC3339 landed_at of the newest landing covered> [<last slice path>]
#     (the path is optional: an empty slice leaves it empty, and it is only echoed)
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
# A queued: or p3: line whose fields do not parse is skipped: queued and
# batches warn naming it, print how many they skipped, and list the rest. An
# earlier run wrote the line, so refusing the whole ledger would stop every
# later run on it, and a parser that rejects the line cannot recover the finding
# from it either way. A p3: line is a P3 finding held back so a run does not
# file one bead per P3. batches groups the ones no finding: line names yet by
# package directory (the dirname of the file in the fingerprint), and lists a
# package once it holds enough of them or its oldest has waited long enough.
#
# A rig other than gastown runs this same script from the gastown checkout: the
# caller names that rig's landings file, run label and repo, and the script reads
# nothing else of the rig. A rig with no tier sweep gates on --green-from-landings
# (see cmd_gate) instead of --green-state.
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
DEFAULT_GATE_MIN_COUNT=3
DEFAULT_GATE_MIN_AGE_MIN=20
REVIEW_COOLDOWN_DAYS=21
RISK_HISTORY="60.days.ago"
DOC_HISTORY_DAYS=60
DOC_PAIR_MAX=6
DOC_STALE_TOP=3
DOC_CMD_CHECKS=20
DOC_MAX=8

usage() {
  cat >&2 <<'EOF'
usage:
  deep-review.sh plan --landings FILE --prev-notes FILE --repo DIR
                      [--ref REF] [--slice N] [--slice-bytes N] [--now RFC3339]
                      [--out DIR] [--mode slice|landings|both] [--run BEAD]
  deep-review.sh docs-audit --repo DIR [--ref REF] [--now RFC3339]
                            [--prev-notes FILE] [--out DIR]
  deep-review.sh gate --landings FILE --label LABEL [--repo DIR] [--ref REF]
                      [--now RFC3339] [--min-count N] [--min-age-min M]
                      [--green-state PATH | --green-from-landings]
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

# jev_targets_enabled: true when daemon.patrols.jev_targets.enabled is true.
# The key lives in settings/config.json's "daemon" section on a two-file town
# and at the top level of the legacy mayor/daemon.json. Any other state,
# including a config that does not parse, leaves the battery off: it is
# opt-in, because every scored file is a paid Jev call.
jev_targets_enabled() {
  local town="${GT_TOWN_ROOT:-$HOME/gt}"
  local host="$town/settings/config.json" legacy="$town/mayor/daemon.json"
  if [ -f "$host" ] && jq -e 'has("daemon")' "$host" >/dev/null 2>&1; then
    jq -e '.daemon.patrols.jev_targets.enabled == true' "$host" >/dev/null 2>&1
  elif [ -f "$legacy" ]; then
    jq -e '.patrols.jev_targets.enabled == true' "$legacy" >/dev/null 2>&1
  else
    return 1
  fi
}

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
        skipped++
        next
      }
      if (!(f[1] in filed) && !(f[1] in shown)) { shown[f[1]] = 1; print }
    }
    END {
      if (skipped)
        printf "deep-review.sh: skipped %d malformed queued line%s; the ledger cannot be trusted\n", skipped, (skipped == 1 ? "" : "s") > "/dev/stderr"
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
  rows="$(awk '
    NR == FNR { if (index($0, "finding: ") == 1) { split(substr($0, 10), f, " "); filed[f[1]] = 1 } next }
    index($0, "p3: ") == 1 {
      n = split(substr($0, 5), f, " ")
      if (n < 4 || f[1] !~ /^[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]$/ || f[2] !~ /^[^:]+:.+:[^:]+$/) {
        printf "deep-review.sh: malformed p3 line: %s\n", $0 > "/dev/stderr"
        skipped++
        next
      }
      fp = f[2]
      if (fp in filed) next
      if (!(fp in date)) order[++count] = fp
      if (!(fp in date) || f[1] < date[fp]) { date[fp] = f[1]; line[fp] = $0 }
    }
    END {
      if (skipped)
        printf "deep-review.sh: skipped %d malformed p3 line%s; the ledger cannot be trusted\n", skipped, (skipped == 1 ? "" : "s") > "/dev/stderr"
      for (i = 1; i <= count; i++) {
        fp = order[i]
        file = substr(fp, 1, index(fp, ":") - 1)
        dir = (file ~ /\//) ? file : "./" file
        sub(/\/[^\/]*$/, "", dir)
        printf "%s\t%s\t%d\t%s\n", dir, date[fp], i, line[fp]
      }
    }
  ' "$notes" "$notes")"
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

# newest cursor line in the notes: prints "<timestamp>\t<path>", or nothing.
# The path is optional: an empty slice leaves it empty, so "cursor: <timestamp>"
# (with or without a trailing space) parses to that timestamp and an empty path.
newest_cursor() {
  jq -R -s -r "$JQ_TS"'
    split("\n") | map(select(startswith("cursor: ")))
    | map((capture("^cursor: (?<t>\\S+)(?: (?<p>.*))?$") // error("malformed cursor line: " + .))
          | .p = (.p // "")
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

# help_commands TOOL OUT: writes $OUT/TOOL-cmds.txt, the top-level subcommands
# TOOL --help lists. Empty when no doc cites TOOL, when it is not on PATH or
# when its help lists nothing, and then the audit does not judge its commands.
help_commands() {
  local tool="$1" out="$2"
  : > "$out/$tool-cmds.txt"
  awk -F'\t' -v t="$tool" '$2 == "c" { split($3, a, " "); if (a[1] == t) f = 1 } END { exit !f }' "$out/doc-refs.tsv" || return 0
  command -v "$tool" >/dev/null 2>&1 || return 0
  { "$tool" --help 2>&1 || true; } | awk '/^  [a-z][a-z0-9_-]* +[^ ]/ { print $1 }' | LC_ALL=C sort -u > "$out/$tool-cmds.txt"
}

# score_docs OUT HAVE_GT HAVE_BD: writes $OUT/docs-audit.txt, one line per
# living doc, "<score> <doc> dead_paths=<n> dead_cmds=<n> code_changed=<n>",
# highest score first, ties by path. A cited command is dead when the top-level
# listing lacks it and $OUT/cmd-ok.txt does not name it.
score_docs() {
  local out="$1" have_gt="$2" have_bd="$3"
  awk -F'\t' -v have_gt="$have_gt" -v have_bd="$have_bd" \
    -v tree="$out/doc-tree.txt" -v gtc="$out/gt-cmds.txt" -v bdc="$out/bd-cmds.txt" -v ok="$out/cmd-ok.txt" \
    -v living="$out/living-docs.txt" -v refs="$out/doc-refs.tsv" -v hist="$out/doc-history.txt" '
    FILENAME == tree {
      isfile[$0] = 1
      d = $0
      while (sub(/\/[^\/]*$/, "", d)) isdir[d] = 1
      next
    }
    FILENAME == gtc { cmd["gt " $1] = 1; next }
    FILENAME == bdc { cmd["bd " $1] = 1; next }
    FILENAME == ok { cmd[$0] = 1; next }
    FILENAME == living { order[++ndocs] = $0; next }
    FILENAME == refs {
      if ($2 == "p") {
        if (isfile[$3]) { cited[$1, ++ncited[$1]] = $3 }
        else if (!isdir[$3]) deadp[$1]++
      } else {
        split($3, a, " ")
        if ((a[1] == "gt" ? have_gt : have_bd) && !($3 in cmd)) deadc[$1]++
      }
      next
    }
    FILENAME == hist {
      if (substr($0, 1, 1) == "@") { idx++; next }
      if ($0 == "") next
      touched[$0] = touched[$0] " " idx
      if (!($0 in newest)) newest[$0] = idx
      next
    }
    END {
      # idx counts commits newest first, so a smaller idx is a later commit
      for (i = 1; i <= ndocs; i++) {
        d = order[i]
        limit = (d in newest) ? newest[d] : 1000000000
        cc = 0
        for (j = 1; j <= ncited[d]; j++) {
          m = split(touched[cited[d, j]], a, " ")
          for (k = 1; k <= m; k++) {
            ix = a[k] + 0
            if (ix < limit && !((d, ix) in marked)) { marked[d, ix] = 1; cc++ }
          }
        }
        printf "%d %s dead_paths=%d dead_cmds=%d code_changed=%d\n", 5 * deadp[d] + 5 * deadc[d] + (cc > 10 ? 10 : cc), d, deadp[d] + 0, deadc[d] + 0, cc
      }
    }
  ' "$out/doc-tree.txt" "$out/gt-cmds.txt" "$out/bd-cmds.txt" "$out/cmd-ok.txt" "$out/living-docs.txt" "$out/doc-refs.tsv" "$out/doc-history.txt" \
    | LC_ALL=C sort -k1,1nr -k2,2 > "$out/docs-audit.txt"
}

# confirm_commands OUT HAVE_GT HAVE_BD: writes $OUT/cmd-ok.txt, the cited
# commands the audit must not count dead, and $OUT/cmd-checks.txt, one
# "<tool> <word> alive|dead|skipped" line per candidate. A candidate is a cited
# command the top-level listing lacks, taken from the docs in the order of
# docs-audit.txt, which scored every candidate dead. Each of the first
# DOC_CMD_CHECKS is asked `tool word --help` and is dead only when the answer
# says unknown command; any other answer, a failure included, is alive. The
# rest are skipped and count alive. Only --help is ever passed, so a command
# that exists is never run.
confirm_commands() {
  local out="$1" have_gt="$2" have_bd="$3" tool word help verdict n=0
  : > "$out/cmd-ok.txt"
  : > "$out/cmd-checks.txt"
  while read -r tool word; do
    if [ "$n" -ge "$DOC_CMD_CHECKS" ]; then
      verdict=skipped
    else
      n=$((n + 1))
      help="$({ "$tool" "$word" --help </dev/null 2>&1 || true; })"
      case "$help" in
        *'unknown command "'*) verdict=dead ;;
        *) verdict=alive ;;
      esac
    fi
    echo "$tool $word $verdict" >> "$out/cmd-checks.txt"
    [ "$verdict" = dead ] || echo "$tool $word" >> "$out/cmd-ok.txt"
  done < <(awk -F'\t' -v have_gt="$have_gt" -v have_bd="$have_bd" \
    -v gtc="$out/gt-cmds.txt" -v bdc="$out/bd-cmds.txt" -v audit="$out/docs-audit.txt" -v refs="$out/doc-refs.tsv" '
    FILENAME == gtc { cmd["gt " $1] = 1; next }
    FILENAME == bdc { cmd["bd " $1] = 1; next }
    FILENAME == audit { split($0, a, " "); order[++ndocs] = a[2]; next }
    $2 == "c" { cited[$1, ++ncited[$1]] = $3 }
    END {
      for (i = 1; i <= ndocs; i++) {
        d = order[i]
        for (j = 1; j <= ncited[d]; j++) {
          c = cited[d, j]
          split(c, a, " ")
          if ((a[1] == "gt" ? have_gt : have_bd) && !(c in cmd) && !(c in seen)) { seen[c] = 1; print c }
        }
      }
    }
  ' "$out/gt-cmds.txt" "$out/bd-cmds.txt" "$out/docs-audit.txt" "$out/doc-refs.tsv")
}

# build_docs_audit REPO REF NOW OUT: writes $OUT/docs-audit.txt, one line per
# living doc, "<score> <doc> dead_paths=<n> dead_cmds=<n> code_changed=<n>",
# highest score first, ties by path. $OUT/doc-refs.tsv (doc, p|c, value) is the
# citation list build_docs reads. Cost: one ls-tree, one git show per doc, one
# log pass, at most two listing --help calls and at most DOC_CMD_CHECKS
# confirming ones, whatever the number of docs.
build_docs_audit() {
  local repo="$1" ref="$2" now="$3" out="$4" since tool d
  local have_gt=0 have_bd=0

  git -C "$repo" ls-tree -r --name-only "$ref" > "$out/doc-tree.txt" || die "cannot list the tree at $ref"
  awk '
    $0 == "README.md" || $0 == "CLAUDE.md" || $0 == "AGENTS.md" || $0 == ".beads/PRIME.md" { print; next }
    /^docs\/[^\/]+\.md$/ { print; next }
    /^docs\/(concepts|guides)\/.*\.md$/ { print; next }
    /^internal\/.*\/README\.md$/ { print }
  ' "$out/doc-tree.txt" | LC_ALL=C sort > "$out/living-docs.txt"

  # the docs in one stream, each behind a \001 marker line
  while IFS= read -r d; do
    printf '\001%s\n' "$d"
    git -C "$repo" show "$ref:$d" || die "cannot read $d at $ref"
    printf '\n'
  done < "$out/living-docs.txt" > "$out/doc-text.txt"

  # Citations are inline code spans outside fenced blocks. A path is the first
  # word of a span that starts with a tracked root; globs and placeholders are
  # skipped and #anchors and :line suffixes stripped. A command is a span that
  # starts with gt or bd and a lowercase word, so flags and <placeholders> skip.
  awk '
    function emit(kind, val,   k) {
      k = doc SUBSEP kind SUBSEP val
      if (!(k in seen)) { seen[k] = 1; printf "%s\t%s\t%s\n", doc, kind, val }
    }
    substr($0, 1, 1) == "\001" { doc = substr($0, 2); fence = 0; next }
    /^[ \t]*```/ { fence = !fence; next }
    fence { next }
    {
      line = $0
      while (match(line, /`[^`]+`/)) {
        tok = substr(line, RSTART + 1, RLENGTH - 2)
        line = substr(line, RSTART + RLENGTH)
        sub(/^[ \t]+/, "", tok)
        n = split(tok, w, /[ \t]+/)
        if (tok ~ /^(internal|cmd|scripts|docs|test|tests)\//) {
          p = w[1]
          if (p ~ /[*?<>{}$]/ || index(p, "[") || index(p, "]") || index(p, "...")) continue
          sub(/#.*$/, "", p)
          sub(/:[0-9]+(-[0-9]+)?(:[0-9]+)?$/, "", p)
          sub(/\/+$/, "", p)
          emit("p", p)
        } else if ((w[1] == "gt" || w[1] == "bd") && n >= 2 && w[2] ~ /^[a-z][a-z0-9_-]*$/) {
          emit("c", w[1] " " w[2])
        }
      }
    }
  ' "$out/doc-text.txt" > "$out/doc-refs.tsv"

  help_commands gt "$out"; [ ! -s "$out/gt-cmds.txt" ] || have_gt=1
  help_commands bd "$out"; [ ! -s "$out/bd-cmds.txt" ] || have_bd=1

  since="$(jq -n -r --arg now "$now" "$JQ_TS"'(($now | ts)[0] - '"$DOC_HISTORY_DAYS"' * 86400) | todate')"
  git -C "$repo" log --since="$since" --name-only --format=@%H "$ref" > "$out/doc-history.txt" \
    || die "cannot read the history at $ref"

  # First pass: every command the listing lacks is dead. Second pass, only when
  # a candidate exists: the ones --help knows are not.
  : > "$out/cmd-ok.txt"
  score_docs "$out" "$have_gt" "$have_bd"
  confirm_commands "$out" "$have_gt" "$have_bd"
  if [ -s "$out/cmd-ok.txt" ]; then score_docs "$out" "$have_gt" "$have_bd"; fi
}

# build_docs NOTES NOW OUT: writes $OUT/docs.txt from docs-audit.txt, doc-refs.tsv
# and slice.txt. First the living docs that cite a slice file or its package
# directory, best score first, DOC_PAIR_MAX at most; then the DOC_STALE_TOP
# best-scoring docs with no reviewed: line since the cooldown cutoff, whether or
# not already paired; no doc twice, DOC_MAX in all.
build_docs() {
  local notes="$1" now="$2" out="$3" cutoff
  cutoff="$(jq -n -r --arg now "$now" "$JQ_TS"'(($now | ts)[0] - '"$REVIEW_COOLDOWN_DAYS"' * 86400) | strftime("%Y-%m-%d")')"
  awk -F'\t' -v slice="$out/slice.txt" -v audit="$out/docs-audit.txt" -v refs="$out/doc-refs.tsv" \
    -v cutoff="$cutoff" -v pair_max="$DOC_PAIR_MAX" -v stale_top="$DOC_STALE_TOP" -v max="$DOC_MAX" '
    FILENAME == slice { inslice[$0] = 1; d = $0; sub(/\/[^\/]*$/, "", d); inslice[d] = 1; next }
    FILENAME == audit { split($0, a, " "); order[++ndocs] = a[2]; next }
    FILENAME == refs { if ($2 == "p" && ($3 in inslice)) paired[$1] = 1; next }
    /^reviewed: [0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9] ./ {
      dt = substr($0, 11, 10); p = substr($0, 22)
      if (!(p in rev) || dt > rev[p]) rev[p] = dt
    }
    END {
      for (i = 1; i <= ndocs && np < pair_max; i++) {
        d = order[i]
        if (paired[d] && printed < max) { print d; chosen[d] = 1; printed++; np++ }
      }
      for (i = 1; i <= ndocs && ns < stale_top; i++) {
        d = order[i]
        if ((d in rev) && (rev[d] "") >= (cutoff "")) continue
        ns++
        if (!(d in chosen) && printed < max) { print d; chosen[d] = 1; printed++ }
      }
    }
  ' "$out/slice.txt" "$out/docs-audit.txt" "$out/doc-refs.tsv" "$notes" > "$out/docs.txt"
}

# ledger_since NOTES NOW: sets SINCE (the newest cursor's timestamp, or NOW minus
# the default window) and CURSOR (that cursor's path, empty without one).
ledger_since() {
  local notes="$1" now="$2" entry
  entry="$(newest_cursor "$notes")" || die "unreadable cursor in $notes"
  if [ -n "$entry" ]; then
    SINCE="${entry%%$'\t'*}"
    CURSOR="${entry#*$'\t'}"
  else
    SINCE="$(jq -n -r --arg now "$now" "$JQ_TS"'($now | ts)[0] - '"$DEFAULT_WINDOW_SECONDS"' | todate')"
    CURSOR=""
  fi
}

# classify_landings LANDINGS OUT: writes $OUT/classified.jsonl and sets LD_OK and
# LD_BAD. A quiet verdict is only as good as the parse behind it, so a line that
# is JSON but lacks a usable landed_at (the record shape changed) or has a
# malformed risk_paths stops the caller. A line that is not JSON is tolerated
# only as the last line, a torn append.
classify_landings() {
  local landings="$1" out="$2" total drift stray
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

  total="$(awk 'END { print NR }' "$landings")"
  LD_OK="$(jq -s '[.[] | select(.k == "ok")] | length' "$out/classified.jsonl")"
  drift="$(jq -s '[.[] | select(.k == "drift")] | length' "$out/classified.jsonl")"
  LD_BAD="$(jq -s '[.[] | select(.k == "bad")] | length' "$out/classified.jsonl")"
  stray="$(jq -s --argjson last "$total" '[.[] | select(.k == "bad" and .n != $last)] | length' "$out/classified.jsonl")"
  [ "$drift" -eq 0 ] || die "$drift line(s) of $landings are JSON without a usable landed_at or risk_paths; the record shape may have changed"
  [ "$stray" -eq 0 ] || die "$stray line(s) of $landings before the last are not JSON"
  [ "$LD_OK" -gt 0 ] || die "$landings holds no readable landing record"
}

# risky_landings OUT SINCE: writes $OUT/landings.jsonl, the classified landings
# with risk paths that landed after SINCE, oldest first. Shared by plan and gate
# so the gate's count is the count the review job will see.
risky_landings() {
  local out="$1" since="$2"
  jq -c --arg since "$since" "$JQ_TS"'
    select(.k == "ok" and (.risk_paths | length) > 0 and (.landed_at | ts) > ($since | ts))
    | {bead, head, landed_commit, landed_at, risk_paths}
  ' "$out/classified.jsonl" | jq -s -c "$JQ_TS"'sort_by(.landed_at | ts) | .[]' > "$out/landings.jsonl"
}

# plan runs as two independent jobs. --mode slice builds only the rotation
# slice: it never reads the landings, and reviewed_until_new is the cursor it
# started from, so no landing cursor moves. --mode landings builds only the
# risky-landings list and an empty slice. --mode both (the default) is the
# original single run: the slice is skipped on a quiet day.
cmd_plan() {
  local landings="" notes="" repo="" ref="HEAD" slice="$DEFAULT_SLICE" slice_bytes="$DEFAULT_SLICE_BYTES" now="" out="" mode="both" run=""
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
      --mode) mode="${2:-}"; shift 2 || usage ;;
      --run) run="${2:-}"; shift 2 || usage ;;
      *) usage ;;
    esac
  done
  case "$mode" in slice|landings|both) ;; *) usage ;; esac
  [ -f "$notes" ] || usage
  if [ "$mode" != slice ]; then [ -f "$landings" ] || usage; fi
  if [ "$mode" != landings ]; then [ -d "$repo" ] || usage; fi
  case "$slice" in ''|*[!0-9]*) usage ;; esac
  [ "$slice" -gt 0 ] || usage
  case "$slice_bytes" in ''|*[!0-9]*) usage ;; esac
  [ "$slice_bytes" -gt 0 ] || usage
  [ -n "$now" ] || now="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  valid_ts "$now" || die "--now is not RFC3339: $now"
  [ -n "$out" ] || out="$(mktemp -d)"
  mkdir -p "$out"

  ledger_since "$notes" "$now"
  local since="$SINCE" cursor="$CURSOR" n=0 newest="$SINCE" ok=0 bad=0 quiet=false
  : > "$out/landings.jsonl"
  if [ "$mode" != slice ]; then
    classify_landings "$landings" "$out"
    ok="$LD_OK"; bad="$LD_BAD"
    risky_landings "$out" "$since"
    n="$(awk 'END { print NR }' "$out/landings.jsonl")"
    if [ "$n" -gt 0 ]; then
      newest="$(jq -s -r "$JQ_TS"'max_by(.landed_at | ts) | .landed_at' "$out/landings.jsonl")"
    else
      quiet=true
    fi
  fi
  echo "mode=$mode"
  echo "quiet=$quiet"
  echo "landings=$n"
  echo "records=$ok"
  echo "unparsed=$bad"
  echo "reviewed_until_old=$since"
  echo "reviewed_until_new=$newest"

  # Rotation slice: skipped in landings mode and, in both mode, on a quiet day.
  # The cursor path no longer picks files; it is echoed so the ledger's cursor:
  # line keeps its shape.
  : > "$out/slice.txt"
  : > "$out/slice-rows.txt"
  : > "$out/reviewed-lines.txt"
  : > "$out/docs.txt"
  if [ "$mode" = slice ] || { [ "$mode" = both ] && [ "$n" -gt 0 ]; }; then
    build_slice "$repo" "$ref" "$notes" "$now" "$out" "$slice" "$slice_bytes"
    echo "rotation_cursor_old=$cursor"
    echo "rotation_cursor_new=$(tail -n 1 "$out/slice.txt")"
    build_docs_audit "$repo" "$ref" "$now" "$out"
    build_docs "$notes" "$now" "$out"
  fi
  echo "docs=$(awk 'END { print NR }' "$out/docs.txt")"
  echo "slice_files=$(awk 'END { print NR }' "$out/slice.txt")"
  echo "slice_bytes=$(awk -F'\t' '{ s += $4 } END { print s + 0 }' "$out/slice-rows.txt")"
  echo "out=$out"

  # Review-targeting battery. It reads the slice that was just built and the
  # history that ranked it, writes its own log, and prints to stderr, so plan's
  # stdout and the slice files do not depend on it.
  if [ "$mode" = slice ] && [ -n "$run" ] && jev_targets_enabled; then
    gt jev review-targets --run "$run" --ranked "$out/slice-rows.txt" \
      --history "$out/history.txt" --repo "$repo" --ref "$ref" >&2 || true
  fi
}

# docs-audit: the audit alone. --prev-notes is accepted so a caller can pass
# plan's flags; the audit reads no ledger.
cmd_docs_audit() {
  local repo="" ref="HEAD" now="" out="" notes=""
  while [ $# -gt 0 ]; do
    case "$1" in
      --repo) repo="${2:-}"; shift 2 || usage ;;
      --ref) ref="${2:-}"; shift 2 || usage ;;
      --now) now="${2:-}"; shift 2 || usage ;;
      --out) out="${2:-}"; shift 2 || usage ;;
      --prev-notes) notes="${2:-}"; shift 2 || usage ;;
      *) usage ;;
    esac
  done
  [ -d "$repo" ] || usage
  [ -z "$notes" ] || [ -f "$notes" ] || usage
  [ -n "$now" ] || now="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  valid_ts "$now" || die "--now is not RFC3339: $now"
  [ -n "$out" ] || out="$(mktemp -d)"
  mkdir -p "$out"
  build_docs_audit "$repo" "$ref" "$now" "$out"
  echo "docs=$(awk 'END { print NR }' "$out/docs-audit.txt")"
  echo "out=$out"
}

# read_green_state PATH REPO: reads the tier sweep record at PATH and, when it
# holds a full green sweep of REPO, sets GREEN_SHA to its last_green_sha. Every
# other outcome sets GREEN_REASON and returns 1: a sweep that cannot be read
# closes the gate instead of opening it, and a tip REPO does not hold cannot
# order any landing against it.
read_green_state() {
  local path="$1" repo="$2" line verdict reason
  [ -f "$path" ] || { GREEN_REASON="no state file"; return 1; }
  [ -r "$path" ] || { GREEN_REASON="$path cannot be read"; return 1; }
  line="$(jq -r '
    if type != "object" then error("not an object") else . end
    | (if (.last_green_sha | type) == "string" then .last_green_sha else "" end
       | gsub("^[[:space:]]+|[[:space:]]+$"; "")) as $sha
    | ((.tiers | objects) // {}) as $t
    | (($t.shell | objects | .verdict) // "missing") as $shell
    | (($t.integration | objects | .verdict) // "missing") as $integ
    | (($t.race | objects | .verdict) // "missing") as $race
    | if $sha == "" then "skip\tno last_green_sha"
      elif $shell != "GREEN" then "skip\tshell tier is \($shell)"
      elif $integ != "GREEN" then "skip\tintegration tier is \($integ)"
      elif $race != "GREEN" then "skip\trace tier is \($race)"
      else "run\t\($sha)"
      end
  ' "$path" 2>/dev/null)" || { GREEN_REASON="$path is not readable JSON"; return 1; }
  IFS=$'\t' read -r verdict reason <<< "$line"
  if [ "$verdict" != run ]; then
    GREEN_REASON="${reason:-no green sweep recorded}"
    return 1
  fi
  git -C "$repo" rev-parse --verify --quiet "$reason^{commit}" >/dev/null 2>&1 \
    || { GREEN_REASON="last_green_sha ${reason:0:8} is not a commit in $repo"; return 1; }
  GREEN_SHA="$reason"
}

# read_green_landings LANDINGS REPO: for a rig with no tier sweep, takes the last
# landing record in LANDINGS whose gate_result starts with "pass" and whose target
# is main as the rig's last green gate on main, and sets GREEN_SHA to its
# landed_commit. A file holding no such record, or a commit REPO does not hold,
# sets GREEN_REASON and returns 1: a gate that cannot name a green commit skips
# and never runs. A line that is not JSON is ignored here; classify_landings
# judges the file.
read_green_landings() {
  local landings="$1" repo="$2" sha
  sha="$(jq -R -r '
    (try fromjson catch null)
    | select(type == "object"
             and ((.gate_result | type) == "string") and (.gate_result | startswith("pass"))
             and ((.target // "main") == "main")
             and ((.landed_commit | type) == "string") and (.landed_commit | length) > 0)
    | .landed_commit
  ' "$landings" | tail -n 1)" || { GREEN_REASON="$landings cannot be read"; return 1; }
  [ -n "$sha" ] || { GREEN_REASON="no landing record with a passing gate on main"; return 1; }
  git -C "$repo" rev-parse --verify --quiet "$sha^{commit}" >/dev/null 2>&1 \
    || { GREEN_REASON="last green landing ${sha:0:8} is not a commit in $repo"; return 1; }
  GREEN_SHA="$sha"
}

# gate: does the landings job have anything to do? Reads the run history itself
# (bd list, both shapes of bd --json as the formula's step 1 does), finds the
# risky landings after the newest cursor with the code plan uses, and decides
# from their count and the age of the oldest. Exit 0 run, 1 skip, 3 an input it
# cannot trust. --ref is accepted so a caller can pass plan's flags; the gate
# reads no tree. --repo is the directory bd runs in.
#
# --green-state hands the gate a tier sweep record instead and makes that sweep
# the threshold: only a landing whose landed_commit the last full green sweep
# covered is counted, and one of them is enough whatever its age, so the review
# reads landings a red or partial sweep has not cleared. A record holding no
# green sweep is a skip, never a run.
#
# --green-from-landings is the same gate for a rig with no tier sweep: the rig's
# last green gate on main is its newest landing record whose gate_result passed,
# only landings that commit contains count, and the threshold is the same single
# landing. It is explicit: a landings file with no passing record is a skip. It
# excludes --green-state.
cmd_gate() {
  local landings="" label="" repo="." now="" green_state="" green_landings=0 min_count="" min_age=""
  while [ $# -gt 0 ]; do
    case "$1" in
      --landings) landings="${2:-}"; shift 2 || usage ;;
      --label) label="${2:-}"; shift 2 || usage ;;
      --repo) repo="${2:-}"; shift 2 || usage ;;
      --ref) [ $# -ge 2 ] || usage; shift 2 ;;
      --now) now="${2:-}"; shift 2 || usage ;;
      --min-count) [ -n "${2:-}" ] || usage; min_count="$2"; shift 2 || usage ;;
      --min-age-min) [ -n "${2:-}" ] || usage; min_age="$2"; shift 2 || usage ;;
      --green-state) [ -n "${2:-}" ] || usage; green_state="$2"; shift 2 || usage ;;
      --green-from-landings) green_landings=1; shift ;;
      *) usage ;;
    esac
  done
  [ -f "$landings" ] && [ -n "$label" ] && [ -d "$repo" ] || usage
  { [ "$green_landings" -eq 0 ] || [ -z "$green_state" ]; } || usage
  local gated=""
  if [ -n "$green_state" ] || [ "$green_landings" -eq 1 ]; then gated=1; fi
  local def_count="$DEFAULT_GATE_MIN_COUNT" def_age="$DEFAULT_GATE_MIN_AGE_MIN"
  if [ -n "$gated" ]; then
    def_count=1
    def_age=0
  fi
  [ -n "$min_count" ] || min_count="$def_count"
  [ -n "$min_age" ] || min_age="$def_age"
  case "$min_count" in ''|*[!0-9]*) usage ;; esac
  case "$min_age" in ''|*[!0-9]*) usage ;; esac
  [ -n "$now" ] || now="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  valid_ts "$now" || die "--now is not RFC3339: $now"

  # the sweep answers first: a caller that asks for a green gate and cannot see
  # one skips without asking bd anything
  local green=""
  if [ -n "$green_state" ]; then
    if ! read_green_state "$green_state" "$repo"; then
      echo "gate: skip (no green sweep: $GREEN_REASON)"
      exit 1
    fi
    green=" green=${GREEN_SHA:0:8}"
  elif [ "$green_landings" -eq 1 ]; then
    if ! read_green_landings "$landings" "$repo"; then
      echo "gate: skip (no green gate: $GREEN_REASON)"
      exit 1
    fi
    green=" green=${GREEN_SHA:0:8}"
  fi

  # global, not local: the EXIT trap runs after this function has returned
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' EXIT
  (cd "$repo" && bd list -l "$label" --all --limit 60 --json) > "$tmp/runs.json" || die "bd list -l $label failed"
  # slurped, so an empty output is an error and not an empty history
  jq -r -s '
    (if length == 1 then .[0] else error("expected one JSON document") end)
    | (if type == "object" then .data else . end)
    | if type == "array" then . else error("run history is not a list") end
    | .[] | (.notes // "") | if type == "string" then . else error("notes is not a string") end
  ' "$tmp/runs.json" > "$tmp/prev-notes.txt" || die "unreadable run history from bd list -l $label"

  ledger_since "$tmp/prev-notes.txt" "$now"
  classify_landings "$landings" "$tmp"
  risky_landings "$tmp" "$SINCE"

  # Only the landings the green tip (the sweep's, or the last green landing's)
  # covered are this run's to review. The records pass through the filter
  # unchanged; a landing that is not an ancestor of the tip, or one the repo
  # does not hold at all, drops.
  if [ -n "$gated" ]; then
    local line commit covered="$tmp/covered.jsonl"
    : > "$covered"
    while IFS= read -r line; do
      commit="$(jq -r '.landed_commit // ""' <<< "$line")"
      [ -n "$commit" ] || continue
      if git -C "$repo" merge-base --is-ancestor "$commit" "$GREEN_SHA" 2>/dev/null; then
        printf '%s\n' "$line" >> "$covered"
      fi
    done < "$tmp/landings.jsonl"
    mv "$covered" "$tmp/landings.jsonl"
  fi

  local n oldest=0
  n="$(awk 'END { print NR }' "$tmp/landings.jsonl")"
  if [ "$n" -gt 0 ]; then
    oldest="$(jq -s -r --arg now "$now" "$JQ_TS"'
      def secs: .[0] + ((.[1] | tonumber) / 1000000000);
      (($now | ts | secs) - (min_by(.landed_at | ts) | .landed_at | ts | secs)) as $s
      | (if $s < 0 then 0 else $s end) / 60 | floor' "$tmp/landings.jsonl")"
  fi
  if [ "$n" -gt 0 ] && { [ "$oldest" -ge "$min_age" ] || [ "$n" -ge "$min_count" ]; }; then
    echo "gate: run n=$n oldest=${oldest}m$green"
  else
    echo "gate: skip n=$n oldest=${oldest}m$green"
    exit 1
  fi
}

[ $# -ge 1 ] || usage
sub="$1"; shift
case "$sub" in
  plan) cmd_plan "$@" ;;
  seen) cmd_seen "$@" ;;
  queued) cmd_queued "$@" ;;
  batches) cmd_batches "$@" ;;
  gate) cmd_gate "$@" ;;
  docs-audit) cmd_docs_audit "$@" ;;
  *) usage ;;
esac
