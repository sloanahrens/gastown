package doctor

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/doltserver"
	"github.com/steveyegge/gastown/internal/git/gitfake"
)

// writeTestrigRigsJSON registers rig "testrig" with prefix "tr".
func writeTestrigRigsJSON(t *testing.T, townRoot string) {
	t.Helper()
	mayorDir := filepath.Join(townRoot, "mayor")
	if err := os.MkdirAll(mayorDir, 0755); err != nil {
		t.Fatal(err)
	}
	rigsJSON := `{
		"version": 1,
		"rigs": {
			"testrig": {
				"git_url": "https://example.com/test.git",
				"beads": {"prefix": "tr"}
			}
		}
	}`
	if err := os.WriteFile(filepath.Join(mayorDir, "rigs.json"), []byte(rigsJSON), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestBeadsRedirectCheck_FixInitBeadsUsesCanonicalDatabase(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	rigName := "testrig"
	rigDir := filepath.Join(tmpDir, rigName)
	if err := os.MkdirAll(rigDir, 0755); err != nil {
		t.Fatal(err)
	}
	writeTestrigRigsJSON(t, tmpDir)

	bd := newFakeBD()
	ctx := bd.ctx(tmpDir)
	ctx.RigName = rigName
	check := NewBeadsRedirectCheck()
	// Stale ambient targets the fix must replace, not inherit.
	check.environ = func() []string {
		return []string{
			"BEADS_DIR=" + filepath.Join(tmpDir, "wrong", ".beads"),
			"BEADS_DB=" + filepath.Join(tmpDir, "wrong.db"),
			"BEADS_DOLT_SERVER_DATABASE=wrong_db",
		}
	}
	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix failed: %v", err)
	}

	db := bd.db(rigDir)
	want := []beads.InitOptions{{Prefix: "tr", Database: "testrig", ServerPort: doltserver.DefaultConfig(tmpDir).Port}}
	if got := db.Inits(); !reflect.DeepEqual(got, want) {
		t.Fatalf("bd init = %+v, want %+v (the canonical rig database)", got, want)
	}
	opens := bd.opened()
	if len(opens) == 0 {
		t.Fatal("bd never opened")
	}
	for _, o := range opens {
		if o.dir != rigDir {
			t.Errorf("bd opened in %s, want %s", o.dir, rigDir)
		}
		if v, n := envLookup(o.env, "BEADS_DOLT_SERVER_DATABASE"); n != 1 || v != "testrig" {
			t.Errorf("BEADS_DOLT_SERVER_DATABASE = %q (x%d), want testrig once", v, n)
		}
		if v, n := envLookup(o.env, "BEADS_DIR"); n != 1 || v != filepath.Join(rigDir, ".beads") {
			t.Errorf("BEADS_DIR = %q (x%d), want the rig's .beads once", v, n)
		}
		if _, n := envLookup(o.env, "BEADS_DB"); n != 0 {
			t.Error("stale BEADS_DB leaked into bd")
		}
	}
	for key, want := range map[string]string{"types.custom": constants.BeadsCustomTypes, "types.infra": constants.BeadsInfraTypes} {
		if got, _ := db.ConfigGet(key); got != want {
			t.Errorf("%s = %q after init, want %q", key, got, want)
		}
	}
}

// TestBeadsRedirectFixInitFailureReturnsError: a failed bd init must surface
// as a Fix error and leave no .beads directory, since Run treats any .beads
// at the rig root as local beads and reports OK.
func TestBeadsRedirectFixInitFailureReturnsError(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	rigDir := filepath.Join(tmpDir, "testrig")
	if err := os.MkdirAll(rigDir, 0755); err != nil {
		t.Fatal(err)
	}
	writeTestrigRigsJSON(t, tmpDir)
	bd := newFakeBD()
	bd.db(rigDir).FailWith("init", errors.New("bd: command not found"))
	ctx := bd.ctx(tmpDir)
	ctx.RigName = "testrig"

	check := NewBeadsRedirectCheck()
	err := check.Fix(ctx)
	if err == nil {
		t.Fatal("Fix returned nil after bd init failed")
	}
	if !strings.Contains(err.Error(), "command not found") {
		t.Errorf("Fix error = %v, want the bd init failure", err)
	}
	if _, statErr := os.Stat(filepath.Join(rigDir, ".beads")); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf(".beads after a failed init: stat err = %v, want the directory removed", statErr)
	}
	if result := check.Run(ctx); result.Status == StatusOK {
		t.Errorf("Run after a failed init reported OK: %q", result.Message)
	}
}

func TestNewBeadsRedirectCheck(t *testing.T) {
	t.Parallel()
	check := NewBeadsRedirectCheck()

	if check.Name() != "beads-redirect" {
		t.Errorf("expected name 'beads-redirect', got %q", check.Name())
	}

	if !check.CanFix() {
		t.Error("expected CanFix to return true")
	}
}

func TestBeadsRedirectCheck_NoRigSpecified(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	check := NewBeadsRedirectCheck()
	ctx := &CheckContext{TownRoot: tmpDir, RigName: ""}

	result := check.Run(ctx)

	if result.Status != StatusOK {
		t.Errorf("expected StatusOK when no rig specified, got %v", result.Status)
	}
	if !strings.Contains(result.Message, "skipping") {
		t.Errorf("expected message about skipping, got %q", result.Message)
	}
}

