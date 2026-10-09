package rig

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/doltserver"
	"github.com/steveyegge/gastown/internal/townconfig"
)

func setupTestTown(t *testing.T) (string, *config.RigsConfig) {
	t.Helper()
	root := t.TempDir()

	rigsConfig := &config.RigsConfig{
		Version: 1,
		Rigs:    make(map[string]config.RigEntry),
	}

	return root, rigsConfig
}

func createTestRig(t *testing.T, root, name string) {
	t.Helper()

	rigPath := filepath.Join(root, name)
	if err := os.MkdirAll(rigPath, 0755); err != nil {
		t.Fatalf("mkdir rig: %v", err)
	}

	// Create agent dirs (witness, refinery, mayor)
	for _, dir := range AgentDirs {
		dirPath := filepath.Join(rigPath, dir)
		if err := os.MkdirAll(dirPath, 0755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}

	// Create some polecats
	polecatsDir := filepath.Join(rigPath, "polecats")
	for _, polecat := range []string{"Toast", "Cheedo"} {
		if err := os.MkdirAll(filepath.Join(polecatsDir, polecat), 0755); err != nil {
			t.Fatalf("mkdir polecat: %v", err)
		}
	}
	// Create a shared support dir that should not be treated as a polecat worktree.
	if err := os.MkdirAll(filepath.Join(polecatsDir, ".claude"), 0755); err != nil {
		t.Fatalf("mkdir polecats/.claude: %v", err)
	}
}

func TestDiscoverRigs(t *testing.T) {
	t.Parallel()
	root, rigsConfig := setupTestTown(t)

	// Create test rig
	createTestRig(t, root, "gastown")
	rigsConfig.Rigs["gastown"] = config.RigEntry{
		GitURL: "git@github.com:test/gastown.git",
	}

	manager := newTestManager(root, rigsConfig)

	rigs, err := manager.DiscoverRigs()
	if err != nil {
		t.Fatalf("DiscoverRigs: %v", err)
	}

	if len(rigs) != 1 {
		t.Errorf("rigs count = %d, want 1", len(rigs))
	}

	rig := rigs[0]
	if rig.Name != "gastown" {
		t.Errorf("Name = %q, want gastown", rig.Name)
	}
	if len(rig.Polecats) != 2 {
		t.Errorf("Polecats count = %d, want 2", len(rig.Polecats))
	}
	if slices.Contains(rig.Polecats, ".claude") {
		t.Errorf("expected polecats/.claude to be ignored, got %v", rig.Polecats)
	}
}

func TestDiscoverRigs_SortedByName(t *testing.T) {
	t.Parallel()
	root, rigsConfig := setupTestTown(t)

	// Register rigs in deliberately non-alphabetical order.
	// Go map iteration is randomized, so without sorting the output
	// order would be nondeterministic across runs.
	names := []string{"zebra", "alpha", "middle", "beta"}
	for _, name := range names {
		createTestRig(t, root, name)
		rigsConfig.Rigs[name] = config.RigEntry{
			GitURL: "git@github.com:test/" + name + ".git",
		}
	}

	manager := newTestManager(root, rigsConfig)

	// Run multiple iterations to catch nondeterminism — a single pass
	// could accidentally return sorted order from a random map.
	for i := 0; i < 10; i++ {
		rigs, err := manager.DiscoverRigs()
		if err != nil {
			t.Fatalf("DiscoverRigs (iter %d): %v", i, err)
		}

		if len(rigs) != len(names) {
			t.Fatalf("iter %d: rigs count = %d, want %d", i, len(rigs), len(names))
		}

		want := []string{"alpha", "beta", "middle", "zebra"}
		for j, rig := range rigs {
			if rig.Name != want[j] {
				t.Errorf("iter %d: rigs[%d].Name = %q, want %q", i, j, rig.Name, want[j])
			}
		}
	}
}

func TestGetRig(t *testing.T) {
	t.Parallel()
	root, rigsConfig := setupTestTown(t)

	createTestRig(t, root, "test-rig")
	rigsConfig.Rigs["test-rig"] = config.RigEntry{
		GitURL: "git@github.com:test/test-rig.git",
	}

	manager := newTestManager(root, rigsConfig)

	rig, err := manager.GetRig("test-rig")
	if err != nil {
		t.Fatalf("GetRig: %v", err)
	}

	if rig.Name != "test-rig" {
		t.Errorf("Name = %q, want test-rig", rig.Name)
	}
}

func TestGetRigNotFound(t *testing.T) {
	t.Parallel()
	root, rigsConfig := setupTestTown(t)
	manager := newTestManager(root, rigsConfig)

	_, err := manager.GetRig("nonexistent")
	if err != ErrRigNotFound {
		t.Errorf("GetRig = %v, want ErrRigNotFound", err)
	}
}

func TestRigExists(t *testing.T) {
	t.Parallel()
	root, rigsConfig := setupTestTown(t)
	rigsConfig.Rigs["exists"] = config.RigEntry{}

	manager := newTestManager(root, rigsConfig)

	if !manager.RigExists("exists") {
		t.Error("expected RigExists = true for existing rig")
	}
	if manager.RigExists("nonexistent") {
		t.Error("expected RigExists = false for nonexistent rig")
	}
}

func TestRemoveRig(t *testing.T) {
	t.Parallel()
	root, rigsConfig := setupTestTown(t)
	rigsConfig.Rigs["to-remove"] = config.RigEntry{}

	manager := newTestManager(root, rigsConfig)

	if err := manager.RemoveRig("to-remove"); err != nil {
		t.Fatalf("RemoveRig: %v", err)
	}

	if manager.RigExists("to-remove") {
		t.Error("rig should not exist after removal")
	}
}

func TestRemoveRigNotFound(t *testing.T) {
	t.Parallel()
	root, rigsConfig := setupTestTown(t)
	manager := newTestManager(root, rigsConfig)

	err := manager.RemoveRig("nonexistent")
	if err != ErrRigNotFound {
		t.Errorf("RemoveRig = %v, want ErrRigNotFound", err)
	}
}

func TestRemoveRigNotFoundWithOrphanDir(t *testing.T) {
	t.Parallel()
	root, rigsConfig := setupTestTown(t)
	manager := newTestManager(root, rigsConfig)

	// Create an orphaned directory on disk without registering it in config
	orphanDir := filepath.Join(root, "orphan-rig")
	if err := os.MkdirAll(orphanDir, 0o755); err != nil {
		t.Fatalf("creating orphan dir: %v", err)
	}

	// Manager should still return ErrRigNotFound (UX is handled in cmd layer)
	err := manager.RemoveRig("orphan-rig")
	if err != ErrRigNotFound {
		t.Errorf("RemoveRig orphan dir = %v, want ErrRigNotFound", err)
	}
}

func TestUsedNamepoolThemes(t *testing.T) {
	t.Parallel()
	root, rigsConfig := setupTestTown(t)

	// Register two rigs
	rigsConfig.Rigs["alpha"] = config.RigEntry{}
	rigsConfig.Rigs["beta"] = config.RigEntry{}

	// Create settings for alpha with explicit theme
	alphaSettings := filepath.Join(root, "alpha", "settings")
	if err := os.MkdirAll(alphaSettings, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(alphaSettings, "config.json"), []byte(`{"type":"rig-settings","version":1,"namepool":{"style":"minerals"}}`), 0644); err != nil {
		t.Fatal(err)
	}

	// beta has no settings — should use fallback
	manager := newTestManager(root, rigsConfig)
	fallback := func(name string) string { return "fallback-" + name }
	themes := manager.UsedNamepoolThemes(fallback)

	if len(themes) != 2 {
		t.Fatalf("expected 2 themes, got %d: %v", len(themes), themes)
	}

	hasAlpha := slices.Contains(themes, "minerals")
	hasBeta := slices.Contains(themes, "fallback-beta")
	if !hasAlpha {
		t.Errorf("expected 'minerals' for alpha, themes: %v", themes)
	}
	if !hasBeta {
		t.Errorf("expected 'fallback-beta' for beta, themes: %v", themes)
	}
}

func TestAddRig_RejectsInvalidNames(t *testing.T) {
	t.Parallel()
	root, rigsConfig := setupTestTown(t)
	manager := newTestManager(root, rigsConfig)

	tests := []struct {
		name      string
		wantError string
	}{
		{"op-baby", `rig name "op-baby" contains invalid characters`},
		{"my.rig", `rig name "my.rig" contains invalid characters`},
		{"my rig", `rig name "my rig" contains invalid characters`},
		{"op-baby-test", `rig name "op-baby-test" contains invalid characters`},
		{"hq", `rig name "hq" is reserved for town-level infrastructure`},
		{"HQ", `rig name "HQ" is reserved for town-level infrastructure`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := manager.AddRig(AddRigOptions{
				Name:   tt.name,
				GitURL: "git@github.com:test/test.git",
			})
			if err == nil {
				t.Errorf("AddRig(%q) succeeded, want error containing %q", tt.name, tt.wantError)
				return
			}
			if !strings.Contains(err.Error(), tt.wantError) {
				t.Errorf("AddRig(%q) error = %q, want error containing %q", tt.name, err.Error(), tt.wantError)
			}
		})
	}
}

