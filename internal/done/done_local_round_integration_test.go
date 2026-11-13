//go:build integration

package done

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestIntegrationRunLocalRoundGateChecksTheResolvedBead: the local-round gate
// receives the one work bead Run resolves for the submit, and a refusal from it
// stops the submit before any label or READY TO LAND write (gt-mxz8kx). The
// seat is hooked on gt-one; the branch names gt-one unless --issue says
// otherwise, and the gate must see whichever bead the submit uses.
//
// Like TestIntegrationRunAmbiguityErrorWritesCleanupStatus, it needs real git
// and a bd on PATH; the stub logs every call and answers the reads Run makes.
func TestIntegrationRunLocalRoundGateChecksTheResolvedBead(t *testing.T) {
	tests := []struct {
		name     string
		issue    string
		wantBead string
	}{
		{name: "branch names the hooked bead", wantBead: "gt-one"},
		{name: "--issue names another bead", issue: "gt-two", wantBead: "gt-two"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
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
			gitOut(t, workDir, "switch", "-c", "polecat/refuge/gt-one+abc123")

			binDir := t.TempDir()
			logPath := filepath.Join(t.TempDir(), "bd.log")
			script := fmt.Sprintf(`#!/bin/sh
if [ "$1" = "--allow-stale" ]; then shift; fi
if [ "$1" = "version" ]; then echo "bd stub"; exit 0; fi
printf '%%s\n' "$*" >> %q
case "$1" in
show)
  echo '[{"id":"'"$2"'","title":"Polecat refuge","status":"open","issue_type":"agent","labels":["gt:agent"]}]'
  ;;
list)
  echo '[{"id":"gt-one","title":"one","status":"hooked","assignee":"gastown/polecats/refuge"}]'
  ;;
update)
  cat > /dev/null
  ;;
*)
  echo '[]'
  ;;
esac
`, logPath)
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
			var gated []string
			sentinel := errors.New("local round refused")
			err = Run(Options{
				Status: ExitCompleted,
				Issue:  tc.issue,
				Env:    func(k string) string { return env[k] },
				Getwd:  func() (string, error) { return workDir, nil },
				LocalRound: func(issueID, _ string) error {
					gated = append(gated, issueID)
					return sentinel
				},
			})
			if !errors.Is(err, sentinel) {
				t.Fatalf("Run = %v, want the local-round refusal", err)
			}
			if len(gated) != 1 || gated[0] != tc.wantBead {
				t.Fatalf("gate checked %q, want exactly %s: the submit must check the bead it submits", gated, tc.wantBead)
			}
			log, _ := os.ReadFile(logPath)
			if strings.Contains(string(log), "ready-to-land") || strings.Contains(string(log), "update "+tc.wantBead) {
				t.Errorf("a refused submit wrote the work bead; bd calls:\n%s", log)
			}
		})
	}
}
