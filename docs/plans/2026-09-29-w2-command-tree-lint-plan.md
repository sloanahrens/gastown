# Command-Tree Lint Implementation Plan (gt-fcxe9.5)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A build-time Go test that fails, naming file, line and token, whenever a formula, template, plugin, hook script or Go exec literal calls a `gt` or `bd` command the real command trees cannot resolve; plus the fixes that turn it green on main.

**Architecture:** A new leaf package `internal/cmdtree` holds the pure parts: a command `Tree` (built from a cobra root or from a checked-in `bd capabilities --json` snapshot), a scanner that extracts `gt`/`bd` invocations from repo files, and a checker that resolves each against the trees and a deny list. The gate itself lives in `internal/cmd` as a `_test.go`, because only package `cmd` can see gt's unexported `rootCmd`.

**Tech Stack:** Go, `github.com/spf13/cobra`, `go/ast` for Go literals, `embed` for the bd snapshot.

**Spec:** bead gt-fcxe9.5 and decision gt-fd2cu.4 (slice 1). Findings: deep review G4-03, B1-08, B5-11 (`~/.claude/docs/research/deep-review/`).

## Global Constraints

- Truth for gt: the in-process cobra tree (`rootCmd`), never a snapshot.
- Truth for bd: `internal/cmdtree/bd-command-tree.json`, generated from `bd capabilities --json` of the beads fork (origin/main da4983e, contract_version 1). Refreshed by `make bd-command-tree BEADS_SRC=<checkout>`.
- `bd sync` is deny-listed with the message: "bd sync is the Dolt federation loop in this fork; never call it from an agent".
- Do not touch `internal/formula` parser semantics, bd itself, or formula structure beyond the token fixes. No baseline or allow-list for real violations: fix the files.
- Every fix is its own commit with the rationale.

---

## Design decisions

**What gets scanned (and in which mode).**

| Files | Mode |
|---|---|
| `internal/formula/formulas/*.toml` | markdown, after TOML un-escaping (`\n`, `\"`, `\\`) per physical line |
| `internal/templates/**` (non-Go), `templates/**` | markdown |
| `plugins/*/plugin.md` | markdown |
| `plugins/**/*.sh` (not `*_test.sh`) | shell |
| `internal/hooks/templates/**`, `scripts/guards/*.sh` (not `*_test.sh`) | shell |
| `internal/config/roles/*.toml` | shell |
| non-test `*.go` under `internal/` and `cmd/` (no testdata) | Go AST |