func TestAddRig_EmptyRepositoryReturnsFriendlyError(t *testing.T) {
	t.Parallel()
	root, rigsConfig := setupTestTown(t)
	manager, f, _ := testManager(root, rigsConfig)
	remoteDir := filepath.Join(t.TempDir(), "empty-remote")
	f.InitBare(t, remoteDir)

	_, err := manager.AddRig(AddRigOptions{
		Name:          "emptyrepo",
		GitURL:        remoteDir,
		BeadsPrefix:   "er",
		SkipDoltCheck: true,
	})
	if err == nil {
		t.Fatal("AddRig succeeded, want empty repository error")
	}
	want := fmt.Sprintf("repository %s is empty (no commits). Push at least one commit before adding it as a rig", remoteDir)
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("AddRig error = %q, want containing %q", err.Error(), want)
	}
	if _, statErr := os.Stat(filepath.Join(root, "emptyrepo")); !os.IsNotExist(statErr) {
		t.Fatalf("expected failed rig directory to be removed, stat err = %v", statErr)
	}
}

// Naming a branch makes the clone itself fail on an empty repository; the
// error must still be the empty-repository one, not git's missing branch.
func TestAddRig_EmptyRepositoryWithBranchReturnsFriendlyError(t *testing.T) {
	t.Parallel()
	root, rigsConfig := setupTestTown(t)
	manager, f, _ := testManager(root, rigsConfig)
	remoteDir := filepath.Join(t.TempDir(), "empty-remote")
	f.InitBare(t, remoteDir)

	_, err := manager.AddRig(AddRigOptions{
		Name:          "emptybranchrepo",
		GitURL:        remoteDir,
		BeadsPrefix:   "ebr",
		DefaultBranch: "main",
		SkipDoltCheck: true,
	})
	if err == nil {
		t.Fatal("AddRig succeeded, want empty repository error")
	}
	want := fmt.Sprintf("repository %s is empty (no commits). Push at least one commit before adding it as a rig", remoteDir)
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("AddRig error = %q, want containing %q", err.Error(), want)
	}
	if strings.Contains(err.Error(), "Remote branch main not found") {
		t.Fatalf("AddRig surfaced low-level clone error: %q", err.Error())
	}
}

func TestListRigNames(t *testing.T) {
	t.Parallel()
	root, rigsConfig := setupTestTown(t)
	rigsConfig.Rigs["rig1"] = config.RigEntry{}
	rigsConfig.Rigs["rig2"] = config.RigEntry{}

	manager := newTestManager(root, rigsConfig)

	names := manager.ListRigNames()
	if len(names) != 2 {
		t.Errorf("names count = %d, want 2", len(names))
	}
}

func TestRigSummary(t *testing.T) {
	t.Parallel()
	rig := &Rig{
		Name:     "test",
		Polecats: []string{"a", "b", "c"},
	}

	summary := rig.Summary()

	if summary.Name != "test" {
		t.Errorf("Name = %q, want test", summary.Name)
	}
	if summary.PolecatCount != 3 {
		t.Errorf("PolecatCount = %d, want 3", summary.PolecatCount)
	}
}

func TestEnsureGitignoreEntry_AddsEntry(t *testing.T) {
	t.Parallel()
	root, rigsConfig := setupTestTown(t)
	manager := newTestManager(root, rigsConfig)

	gitignorePath := filepath.Join(root, ".gitignore")

	if err := manager.ensureGitignoreEntry(gitignorePath, ".test-entry/"); err != nil {
		t.Fatalf("ensureGitignoreEntry: %v", err)
	}

	content, _ := os.ReadFile(gitignorePath)
	if string(content) != ".test-entry/\n" {
		t.Errorf("content = %q, want .test-entry/", string(content))
	}
}

func TestEnsureGitignoreEntry_DoesNotDuplicate(t *testing.T) {
	t.Parallel()
	root, rigsConfig := setupTestTown(t)
	manager := newTestManager(root, rigsConfig)

	gitignorePath := filepath.Join(root, ".gitignore")

	// Pre-populate with the entry
	if err := os.WriteFile(gitignorePath, []byte(".test-entry/\n"), 0644); err != nil {
		t.Fatalf("writing .gitignore: %v", err)
	}

	if err := manager.ensureGitignoreEntry(gitignorePath, ".test-entry/"); err != nil {
		t.Fatalf("ensureGitignoreEntry: %v", err)
	}

	content, _ := os.ReadFile(gitignorePath)
	if string(content) != ".test-entry/\n" {
		t.Errorf("content = %q, want single .test-entry/", string(content))
	}
}

func TestEnsureGitignoreEntry_AppendsToExisting(t *testing.T) {
	t.Parallel()
	root, rigsConfig := setupTestTown(t)
	manager := newTestManager(root, rigsConfig)

	gitignorePath := filepath.Join(root, ".gitignore")

	// Pre-populate with existing entries
	if err := os.WriteFile(gitignorePath, []byte("node_modules/\n*.log\n"), 0644); err != nil {
		t.Fatalf("writing .gitignore: %v", err)
	}

	if err := manager.ensureGitignoreEntry(gitignorePath, ".test-entry/"); err != nil {
		t.Fatalf("ensureGitignoreEntry: %v", err)
	}

	content, _ := os.ReadFile(gitignorePath)
	expected := "node_modules/\n*.log\n.test-entry/\n"
	if string(content) != expected {
		t.Errorf("content = %q, want %q", string(content), expected)
	}
}

func TestInitBeads_TrackedBeads_CreatesRedirect(t *testing.T) {
	t.Parallel()
	// When the cloned repo has tracked beads (mayor/rig/.beads exists),
	// initBeads should create a redirect file at <rig>/.beads/redirect
	// pointing to mayor/rig/.beads instead of creating a local database.
	rigPath := t.TempDir()

	// Simulate tracked beads in the cloned repo (metadata.json is the marker)
	mayorBeadsDir := filepath.Join(rigPath, "mayor", "rig", ".beads")
	if err := os.MkdirAll(mayorBeadsDir, 0755); err != nil {
		t.Fatalf("mkdir mayor beads: %v", err)
	}
	// Create metadata.json to simulate a real beads database
	if err := os.WriteFile(filepath.Join(mayorBeadsDir, "metadata.json"), []byte("{}\n"), 0644); err != nil {
		t.Fatalf("write mayor metadata.json: %v", err)
	}

	manager := &Manager{}
	if err := manager.InitBeads(rigPath, "gt", ""); err != nil {
		t.Fatalf("initBeads: %v", err)
	}

	// Verify redirect file was created
	redirectPath := filepath.Join(rigPath, ".beads", "redirect")
	content, err := os.ReadFile(redirectPath)
	if err != nil {
		t.Fatalf("reading redirect file: %v", err)
	}

	expected := "mayor/rig/.beads\n"
	if string(content) != expected {
		t.Errorf("redirect content = %q, want %q", string(content), expected)
	}

	// Verify no local database was created (no config.yaml at rig level)
	rigConfigPath := filepath.Join(rigPath, ".beads", "config.yaml")
	if _, err := os.Stat(rigConfigPath); !os.IsNotExist(err) {
		t.Errorf("expected no config.yaml at rig level when using redirect, but it exists")
	}
}

func TestInitBeads_LocalBeads_CreatesDatabase(t *testing.T) {
	t.Parallel()
	// When the cloned repo does NOT have tracked beads (no mayor/rig/.beads),
	// initBeads should create a local database at <rig>/.beads/
	rigPath := t.TempDir()

	// Create mayor/rig directory but WITHOUT .beads (no tracked beads)
	mayorRigDir := filepath.Join(rigPath, "mayor", "rig")
	if err := os.MkdirAll(mayorRigDir, 0755); err != nil {
		t.Fatalf("mkdir mayor/rig: %v", err)
	}

	manager, _, bd := testManager("", nil)
	if err := manager.InitBeads(rigPath, "gt", ""); err != nil {
		t.Fatalf("initBeads: %v", err)
	}

	// Verify NO redirect file was created
	redirectPath := filepath.Join(rigPath, ".beads", "redirect")
	if _, err := os.Stat(redirectPath); !os.IsNotExist(err) {
		t.Errorf("expected no redirect file for local beads, but it exists")
	}
	if inits := bd.withVerb("init"); len(inits) != 1 || inits[0].Dir != rigPath {
		t.Errorf("bd init calls = %+v, want one in %s", inits, rigPath)
	}
}

