//go:build integration

package done

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestIntegrationRunAmbiguityErrorWritesCleanupStatus: gt done stops when the hook holds
// two beads and no --issue says which, and the stop still records the
// worktree's cleanup_status on the agent bead. The slot's reclaim reads that
// field and cannot repair a missing one (hq-vx224).
//
// Run needs real git (the unit tier runs none) and a bd on PATH; the stub
// logs every call and answers the three reads Run makes before it stops.
func TestIntegrationRunAmbiguityErrorWritesCleanupStatus(t *testing.T) {
	townRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(townRoot, "mayor", "town.json"), "{}\n")

	workDir := filepath.Join(townRoot, "gastown", "polecats", "refuge", "gastown")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	runGitCmd(t, "", "init", "-b", "main", workDir)
	gitOut(t, workDir, "config", "user.email", "polecat@example.com")
	gitOut(t, workDir, "config", "user.name", "Polecat")
	commitFile(t, workDir, "README.md", "base")
	// A branch that names no issue, so only the hook can supply one.
	gitOut(t, workDir, "switch", "-c", "fix/add-retry")

	binDir := t.TempDir()
	logDir := t.TempDir()
	logPath := filepath.Join(logDir, "bd.log")
	// bodyPath collects the descriptions bd update reads from stdin.
	bodyPath := filepath.Join(logDir, "update-bodies")
	script := fmt.Sprintf(`#!/bin/sh
if [ "$1" = "--allow-stale" ]; then shift; fi
if [ "$1" = "version" ]; then echo "bd stub"; exit 0; fi
printf '%%s\n' "$*" >> %q
case "$1" in
show)
  echo '[{"id":"'"$2"'","title":"Polecat refuge","status":"open","issue_type":"agent","labels":["gt:agent"]}]'
  ;;
list)
  echo '[{"id":"gt-one","title":"one","status":"hooked","assignee":"gastown/polecats/refuge"},{"id":"gt-two","title":"two","status":"hooked","assignee":"gastown/polecats/refuge"}]'
  ;;
update)
  cat >> %q
  ;;
*)
  echo '[]'
  ;;
esac
`, logPath, bodyPath)
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	env := map[string]string{
		"GT_TOWN_ROOT": townRoot,
		"GT_ROLE":      "gastown/polecats/refuge",
		"GT_RIG":       "gastown",
		"GT_POLECAT":   "refuge",
		"BD_ACTOR":     "gastown/polecats/refuge",
	}
	err = Run(Options{
		Status: ExitCompleted,
		Env:    func(k string) string { return env[k] },
		Getwd:  func() (string, error) { return workDir, nil },
	})
	if err == nil || !strings.Contains(err.Error(), "multiple active assignments") {
		t.Fatalf("Run = %v, want the ambiguity error", err)
	}

	log, _ := os.ReadFile(logPath)
	bodies, _ := os.ReadFile(bodyPath)
	if !strings.Contains(string(log), "update gt-gastown-polecat-refuge") || !strings.Contains(string(bodies), "cleanup_status") {
		t.Errorf("the ambiguity stop wrote no cleanup_status to the agent bead; bd calls:\n%s\nupdate bodies:\n%s", log, bodies)
	}
}
