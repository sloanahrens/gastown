> Status: historical (2026-09). Superseded by: gt-nj23 (epic). Not maintained.

# Docs and Comments Hygiene Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** One standard for docs and comments, a deterministic lint gate, an om review criterion, a weekly audit polecat, and a generic daemon patrol that dispatches it.

**Architecture:** `docs/writing-for-agents.md` is the single source of truth; three pointers reach it (om `context_file`, the audit formula, the polecat CLAUDE.md template). `scripts/docs-lint.sh` runs inside `make lint` and also computes the audit's slice. `mol-doc-audit` is a polecat formula with the mol-polecat-work skeleton. A new `scheduled_slings` daemon patrol creates one bead per run and slings it with `--formula` and `--agent`; the open bead is the double-dispatch guard.

**Tech Stack:** Go (daemon, templates, formula validation), bash scripts with `_test.sh` harnesses, TOML formulas, JSON rig config.

**Spec:** `docs/plans/2026-09-19-docs-hygiene-design.md`

## Global Constraints

- Build and test only through the Makefile: `make build`, `make test`, `make lint`. Bare `go test ./...` fails on ICU cgo flags. Run the suite as `GOFLAGS=-p=6 make test`; a single package as `CGO_CPPFLAGS=-I$(brew --prefix icu4c)/include CGO_LDFLAGS=-L$(brew --prefix icu4c)/lib go test ./internal/daemon/ -run <Name>`.
- Never search from `/` or `$HOME`. Root every `find` and `grep` at the repo.
- No tests call a model. Shell tests run against fixtures under `scripts/testdata/`; Go tests use fakes.
- Word ceiling for the lint gate in v1: 2,000 words, applied to AGENTS.md, `internal/templates/polecat-CLAUDE.md`, `docs/HOOKS.md`, `plugins/*/plugin.md`. The standard's sprawl rule is 1,500 and applies to every agent-facing file via the audit.
- Historical tier: every file under `docs/plans` and `docs/research`, plus any `docs/design` file whose first non-blank line starts with `> Status:`.
- Status header format, exactly: `> Status: historical (<YYYY-MM>). <Superseded by | Merged in | Abandoned>: <ref>. Not maintained.`
- om criterion name `docs-and-comments`, weight 2. `context_file` is `docs/writing-for-agents.md`.
- Audit agent: `deepseek-pro`. Slice defaults: 8 docs, 4 Go files. Patrol label: `scheduled:<name>`. Bead title: `<name> <YYYY-MM-DD>`.
- Landing order (spec, Rollout): Task 1 must be on main before Task 5 is filed. Task 4 lands in the same MR as the `lint: docs-lint` wiring, or after Task 3 in an MR that wires it, never before.
- Every commit message references the bead id of the task.

---

## File map

| Path | Task | Responsibility |
|---|---|---|
| `docs/writing-for-agents.md` | 1 | The standard: flat, numbered, checkable rules |
| `internal/templates/polecat-CLAUDE.md` | 2 | One pointer line under a new short heading |
| `internal/templates/templates_test.go` | 2 | Asserts the pointer renders into a polecat CLAUDE.md |
| `scripts/docs-lint.sh` | 3 | Tier lists, four checks, `--list`, `--slice`, `--words` |
| `scripts/docs-lint_test.sh` | 3 | Fixture-driven shell test |
| `scripts/testdata/docs-lint/` | 3 | Mini repo tree with one violation per rule |
| `Makefile` | 3, 4 | `docs-lint` target; test-makefile entries; `lint` depends on `docs-lint` (Task 4) |
| `docs/plans/*.md`, `docs/research/*.md`, `docs/design/*.md` | 4 | Status headers and classification |
| `.om.json` | 5 | New criterion and `context_file` |
| `internal/formula/formulas/mol-doc-audit.formula.toml` | 6 | The audit formula |
| `internal/daemon/scheduled_slings.go` | 7, 8 | Config types, validation, decision function, bead JSON parsing, runner, dispatch |
| `internal/daemon/scheduled_slings_test.go` | 7, 8 | Table tests and fake-runner tests |
| `internal/daemon/types.go` | 7 | `ScheduledSlings` field on `PatrolsConfig`; opt-in branch in `IsPatrolEnabled` |
| `internal/daemon/daemon.go` | 8 | Ticker, select case, running flag |

---

### Task 1: The standard, `docs/writing-for-agents.md`

**Files:**
- Create: `docs/writing-for-agents.md`

**Interfaces:**
- Produces: the file path `docs/writing-for-agents.md` and rule ids `R1` to `R13`, which Task 5's guidance, Task 6's audit step, and Task 2's pointer name.

- [ ] **Step 1: Write the file**

