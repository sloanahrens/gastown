#!/usr/bin/env bash
# Tests for scripts/docs-lint.sh: one violation per rule in the fixture tree,
# exact finding lines, tier listing, deterministic slice ordering, word total.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
LINT="$SCRIPT_DIR/docs-lint.sh"
FIXTURE="$SCRIPT_DIR/testdata/docs-lint"

TMP=""
PASS=0
FAIL=0
cleanup() { [[ -n "$TMP" && -d "$TMP" ]] && rm -rf "$TMP"; true; }
trap cleanup EXIT

assert_eq() {
  local name="$1" expected="$2" actual="$3"
  if [[ "$expected" == "$actual" ]]; then
    echo "  PASS: $name"; PASS=$((PASS + 1))
  else
    echo "  FAIL: $name"; echo "    expected:"; printf '%s\n' "$expected" | sed 's/^/      /'
    echo "    actual:"; printf '%s\n' "$actual" | sed 's/^/      /'; FAIL=$((FAIL + 1))
  fi
}

# A git repo is needed for --slice; commit files with fixed dates so the
# least-recently-modified order is known.
setup_repo() {
  cleanup
  TMP="$(mktemp -d)"
  cp -R "$FIXTURE"/. "$TMP"/
  git -C "$TMP" init -q
  git -C "$TMP" config user.email t@t; git -C "$TMP" config user.name t
  commit_at() { # commit_at <date> <paths...>
    local d="$1"; shift
    git -C "$TMP" add "$@"
    GIT_AUTHOR_DATE="$d" GIT_COMMITTER_DATE="$d" git -C "$TMP" commit -q -m "$*" --allow-empty
  }
  commit_at 2026-01-01T00:00:00Z docs/design/live.md internal/pkg/b.go
  commit_at 2026-02-01T00:00:00Z docs/HOOKS.md internal/pkg/a.go
  commit_at 2026-03-01T00:00:00Z .
}

# A root with no AGENTS.md, plugins/, internal/ or docs/: every glob a tier
# scans matches nothing. Reuses cleanup's mktemp-and-trap, like setup_repo.
setup_globless_root() {
  cleanup
  TMP="$(mktemp -d)"
}

echo "docs-lint: findings"
setup_repo
set +e
actual="$(DOCS_LINT_ROOT="$TMP" bash "$LINT" 2>&1)"; rc=$?
set -e
expected="docs/guides/guide.md:4: dead-link: ../missing.md does not exist
docs/guides/guide.md:6: dead-make-target: make nope is not a Makefile target
docs/guides/marked.md:5: stray-markup: committed tool-call or merge-conflict markup
docs/guides/marked.md:6: stray-markup: committed tool-call or merge-conflict markup
docs/guides/marked.md:7: stray-markup: committed tool-call or merge-conflict markup
docs/plans/conflict.md:1: status-conflict: more than one \"> Status:\" line
docs/plans/old-plan.md:1: status-header: first non-blank line must start with \"> Status:\"
internal/formula/formulas/mol-release.formula.toml:12: polecat-main-push: pushes main/master and offers a polecat; the guards refuse a polecat session (gt-ibt8)
plugins/p/plugin.md:1: word-ceiling: 2102 words, ceiling 2000"
assert_eq "finding lines" "$expected" "$actual"
assert_eq "exit 1 on findings" "1" "$rc"

echo "docs-lint: clean tree exits 0"
rm "$TMP/plugins/p/plugin.md"
rm "$TMP/internal/formula/formulas/mol-release.formula.toml"
rm "$TMP/docs/guides/marked.md" "$TMP/docs/plans/conflict.md"
printf '> Status: historical (2026-01). Abandoned: none. Not maintained.\n\n# Old plan\n' > "$TMP/docs/plans/old-plan.md"
sed -i.bak -e '/missing.md/d' -e '/make nope/d' "$TMP/docs/guides/guide.md" && rm "$TMP/docs/guides/guide.md.bak"
set +e; DOCS_LINT_ROOT="$TMP" bash "$LINT" >/dev/null 2>&1; rc=$?; set -e
assert_eq "exit 0 when clean" "0" "$rc"

echo "docs-lint: an empty glob is an empty tier, not a failure (gt-et39)"
# With the formula glob unmatched, the check pipeline's non-zero status used to
# reach run_checks' assignment, where set -e aborted the run: a tree full of
# findings reported exit 1 with no findings listed, and a clean tree reported
# exit 1 with nothing to explain it.
setup_globless_root
printf '# Agents\n\nSee [missing](docs/nope.md).\n' > "$TMP/AGENTS.md"
set +e
actual="$(DOCS_LINT_ROOT="$TMP" bash "$LINT" 2>&1)"; rc=$?
set -e
assert_eq "a finding still prints with an empty formula glob" \
  "AGENTS.md:3: dead-link: docs/nope.md does not exist" "$actual"
assert_eq "and the verdict is that finding" "1" "$rc"

printf '# Agents\n\nNo links here.\n' > "$TMP/AGENTS.md"
set +e
actual="$(DOCS_LINT_ROOT="$TMP" bash "$LINT" 2>&1)"; rc=$?
set -e
assert_eq "a clean tree with empty globs is silent" "" "$actual"
assert_eq "and exits 0" "0" "$rc"

assert_eq "agent-facing tier lists what is there" "AGENTS.md" \
  "$(DOCS_LINT_ROOT="$TMP" bash "$LINT" --list agent-facing)"
set +e
actual="$(DOCS_LINT_ROOT="$TMP" bash "$LINT" --list go)"; rc=$?
set -e
assert_eq "go tier is empty without cmd/ or internal/" "" "$actual"
assert_eq "and exits 0" "0" "$rc"

echo "docs-lint: --list"
setup_repo
assert_eq "historical tier" "docs/design/old.md
docs/plans/conflict.md
docs/plans/old-plan.md
docs/research/survey.md" "$(DOCS_LINT_ROOT="$TMP" bash "$LINT" --list historical)"
assert_eq "reference tier" "README.md
docs/design/live.md
docs/guides/guide.md
docs/guides/marked.md" "$(DOCS_LINT_ROOT="$TMP" bash "$LINT" --list reference)"
assert_eq "agent-facing tier" "AGENTS.md
docs/HOOKS.md
internal/formula/formulas/mol-release.formula.toml
internal/formula/formulas/mol-x.formula.toml
internal/templates/polecat-CLAUDE.md
plugins/p/plugin.md" "$(DOCS_LINT_ROOT="$TMP" bash "$LINT" --list agent-facing)"
assert_eq "go tier excludes tests" "internal/pkg/a.go
internal/pkg/b.go" "$(DOCS_LINT_ROOT="$TMP" bash "$LINT" --list go)"

echo "docs-lint: --slice is least-recently-modified first, docs then go"
assert_eq "slice 2 1" "docs/design/live.md
docs/HOOKS.md
internal/pkg/b.go" "$(DOCS_LINT_ROOT="$TMP" bash "$LINT" --slice 2 1)"

echo "docs-lint: --words"
words="$(DOCS_LINT_ROOT="$TMP" bash "$LINT" --words)"
if [[ "$words" =~ ^[0-9]+$ ]]; then assert_eq "words is a number" "ok" "ok"; else assert_eq "words is a number" "a number" "$words"; fi

echo
echo "passed: $PASS failed: $FAIL"
[[ "$FAIL" -eq 0 ]]