// A failed bd init still leaves the rig a config.yaml and server metadata, and
// bd init itself ran pinned to the rig's .beads.
func TestInitBeadsWritesConfigOnFailure(t *testing.T) {
	t.Parallel()
	rigPath := t.TempDir()
	beadsDir := filepath.Join(rigPath, ".beads")

	manager, _, bd := testManager("", nil)
	bd.fail = func(c bdCall) error {
		if c.Verb == "init" {
			return errors.New("bd init failed")
		}
		return nil
	}
	if err := manager.InitBeads(rigPath, "gt", "testrig"); err != nil {
		t.Fatalf("initBeads: %v", err)
	}

	configPath := filepath.Join(beadsDir, "config.yaml")
	config, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("reading config.yaml: %v", err)
	}
	want := "prefix: gt\nissue-prefix: gt\ndolt.idle-timeout: \"0\"\nexport.auto: \"false\"\n"
	if string(config) != want {
		t.Fatalf("config.yaml = %q, want %q", string(config), want)
	}

	metadataPath := filepath.Join(beadsDir, "metadata.json")
	metadataBytes, err := os.ReadFile(metadataPath)
	if err != nil {
		t.Fatalf("reading metadata.json: %v", err)
	}
	var metadata map[string]interface{}
	if err := json.Unmarshal(metadataBytes, &metadata); err != nil {
		t.Fatalf("parsing metadata.json: %v", err)
	}
	if metadata["dolt_mode"] != "server" {
		t.Fatalf("dolt_mode = %v, want server", metadata["dolt_mode"])
	}
	if metadata["dolt_database"] != "testrig" {
		t.Fatalf("dolt_database = %v, want testrig", metadata["dolt_database"])
	}
	inits := bd.withVerb("init")
	if len(inits) != 1 {
		t.Fatalf("bd init calls = %d, want 1", len(inits))
	}
	if got, _ := envValue(inits[0].Env, "BEADS_DIR"); got != beadsDir {
		t.Fatalf("bd init BEADS_DIR = %q, want %q", got, beadsDir)
	}
	if calls := bd.withVerb("config"); len(calls) != 0 {
		t.Errorf("bd config ran after a failed init: %v", bd.argvs())
	}
}

// When bd init succeeds, InitBeads sets issue_prefix explicitly:
// bd init --prefix may not persist it.
func TestInitBeadsSetsIssuePrefix(t *testing.T) {
	t.Parallel()
	rigPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rigPath, "mayor", "rig"), 0755); err != nil {
		t.Fatalf("mkdir mayor/rig: %v", err)
	}

	manager, _, bd := testManager("", nil)
	if err := manager.InitBeads(rigPath, "myrig", ""); err != nil {
		t.Fatalf("initBeads: %v", err)
	}
	if !slices.Contains(bd.argvs(), "config set issue_prefix myrig") {
		t.Errorf("expected 'bd config set issue_prefix myrig', got:\n%s", strings.Join(bd.argvs(), "\n"))
	}
}

func TestInitBeadsPassesCanonicalDatabase(t *testing.T) {
	t.Parallel()
	rigPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rigPath, "mayor", "rig"), 0755); err != nil {
		t.Fatalf("mkdir mayor/rig: %v", err)
	}

	manager, _, bd := testManager("", nil)
	// Stale selectors in the caller's environment must not reach bd.
	manager.env = []string{
		"BEADS_DIR=" + filepath.Join(rigPath, "wrong", ".beads"),
		"BEADS_DB=" + filepath.Join(rigPath, "wrong.db"),
		"BEADS_DOLT_SERVER_DATABASE=stale_prefix_db",
	}
	if err := manager.InitBeads(rigPath, "xx", "my_project"); err != nil {
		t.Fatalf("InitBeads: %v", err)
	}

	calls := bd.recorded()
	if len(calls) == 0 || calls[0].Verb != "init" || calls[0].Init.Prefix != "xx" || calls[0].Init.Database != "my_project" {
		t.Fatalf("bd init did not use canonical database; calls:\n%s", strings.Join(bd.argvs(), "\n"))
	}
	for _, c := range calls {
		if db, _ := envValue(c.Env, "BEADS_DOLT_SERVER_DATABASE"); db != "my_project" {
			t.Errorf("bd %s: BEADS_DOLT_SERVER_DATABASE = %q, want my_project", c.argv(), db)
		}
		if dir, _ := envValue(c.Env, "BEADS_DIR"); dir != filepath.Join(rigPath, ".beads") {
			t.Errorf("bd %s: BEADS_DIR = %q, want the rig's .beads", c.argv(), dir)
		}
		if db, ok := envValue(c.Env, "BEADS_DB"); ok {
			t.Errorf("bd %s: stale BEADS_DB %q leaked", c.argv(), db)
		}
	}
}

func TestBdSubprocessEnvUsesHardenedBDEnv(t *testing.T) {
	t.Parallel()
	beadsDir := filepath.Join(t.TempDir(), ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(`{"dolt_database":"metadata_db","dolt_server_host":"metadata-host","dolt_server_port":3307}`), 0644); err != nil {
		t.Fatal(err)
	}

	base := []string{
		"BEADS_DIR=/wrong",
		"BEADS_DB=/wrong.db",
		"BD_DB=/wrong.bd",
		"BEADS_DOLT_SERVER_DATABASE=wrongdb",
		"BEADS_DOLT_SERVER_HOST=inherited-host",
		"BEADS_DOLT_SERVER_PORT=4401",
		"BEADS_DOLT_PORT=4401",
		"BEADS_DOLT_DATA_DIR=/wrong/data",
		"BEADS_DOLT_AUTO_START=1",
		"GT_DOLT_DATA=/wrong/gt-data",
		"GT_DOLT_HOST=127.0.0.2",
		"GT_DOLT_PORT=5507",
	}
	env := bdSubprocessEnv(base, beadsDir, "explicit_db")
	got := rigEnvMap(env)
	if got["BEADS_DIR"] != beadsDir {
		t.Fatalf("BEADS_DIR = %q, want %q in %v", got["BEADS_DIR"], beadsDir, env)
	}
	if got["BEADS_DOLT_SERVER_DATABASE"] != "explicit_db" {
		t.Fatalf("BEADS_DOLT_SERVER_DATABASE = %q, want explicit_db in %v", got["BEADS_DOLT_SERVER_DATABASE"], env)
	}
	// Outside a town there is no endpoint: bd gets the endpoint it
	// inherited, and GT_DOLT_* is never translated (gt-y3pgh.3).
	if got["BEADS_DOLT_SERVER_HOST"] != "inherited-host" {
		t.Fatalf("BEADS_DOLT_SERVER_HOST = %q, want the inherited host in %v", got["BEADS_DOLT_SERVER_HOST"], env)
	}
	if got["BEADS_DOLT_SERVER_PORT"] != "4401" || got["BEADS_DOLT_PORT"] != "4401" {
		t.Fatalf("ports = server:%q legacy:%q, want the inherited 4401 in %v", got["BEADS_DOLT_SERVER_PORT"], got["BEADS_DOLT_PORT"], env)
	}
	if got["BEADS_DOLT_AUTO_START"] != "0" || got["BD_DOLT_AUTO_COMMIT"] != "on" {
		t.Fatalf("bd mutation guardrails missing in %v", env)
	}
	for _, key := range []string{"BEADS_DB", "BD_DB", "BEADS_DOLT_DATA_DIR", "GT_DOLT_DATA"} {
		if value, ok := got[key]; ok {
			t.Fatalf("%s leaked as %q in %v", key, value, env)
		}
	}
}

func TestBdSubprocessEnvUsesTownConfigBeforeMetadataExists(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	mayorDir := filepath.Join(townRoot, "mayor")
	if err := os.MkdirAll(mayorDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mayorDir, "town.json"), []byte(`{"name":"test-town"}`), 0644); err != nil {
		t.Fatal(err)
	}
	doltDataDir := filepath.Join(townRoot, ".dolt-data")
	if err := os.MkdirAll(doltDataDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(doltDataDir, "config.yaml"), []byte("listener:\n  host: 127.0.0.2\n  port: 5507\n"), 0644); err != nil {
		t.Fatal(err)
	}
	beadsDir := filepath.Join(townRoot, "rig", ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	base := []string{
		"GT_DOLT_HOST=stale-host",
		"GT_DOLT_PORT=9999",
		"BEADS_DOLT_SERVER_HOST=stale-host",
		"BEADS_DOLT_SERVER_PORT=9999",
		"BEADS_DOLT_PORT=9999",
	}

	env := bdSubprocessEnv(base, beadsDir, "explicit_db")
	got := rigEnvMap(env)
	if got["BEADS_DOLT_SERVER_DATABASE"] != "explicit_db" {
		t.Fatalf("BEADS_DOLT_SERVER_DATABASE = %q, want explicit_db in %v", got["BEADS_DOLT_SERVER_DATABASE"], env)
	}
	if got["BEADS_DOLT_SERVER_HOST"] != "127.0.0.2" {
		t.Fatalf("BEADS_DOLT_SERVER_HOST = %q, want config host in %v", got["BEADS_DOLT_SERVER_HOST"], env)
	}
	if got["BEADS_DOLT_SERVER_PORT"] != "5507" || got["BEADS_DOLT_PORT"] != "5507" {
		t.Fatalf("ports = server:%q legacy:%q, want config port in %v", got["BEADS_DOLT_SERVER_PORT"], got["BEADS_DOLT_PORT"], env)
	}
}

