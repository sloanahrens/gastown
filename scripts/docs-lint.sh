#!/usr/bin/env bash
# docs-lint.sh — deterministic checks on the docs and comments tiers, and the
# tier lists the weekly audit slices from. Rules: docs/writing-for-agents.md.
#
# Usage:
#   docs-lint.sh                 run all checks; one line per finding
#                                (path:line: rule: message); exit 1 on any,
#                                exit 2 when a check could not run
#   docs-lint.sh --list <tier>   agent-facing | reference | historical | go
#   docs-lint.sh --slice N M     N docs (agent-facing+reference) and M go files,
#                                least recently modified first (git log date,
#                                ties by path)
#   docs-lint.sh --words         total words across the agent-facing tier
#
# Env: DOCS_LINT_ROOT (repo root; default: this script's parent),
#      DOCS_LINT_CEILING (word ceiling for always-loaded files; default 2000).
set -euo pipefail

ROOT="${DOCS_LINT_ROOT:-$(cd "$(dirname "$0")/.." && pwd)}"
CEILING="${DOCS_LINT_CEILING:-2000}"
cd "$ROOT"

# --- tiers -------------------------------------------------------------------

first_nonblank() { grep -m1 -v '^[[:space:]]*$' "$1" 2>/dev/null || true; }

is_historical() {
  case "$1" in
    docs/plans/*|docs/research/*) return 0 ;;
    docs/design/*) [[ "$(first_nonblank "$1")" == "> Status:"* ]] ;;
    *) return 1 ;;
  esac
}

# glob_paths <pattern>...: emit the path each shell glob matches, and nothing
# when one matches nothing. `ls <pattern>` instead prints an error and exits
# non-zero on an unmatched glob, and as the last command of the agent-facing
# group that status rode `pipefail` up to run_checks' assignment, where `set -e`
# aborted the run before it printed a single finding (gt-et39). A glob that
# matches nothing is an empty tier, not a failure.
glob_paths() {
  local p
  for p in "$@"; do compgen -G "$p" || true; done
}

tier_agent_facing() {
  { [[ -f AGENTS.md ]] && echo AGENTS.md
    [[ -f internal/templates/polecat-CLAUDE.md ]] && echo internal/templates/polecat-CLAUDE.md
    [[ -f docs/HOOKS.md ]] && echo docs/HOOKS.md
    glob_paths 'plugins/*/plugin.md' 'internal/formula/formulas/*.formula.toml'
  } | LC_ALL=C sort
}

# Always-loaded files the gate holds to the ceiling (formulas are audited, not gated).
# sed, not `grep -v`: grep exits 1 when it filters every line, so a root with an
# empty agent-facing tier would take the same pipefail path the glob did (gt-et39).
tier_ceiling() { tier_agent_facing | sed '/\.formula\.toml$/d'; }

tier_historical() {
  find docs/plans docs/research docs/design -name '*.md' 2>/dev/null | sed 's#^\./##' | LC_ALL=C sort \
    | while IFS= read -r f; do is_historical "$f" && echo "$f"; done || true
}

tier_reference() {
  { [[ -f README.md ]] && echo README.md
    find docs -name '*.md' 2>/dev/null | sed 's#^\./##'
  } | LC_ALL=C sort | while IFS= read -r f; do
      [[ "$f" == docs/HOOKS.md ]] && continue
      is_historical "$f" || echo "$f"
    done || true
}

# `|| true` for the docs-only root: find exits 1 on a missing cmd/ or internal/,
# which is the same pipefail path out of `--list go` and `--slice` (gt-et39).
tier_go() { find cmd internal -name '*.go' -not -name '*_test.go' 2>/dev/null | sed 's#^\./##' | LC_ALL=C sort || true; }

list_tier() {
  case "$1" in
    agent-facing) tier_agent_facing ;;
    reference) tier_reference ;;
    historical) tier_historical ;;
    go) tier_go ;;
    *) echo "docs-lint: unknown tier '$1'" >&2; exit 2 ;;
  esac
}

# --- slice -------------------------------------------------------------------

lrm_sorted() { # stdin: paths; stdout: paths ordered by last-commit date, oldest first, ties by path
  while IFS= read -r f; do
    ts="$(git log -1 --format=%ct -- "$f" 2>/dev/null || true)"
    printf '%s %s\n' "${ts:-0}" "$f"
  done | LC_ALL=C sort -k1,1n -k2,2 | cut -d' ' -f2-
}

slice() {
  local n="$1" m="$2"
  { tier_agent_facing; tier_reference; } | LC_ALL=C sort -u | lrm_sorted | head -n "$n"
  tier_go | lrm_sorted | head -n "$m"
}

# --- checks ------------------------------------------------------------------

finding() { printf '%s:%s: %s: %s\n' "$1" "$2" "$3" "$4"; }

# fence_filter prints a file with every ``` fenced block removed (the fence
# delimiter lines included), keeping original line numbers. Code fences are
# code or fixtures, not prose: links and "> Status:" lines inside them are not
# findings.
fence_filter() { sed -e '/^```/,/^```/d' "$1" 2>/dev/null || true; }

check_dead_links() {
  { tier_agent_facing; tier_reference; tier_historical; } | grep '\.md$' | LC_ALL=C sort -u | while IFS= read -r f; do
    # Templates render at the repo root, so their relative links resolve from
    # there, not from their source location under internal/templates/.
    local base
    if [[ "$f" == internal/templates/* ]]; then base="$ROOT"; else base="$(dirname "$f")"; fi
    fence_filter "$f" | grep -onE '\[[^]]*\]\([^)]+\)' | while IFS=: read -r line match; do
      target="${match#*](}"; target="${target%)}"
      target="${target%% *}"           # drop a "title" after the path
      target="${target%%#*}"           # drop the fragment
      [[ -z "$target" ]] && continue
      case "$target" in http://*|https://*|mailto:*|*://*) continue ;; esac
      if [[ ! -e "$base/$target" ]]; then
        finding "$f" "$line" dead-link "$target does not exist"
      fi
    done
  done
}

check_dead_make_targets() {
  [[ -f Makefile ]] || return 0
  { tier_agent_facing; tier_reference; } | grep '\.md$' | LC_ALL=C sort -u | while IFS= read -r f; do
    grep -onE '`make [A-Za-z0-9_-]+' "$f" 2>/dev/null | while IFS=: read -r line match; do
      t="${match#\`make }"
      grep -qE "^${t}:" Makefile || finding "$f" "$line" dead-make-target "make $t is not a Makefile target"
    done
  done
}

check_status_headers() {
  find docs/plans docs/research -name '*.md' 2>/dev/null | sed 's#^\./##' | LC_ALL=C sort | while IFS= read -r f; do
    [[ "$(first_nonblank "$f")" == "> Status:"* ]] || finding "$f" 1 status-header 'first non-blank line must start with "> Status:"'
  done
}

# Files the stray-markup and status-conflict rules scan: the three doc tiers
# (agent-facing, reference, historical) for .md/.toml, plus the go tier for
# non-test .go. The go tier already excludes *_test.go (see tier_go).
markup_scan_files() {
  { tier_agent_facing; tier_reference; tier_historical; } | LC_ALL=C sort -u \
    | grep -E '\.(md|toml)$'
  tier_go
}

# stray-markup: a line that looks like a raw agent tool-call fragment or an
# unresolved merge-conflict marker is committed output, never intended prose.
check_stray_markup() {
  markup_scan_files | while IFS= read -r f; do
    grep -nE '<tool_call>|</tool_call>|<function=|<parameter=|^<<<<<<< |^>>>>>>> ' "$f" 2>/dev/null \
      | while IFS=: read -r line rest; do
          finding "$f" "$line" stray-markup 'committed tool-call or merge-conflict markup'
        done
  done
}

# status-conflict: a file carrying more than one "> Status:" line contradicts
# itself (e.g. an "In Progress" body under a "historical / Not maintained"
# header). One file, one status.
check_status_conflict() {
  { tier_agent_facing; tier_reference; tier_historical; } | grep '\.md$' | LC_ALL=C sort -u | while IFS= read -r f; do
    n="$(fence_filter "$f" | grep -c '^> Status:' || true)"
    [[ "${n:-0}" -gt 1 ]] && finding "$f" 1 status-conflict 'more than one "> Status:" line'
  done
}

# polecat-main-push: a formula that pushes the rig's default branch and also
# offers a `gt sling .../polecats/...` assignment is advertising a flow the
# guards refuse. A polecat session cannot push main: the tap guard
# (matchesPolecatMainPush) blocks the command before git runs and
# .githooks/pre-push refuses the push itself, both by design (gt-ibt8). The
# allow path a release has is a crew/mayor/refinery session (gt-deff), so a
# formula that pushes main must not offer a polecat.
#
# Scoped to formulas. plugin.md is exempt: the plugins that push a default
# branch run from the daemon, which sets GT_ROLE=daemon/plugin and is not a
# polecat caller, so their instructions are correct as written.
formula_pushes_default() { # <file>: does a command send work to main/master?
  # The refspec's DESTINATION decides, so "main:polecat/x" (a read from main)
  # is left alone while "HEAD:main" and ":main" are findings. A bare
  # --all/--mirror is out of scope: no formula writes one, and the guards
  # catch it at runtime regardless.
  grep -qE 'git[[:space:]]+(-[^[:space:]]+[[:space:]]+[^[:space:]]+[[:space:]]+)*push([[:space:]]+-[^[:space:]]+)*[[:space:]]+[^[:space:]]+[[:space:]]+([^[:space:]]*:)?(refs/heads/)?(main|master)([^[:alnum:]:_-]|$)' "$1" 2>/dev/null
}

check_polecat_main_push() {
  tier_agent_facing | grep '\.formula\.toml$' | while IFS= read -r f; do
    formula_pushes_default "$f" || continue
    grep -nE '\bsling[[:space:]]+[^[:space:]]*polecats/' "$f" 2>/dev/null \
      | while IFS=: read -r line rest; do
          finding "$f" "$line" polecat-main-push 'pushes main/master and offers a polecat; the guards refuse a polecat session (gt-ibt8)'
        done
  done
}

check_word_ceiling() {
  tier_ceiling | while IFS= read -r f; do
    w="$(wc -w < "$f" | tr -d ' ')"
    [[ "$w" -gt "$CEILING" ]] && finding "$f" 1 word-ceiling "$w words, ceiling $CEILING"
    true
  done
}

run_checks() {
  # Checks run in subshell pipelines, so the verdict is read from their output
  # and never from the pipeline's status. `|| rc=$?` keeps that status out of
  # `set -e`, which aborted at this assignment and printed none of the findings
  # the checks had already collected (gt-et39). A status that arrives with no
  # findings is a check that could not run, not a clean tree: it says so on
  # stderr rather than exiting 1 for a reader to misread as a verdict.
  local out rc=0
  out="$( { check_dead_links; check_dead_make_targets; check_status_headers; check_status_conflict; check_stray_markup; check_polecat_main_push; check_word_ceiling; } | LC_ALL=C sort -t: -k1,1 -k2,2n )" || rc=$?
  if [[ -n "$out" ]]; then printf '%s\n' "$out"; exit 1; fi
  if [[ "$rc" -ne 0 ]]; then
    echo "docs-lint: a check exited $rc and reported no findings" >&2
    exit 2
  fi
  exit 0
}

# --- main --------------------------------------------------------------------

case "${1:-}" in
  --list) list_tier "${2:?tier}" ;;
  --slice) slice "${2:?docs count}" "${3:?go count}" ;;
  --words) tier_agent_facing | xargs wc -w 2>/dev/null | tail -1 | awk '{print $1}' ;;
  "") run_checks ;;
  *) echo "docs-lint: unknown option '$1'" >&2; exit 2 ;;
esac
