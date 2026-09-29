# W2 cobra strictness + test-DB isolation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close two deep-review stopgaps: parent commands that exit 0 on an unknown subcommand plus cobra prefix matching (gt-fcxe9.6, G4-04/G4-11), and isolated bd clients that disable bd's test-database firewall on any port plus the forked test-database prefix lists (gt-fcxe9.9, B2-03).

**Architecture:** Every cobra parent that has children and no Run/RunE gets `RunE: requireSubcommand`, which now fails with exit status 2 (cobra already prints the error and usage to stderr). A tree-walk test fails on any help-only parent. `cobra.EnablePrefixMatching` goes off. For B2-03, a new leaf package `internal/testdb` owns the one prefix list and `IsTestDatabaseName`; the beads isolated client forwards `BEADS_TEST_SERVER=1` only for a port testutil registered as a real test container, and never for 3307.

**Tech Stack:** Go, cobra v1.10.2, existing `internal/cmdtree` lint.

**Spec:** beads gt-fcxe9.6 and gt-fcxe9.9; `~/.claude/docs/research/deep-review/gastown-cli-operator.md` G4-04/G4-11; `beads-storage.md` B2-03.

## Global Constraints

- No live Dolt server, no town commands; unit tests only.
- Do not duplicate the `internal/cmdtree` scanner; keep `TestCommandTokensResolve` green.
- Exit status for an unknown or missing subcommand under a parent: 2 (Claude Code PreToolUse treats 2 as block, so `gt tap guard <typo>` fails closed).
- The test-database prefix list has exactly one definition in the main module.
- Commits: conventional, no AI attribution, no Co-Authored-By trailer.

---

### Task 1: Parents reject unknown subcommands (gt-fcxe9.6)

**Files:**
- Modify: `internal/cmd/root.go` (`requireSubcommand` returns `*ExitCodeError{Code: 2}`; prefix matching off)
- Modify: the 15 parent command files (activity, bead, boot, callbacks, checkpoint, cycle, dog, issue, patrol, signal, slot, tap, tap guard, town, warrant; the test prints the live list)
- Create: `internal/cmd/command_parents_test.go`
- Modify: `internal/cmd/command_tree_lint_test.go` (mark requireSubcommand parents help-only so the lint keeps flagging calls that stop on them)

**Interfaces:**
- Produces: `requireSubcommand(cmd *cobra.Command, args []string) error` returning an error for which `exitCodeForError` is 2.

- [ ] **Step 1: Write the failing tests**

```go
// Every parent with subcommands must be runnable, or cobra prints help and exits 0.
func TestParentCommandsAreNotHelpOnly(t *testing.T) {
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		for _, child := range c.Commands() {
			if child.HasSubCommands() && !child.Runnable() {
				t.Errorf("%s has subcommands but no Run/RunE: set RunE: requireSubcommand", child.CommandPath())
			}
			walk(child)
		}
	}
	walk(rootCmd)
}

func TestRequireSubcommandExitsTwo(t *testing.T) { /* no args and unknown arg both give exitCodeForError == 2 */ }

func TestTapGuardUnknownGuardExitsTwo(t *testing.T) {
	c, rest, err := rootCmd.Find([]string{"tap", "guard", "no-such-guard"})
	// err nil, c == tapGuardCmd, c.RunE(c, rest) -> exit 2
}

func TestPrefixMatchingDisabled(t *testing.T) {
	// cobra.EnablePrefixMatching false; rootCmd.Find([]string{"stat"}) errors, never resolves to status
}
```