func TestBeadsRedirectCheck_NoBeadsAtAll(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	rigName := "testrig"
	rigDir := filepath.Join(tmpDir, rigName)
	if err := os.MkdirAll(rigDir, 0755); err != nil {
		t.Fatal(err)
	}

	check := NewBeadsRedirectCheck()
	ctx := &CheckContext{TownRoot: tmpDir, RigName: rigName}

	result := check.Run(ctx)

	if result.Status != StatusError {
		t.Errorf("expected StatusError when no beads exist (fixable), got %v", result.Status)
	}
}

func TestBeadsRedirectCheck_LocalBeadsOnly(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	rigName := "testrig"
	rigDir := filepath.Join(tmpDir, rigName)

	// Create local beads at rig root (no mayor/rig/.beads)
	localBeads := filepath.Join(rigDir, ".beads")
	if err := os.MkdirAll(localBeads, 0755); err != nil {
		t.Fatal(err)
	}

	check := NewBeadsRedirectCheck()
	ctx := &CheckContext{TownRoot: tmpDir, RigName: rigName}

	result := check.Run(ctx)

	if result.Status != StatusOK {
		t.Errorf("expected StatusOK for local beads (no redirect needed), got %v", result.Status)
	}
	if !strings.Contains(result.Message, "local beads") {
		t.Errorf("expected message about local beads, got %q", result.Message)
	}
}

func TestBeadsRedirectCheck_TrackedBeadsMissingRedirect(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	rigName := "testrig"
	rigDir := filepath.Join(tmpDir, rigName)

	// Create tracked beads at mayor/rig/.beads
	trackedBeads := filepath.Join(rigDir, "mayor", "rig", ".beads")
	if err := os.MkdirAll(trackedBeads, 0755); err != nil {
		t.Fatal(err)
	}

	check := NewBeadsRedirectCheck()
	ctx := &CheckContext{TownRoot: tmpDir, RigName: rigName}

	result := check.Run(ctx)

	if result.Status != StatusError {
		t.Errorf("expected StatusError for missing redirect, got %v", result.Status)
	}
	if !strings.Contains(result.Message, "Missing") {
		t.Errorf("expected message about missing redirect, got %q", result.Message)
	}
}

func TestBeadsRedirectCheck_TrackedBeadsCorrectRedirect(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	rigName := "testrig"
	rigDir := filepath.Join(tmpDir, rigName)

	// Create tracked beads at mayor/rig/.beads
	trackedBeads := filepath.Join(rigDir, "mayor", "rig", ".beads")
	if err := os.MkdirAll(trackedBeads, 0755); err != nil {
		t.Fatal(err)
	}

	// Create rig-level .beads with correct redirect
	rigBeads := filepath.Join(rigDir, ".beads")
	if err := os.MkdirAll(rigBeads, 0755); err != nil {
		t.Fatal(err)
	}
	redirectPath := filepath.Join(rigBeads, "redirect")
	if err := os.WriteFile(redirectPath, []byte("mayor/rig/.beads\n"), 0644); err != nil {
		t.Fatal(err)
	}

	check := NewBeadsRedirectCheck()
	ctx := &CheckContext{TownRoot: tmpDir, RigName: rigName}

	result := check.Run(ctx)

	if result.Status != StatusOK {
		t.Errorf("expected StatusOK for correct redirect, got %v", result.Status)
	}
	if !strings.Contains(result.Message, "correctly configured") {
		t.Errorf("expected message about correct config, got %q", result.Message)
	}
}

func TestBeadsRedirectCheck_TrackedBeadsWrongRedirect(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	rigName := "testrig"
	rigDir := filepath.Join(tmpDir, rigName)

	// Create tracked beads at mayor/rig/.beads
	trackedBeads := filepath.Join(rigDir, "mayor", "rig", ".beads")
	if err := os.MkdirAll(trackedBeads, 0755); err != nil {
		t.Fatal(err)
	}

	// Create rig-level .beads with wrong redirect
	rigBeads := filepath.Join(rigDir, ".beads")
	if err := os.MkdirAll(rigBeads, 0755); err != nil {
		t.Fatal(err)
	}
	redirectPath := filepath.Join(rigBeads, "redirect")
	if err := os.WriteFile(redirectPath, []byte("wrong/path\n"), 0644); err != nil {
		t.Fatal(err)
	}

	check := NewBeadsRedirectCheck()
	ctx := &CheckContext{TownRoot: tmpDir, RigName: rigName}

	result := check.Run(ctx)

	if result.Status != StatusError {
		t.Errorf("expected StatusError for wrong redirect (fixable), got %v", result.Status)
	}
	if !strings.Contains(result.Message, "wrong/path") {
		t.Errorf("expected message to contain wrong path, got %q", result.Message)
	}
}