func TestBdSubprocessEnvClearsStaleHostWhenConfigHasNoHost(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	mayorDir := filepath.Join(townRoot, "mayor")
	if err := os.MkdirAll(mayorDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mayorDir, "town.json"), []byte(`{"name":"test-town"}`), 0644); err != nil {
		t.Fatal(err)
	}
	doltDataDir := filepath.Join(townRoot, ".dolt-data")
	if err := os.MkdirAll(doltDataDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(doltDataDir, "config.yaml"), []byte("listener:\n  port: 5507\n"), 0644); err != nil {
		t.Fatal(err)
	}
	beadsDir := filepath.Join(townRoot, "rig", ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	base := []string{"GT_DOLT_HOST=stale-host", "BEADS_DOLT_SERVER_HOST=stale-host", "GT_DOLT_PORT=9999"}

	env := bdSubprocessEnv(base, beadsDir, "explicit_db")
	got := rigEnvMap(env)
	if _, ok := got["BEADS_DOLT_SERVER_HOST"]; ok {
		t.Fatalf("BEADS_DOLT_SERVER_HOST leaked from config without host: %v", env)
	}
	if got["BEADS_DOLT_SERVER_PORT"] != "5507" || got["BEADS_DOLT_PORT"] != "5507" {
		t.Fatalf("ports = server:%q legacy:%q, want config port in %v", got["BEADS_DOLT_SERVER_PORT"], got["BEADS_DOLT_PORT"], env)
	}
}

// bd init gets the town's port; a town without an endpoint passes none
// rather than a guessed default (gt-y3pgh.3).
func TestBdInitServerPort(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	doltDataDir := filepath.Join(townRoot, ".dolt-data")
	if err := os.MkdirAll(doltDataDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(doltDataDir, "config.yaml"), []byte("listener:\n  port: 5507\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if got := bdInitServerPort(townRoot); got != 5507 {
		t.Fatalf("bdInitServerPort() = %d, want 5507", got)
	}
	if got := bdInitServerPort(t.TempDir()); got != 0 {
		t.Fatalf("bdInitServerPort(no endpoint) = %d, want none", got)
	}
}

func rigEnvMap(env []string) map[string]string {
	out := make(map[string]string)
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			out[key] = value
		}
	}
	return out
}

func TestIsValidBeadsPrefix(t *testing.T) {
	t.Parallel()
	tests := []struct {
		prefix string
		want   bool
	}{
		// Valid prefixes
		{"gt", true},
		{"bd", true},
		{"hq", true},
		{"gastown", true},
		{"myProject", true},
		{"my-project", true},
		{"a", true},
		{"A", true},
		{"test123", true},
		{"a1b2c3", true},
		{"a-b-c", true},

		// Invalid prefixes
		{"", false},                      // empty
		{"1abc", false},                  // starts with number
		{"-abc", false},                  // starts with hyphen
		{"abc def", false},               // contains space
		{"abc;ls", false},                // shell injection attempt
		{"$(whoami)", false},             // command substitution
		{"`id`", false},                  // backtick command
		{"abc|cat", false},               // pipe
		{"../etc/passwd", false},         // path traversal
		{"aaaaaaaaaaaaaaaaaaaaa", false}, // too long (21 chars, >20 limit)
		{"valid-but-with-$var", false},   // variable reference
	}

	for _, tt := range tests {
		t.Run(tt.prefix, func(t *testing.T) {
			got := isValidBeadsPrefix(tt.prefix)
			if got != tt.want {
				t.Errorf("isValidBeadsPrefix(%q) = %v, want %v", tt.prefix, got, tt.want)
			}
		})
	}
}

// TestDropRigOrphanDBs_RemovesPrefixDB is the regression test for gh#3562.
//
// `bd init --prefix <prefix> --server` creates a Dolt database whose name
// matches the prefix exactly (e.g. "ma" for prefix=ma) on bd >= 0.62. The rig
// uses <rigName> as its canonical database, so the prefix-named DB is an
// orphan that must be removed — otherwise beads created from the rig land in
// the orphan while the mayor reads from <rigName>, silently splitting the data.
func TestDropRigOrphanDBs_RemovesPrefixDB(t *testing.T) {
	t.Parallel()
	const rigName = "mobile_apps"
	const prefix = "ma"
	dolt := newFakeDolt(rigName, prefix)

	if err := dropRigOrphanDBs(dolt, prefix, rigName); err != nil {
		t.Fatalf("dropRigOrphanDBs: %v", err)
	}
	if dolt.Exists(prefix) {
		t.Errorf("prefix DB %q should have been removed", prefix)
	}
	if !dolt.Exists(rigName) {
		t.Errorf("rig DB %q should be preserved", rigName)
	}
}

// TestDropRigOrphanDBs_RemovesLegacyBeadsPrefixDB covers the bd < 0.62
// naming convention where bd init created "beads_<prefix>" instead of
// "<prefix>". Both forms must be cleaned up to keep older workspaces
// functional after upgrade.
func TestDropRigOrphanDBs_RemovesLegacyBeadsPrefixDB(t *testing.T) {
	t.Parallel()
	const rigName = "mobile_apps"
	const prefix = "ma"
	const legacyOrphan = "beads_ma"
	dolt := newFakeDolt(rigName, legacyOrphan)

	if err := dropRigOrphanDBs(dolt, prefix, rigName); err != nil {
		t.Fatalf("dropRigOrphanDBs: %v", err)
	}
	if dolt.Exists(legacyOrphan) {
		t.Errorf("legacy orphan %q should have been removed", legacyOrphan)
	}
	if !dolt.Exists(rigName) {
		t.Errorf("rig DB %q should be preserved", rigName)
	}
}

// TestDropRigOrphanDBs_PreservesRigDB is the safety check that the helper
// never removes a database whose name happens to match the prefix when the
// rig itself is named after its prefix.
func TestDropRigOrphanDBs_PreservesRigDB(t *testing.T) {
	t.Parallel()
	// Pathological case: rigName == prefix. Nothing should be dropped.
	const rigName = "gt"
	const prefix = "gt"
	dolt := newFakeDolt(rigName, "hq")

	if err := dropRigOrphanDBs(dolt, prefix, rigName); err != nil {
		t.Fatalf("dropRigOrphanDBs: %v", err)
	}
	if !dolt.Exists(rigName) || !dolt.Exists("hq") {
		t.Errorf("rig DB %q and hq must be preserved when prefix == rigName", rigName)
	}
}

// An orphan that survives its removal is an error naming it: AddRig treats
// that as fatal rather than leave a silently split rig.
func TestDropRigOrphanDBs_ReportsSurvivingOrphan(t *testing.T) {
	t.Parallel()
	dolt := newFakeDolt("mobile_apps", "ma")
	dolt.stuck["ma"] = true

	err := dropRigOrphanDBs(dolt, "ma", "mobile_apps")
	if err == nil || !strings.Contains(err.Error(), "ma: still present after RemoveDatabase") {
		t.Fatalf("dropRigOrphanDBs = %v, want the surviving orphan named", err)
	}
}

func TestInitBeadsRejectsInvalidPrefix(t *testing.T) {
	t.Parallel()
	rigPath := t.TempDir()
	manager := &Manager{}

	tests := []string{
		"",
		"$(whoami)",
		"abc;rm -rf /",
		"../etc",
		"123",
	}

	for _, prefix := range tests {
		t.Run(prefix, func(t *testing.T) {
			err := manager.InitBeads(rigPath, prefix, "")
			if err == nil {
				t.Errorf("initBeads(%q) should have failed", prefix)
			}
			if !strings.Contains(err.Error(), "invalid beads prefix") {
				t.Errorf("initBeads(%q) error = %q, want error containing 'invalid beads prefix'", prefix, err.Error())
			}
		})
	}
}