Commands are resolved with `rootCmd.Find` and the RunE is called directly, so no test runs `persistentPreRun` (which logs usage and touches the town).

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/cmd -run 'TestParentCommandsAreNotHelpOnly|TestRequireSubcommandExitsTwo|TestTapGuardUnknownGuardExitsTwo|TestPrefixMatchingDisabled' -count=1`
Expected: FAIL listing the 15 parents, exit 1 not 2, `stat` resolving to `status`.

- [ ] **Step 3: Implement**

`requireSubcommand` wraps its current message in `&ExitCodeError{Code: 2, Err: ...}`; `cobra.EnablePrefixMatching = false` with a comment citing G4-11; add `RunE: requireSubcommand` to each listed parent.

- [ ] **Step 4: Keep the lint honest**

In `TestCommandTokensResolve`, after `cmdtree.FromCobra`, walk rootCmd and `MarkHelpOnly` every parent whose RunE is requireSubcommand, so a formula line that stops on such a parent is still a violation. Fix any real call sites this surfaces.

- [ ] **Step 5: Run `go test ./internal/cmd ./internal/cmdtree -count=1`, then commit**

`fix(cmd): parents exit 2 on unknown subcommands; disable prefix matching`

### Task 2: One test-database prefix list (gt-fcxe9.9 part b)

**Files:**
- Create: `internal/testdb/testdb.go`, `internal/testdb/testdb_test.go`
- Modify: `internal/reaper/reaper.go`, `internal/daemon/jsonl_git_backup.go`, `internal/cmd/dolt.go` (cleanup hint glob, migrate skip), `internal/beads/beads.go` + `bd_container_retry.go` (mint prefix), `internal/testutil/doltpool.go` (store and SQL pool names)

**Interfaces:**
- Produces: `testdb.MintPrefix = "testdb_"`, `testdb.RemotesCheckPrefix = "dolt_remotes_check_"`, `testdb.Prefixes() []string` (copy), `testdb.IsTestDatabaseName(name string) bool` (case-insensitive prefix match).

List (union of every copy plus upstream beads): `testdb_`, `beads_t`, `beads_pt`, `beads_vr`, `doctest_`, `doctortest_`, `benchdb_`, `dolt_remotes_check_`.

- [ ] **Step 1: Failing tests** — `IsTestDatabaseName` table (each prefix, upper case, `hq`, `gastown`, `beads` are not test names); a source scan of the main module's non-test `.go` files that fails if any file other than `internal/testdb/testdb.go` contains a string literal `"beads_pt"` or `"doctest_"`; a check that `plugins/dolt-snapshots/main.go` (a separate module that cannot import internal/) carries exactly `Prefixes()`.
- [ ] **Step 2: Verify fail** (package does not exist; copies found).
- [ ] **Step 3: Implement** the package and switch every caller. The JSONL scrub regexp is built from `Prefixes()` with `regexp.QuoteMeta`. The cleanup hint glob is built from `Prefixes()`.
- [ ] **Step 4: `go test ./internal/testdb ./internal/reaper ./internal/daemon -run 'Pollution|Database|Discover' ./internal/beads -run TestDatabase -count=1`**
- [ ] **Step 5: Commit** `refactor(testdb): one test-database prefix list`

### Task 3: Isolated clients stop disabling bd's firewall (gt-fcxe9.9 part a)

**Files:**
- Modify: `internal/beads/beads.go` (`buildRunEnv`, `buildRoutingEnv`), `internal/beads/test_container.go` (registry)
- Modify: `internal/testutil/doltserver.go` (register the shared and per-test container ports)
- Test: `internal/beads/beads_test.go`

**Interfaces:**
- Produces: `beads.RegisterTestServerPort(port int)`; unexported `isTestServerPort(port int) bool` (false for 0 and for `productionDoltPort` 3307 even if registered).

- [ ] **Step 1: Failing test** — with `BEADS_TEST_SERVER=1` in the process env, `NewIsolatedWithPort(dir, 19999)` and `NewIsolatedWithPort(dir, 3307)` produce run and routing envs with no `BEADS_TEST_SERVER`; after `RegisterTestServerPort(19998)` a client on 19998 carries it once; registering 3307 is ignored.
- [ ] **Step 2: Verify fail** (env carries `BEADS_TEST_SERVER=1` today).
- [ ] **Step 3: Implement** a mutex-guarded port set; append `BEADS_TEST_SERVER=1` only when `isTestServerPort(b.serverPort)`; testutil calls `beads.RegisterTestServerPort` wherever it sets `BEADS_TEST_SERVER` for a container it started.
- [ ] **Step 4: `go test ./internal/beads ./internal/testutil -count=1`**
- [ ] **Step 5: Commit** `fix(beads): only declare a registered test container a test server`

### Task 4: Gates and review

- [ ] `make lint`, `go build ./...`, touched packages plus reverse deps, full `make test` under `gt slot run` with wall time, each judged by exit code.
- [ ] `om review -base origin/main` from the worktree; fix blockers and majors; re-run to approve.
- [ ] Attribution grep over `origin/main..HEAD` prints nothing; `git push origin crew/sloan/w2-cobra`.

## Known limits

- cobra adds its `completion` command lazily at Execute time, so the tree walk cannot see it; it is out of scope.
- Disabling prefix matching breaks operator muscle memory such as `gt ref at`; aliases are the supported shortcut.
- bd itself still honours `BEADS_TEST_SERVER=1` on any port (B2-03 fix direction a lives in beads, not here).
