package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// formulaTown is a temp town whose routes.jsonl maps each prefix to a path.
// InstantiateFormulaOnBead reads the routes to pick the bead's rig; nothing
// reads the cwd.
func formulaTown(t *testing.T, routes ...string) string {
	t.Helper()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, ".beads"), 0755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, ".beads", "routes.jsonl"), []byte(strings.Join(routes, "\n")+"\n"), 0644); err != nil {
		t.Fatalf("write routes.jsonl: %v", err)
	}
	return townRoot
}

// polecatWorkTree is the cook tree of a formula declaring mol-polecat-work's
// vars: a required issue and the defaulted commands.
const polecatWorkTree = `{"formula": "mol-polecat-work", "type": "workflow", "vars": [
  {"name": "base_branch", "required": false, "default": "main", "value": "main", "provided": false},
  {"name": "build_command", "required": false, "default": "", "value": "", "provided": false},
  {"name": "issue", "required": true, "default": null, "value": "gt-x", "provided": true},
  {"name": "lint_command", "required": false, "default": "", "value": "", "provided": false},
  {"name": "setup_command", "required": false, "default": "", "value": "", "provided": false},
  {"name": "test_command", "required": false, "default": "", "value": "", "provided": false},
  {"name": "typecheck_command", "required": false, "default": "", "value": "", "provided": false}
], "unresolved_vars": [], "warnings": [], "steps": [{"id": "load-context", "title": "Load", "children": []}]}`

// formulaBDFake is a formula engine whose cook prints polecatWorkTree and
// whose bond prints bondOut.
func formulaBDFake(bondOut string) *fakeCook {
	return &fakeCook{out: cookTree(polecatWorkTree), bondOut: []byte(bondOut)}
}

// formulaBDVia is a formulaBD over open whose contention backoff does not wait.
func formulaBDVia(open func(formulaSite) formulaEngine) formulaBD {
	return formulaBD{open: open, sleep: noSleep}
}

// logLineWith returns the first logged line containing needle, or "".
func logLineWith(log, needle string) string {
	for _, line := range strings.Split(log, "\n") {
		if strings.Contains(line, needle) {
			return line
		}
	}
	return ""
}

// TestInstantiateFormulaOnBead verifies the formula-on-bead pattern used by
// issue #288: cook the formula, then bond it straight onto the bead with the
// caller's extra vars, never through the legacy mol wisp.
func TestInstantiateFormulaOnBead(t *testing.T) {
	t.Parallel()
	townRoot := formulaTown(t, `{"prefix":"gt-","path":"gastown/mayor/rig"}`, `{"prefix":"hq-","path":"."}`)
	bd := formulaBDFake(`{"result_id":"gt-abc123","id_mapping":{"mol-polecat-work":"gt-wisp-288"}}`)

	extraVars := []string{"branch=polecat/furiosa/gt-abc123"}
	result, err := formulaBDVia(bd.open).instantiate("mol-polecat-work", "gt-abc123", "Test Bug Fix", "", townRoot, extraVars)
	if err != nil {
		t.Fatalf("InstantiateFormulaOnBead failed: %v", err)
	}
	if result.WispRootID != "gt-wisp-288" {
		t.Errorf("WispRootID = %q, want gt-wisp-288", result.WispRootID)
	}
	if result.BeadToHook != "gt-abc123" {
		t.Errorf("BeadToHook = %q, want the base bead gt-abc123", result.BeadToHook)
	}

	log := bd.log()
	if !strings.Contains(log, "cook mol-polecat-work --var feature=Test Bug Fix --var issue=gt-abc123") {
		t.Errorf("cook command not found in log:\n%s", log)
	}
	if strings.Contains(log, "mol wisp") {
		t.Errorf("legacy mol wisp command should not be called:\n%s", log)
	}
	if !strings.Contains(log, "--var branch=polecat/furiosa/gt-abc123") {
		t.Errorf("extra vars not passed to bond command:\n%s", log)
	}
	if !strings.Contains(log, "mol bond mol-polecat-work gt-abc123 --json --ephemeral") {
		t.Errorf("direct mol bond command not found in log:\n%s", log)
	}
}