- **Markdown mode** checks fenced code blocks (as shell) and inline backtick spans. Prose outside code is not checked: "the gt binary" is not an invocation, and flagging it would force weakening the lint.
- **Shell mode** checks every line except comment lines (`#`, `//`). An invocation starts at `gt`/`bd` preceded by line start, whitespace or one of `` ;&|(`$'"{ ``, and followed by whitespace. That excludes `~/gt/...` paths and `gt-abc` bead ids.
- **Go mode** reads `exec.Command`/`exec.CommandContext` whose binary argument is the literal `"gt"` or `"bd"`, `BdCmd(...)`, and the `beads.Command*` family, taking leading string-literal arguments until the first non-literal.

**Resolution rule** (same for both trees). Words are taken until the first token that is not `[a-z][a-z0-9-]*` (flags, placeholders, variables, quotes, shell operators stop it).
1. The first word must be a top-level command or alias. Otherwise: violation.
2. Descend while the next word names a child.
3. If the reached command has children, does not take positional arguments, and the next word is a plain word (`[a-z][a-z-]*`), that word is an unknown subcommand: violation (`gt mq close`).
4. Deny list: any path with prefix `bd sync`.

"Takes positional arguments": for gt, a parent is argument-free when its `RunE` is `requireSubcommand` or it is not runnable. For bd the snapshot lacks runnability, so parents are treated as argument-free (strict). If strict produces a false positive on a real, working bd call, the snapshot generator records `runnable` from cobra instead; no hand allow-list.

---

### Task 1: `internal/cmdtree` Tree and resolution

**Files:** Create `internal/cmdtree/tree.go`, `internal/cmdtree/tree_test.go`.

**Produces:**
```go
type Tree struct{ /* root *Node */ }
type Node struct { Name string; Children map[string]*Node; TakesArgs bool }
func NewTree() *Tree
func (t *Tree) Add(path []string, aliases []string, takesArgs bool) // aliases of the last element
func FromCobra(root *cobra.Command, takesArgs func(*cobra.Command) bool) *Tree
type Resolution struct { Matched []string; Unknown string; OK bool }
func (t *Tree) Resolve(words []string) Resolution
```
- [ ] Write table tests: top-level unknown (`rigs`), known leaf with args (`show gt-1`), nested (`mol wisp list`), alias, unknown subcommand under argument-free parent (`mq close`), plain word under an args-taking parent passes.
- [ ] Run `go test ./internal/cmdtree/` and see it fail to compile; implement; see it pass. Commit.

### Task 2: bd snapshot, generator, refresh target

**Files:** Create `internal/cmdtree/bd-command-tree.json`, `internal/cmdtree/bdtree.go`, `internal/cmdtree/bdtree_test.go`, `internal/cmdtree/gen-bd-tree/main.go`, `scripts/refresh-bd-command-tree.sh`; modify `Makefile`.

**Produces:** `func LoadBdTree() (*Tree, BdSnapshot, error)`; `BdSnapshot{Source, Version, Commit string; ContractVersion int; Commands []BdCommand}` with `BdCommand{Path string; Aliases []string; Hidden bool; Flags []string}` (local flag names only; persistent flags listed once at top level).
- [ ] Test: snapshot loads, `contract_version` is 1, `sync`, `list`, `mol wisp` resolve, `daemons` does not.
- [ ] Generator reads `bd capabilities --json` on stdin, writes the reduced snapshot. The script builds bd from `BEADS_SRC` at `BEADS_REF` (default `origin/main`) via `git archive` into a temp dir (never PATH), runs it in an empty dir with `BD_DISABLE_METRICS=1`, and pipes to the generator.
- [ ] Generate from da4983e; test passes; commit.

### Task 3: scanner

**Files:** Create `internal/cmdtree/scan.go`, `internal/cmdtree/scan_test.go`.

**Produces:**
```go
type Ref struct { File string; Line int; Bin string; Words []string; Text string }
func ScanShell(file, text string, firstLine int) []Ref
func ScanMarkdown(file, text string) []Ref
func ScanTOMLMarkdown(file, text string) []Ref // un-escape then markdown
func ScanGo(file string, src []byte) ([]Ref, error)
func ScanRepo(root string) ([]Ref, error) // the file table above
```
- [ ] Tests: path `~/gt/x` ignored; `gt-abc` ignored; `$(gt rigs --names)` found; trailing `# comment` stops words; comment lines skipped; fenced block and inline span found, bare prose not; escaped `\n` TOML string keeps the physical line; `exec.Command("gt", "swarm", "land", id)` gives `swarm land`; `BdCmd("dep", "add", x)` gives `dep add`.
- [ ] Implement; pass; commit.

### Task 4: checker and the gate (RED on main)

**Files:** Create `internal/cmdtree/check.go`, `internal/cmdtree/check_test.go`, `internal/cmd/command_tree_lint_test.go`.

**Produces:** `type Violation struct{ Ref; Reason string }`, `func Check(refs []Ref, trees map[string]*Tree) []Violation`, `const BdSyncDenied = "bd sync is the Dolt federation loop in this fork; never call it from an agent"`.
- [ ] `TestCommandTokensResolve` in `internal/cmd`: builds the gt tree from `rootCmd` (argument-free = not runnable or `RunE` is `requireSubcommand`, compared by function pointer), loads the bd tree, scans the repo root, and fails with one `file:line: token: reason` line per violation.
- [ ] Run it on the unmodified tree. It must FAIL. Record the list in the report; it is the fix list. Commit the gate with the failure list in the message.

### Task 5..N: one commit per fix

Intended replacements (from the findings; confirm each against the tree):

| Site | Dead token | Fix |
|---|---|---|
| mol-deacon-patrol context-check | `gt context --usage` | delete; the step becomes a self-assessment (no command reports context usage) |
| mol-orphan-scan, mol-digest-generate | `gt rigs` | `gt rig list` |
| mol-town-shutdown | `gt rigs --names`, `gt stop --all --preserve-sandbox` | `gt rig list --json` piped to names; `gt down` equivalent as the tree has it |
| mol-polecat-lease | `gt session kill` | `gt session stop` |
| gastown-release | `gt mol wisp create` | `gt formula run` / `bd mol wisp` as the tree has it |
| refinery.md.tmpl | `gt mq close` | `gt mq reject` (per memory: reject only for genuine rework, which is this path) |
| internal/refinery/engineer.go | `exec gt swarm land` | remove the dead exec; report the landing as unsupported instead of a silent Warning |
| 7 formulas | `bd sync` | delete the step or line |

- [ ] For each: edit, run the gate, confirm that violation is gone, commit with rationale, `bd comments add gt-fcxe9.5`.
- [ ] Final: gate green.

### Task N+1: gates, review, push

- [ ] `make lint`, `go build ./...`, `go test ./internal/cmdtree/ ./internal/cmd/ -run TestCommandTokensResolve`, touched packages, full `make test` timed. Exit codes only.
- [ ] `om review -base origin/main` from the worktree; fix blockers/majors; re-run.
- [ ] Attribution grep empty; `git push origin crew/sloan/w2-lint`.