func TestDeriveBeadsPrefix(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		want string
	}{
		// Compound words with common suffixes should split
		{"gastown", "gt"},     // gas + town
		{"nashville", "nv"},   // nash + ville
		{"bridgeport", "bp"},  // bridge + port
		{"someplace", "sp"},   // some + place
		{"greenland", "gl"},   // green + land
		{"springfield", "sf"}, // spring + field
		{"hollywood", "hw"},   // holly + wood
		{"oxford", "of"},      // ox + ford

		// Hyphenated names
		{"my-project", "mp"},
		{"gas-town", "gt"},
		{"some-long-name", "sln"},

		// Underscored names
		{"my_project", "mp"},

		// Short single words (use the whole name)
		{"foo", "foo"},
		{"bar", "bar"},
		{"ab", "ab"},

		// Longer single words without known suffixes (first 2 chars)
		{"myrig", "my"},
		{"awesome", "aw"},
		{"coolrig", "co"},

		// camelCase names
		{"myProject", "mp"},
		{"gasStation", "gs"},
		{"HTMLParser", "hp"},

		// With language suffixes stripped
		{"myproject-py", "my"},
		{"myproject-go", "my"},

		// Path-like names (slashes stripped)
		{"/my_app", "ma"},
		{"/some/deep/path", "pa"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := deriveBeadsPrefix(tt.name)
			if got != tt.want {
				t.Errorf("deriveBeadsPrefix(%q) = %q, want %q", tt.name, got, tt.want)
			}
		})
	}
}