// TestInstantiateFormulaOnBead_CookFailureFailsClosed: a formula bd cannot
// cook fails the pour with one line, before any bond.
func TestInstantiateFormulaOnBead_CookFailureFailsClosed(t *testing.T) {
	t.Parallel()
	townRoot := formulaTown(t, `{"prefix":"gt-","path":"."}`)
	fake := &fakeCook{msg: "invalid formula shiny.formula.toml: line 7: steps.acceptance: unknown key"}

	_, err := formulaBDVia(fake.open).instantiate("shiny", "gt-test", "Test", "", townRoot, nil)
	if err == nil {
		t.Fatal("cook failure must fail the pour")
	}
	if strings.Contains(err.Error(), "\n") || !strings.HasPrefix(err.Error(), "cook formula shiny: invalid formula") {
		t.Errorf("error = %q, want one line naming the formula and bd's message", err)
	}
	if bonds := fake.called("mol bond "); len(bonds) != 0 {
		t.Errorf("bond ran after a failed cook: %v", bonds)
	}
}

// TestCookFormula verifies the CookFormula helper cooks the named formula in
// the work dir, for the town.
func TestCookFormula(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	fake := &fakeCook{out: cookTree(polecatWorkTree)}

	if err := formulaBDVia(fake.open).cook("mol-polecat-work", townRoot, townRoot); err != nil {
		t.Fatalf("CookFormula failed: %v", err)
	}
	cooks := fake.called("cook ")
	if len(cooks) != 1 || cooks[0].argv != "cook mol-polecat-work" {
		t.Fatalf("cooks = %+v, want one cook mol-polecat-work", cooks)
	}
	if want := (formulaSite{dir: townRoot, townRoot: townRoot}); cooks[0].site != want {
		t.Errorf("cook site = %+v, want %+v", cooks[0].site, want)
	}
}

// TestAutoApplyLogic verifies the auto-apply detection logic.
// When formulaName is empty and target contains "/polecats/", mol-polecat-work should be applied.
func TestAutoApplyLogic(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		formulaName   string
		hookRawBead   bool
		targetAgent   string
		wantAutoApply bool
	}{
		{
			name:          "bare bead to polecat - should auto-apply",
			formulaName:   "",
			hookRawBead:   false,
			targetAgent:   "gastown/polecats/Toast",
			wantAutoApply: true,
		},
		{
			name:          "bare bead with --hook-raw-bead - should not auto-apply",
			formulaName:   "",
			hookRawBead:   true,
			targetAgent:   "gastown/polecats/Toast",
			wantAutoApply: false,
		},
		{
			name:          "formula already specified - should not auto-apply",
			formulaName:   "mol-review",
			hookRawBead:   false,
			targetAgent:   "gastown/polecats/Toast",
			wantAutoApply: false,
		},
		{
			name:          "non-polecat target - should not auto-apply",
			formulaName:   "",
			hookRawBead:   false,
			targetAgent:   "gastown/witness",
			wantAutoApply: false,
		},
		{
			name:          "town singleton target - should not auto-apply",
			formulaName:   "",
			hookRawBead:   false,
			targetAgent:   "deacon/",
			wantAutoApply: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// This mirrors the logic in sling.go
			shouldAutoApply := tt.formulaName == "" && !tt.hookRawBead && strings.Contains(tt.targetAgent, "/polecats/")

			if shouldAutoApply != tt.wantAutoApply {
				t.Errorf("auto-apply logic: got %v, want %v", shouldAutoApply, tt.wantAutoApply)
			}
		})
	}
}

