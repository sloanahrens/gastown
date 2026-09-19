#!/usr/bin/env bash
# docs-lint.sh — deterministic checks on the docs and comments tiers, and the
# tier lists the weekly audit slices from. Rules: docs/writing-for-agents.md.
#
# Usage:
#   docs-lint.sh                 run all checks; one line per finding
#                                (path:line: rule: message); exit 1 on any
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

tier_agent_facing() {
  { [[ -f AGENTS.md ]] && echo AGENTS.md
    [[ -f internal/templates/polecat-CLAUDE.md ]] && echo internal/templates/polecat-CLAUDE.md
    [[ -f docs/HOOKS.md ]] && echo docs/HOOKS.md
    ls plugins/*/plugin.md 2>/dev/null
    ls internal/formula/formulas/*.formula.toml 2>/dev/null
  } | LC_ALL=C sort
}

# Always-loaded files the gate holds to the ceiling (formulas are audited, not gated).
tier_ceiling() { tier_agent_facing | grep -v '\.formula\.toml$'; }

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

tier_go() { find cmd internal -name '*.go' -not -name '*_test.go' 2>/dev/null | sed 's#^\./##' | LC_ALL=C sort; }

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

check_dead_links() {
  { tier_agent_facing; tier_reference; tier_historical; } | grep '\.md$' | LC_ALL=C sort -u | while IFS= read -r f; do
    grep -onE '\[[^]]*\]\([^)]+\)' "$f" 2>/dev/null | while IFS=: read -r line match; do
      target="${match#*](}"; target="${target%)}"
      target="${target%% *}"           # drop a "title" after the path
      target="${target%%#*}"           # drop the fragment
      [[ -z "$target" ]] && continue
      case "$target" in http://*|https://*|mailto:*|*://*) continue ;; esac
      if [[ ! -e "$(dirname "$f")/$target" ]]; then
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

check_word_ceiling() {
  tier_ceiling | while IFS= read -r f; do
    w="$(wc -w < "$f" | tr -d ' ')"
    [[ "$w" -gt "$CEILING" ]] && finding "$f" 1 word-ceiling "$w words, ceiling $CEILING"
    true
  done
}

run_checks() {
  # Checks run in subshell pipelines, so the verdict is read from their output.
  out="$( { check_dead_links; check_dead_make_targets; check_status_headers; check_word_ceiling; } | LC_ALL=C sort -t: -k1,1 -k2,2n )"
  if [[ -n "$out" ]]; then printf '%s\n' "$out"; exit 1; fi
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