```markdown
# Writing for agents

The standard for every document and comment in this repository. The audit applies every rule to every file in its slice; om weighs new and edited text against them; a polecat reads this before writing any comment, directive, formula, plugin.md, or doc.

Each rule is one checkable line. The paragraph under it says why. Rule ids are stable; cite them in commit messages and findings.

## Tiers

- **Agent-facing**: AGENTS.md, `internal/templates/polecat-CLAUDE.md`, `docs/HOOKS.md`, `plugins/*/plugin.md`, the description blocks of `internal/formula/formulas/*.toml`.
- **Reference**: README.md and every file under `docs/` that is not historical.
- **Historical**: every file under `docs/plans` and `docs/research`, and any `docs/design` file whose first non-blank line is a `> Status:` header.
- **Go**: doc comments on exported identifiers and inline comments in non-test `.go` files.

The authoritative file lists are `scripts/docs-lint.sh --list <tier>`. This section describes them; it does not duplicate them.

## Rules

### Pointer

**R1. A line that names another document states what it is and when to read it, trigger word first.**

The wording of a pointer, not its target, decides whether an agent reaches the material. "Before editing any formula, read X" fires; "see also X" does not.

### Single source of truth

**R2. Each meaning lives in exactly one place. A second copy becomes a pointer to the first.**

Two copies drift. The reader who finds the stale one has no way to know it is stale.

### Cache

**R3. A doc or comment that restates `--help` output, a config file, the Makefile, or the directory layout is deleted.**

The environment is the source of truth for those, and it cannot go stale. Keep what a lookup cannot find: the convention, the reason, the gotcha.

### No-op

**R4. A sentence the agent would already obey by default is deleted whole.**

It spends context to change nothing. Trim words from it and it still changes nothing.

### Sediment

**R5. A line describing behaviour that no longer exists is deleted, not amended.**

Adding feels safe and removing feels risky, so stale layers settle until the live line is buried. Deletion is the only cure.

### Sprawl

**R6. An agent-facing file over 1,500 words is split by branch, or its reference material moves behind a pointer.**

Attention thins across a long file. Inline what every reader needs; push behind a pointer what only some readers reach. (The lint gate enforces a looser 2,000 on the always-loaded files; this rule is the audit's target.)

### Completion criterion

**R7. Every step in a formula or directive ends with a condition an agent can check.**

"Understanding reached" invites stopping early. "Every modified file accounted for" forces the legwork.

### Negation

**R8. State the target behaviour. A prohibition stands only as a guardrail, with the positive form beside it.**

A ban names the thing it bans and makes it more available, not less.

### Comments

**R9. A comment says why. It states what the code visibly does only when the why is in the same sentence.**

The code already says what. A comment that repeats it is a cache (R3) that rots the first time the code changes.

**R10. A doc comment on an exported identifier states the contract in one sentence.**

Callers read the contract. Implementation detail belongs in the body.

**R11. Narrative history collapses to one bead-id citation. The citation replaces the story; it never accompanies it.**

`bd show` holds the story. `// kept at 60m so gate slots are not starved (gt-uoqg)` is complete; three sentences of what it used to be are sediment (R5).

### Historical docs

**R12. A historical doc starts with `> Status: historical (<YYYY-MM>). <Superseded by | Merged in | Abandoned>: <ref>. Not maintained.`**

The header is what stops an agent citing a 900-line proposal as current truth, and it is the classifier `scripts/docs-lint.sh` uses for the tier.

### The test

**R13. A line that changes nothing an agent or reader does is deleted.**

This is the rule behind the others. When a line survives every rule above, ask this one.

## Applying the rules

An audit run records, per file, which rule ids produced edits. A finding it cannot act on (a deletion proposal, rot outside its slice, a rule that needs sharpening) goes in the run bead comment, not in the file.
```

- [ ] **Step 2: Check the word count is under the ceiling this file will later be held to**

Run: `wc -w docs/writing-for-agents.md`
Expected: under 1,000.

- [ ] **Step 3: Commit**

```bash
git add docs/writing-for-agents.md
git commit -m "docs: add writing-for-agents standard (R1-R13) (<bead-id>)"
```

---

### Task 2: Pointer in the polecat CLAUDE.md template

**Files:**
- Modify: `internal/templates/polecat-CLAUDE.md` (insert before the `## Do NOT` heading, currently line 352)
- Test: `internal/templates/templates_test.go`

**Interfaces:**
- Consumes: `templates.CreatePolecatCLAUDEmd(worktreePath, rigName, polecatName string) (bool, error)` (`internal/templates/templates.go:239`).
- Produces: nothing new.

- [ ] **Step 1: Write the failing test**

Append to `internal/templates/templates_test.go`:

```go
func TestPolecatCLAUDEmd_PointsAtWritingForAgents(t *testing.T) {
	dir := t.TempDir()
	if _, err := CreatePolecatCLAUDEmd(dir, "gastown", "agate"); err != nil {
		t.Fatalf("CreatePolecatCLAUDEmd: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "CLAUDE.md"))
	if err != nil {
		t.Fatalf("read rendered CLAUDE.md: %v", err)
	}
	want := "if the repo has docs/writing-for-agents.md, read it"
	if !strings.Contains(string(data), want) {
		t.Fatalf("rendered CLAUDE.md lacks the writing-for-agents pointer %q", want)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `CGO_CPPFLAGS=-I$(brew --prefix icu4c)/include CGO_LDFLAGS=-L$(brew --prefix icu4c)/lib go test ./internal/templates/ -run TestPolecatCLAUDEmd_PointsAtWritingForAgents`
Expected: FAIL, "lacks the writing-for-agents pointer".

- [ ] **Step 3: Insert the section**

Insert immediately before the line `## Do NOT` in `internal/templates/polecat-CLAUDE.md`:

```markdown
## Docs and comments

Before writing or editing any comment, directive, formula, plugin.md, or doc: if the repo has docs/writing-for-agents.md, read it. The merge gate reviews against it.

```

- [ ] **Step 4: Run the test and the word count**

Run: the command from Step 2.
Expected: PASS.

Run: `wc -w internal/templates/polecat-CLAUDE.md`
Expected: under 2,000 (it was 1,920 before this change).

- [ ] **Step 5: Commit**

```bash
git add internal/templates/polecat-CLAUDE.md internal/templates/templates_test.go
git commit -m "templates: polecat CLAUDE.md points at docs/writing-for-agents.md when present (<bead-id>)"
```

---

### Task 3: `scripts/docs-lint.sh` with tests and a `docs-lint` make target

**Files:**
- Create: `scripts/docs-lint.sh`
- Create: `scripts/docs-lint_test.sh`
- Create: `scripts/testdata/docs-lint/` (fixture tree, listed below)
- Modify: `Makefile` (`.PHONY` line 1, new `docs-lint` target after `lint` at line 60, two lines in `test-makefile` at line 221)

**Interfaces:**
- Produces: `scripts/docs-lint.sh` with no args (run checks, exit 1 on findings), `--list <agent-facing|reference|historical|go>`, `--slice <docs> <go>`, `--words`. Env `DOCS_LINT_ROOT` (repo root, default the script's parent) and `DOCS_LINT_CEILING` (default 2000). Finding line format: `path:line: rule: message`. Task 6 consumes `--slice` and `--words`.
- This task does NOT make `lint` depend on `docs-lint`; Task 4 does, once main is green.

- [ ] **Step 1: Create the fixture tree**

```bash
mkdir -p scripts/testdata/docs-lint/{docs/plans,docs/research,docs/design,docs/guides,plugins/p,internal/templates,internal/formula/formulas,internal/pkg}
cd scripts/testdata/docs-lint
printf 'build:\n\t@true\n\ntest:\n\t@true\n' > Makefile
printf '# Agents\n\nShort and fine.\n' > AGENTS.md
printf '# Readme\n\nSee [the guide](docs/guides/guide.md).\n' > README.md
printf '# Hooks\n\nRun `make build` first.\n' > docs/HOOKS.md
printf '# Guide\n\nGood link: [hooks](../HOOKS.md).\nBad link: [missing](../missing.md).\nGood target: `make build`.\nBad target: `make nope`.\nProse that must not match: we make decisions here.\nExternal: [site](https://example.com) and [anchor](#guide).\n' > docs/guides/guide.md
printf '# Old plan\n\nNo header here.\n' > docs/plans/old-plan.md
printf '> Status: historical (2026-07). Merged in: abc123. Not maintained.\n\n# Research\n' > docs/research/survey.md
printf '> Status: historical (2026-07). Abandoned: gt-0000. Not maintained.\n\n# Old design\n' > docs/design/old.md
printf '# Live architecture\n\nStill true.\n' > docs/design/live.md
printf '# Polecat\n\nfine\n' > internal/templates/polecat-CLAUDE.md
printf 'description = """\nA formula. See [guide](../../../docs/guides/guide.md).\n"""\nformula = "mol-x"\nversion = 1\n' > internal/formula/formulas/mol-x.formula.toml
printf 'package pkg\n' > internal/pkg/a.go
printf 'package pkg\n' > internal/pkg/b.go
printf 'package pkg\n' > internal/pkg/a_test.go
# plugin.md over the ceiling: 2,100 words
{ printf '# Plugin\n\n'; for i in $(seq 1 2100); do printf 'word '; done; printf '\n'; } > plugins/p/plugin.md
cd -
```

- [ ] **Step 2: Write the failing test**

`scripts/docs-lint_test.sh`:

```bash
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
cleanup() { [[ -n "$TMP" && -d "$TMP" ]] && rm -rf "$TMP"; }
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

echo "docs-lint: findings"
setup_repo
set +e
actual="$(DOCS_LINT_ROOT="$TMP" bash "$LINT" 2>&1)"; rc=$?
set -e
expected="docs/guides/guide.md:4: dead-link: ../missing.md does not exist
docs/guides/guide.md:6: dead-make-target: make nope is not a Makefile target
docs/plans/old-plan.md:1: status-header: first non-blank line must start with \"> Status:\"
plugins/p/plugin.md:1: word-ceiling: 2102 words, ceiling 2000"
assert_eq "finding lines" "$expected" "$actual"
assert_eq "exit 1 on findings" "1" "$rc"

echo "docs-lint: clean tree exits 0"
rm "$TMP/plugins/p/plugin.md"
printf '> Status: historical (2026-01). Abandoned: none. Not maintained.\n\n# Old plan\n' > "$TMP/docs/plans/old-plan.md"
sed -i.bak -e '/missing.md/d' -e '/make nope/d' "$TMP/docs/guides/guide.md" && rm "$TMP/docs/guides/guide.md.bak"
set +e; DOCS_LINT_ROOT="$TMP" bash "$LINT" >/dev/null 2>&1; rc=$?; set -e
assert_eq "exit 0 when clean" "0" "$rc"

echo "docs-lint: --list"
setup_repo
assert_eq "historical tier" "docs/design/old.md
docs/plans/old-plan.md
docs/research/survey.md" "$(DOCS_LINT_ROOT="$TMP" bash "$LINT" --list historical)"
assert_eq "reference tier" "README.md
docs/design/live.md
docs/guides/guide.md" "$(DOCS_LINT_ROOT="$TMP" bash "$LINT" --list reference)"
assert_eq "agent-facing tier" "AGENTS.md
docs/HOOKS.md
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
```

- [ ] **Step 3: Run it to verify it fails**

Run: `bash scripts/docs-lint_test.sh`
Expected: fails at the first assertion because `scripts/docs-lint.sh` does not exist (bash reports "No such file").

- [ ] **Step 4: Write the script**

`scripts/docs-lint.sh`:

```bash
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
```

Notes for the implementer: `grep -on` prints `line:match`; a match containing a colon (a URL) still splits correctly because `read -r line match` takes everything after the first colon. The sort in `run_checks` orders findings by path then line so the test's expected block is stable. `wc -w` on the fixture plugin.md counts the heading, so 2,100 `word` tokens plus `#` and `Plugin` is 2,102, which the test expects.

- [ ] **Step 5: Run the test until it passes**

Run: `bash scripts/docs-lint_test.sh`
Expected: `passed: 9 failed: 0`. If a finding line differs by wording, fix the script, not the test, unless the test's expectation was wrong on inspection.

- [ ] **Step 6: Run the script against the real repo and record the counts**

Run: `bash scripts/docs-lint.sh | awk -F': ' '{print $2}' | sort | uniq -c`
Expected: `status-header` findings for every file under docs/plans and docs/research, at most a handful of `dead-make-target` (the baseline found `make test-changed`), few or no `dead-link`, no `word-ceiling`. Paste the counts into the bead notes: `bd update <bead-id> --notes "docs-lint baseline: <counts>"`. Task 4 fixes them.

- [ ] **Step 7: Add the make target and the test hooks**

In `Makefile`: append `docs-lint` to the `.PHONY` list on line 1. After the `lint:` recipe add:

```makefile
# Deterministic docs and comments checks (docs/writing-for-agents.md).
# Also the tier lists the weekly doc audit slices from.
docs-lint:
	bash scripts/docs-lint.sh
```

In `test-makefile`, add after the `install-binary_test.sh` line:

```makefile
	bash -n scripts/docs-lint.sh
	bash scripts/docs-lint_test.sh
```

- [ ] **Step 8: Verify the make targets**

Run: `make test-makefile 2>&1 | tail -3`
Expected: ends with `passed: 9 failed: 0` from docs-lint_test.sh and no error.

Run: `make docs-lint; echo "rc=$?"`
Expected: the findings from Step 6 and `rc=1`. This is correct at this task; `lint` does not depend on it yet.

- [ ] **Step 9: Commit**

```bash
git add scripts/docs-lint.sh scripts/docs-lint_test.sh scripts/testdata/docs-lint Makefile
git commit -m "scripts: docs-lint with tier lists, four checks, slice and words modes (<bead-id>)"
```

---

### Task 4: Bootstrap the tree to green and wire `lint: docs-lint`

**Files:**
- Modify: every `docs/plans/*.md` and `docs/research/*.md` (status header)
- Modify: the proposal-shaped files under `docs/design/` (status header)
- Modify: whichever doc cites `make test-changed` (find it in Step 3)
- Modify: `Makefile` line 60 (`lint:` gains the `docs-lint` prerequisite)

**Interfaces:**
- Consumes: `scripts/docs-lint.sh` from Task 3.
- Produces: a green `make lint` that includes docs-lint.

- [ ] **Step 1: Add the header to every docs/plans and docs/research file**

For each file, prepend one line and a blank line. The month is the file's first commit month; the ref is the bead, commit, or successor doc it names. Use the exact format. Example for the agent-bead migration trio:

```
> Status: historical (2026-09). Merged in: gt-a6g. Not maintained.
```

For the two docs-hygiene files from this design, use `Superseded by: gt-nj23 (epic)`. For a plan whose fate is unknown after five minutes of `git log -S` and `bd search`, use `Abandoned: unknown` and list it in the bead notes.

- [ ] **Step 2: Classify docs/design**

The rule: a doc that describes something on main today is reference and gets no header; a doc that proposes, plans, or surveys is historical and gets the header. Verify each with a grep for two or three identifiers the doc names (a function, a command, a config key) under `internal/` and `cmd/`. Starting point from the baseline (inbound-reference counts in parentheses); confirm each, do not copy blindly:

Reference (no header): architecture.md (9), polecat-lifecycle-patrol.md (4), mail-protocol.md (4), escalation.md (4), dolt-storage.md (3), directives-and-overlays.md (3), scheduler.md (2), formula-resolution.md (2), persistent-polecat-pool.md (2), plugin-system.md (1), property-layers.md (1), tmux-keybindings.md (0), dog-infrastructure.md (0), dog-execution-model.md (0), convoy/convoy-lifecycle.md, otel/otel-architecture.md, otel/otel-data-model.md.

Historical (header): witness-at-team-lead.md, sandboxed-polecat-execution.md, agent-api-inventory.md, mol-mall-design.md, factory-worker-api.md, polecat-self-managed-completion.md, model-aware-molecules.md, ledger-export-triggers.md, federation.md, convoy/mountain-eater.md, convoy/roadmap.md, convoy/spec.md, convoy/stage-launch/*.

Record the final classification in the bead notes: `bd update <bead-id> --notes "docs/design historical: <list>"`.

- [ ] **Step 3: Fix the dead make target and any dead links**

Run: `bash scripts/docs-lint.sh | grep -v status-header`
For `dead-make-target: make test-changed`: either the doc should say the target that exists (`make test`), or the sentence is sediment (R5) and goes. For each `dead-link`: fix the path if the target moved, delete the link sentence if the target is gone.

- [ ] **Step 4: Run docs-lint until it is clean**

Run: `bash scripts/docs-lint.sh; echo "rc=$?"`
Expected: no output, `rc=0`.

- [ ] **Step 5: Wire the gate**

In `Makefile`, change the `lint:` line to `lint: docs-lint`.

- [ ] **Step 6: Verify the gate**

Run: `make lint 2>&1 | tail -3; echo "rc=${PIPESTATUS[0]}"`
Expected: docs-lint runs first with no findings, golangci-lint passes, rc=0.

Run: `bash scripts/docs-lint.sh --list historical | wc -l` and `--list reference | wc -l`
Expected: historical about 20, reference about 50. Put both numbers in the bead notes.

- [ ] **Step 7: Commit**

```bash
git add Makefile docs
git commit -m "docs: status headers on historical docs, docs/design classified, docs-lint wired into make lint (<bead-id>)"
```

---

### Task 5: The om criterion and `context_file`

**Files:**
- Modify: `.om.json`

**Interfaces:**
- Consumes: `docs/writing-for-agents.md` on main (Task 1 merged). File this bead's MR only after `git log origin/main -- docs/writing-for-agents.md` shows the file.
- Produces: the reviewer sees the standard in every gastown review.

- [ ] **Step 1: Confirm the precondition**

Run: `git fetch origin && git show origin/main:docs/writing-for-agents.md | head -1`
Expected: `# Writing for agents`. If this fails, stop; the MR would fail every review with exit 2 (missing context_file).

- [ ] **Step 2: Rewrite `.om.json`**

Keep the existing `backend`, `threshold`, `depth`, `timeout`, and the five existing criteria verbatim. Add `context_file` after `timeout` and the new criterion at the end of `rubric`:

```json
{
  "backend": [
    "/Users/sloan/.local/bin/claude-deepseek-pro",
    "-p"
  ],
  "threshold": 0.6,
  "depth": "standard",
  "timeout": 900,
  "context_file": "docs/writing-for-agents.md",
  "rubric": [
    { "name": "correctness", "weight": 3, "guidance": "<unchanged>" },
    { "name": "regression-safety", "weight": 3, "guidance": "<unchanged>" },
    { "name": "upstream-mergeability", "weight": 2, "guidance": "<unchanged>" },
    { "name": "tests", "weight": 2, "guidance": "<unchanged>" },
    { "name": "security", "weight": 3, "guidance": "<unchanged>" },
    {
      "name": "docs-and-comments",
      "weight": 2,
      "guidance": "A change that alters behaviour updates every doc and comment that describes it. New or edited docs and comments follow the rules R1-R13 in the repository context (docs/writing-for-agents.md); cite the rule id in a finding. A stale or contradicted line in an agent-facing file (AGENTS.md, internal/templates/polecat-CLAUDE.md, a directive, a formula description, a plugin.md, docs/HOOKS.md) is major; elsewhere minor. This criterion moves the score; it does not block alone."
    }
  ]
}
```

`<unchanged>` means copy the current text exactly; do not retype it.

- [ ] **Step 3: Validate**

Run: `jq -e '.context_file == "docs/writing-for-agents.md" and (.rubric | map(.name) | index("docs-and-comments") != null)' .om.json && test -f "$(jq -r .context_file .om.json)" && echo ok`
Expected: `true` then `ok`.

- [ ] **Step 4: Commit, and put the landing note in the MR**

```bash
git add .om.json
git commit -m "om: docs-and-comments criterion (weight 2) and context_file (<bead-id>)"
```

Then: `bd update <bead-id> --notes "LANDING: after merge the operator must run om's deploy.sh --rig gastown to re-stamp the rubric sha; every gastown gate fails version_mismatch until then."` The mayor relays this to the operator when the MR is queued.

---

### Task 6: The audit formula `mol-doc-audit`

**Files:**
- Create: `internal/formula/formulas/mol-doc-audit.formula.toml`
- Test: existing `internal/formula/variable_validation_test.go` (`TestAllEmbeddedFormulas_VariableValidation`) and `internal/formula/embed_test.go` cover every shipped formula.

**Interfaces:**
- Consumes: `scripts/docs-lint.sh --slice N M` and `--words` (Task 3); rule ids R1-R13 (Task 1).
- Produces: formula name `mol-doc-audit`, vars `issue`, `base_branch`, `slice_docs`, `slice_go`, plus the rig command vars mol-polecat-work declares. Task 8 slings it by name.

- [ ] **Step 1: Copy the skeleton**

The formula reuses mol-polecat-work's steps except `implement` and `self-review`. Build the file mechanically so the copied text is exact:

```bash
SRC=internal/formula/formulas/mol-polecat-work.formula.toml
DST=internal/formula/formulas/mol-doc-audit.formula.toml
{
  cat <<'HDR'
description = """
Weekly docs-and-comments audit of one bounded slice of the repository.

You are a polecat. The skeleton is mol-polecat-work: load context, branch,
work, commit, self-review, build-check, pre-verify, gt done. The work step is
an audit, not a feature: apply every rule in docs/writing-for-agents.md to
every file in a slice the lint script chose, edit in place, and post a
findings comment on your bead. The MR rides the normal refinery and om gate.

## Variables

| Variable | Source | Description |
|----------|--------|-------------|
| issue | hook_bead | The run bead you are assigned |
| base_branch | sling vars | Branch to rebase on (default: main) |
| slice_docs | sling vars | Docs in the slice (default 8) |
| slice_go | sling vars | Go files in the slice (default 4) |
| setup_command, typecheck_command, test_command, lint_command, build_command | rig config | As in mol-polecat-work |

## Hard limits

- You edit only files the slice names. A file outside the slice is reverted before submit.
- In a Go file you edit comments only. Code, identifiers, and tests are untouched.
- You never delete a historical doc. A deletion is a proposal in the bead comment.
"""
formula = "mol-doc-audit"
version = 1

HDR
  sed -n '57,226p' "$SRC"      # load-context, branch-setup (verbatim)
} > "$DST"
```

Check the copied range still starts at `[[steps]]` / `id = "load-context"` and ends just before `[[steps]]` / `id = "implement"`; the line numbers are from version 14 of mol-polecat-work. If they moved, use the `id =` markers.

- [ ] **Step 2: Append the audit step**

```bash
cat >> "$DST" <<'STEP'
[[steps]]
id = "audit"
title = "Audit the slice against docs/writing-for-agents.md"
needs = ["branch-setup"]
description = """
**1. Compute the slice. The script chooses; you do not.**
```bash
bash scripts/docs-lint.sh --slice {{slice_docs}} {{slice_go}} | tee /tmp/doc-audit-slice.txt
bash scripts/docs-lint.sh --words   # record as WORDS_BEFORE
bd update {{issue}} --notes "slice: $(tr '\\n' ' ' < /tmp/doc-audit-slice.txt)"
```

**2. Read the standard.** Open docs/writing-for-agents.md and keep the rule ids
R1-R13 in front of you. They are the checklist.

**3. For every file in the slice, apply every rule.**
- A `.md` or `.toml` file: read the whole file. For each rule, ask whether any
  line fails it. Edit in place. Deleting is the usual fix (R3, R4, R5, R13);
  a pointer replaces a duplicate (R2); a split or a move behind a pointer fixes
  sprawl (R6).
- A `.go` file: comments only. Every doc comment on an exported identifier
  states the contract in one sentence (R10); every inline comment says why
  (R9); history collapses to one bead citation (R11). Never touch code.
- A historical doc (first line `> Status:`): check the header only. If a
  commit or bead it names is on main or closed, write a deletion proposal for
  step 5 with the evidence (`git log --oneline -1 <sha>`, `bd show <id>`).
  Do not delete it.
- A claim you cannot verify against the code or the environment: leave it
  and record it as a proposal.

**4. Commit per file** (the commit-changes step tells you how). The message
names the rule ids applied: `docs: R3 R5 on docs/reference.md ({{issue}})`.

**5. Post the run comment on your bead. This is the audit's output.**
```bash
bd comments add {{issue}} "$(cat <<'EOF'
doc-audit run
files: <one per line, with the rule ids applied or 'clean'>
findings per rule: R1=<n> R2=<n> ... R13=<n>
words agent-facing: before=<WORDS_BEFORE> after=<docs-lint.sh --words now>
proposals:
- <deletion | out-of-slice rot | rule to sharpen>: <path or rule> — <evidence>
EOF
)"
```

**Done when:** every file in /tmp/doc-audit-slice.txt has a commit or is listed
as clean in the comment, and the comment is posted.
"""

STEP
```

- [ ] **Step 3: Append commit-changes, then the replacement self-review, then build-check through submit-and-exit, then vars**

```bash
sed -n '312,366p' "$SRC" >> "$DST"     # commit-changes (verbatim)
cat >> "$DST" <<'STEP'
[[steps]]
id = "self-review"
title = "Self-review: only the slice changed"
needs = ["commit-changes"]
description = """
**1. Diff scope is a hard gate.**
```bash
git fetch origin {{base_branch}}
git diff --name-only origin/{{base_branch}}...HEAD | sort > /tmp/doc-audit-changed.txt
sort /tmp/doc-audit-slice.txt > /tmp/doc-audit-slice-sorted.txt
comm -23 /tmp/doc-audit-changed.txt /tmp/doc-audit-slice-sorted.txt
```
The last command must print nothing. Any path it prints is outside the slice:
revert it (`git checkout origin/{{base_branch}} -- <path>` then commit) and add
a line to the bead comment: `reverted out-of-slice edit: <path>`.

**2. Go files: comments only.**
```bash
for f in $(grep '\\.go$' /tmp/doc-audit-changed.txt); do
  git diff origin/{{base_branch}}...HEAD -- "$f" | grep -E '^[-+]' | grep -vE '^(\\+\\+\\+|---)' | grep -vE '^[-+][[:space:]]*(//|/\\*|\\*)' && echo "NON-COMMENT CHANGE in $f"
done
```
Must print nothing. A non-comment change is reverted the same way.

**3. Re-read each diff once** with the rule ids in view. An edit that does not
cite a rule it satisfies is reverted.

**Done when:** steps 1 and 2 print nothing and every remaining edit cites a rule.
"""

STEP
sed -n '414,611p' "$SRC" >> "$DST"     # build-check, pre-verify, submit-and-exit (verbatim)
cat >> "$DST" <<'VARS'
[vars]
[vars.issue]
description = "The run bead assigned to this polecat"
required = true

[vars.base_branch]
description = "The base branch to rebase on and compare against"
default = "main"

[vars.slice_docs]
description = "Number of agent-facing or reference docs in the slice"
default = "8"

[vars.slice_go]
description = "Number of non-test Go files in the slice"
default = "4"

VARS
sed -n '/^\[vars.setup_command\]/,$p' "$SRC" >> "$DST"   # rig command vars (verbatim)
```

Then open the file and check: no step id appears twice; the `needs` chain is load-context -> branch-setup -> audit -> commit-changes -> self-review -> build-check -> pre-verify -> submit-and-exit (edit `needs` on the copied `commit-changes` from `["implement"]` to `["audit"]`); the copied `[vars]` block from the tail of mol-polecat-work does not repeat `issue` or `base_branch` (delete any duplicate). If mol-polecat-work has a `[squash]` block after vars, keep it.

- [ ] **Step 4: Run the formula tests**

Run: `CGO_CPPFLAGS=-I$(brew --prefix icu4c)/include CGO_LDFLAGS=-L$(brew --prefix icu4c)/lib go test ./internal/formula/ -run 'TestAllEmbeddedFormulas|TestCheckFormulaHealth'`
Expected: PASS. A failure names the variable or step that is wrong; fix the TOML, not the test.

- [ ] **Step 5: Check the formula's own word count**

Run: `wc -w "$DST"`
Expected: fewer than mol-polecat-work's 3,529. Note the number in the bead; it is a future sprawl finding either way.

- [ ] **Step 6: Commit**

```bash
git add internal/formula/formulas/mol-doc-audit.formula.toml
git commit -m "formula: mol-doc-audit, weekly docs-and-comments audit on a lint-chosen slice (<bead-id>)"
```

---

### Task 7: `scheduled_slings` config, validation, decision function, bead parsing

**Files:**
- Create: `internal/daemon/scheduled_slings.go`
- Create: `internal/daemon/scheduled_slings_test.go`
- Modify: `internal/daemon/types.go` (`PatrolsConfig` at lines 118-136; `IsPatrolEnabled` opt-in block at lines 240-256)

**Interfaces:**
- Produces, consumed by Task 8:

```go
type ScheduledSlingsConfig struct {
	Enabled bool                  `json:"enabled"`
	Entries []ScheduledSlingEntry `json:"entries,omitempty"`
}
type ScheduledSlingEntry struct {
	Name        string            `json:"name"`
	Rig         string            `json:"rig"`
	Formula     string            `json:"formula"`
	Agent       string            `json:"agent,omitempty"`
	IntervalStr string            `json:"interval"`
	Priority    int               `json:"priority,omitempty"` // 0 means default 3
	Vars        map[string]string `json:"vars,omitempty"`
}
func (e ScheduledSlingEntry) validate() error
func (e ScheduledSlingEntry) interval() time.Duration   // valid after validate
func (e ScheduledSlingEntry) label() string             // "scheduled:" + Name
func (e ScheduledSlingEntry) priority() int             // Priority or 3
type scheduledBead struct { ID, Status string; CreatedAt time.Time }
type scheduledAction int  // scheduledSkipOpen, scheduledSkipRecent, scheduledDispatch
func decideScheduledSling(beads []scheduledBead, interval time.Duration, now time.Time) scheduledAction
func parseScheduledBeads(data []byte) ([]scheduledBead, error)   // bd list --json output
func parseCreatedBeadID(data []byte) (string, error)              // bd create --json output, object or array
```

- [ ] **Step 1: Write the failing tests**

`internal/daemon/scheduled_slings_test.go`:

```go
package daemon

import (
	"testing"
	"time"
)

func TestScheduledSlingEntry_Validate(t *testing.T) {
	good := ScheduledSlingEntry{Name: "doc-audit", Rig: "gastown", Formula: "mol-doc-audit", IntervalStr: "168h"}
	if err := good.validate(); err != nil {
		t.Fatalf("valid entry rejected: %v", err)
	}
	cases := map[string]ScheduledSlingEntry{
		"empty name":    {Rig: "gastown", Formula: "f", IntervalStr: "1h"},
		"bad name":      {Name: "Doc Audit", Rig: "gastown", Formula: "f", IntervalStr: "1h"},
		"empty rig":     {Name: "a", Formula: "f", IntervalStr: "1h"},
		"empty formula": {Name: "a", Rig: "gastown", IntervalStr: "1h"},
		"no interval":   {Name: "a", Rig: "gastown", Formula: "f"},
		"bad interval":  {Name: "a", Rig: "gastown", Formula: "f", IntervalStr: "weekly"},
		"zero interval": {Name: "a", Rig: "gastown", Formula: "f", IntervalStr: "0s"},
	}
	for name, e := range cases {
		if err := e.validate(); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	if got := good.label(); got != "scheduled:doc-audit" {
		t.Errorf("label = %q", got)
	}
	if got := good.priority(); got != 3 {
		t.Errorf("default priority = %d, want 3", got)
	}
	if got := good.interval(); got != 168*time.Hour {
		t.Errorf("interval = %v", got)
	}
}

func TestDecideScheduledSling(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	week := 168 * time.Hour
	cases := []struct {
		name  string
		beads []scheduledBead
		want  scheduledAction
	}{
		{"no beads", nil, scheduledDispatch},
		{"open bead", []scheduledBead{{ID: "gt-1", Status: "open", CreatedAt: now.Add(-30 * 24 * time.Hour)}}, scheduledSkipOpen},
		{"in_progress bead", []scheduledBead{{ID: "gt-1", Status: "in_progress", CreatedAt: now.Add(-2 * time.Hour)}}, scheduledSkipOpen},
		{"closed recent", []scheduledBead{{ID: "gt-1", Status: "closed", CreatedAt: now.Add(-2 * 24 * time.Hour)}}, scheduledSkipRecent},
		{"closed old", []scheduledBead{{ID: "gt-1", Status: "closed", CreatedAt: now.Add(-8 * 24 * time.Hour)}}, scheduledDispatch},
		{"mixed: newest closed old, older open", []scheduledBead{
			{ID: "gt-1", Status: "closed", CreatedAt: now.Add(-8 * 24 * time.Hour)},
			{ID: "gt-0", Status: "open", CreatedAt: now.Add(-20 * 24 * time.Hour)},
		}, scheduledSkipOpen},
		{"exactly one interval ago", []scheduledBead{{ID: "gt-1", Status: "closed", CreatedAt: now.Add(-week)}}, scheduledDispatch},
	}
	for _, c := range cases {
		if got := decideScheduledSling(c.beads, week, now); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestParseScheduledBeads(t *testing.T) {
	data := []byte(`[{"id":"gt-abc","status":"closed","created_at":"2026-09-12T10:00:00Z","labels":["scheduled:doc-audit"]},
	                 {"id":"gt-def","status":"open","created_at":"2026-09-19T10:00:00-05:00"}]`)
	got, err := parseScheduledBeads(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "gt-abc" || got[1].Status != "open" {
		t.Fatalf("parsed %+v", got)
	}
	if !got[1].CreatedAt.Equal(time.Date(2026, 9, 19, 15, 0, 0, 0, time.UTC)) {
		t.Errorf("created_at not parsed with offset: %v", got[1].CreatedAt)
	}
	if _, err := parseScheduledBeads([]byte(`not json`)); err == nil {
		t.Error("expected error on bad json")
	}
	empty, err := parseScheduledBeads([]byte(`[]`))
	if err != nil || len(empty) != 0 {
		t.Errorf("empty list: %v %v", empty, err)
	}
}

func TestParseCreatedBeadID(t *testing.T) {
	for _, in := range []string{`{"id":"gt-new1","title":"x"}`, `[{"id":"gt-new1","title":"x"}]`} {
		id, err := parseCreatedBeadID([]byte(in))
		if err != nil || id != "gt-new1" {
			t.Errorf("%s: id=%q err=%v", in, id, err)
		}
	}
	if _, err := parseCreatedBeadID([]byte(`{}`)); err == nil {
		t.Error("expected error when id is missing")
	}
}

func TestIsPatrolEnabled_ScheduledSlingsIsOptIn(t *testing.T) {
	if IsPatrolEnabled(nil, "scheduled_slings") {
		t.Error("nil config must not enable scheduled_slings")
	}
	if IsPatrolEnabled(&DaemonPatrolConfig{Patrols: &PatrolsConfig{}}, "scheduled_slings") {
		t.Error("absent block must not enable scheduled_slings")
	}
	on := &DaemonPatrolConfig{Patrols: &PatrolsConfig{ScheduledSlings: &ScheduledSlingsConfig{Enabled: true}}}
	if !IsPatrolEnabled(on, "scheduled_slings") {
		t.Error("enabled block must enable scheduled_slings")
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `CGO_CPPFLAGS=-I$(brew --prefix icu4c)/include CGO_LDFLAGS=-L$(brew --prefix icu4c)/lib go test ./internal/daemon/ -run 'ScheduledSling|ParseScheduled|ParseCreatedBead|IsPatrolEnabled_ScheduledSlings'`
Expected: build failure, undefined types.

- [ ] **Step 3: Add the config field and the opt-in branch**

In `internal/daemon/types.go`, add to `PatrolsConfig` after `RestartTracker`:

```go
	// ScheduledSlings dispatches a formula onto a rig on an interval, one bead
	// per run; the open bead is the double-dispatch guard (gt-nj23).
	ScheduledSlings *ScheduledSlingsConfig `json:"scheduled_slings,omitempty"`
```

In `IsPatrolEnabled`, add beside the `dolt_remotes` opt-in block:

```go
	if patrol == "scheduled_slings" {
		if config == nil || config.Patrols == nil || config.Patrols.ScheduledSlings == nil {
			return false
		}
		return config.Patrols.ScheduledSlings.Enabled
	}
```

- [ ] **Step 4: Write `scheduled_slings.go` (Task 7 half)**

```go
package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"time"
)

// ScheduledSlingsConfig is the opt-in scheduled_slings patrol: each entry is a
// formula slung onto a rig on an interval, one bead per run (gt-nj23).
type ScheduledSlingsConfig struct {
	Enabled bool                  `json:"enabled"`
	Entries []ScheduledSlingEntry `json:"entries,omitempty"`
}

// ScheduledSlingEntry is one scheduled formula. Name is the schedule's
// identity: runs are found by the label "scheduled:<name>".
type ScheduledSlingEntry struct {
	Name        string            `json:"name"`
	Rig         string            `json:"rig"`
	Formula     string            `json:"formula"`
	Agent       string            `json:"agent,omitempty"`
	IntervalStr string            `json:"interval"`
	Priority    int               `json:"priority,omitempty"`
	Vars        map[string]string `json:"vars,omitempty"`
}

const defaultScheduledSlingPriority = 3

var scheduledSlingNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

func (e ScheduledSlingEntry) validate() error {
	if !scheduledSlingNameRe.MatchString(e.Name) {
		return fmt.Errorf("scheduled_slings: name %q must match %s", e.Name, scheduledSlingNameRe)
	}
	if e.Rig == "" || e.Formula == "" {
		return fmt.Errorf("scheduled_slings[%s]: rig and formula are required", e.Name)
	}
	d, err := time.ParseDuration(e.IntervalStr)
	if err != nil || d <= 0 {
		return fmt.Errorf("scheduled_slings[%s]: interval %q must be a positive Go duration", e.Name, e.IntervalStr)
	}
	return nil
}

func (e ScheduledSlingEntry) interval() time.Duration {
	d, _ := time.ParseDuration(e.IntervalStr)
	return d
}

func (e ScheduledSlingEntry) label() string { return "scheduled:" + e.Name }

func (e ScheduledSlingEntry) priority() int {
	if e.Priority == 0 {
		return defaultScheduledSlingPriority
	}
	return e.Priority
}

// scheduledBead is the slice of a run bead the decision needs.
type scheduledBead struct {
	ID        string
	Status    string
	CreatedAt time.Time
}

type scheduledAction int

const (
	scheduledSkipOpen   scheduledAction = iota // a run is in flight or its MR is queued
	scheduledSkipRecent                        // the newest run started less than an interval ago
	scheduledDispatch
)

func (a scheduledAction) String() string {
	switch a {
	case scheduledSkipOpen:
		return "skip-open"
	case scheduledSkipRecent:
		return "skip-recent"
	default:
		return "dispatch"
	}
}

// decideScheduledSling is pure: the beads carrying the entry's label, the
// entry's interval, and now. Any open bead wins over recency so a run whose
// MR is still in the queue is never doubled.
func decideScheduledSling(beads []scheduledBead, interval time.Duration, now time.Time) scheduledAction {
	var newest time.Time
	for _, b := range beads {
		if b.Status != "closed" {
			return scheduledSkipOpen
		}
		if b.CreatedAt.After(newest) {
			newest = b.CreatedAt
		}
	}
	if !newest.IsZero() && now.Sub(newest) < interval {
		return scheduledSkipRecent
	}
	return scheduledDispatch
}

// parseScheduledBeads reads `bd list --json` output.
func parseScheduledBeads(data []byte) ([]scheduledBead, error) {
	var rows []struct {
		ID        string    `json:"id"`
		Status    string    `json:"status"`
		CreatedAt time.Time `json:"created_at"`
	}
	if err := json.Unmarshal(data, &rows); err != nil {
		return nil, fmt.Errorf("parsing bd list output: %w", err)
	}
	out := make([]scheduledBead, 0, len(rows))
	for _, r := range rows {
		out = append(out, scheduledBead{ID: r.ID, Status: r.Status, CreatedAt: r.CreatedAt})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

// parseCreatedBeadID reads `bd create --json`, which some bd versions print
// as an object and others as a one-element array.
func parseCreatedBeadID(data []byte) (string, error) {
	var obj struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &obj); err == nil && obj.ID != "" {
		return obj.ID, nil
	}
	var arr []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &arr); err == nil && len(arr) > 0 && arr[0].ID != "" {
		return arr[0].ID, nil
	}
	return "", errors.New("bd create output has no id")
}
```

- [ ] **Step 5: Run the tests**

Run: the command from Step 2.
Expected: PASS for all five tests. Run `make build` to confirm the package still compiles with `daemon.go` untouched.

- [ ] **Step 6: Commit**

```bash
git add internal/daemon/scheduled_slings.go internal/daemon/scheduled_slings_test.go internal/daemon/types.go
git commit -m "daemon: scheduled_slings config, validation, decision function, bd JSON parsing (<bead-id>)"
```

---

### Task 8: `scheduled_slings` dispatch, ticker, single flight, escalation

**Files:**
- Modify: `internal/daemon/scheduled_slings.go` (append the runner and the Daemon methods)
- Modify: `internal/daemon/scheduled_slings_test.go` (append fake-runner tests)
- Modify: `internal/daemon/daemon.go` (struct fields near line 167; ticker block after the main_branch_test ticker near line 737; select case after `case <-mainBranchTestChan` near line 861)

**Interfaces:**
- Consumes: everything Task 7 produces; `d.bdPath`, `d.gtPath`, `d.config.TownRoot`, `d.patrolConfig`, `d.logger`, `d.escalate(source, message string)`, `d.isPatrolActive(name)`, `beads.ConfigureCommand(cmd, dir, fallbackBeadsDir, mode)` (sets `cmd.Dir = dir`; `internal/beads/exec.go:41`) and `beads.SubprocessModeForArgs(args)`, `bdMutationRoutingEnv(townRoot)` (`convoy_manager.go:630`), `slingErrorLine` (`convoy_sling_timing.go:25`), `util.SetProcessGroup`.
- Produces:

```go
type scheduledSlingRunner interface {
	listBeads(ctx context.Context, rig, label string) ([]scheduledBead, error)
	createBead(ctx context.Context, rig, title, label, description string, priority int) (string, error)
	sling(ctx context.Context, beadID string, e ScheduledSlingEntry) error
}
func (d *Daemon) triggerScheduledSlings() bool
func (d *Daemon) runScheduledSlings()
func (d *Daemon) runScheduledSlingEntry(ctx context.Context, e ScheduledSlingEntry, now time.Time) error
```

- [ ] **Step 1: Write the failing tests**

Append to `scheduled_slings_test.go`:

```go
type fakeScheduledRunner struct {
	beads    []scheduledBead
	listErr  error
	created  []string // titles
	createID string
	createErr error
	slung    []ScheduledSlingEntry
	slungIDs []string
	slingErr error
}

func (f *fakeScheduledRunner) listBeads(_ context.Context, _, _ string) ([]scheduledBead, error) {
	return f.beads, f.listErr
}
func (f *fakeScheduledRunner) createBead(_ context.Context, _, title, _, _ string, _ int) (string, error) {
	f.created = append(f.created, title)
	return f.createID, f.createErr
}
func (f *fakeScheduledRunner) sling(_ context.Context, id string, e ScheduledSlingEntry) error {
	f.slungIDs = append(f.slungIDs, id)
	f.slung = append(f.slung, e)
	return f.slingErr
}

func newScheduledTestDaemon(t *testing.T, entries []ScheduledSlingEntry, runner scheduledSlingRunner) (*Daemon, *[]string) {
	t.Helper()
	var escalations []string
	d := &Daemon{
		config: &Config{TownRoot: t.TempDir()},
		logger: log.New(os.Stderr, "", 0),
		patrolConfig: &DaemonPatrolConfig{Patrols: &PatrolsConfig{
			ScheduledSlings: &ScheduledSlingsConfig{Enabled: true, Entries: entries},
		}},
		scheduledSlingRunner:   runner,
		scheduledSlingFailures: map[string]int{},
		scheduledSlingEscalate: func(source, msg string) { escalations = append(escalations, source+": "+msg) },
	}
	return d, &escalations
}

var docAuditEntry = ScheduledSlingEntry{Name: "doc-audit", Rig: "gastown", Formula: "mol-doc-audit", Agent: "deepseek-pro", IntervalStr: "168h"}

func TestRunScheduledSlingEntry_DispatchesWhenDue(t *testing.T) {
	f := &fakeScheduledRunner{createID: "gt-run1"}
	d, _ := newScheduledTestDaemon(t, []ScheduledSlingEntry{docAuditEntry}, f)
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	if err := d.runScheduledSlingEntry(context.Background(), docAuditEntry, now); err != nil {
		t.Fatal(err)
	}
	if len(f.created) != 1 || f.created[0] != "doc-audit 2026-09-19" {
		t.Errorf("created titles = %v", f.created)
	}
	if len(f.slungIDs) != 1 || f.slungIDs[0] != "gt-run1" || f.slung[0].Agent != "deepseek-pro" {
		t.Errorf("sling calls = %v %v", f.slungIDs, f.slung)
	}
}

func TestRunScheduledSlingEntry_SkipsWhileOpen(t *testing.T) {
	f := &fakeScheduledRunner{beads: []scheduledBead{{ID: "gt-old", Status: "open", CreatedAt: time.Now().Add(-10 * 24 * time.Hour)}}}
	d, _ := newScheduledTestDaemon(t, []ScheduledSlingEntry{docAuditEntry}, f)
	if err := d.runScheduledSlingEntry(context.Background(), docAuditEntry, time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(f.created) != 0 || len(f.slungIDs) != 0 {
		t.Errorf("expected no dispatch while a run bead is open, got create=%v sling=%v", f.created, f.slungIDs)
	}
}

func TestRunScheduledSlings_EscalatesOnThirdConsecutiveFailure(t *testing.T) {
	f := &fakeScheduledRunner{createID: "gt-x", slingErr: errors.New("boom")}
	d, esc := newScheduledTestDaemon(t, []ScheduledSlingEntry{docAuditEntry}, f)
	for i := 0; i < 3; i++ {
		d.runScheduledSlings()
	}
	if len(*esc) != 1 {
		t.Fatalf("expected exactly one escalation after three failures, got %d: %v", len(*esc), *esc)
	}
	d.runScheduledSlings()
	if len(*esc) != 1 {
		t.Errorf("fourth failure must not escalate again, got %d", len(*esc))
	}
	f.slingErr = nil
	d.runScheduledSlings() // beads is still empty in the fake, so this dispatches and succeeds
	if d.scheduledSlingFailures["doc-audit"] != 0 {
		t.Errorf("success must reset the failure count, got %d", d.scheduledSlingFailures["doc-audit"])
	}
}

func TestRunScheduledSlings_InvalidEntryIsSkippedNotFatal(t *testing.T) {
	f := &fakeScheduledRunner{createID: "gt-ok"}
	bad := ScheduledSlingEntry{Name: "bad", Rig: "gastown", Formula: "f", IntervalStr: "weekly"}
	d, esc := newScheduledTestDaemon(t, []ScheduledSlingEntry{bad, docAuditEntry}, f)
	d.runScheduledSlings()
	if len(f.slungIDs) != 1 {
		t.Errorf("valid entry must still dispatch, got %v", f.slungIDs)
	}
	if len(*esc) != 0 {
		t.Errorf("a config error is logged, not escalated: %v", *esc)
	}
}

func TestTriggerScheduledSlings_SingleFlight(t *testing.T) {
	d := &Daemon{logger: log.New(os.Stderr, "", 0)} // patrol inactive: run returns at once
	if !d.triggerScheduledSlings() {
		t.Fatal("first trigger should start")
	}
	deadline := time.Now().Add(2 * time.Second)
	for d.scheduledSlingsRunning.Load() {
		if time.Now().After(deadline) {
			t.Fatal("first cycle did not clear the flag")
		}
		time.Sleep(time.Millisecond)
	}
	d.scheduledSlingsRunning.Store(true)
	if d.triggerScheduledSlings() {
		t.Error("second trigger must skip while running")
	}
}

func TestExecScheduledRunner_SlingArgv(t *testing.T) {
	r := &execScheduledSlingRunner{townRoot: "/town", bdPath: "bd", gtPath: "gt"}
	e := docAuditEntry
	e.Vars = map[string]string{"slice_docs": "8"}
	got := r.slingArgs("gt-run1", e)
	want := []string{"sling", "gt-run1", "gastown", "--formula=mol-doc-audit", "--agent=deepseek-pro", "--actor=daemon/scheduled:doc-audit", "--no-boot", "--var", "slice_docs=8"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("argv\n got %v\nwant %v", got, want)
	}
}
```

Add imports `context`, `errors`, `log`, `os`, `strings` to the test file.

- [ ] **Step 2: Run to verify they fail**

Run: `CGO_CPPFLAGS=-I$(brew --prefix icu4c)/include CGO_LDFLAGS=-L$(brew --prefix icu4c)/lib go test ./internal/daemon/ -run 'ScheduledSling|ExecScheduledRunner'`
Expected: build failure, undefined `scheduledSlingRunner`, Daemon fields.

- [ ] **Step 3: Add the Daemon fields**

In `internal/daemon/daemon.go`, after `mainBranchTestRunning atomic.Bool`:

```go
	// scheduledSlingsRunning is the single-flight guard for the
	// scheduled_slings patrol, on its own goroutine like main_branch_test so a
	// slow sling never blocks the select loop (gt-nj23).
	scheduledSlingsRunning atomic.Bool
	// scheduledSlingRunner is nil in production (an exec runner is built on
	// first use) and a fake in tests.
	scheduledSlingRunner scheduledSlingRunner
	// scheduledSlingFailures counts consecutive failures per entry name; the
	// third escalates once, success resets. Touched only by the patrol goroutine.
	scheduledSlingFailures map[string]int
	// scheduledSlingEscalate defaults to d.escalate; tests capture it.
	scheduledSlingEscalate func(source, message string)
```

- [ ] **Step 4: Append the runner and the patrol to `scheduled_slings.go`**

Add imports `bytes`, `context`, `os/exec`, `path/filepath`, `strings`, plus `"github.com/steveyegge/gastown/internal/beads"` and `"github.com/steveyegge/gastown/internal/util"`.

```go
// scheduledSlingRunner is the side-effect boundary: bd list, bd create, gt sling.
type scheduledSlingRunner interface {
	listBeads(ctx context.Context, rig, label string) ([]scheduledBead, error)
	createBead(ctx context.Context, rig, title, label, description string, priority int) (string, error)
	sling(ctx context.Context, beadID string, e ScheduledSlingEntry) error
}

const (
	scheduledSlingsTickInterval  = 15 * time.Minute
	scheduledSlingCommandTimeout = 5 * time.Minute
	scheduledSlingEscalateAfter  = 3
)

type execScheduledSlingRunner struct {
	townRoot, bdPath, gtPath string
}

func (r *execScheduledSlingRunner) runBd(ctx context.Context, rig string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, r.bdPath, args...)
	// ConfigureCommand sets cmd.Dir to the rig dir, so bd's cwd routing lands
	// on the rig database (never --repo: see the bd-create-repo memory).
	rigDir := filepath.Join(r.townRoot, rig)
	beads.ConfigureCommand(cmd, rigDir, filepath.Join(rigDir, ".beads"), beads.SubprocessModeForArgs(args))
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("bd %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

func (r *execScheduledSlingRunner) listBeads(ctx context.Context, rig, label string) ([]scheduledBead, error) {
	out, err := r.runBd(ctx, rig, "list", "--label", label, "--json", "--limit", "0", "--brief")
	if err != nil {
		return nil, err
	}
	return parseScheduledBeads(out)
}

func (r *execScheduledSlingRunner) createBead(ctx context.Context, rig, title, label, description string, priority int) (string, error) {
	out, err := r.runBd(ctx, rig, "create", "--title", title, "--type", "task",
		"--priority", fmt.Sprint(priority), "--labels", label, "--description", description, "--json")
	if err != nil {
		return "", err
	}
	return parseCreatedBeadID(out)
}

func (r *execScheduledSlingRunner) slingArgs(beadID string, e ScheduledSlingEntry) []string {
	args := []string{"sling", beadID, e.Rig, "--formula=" + e.Formula}
	if e.Agent != "" {
		args = append(args, "--agent="+e.Agent)
	}
	args = append(args, "--actor=daemon/scheduled:"+e.Name, "--no-boot")
	keys := make([]string, 0, len(e.Vars))
	for k := range e.Vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "--var", k+"="+e.Vars[k])
	}
	return args
}

func (r *execScheduledSlingRunner) sling(ctx context.Context, beadID string, e ScheduledSlingEntry) error {
	cmd := exec.CommandContext(ctx, r.gtPath, r.slingArgs(beadID, e)...)
	cmd.Dir = r.townRoot
	cmd.Env = bdMutationRoutingEnv(r.townRoot)
	util.SetProcessGroup(cmd)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("gt sling %s: %w: %s", beadID, err, slingErrorLine(stderr.String()))
	}
	return nil
}

func (d *Daemon) scheduledRunner() scheduledSlingRunner {
	if d.scheduledSlingRunner == nil {
		d.scheduledSlingRunner = &execScheduledSlingRunner{townRoot: d.config.TownRoot, bdPath: d.bdPath, gtPath: d.gtPath}
	}
	return d.scheduledSlingRunner
}

// triggerScheduledSlings starts a cycle on its own goroutine; an overlapping
// tick is skipped. Returns true if a cycle started.
func (d *Daemon) triggerScheduledSlings() bool {
	if !d.scheduledSlingsRunning.CompareAndSwap(false, true) {
		d.logger.Printf("scheduled_slings: previous cycle still running, skipping this tick")
		return false
	}
	go func() {
		defer d.scheduledSlingsRunning.Store(false)
		d.runScheduledSlings()
	}()
	return true
}

func (d *Daemon) runScheduledSlings() {
	if !d.isPatrolActive("scheduled_slings") {
		return
	}
	cfg := d.patrolConfig.Patrols.ScheduledSlings
	if d.scheduledSlingFailures == nil {
		d.scheduledSlingFailures = map[string]int{}
	}
	if d.scheduledSlingEscalate == nil {
		d.scheduledSlingEscalate = d.escalate
	}
	now := time.Now()
	for _, e := range cfg.Entries {
		if err := e.validate(); err != nil {
			d.logger.Printf("scheduled_slings: %v (entry skipped)", err)
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), scheduledSlingCommandTimeout)
		err := d.runScheduledSlingEntry(ctx, e, now)
		cancel()
		if err == nil {
			d.scheduledSlingFailures[e.Name] = 0
			continue
		}
		d.scheduledSlingFailures[e.Name]++
		n := d.scheduledSlingFailures[e.Name]
		d.logger.Printf("scheduled_slings: %s: failure %d: %v", e.Name, n, err)
		if n == scheduledSlingEscalateAfter {
			d.scheduledSlingEscalate("scheduled_slings",
				fmt.Sprintf("scheduled sling %s (%s on %s) failed %d ticks in a row; last error: %v",
					e.Name, e.Formula, e.Rig, n, err))
		}
	}
}

// runScheduledSlingEntry evaluates one entry and dispatches when due.
func (d *Daemon) runScheduledSlingEntry(ctx context.Context, e ScheduledSlingEntry, now time.Time) error {
	r := d.scheduledRunner()
	beadsForLabel, err := r.listBeads(ctx, e.Rig, e.label())
	if err != nil {
		return err
	}
	action := decideScheduledSling(beadsForLabel, e.interval(), now)
	if action != scheduledDispatch {
		d.logger.Printf("scheduled_slings: %s: %s", e.Name, action)
		return nil
	}
	title := fmt.Sprintf("%s %s", e.Name, now.UTC().Format("2006-01-02"))
	desc := fmt.Sprintf("Scheduled run of formula %s on rig %s, created by the daemon's scheduled_slings patrol. Label %s is the run's identity; while this bead is open no second run is dispatched.", e.Formula, e.Rig, e.label())
	id, err := r.createBead(ctx, e.Rig, title, e.label(), desc, e.priority())
	if err != nil {
		return err
	}
	d.logger.Printf("scheduled_slings: %s: created %s, slinging %s to %s (agent %q)", e.Name, id, e.Formula, e.Rig, e.Agent)
	if err := r.sling(ctx, id, e); err != nil {
		return err
	}
	return nil
}
```

`slingErrorLine` already exists in `convoy_sling_timing.go`; reuse it.

- [ ] **Step 5: Wire the ticker and the select case in `daemon.go`**

After the main_branch_test ticker block (around line 737):

```go
	// Start the scheduled_slings ticker if configured. Each tick evaluates
	// every entry; the decision function decides due-ness, so a coarse tick
	// is enough (gt-nj23).
	var scheduledSlingsTicker *time.Ticker
	var scheduledSlingsChan <-chan time.Time
	if d.isPatrolActive("scheduled_slings") {
		scheduledSlingsTicker = time.NewTicker(scheduledSlingsTickInterval)
		scheduledSlingsChan = scheduledSlingsTicker.C
		defer scheduledSlingsTicker.Stop()
		d.logger.Printf("Scheduled slings ticker started (tick %v, %d entries)", scheduledSlingsTickInterval, len(d.patrolConfig.Patrols.ScheduledSlings.Entries))
	}
```

After `case <-mainBranchTestChan:` block (around line 870):

```go
		case <-scheduledSlingsChan:
			// Scheduled slings — bead-per-run formula dispatch on an interval.
			if !d.isShutdownInProgress() {
				d.triggerScheduledSlings()
			}
```

- [ ] **Step 6: Run the tests and the package**

Run: the command from Step 2.
Expected: PASS for all six.

Run: `CGO_CPPFLAGS=-I$(brew --prefix icu4c)/include CGO_LDFLAGS=-L$(brew --prefix icu4c)/lib go test ./internal/daemon/ 2>&1 | tail -3`
Expected: `ok`. Then `make build` and `make lint` green.

- [ ] **Step 7: Commit**

```bash
git add internal/daemon/scheduled_slings.go internal/daemon/scheduled_slings_test.go internal/daemon/daemon.go
git commit -m "daemon: scheduled_slings patrol — bead-per-run formula dispatch with open-bead guard (<bead-id>)"
```

---

### Task 9: Operator runbook (no code; the operator session does these by hand)

**Interfaces:**
- Consumes: Tasks 1-8 merged in the order the spec's Rollout section gives.

- [ ] **Step 1: After Task 5 merges, re-stamp the rubric**

```bash
cd ~/gt/om/mayor/rig && git pull --ff-only origin main
contrib/gastown/deploy.sh --rig gastown --rubric ~/gt/gastown/mayor/rig/.om.json --notify
contrib/gastown/deploy.sh --rig gastown --check; echo "rc=$?"
```
Expected: `rc=0`. Every gastown gate between the merge and this command fails `version_mismatch`; run it within minutes of the merge.

- [ ] **Step 2: After Task 6 merges, install and confirm the formula reached the town**

```bash
cd ~/gt/gastown/mayor/rig && git pull --ff-only origin main && make safe-install
ls ~/gt/.beads/formulas/mol-doc-audit.formula.toml || cp internal/formula/formulas/mol-doc-audit.formula.toml ~/gt/.beads/formulas/
cd ~/gt/gastown && bd formula list 2>/dev/null | grep mol-doc-audit
```

- [ ] **Step 3: Hand-run one audit before automating**

```bash
cd ~/gt/gastown
id=$(bd create --title "doc-audit $(date -u +%F) (hand run)" --type task --priority 3 --labels scheduled:doc-audit \
  --description "Hand-run of mol-doc-audit before the scheduled_slings patrol is armed (gt-nj23)." --json | python3 -c 'import json,sys; d=json.load(sys.stdin); d=d[0] if isinstance(d,list) else d; print(d["id"])')
echo "$id"
gt sling "$id" gastown --formula mol-doc-audit --agent deepseek-pro --dry-run
gt sling "$id" gastown --formula mol-doc-audit --agent deepseek-pro
```
Watch: `gt convoy list`, the polecat's session, then `gt mq list`. Verify the polecat runs on deepseek-pro: `ps eww -p <pid> | tr ' ' '\n' | grep ANTHROPIC_BASE_URL`. When the MR lands, read the run bead comment (`bd show "$id"`) and the om receipt. If om rejects it, read the findings before deciding whether the formula or the rule text needs a change.

- [ ] **Step 4: After Task 8 merges and the hand run merged cleanly, arm the patrol**

```bash
cp ~/gt/mayor/daemon.json ~/gt/mayor/daemon.json.bak-$(date +%Y%m%d)-scheduled-slings
python3 - <<'PY'
import json; p='/Users/sloan/gt/mayor/daemon.json'; d=json.load(open(p))
d['patrols']['scheduled_slings']={"enabled":True,"entries":[{"name":"doc-audit","rig":"gastown","formula":"mol-doc-audit","agent":"deepseek-pro","interval":"168h","priority":3}]}
json.dump(d,open(p,'w'),indent=2); print("armed")
PY
```
Restart the daemon under launchd as stop plus kickstart (not a bare stop), then `grep scheduled_slings ~/gt/daemon.log | tail -3` and expect "Scheduled slings ticker started (tick 15m0s, 1 entries)". The first tick sees the hand run's closed bead: if it closed less than a week ago the log says `doc-audit: skip-recent`, which is the correct first observation.

- [ ] **Step 5: File the follow-up bead now**

```bash
cd ~/gt/gastown && bd create --title "docs-lint: lower word ceiling to 1,500 and include formula descriptions" --type task --priority 3 --parent gt-nj23 \
  --description "Deviation recorded in the design (component 3): v1 gates always-loaded files at 2,000 words and leaves formulas ungated because 14 formulas already exceed 1,500. When mol-doc-audit has visited every formula once (check run bead comments under label scheduled:doc-audit), set DOCS_LINT_CEILING default to 1500 and extend tier_ceiling in scripts/docs-lint.sh to formula toml files. Main must be green on merge."
```

---

## Self-review against the spec

- Component 1 (standard, three pointers): Task 1 writes it; Task 5 wires `context_file`; Task 6 step 2 reads it; Task 2 adds the template pointer. Covered.
- Component 2 (criterion, landing hazards): Task 5, with the precondition check and the landing note; Task 9 step 1 re-stamps. Covered.
- Component 3 (docs-lint, modes, wiring, bootstrap): Task 3 script and modes; Task 4 headers, classification, fixes, `lint: docs-lint`. Covered.
- Component 4 (formula, slice, self-check, run comment): Task 6. Covered.
- Component 5 (patrol, decision function, no state, escalation after three): Tasks 7 and 8. Covered.
- Component 6 (tests, rollout, measurement): each task carries its tests; Task 9 is the rollout; measurement is the run comment (Task 6) and om receipts (existing). Covered.
- Type consistency: `scheduledBead`, `scheduledAction`, `ScheduledSlingEntry` methods, and `scheduledSlingRunner` names match between Tasks 7 and 8 and their tests; `slingArgs` is on `execScheduledSlingRunner` in both.