// TestFormulaOnBeadPassesVariables verifies that feature and issue variables are passed.
func TestFormulaOnBeadPassesVariables(t *testing.T) {
	t.Parallel()
	townRoot := formulaTown(t, `{"prefix":"gt-","path":"."}`)
	bd := formulaBDFake(`{"result_id":"gt-abc123","id_mapping":{"mol-polecat-work":"gt-wisp-var"}}`)

	if _, err := formulaBDVia(bd.open).instantiate("mol-polecat-work", "gt-abc123", "My Cool Feature", "", townRoot, nil); err != nil {
		t.Fatalf("InstantiateFormulaOnBead: %v", err)
	}

	bondLine := logLineWith(bd.log(), "mol bond")
	if bondLine == "" {
		t.Fatalf("mol bond command not found:\n%s", bd.log())
	}
	if !strings.Contains(bondLine, "feature=My Cool Feature") {
		t.Errorf("mol bond missing feature variable:\n%s", bondLine)
	}
	if !strings.Contains(bondLine, "issue=gt-abc123") {
		t.Errorf("mol bond missing issue variable:\n%s", bondLine)
	}
}

// TestInstantiateFormulaOnBead_DirectBondParsesIDMapping: the direct bond
// returns the base bead as result_id and the spawned molecule root under
// id_mapping keyed by the formula name; the root comes from the mapping, and
// every variable the formula declares a default for is passed to bd.
func TestInstantiateFormulaOnBead_DirectBondParsesIDMapping(t *testing.T) {
	t.Parallel()
	townRoot := formulaTown(t, `{"prefix":"gt-","path":"."}`)
	bd := formulaBDFake(`{"result_id":"gt-abc123","id_mapping":{"mol-polecat-work":"gt-mol-fallback"}}`)

	result, err := formulaBDVia(bd.open).instantiate("mol-polecat-work", "gt-abc123", "My Cool Feature", "", townRoot, nil)
	if err != nil {
		t.Fatalf("InstantiateFormulaOnBead: %v", err)
	}
	if result.WispRootID != "gt-mol-fallback" {
		t.Fatalf("WispRootID = %q, want %q", result.WispRootID, "gt-mol-fallback")
	}
	if result.BeadToHook != "gt-abc123" {
		t.Fatalf("BeadToHook = %q, want %q", result.BeadToHook, "gt-abc123")
	}

	log := bd.log()
	if strings.Contains(log, "mol wisp") {
		t.Fatalf("legacy mol wisp should not be called:\n%s", log)
	}
	directBondLine := logLineWith(log, "mol bond mol-polecat-work gt-abc123 --json --ephemeral")
	if directBondLine == "" {
		t.Fatalf("missing direct bond in log:\n%s", log)
	}
	if !containsVarArg(directBondLine, "feature", "My Cool Feature") {
		t.Fatalf("direct bond missing feature variable:\n%s", log)
	}
	if !containsVarArg(directBondLine, "issue", "gt-abc123") {
		t.Fatalf("direct bond missing issue variable:\n%s", log)
	}
	for _, required := range []struct {
		key   string
		value string
	}{
		{"base_branch", "main"},
		{"setup_command", ""},
		{"typecheck_command", ""},
		{"lint_command", ""},
		{"test_command", ""},
		{"build_command", ""},
	} {
		if !containsVarArg(directBondLine, required.key, required.value) {
			t.Fatalf("direct bond missing required variable %q:\n%s", required.key, log)
		}
	}
}