func TestBeadsRedirectCheck_FixWrongRedirect(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	rigName := "testrig"
	rigDir := filepath.Join(tmpDir, rigName)

	// Create tracked beads at mayor/rig/.beads
	trackedBeads := filepath.Join(rigDir, "mayor", "rig", ".beads")
	if err := os.MkdirAll(trackedBeads, 0755); err != nil {
		t.Fatal(err)
	}

	// Create rig-level .beads with wrong redirect
	rigBeads := filepath.Join(rigDir, ".beads")
	if err := os.MkdirAll(rigBeads, 0755); err != nil {
		t.Fatal(err)
	}
	redirectPath := filepath.Join(rigBeads, "redirect")
	if err := os.WriteFile(redirectPath, []byte("wrong/path\n"), 0644); err != nil {
		t.Fatal(err)
	}

	check := NewBeadsRedirectCheck()
	ctx := &CheckContext{TownRoot: tmpDir, RigName: rigName}

	// Verify fix is needed
	result := check.Run(ctx)
	if result.Status != StatusError {
		t.Fatalf("expected StatusError before fix, got %v", result.Status)
	}

	// Apply fix
	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix failed: %v", err)
	}

	// Verify redirect was corrected
	content, err := os.ReadFile(redirectPath)
	if err != nil {
		t.Fatalf("redirect file not found: %v", err)
	}
	if string(content) != "mayor/rig/.beads\n" {
		t.Errorf("redirect content = %q, want 'mayor/rig/.beads\\n'", string(content))
	}

	// Verify check now passes
	result = check.Run(ctx)
	if result.Status != StatusOK {
		t.Errorf("expected StatusOK after fix, got %v", result.Status)
	}
}

// TestBeadsRedirectFixKeepsMetadataOnWrongRedirect: Run flags only the wrong
// redirect target, so Fix must correct the redirect without deleting the
// metadata.json, config.yaml and other files the directory already holds.
func TestBeadsRedirectFixKeepsMetadataOnWrongRedirect(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	rigName := "testrig"
	rigDir := filepath.Join(tmpDir, rigName)

	trackedBeads := filepath.Join(rigDir, "mayor", "rig", ".beads")
	if err := os.MkdirAll(trackedBeads, 0755); err != nil {
		t.Fatal(err)
	}

	rigBeads := filepath.Join(rigDir, ".beads")
	if err := os.MkdirAll(rigBeads, 0755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"redirect":      "wrong/path\n",
		"metadata.json": `{"database":"testrig"}`,
		"config.yaml":   "prefix: tr\n",
	} {
		if err := os.WriteFile(filepath.Join(rigBeads, name), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}

	check := NewBeadsRedirectCheck()
	ctx := &CheckContext{TownRoot: tmpDir, RigName: rigName}
	if result := check.Run(ctx); result.Status != StatusError {
		t.Fatalf("wrong redirect before fix: %v %q, want StatusError", result.Status, result.Message)
	}

	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix failed: %v", err)
	}

	if _, err := os.Stat(filepath.Join(rigBeads, "metadata.json")); err != nil {
		t.Errorf("metadata.json after Fix: %v, want it kept", err)
	}
	content, err := os.ReadFile(filepath.Join(rigBeads, "redirect"))
	if err != nil {
		t.Fatalf("redirect file after Fix: %v", err)
	}
	if got := string(content); got != "mayor/rig/.beads\n" {
		t.Errorf("redirect content = %q, want 'mayor/rig/.beads\\n'", got)
	}
	if result := check.Run(ctx); result.Status != StatusOK {
		t.Errorf("after Fix: %v %q, want StatusOK", result.Status, result.Message)
	}
}

func TestBeadsRedirectCheck_Fix(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	rigName := "testrig"
	rigDir := filepath.Join(tmpDir, rigName)

	// Create tracked beads at mayor/rig/.beads
	trackedBeads := filepath.Join(rigDir, "mayor", "rig", ".beads")
	if err := os.MkdirAll(trackedBeads, 0755); err != nil {
		t.Fatal(err)
	}

	check := NewBeadsRedirectCheck()
	ctx := &CheckContext{TownRoot: tmpDir, RigName: rigName}

	// Verify fix is needed
	result := check.Run(ctx)
	if result.Status != StatusError {
		t.Fatalf("expected StatusError before fix, got %v", result.Status)
	}

	// Apply fix
	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix failed: %v", err)
	}

	// Verify redirect file was created
	redirectPath := filepath.Join(rigDir, ".beads", "redirect")
	content, err := os.ReadFile(redirectPath)
	if err != nil {
		t.Fatalf("redirect file not created: %v", err)
	}

	expected := "mayor/rig/.beads\n"
	if string(content) != expected {
		t.Errorf("redirect content = %q, want %q", string(content), expected)
	}

	// Verify check now passes
	result = check.Run(ctx)
	if result.Status != StatusOK {
		t.Errorf("expected StatusOK after fix, got %v", result.Status)
	}
}

