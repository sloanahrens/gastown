//go:build integration

package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
)

func setupRoutedSubmitCommandTown(t *testing.T, workDir string) {
	t.Helper()
	townRoot := routedSourceTestTownRoot(workDir)
	rigsPath := filepath.Join(townRoot, "mayor", "rigs.json")
	if err := config.SaveRigsConfig(rigsPath, &config.RigsConfig{
		Version: config.CurrentRigsVersion,
		Rigs: map[string]config.RigEntry{
			"gastown": {GitURL: "file://test-gastown"},
		},
	}); err != nil {
		t.Fatalf("save rigs config: %v", err)
	}
}

func setupRoutedSubmitGitRepo(t *testing.T, workDir string, pushBranch bool) string {
	t.Helper()
	remote := t.TempDir()
	runGitForMQSubmitTest(t, remote, "init", "--bare")
	runGitForMQSubmitTest(t, workDir, "init")
	runGitForMQSubmitTest(t, workDir, "config", "user.email", "test@example.com")
	runGitForMQSubmitTest(t, workDir, "config", "user.name", "Test User")
	runGitForMQSubmitTest(t, workDir, "remote", "add", "origin", remote)
	writeMQSubmitTestFile(t, workDir, ".gitignore", ".beads/\n.runtime/\n")
	writeMQSubmitTestFile(t, workDir, "file.txt", "main\n")
	runGitForMQSubmitTest(t, workDir, "add", ".gitignore", "file.txt")
	runGitForMQSubmitTest(t, workDir, "commit", "-m", "main")
	runGitForMQSubmitTest(t, workDir, "branch", "-M", "main")
	runGitForMQSubmitTest(t, workDir, "push", "-u", "origin", "main")
	branch := "feature/routed-submit"
	runGitForMQSubmitTest(t, workDir, "checkout", "-b", branch)
	writeMQSubmitTestFile(t, workDir, "file.txt", "feature\n")
	runGitForMQSubmitTest(t, workDir, "commit", "-am", "feature")
	if pushBranch {
		runGitForMQSubmitTest(t, workDir, "push", "origin", branch)
	}
	return branch
}

func installSubmitSourceBDRecorder(t *testing.T, currentBeadsDir, ownerBeadsDir string) string {
	t.Helper()
	binDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "bd.log")
	script := fmt.Sprintf(`#!/bin/sh
if [ "$1" = "--allow-stale" ]; then
  shift
fi
if [ "$1" = "version" ]; then
  echo "bd stub"
  exit 0
fi
printf '%%s\t%%s\n' "$BEADS_DIR" "$*" >> %q
if [ "$1" = "update" ]; then
  if [ -n "$GT_TEST_BD_UPDATE_FAILS" ]; then
    echo "Error: database not found: gastown" >&2
    exit 1
  fi
  case "$*" in *--add-label=gt:ready-to-land*) : > %q.ready ;; esac
  exit 0
fi
if [ "$1" = "show" ] && [ "$2" = "gt-gastown-polecat-refuge" ]; then
  echo '[{"id":"gt-gastown-polecat-refuge","title":"Polecat refuge","status":"open","issue_type":"agent","labels":["gt:agent"]}]'
  exit 0
fi
labels='[]'
if [ -e %q.ready ]; then labels='["gt:ready-to-land"]'; fi
if [ "$1" = "show" ] && [ "$2" = "bd-source" ]; then
  if [ "$BEADS_DIR" = %q ]; then
    echo '[{"id":"bd-source","title":"current mirror","status":"open","priority":1,"issue_type":"task","labels":'"$labels"',"description":"convoy_id: hq-cv-test\\nmerge_strategy: mr"}]'
    exit 0
  fi
  if [ "$BEADS_DIR" = %q ]; then
    echo '[{"id":"bd-source","title":"owner source","status":"open","priority":1,"issue_type":"task","labels":'"$labels"',"description":"convoy_id: hq-cv-test\\nmerge_strategy: mr"}]'
    exit 0
  fi
  echo "Issue not found in $BEADS_DIR" >&2
  exit 1
fi
if [ "$1" = "show" ] && [ "$2" = "gt-mr" ]; then
  echo '[{"id":"gt-mr","title":"Merge: bd-source","status":"open","priority":1,"issue_type":"task","labels":["gt:merge-request"],"description":"branch: feature/routed-submit\\ntarget: main\\nsource_issue: bd-source\\nrig: gastown"}]'
  exit 0
fi
if [ "$1" = "list" ]; then
  echo '[]'
  exit 0
fi
if [ "$1" = "sql" ]; then
  echo '[]'
  exit 0
fi
if [ "$1" = "create" ]; then
  if [ -n "$GT_TEST_BD_CREATE_FAILS" ]; then
    echo "Error: database not found: gastown" >&2
    exit 1
  fi
  echo '{"id":"gt-mr","title":"Merge: bd-source","status":"open","priority":1,"issue_type":"task","labels":["gt:merge-request"]}'
  exit 0
fi
if [ "$1" = "comments" ] && [ "$2" = "add" ]; then
  exit 0
fi
if [ "$1" = "close" ]; then
  if [ -n "$GT_TEST_BD_CLOSE_FAILS" ]; then
    echo "Error: database not found: gastown" >&2
    exit 1
  fi
  exit 0
fi
echo "unexpected bd command: $*" >&2
exit 1
`, logPath, logPath, logPath, currentBeadsDir, ownerBeadsDir)
	path := filepath.Join(binDir, "bd")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write bd recorder: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	beads.ResetBdAllowStaleCacheForTest()
	t.Cleanup(beads.ResetBdAllowStaleCacheForTest)
	return logPath
}

func readSubmitSourceBDLog(t *testing.T, logPath string) string {
	t.Helper()
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read bd recorder log: %v", err)
	}
	return string(log)
}
