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

// localRoundRunResult is what one Run that the local-round gate refuses leaves
// behind: the beads the gate was asked to check and the bd calls Run made.
type localRoundRunResult struct {
	gated []string
	err   error
	log   string
}

// runRefusedByLocalRound runs gt done for a polecat hooked on gt-one, on a
// branch that names gt-one, with the local-round gate refusing and recording
// the bead it is asked about. Run needs real git and a bd on PATH; the stub logs
// every call and answers the reads Run makes before the gate.
func runRefusedByLocalRound(t *testing.T, issue string) localRoundRunResult {
	t.Helper()
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
	var res localRoundRunResult
	res.err = Run(Options{
		Status: ExitCompleted,
		Issue:  issue,
		Env:    func(k string) string { return env[k] },
		Getwd:  func() (string, error) { return workDir, nil },
		LocalRound: func(issueID, _ string) error {
			res.gated = append(res.gated, issueID)
			return errLocalRoundRefused
		},
	})
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("reading the bd log: %v: without it the no-write check passes vacuously", err)
	}
	res.log = string(log)
	return res
}

// errLocalRoundRefused is the refusal the stub gate returns.
var errLocalRoundRefused = errors.New("local round refused")

// TestIntegrationRunLocalRoundGateChecksTheResolvedBead: the local-round gate
// receives the one work bead Run resolves for the submit: the branch's bead
// unless --issue names another (gt-mxz8kx).
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
			res := runRefusedByLocalRound(t, tc.issue)
			if !errors.Is(res.err, errLocalRoundRefused) {
				t.Fatalf("Run = %v, want the local-round refusal", res.err)
			}
			if len(res.gated) != 1 || res.gated[0] != tc.wantBead {
				t.Fatalf("gate checked %q, want exactly %s: the submit must check the bead it submits", res.gated, tc.wantBead)
			}
		})
	}
}

// TestDoneLocalRoundWritesNothingOnRefusal: a submit the local-round gate
// refuses returns the refusal and writes neither the READY TO LAND block nor
// the ready-to-land label to the work bead (gt-mxz8kx).
func TestDoneLocalRoundWritesNothingOnRefusal(t *testing.T) {
	res := runRefusedByLocalRound(t, "")
	if !errors.Is(res.err, errLocalRoundRefused) {
		t.Fatalf("Run = %v, want the local-round refusal", res.err)
	}
	if strings.Contains(res.log, "ready-to-land") || strings.Contains(res.log, "update gt-one") {
		t.Errorf("a refused submit wrote the work bead; bd calls:\n%s", res.log)
	}
}