func TestBeadsRedirectCheck_FixNoOp_LocalBeads(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	rigName := "testrig"
	rigDir := filepath.Join(tmpDir, rigName)

	// Create only local beads (no tracked beads)
	localBeads := filepath.Join(rigDir, ".beads")
	if err := os.MkdirAll(localBeads, 0755); err != nil {
		t.Fatal(err)
	}

	check := NewBeadsRedirectCheck()
	ctx := &CheckContext{TownRoot: tmpDir, RigName: rigName}

	// Fix should be a no-op
	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix failed: %v", err)
	}

	// Verify no redirect was created
	redirectPath := filepath.Join(rigDir, ".beads", "redirect")
	if _, err := os.Stat(redirectPath); !os.IsNotExist(err) {
		t.Error("redirect file should not be created for local beads")
	}
}

func TestBeadsRedirectCheck_FixInitBeads(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	rigName := "testrig"
	rigDir := filepath.Join(tmpDir, rigName)

	// Create rig directory (no beads at all)
	if err := os.MkdirAll(rigDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create mayor/rigs.json with prefix for the rig
	mayorDir := filepath.Join(tmpDir, "mayor")
	if err := os.MkdirAll(mayorDir, 0755); err != nil {
		t.Fatal(err)
	}
	rigsJSON := `{
		"version": 1,
		"rigs": {
			"testrig": {
				"git_url": "https://example.com/test.git",
				"beads": {
					"prefix": "tr"
				}
			}
		}
	}`
	if err := os.WriteFile(filepath.Join(mayorDir, "rigs.json"), []byte(rigsJSON), 0644); err != nil {
		t.Fatal(err)
	}

	check := NewBeadsRedirectCheck()
	bd := newFakeBD()
	ctx := bd.ctx(tmpDir)
	ctx.RigName = rigName

	// Verify fix is needed
	result := check.Run(ctx)
	if result.Status != StatusError {
		t.Fatalf("expected StatusError before fix, got %v", result.Status)
	}

	// Apply fix - runs bd init
	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix failed: %v", err)
	}

	// Verify .beads directory was created
	beadsDir := filepath.Join(rigDir, ".beads")
	if _, err := os.Stat(beadsDir); os.IsNotExist(err) {
		t.Fatal(".beads directory not created")
	}

	if inits := bd.db(rigDir).Inits(); len(inits) != 1 || inits[0].Prefix != "tr" {
		t.Fatalf("bd init = %+v, want one with prefix tr", inits)
	}

	// Verify check now passes (local beads exist)
	result = check.Run(ctx)
	if result.Status != StatusOK {
		t.Errorf("expected StatusOK after fix, got %v", result.Status)
	}
}