func TestSplitCompoundWord(t *testing.T) {
	t.Parallel()
	tests := []struct {
		word string
		want []string
	}{
		// Known suffixes
		{"gastown", []string{"gas", "town"}},
		{"nashville", []string{"nash", "ville"}},
		{"bridgeport", []string{"bridge", "port"}},
		{"someplace", []string{"some", "place"}},
		{"greenland", []string{"green", "land"}},
		{"springfield", []string{"spring", "field"}},
		{"hollywood", []string{"holly", "wood"}},
		{"oxford", []string{"ox", "ford"}},

		// Just the suffix (should not split)
		{"town", []string{"town"}},
		{"ville", []string{"ville"}},

		// No known suffix
		{"myrig", []string{"myrig"}},
		{"awesome", []string{"awesome"}},

		// Empty prefix would result (should not split)
		// Note: "town" itself shouldn't split to ["", "town"]
	}

	for _, tt := range tests {
		t.Run(tt.word, func(t *testing.T) {
			got := splitCompoundWord(tt.word)
			if len(got) != len(tt.want) {
				t.Errorf("splitCompoundWord(%q) = %v, want %v", tt.word, got, tt.want)
				return
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("splitCompoundWord(%q)[%d] = %q, want %q", tt.word, i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestSplitCamelCase(t *testing.T) {
	t.Parallel()
	tests := []struct {
		word string
		want []string
	}{
		// Basic camelCase
		{"myProject", []string{"my", "Project"}},
		{"gasStation", []string{"gas", "Station"}},

		// PascalCase
		{"MyProject", []string{"My", "Project"}},

		// Uppercase runs
		{"HTMLParser", []string{"HTML", "Parser"}},
		{"parseJSON", []string{"parse", "JSON"}},

		// No splits (single word, all lower)
		{"gastown", []string{"gastown"}},
		{"a", []string{"a"}},

		// All uppercase (no lower transition)
		{"AB", []string{"AB"}},

		// Empty
		{"", nil},
	}

	for _, tt := range tests {
		t.Run(tt.word, func(t *testing.T) {
			got := splitCamelCase(tt.word)
			if len(got) != len(tt.want) {
				t.Errorf("splitCamelCase(%q) = %v, want %v", tt.word, got, tt.want)
				return
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("splitCamelCase(%q)[%d] = %q, want %q", tt.word, i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestConvertToSSH(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		https   string
		wantSSH string
	}{
		{
			name:    "GitHub with .git suffix",
			https:   "https://github.com/owner/repo.git",
			wantSSH: "git@github.com:owner/repo.git",
		},
		{
			name:    "GitHub without .git suffix",
			https:   "https://github.com/owner/repo",
			wantSSH: "git@github.com:owner/repo.git",
		},
		{
			name:    "GitHub with org/subpath",
			https:   "https://github.com/myorg/myproject.git",
			wantSSH: "git@github.com:myorg/myproject.git",
		},
		{
			name:    "GitLab with .git suffix",
			https:   "https://gitlab.com/owner/repo.git",
			wantSSH: "git@gitlab.com:owner/repo.git",
		},
		{
			name:    "GitLab without .git suffix",
			https:   "https://gitlab.com/owner/repo",
			wantSSH: "git@gitlab.com:owner/repo.git",
		},
		{
			name:    "Bitbucket with .git suffix",
			https:   "https://bitbucket.org/owner/repo.git",
			wantSSH: "git@bitbucket.org:owner/repo.git",
		},
		{
			name:    "Bitbucket without .git suffix",
			https:   "https://bitbucket.org/owner/repo",
			wantSSH: "git@bitbucket.org:owner/repo.git",
		},
		{
			name:    "Unknown host returns empty",
			https:   "https://gitlab.example.com/owner/repo.git",
			wantSSH: "",
		},
		{
			name:    "Non-HTTPS URL returns empty",
			https:   "git@github.com:owner/repo.git",
			wantSSH: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := convertToSSH(tt.https)
			if got != tt.wantSSH {
				t.Errorf("convertToSSH(%q) = %q, want %q", tt.https, got, tt.wantSSH)
			}
		})
	}
}

func TestDetectBeadsPrefixFromConfig_TrailingDash(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		configYAML string
		want       string
	}{
		{
			name:       "prefix without trailing dash is unchanged",
			configYAML: "prefix: baseball-v3\n",
			want:       "baseball-v3",
		},
		{
			name:       "prefix with trailing dash is stripped",
			configYAML: "prefix: baseball-v3-\n",
			want:       "baseball-v3",
		},
		{
			name:       "issue-prefix with trailing dash is stripped",
			configYAML: "issue-prefix: baseball-v3-\n",
			want:       "baseball-v3",
		},
		{
			name:       "quoted prefix with trailing dash is stripped",
			configYAML: "prefix: \"baseball-v3-\"\n",
			want:       "baseball-v3",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			configPath := filepath.Join(dir, "config.yaml")
			if err := os.WriteFile(configPath, []byte(tt.configYAML), 0644); err != nil {
				t.Fatalf("writing config: %v", err)
			}
			got := detectBeadsPrefixFromConfig(configPath)
			if got != tt.want {
				t.Errorf("detectBeadsPrefixFromConfig() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDetectBeadsPrefixFromConfig_NoFallbackToJSONL(t *testing.T) {
	t.Parallel()
	// Verify that detectBeadsPrefixFromConfig does NOT fall back to issues.jsonl.
	// Gastown requires Dolt server — JSONL is not a supported data source.
	dir := t.TempDir()

	// Write config.yaml without a prefix key
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("# no prefix\n"), 0644); err != nil {
		t.Fatalf("writing config.yaml: %v", err)
	}

	// Write issues.jsonl with valid data — should be ignored
	issuesPath := filepath.Join(dir, "issues.jsonl")
	if err := os.WriteFile(issuesPath, []byte(`{"id":"gt-mawit","title":"test"}`), 0644); err != nil {
		t.Fatalf("writing issues.jsonl: %v", err)
	}

	got := detectBeadsPrefixFromConfig(configPath)
	if got != "" {
		t.Errorf("detectBeadsPrefixFromConfig() = %q, want empty (should not read issues.jsonl)", got)
	}
}

func TestIsStandardBeadHash(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input string
		want  bool
	}{
		{"mawit", true},
		{"abc12", true},
		{"z0ixd", true},
		{"00000", true},
		{"abcde", true},
		{"witness", false},    // too long (agent role)
		{"abc", false},        // too short
		{"ABC12", false},      // uppercase
		{"abc-1", false},      // contains hyphen
		{"", false},           // empty
		{"abc1234567", false}, // 10 chars (MR hash)
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := isStandardBeadHash(tt.input)
			if got != tt.want {
				t.Errorf("isStandardBeadHash(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestRegisterRig_RejectsReservedNames(t *testing.T) {
	t.Parallel()
	root, rigsConfig := setupTestTown(t)
	manager := newTestManager(root, rigsConfig)

	tests := []struct {
		name      string
		wantError string
	}{
		{"hq", `rig name "hq" is reserved for town-level infrastructure`},
		{"HQ", `rig name "HQ" is reserved for town-level infrastructure`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := manager.RegisterRig(RegisterRigOptions{
				Name: tt.name,
			})
			if err == nil {
				t.Errorf("RegisterRig(%q) succeeded, want error containing %q", tt.name, tt.wantError)
				return
			}
			if !strings.Contains(err.Error(), tt.wantError) {
				t.Errorf("RegisterRig(%q) error = %q, want error containing %q", tt.name, err.Error(), tt.wantError)
			}
		})
	}
}

func TestRegisterRig_DetectsAndPersistsCustomPushURL(t *testing.T) {
	t.Parallel()
	root, rigsConfig := setupTestTown(t)
	manager, f, _ := testManager(root, rigsConfig)

	rigName := "adoptme"
	rigPath := filepath.Join(root, rigName)
	upstreamURL := remoteRepo(t, f, filepath.Join(root, "upstream.git"), nil)
	forkURL := filepath.Join(root, "fork.git")
	f.Clone(t, upstreamURL, rigPath)
	if err := f.Open(rigPath).ConfigurePushURL("origin", forkURL); err != nil {
		t.Fatal(err)
	}

	result, err := manager.RegisterRig(RegisterRigOptions{Name: rigName, GitURL: upstreamURL})
	if err != nil {
		t.Fatalf("RegisterRig: %v", err)
	}

	if result.GitURL != upstreamURL {
		t.Errorf("GitURL = %q, want %q", result.GitURL, upstreamURL)
	}

	entry, ok := rigsConfig.Rigs[rigName]
	if !ok {
		t.Fatalf("rig entry %q missing from config", rigName)
	}
	if entry.PushURL != forkURL {
		t.Errorf("PushURL = %q, want %q", entry.PushURL, forkURL)
	}
}

func TestRegisterRig_DetectPushURLEmptyWhenPushEqualsFetch(t *testing.T) {
	t.Parallel()
	root, rigsConfig := setupTestTown(t)
	manager, f, _ := testManager(root, rigsConfig)

	rigName := "adoptsame"
	url := remoteRepo(t, f, filepath.Join(root, "upstream-same.git"), nil)
	f.Clone(t, url, filepath.Join(root, rigName))

	if _, err := manager.RegisterRig(RegisterRigOptions{Name: rigName, GitURL: url}); err != nil {
		t.Fatalf("RegisterRig: %v", err)
	}

	entry := rigsConfig.Rigs[rigName]
	if entry.PushURL != "" {
		t.Errorf("PushURL = %q, want empty when push URL equals fetch URL", entry.PushURL)
	}
}

// TestRegisterRig_RecordsTheRigDatabase: adopting a rig records the database
// its bd metadata.json names in the registry entry (gt-y3pgh.11).
func TestRegisterRig_RecordsTheRigDatabase(t *testing.T) {
	t.Parallel()
	root, rigsConfig := setupTestTown(t)
	manager, f, _ := testManager(root, rigsConfig)

	rigName := "adoptdb"
	url := remoteRepo(t, f, filepath.Join(root, "upstream-db.git"), nil)
	f.Clone(t, url, filepath.Join(root, rigName))
	meta := filepath.Join(root, rigName, ".beads", "metadata.json")
	if err := os.MkdirAll(filepath.Dir(meta), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(meta, []byte(`{"dolt_mode":"server","dolt_database":"adb"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := manager.RegisterRig(RegisterRigOptions{Name: rigName, GitURL: url}); err != nil {
		t.Fatalf("RegisterRig: %v", err)
	}
	if got := rigsConfig.Rigs[rigName].DoltDatabase; got != "adb" {
		t.Errorf("registry dolt_database = %q, want adb", got)
	}
}

// TestRegisterRig_ValidConfigUnchanged is the positive half of gt-8xk9k: an
// existing config.json that decodes still supplies the adopted values, and
// nothing is reported.
func TestRegisterRig_ValidConfigUnchanged(t *testing.T) {
	t.Parallel()
	root, rigsConfig := setupTestTown(t)
	manager, _, _ := testManager(root, rigsConfig)

	rigName := "adoptme"
	rigPath := filepath.Join(root, rigName)
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	url := "https://example.invalid/adoptme.git"
	writeRigConfigFile(t, rigPath, `{"type":"rig","version":1,"name":"adoptme","git_url":"`+url+`","default_branch":"develop"}`)

	result, err := manager.RegisterRig(RegisterRigOptions{Name: rigName})
	if err != nil {
		t.Fatalf("RegisterRig: %v", err)
	}
	if !result.FromConfig {
		t.Errorf("FromConfig = false for a valid config.json")
	}
	if result.DefaultBranch != "develop" {
		t.Errorf("DefaultBranch = %q; want develop", result.DefaultBranch)
	}
	if result.GitURL != url {
		t.Errorf("GitURL = %q; want %q from config", result.GitURL, url)
	}
	if RigConfigWarned(rigPath) {
		t.Errorf("RigConfigWarned(%s) = true for a valid config", rigPath)
	}
}

// TestRegisterRig_ReportsUnparseableConfig pins gt-8xk9k for adoption: an
// existing config.json that no longer decodes is reported once and treated as
// absent, so adoption falls back to the caller's values rather than reading a
// broken file as authoritative.
func TestRegisterRig_ReportsUnparseableConfig(t *testing.T) {
	t.Parallel()
	root, rigsConfig := setupTestTown(t)
	manager, _, _ := testManager(root, rigsConfig)

	rigName := "typorig"
	rigPath := filepath.Join(root, rigName)
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	url := "https://example.invalid/typorig.git"
	writeRigConfigFile(t, rigPath, `{"type":"rig","version":1,"name":"typorig","git_url":"https://example.invalid/other.git","default_branch":"develop","default_branchh":"x"}`)

	result, err := manager.RegisterRig(RegisterRigOptions{Name: rigName, GitURL: url})
	if err != nil {
		t.Fatalf("RegisterRig: %v", err)
	}
	if result.FromConfig {
		t.Errorf("FromConfig = true for an unparseable config.json")
	}
	if result.DefaultBranch != "" {
		t.Errorf("DefaultBranch = %q; want the empty fallback", result.DefaultBranch)
	}
	if result.GitURL != url {
		t.Errorf("GitURL = %q; want the caller's %q", result.GitURL, url)
	}
	if !RigConfigWarned(rigPath) {
		t.Errorf("RegisterRig did not report the unparseable config.json")
	}
}

func TestDetectGitURL_MayorRigFallback(t *testing.T) {
	t.Parallel()
	// Verify detectGitURL finds the origin remote from mayor/rig when the
	// root rigPath has no git repo.
	root, rigsConfig := setupTestTown(t)
	manager, f, _ := testManager(root, rigsConfig)

	rigName := "detectme"
	upstreamURL := remoteRepo(t, f, filepath.Join(root, "detect-upstream.git"), nil)
	// mayor/rig is a clone; the rig directory itself is not a repository.
	f.Clone(t, upstreamURL, filepath.Join(root, rigName, "mayor", "rig"))

	// detectGitURL is unexported, so test via RegisterRig with no --url
	result, err := manager.RegisterRig(RegisterRigOptions{Name: rigName, Force: true})
	if err != nil {
		t.Fatalf("RegisterRig: %v", err)
	}
	if result.GitURL != upstreamURL {
		t.Errorf("GitURL = %q, want %q (should detect from mayor/rig)", result.GitURL, upstreamURL)
	}
}

func TestRegisterRig_LegacyConfigPreservesExistingPushURL(t *testing.T) {
	t.Parallel()
	// Legacy config.json (pre-push_url feature) has no push_url field.
	// RegisterRig should NOT clear existing push URLs in .repo.git because
	// empty push_url in legacy config is indistinguishable from "never set"
	// due to omitempty. Existing git push URLs are preserved.
	root, rigsConfig := setupTestTown(t)
	manager, f, _ := testManager(root, rigsConfig)

	rigName := "legacyrig"
	rigPath := filepath.Join(root, rigName)
	upstreamURL := remoteRepo(t, f, filepath.Join(root, "upstream-legacy.git"), nil)
	forkURL := filepath.Join(root, "fork-legacy.git")

	// Create .repo.git bare repo with a custom push URL (user configured manually)
	bareRepoPath := filepath.Join(rigPath, ".repo.git")
	if err := f.Open(root).CloneBareWithBranch(upstreamURL, bareRepoPath, ""); err != nil {
		t.Fatal(err)
	}
	bare := f.OpenWithDir(bareRepoPath, "")
	if err := bare.ConfigurePushURL("origin", forkURL); err != nil {
		t.Fatal(err)
	}

	// Write legacy config.json WITHOUT push_url field
	configData := fmt.Sprintf(`{"type":"rig","version":1,"name":"%s","git_url":"%s","default_branch":"main"}`, rigName, upstreamURL)
	if err := os.WriteFile(filepath.Join(rigPath, "config.json"), []byte(configData), 0644); err != nil {
		t.Fatalf("write config.json: %v", err)
	}

	if _, err := manager.RegisterRig(RegisterRigOptions{Name: rigName, GitURL: upstreamURL}); err != nil {
		t.Fatalf("RegisterRig: %v", err)
	}

	// Legacy config has empty PushURL, so auto-detect runs and finds the fork URL
	// from .repo.git. The detected push URL is persisted to both config.json and
	// town config (RigEntry.PushURL), while keeping "not authoritative" semantics
	// (no clearing on empty detect).
	entry := rigsConfig.Rigs[rigName]
	if entry.PushURL != forkURL {
		t.Errorf("town config PushURL = %q, want %q (auto-detected from .repo.git)", entry.PushURL, forkURL)
	}
	if got, err := bare.GetPushURL("origin"); err != nil || got != forkURL {
		t.Errorf(".repo.git push URL = %q, %v; want %q (should be preserved for legacy config)", got, err, forkURL)
	}
	cfg, err := LoadRigConfig(rigPath)
	if err != nil || cfg.PushURL != forkURL {
		t.Errorf("config.json push_url = %+v, %v; want %q synced from detection", cfg, err, forkURL)
	}
}

func TestEnsureMetadata_SetsRequiredFields(t *testing.T) {
	t.Parallel()
	// Verify that EnsureMetadata writes the fields that AddRig depends on:
	// dolt_mode=server, dolt_database=<rigName>, backend=dolt
	// This guards against the regression fixed in PR #1343.
	townRoot := t.TempDir()
	rigName := "myrig"

	// Create the beads directory structure that EnsureMetadata expects
	beadsDir := filepath.Join(townRoot, rigName, "mayor", "rig", ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatalf("mkdir beads dir: %v", err)
	}

	if err := doltserver.EnsureMetadata(townRoot, rigName); err != nil {
		t.Fatalf("EnsureMetadata: %v", err)
	}

	metadataPath := filepath.Join(beadsDir, "metadata.json")
	data, err := os.ReadFile(metadataPath)
	if err != nil {
		t.Fatalf("read metadata.json: %v", err)
	}

	var meta map[string]interface{}
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatalf("parse metadata.json: %v", err)
	}

	checks := map[string]string{
		"backend":       "dolt",
		"dolt_mode":     "server",
		"dolt_database": rigName,
	}
	for key, want := range checks {
		got, ok := meta[key].(string)
		if !ok {
			t.Errorf("metadata.json missing %q field", key)
			continue
		}
		if got != want {
			t.Errorf("metadata.json %q = %q, want %q", key, got, want)
		}
	}
}

func TestAddRig_UpstreamURL(t *testing.T) {
	t.Parallel()
	root, rigsConfig := setupTestTown(t)
	manager, f, _ := testManager(root, rigsConfig)
	forkURL := remoteRepo(t, f, filepath.Join(t.TempDir(), "fork"), nil)
	upstreamURL := remoteRepo(t, f, filepath.Join(t.TempDir(), "upstream"), nil)

	if _, err := manager.AddRig(AddRigOptions{
		Name:          "forkrig",
		GitURL:        forkURL,
		UpstreamURL:   upstreamURL,
		BeadsPrefix:   "fk",
		SkipDoltCheck: true,
	}); err != nil {
		t.Fatalf("AddRig: %v", err)
	}

	rigPath := filepath.Join(root, "forkrig")
	for name, g := range map[string]Repo{
		"bare repo":   f.OpenWithDir(filepath.Join(rigPath, ".repo.git"), ""),
		"mayor clone": f.Open(filepath.Join(rigPath, "mayor", "rig")),
	} {
		if got, err := g.RemoteURL("upstream"); err != nil || got != upstreamURL {
			t.Errorf("%s upstream = %q, %v; want %q", name, got, err, upstreamURL)
		}
	}
	cfg, err := LoadRigConfig(rigPath)
	if err != nil {
		t.Fatalf("reading config.json: %v", err)
	}
	if cfg.UpstreamURL != upstreamURL {
		t.Errorf("config.json UpstreamURL = %q, want %q", cfg.UpstreamURL, upstreamURL)
	}
	if entry := rigsConfig.Rigs["forkrig"]; entry.UpstreamURL != upstreamURL {
		t.Errorf("RigEntry.UpstreamURL = %q, want %q", entry.UpstreamURL, upstreamURL)
	}
}

// TestAddRig_MayorCloneProtectsBeads is a regression guard for gt-ylpg: the
// mayor clone is a standalone `git clone`, not a linked worktree off the
// bare repo, so it needs its own .git/info/exclude entry for .beads/ or the
// credential key, backups, and locks it holds are one `git clean -fd` away
// from deletion. That git honors the entry is TestIntegrationAddRig's.
func TestAddRig_MayorCloneProtectsBeads(t *testing.T) {
	t.Parallel()
	root, rigsConfig := setupTestTown(t)
	manager, f, _ := testManager(root, rigsConfig)
	gitURL := remoteRepo(t, f, filepath.Join(t.TempDir(), "source"), nil)

	if _, err := manager.AddRig(AddRigOptions{
		Name:          "beadsrig",
		GitURL:        gitURL,
		BeadsPrefix:   "bd",
		SkipDoltCheck: true,
	}); err != nil {
		t.Fatalf("AddRig: %v", err)
	}

	exclude, err := os.ReadFile(filepath.Join(root, "beadsrig", "mayor", "rig", ".git", "info", "exclude"))
	if err != nil {
		t.Fatalf("mayor clone has no info/exclude: %v", err)
	}
	if !containsLine(string(exclude), ".beads/") {
		t.Fatalf("mayor clone info/exclude does not cover .beads/:\n%s", exclude)
	}
}

// TestAddRig_BranchFlag verifies that --branch is passed to the bare clone so
// the bare repo's HEAD and origin tracking ref both point to the specified branch.
func TestAddRig_BranchFlag(t *testing.T) {
	t.Parallel()
	root, rigsConfig := setupTestTown(t)
	manager, f, _ := testManager(root, rigsConfig)
	// A remote with two branches: main (its HEAD) and develop.
	repoDir := remoteRepo(t, f, filepath.Join(t.TempDir(), "remote.git"), nil)
	f.SetRef(t, repoDir, "refs/heads/develop", f.Ref(repoDir, "refs/heads/main"))
	f.Commit(t, repoDir, "develop", "develop commit", map[string]string{"develop.txt": "develop branch\n"})

	if _, err := manager.AddRig(AddRigOptions{
		Name:          "testrig",
		GitURL:        repoDir,
		BeadsPrefix:   "tr",
		DefaultBranch: "develop",
		SkipDoltCheck: true,
	}); err != nil {
		t.Fatalf("AddRig: %v", err)
	}

	rigPath := filepath.Join(root, "testrig")
	bareGit := f.OpenWithDir(filepath.Join(rigPath, ".repo.git"), "")
	if got := bareGit.DefaultBranch(); got != "develop" {
		t.Errorf("bare repo DefaultBranch() = %q, want %q", got, "develop")
	}
	if exists, err := bareGit.RefExists("refs/remotes/origin/develop"); err != nil || !exists {
		t.Errorf("refs/remotes/origin/develop in bare repo: %v, %v", exists, err)
	}
	if _, err := os.Stat(filepath.Join(rigPath, "mayor", "rig", "develop.txt")); err != nil {
		t.Errorf("mayor clone is not on develop: %v", err)
	}
	cfg, err := LoadRigConfig(rigPath)
	if err != nil {
		t.Fatalf("reading config.json: %v", err)
	}
	if cfg.DefaultBranch != "develop" {
		t.Errorf("config.json DefaultBranch = %q, want %q", cfg.DefaultBranch, "develop")
	}
}

func TestBeadsConfigHasSyncRemote(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		configYAML string
		want       bool
	}{
		{
			name:       "sync.remote present",
			configYAML: "sync.remote: \"git+https://github.com/example/repo.git\"\n",
			want:       true,
		},
		{
			name:       "sync.remote with single quotes",
			configYAML: "sync.remote: 'git+ssh://git@github.com/example/repo.git'\n",
			want:       true,
		},
		{
			name:       "sync.remote empty value",
			configYAML: "sync.remote: \"\"\n",
			want:       false,
		},
		{
			name:       "sync.remote absent",
			configYAML: "prefix: gt\nissue-prefix: gt\n",
			want:       false,
		},
		{
			name:       "sync.remote commented out",
			configYAML: "# sync.remote: git+https://example.com/repo.git\nprefix: gt\n",
			want:       false,
		},
		{
			name:       "sync.remote with prefix and other keys",
			configYAML: "prefix: gt\nsync.remote: git+https://github.com/org/repo.git\ndolt.idle-timeout: \"0\"\n",
			want:       true,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			configPath := filepath.Join(dir, "config.yaml")
			if err := os.WriteFile(configPath, []byte(tt.configYAML), 0644); err != nil {
				t.Fatalf("writing config: %v", err)
			}
			got := beadsConfigHasSyncRemote(configPath)
			if got != tt.want {
				t.Errorf("beadsConfigHasSyncRemote() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBeadsConfigHasSyncRemote_MissingFile(t *testing.T) {
	t.Parallel()
	got := beadsConfigHasSyncRemote("/nonexistent/path/config.yaml")
	if got {
		t.Error("beadsConfigHasSyncRemote() = true for missing file, want false")
	}
}

// A source repo tracking beads config with sync.remote makes bd init prompt
// unless told to reinit (GH #3873): AddRig passes the flags.
func TestAddRig_TrackedBeadsWithSyncRemote_PassesReinitFlags(t *testing.T) {
	t.Parallel()
	root, rigsConfig := setupTestTown(t)
	manager, f, bd := testManager(root, rigsConfig)
	repoDir := remoteRepo(t, f, filepath.Join(t.TempDir(), "remote.git"), map[string]string{
		".beads/config.yaml": "prefix: gt\nsync.remote: \"git+https://github.com/steveyegge/gastown.git\"\n",
	})

	if _, err := manager.AddRig(AddRigOptions{
		Name:          "testrip",
		GitURL:        repoDir,
		BeadsPrefix:   "gt",
		SkipDoltCheck: true,
	}); err != nil {
		t.Fatalf("AddRig: %v", err)
	}

	mayorRig := filepath.Join(root, "testrip", "mayor", "rig")
	var sourceInit *beads.InitOptions
	for _, c := range bd.withVerb("init") {
		if c.Dir == mayorRig {
			sourceInit = &c.Init
		}
	}
	if sourceInit == nil {
		t.Fatalf("no bd init in the mayor clone; calls:\n%s", strings.Join(bd.argvs(), "\n"))
	}
	if !sourceInit.ReinitLocal || !sourceInit.DiscardRemote || sourceInit.DestroyToken != "DESTROY-gt" || !sourceInit.SkipAgents {
		t.Errorf("bd init options = %+v, want reinit-local, discard-remote, destroy token DESTROY-gt, skip-agents", *sourceInit)
	}
}

func TestAddRig_TrackedBeadsWithoutSyncRemote_NoReinitFlags(t *testing.T) {
	t.Parallel()
	root, rigsConfig := setupTestTown(t)
	manager, f, bd := testManager(root, rigsConfig)
	repoDir := remoteRepo(t, f, filepath.Join(t.TempDir(), "remote.git"), map[string]string{
		".beads/config.yaml": "prefix: gt\n",
	})

	if _, err := manager.AddRig(AddRigOptions{
		Name:          "testrip",
		GitURL:        repoDir,
		BeadsPrefix:   "gt",
		SkipDoltCheck: true,
	}); err != nil {
		t.Fatalf("AddRig: %v", err)
	}

	inits := bd.withVerb("init")
	if len(inits) == 0 {
		t.Fatalf("no bd init ran; calls:\n%s", strings.Join(bd.argvs(), "\n"))
	}
	for _, c := range inits {
		if c.Init.ReinitLocal || c.Init.DiscardRemote {
			t.Errorf("bd init should NOT reinit or discard the remote without sync.remote; got %+v", c.Init)
		}
	}
}

// setupIdentityRig creates a rig directory with a server-mode metadata.json
// pointing at the rig-named database, matching the state gt rig add leaves
// behind after EnsureMetadata, and a manager whose bd config get of
// issue_prefix answers value, or fails with err when it is set.
func setupIdentityRig(t *testing.T, rigName, value string, err error) (*Manager, string) {
	t.Helper()
	root, rigsConfig := setupTestTown(t)
	rigPath := filepath.Join(root, rigName)
	beadsDir := filepath.Join(rigPath, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatalf("mkdir beads: %v", err)
	}
	metadata := fmt.Sprintf(`{"backend":"dolt","dolt_mode":"server","dolt_database":%q}`, rigName)
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(metadata), 0644); err != nil {
		t.Fatalf("write metadata.json: %v", err)
	}
	manager, _, bd := testManager(root, rigsConfig)
	bd.config = map[string]string{"issue_prefix": value}
	bd.fail = func(bdCall) error { return err }
	return manager, rigPath
}

// TestVerifyRigIdentityRoundTrip covers gt-79g: metadata.json can name the
// correct database while that database is uninitialized (issue_prefix missing)
// or the data actually lives in a prefix-derived orphan. A name-only check
// passes in that state, so verifyRigIdentity must round-trip through bd —
// the same read path every agent uses — and confirm the expected prefix.
func TestVerifyRigIdentityRoundTrip(t *testing.T) {
	t.Parallel()
	const rigName = "gastown"
	const prefix = "gt"

	t.Run("healthy database round-trips", func(t *testing.T) {
		t.Parallel()
		manager, rigPath := setupIdentityRig(t, rigName, "gt", nil)
		if err := manager.VerifyRigIdentity(rigPath, rigName, prefix); err != nil {
			t.Errorf("verifyRigIdentity = %v, want nil", err)
		}
	})

	t.Run("uninitialized database fails round-trip", func(t *testing.T) {
		t.Parallel()
		manager, rigPath := setupIdentityRig(t, rigName, "", &beads.CLIError{
			Args: []string{"config", "get", "issue_prefix"}, Stderr: []byte("Error: database not initialized: issue_prefix missing\n"), Err: errors.New("exit status 1")})
		err := manager.VerifyRigIdentity(rigPath, rigName, prefix)
		if err == nil {
			t.Fatal("verifyRigIdentity = nil, want round-trip error for uninitialized database")
		}
		if !strings.Contains(err.Error(), "round-trip") {
			t.Errorf("error %q should mention round-trip verification", err)
		}
		if !strings.Contains(err.Error(), "issue_prefix missing") {
			t.Errorf("error %q should include bd's output for diagnosis", err)
		}
	})

	t.Run("wrong prefix fails round-trip", func(t *testing.T) {
		t.Parallel()
		manager, rigPath := setupIdentityRig(t, rigName, "other", nil)
		err := manager.VerifyRigIdentity(rigPath, rigName, prefix)
		if err == nil {
			t.Fatal("verifyRigIdentity = nil, want error for prefix mismatch")
		}
		if !strings.Contains(err.Error(), "other") || !strings.Contains(err.Error(), prefix) {
			t.Errorf("error %q should name both the actual and expected prefix", err)
		}
	})

	t.Run("missing bd skips round-trip", func(t *testing.T) {
		t.Parallel()
		manager, rigPath := setupIdentityRig(t, rigName, "", &exec.Error{Name: "bd", Err: exec.ErrNotFound})
		if err := manager.VerifyRigIdentity(rigPath, rigName, prefix); err != nil {
			t.Errorf("verifyRigIdentity = %v, want nil when bd is not installed", err)
		}
	})

	t.Run("empty prefix skips round-trip", func(t *testing.T) {
		t.Parallel()
		manager, rigPath := setupIdentityRig(t, rigName, "", errors.New("exit status 1"))
		if err := manager.VerifyRigIdentity(rigPath, rigName, ""); err != nil {
			t.Errorf("verifyRigIdentity = %v, want nil when no prefix is expected", err)
		}
	})
}

// TestAddRig_KeepsRegistryChangesLandedDuringClone is the regression test for
// gt-4iobv: AddRig holds the registry as it was before the clone, and saving
// that snapshot whole dropped every rig another gt process registered or
// parked while the clone ran.
func TestAddRig_KeepsRegistryChangesLandedDuringClone(t *testing.T) {
	t.Parallel()
	root, rigsConfig := setupTestTown(t)
	manager, f, _ := testManager(root, rigsConfig)
	gitURL := remoteRepo(t, f, filepath.Join(t.TempDir(), "source"), nil)

	// The registry as AddRig's snapshot found it: riga, not parked.
	rigsConfig.Rigs["riga"] = config.RigEntry{GitURL: "https://example.com/a.git"}
	rigsPath := filepath.Join(root, "mayor", "rigs.json")
	if err := config.SaveRigsConfig(rigsPath, rigsConfig); err != nil {
		t.Fatalf("saving the registry snapshot: %v", err)
	}

	// The clone runs for minutes. In that window gt rig park parks riga and
	// a second gt rig add registers rigb.
	if _, err := townconfig.Park(root, "riga", config.RigParked{By: "test", Reason: "concurrent"}); err != nil {
		t.Fatalf("parking riga: %v", err)
	}
	concurrent, err := config.LoadRigsConfig(rigsPath)
	if err != nil {
		t.Fatalf("loading the registry: %v", err)
	}
	concurrent.Rigs["rigb"] = config.RigEntry{GitURL: "https://example.com/b.git"}
	if err := config.SaveRigsConfig(rigsPath, concurrent); err != nil {
		t.Fatalf("registering rigb: %v", err)
	}

	if _, err := manager.AddRig(AddRigOptions{
		Name:          "rigc",
		GitURL:        gitURL,
		BeadsPrefix:   "rc",
		SkipDoltCheck: true,
	}); err != nil {
		t.Fatalf("AddRig: %v", err)
	}

	after, err := config.LoadRigsConfig(rigsPath)
	if err != nil {
		t.Fatalf("loading the registry after the add: %v", err)
	}
	if _, ok := after.Rigs["rigb"]; !ok {
		t.Errorf("rigb, registered while the clone ran, is gone: %v", after.Rigs)
	}
	parked := after.Rigs["riga"].Parked
	if parked == nil {
		t.Errorf("riga's park record is gone: %v", after.Rigs)
	} else if parked.Reason != "concurrent" {
		t.Errorf("riga's park record = %+v, want the record the concurrent park wrote", *parked)
	}
	if _, ok := after.Rigs["rigc"]; !ok {
		t.Errorf("rigc, the rig this add registered, is missing: %v", after.Rigs)
	}
}
