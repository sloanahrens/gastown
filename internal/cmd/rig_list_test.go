package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
)

// TestRunRigListJSON_RepoPath pins the contract the git-hygiene,
// gitignore-reconcile, and submodule-commit plugins depend on: every rig in
// `gt rig list --json` carries a repo_path that is the rig's actual git working
// tree, so `git -C "$path"` operates on the rig repository.
//
// The trap this guards against is that repo_path must NOT be the rig root.
// The rig root is not a working tree — `gt rig add` puts the clone at
// <rig>/mayor/rig — and because the town root is itself git-tracked, running
// git against the rig root resolves to the *town* repository by upward search.
// Plugins would then inspect and mutate the wrong repo.
func TestRunRigListJSON_RepoPath(t *testing.T) {
	const rigName = "riglist-a"

	townRoot := t.TempDir()
	// runRigList resolves the town root through os.Getwd, which reports the
	// physical path; on macOS t.TempDir's /var/... is a symlink to /private/var.
	// Compare against resolved paths so the assertion is about repo_path, not
	// about how the temp directory is spelled.
	expectedTownRoot, err := filepath.EvalSymlinks(townRoot)
	if err != nil {
		t.Fatalf("resolving temp dir: %v", err)
	}

	mayorDir := filepath.Join(townRoot, "mayor")
	if err := os.MkdirAll(mayorDir, 0755); err != nil {
		t.Fatalf("creating mayor dir: %v", err)
	}

	rigPath := filepath.Join(expectedTownRoot, rigName)
	repoPath := filepath.Join(rigPath, "mayor", "rig")
	if err := os.MkdirAll(filepath.Join(repoPath, ".git"), 0755); err != nil {
		t.Fatalf("creating rig clone: %v", err)
	}

	rigsConfig := &config.RigsConfig{
		Version: config.CurrentRigsVersion,
		Rigs: map[string]config.RigEntry{
			rigName: {
				GitURL:  "https://example.com/" + rigName + ".git",
				AddedAt: time.Now(),
			},
		},
	}
	if err := config.SaveRigsConfig(filepath.Join(mayorDir, "rigs.json"), rigsConfig); err != nil {
		t.Fatalf("saving rigs.json: %v", err)
	}

	originalWd, _ := os.Getwd()
	defer os.Chdir(originalWd)
	if err := os.Chdir(townRoot); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	originalJSON := rigListJSON
	rigListJSON = true
	defer func() { rigListJSON = originalJSON }()

	var runErr error
	out := captureStdout(t, func() { runErr = runRigList(nil, nil) })
	if runErr != nil {
		t.Fatalf("runRigList: %v", runErr)
	}

	var rigs []map[string]any
	if err := json.Unmarshal([]byte(out), &rigs); err != nil {
		t.Fatalf("parsing rig list JSON (%q): %v", out, err)
	}
	if len(rigs) != 1 {
		t.Fatalf("got %d rigs in JSON, want 1: %q", len(rigs), out)
	}

	got, ok := rigs[0]["repo_path"]
	if !ok {
		t.Fatalf("repo_path missing from rig list JSON: %q", out)
	}
	if got != repoPath {
		t.Errorf("repo_path = %v, want %q", got, repoPath)
	}
	if got == rigPath {
		t.Errorf("repo_path is the rig root (%q); plugins would run git against the town repo", rigPath)
	}
}

func TestGetRigLED(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		hasWitness  bool
		hasRefinery bool
		opState     string
		want        string
	}{
		// Operational state overrides session state (GH#2555)
		{"parked no sessions", false, false, "PARKED", "🅿️"},
		{"parked with sessions", true, true, "PARKED", "🅿️"},
		{"parked partial", true, false, "PARKED", "🅿️"},
		{"docked no sessions", false, false, "DOCKED", "🛑"},
		{"docked with sessions", true, true, "DOCKED", "🛑"},

		// Both running - fully active
		{"both running", true, true, "OPERATIONAL", "🟢"},

		// One running - partially active
		{"witness only", true, false, "OPERATIONAL", "🟡"},
		{"refinery only", false, true, "OPERATIONAL", "🟡"},

		// Nothing running
		{"stopped operational", false, false, "OPERATIONAL", "⚫"},
		{"stopped empty state", false, false, "", "⚫"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := GetRigLED(tt.hasWitness, tt.hasRefinery, tt.opState)
			if got != tt.want {
				t.Errorf("GetRigLED(%v, %v, %q) = %q, want %q",
					tt.hasWitness, tt.hasRefinery, tt.opState, got, tt.want)
			}
		})
	}
}