// TestBondFormulaDirectPinsTargetBeadsDir: the bond runs in the formula work
// dir but writes to the database the bead's prefix routes to, so a polecat
// worktree's own .beads cannot capture the wisp.
func TestBondFormulaDirectPinsTargetBeadsDir(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		beadID       string
		wantBeadsDir func(string) string
	}{
		{
			name:   "rig-prefixed bead",
			beadID: "gt-abc123",
			wantBeadsDir: func(townRoot string) string {
				return filepath.Join(townRoot, "gastown", "mayor", "rig", ".beads")
			},
		},
		{
			name:   "hq bead",
			beadID: "hq-abc123",
			wantBeadsDir: func(townRoot string) string {
				return filepath.Join(townRoot, ".beads")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			townRoot := formulaTown(t, `{"prefix":"gt-","path":"gastown/mayor/rig"}`, `{"prefix":"hq-","path":"."}`)
			rigBeadsDir := filepath.Join(townRoot, "gastown", "mayor", "rig", ".beads")
			formulaWorkDir := filepath.Join(townRoot, "polecats", "radrat", "gastown")
			for _, dir := range []string{rigBeadsDir, filepath.Join(formulaWorkDir, ".beads")} {
				if err := os.MkdirAll(dir, 0755); err != nil {
					t.Fatalf("mkdir %s: %v", dir, err)
				}
			}

			fake := formulaBDFake(`{"result_id":"` + tc.beadID + `","id_mapping":{"mol-polecat-work":"gt-mol-direct"}}`)

			bondVars, err := formulaBDVia(fake.open).varsForBead("mol-polecat-work", tc.beadID, "Test", formulaWorkDir, townRoot, nil)
			if err != nil {
				t.Fatalf("varsForBead: %v", err)
			}
			rootID, err := formulaBDVia(fake.open).bond("mol-polecat-work", "mol-polecat-work", tc.beadID, formulaWorkDir, townRoot, bondVars)
			if err != nil {
				t.Fatalf("bondFormulaDirect: %v", err)
			}
			if rootID != "gt-mol-direct" {
				t.Fatalf("rootID = %q, want gt-mol-direct", rootID)
			}
			bonds := fake.called("mol bond ")
			if len(bonds) != 1 {
				t.Fatalf("bonds = %+v, want one", bonds)
			}
			if want := (formulaSite{dir: formulaWorkDir, beadsDir: tc.wantBeadsDir(townRoot), townRoot: townRoot}); bonds[0].site != want {
				t.Fatalf("bond site = %+v, want %+v", bonds[0].site, want)
			}
		})
	}
}

// TestInstantiateFormulaOnBead_DirectBondHandlesNonGTIDs: the direct bond
// accepts the spawned root bd returns even when its prefix is not gt-.
func TestInstantiateFormulaOnBead_DirectBondHandlesNonGTIDs(t *testing.T) {
	t.Parallel()
	townRoot := formulaTown(t, `{"prefix":"oag-","path":"."}`)
	bd := formulaBDFake(`{"result_id":"oag-npeat","id_mapping":{"mol-polecat-work":"oag-wisp-wisp-rsia"}}`)

	result, err := formulaBDVia(bd.open).instantiate("mol-polecat-work", "oag-npeat", "Fix formula bug", "", townRoot, nil)
	if err != nil {
		t.Fatalf("InstantiateFormulaOnBead: %v", err)
	}
	if result.WispRootID != "oag-wisp-wisp-rsia" {
		t.Fatalf("WispRootID = %q, want %q", result.WispRootID, "oag-wisp-wisp-rsia")
	}
	if result.BeadToHook != "oag-npeat" {
		t.Fatalf("BeadToHook = %q, want %q", result.BeadToHook, "oag-npeat")
	}

	log := bd.log()
	if strings.Contains(log, "mol wisp") {
		t.Fatalf("legacy mol wisp should not have been called:\n%s", log)
	}
	if !strings.Contains(log, "mol bond mol-polecat-work oag-npeat --json --ephemeral") {
		t.Fatalf("direct bond should have been called with formula and bead:\n%s", log)
	}
}

