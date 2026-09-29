package doctor

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/doltserver"
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
	tmpDir := t.TempDir()
	rigName := "testrig"
	rigDir := filepath.Join(tmpDir, rigName)
	if err := os.MkdirAll(rigDir, 0755); err != nil {
		t.Fatal(err)
	}
	writeTestrigRigsJSON(t, tmpDir)

	// Stale ambient targets the fix must replace, not inherit.
	t.Setenv("BEADS_DIR", filepath.Join(tmpDir, "wrong", ".beads"))
	t.Setenv("BEADS_DB", filepath.Join(tmpDir, "wrong.db"))
	t.Setenv("BEADS_DOLT_SERVER_DATABASE", "wrong_db")

	bd := newFakeBD()
	ctx := bd.ctx(tmpDir)
	ctx.RigName = rigName
	if err := NewBeadsRedirectCheck().Fix(ctx); err != nil {
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

// TestBeadsRedirectCheck_FixInitFallsBackToConfigYAML: when bd init fails
// (bd missing or erroring), the fix still leaves a minimal config.yaml
// carrying the rig's prefix.
func TestBeadsRedirectCheck_FixInitFallsBackToConfigYAML(t *testing.T) {
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
	if err := NewBeadsRedirectCheck().Fix(ctx); err != nil {
		t.Fatalf("Fix failed: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(rigDir, ".beads", "config.yaml"))
	if err != nil {
		t.Fatalf("fallback config.yaml: %v", err)
	}
	if !strings.Contains(string(data), "tr") {
		t.Errorf("fallback config.yaml lacks the prefix:\n%s", data)
	}
	if got, _ := bd.db(rigDir).ConfigGet("types.custom"); got != "" {
		t.Errorf("types configured after a failed init: %q", got)
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

	// Apply fix - runs bd init (the fallback without bd is
	// TestBeadsRedirectCheck_FixInitFallsBackToConfigYAML)
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