func TestBeadsRedirectCheck_ConflictingLocalBeads(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	rigName := "testrig"
	rigDir := filepath.Join(tmpDir, rigName)

	// Create tracked beads at mayor/rig/.beads
	trackedBeads := filepath.Join(rigDir, "mayor", "rig", ".beads")
	if err := os.MkdirAll(trackedBeads, 0755); err != nil {
		t.Fatal(err)
	}
	// Add some content to tracked beads
	if err := os.WriteFile(filepath.Join(trackedBeads, "issues.jsonl"), []byte(`{"id":"tr-1"}`), 0644); err != nil {
		t.Fatal(err)
	}

	// Create conflicting local beads with actual data
	localBeads := filepath.Join(rigDir, ".beads")
	if err := os.MkdirAll(localBeads, 0755); err != nil {
		t.Fatal(err)
	}
	// Add data to local beads (this is the conflict)
	if err := os.WriteFile(filepath.Join(localBeads, "issues.jsonl"), []byte(`{"id":"local-1"}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(localBeads, "config.yaml"), []byte("prefix: local\n"), 0644); err != nil {
		t.Fatal(err)
	}

	check := NewBeadsRedirectCheck()
	ctx := &CheckContext{TownRoot: tmpDir, RigName: rigName}

	// Check should detect conflicting beads
	result := check.Run(ctx)
	if result.Status != StatusError {
		t.Errorf("expected StatusError for conflicting beads, got %v", result.Status)
	}
	if !strings.Contains(result.Message, "Conflicting") {
		t.Errorf("expected message about conflicting beads, got %q", result.Message)
	}
}

func TestDefaultBranchExistsCheck_NoRig(t *testing.T) {
	t.Parallel()
	check := NewDefaultBranchExistsCheck()
	ctx := &CheckContext{TownRoot: t.TempDir(), RigName: ""}

	result := check.Run(ctx)
	if result.Status != StatusError {
		t.Errorf("expected StatusError with no rig, got %v", result.Status)
	}
}

func TestDefaultBranchExistsCheck_NoConfig(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	rigName := "testrig"
	rigDir := filepath.Join(tmpDir, rigName)
	if err := os.MkdirAll(rigDir, 0755); err != nil {
		t.Fatal(err)
	}

	check := NewDefaultBranchExistsCheck()
	ctx := &CheckContext{TownRoot: tmpDir, RigName: rigName}

	result := check.Run(ctx)
	if result.Status != StatusWarning {
		t.Errorf("expected StatusWarning with no config, got %v", result.Status)
	}
}

func TestDefaultBranchExistsCheck_EmptyDefaultBranch(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	rigName := "testrig"
	rigDir := filepath.Join(tmpDir, rigName)
	if err := os.MkdirAll(rigDir, 0755); err != nil {
		t.Fatal(err)
	}
	// Write config with no default_branch
	if err := os.WriteFile(filepath.Join(rigDir, "config.json"), []byte(`{"name":"testrig"}`), 0644); err != nil {
		t.Fatal(err)
	}

	check := NewDefaultBranchExistsCheck()
	ctx := &CheckContext{TownRoot: tmpDir, RigName: rigName}

	result := check.Run(ctx)
	if result.Status != StatusOK {
		t.Errorf("expected StatusOK with no default_branch, got %v", result.Status)
	}
}

func TestDefaultBranchExistsCheck_NoBareRepo(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	rigName := "testrig"
	rigDir := filepath.Join(tmpDir, rigName)
	if err := os.MkdirAll(rigDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, "config.json"), []byte(`{"default_branch":"main"}`), 0644); err != nil {
		t.Fatal(err)
	}

	check := NewDefaultBranchExistsCheck()
	ctx := &CheckContext{TownRoot: tmpDir, RigName: rigName}

	result := check.Run(ctx)
	if result.Status != StatusOK {
		t.Errorf("expected StatusOK when no bare repo, got %v", result.Status)
	}
}

func TestDefaultBranchExistsCheck_NotFixable(t *testing.T) {
	t.Parallel()
	check := NewDefaultBranchExistsCheck()
	if check.CanFix() {
		t.Error("DefaultBranchExistsCheck should not be fixable")
	}
}

// TestDefaultBranchAllRigsSkippedRigNotCounted: a rig with no .repo.git is
// skipped, so it must not appear in the "All N rig(s) validated" tally, and a
// town whose rigs were all skipped is not reported as validated.
func TestDefaultBranchAllRigsSkippedRigNotCounted(t *testing.T) {
	t.Parallel()

	writeRigConfig := func(t *testing.T, townRoot, rigName string) {
		t.Helper()
		rigPath := filepath.Join(townRoot, rigName)
		if err := os.MkdirAll(rigPath, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(rigPath, "config.json"), []byte(`{"default_branch":"dev"}`), 0644); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("skipped rig excluded from count", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()
		writeRigConfig(t, townRoot, "skippedrig")
		writeRigConfig(t, townRoot, "validrig")

		// validrig has a bare repo carrying the configured default_branch.
		gf := gitfake.New()
		bareRepo := filepath.Join(townRoot, "validrig", ".repo.git")
		gf.InitBare(t, bareRepo)
		id := gf.Commit(t, bareRepo, "dev", "init", nil)
		gf.SetRef(t, bareRepo, "refs/remotes/origin/dev", id)

		result := NewDefaultBranchAllRigsCheck().Run(withGit(&CheckContext{TownRoot: townRoot}, gf))

		if result.Status != StatusOK {
			t.Fatalf("status = %v %v, want StatusOK", result.Status, result.Details)
		}
		if !strings.Contains(result.Message, "All 1 rig(s)") {
			t.Errorf("message = %q, want it to count only the validated rig", result.Message)
		}
	})

	t.Run("skipped-only town is not validated", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()
		writeRigConfig(t, townRoot, "skippedrig")

		result := NewDefaultBranchAllRigsCheck().Run(&CheckContext{TownRoot: townRoot})

		// Every rig was skipped before its ref was checked, so the only honest
		// verdict is the no-op pass, never a validated tally.
		const want = "No rigs with custom default_branch configured"
		if result.Status != StatusOK || result.Message != want {
			t.Errorf("result = %v %q, want StatusOK %q", result.Status, result.Message, want)
		}
	})
}

func TestBeadsRedirectCheck_FixConflictingLocalBeads(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	rigName := "testrig"
	rigDir := filepath.Join(tmpDir, rigName)

	// Create tracked beads at mayor/rig/.beads with config.yaml as data marker
	trackedBeads := filepath.Join(rigDir, "mayor", "rig", ".beads")
	if err := os.MkdirAll(trackedBeads, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(trackedBeads, "config.yaml"), []byte("prefix: tr\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Create conflicting local beads with actual data
	localBeads := filepath.Join(rigDir, ".beads")
	if err := os.MkdirAll(localBeads, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(localBeads, "config.yaml"), []byte("prefix: local\n"), 0644); err != nil {
		t.Fatal(err)
	}

	check := NewBeadsRedirectCheck()
	ctx := &CheckContext{TownRoot: tmpDir, RigName: rigName}

	// Verify fix is needed
	result := check.Run(ctx)
	if result.Status != StatusError {
		t.Fatalf("expected StatusError before fix, got %v", result.Status)
	}

	// Apply fix - should remove conflicting local beads and create redirect
	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix failed: %v", err)
	}

	// Verify redirect was created
	redirectPath := filepath.Join(localBeads, "redirect")
	content, err := os.ReadFile(redirectPath)
	if err != nil {
		t.Fatalf("redirect file not created: %v", err)
	}
	if string(content) != "mayor/rig/.beads\n" {
		t.Errorf("redirect content = %q, want 'mayor/rig/.beads\\n'", string(content))
	}

	// Verify check now passes
	result = check.Run(ctx)
	if result.Status != StatusOK {
		t.Errorf("expected StatusOK after fix, got %v", result.Status)
	}
}

func TestMayorCloneExistsCheck_SharedBareRepoNeedsNoClone(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "myrig", ".repo.git"), 0755); err != nil {
		t.Fatal(err)
	}

	result := NewMayorCloneExistsCheck().Run(&CheckContext{TownRoot: townRoot, RigName: "myrig"})

	if result.Status != StatusOK {
		t.Errorf("rig whose repo is .repo.git with no mayor/rig clone: %v %v, want OK",
			result.Status, result.Details)
	}
}

func TestMayorCloneExistsCheck_NoRepoAtAllIsStillReported(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "myrig"), 0755); err != nil {
		t.Fatal(err)
	}

	result := NewMayorCloneExistsCheck().Run(&CheckContext{TownRoot: townRoot, RigName: "myrig"})

	if result.Status != StatusWarning {
		t.Errorf("rig with neither .repo.git nor mayor/rig: %v, want Warning", result.Status)
	}
}

func TestMayorCloneExistsCheck_BareRepoPathHoldingAFileIsNotARepo(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	rigPath := filepath.Join(townRoot, "myrig")
	if err := os.MkdirAll(rigPath, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigPath, ".repo.git"), []byte("not a repo\n"), 0644); err != nil {
		t.Fatal(err)
	}

	result := NewMayorCloneExistsCheck().Run(&CheckContext{TownRoot: townRoot, RigName: "myrig"})

	if result.Status != StatusWarning {
		t.Errorf("rig whose .repo.git is a file and has no mayor/rig clone: %v %v, want Warning",
			result.Status, result.Details)
	}
}

// hooksPathRig builds a rig named rigName holding one polecat clone at each
// of rels (relative to the rig dir), and returns the check context over it.
// A clone nested inside another must be listed before its parent: gitfake,
// like git init, leaves a path that already resolves to a repository alone.
func hooksPathRig(t *testing.T, rigName string, rels ...string) *CheckContext {
	t.Helper()
	townRoot := t.TempDir()
	gf := gitfake.New()
	for _, rel := range rels {
		clonePath := filepath.Join(townRoot, rigName, rel)
		gf.InitRepo(t, clonePath)
		if err := os.MkdirAll(filepath.Join(clonePath, ".githooks"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	return withGit(&CheckContext{TownRoot: townRoot, RigName: rigName}, gf)
}

// assertHooksPathConfigured warns for exactly the clone at rel, fixes it, and
// passes. The exact list is the point: a detail for any other path would mean
// the check resolved a different directory than the one on disk.
func assertHooksPathConfigured(t *testing.T, ctx *CheckContext, rel string) {
	t.Helper()
	check := NewHooksPathConfiguredCheck()

	result := check.Run(ctx)
	if result.Status != StatusWarning {
		t.Fatalf("clone at %s with hooksPath unset: %v %v, want Warning", rel, result.Status, result.Details)
	}
	if want := []string{rel}; !reflect.DeepEqual(result.Details, want) {
		t.Fatalf("details = %v, want exactly %v", result.Details, want)
	}

	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if got, _ := ctx.git(filepath.Join(ctx.RigPath(), rel)).ConfigGet("core.hooksPath"); got != ".githooks" {
		t.Fatalf("core.hooksPath after Fix = %q, want .githooks", got)
	}
	if result := check.Run(ctx); result.Status != StatusOK {
		t.Fatalf("after Fix: %v %v, want OK", result.Status, result.Details)
	}
}

// TestGitExcludeConfiguredWorktreeUsesCommonDir: when mayor/rig is a worktree,
// git reads info/exclude from the common dir, never from the per-worktree
// gitdir. Entries parked in the per-worktree file must not count as configured,
// and Fix must write the common dir's file.
func TestGitExcludeConfiguredWorktreeUsesCommonDir(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		gitdir func(rigPath string) string
	}{
		{"absolute", func(rigPath string) string {
			return filepath.Join(rigPath, ".repo.git", "worktrees", "mayorrig")
		}},
		{"relative", func(string) string {
			return filepath.Join("..", "..", ".repo.git", "worktrees", "mayorrig")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			townRoot := t.TempDir()
			rigPath := filepath.Join(townRoot, "testrig")
			mayorRig := filepath.Join(rigPath, "mayor", "rig")
			worktreeDir := filepath.Join(rigPath, ".repo.git", "worktrees", "mayorrig")
			for _, dir := range []string{mayorRig, worktreeDir, filepath.Join(worktreeDir, "info")} {
				if err := os.MkdirAll(dir, 0755); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(worktreeDir, "commondir"), []byte("../..\n"), 0644); err != nil {
				t.Fatal(err)
			}
			// git ignores the per-worktree exclude, so these entries have no effect.
			perWorktree := filepath.Join(worktreeDir, "info", "exclude")
			if err := os.WriteFile(perWorktree, []byte("/polecats/\n/mayor/\n"), 0644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(mayorRig, ".git"), []byte("gitdir: "+tc.gitdir(rigPath)+"\n"), 0644); err != nil {
				t.Fatal(err)
			}

			ctx := &CheckContext{TownRoot: townRoot, RigName: "testrig"}
			check := NewGitExcludeConfiguredCheck()
			if result := check.Run(ctx); result.Status != StatusWarning {
				t.Fatalf("entries only in the per-worktree exclude: %v %q, want StatusWarning", result.Status, result.Message)
			}

			if err := check.Fix(ctx); err != nil {
				t.Fatalf("Fix: %v", err)
			}
			data, err := os.ReadFile(filepath.Join(rigPath, ".repo.git", "info", "exclude"))
			if err != nil {
				t.Fatalf("common dir exclude after Fix: %v", err)
			}
			for _, want := range check.requiredExcludes() {
				if !strings.Contains(string(data), want) {
					t.Errorf("common dir exclude lacks %q after Fix:\n%s", want, data)
				}
			}
			if result := check.Run(ctx); result.Status != StatusOK {
				t.Errorf("after Fix: %v %q, want StatusOK", result.Status, result.Message)
			}
		})
	}
}

// TestEnsureOwnedDir: only a directory this call creates is reported as
// owned. Fix removes its own directory when bd init fails, and it must never
// remove one that already existed.
func TestEnsureOwnedDir(t *testing.T) {
	t.Parallel()

	t.Run("creates a missing directory", func(t *testing.T) {
		t.Parallel()
		dir := filepath.Join(t.TempDir(), "made")
		created, err := ensureOwnedDir(dir)
		if err != nil || !created {
			t.Fatalf("ensureOwnedDir(%s) = (%v, %v), want (true, nil)", dir, created, err)
		}
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			t.Fatalf("directory after ensureOwnedDir: %v, %v", info, err)
		}
	})

	t.Run("existing directory is not owned", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		marker := filepath.Join(dir, "metadata.json")
		if err := os.WriteFile(marker, []byte("{}"), 0644); err != nil {
			t.Fatal(err)
		}
		created, err := ensureOwnedDir(dir)
		if err != nil || created {
			t.Fatalf("ensureOwnedDir(existing) = (%v, %v), want (false, nil)", created, err)
		}
		if _, err := os.Stat(marker); err != nil {
			t.Errorf("pre-existing content after ensureOwnedDir: %v", err)
		}
	})

	t.Run("existing file is an error", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), ".beads")
		if err := os.WriteFile(path, []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
		if created, err := ensureOwnedDir(path); err == nil || created {
			t.Fatalf("ensureOwnedDir(file) = (%v, %v), want an error", created, err)
		}
	})
}

// TestGitExcludePathPlainGitDir: a repository whose .git is a directory is its
// own common dir.
func TestGitExcludePathPlainGitDir(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	got, err := gitExcludePath(repo)
	if err != nil {
		t.Fatalf("gitExcludePath: %v", err)
	}
	if want := filepath.Join(repo, ".git", "info", "exclude"); got != want {
		t.Errorf("path = %q, want %q", got, want)
	}
}

// TestGitExcludePathWorktreeWithoutCommonDir: a worktree gitdir without a
// commondir file is its own common dir.
func TestGitExcludePathWorktreeWithoutCommonDir(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	gitDir := filepath.Join(repo, ".repo.git", "worktrees", "mayorrig")
	if err := os.MkdirAll(gitDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".git"), []byte("gitdir: "+gitDir+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	got, err := gitExcludePath(repo)
	if err != nil {
		t.Fatalf("gitExcludePath: %v", err)
	}
	if want := filepath.Join(gitDir, "info", "exclude"); got != want {
		t.Errorf("path = %q, want %q", got, want)
	}
}

// TestGitExcludePathErrors: every failure branch returns the error that
// explains it, never a path git would not read.
func TestGitExcludePathErrors(t *testing.T) {
	t.Parallel()

	t.Run("missing .git", func(t *testing.T) {
		t.Parallel()
		if _, err := gitExcludePath(t.TempDir()); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("err = %v, want fs.ErrNotExist", err)
		}
	})

	t.Run("not a gitdir file", func(t *testing.T) {
		t.Parallel()
		repo := t.TempDir()
		if err := os.WriteFile(filepath.Join(repo, ".git"), []byte("this is not a gitdir pointer\n"), 0644); err != nil {
			t.Fatal(err)
		}
		if _, err := gitExcludePath(repo); err == nil || !strings.Contains(err.Error(), "not a gitdir file") {
			t.Fatalf("err = %v, want a not-a-gitdir-file error", err)
		}
	})

	t.Run("unreadable .git file", func(t *testing.T) {
		t.Parallel()
		if os.Geteuid() == 0 {
			t.Skip("root reads a mode-000 file")
		}
		repo := t.TempDir()
		if err := os.WriteFile(filepath.Join(repo, ".git"), []byte("gitdir: /nowhere\n"), 0000); err != nil {
			t.Fatal(err)
		}
		if _, err := gitExcludePath(repo); err == nil || !strings.Contains(err.Error(), "cannot read .git file") {
			t.Fatalf("err = %v, want a cannot-read error", err)
		}
	})
}

// TestGitExcludeConfiguredCheckRunRejectsBadGitFile: an unreadable .git
// pointer is an error verdict, not an exclude that "has no entries".
func TestGitExcludeConfiguredCheckRunRejectsBadGitFile(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	mayorRig := filepath.Join(townRoot, "testrig", "mayor", "rig")
	if err := os.MkdirAll(mayorRig, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mayorRig, ".git"), []byte("garbage\n"), 0644); err != nil {
		t.Fatal(err)
	}

	result := NewGitExcludeConfiguredCheck().Run(&CheckContext{TownRoot: townRoot, RigName: "testrig"})
	if result.Status != StatusError {
		t.Fatalf("status = %v %q, want StatusError", result.Status, result.Message)
	}
	if !strings.Contains(result.Message, "Cannot resolve mayor/rig git dir") {
		t.Errorf("message = %q, want the resolution failure", result.Message)
	}
}

func TestHooksPathConfiguredNestedPolecatWorktree(t *testing.T) {
	t.Parallel()
	const rigName = "testrig"
	nested := filepath.Join("polecats", "p1", rigName)
	// Polecat worktrees are nested under polecats/<name>/<rigname>/; the
	// parent polecats/<name>/ is not itself a git clone.
	assertHooksPathConfigured(t, hooksPathRig(t, rigName, nested), nested)
}

func TestHooksPathConfiguredLegacyFlatPolecatWorktree(t *testing.T) {
	t.Parallel()
	const rigName = "testrig"
	flat := filepath.Join("polecats", "p1")
	// The flat polecats/<name>/ layout predates the per-rig nesting.
	assertHooksPathConfigured(t, hooksPathRig(t, rigName, flat), flat)
}

// A polecat directory can hold both layouts while only the nested path is a
// clone; the check has to configure that one and leave its parent alone.
func TestHooksPathConfiguredNestedPreferredOverFlatClone(t *testing.T) {
	t.Parallel()
	const rigName = "testrig"
	nested := filepath.Join("polecats", "p1", rigName)
	flat := filepath.Join("polecats", "p1")
	ctx := hooksPathRig(t, rigName, nested, flat)

	assertHooksPathConfigured(t, ctx, nested)

	if got, _ := ctx.git(filepath.Join(ctx.RigPath(), flat)).ConfigGet("core.hooksPath"); got != "" {
		t.Fatalf("flat clone core.hooksPath = %q, want untouched", got)
	}
}

// A nested directory that is not a clone is not the polecat's worktree: the
// flat clone at polecats/<name> is, and it is the one the check resolves.
func TestHooksPathConfiguredNestedDirectoryWithoutGitFallsBackToFlat(t *testing.T) {
	t.Parallel()
	const rigName = "testrig"
	flat := filepath.Join("polecats", "p1")
	ctx := hooksPathRig(t, rigName, flat)
	if err := os.MkdirAll(filepath.Join(ctx.RigPath(), flat, rigName), 0755); err != nil {
		t.Fatal(err)
	}

	assertHooksPathConfigured(t, ctx, flat)
}

// A stat that fails for a reason other than not-exist (ENOTDIR here) must not
// be read as "the nested worktree is there": the flat clone is the candidate.
func TestHooksPathConfiguredNestedPathStatErrorFallsBackToFlat(t *testing.T) {
	t.Parallel()
	const rigName = "testrig"
	flat := filepath.Join("polecats", "p1")
	ctx := hooksPathRig(t, rigName, flat)
	if err := os.WriteFile(filepath.Join(ctx.RigPath(), flat, rigName), []byte("not a directory\n"), 0644); err != nil {
		t.Fatal(err)
	}

	assertHooksPathConfigured(t, ctx, flat)
}

// The gt-ux2hv acceptance scenario: a polecat worktree whose
// .repo.git/worktrees/<name> directory was removed is dead, so Fix has to
// re-register it and the Run after Fix has to call it healthy again. git's own
// agreement that the repaired worktree is usable needs real git, which unit
// tests in this package may not run; the integration tier proves that half
// (gt-9d3zt).
func TestBareRepoExistsFixReregistersUsableWorktree(t *testing.T) {
	t.Parallel()
	gf := gitfake.New()
	tmpDir := t.TempDir()
	rigName := "testrig"
	rigDir := filepath.Join(tmpDir, rigName)

	bareRepo := initBareRepoWithRemote(t, gf, rigDir, "https://github.com/example/repo.git")
	setupWorktreeRef(t, rigDir, bareRepo)
	wtMetaDir := filepath.Join(bareRepo, "worktrees", "rig")
	if err := os.RemoveAll(wtMetaDir); err != nil {
		t.Fatal(err)
	}

	check := NewBareRepoExistsCheck()
	ctx := withGit(&CheckContext{TownRoot: tmpDir, RigName: rigName}, gf)
	if result := check.Run(ctx); result.Status != StatusError {
		t.Fatalf("expected StatusError with the registration removed, got %v: %s", result.Status, result.Message)
	}

	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix failed: %v", err)
	}
	if result := NewBareRepoExistsCheck().Run(ctx); result.Status != StatusOK {
		t.Fatalf("expected StatusOK after Fix, got %v: %s", result.Status, result.Message)
	}
	if missing := worktreeMetaMissing(wtMetaDir); len(missing) > 0 {
		t.Errorf("Fix left the registration incomplete: missing %v", missing)
	}
}