// TestInstantiateFormulaOnBead_DirectBondCreatesNoOrphanCleanup: the direct
// path creates no intermediate wisp, so it has nothing to close afterwards.
func TestInstantiateFormulaOnBead_DirectBondCreatesNoOrphanCleanup(t *testing.T) {
	t.Parallel()
	townRoot := formulaTown(t, `{"prefix":"gt-","path":"."}`)
	bd := formulaBDFake(`{"result_id":"gt-test","id_mapping":{"mol-polecat-work":"gt-wisp-clean"}}`)

	result, err := formulaBDVia(bd.open).instantiate("mol-polecat-work", "gt-test", "Test cleanup", "", townRoot, nil)
	if err != nil {
		t.Fatalf("InstantiateFormulaOnBead: %v", err)
	}
	if result.WispRootID != "gt-wisp-clean" {
		t.Fatalf("WispRootID = %q, want %q", result.WispRootID, "gt-wisp-clean")
	}

	log := bd.log()
	if strings.Contains(log, "mol wisp") || strings.Contains(log, "close") {
		t.Fatalf("direct bond should not create or clean an orphaned wisp:\n%s", log)
	}
}

// TestInstantiateFormulaOnBead_DirectBondParseFailure: a bond that exits 0
// but prints no parsable root is an error, not a hook with no molecule.
func TestInstantiateFormulaOnBead_DirectBondParseFailure(t *testing.T) {
	t.Parallel()
	townRoot := formulaTown(t, `{"prefix":"gt-","path":"."}`)
	bd := formulaBDFake("NOT-JSON-GARBAGE")

	_, err := formulaBDVia(bd.open).instantiate("mol-polecat-work", "gt-abc123", "My Feature", "", townRoot, nil)
	if err == nil {
		t.Fatal("expected error when bond returns non-JSON, got nil")
	}
	if !strings.Contains(err.Error(), "missing spawned root id") {
		t.Fatalf("error message should mention missing spawned root id: %v", err)
	}
}

// A bead title reaches bd as one argv element, never through a shell: the
// metacharacters arrive byte-for-byte (gt-4k3fj.12).
func TestBondFormulaDirectPassesShellMetacharactersVerbatim(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	fake := formulaBDFake(`{"result_id":"gt-x","id_mapping":{"mol-polecat-work":"gt-mol-direct"}}`)

	title := `a; b (c) "d" $e $(touch pwned) ` + "`touch pwned`"
	vars, err := formulaBDVia(fake.open).varsForBead("mol-polecat-work", "gt-x", title, townRoot, townRoot, nil)
	if err != nil {
		t.Fatalf("varsForBead: %v", err)
	}
	rootID, err := formulaBDVia(fake.open).bond("mol-polecat-work", "mol-polecat-work", "gt-x", townRoot, townRoot, vars)
	if err != nil {
		t.Fatalf("bondFormulaDirect: %v", err)
	}
	if rootID != "gt-mol-direct" {
		t.Fatalf("rootID = %q, want gt-mol-direct", rootID)
	}
	want := "feature=" + title
	for _, bond := range fake.called("mol bond ") {
		if slices.Contains(bond.vars, want) {
			return
		}
	}
	t.Fatalf("bond lost the title as one var; want %q in %+v", want, fake.called("mol bond "))
}

// The bond error carries bd's own cause (the engine reads it from bd's --json
// failure payload), not just "exit status 1" (gt-4k3fj.12). A 1213 is retried
// first (gt-4ckuf), so this also pins the retry's cap: exhaustion reports the
// cause, having made exactly bdContentionAttempts attempts rather than looping.
func TestBondFormulaDirectErrorCarriesBdJSONCause(t *testing.T) {
	t.Parallel()
	fake := &fakeCook{bond: func(int) ([]byte, error) {
		return nil, errors.New("creating wisp: sql commit (regular): Error 1213 (40001): serialization failure")
	}}

	townRoot := t.TempDir()
	_, err := formulaBDVia(fake.open).bond("mol-polecat-work", "mol-polecat-work", "gt-x", townRoot, townRoot, []string{"feature=t"})
	if err == nil {
		t.Fatal("bondFormulaDirect succeeded, want failure")
	}
	if !strings.Contains(err.Error(), "Error 1213 (40001): serialization failure") {
		t.Fatalf("error hides bd's cause: %v", err)
	}
	if n := len(fake.called("mol bond ")); n != bdContentionAttempts {
		t.Fatalf("bond attempts = %d, want the cap %d", n, bdContentionAttempts)
	}
}
