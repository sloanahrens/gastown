# Agent-Bead Migration Completion Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Every rig-scoped agent bead has exactly one authoritative row (in its rig database); the legacy town (`hq`) rows are reconciled, archived and deleted; no gastown code path can write an agent bead anywhere else again.

**Architecture:** `internal/beads` is the only door: `ForAgentBead()` resolves rig-prefixed IDs to the rig database only (no town fallback) and scopes list operations to the wrapper's own database. A structural test blocks shell `bd` writes to agent-bead IDs. `gt doctor` gains a read-only `agent-beads-shadow` check. A new `gt polecat identity reconcile` command performs the per-ID reconcile → archive → delete, run by the mayor one ID at a time.

**Tech Stack:** Go 1.27, cobra CLI, `bd` 1.2.2 subprocess wrapper (`internal/beads`), go/ast for the structural test, existing shell-script `bd` mocks in tests.

**Spec:** `docs/plans/2026-09-09-agent-bead-migration-design.md`

## Global Constraints

- The rig database is the single store for rig-prefixed agent beads; `hq` holds only `hq-` agents. (spec, Decisions 1)
- No change to `bd`. (spec, Decisions 2)
- Legacy `hq` rows are reconciled into the rig row, archived to `~/gt/.beads/archive/agent-bead-legacy.jsonl`, then deleted — one ID per command, never from a patrol. (spec, Decisions 3, Component 4)
- Out of scope: om/beads witness naming (`om-witness` vs `om-om-witness`), the `done`→`idle` state machine, route-first resolution in `bd`. (spec, Decisions 4)
- Tests: `go test` needs `CGO_CPPFLAGS=-I/opt/homebrew/opt/icu4c/include CGO_LDFLAGS=-L/opt/homebrew/opt/icu4c/lib` (the Makefile exports them under `make test`). `//go:build integration` tests need Docker and skip without it.
- Every commit references its bead ID in the subject, e.g. `(gt-xxx)`; after each commit run `bd comments add <bead> "commit: <hash> — <summary>"`.
- Each task is one gastown rig bead under epic **gt-a6g**; Tasks 1→2→3→4→5 are sequential dependencies (each builds on the previous wrapper semantics); Task 6 is run by the mayor after Tasks 1–5 are merged and installed.

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/beads/beads.go` (modify: `ForAgentBead`, `resolveAgentBead`, `agentBeadDirCache`, `pinnedToBeadsDir`) | Resolution contract: rig-prefixed agent IDs → rig DB only; list scope = wrapper's own DB |
| `internal/beads/beads_agent.go` (modify: doc of `ListAgentBeads`; add `DeleteLegacyAgentBead`) | Agent helpers; guarded delete used only by reconcile |
| `internal/beads/agent_reconcile.go` (create) | Pure merge logic: `MergeLegacyAgentBead` + `ReconcileRow` |
| `internal/beads/agent_reconcile_test.go` (create) | Winner rule, ghost-reference clearing |
| `internal/beads/beads_agent_test.go` (modify) | Invert the two legacy-fallback tests; add no-town-probe tests |
| `internal/beads/agent_bead_guard_test.go` (create) | Structural test: no shell `bd` writes to agent-bead IDs outside `internal/beads` |
| `internal/witness/handlers.go:2746-2774` (modify) | `clearCompletionMetadata` through the wrapper |
| `internal/witness/handlers_test.go` (modify) | Update `TestClearCompletionMetadata_NoBd` |
| `internal/doctor/agent_beads_shadow_check.go` (create) | `agent-beads-shadow` check |
| `internal/doctor/agent_beads_shadow_check_test.go` (create) | Fixture-driven check tests |
| `internal/cmd/doctor.go:265`, `internal/cmd/upgrade.go:156` (modify) | Register the new check |
| `internal/cmd/polecat_identity_reconcile.go` (create) | `gt polecat identity reconcile <rig>/<name> [--apply]` |
| `internal/cmd/polecat_identity_reconcile_test.go` (create) | Table rendering + apply ordering with a fake `bd` |
| `internal/cmd/polecat_identity.go:152-159` (modify) | Register the subcommand |
| `docs/plans/2026-09-09-agent-bead-migration-runbook.md` (create, Task 6) | Mayor's per-ID repair record |

---

### Task 1: Resolution contract — rig-prefixed agent IDs never touch the town database

**Files:**
- Modify: `internal/beads/beads.go:632-655` (`ForAgentBead`), `internal/beads/beads.go:663-673` (`agentBeadDirCache`), `internal/beads/beads.go:720-757` (`resolveAgentBead`)
- Modify: `internal/beads/beads_agent_test.go:726-830`
- Test: `internal/beads/beads_agent_test.go`

**Interfaces:**
- Consumes: `setupDualScopeTown(t, home func(rigBeadsDir, townBeadsDir string) string) (townRoot, rigBeadsDir, townBeadsDir, logPath string)` — existing fixture that installs a shell-script `bd` mock logging `beads_dir=<dir> args=<verb>` and answering `show` only for the directory `home` returns.
- Produces: `func (b *Beads) ForAgentBead() *Beads` (unchanged signature; new semantics: returns a copy of `b` with `agentScope=true`, **same** `workDir`/`beadsDir`); `func (b *Beads) resolveAgentBead(id string) *Beads` returns the canonical (prefix-routed) pinned wrapper only.

- [ ] **Step 1: Invert the two legacy-fallback tests into "never probes town" tests**

Replace `TestCreateOrReopenAgentBead_FallsBackToLegacyTownBead` (line 726) with:

```go
// TestCreateOrReopenAgentBead_IgnoresLegacyTownBead: a rig-prefixed agent
// bead that exists ONLY in the town database is legacy state. Writes must go
// to the rig database (creating there), never to the town row.
func TestCreateOrReopenAgentBead_IgnoresLegacyTownBead(t *testing.T) {
	_, rigBeadsDir, townBeadsDir, logPath := setupDualScopeTown(t,
		func(_, town string) string { return town })

	bd := NewWithBeadsDir(filepath.Dir(rigBeadsDir), rigBeadsDir)
	if _, err := bd.CreateOrReopenAgentBead("gt-gastown-polecat-rust", "gt-gastown-polecat-rust", &AgentFields{
		RoleType:   "polecat",
		Rig:        "gastown",
		AgentState: "spawning",
	}); err != nil {
		t.Fatalf("CreateOrReopenAgentBead: %v", err)
	}

	logBytes, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read mock log: %v", err)
	}
	logOutput := string(logBytes)
	if strings.Contains(logOutput, "beads_dir="+townBeadsDir+" args=update") ||
		strings.Contains(logOutput, "beads_dir="+townBeadsDir+" args=create") {
		t.Fatalf("CreateOrReopenAgentBead wrote to the town database for a rig-prefixed ID; log:\n%s", logOutput)
	}
	if !strings.Contains(logOutput, "beads_dir="+rigBeadsDir+" args=create") {
		t.Fatalf("CreateOrReopenAgentBead did not create in the rig database; log:\n%s", logOutput)
	}
}
```

Replace `TestGetAgentBead_DualScopeFindsLegacyTownBead` (line 755) with:

```go
// TestGetAgentBead_DoesNotFallBackToLegacyTownBead: with no rig-local row,
// GetAgentBead returns not-found (nil, nil, nil) and never probes the town
// database. The legacy fallback is what let stale town rows answer for
// migrated agents (hq-kt9y1 census: 10/30 gastown polecats diverged).
func TestGetAgentBead_DoesNotFallBackToLegacyTownBead(t *testing.T) {
	_, rigBeadsDir, townBeadsDir, logPath := setupDualScopeTown(t,
		func(_, town string) string { return town })

	bd := NewWithBeadsDir(filepath.Dir(rigBeadsDir), rigBeadsDir).ForAgentBead()
	issue, fields, err := bd.GetAgentBead("gt-gastown-polecat-rust")
	if err != nil {
		t.Fatalf("GetAgentBead: %v", err)
	}
	if issue != nil || fields != nil {
		t.Fatalf("GetAgentBead returned the legacy town row (issue=%v); rig-prefixed IDs must resolve rig-local only", issue)
	}

	logBytes, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read mock log: %v", err)
	}
	if strings.Contains(string(logBytes), "beads_dir="+townBeadsDir+" args=show") {
		t.Fatalf("GetAgentBead probed the town database for a rig-prefixed ID; log:\n%s", logBytes)
	}
}
```

Keep `TestGetAgentBead_DualScopePrefersRigLocal` as is (it already asserts no town probe when the rig row exists).

Add one more test after it:

```go
// TestForAgentBead_KeepsWrapperDatabaseForLists: ForAgentBead no longer
// re-roots the wrapper to the town database, so list-style operations on a
// rig wrapper list the rig database.
func TestForAgentBead_KeepsWrapperDatabaseForLists(t *testing.T) {
	_, rigBeadsDir, townBeadsDir, logPath := setupDualScopeTown(t,
		func(rig, _ string) string { return rig })

	bd := NewWithBeadsDir(filepath.Dir(rigBeadsDir), rigBeadsDir).ForAgentBead()
	if _, err := bd.ListAgentBeads(); err != nil {
		t.Fatalf("ListAgentBeads: %v", err)
	}

	logBytes, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read mock log: %v", err)
	}
	logOutput := string(logBytes)
	if !strings.Contains(logOutput, "beads_dir="+rigBeadsDir+" args=list") {
		t.Fatalf("ListAgentBeads on a rig wrapper did not list the rig database; log:\n%s", logOutput)
	}
	if strings.Contains(logOutput, "beads_dir="+townBeadsDir+" args=list") {
		t.Fatalf("ListAgentBeads on a rig wrapper listed the town database; log:\n%s", logOutput)
	}
}
```

- [ ] **Step 2: Run the three tests to verify they fail**

Run: `CGO_CPPFLAGS=-I/opt/homebrew/opt/icu4c/include CGO_LDFLAGS=-L/opt/homebrew/opt/icu4c/lib go test ./internal/beads/ -run 'TestCreateOrReopenAgentBead_IgnoresLegacyTownBead|TestGetAgentBead_DoesNotFallBackToLegacyTownBead|TestForAgentBead_KeepsWrapperDatabaseForLists' -v`
Expected: all three FAIL (town `update`/`show`/`list` present in the mock log).

- [ ] **Step 3: Make `ForAgentBead` keep its own database and drop the town fallback**

In `internal/beads/beads.go` replace the body of `ForAgentBead` (line 632) and the comment above it:

```go
// ForAgentBead returns a Beads wrapper suitable for operating on agent beads.
//
// Agent beads (labeled gt:agent) live RIG-LOCAL: rig-prefixed agent IDs
// (e.g. "gt-gastown-polecat-furiosa") belong in their rig's database and
// hq-prefixed global agents (mayor, deacon, dogs) in the town database.
//
// Per-ID operations (Show, Update, and the agent-bead helpers) resolve each
// ID to its canonical prefix-routed database and nowhere else: there is no
// town fallback for rig-prefixed IDs (gt-a6g completed the gt-8we migration;
// legacy town rows are reconciled and deleted by
// `gt polecat identity reconcile`). List-style operations run against the
// wrapper's own database, so a wrapper built from a rig lists that rig's
// agents and one built from the town root lists town agents.
//
// If the wrapper is already pinned or agent-scoped, returns it unchanged.
func (b *Beads) ForAgentBead() *Beads {
	if b.noRoute || b.agentScope {
		return b
	}
	return &Beads{
		workDir:    b.workDir,
		beadsDir:   b.beadsDir,
		isolated:   b.isolated,
		serverPort: b.serverPort,
		store:      b.store,
		townRoot:   b.getTownRoot(),
		agentScope: true,
	}
}
```

Delete `agentBeadDirCache`, `cacheAgentBeadDir` (lines 663-673) and replace `resolveAgentBead` (line 720) with:

```go
// resolveAgentBead returns a wrapper pinned to the canonical database for
// this agent bead ID: the database its prefix routes to (rig-local for rig
// agents, town for hq- agents). No existence probe and no town fallback: a
// rig-prefixed ID whose rig row is missing is simply not found there.
// Pinned wrappers are returned unchanged, and so is b when no town root is
// found.
func (b *Beads) resolveAgentBead(id string) *Beads {
	if b.noRoute {
		return b
	}
	dir := b.agentBeadCanonicalDir(id)
	if dir == "" {
		return b
	}
	return b.pinnedToBeadsDir(dir)
}
```

Remove the now-unused `sync` import only if nothing else in the file uses it (`townRootOnce sync.Once` does — keep it).

- [ ] **Step 4: Run the beads package tests**

Run: `CGO_CPPFLAGS=-I/opt/homebrew/opt/icu4c/include CGO_LDFLAGS=-L/opt/homebrew/opt/icu4c/lib go test ./internal/beads/ 2>&1 | tail -20`
Expected: PASS. If any other test asserted the town fallback (search: `grep -n "legacy town" internal/beads/*_test.go`), it is asserting the removed behaviour — rewrite it to assert rig-only, following the two inversions above.

- [ ] **Step 5: Build and vet**

Run: `make build && go vet ./internal/beads/`
Expected: builds; vet clean.

- [ ] **Step 6: Commit**

```bash
git add internal/beads/beads.go internal/beads/beads_agent_test.go
git commit -m "fix: ForAgentBead resolves rig-prefixed agent beads rig-local only — no town fallback, lists scoped to the wrapper's database (<bead>)"
bd comments add <bead> "commit: $(git rev-parse --short HEAD) — resolveAgentBead canonical-only; ForAgentBead keeps its own DB; legacy-fallback tests inverted"
```

---

### Task 2: Writer audit — witness completion-metadata clearing goes through the wrapper

**Files:**
- Modify: `internal/witness/handlers.go:2746-2774` (`clearCompletionMetadata`) and its call site at `:2554`
- Modify: `internal/witness/handlers_test.go:2418-2428`
- Test: `internal/witness/handlers_test.go`

**Interfaces:**
- Consumes: `beads.New(workDir string) *Beads`, `(*Beads).ForAgentBead()`, `(*Beads).GetAgentBead(id) (*Issue, *AgentFields, error)`, `(*Beads).UpdateAgentDescriptionFields(id, AgentFieldUpdates) error`, `AgentFieldUpdates{ExitType, MRID, Branch, CompletionTime *string}` — all in `internal/beads`.
- Produces: `func clearCompletionMetadata(workDir, agentBeadID string) error` (no `*BdCli` parameter).

- [ ] **Step 1: Inventory every agent-bead access outside `internal/beads`**

Run and paste the output into the bead as a comment (this is the audit record Task 3 must cover):

```bash
grep -rn --include='*.go' -E '(bd\.Run|bd\.Exec|BdCmd|exec\.Command\("bd")\(' internal/ | grep -v _test | grep -iE 'agentbead|agentID|witnessbead|refinerybead|polecatbead|crewbead'
grep -rn --include='*.go' -E 'ListAgentBeads\(|ListAgentBeadsFromWisps\(' internal/ | grep -v _test | grep -v '^internal/beads/'
```

Expected today: exactly one write hit (`internal/witness/handlers.go:2774`). For each `ListAgentBeads` caller, confirm the wrapper it is called on is built from the database it intends to list (`NewRigLocal(rigPath)` / `New(townRoot)` / a rig `New(r.Path).ForAgentBead()`); with Task 1, a rig-derived `ForAgentBead()` wrapper now lists the rig. If a caller built a rig wrapper but needs town agents, change it to `beads.New(townRoot).ForAgentBead()`. Record each decision in the bead comment.

- [ ] **Step 2: Update the existing test to the new signature and add a wrapper-path test**

In `internal/witness/handlers_test.go` replace `TestClearCompletionMetadata_NoBd` (line 2418):

```go
func TestClearCompletionMetadata_NoBeadsDir(t *testing.T) {
	t.Parallel()
	// A workDir with no beads database: the wrapper's Show fails and the
	// function must surface that as an error, not silently succeed.
	err := clearCompletionMetadata(t.TempDir(), "gt-fake-agent")
	if err == nil {
		t.Error("expected error when no beads database is reachable")
	}
}
```

- [ ] **Step 3: Run the test to verify it fails to compile**

Run: `CGO_CPPFLAGS=-I/opt/homebrew/opt/icu4c/include CGO_LDFLAGS=-L/opt/homebrew/opt/icu4c/lib go test ./internal/witness/ -run TestClearCompletionMetadata_NoBeadsDir`
Expected: build FAIL — `clearCompletionMetadata` still takes three arguments.

- [ ] **Step 4: Rewrite `clearCompletionMetadata` through the wrapper**

Replace lines 2743-2774 of `internal/witness/handlers.go`:

```go
// clearCompletionMetadata removes completion metadata fields from an agent
// bead so the same completion is not re-processed on the next patrol cycle.
// It goes through the agent-scoped wrapper (never shell `bd update`): a
// cwd-routed update from the town root landed on the legacy town row and
// left the canonical rig row stale (gt-a6g).
func clearCompletionMetadata(workDir, agentBeadID string) error {
	bd := beads.New(workDir).ForAgentBead()
	issue, fields, err := bd.GetAgentBead(agentBeadID)
	if err != nil {
		return fmt.Errorf("reading agent bead %s: %w", agentBeadID, err)
	}
	if issue == nil || fields == nil {
		return fmt.Errorf("reading agent bead %s: %w", agentBeadID, beads.ErrNotFound)
	}

	empty := ""
	updates := beads.AgentFieldUpdates{
		ExitType:       &empty,
		MRID:           &empty,
		CompletionTime: &empty,
	}
	if !fields.MRFailed && !fields.PushFailed {
		updates.Branch = &empty
	}
	return bd.UpdateAgentDescriptionFields(agentBeadID, updates)
}
```

Update the call at line 2554 from `clearCompletionMetadata(bd, workDir, agentBeadID)` to `clearCompletionMetadata(workDir, agentBeadID)`. If `bd` becomes unused in that function, remove it from the signature or keep it for the other calls — do not leave an unused variable.

- [ ] **Step 5: Run witness tests, build, vet**

Run: `CGO_CPPFLAGS=-I/opt/homebrew/opt/icu4c/include CGO_LDFLAGS=-L/opt/homebrew/opt/icu4c/lib go test ./internal/witness/ 2>&1 | tail -5 && make build && go vet ./internal/witness/`
Expected: PASS; build ok; vet clean.

- [ ] **Step 6: Commit**

```bash
git add internal/witness/handlers.go internal/witness/handlers_test.go
git commit -m "fix: witness clears completion metadata through the agent-bead wrapper, not cwd-routed bd update (<bead>)"
bd comments add <bead> "commit: $(git rev-parse --short HEAD) — clearCompletionMetadata via ForAgentBead; audit inventory recorded above"
```

---

### Task 3: Structural guard — no shell `bd` writes to agent-bead IDs outside `internal/beads`

**Files:**
- Create: `internal/beads/agent_bead_guard_test.go`
- Test: same file

**Interfaces:**
- Consumes: nothing at runtime; walks source with `go/parser`.
- Produces: `TestNoShellBdWritesToAgentBeads` — fails the suite when a production file in the scanned packages passes an agent-bead identifier to `bd.Run`, `bd.Exec`, `BdCmd`, or `exec.Command("bd", …)`, or calls an agent-bead helper directly on a `beads.New*(…)` call result.

- [ ] **Step 1: Write the guard test with a fixture proving it detects each banned pattern**

```go
package beads

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// agentBeadIdentRE matches expressions that carry an agent-bead ID.
var agentBeadIdentRE = regexp.MustCompile(`(?i)agentbead|agentid\b|witnessbeadid|refinerybeadid|polecatbeadid|crewbeadid`)

// agentBeadHelpers are the Beads methods that must only be called on an
// agent-scoped or pinned wrapper — never chained directly onto beads.New*().
var agentBeadHelpers = map[string]bool{
	"UpdateAgentState": true, "UpdateAgentCleanupStatus": true,
	"UpdateAgentDescriptionFields": true, "CreateAgentBead": true,
	"CreateOrReopenAgentBead": true, "ResetAgentBeadForReuse": true,
	"ListAgentBeads": true, "GetAgentBead": true,
}

// TestNoShellBdWritesToAgentBeads is the gt-a6g structural guard. Agent beads
// live in exactly one database per ID; the only code allowed to decide which
// database is internal/beads. A shell `bd update <agentBeadID>` from an
// arbitrary cwd resolves local-first and wrote the legacy town row for months
// (hq-kt9y1). This test fails the build when that pattern reappears.
func TestNoShellBdWritesToAgentBeads(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
	packages := []string{
		"internal/cmd", "internal/witness", "internal/refinery", "internal/daemon",
		"internal/polecat", "internal/doctor", "internal/deacon", "internal/dog",
	}
	var violations []string
	for _, pkg := range packages {
		dir := filepath.Join(repoRoot, pkg)
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			src, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				t.Fatalf("read %s: %v", name, err)
			}
			violations = append(violations, agentBeadShellWrites(t, filepath.Join(pkg, name), src)...)
		}
	}
	if len(violations) > 0 {
		t.Fatalf("agent beads must be read/written through beads.New(...).ForAgentBead() or beads.NewRigLocal(...), never via shell bd or a bare beads.New*() chain:\n%s", strings.Join(violations, "\n"))
	}
}

// agentBeadShellWrites returns one line per violation in src.
func agentBeadShellWrites(t *testing.T, label string, src []byte) []string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, label, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", label, err)
	}
	var out []string
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		pos := fset.Position(call.Pos())
		// (a) shell bd invocations carrying an agent-bead identifier.
		if isShellBdCall(call) {
			for _, arg := range call.Args {
				if agentBeadIdentRE.MatchString(exprString(arg)) {
					out = append(out, pos.String()+": shell bd call with agent-bead argument "+exprString(arg))
					break
				}
			}
		}
		// (b) agent-bead helper chained directly onto beads.New*(...).
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && agentBeadHelpers[sel.Sel.Name] {
			if recv, ok := sel.X.(*ast.CallExpr); ok {
				if rs, ok := recv.Fun.(*ast.SelectorExpr); ok {
					if pkg, ok := rs.X.(*ast.Ident); ok && pkg.Name == "beads" && strings.HasPrefix(rs.Sel.Name, "New") && rs.Sel.Name != "NewRigLocal" {
						out = append(out, pos.String()+": "+sel.Sel.Name+" called directly on beads."+rs.Sel.Name+"(...); insert .ForAgentBead()")
					}
				}
			}
		}
		return true
	})
	return out
}

func isShellBdCall(call *ast.CallExpr) bool {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		return fun.Name == "BdCmd"
	case *ast.SelectorExpr:
		if x, ok := fun.X.(*ast.Ident); ok {
			if x.Name == "bd" && (fun.Sel.Name == "Run" || fun.Sel.Name == "Exec") {
				return true
			}
			if x.Name == "exec" && fun.Sel.Name == "Command" && len(call.Args) > 0 {
				if lit, ok := call.Args[0].(*ast.BasicLit); ok && lit.Value == `"bd"` {
					return true
				}
			}
		}
	}
	return false
}

func exprString(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return exprString(v.X) + "." + v.Sel.Name
	case *ast.CallExpr:
		return exprString(v.Fun) + "()"
	case *ast.BasicLit:
		return v.Value
	default:
		return ""
	}
}

// TestAgentBeadGuardDetectsEachPattern proves the guard is not vacuous.
func TestAgentBeadGuardDetectsEachPattern(t *testing.T) {
	cases := map[string]string{
		"bd.Run":       `package x; func f(bd *BdCli, workDir, agentBeadID string) { _ = bd.Run(workDir, "update", agentBeadID, "--description", "d") }`,
		"BdCmd":        `package x; func f(agentID string) { _ = BdCmd("update", agentID, "--status=open") }`,
		"exec.Command": `package x; import "os/exec"; func f(witnessBeadID string) { _ = exec.Command("bd", "close", witnessBeadID) }`,
		"bare chain":   `package x; func f(id string) { _ = beads.New("/x").UpdateAgentState(id, "idle") }`,
	}
	for name, src := range cases {
		if v := agentBeadShellWrites(t, name+".go", []byte(src)); len(v) == 0 {
			t.Errorf("%s: guard did not flag %q", name, src)
		}
	}
	clean := `package x; func f(id string) { _ = beads.New("/x").ForAgentBead().UpdateAgentState(id, "idle"); _ = beads.NewRigLocal("/x").GetAgentBead(id) }`
	if v := agentBeadShellWrites(t, "clean.go", []byte(clean)); len(v) != 0 {
		t.Errorf("guard flagged compliant code: %v", v)
	}
}
```

- [ ] **Step 2: Run the guard against the tree**

Run: `CGO_CPPFLAGS=-I/opt/homebrew/opt/icu4c/include CGO_LDFLAGS=-L/opt/homebrew/opt/icu4c/lib go test ./internal/beads/ -run 'TestNoShellBdWritesToAgentBeads|TestAgentBeadGuardDetectsEachPattern' -v`
Expected: `TestAgentBeadGuardDetectsEachPattern` PASS. `TestNoShellBdWritesToAgentBeads` PASS if Task 2 removed the only shell write; if it FAILS, each listed line is a real violation from the Task 2 audit that was missed — fix it through the wrapper (same pattern as Task 2 Step 4) and re-run. Do not widen the regexp to make the test pass.

- [ ] **Step 3: Commit**

```bash
git add internal/beads/agent_bead_guard_test.go
git commit -m "test: structural guard — agent beads only through ForAgentBead/NewRigLocal, never shell bd (<bead>)"
bd comments add <bead> "commit: $(git rev-parse --short HEAD) — TestNoShellBdWritesToAgentBeads + self-check fixture"
```

---

### Task 4: `gt doctor` check `agent-beads-shadow`

**Files:**
- Create: `internal/doctor/agent_beads_shadow_check.go`
- Create: `internal/doctor/agent_beads_shadow_check_test.go`
- Modify: `internal/cmd/doctor.go:265`, `internal/cmd/upgrade.go:156`

**Interfaces:**
- Consumes: `beads.LoadRoutes(beadsDir) ([]beads.Route, error)` with `Route{Prefix, Path}`; `beads.NewRigLocal(workDir)`; `(*Beads).ListAgentBeads() (map[string]*Issue, error)`; `beads.ParseAgentFields(desc) *AgentFields`; doctor `Check` interface (`Name, Description, Run(*CheckContext) *CheckResult, Fix(*CheckContext) error, CanFix() bool`); test helpers `setupTownDuplicateFixture(t) string` and `writeTownDuplicateBdScript(t, tmpDir, logFile)` from `agent_beads_check_test.go` (they build a town whose `hq` holds `gs-gastown-witness`/`gs-gastown-refinery` duplicates).
- Produces: `NewAgentBeadsShadowCheck() *AgentBeadsShadowCheck`; `shadowedAgentFields(rig, town *beads.Issue) []string` (names of differing agent fields).

- [ ] **Step 1: Write the failing tests**

```go
package doctor

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

func TestAgentBeadsShadowCheck_ReportsTownDuplicates(t *testing.T) {
	tmpDir := setupTownDuplicateFixture(t)
	writeTownDuplicateBdScript(t, tmpDir, filepath.Join(tmpDir, "bd.log"))

	check := NewAgentBeadsShadowCheck()
	result := check.Run(&CheckContext{TownRoot: tmpDir})

	if result.Status != StatusWarning {
		t.Fatalf("status = %v, want warning; message: %s", result.Status, result.Message)
	}
	for _, want := range []string{"gs-gastown-witness", "gs-gastown-refinery"} {
		found := false
		for _, d := range result.Details {
			if strings.HasPrefix(d, want) {
				found = true
			}
		}
		if !found {
			t.Errorf("expected %s in details, got %v", want, result.Details)
		}
	}
	if !strings.Contains(result.FixHint, "gt polecat identity reconcile") {
		t.Errorf("fix hint must name the reconcile command, got %q", result.FixHint)
	}
	if check.CanFix() {
		t.Error("shadow check must never auto-fix")
	}
}

func TestAgentBeadsShadowCheck_CleanWhenNoRoutes(t *testing.T) {
	result := NewAgentBeadsShadowCheck().Run(&CheckContext{TownRoot: t.TempDir()})
	if result.Status != StatusOK {
		t.Fatalf("status = %v, want OK (no routes => nothing to shadow)", result.Status)
	}
}

func TestShadowedAgentFields(t *testing.T) {
	rig := &beads.Issue{Description: "x\n\nrole_type: polecat\nagent_state: done\nactive_mr: gt-wisp-0yhh\n"}
	town := &beads.Issue{Description: "x\n\nrole_type: polecat\nagent_state: done\nactive_mr: null\n"}
	got := shadowedAgentFields(rig, town)
	if len(got) != 1 || got[0] != "active_mr" {
		t.Fatalf("shadowedAgentFields = %v, want [active_mr]", got)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `CGO_CPPFLAGS=-I/opt/homebrew/opt/icu4c/include CGO_LDFLAGS=-L/opt/homebrew/opt/icu4c/lib go test ./internal/doctor/ -run 'AgentBeadsShadow|ShadowedAgentFields'`
Expected: build FAIL (`NewAgentBeadsShadowCheck` undefined).

- [ ] **Step 3: Implement the check**

```go
package doctor

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
)

// AgentBeadsShadowCheck reports rig-prefixed agent beads that ALSO exist in
// the town database. Such a row is a pre-gt-8we legacy copy: bd resolves the
// local store before routes.jsonl, so a bd call from the town root reads or
// writes the stale copy while gt reads the rig row (hq-kt9y1: 10/30 gastown
// polecats diverged, two slots leaked to ghost active_mr). Detection only —
// repair is `gt polecat identity reconcile`, one ID per command.
type AgentBeadsShadowCheck struct{}

func NewAgentBeadsShadowCheck() *AgentBeadsShadowCheck { return &AgentBeadsShadowCheck{} }

func (c *AgentBeadsShadowCheck) Name() string { return "agent-beads-shadow" }
func (c *AgentBeadsShadowCheck) Description() string {
	return "Rig-prefixed agent beads must not have a legacy copy in the town database"
}
func (c *AgentBeadsShadowCheck) CanFix() bool               { return false }
func (c *AgentBeadsShadowCheck) Fix(_ *CheckContext) error { return fmt.Errorf("agent-beads-shadow has no auto-fix; run gt polecat identity reconcile <rig>/<name>") }

func (c *AgentBeadsShadowCheck) Run(ctx *CheckContext) *CheckResult {
	res := &CheckResult{Name: c.Name(), Category: CategoryRig, Status: StatusOK, Message: "No rig agent beads shadowed in the town database"}
	townBeadsDir := filepath.Join(ctx.TownRoot, ".beads")
	routes, err := beads.LoadRoutes(townBeadsDir)
	if err != nil || len(routes) == 0 {
		return res
	}
	townAgents, err := beads.NewRigLocal(ctx.TownRoot).ListAgentBeads()
	if err != nil {
		res.Status = StatusWarning
		res.Message = "Could not list town agent beads: " + err.Error()
		return res
	}
	for _, route := range routes {
		if route.Path == "." || strings.HasPrefix(route.Prefix, "hq-") {
			continue
		}
		rigDir := filepath.Join(ctx.TownRoot, route.Path)
		rigAgents, rigErr := beads.NewRigLocal(rigDir).ListAgentBeads()
		for id, townIssue := range townAgents {
			if !strings.HasPrefix(id, route.Prefix) {
				continue
			}
			line := id + " exists in town DB (updated " + townIssue.UpdatedAt.Format("2006-01-02T15:04Z") + ")"
			if rigErr != nil {
				line += "; rig DB unreadable: " + rigErr.Error()
			} else if rigIssue, ok := rigAgents[id]; ok {
				diff := shadowedAgentFields(rigIssue, townIssue)
				line += fmt.Sprintf("; rig row updated %s; differing fields: %s", rigIssue.UpdatedAt.Format("2006-01-02T15:04Z"), strings.Join(diff, ","))
				if len(diff) == 0 {
					line += "(none)"
				}
			} else {
				line += "; NO rig row (run gt doctor --fix agent-beads-exist first)"
			}
			res.Details = append(res.Details, line)
		}
	}
	sort.Strings(res.Details)
	if len(res.Details) > 0 {
		res.Status = StatusWarning
		res.Message = fmt.Sprintf("%d rig agent bead(s) shadowed by a legacy town row", len(res.Details))
		res.FixHint = "For each ID: gt polecat identity reconcile <rig>/<name> (dry-run), review, then --apply"
	}
	return res
}

// shadowedAgentFields returns the agent description fields whose values
// differ between the rig row and the town row, in a fixed order.
func shadowedAgentFields(rig, town *beads.Issue) []string {
	r, tn := beads.ParseAgentFields(rig.Description), beads.ParseAgentFields(town.Description)
	if r == nil || tn == nil {
		return []string{"description"}
	}
	var out []string
	add := func(name string, a, b string) {
		if a != b {
			out = append(out, name)
		}
	}
	add("agent_state", r.AgentState, tn.AgentState)
	add("hook_bead", r.HookBead, tn.HookBead)
	add("cleanup_status", r.CleanupStatus, tn.CleanupStatus)
	add("active_mr", r.ActiveMR, tn.ActiveMR)
	add("exit_type", r.ExitType, tn.ExitType)
	add("mr_id", r.MRID, tn.MRID)
	add("branch", r.Branch, tn.Branch)
	add("last_source_issue", r.LastSourceIssue, tn.LastSourceIssue)
	add("completion_time", r.CompletionTime, tn.CompletionTime)
	return out
}
```

Check `Issue.UpdatedAt`'s actual type in `internal/beads` (`grep -n "UpdatedAt" internal/beads/beads.go`); if it is a `string`, print it directly instead of calling `Format`.

Register it: in `internal/cmd/doctor.go` after line 265 add `d.Register(doctor.NewAgentBeadsShadowCheck())`; same in `internal/cmd/upgrade.go` after line 156.

- [ ] **Step 4: Run doctor tests, build, vet**

Run: `CGO_CPPFLAGS=-I/opt/homebrew/opt/icu4c/include CGO_LDFLAGS=-L/opt/homebrew/opt/icu4c/lib go test ./internal/doctor/ 2>&1 | tail -5 && make build && go vet ./internal/doctor/ ./internal/cmd/`
Expected: PASS, build ok, vet clean. Then a live read-only run: `./gt doctor --rig gastown 2>&1 | grep agent-beads-shadow` — expected today: warning listing 30 gastown IDs.

- [ ] **Step 5: Commit**

```bash
git add internal/doctor/agent_beads_shadow_check.go internal/doctor/agent_beads_shadow_check_test.go internal/cmd/doctor.go internal/cmd/upgrade.go
git commit -m "feat: gt doctor agent-beads-shadow reports rig agent beads with a legacy town copy (<bead>)"
bd comments add <bead> "commit: $(git rev-parse --short HEAD) — agent-beads-shadow check, detection only"
```

---

### Task 5: `gt polecat identity reconcile <rig>/<name> [--apply]`

**Files:**
- Create: `internal/beads/agent_reconcile.go`, `internal/beads/agent_reconcile_test.go`
- Modify: `internal/beads/beads_agent.go` (add `DeleteLegacyAgentBead`)
- Create: `internal/cmd/polecat_identity_reconcile.go`, `internal/cmd/polecat_identity_reconcile_test.go`
- Modify: `internal/cmd/polecat_identity.go:152-159`

**Interfaces:**
- Consumes: `beads.NewRigLocal`, `(*Beads).GetAgentBead`, `(*Beads).Show`, `(*Beads).UpdateAgentDescriptionFields`, `beads.ResolveBeadsDirForID(townBeadsDir, id) string`, `beads.GetTownBeadsPath(townRoot)`, `beads.FindTownRoot(dir)`, `polecatBeadIDForRig(r, rigName, name)` and `getRig(name)` from `internal/cmd/polecat_identity.go`.
- Produces:
  - `type ReconcileRow struct { Field, Rig, Town, Winner string }`
  - `func MergeLegacyAgentBead(rig, town *Issue, beadExists func(id string) bool) (AgentFieldUpdates, []ReconcileRow)` — per differing field, the row with the newer `UpdatedAt` wins; `active_mr`/`hook_bead` values whose bead does not exist are cleared (`Winner: "clear"`); **`cleanup_status` reconciles by severity, never by recency** (blocking > unknown > clean; refinery review hq-wisp-j0g — this field is behind four fail-open P0s).
  - `func (b *Beads) DeleteLegacyAgentBead(id string) error` — refuses unless `b.noRoute` and the row is an agent bead; runs `bd delete <id> --force`.
  - CLI: `gt polecat identity reconcile <rig>/<name>` (dry-run) and `--apply`.

- [ ] **Step 1: Write the merge-rule tests**

`internal/beads/agent_reconcile_test.go`:

```go
package beads

import (
	"testing"
	"time"
)

func agentIssue(desc string, updated time.Time) *Issue {
	return &Issue{ID: "gt-gastown-polecat-garnet", Description: "t\n\nrole_type: polecat\nrig: gastown\n" + desc, UpdatedAt: updated}
}

func TestMergeLegacyAgentBead_NewerRowWinsPerField(t *testing.T) {
	older, newer := time.Date(2026, 9, 8, 18, 55, 0, 0, time.UTC), time.Date(2026, 9, 9, 1, 7, 0, 0, time.UTC)
	rig := agentIssue("agent_state: spawning\nhook_bead: gt-911\ncleanup_status: null\n", older)
	town := agentIssue("agent_state: done\nhook_bead: null\ncleanup_status: clean\n", newer)
	exists := func(string) bool { return true }

	updates, rows := MergeLegacyAgentBead(rig, town, exists)
	if updates.AgentState == nil || *updates.AgentState != "done" {
		t.Fatalf("agent_state: newer town value must win, got %v", updates.AgentState)
	}
	if updates.CleanupStatus == nil || *updates.CleanupStatus != "clean" {
		t.Fatalf("cleanup_status: newer town value must win, got %v", updates.CleanupStatus)
	}
	if updates.HookBead == nil || *updates.HookBead != "" {
		t.Fatalf("hook_bead: newer town value (null) must win, got %v", updates.HookBead)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3 differing fields", len(rows))
	}
}

func TestMergeLegacyAgentBead_GhostReferencesAreClearedNeverCopied(t *testing.T) {
	older, newer := time.Date(2026, 9, 8, 18, 55, 0, 0, time.UTC), time.Date(2026, 9, 9, 1, 7, 0, 0, time.UTC)
	// rig row is NEWER and carries a ghost active_mr; town says null.
	rig := agentIssue("agent_state: done\nactive_mr: gt-wisp-0yhh\n", newer)
	town := agentIssue("agent_state: done\nactive_mr: null\n", older)
	exists := func(id string) bool { return id != "gt-wisp-0yhh" }

	updates, rows := MergeLegacyAgentBead(rig, town, exists)
	if updates.ActiveMR == nil || *updates.ActiveMR != "" {
		t.Fatalf("active_mr referencing a missing bead must be cleared, got %v", updates.ActiveMR)
	}
	if len(rows) != 1 || rows[0].Field != "active_mr" || rows[0].Winner != "clear" {
		t.Fatalf("rows = %+v, want one active_mr row with winner=clear", rows)
	}
}

// cleanup_status is the field behind four consecutive fail-open P0s (gt-7kr,
// gt-14a, gt-hsg, gt-ido). Recency must never promote a blocking value to a
// clearing one; severity decides.
func TestMergeLegacyAgentBead_CleanupStatusReconcilesBySeverityNotRecency(t *testing.T) {
	older, newer := time.Date(2026, 9, 8, 18, 55, 0, 0, time.UTC), time.Date(2026, 9, 9, 1, 7, 0, 0, time.UTC)
	exists := func(string) bool { return true }

	// newer 'clean' must NOT overwrite older blocking 'has_unpushed'
	rig := agentIssue("cleanup_status: has_unpushed\n", older)
	town := agentIssue("cleanup_status: clean\n", newer)
	updates, rows := MergeLegacyAgentBead(rig, town, exists)
	if updates.CleanupStatus != nil {
		t.Fatalf("newer 'clean' must not overwrite blocking 'has_unpushed'; got update %q", *updates.CleanupStatus)
	}
	if len(rows) != 1 || rows[0].Field != "cleanup_status" || rows[0].Winner != "rig (severity)" {
		t.Fatalf("rows = %+v, want one cleanup_status row won by rig on severity", rows)
	}

	// newer unknown ('' / null) DOES beat older 'clean': unknown fails closed
	rig2 := agentIssue("cleanup_status: clean\n", older)
	town2 := agentIssue("cleanup_status: null\n", newer)
	updates2, _ := MergeLegacyAgentBead(rig2, town2, exists)
	if updates2.CleanupStatus == nil || *updates2.CleanupStatus != "" {
		t.Fatalf("unknown must beat clean (fail closed); got %v", updates2.CleanupStatus)
	}

	// older blocking on the TOWN side also wins over newer rig 'clean'
	rig3 := agentIssue("cleanup_status: clean\n", newer)
	town3 := agentIssue("cleanup_status: has_stash\n", older)
	updates3, _ := MergeLegacyAgentBead(rig3, town3, exists)
	if updates3.CleanupStatus == nil || *updates3.CleanupStatus != "has_stash" {
		t.Fatalf("blocking town value must win over newer rig 'clean'; got %v", updates3.CleanupStatus)
	}
}

func TestMergeLegacyAgentBead_IdenticalRowsProduceNoUpdates(t *testing.T) {
	ts := time.Date(2026, 9, 9, 4, 43, 0, 0, time.UTC)
	rig := agentIssue("agent_state: done\ncleanup_status: clean\n", ts)
	town := agentIssue("agent_state: done\ncleanup_status: clean\n", ts)
	updates, rows := MergeLegacyAgentBead(rig, town, func(string) bool { return true })
	if updates != (AgentFieldUpdates{}) || len(rows) != 0 {
		t.Fatalf("identical rows must produce no updates; got %+v / %+v", updates, rows)
	}
}
```

If `Issue.UpdatedAt` is not a `time.Time` in this codebase, adapt `agentIssue` to set the real field/type and have `MergeLegacyAgentBead` parse it — the rule is unchanged.

- [ ] **Step 2: Run to verify failure**

Run: `CGO_CPPFLAGS=-I/opt/homebrew/opt/icu4c/include CGO_LDFLAGS=-L/opt/homebrew/opt/icu4c/lib go test ./internal/beads/ -run MergeLegacyAgentBead`
Expected: build FAIL (`MergeLegacyAgentBead` undefined).

- [ ] **Step 3: Implement the merge rule and the guarded delete**

`internal/beads/agent_reconcile.go`:

```go
package beads

// ReconcileRow is one differing agent field in a reconcile table.
type ReconcileRow struct {
	Field  string
	Rig    string // value on the canonical rig row
	Town   string // value on the legacy town row
	Winner string // "rig", "town", or "clear"
}

// MergeLegacyAgentBead computes the field updates that bring the canonical
// rig row up to date with a legacy town copy before the copy is deleted.
// Rule (gt-a6g): per differing field the row with the newer UpdatedAt wins,
// except that active_mr and hook_bead values whose bead no longer exists are
// cleared, never copied. Identical rows produce no updates.
func MergeLegacyAgentBead(rig, town *Issue, beadExists func(id string) bool) (AgentFieldUpdates, []ReconcileRow) {
	var updates AgentFieldUpdates
	var rows []ReconcileRow
	r, t := ParseAgentFields(rig.Description), ParseAgentFields(town.Description)
	if r == nil || t == nil {
		return updates, rows
	}
	townNewer := town.UpdatedAt.After(rig.UpdatedAt)

	pick := func(field, rigVal, townVal string, dst **string, isRef bool) {
		if rigVal == townVal {
			return
		}
		winner, val := "rig", rigVal
		if townNewer {
			winner, val = "town", townVal
		}
		if isRef && val != "" && !beadExists(val) {
			winner, val = "clear", ""
		}
		if winner != "rig" || isRef {
			v := val
			*dst = &v
		}
		rows = append(rows, ReconcileRow{Field: field, Rig: rigVal, Town: townVal, Winner: winner})
	}

	pick("agent_state", r.AgentState, t.AgentState, &updates.AgentState, false)
	pick("hook_bead", r.HookBead, t.HookBead, &updates.HookBead, true)
	// cleanup_status reconciles by SEVERITY, never by recency (refinery
	// review hq-wisp-j0g): it is the field behind four fail-open P0s. A
	// blocking value beats unknown beats clean regardless of updated_at, so
	// a stale 'clean' can never manufacture a clearance.
	if r.CleanupStatus != t.CleanupStatus {
		winner := "rig"
		if cleanupSeverity(t.CleanupStatus) > cleanupSeverity(r.CleanupStatus) {
			winner = "town"
			v := t.CleanupStatus
			updates.CleanupStatus = &v
		}
		rows = append(rows, ReconcileRow{Field: "cleanup_status", Rig: r.CleanupStatus, Town: t.CleanupStatus, Winner: winner + " (severity)"})
	}
	pick("active_mr", r.ActiveMR, t.ActiveMR, &updates.ActiveMR, true)
	pick("exit_type", r.ExitType, t.ExitType, &updates.ExitType, false)
	pick("mr_id", r.MRID, t.MRID, &updates.MRID, false)
	pick("branch", r.Branch, t.Branch, &updates.Branch, false)
	pick("last_source_issue", r.LastSourceIssue, t.LastSourceIssue, &updates.LastSourceIssue, false)
	pick("completion_time", r.CompletionTime, t.CompletionTime, &updates.CompletionTime, false)
	return updates, rows
}

// cleanupSeverity orders cleanup_status values for reconciliation:
// blocking (has_uncommitted, has_stash, has_unpushed, anything unrecognised)
// > unknown ('' or null, which fails closed) > clean. Only 'clean' clears.
func cleanupSeverity(v string) int {
	switch v {
	case "clean":
		return 0
	case "", "null":
		return 1
	default:
		return 2
	}
}
```

Note the ghost-clear branch: when the rig row itself carries the ghost (winner would be "rig"), `isRef` forces the update to be emitted so the rig row gets cleared. `ParseAgentFields` maps `null` to `""` — verify with `grep -n '"null"' internal/beads/beads_agent.go`; if it does not, normalise `"null"` to `""` inside `pick` before comparing.

Append to `internal/beads/beads_agent.go`:

```go
// DeleteLegacyAgentBead permanently deletes an agent bead row from the
// database this wrapper is PINNED to. It exists for the gt-a6g reconcile
// command only: the wrapper must be pinned (NewRigLocal) so the caller has
// chosen the database explicitly, and the row must be an agent bead.
func (b *Beads) DeleteLegacyAgentBead(id string) error {
	if !b.noRoute {
		return fmt.Errorf("DeleteLegacyAgentBead requires a pinned wrapper (beads.NewRigLocal)")
	}
	issue, err := b.Show(id)
	if err != nil {
		return err
	}
	if !IsAgentBead(issue) {
		return fmt.Errorf("refusing to delete %s: not an agent bead (type=%s)", id, issue.Type)
	}
	return b.deleteBead(id)
}
```

- [ ] **Step 4: Run the beads tests**

Run: `CGO_CPPFLAGS=-I/opt/homebrew/opt/icu4c/include CGO_LDFLAGS=-L/opt/homebrew/opt/icu4c/lib go test ./internal/beads/ 2>&1 | tail -5`
Expected: PASS (including the Task 3 guard — `DeleteLegacyAgentBead` lives inside `internal/beads`, which the guard does not scan).

- [ ] **Step 5: Write the CLI test (table + apply ordering) with a fake `bd`**

`internal/cmd/polecat_identity_reconcile_test.go` — model the fixture on `internal/beads/beads_agent_test.go:setupDualScopeTown` (copy the helper into this test file as `setupReconcileTown`; it must write `mayor/town.json`, `.beads/routes.jsonl` with `{"prefix":"gt-","path":"gastown/mayor/rig"}`, `gastown/.beads/redirect` → `mayor/rig/.beads`, and a `bd` shell script on `PATH` that logs `beads_dir=$BEADS_DIR args=$*` to `logPath`, answers `show <id> --json` with a JSON array containing one issue whose description is chosen per directory, answers `delete` and `update` with exit 0, and answers `show` for `gt-wisp-0yhh` with exit 1):

```go
func TestReconcile_DryRunPrintsTableAndWritesNothing(t *testing.T) {
	townRoot, logPath := setupReconcileTown(t,
		"agent_state: done\nactive_mr: gt-wisp-0yhh\n", // rig row
		"agent_state: done\nactive_mr: null\n")           // town row
	var out bytes.Buffer
	err := runReconcile(&out, townRoot, "gastown", "garnet", false)
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if !strings.Contains(out.String(), "active_mr") || !strings.Contains(out.String(), "clear") {
		t.Fatalf("table must show active_mr with winner clear:\n%s", out.String())
	}
	log, _ := os.ReadFile(logPath)
	if strings.Contains(string(log), "args=update") || strings.Contains(string(log), "args=delete") {
		t.Fatalf("dry-run must not write; log:\n%s", log)
	}
}

func TestReconcile_ApplyUpdatesRigThenArchivesThenDeletesTown(t *testing.T) {
	townRoot, logPath := setupReconcileTown(t,
		"agent_state: done\nactive_mr: gt-wisp-0yhh\n",
		"agent_state: done\nactive_mr: null\n")
	var out bytes.Buffer
	if err := runReconcile(&out, townRoot, "gastown", "garnet", true); err != nil {
		t.Fatalf("apply: %v", err)
	}
	log, _ := os.ReadFile(logPath)
	rigBeads := filepath.Join(townRoot, "gastown", "mayor", "rig", ".beads")
	townBeads := filepath.Join(townRoot, ".beads")
	upd := strings.Index(string(log), "beads_dir="+rigBeads+" args=update")
	del := strings.Index(string(log), "beads_dir="+townBeads+" args=delete")
	if upd == -1 || del == -1 || upd > del {
		t.Fatalf("expected rig update BEFORE town delete; log:\n%s", log)
	}
	archive, err := os.ReadFile(filepath.Join(townRoot, ".beads", "archive", "agent-bead-legacy.jsonl"))
	if err != nil || !strings.Contains(string(archive), `"gt-gastown-polecat-garnet"`) {
		t.Fatalf("archive must contain the town row before delete: %v / %s", err, archive)
	}
	if fi, _ := os.Stat(filepath.Join(townRoot, ".beads", "archive", "agent-bead-legacy.jsonl")); fi.ModTime().After(time.Now()) {
		t.Fatal("unreachable")
	}
}

func TestReconcile_RefusesWhenNoTownRow(t *testing.T) {
	townRoot, _ := setupReconcileTown(t, "agent_state: done\n", "") // empty => town show exits 1
	err := runReconcile(io.Discard, townRoot, "gastown", "garnet", true)
	if err == nil || !strings.Contains(err.Error(), "no legacy town row") {
		t.Fatalf("expected refusal, got %v", err)
	}
}
```

- [ ] **Step 6: Run to verify failure**

Run: `CGO_CPPFLAGS=-I/opt/homebrew/opt/icu4c/include CGO_LDFLAGS=-L/opt/homebrew/opt/icu4c/lib go test ./internal/cmd/ -run 'TestReconcile_'`
Expected: build FAIL (`runReconcile` undefined).

- [ ] **Step 7: Implement the command**

`internal/cmd/polecat_identity_reconcile.go`:

```go
package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/beads"
)

var polecatIdentityReconcileApply bool

var polecatIdentityReconcileCmd = &cobra.Command{
	Use:   "reconcile <rig>/<name>",
	Short: "Merge a legacy town copy of an agent bead into its rig row, archive it, and delete it",
	Long: `Completes the gt-8we migration for ONE agent bead (gt-a6g).

Dry-run (default) prints a field table: rig value, town value, winner.
--apply then (1) writes the merged fields to the rig row, (2) appends the
full town row to <town>/.beads/archive/agent-bead-legacy.jsonl, (3) deletes
the town row, (4) re-reads both stores and fails loudly on any mismatch.
Run it one ID at a time from an operator session; never from a patrol.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		rigName, name, ok := strings.Cut(args[0], "/")
		if !ok {
			return fmt.Errorf("expected <rig>/<name>, got %q", args[0])
		}
		townRoot, err := findTownRoot()
		if err != nil {
			return err
		}
		return runReconcile(os.Stdout, townRoot, rigName, name, polecatIdentityReconcileApply)
	},
}

func init() {
	polecatIdentityReconcileCmd.Flags().BoolVar(&polecatIdentityReconcileApply, "apply", false, "perform the merge, archive and delete (default is dry-run)")
}

// runReconcile is the testable core of `gt polecat identity reconcile`.
func runReconcile(out io.Writer, townRoot, rigName, name string, apply bool) error {
	id := fmt.Sprintf("%s-%s-polecat-%s", beads.GetPrefixForRig(townRoot, rigName), rigName, name)
	townBeadsDir := beads.GetTownBeadsPath(townRoot)
	rigBeadsDir := beads.ResolveBeadsDirForID(townBeadsDir, id)
	if beads.ResolveBeadsDir(rigBeadsDir) == beads.ResolveBeadsDir(townBeadsDir) {
		return fmt.Errorf("%s routes to the town database; nothing to reconcile", id)
	}
	rigBd := beads.NewRigLocal(filepath.Dir(rigBeadsDir))
	townBd := beads.NewRigLocal(townRoot)

	rigIssue, _, err := rigBd.GetAgentBead(id)
	if err != nil {
		return fmt.Errorf("reading rig row %s: %w", id, err)
	}
	if rigIssue == nil {
		return fmt.Errorf("%s has no rig row; run 'gt doctor --rig %s --fix' (agent-beads-exist) first", id, rigName)
	}
	townIssue, _, err := townBd.GetAgentBead(id)
	if err != nil {
		return fmt.Errorf("reading town row %s: %w", id, err)
	}
	if townIssue == nil {
		return fmt.Errorf("%s has no legacy town row; nothing to reconcile", id)
	}

	exists := func(ref string) bool {
		_, err := beads.New(townRoot).Show(ref)
		return err == nil
	}
	updates, rows := beads.MergeLegacyAgentBead(rigIssue, townIssue, exists)

	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintf(tw, "field\trig(%s)\ttown\twinner\n", rigName)
	fmt.Fprintf(tw, "updated_at\t%v\t%v\t\n", rigIssue.UpdatedAt, townIssue.UpdatedAt)
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.Field, orNull(r.Rig), orNull(r.Town), r.Winner)
	}
	if len(rows) == 0 {
		fmt.Fprintln(tw, "(no differing fields; --apply is archive+delete only)")
	}
	tw.Flush()
	if !apply {
		fmt.Fprintln(out, "dry-run: nothing written. Re-run with --apply to merge, archive and delete.")
		return nil
	}

	if updates != (beads.AgentFieldUpdates{}) {
		if err := rigBd.UpdateAgentDescriptionFields(id, updates); err != nil {
			return fmt.Errorf("step 1/4 update rig row: %w", err)
		}
	}
	archivePath := filepath.Join(townBeadsDir, "archive", "agent-bead-legacy.jsonl")
	if err := appendJSONLine(archivePath, townIssue); err != nil {
		return fmt.Errorf("step 2/4 archive town row: %w", err)
	}
	if err := townBd.DeleteLegacyAgentBead(id); err != nil {
		return fmt.Errorf("step 3/4 delete town row (archived at %s): %w", archivePath, err)
	}
	if _, err := townBd.Show(id); err == nil {
		return fmt.Errorf("step 4/4 verify: town row %s still exists after delete", id)
	}
	after, _, err := rigBd.GetAgentBead(id)
	if err != nil || after == nil {
		return fmt.Errorf("step 4/4 verify: rig row %s unreadable after merge: %v", id, err)
	}
	fmt.Fprintf(out, "reconciled %s: rig row updated, town row archived to %s and deleted\n", id, archivePath)
	return nil
}

func orNull(s string) string {
	if s == "" {
		return "null"
	}
	return s
}

func appendJSONLine(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	return err
}
```

Verify the helper names exist before use and substitute the real ones if they differ: `findTownRoot()` (`grep -rn "func findTownRoot\|func FindTownRoot" internal/cmd internal/beads`), `beads.GetPrefixForRig(townRoot, rig)` (seen in `beads.go` `targetBeadsDirForCreate`). Register in `internal/cmd/polecat_identity.go` after line 156: `polecatIdentityCmd.AddCommand(polecatIdentityReconcileCmd)`.

- [ ] **Step 8: Run cmd tests for the command, build, vet**

Run: `CGO_CPPFLAGS=-I/opt/homebrew/opt/icu4c/include CGO_LDFLAGS=-L/opt/homebrew/opt/icu4c/lib go test ./internal/cmd/ -run 'TestReconcile_' -v && make build && go vet ./internal/cmd/`
Expected: 3 PASS; build ok; vet clean. Then the guard: `go test ./internal/beads/ -run TestNoShellBdWritesToAgentBeads` — PASS (the command uses only wrapper methods).

- [ ] **Step 9: Live dry-run (read-only) against the real town**

Run: `./gt polecat identity reconcile gastown/garnet`
Expected: a table showing `active_mr  gt-wisp-0yhh  null  clear` and the dry-run footer; `gt agents state`/`bd show` unchanged afterwards.

- [ ] **Step 10: Commit**

```bash
git add internal/beads/agent_reconcile.go internal/beads/agent_reconcile_test.go internal/beads/beads_agent.go internal/cmd/polecat_identity_reconcile.go internal/cmd/polecat_identity_reconcile_test.go internal/cmd/polecat_identity.go
git commit -m "feat: gt polecat identity reconcile — merge legacy town agent bead into rig row, archive, delete (<bead>)"
bd comments add <bead> "commit: $(git rev-parse --short HEAD) — MergeLegacyAgentBead rule, DeleteLegacyAgentBead guard, reconcile CLI with dry-run default"
```

---

### Task 6: Data repair (mayor session, after Tasks 1–5 are merged AND installed)

**Files:**
- Create: `docs/plans/2026-09-09-agent-bead-migration-runbook.md` (the record of what was run, per ID)

**Interfaces:**
- Consumes: `gt doctor --rig <rig>` (`agent-beads-shadow`), `gt polecat identity reconcile <rig>/<name> [--apply]`.
- Produces: `agent-beads-shadow` reports 0 in every rig; live acceptance passes.

- [ ] **Step 1: Confirm the installed binary carries Tasks 1–5**

Run: `gt version && git -C ~/gt/gastown/mayor/rig log --oneline -1 origin/main && gt doctor --rig gastown 2>&1 | grep -A3 agent-beads-shadow`
Expected: `gt version` commit is a descendant of the Task 5 merge; shadow check lists the gastown IDs (30 today). Do not proceed on a stale binary — that is exactly the merged-vs-in-effect gap of gt-50k.

- [ ] **Step 2: Diverged IDs first, one at a time, dry-run reviewed before apply**

For each of `garnet shale jade agate malachite mica pyrite slate obsidian pearl` (the diverged set from hq-kt9y1; re-derive from the doctor output, it is authoritative):

```bash
gt polecat identity reconcile gastown/<name>           # review the table
gt polecat identity reconcile gastown/<name> --apply   # only after the table is accepted
gt doctor --rig gastown 2>&1 | grep -c "gt-gastown-polecat-<name> exists"   # expect 0
```

Paste each table into the runbook with the decision. Stop and escalate to the overseer if any table proposes copying a value from a row older than the live state you can observe (`tmux ls`, `gt polecat list`).

- [ ] **Step 3: Identical IDs (delete-only), one at a time**

Same two commands per ID; the table reads "(no differing fields)". Record the count.

- [ ] **Step 4: om and beads rigs**

Run `gt doctor --rig om` and `gt doctor --rig beads`; reconcile any reported IDs the same way. (om/be witness/refinery naming is out of scope — if `om-witness` is reported as shadowed only because of the naming wrinkle, record it in the runbook and leave it.)

- [ ] **Step 5: Live acceptance (from the spec)**

```bash
diff <(cd ~/gt && bd show gt-gastown-polecat-garnet --json | python3 -c 'import json,sys;print(json.load(sys.stdin)[0]["updated_at"])') \
     <(cd ~/gt/gastown/refinery/rig && bd show gt-gastown-polecat-garnet --json | python3 -c 'import json,sys;print(json.load(sys.stdin)[0]["updated_at"])') && echo SAME
gt polecat list gastown | grep -A1 -E "garnet|shale"      # no active_mr
gt doctor --rig gastown 2>&1 | grep agent-beads-shadow    # ✓ / 0 shadowed
gt agents resolve --role witness --rig gastown             # resolves
```

Expected: `SAME`; garnet/shale show `idle-*` reuse with no `active_mr`; shadow check clean; resolve succeeds. Then sling any small bead to gastown and confirm the allocator can pick garnet or shale.

- [ ] **Step 6: Close out**

Commit the runbook on a docs branch through the normal queue; comment on gt-a6g with the archive line count and the before/after shadow counts; close gt-a6g. Leave gt-2h6 open (reclaim defect) and file the two deferred beads (om/be naming; `done`→`idle` semantics) if not already filed.

---

## Self-review

- **Spec coverage:** Component 1 → Task 1; Component 2 → Tasks 2–3; Component 3 → Task 4; Component 4 → Task 5; Component 5 (sequencing) and live acceptance → Task 6; error handling (resolver not-found, reconcile refusals, doctor warning on unreadable store) → Tasks 1, 5, 4; rollback → archive file + Dolt history noted in Task 5/6.
- **Placeholders:** none; every code step carries the code. Two "verify the real name" notes (`Issue.UpdatedAt` type, `findTownRoot`/`GetPrefixForRig`) are explicit checks with the grep to run, not deferrals.
- **Type consistency:** `MergeLegacyAgentBead(rig, town *Issue, beadExists func(string) bool) (AgentFieldUpdates, []ReconcileRow)` is used identically in Task 5 tests and CLI; `DeleteLegacyAgentBead(id) error` matches; `clearCompletionMetadata(workDir, agentBeadID string) error` matches Task 2 test and call site; `NewAgentBeadsShadowCheck()` and `shadowedAgentFields(rig, town *beads.Issue) []string` match Task 4 tests.
