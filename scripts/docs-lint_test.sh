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
plugins/p/plugin.md:1: word-ceiling: 2102 words, ceiling 1500"
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

echo "docs-lint: formulas are held to the ceiling unless exempt (gt-nj23.10)"
# The clean tree above plus two formulas over 1,500 words: one on
# CEILING_EXEMPT, one not. Only the second is a finding.
cp "$FIXTURE/plugins/p/plugin.md" "$TMP/internal/formula/formulas/mol-big.formula.toml"
cp "$FIXTURE/plugins/p/plugin.md" "$TMP/internal/formula/formulas/mol-polecat-work.formula.toml"
set +e
actual="$(DOCS_LINT_ROOT="$TMP" bash "$LINT" 2>&1)"; rc=$?
set -e
assert_eq "a formula over the ceiling is a finding; an exempt one is not" \
  "internal/formula/formulas/mol-big.formula.toml:1: word-ceiling: 2102 words, ceiling 1500" "$actual"
assert_eq "and exits 1" "1" "$rc"

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

echo "docs-lint: the word ceiling and --words ignore the caller's locale (gt-shrnc)"
# 1,498 words plus five lone em dashes. gt-shrnc was found where wc counted a lone
# em dash differently under en_US.UTF-8 and C.UTF-8, so the verdict depended on
# the caller's locale. The count must be the same whatever the caller sets, and
# each caller locale here is checked against the one total. On a host whose wc
# counts the dashes the same everywhere (this one) the loop cannot tell a pinned
# locale from an unpinned one; the case below, where no UTF-8 locale is installed,
# is what proves the run never falls back to the caller's.
setup_globless_root
awk 'BEGIN { for (i = 0; i < 1498; i++) printf "a "; for (i = 0; i < 5; i++) printf "\342\200\224 "; print "" }' > "$TMP/AGENTS.md"
ceiling_finding="AGENTS.md:1: word-ceiling: 1503 words, ceiling 1500"
for caller in C C.UTF-8 en_US.UTF-8; do
  # stderr is dropped: a host without this caller locale makes bash warn, and
  # the warning is not a finding.
  set +e
  findings="$(LC_ALL="$caller" DOCS_LINT_ROOT="$TMP" bash "$LINT" 2>/dev/null)"; rc=$?
  total="$(LC_ALL="$caller" DOCS_LINT_ROOT="$TMP" bash "$LINT" --words 2>/dev/null)"
  set -e
  assert_eq "the alarm is raised under LC_ALL=$caller" "$ceiling_finding" "$findings"
  assert_eq "and exits 1 under LC_ALL=$caller" "1" "$rc"
  assert_eq "--words is 1503 under LC_ALL=$caller" "1503" "$total"
done

echo "docs-lint: with no UTF-8 locale the count stops instead of unpinning (gt-shrnc)"
# A `locale` that reports the C charmap for every query stands in for a host
# with neither candidate installed; the run must not fall back to the caller's.
mkdir -p "$TMP/bin"
printf '#!/bin/sh\necho ANSI_X3.4-1968\n' > "$TMP/bin/locale"
chmod +x "$TMP/bin/locale"
set +e
err="$(PATH="$TMP/bin:$PATH" DOCS_LINT_ROOT="$TMP" bash "$LINT" 2>&1 >/dev/null)"; rc=$?
set -e
assert_eq "a host with no UTF-8 locale exits 2" "2" "$rc"
assert_eq "and names the locales it tried" \
  "docs-lint: cannot count words: neither C.UTF-8 nor en_US.UTF-8 is installed" "$err"

echo "docs-lint: a word count that fails stops the run and says why (gt-shrnc)"
# A wc that cannot count stands in for one that cannot read a file or prints no
# count. The stub counts normally unless its input carries a marker, so the first
# agent-facing file is counted and the second is not: a run that carried on, or
# printed what it had, would show it. AGENTS.md sorts before docs/HOOKS.md.
setup_globless_root
mkdir -p "$TMP/bin" "$TMP/docs"
real_wc="$(command -v wc)"
cat > "$TMP/bin/wc" <<STUB
#!/bin/sh
input="\$(cat)"
case "\$input" in
  *COUNT-FAILS*) exit 1 ;;
  *COUNT-GARBAGE*) echo "no count here"; exit 0 ;;
esac
printf '%s\n' "\$input" | $real_wc "\$@"
STUB
chmod +x "$TMP/bin/wc"
printf '# Agents\n\nthree words here\n' > "$TMP/AGENTS.md"
for marker in COUNT-FAILS COUNT-GARBAGE; do
  printf '# Hooks\n\n%s\n' "$marker" > "$TMP/docs/HOOKS.md"
  for mode in "" "--words"; do
    set +e
    out="$(PATH="$TMP/bin:$PATH" DOCS_LINT_ROOT="$TMP" bash "$LINT" $mode 2>"$TMP/stderr")"; rc=$?
    set -e
    err="$(cat "$TMP/stderr")"
    label="${mode:-the lint} with $marker"
    assert_eq "$label exits 2" "2" "$rc"
    assert_eq "$label prints nothing on stdout: no finding, no partial total" "" "$out"
    case "$err" in
      "docs-lint: cannot count words in docs/HOOKS.md"*) assert_eq "$label names the file it could not count" "ok" "ok" ;;
      *) assert_eq "$label names the file it could not count" "docs-lint: cannot count words in docs/HOOKS.md ..." "$err" ;;
    esac
  done
done

echo
echo "passed: $PASS failed: $FAIL"
[[ "$FAIL" -eq 0 ]]
