//go:build integration

package cmd

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

type hookShowJSON struct {
	Agent  string `json:"agent"`
	BeadID string `json:"bead_id"`
	Status string `json:"status"`
}

// TestIntegrationHookShowShorthandResolvesToCanonical verifies that hook show accepts
// shorthand polecat targets (rig/name) and resolves them to canonical
// assignee IDs (rig/polecats/name) before querying hooked work.
func TestIntegrationHookShowShorthandResolvesToCanonical(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("bd"); err != nil {
		t.Skip("bd not installed, skipping integration test")
	}

	townRoot, polecatDir, rigPrefix := setupHookTestTown(t)

	rigDir := filepath.Join(polecatDir, "..", "..", "mayor", "rig")
	initBeadsDBWithPrefix(t, rigDir, rigPrefix)
	rigRootBeadsDir := filepath.Join(townRoot, "gastown", ".beads")
	if err := os.MkdirAll(rigRootBeadsDir, 0755); err != nil {
		t.Fatalf("mkdir stale rig-root beads dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(rigRootBeadsDir, "metadata.json"), []byte(`{"dolt_database":"hq"}`), 0644); err != nil {
		t.Fatalf("write stale rig-root metadata: %v", err)
	}

	// The stale-routing guards go in the gt subprocess's environment, not this
	// process's: t.Parallel forbids t.Setenv, and every bd/gt call the test
	// drives is a subprocess anyway. StripEnvKey first, so a value the host
	// already exports cannot win the child's getenv.
	env := os.Environ()
	hostile := []string{
		"BEADS_DIR=" + filepath.Join(townRoot, ".beads"),
		"BEADS_DOLT_SERVER_DATABASE=hq",
		"BEADS_DB=" + filepath.Join(townRoot, "wrong.db"),
		"BD_DB=" + filepath.Join(townRoot, "wrong.bd"),
		"BEADS_DOLT_DATA_DIR=" + filepath.Join(townRoot, "wrong-data"),
	}
	for _, kv := range hostile {
		k, _, _ := strings.Cut(kv, "=")
		env = beads.StripEnvKey(env, k)
	}
	env = append(env, hostile...)

	b := beads.New(rigDir)
	issue, err := b.Create(beads.CreateOptions{
		Title:    "Hook show target normalization test",
		Type:     "task",
		Priority: 2,
	})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}

	hooked := beads.StatusHooked
	assignee := "gastown/polecats/toast"
	if err := b.Update(issue.ID, beads.UpdateOptions{
		Status:   &hooked,
		Assignee: &assignee,
	}); err != nil {
		t.Fatalf("hook issue: %v", err)
	}

	gtBinary := buildGT(t)
	runShow := func(target string) hookShowJSON {
		// cmd.Dir is the polecat worktree, the same cwd runHookShow saw when it
		// was called in-process; the redirect there points at the rig database.
		out := runGTCmdOutput(t, gtBinary, polecatDir, env, "hook", "show", target, "--json")
		var parsed hookShowJSON
		if err := json.Unmarshal([]byte(out), &parsed); err != nil {
			t.Fatalf("parse `gt hook show %s --json` output %q: %v", target, out, err)
		}
		return parsed
	}

	canonical := runShow("gastown/polecats/toast")
	if canonical.BeadID != issue.ID || canonical.Status != beads.StatusHooked {
		t.Fatalf("canonical target mismatch: got bead=%q status=%q, want bead=%q status=%q",
			canonical.BeadID, canonical.Status, issue.ID, beads.StatusHooked)
	}

	shorthand := runShow("gastown/toast")
	if shorthand.BeadID != issue.ID || shorthand.Status != beads.StatusHooked {
		t.Fatalf("shorthand target mismatch: got bead=%q status=%q, want bead=%q status=%q",
			shorthand.BeadID, shorthand.Status, issue.ID, beads.StatusHooked)
	}
	if shorthand.Agent != "gastown/polecats/toast" {
		t.Fatalf("shorthand target did not normalize: got agent=%q, want %q",
			shorthand.Agent, "gastown/polecats/toast")
	}

	inProgress := "in_progress"
	if err := b.Update(issue.ID, beads.UpdateOptions{Status: &inProgress}); err != nil {
		t.Fatalf("mark issue in progress: %v", err)
	}
	active := runShow("gastown/toast")
	if active.BeadID != issue.ID || active.Status != "in_progress" {
		t.Fatalf("in-progress target mismatch: got bead=%q status=%q, want bead=%q status=in_progress",
			active.BeadID, active.Status, issue.ID)
	}
}
