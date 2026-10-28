//go:build integration || e2e_agent

package cmd

// Shared helpers for scheduler integration and e2e_agent tests: they run the
// real gt and bd binaries, taking explicit paths and env slices so callers
// control isolation.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/testutil"
)

// --- Environment helpers ---

// cleanSchedulerTestEnv returns os.Environ() with GT_*/BD_* variables removed
// (except GT_DOLT_PORT and BEADS_DOLT_PORT) and HOME overridden to tmpHome.
// This isolates gt/bd processes from the host while preserving test Dolt routing.
func cleanSchedulerTestEnv(tmpHome string) []string {
	return testutil.CleanGTEnv("HOME=" + tmpHome)
}

// gtTestCommand builds a gt subprocess bounded by gtTestCmdTimeout. On timeout
// gt gets SIGQUIT, so its goroutine dump lands in the failure's stderr, and
// os.Kill if it is still running WaitDelay later.
func gtTestCommand(binary, dir string, env, args []string) (*exec.Cmd, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), gtTestCmdTimeout)
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGQUIT) }
	cmd.WaitDelay = 10 * time.Second
	return cmd, cancel
}

// runGTCmdOutput runs a gt command and returns stdout only.
// Fails the test if the command exits non-zero.
func runGTCmdOutput(t *testing.T, binary, dir string, env []string, args ...string) string {
	t.Helper()
	cmd, cancel := gtTestCommand(binary, dir, env, args)
	defer cancel()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("gt %v failed: %v\nstdout:\n%s\nstderr:\n%s", args, err, out, stderr.String())
	}
	return string(out)
}

// runGTCmdMayFail runs a gt command and returns combined output and any error.
// Does NOT fail the test on non-zero exit.
func runGTCmdMayFail(t *testing.T, binary, dir string, env []string, args ...string) (string, error) {
	t.Helper()
	cmd, cancel := gtTestCommand(binary, dir, env, args)
	defer cancel()
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// --- Scheduler query helpers ---

// getSchedulerStatus runs `gt scheduler status --json` and returns the parsed output.
func getSchedulerStatus(t *testing.T, gtBinary, dir string, env []string) map[string]interface{} {
	t.Helper()
	out := runGTCmdOutput(t, gtBinary, dir, env, "scheduler", "status", "--json")
	var result map[string]interface{}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("parse scheduler status JSON: %v\nraw: %s", err, out)
	}
	return result
}

// getSchedulerList runs `gt scheduler list --json` and returns the parsed output.
func getSchedulerList(t *testing.T, gtBinary, dir string, env []string) []map[string]interface{} {
	t.Helper()
	out := runGTCmdOutput(t, gtBinary, dir, env, "scheduler", "list", "--json")
	var result []map[string]interface{}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("parse scheduler list JSON: %v\nraw: %s", err, out)
	}
	return result
}

// --- Bead helpers ---

// createTestBead creates a bead with the given title and returns the
// auto-generated bead ID.
func createTestBead(t *testing.T, dir, title string) string {
	t.Helper()
	issue, err := beads.NewPlain(dir, nil).Create(beads.CreateOptions{
		Title:       title,
		Labels:      []string{"gt:task"},
		Description: "Integration test bead",
		Priority:    -1,
	})
	if err != nil {
		t.Fatalf("bd create failed: %v\n%s", err, bdErrOutput(err))
	}
	if issue.ID == "" {
		t.Fatalf("bd create returned empty ID")
	}
	return issue.ID
}

// beadHasLabel checks whether a bead has the specified label.
func beadHasLabel(t *testing.T, beadID, label, dir string) bool {
	t.Helper()
	issue, err := beads.NewPlain(dir, nil).Show(beadID)
	if err != nil {
		t.Fatalf("bd show %s failed: %v", beadID, err)
	}
	return slices.Contains(issue.Labels, label)
}

// getBeadDescription returns the description of a bead.
func getBeadDescription(t *testing.T, beadID, dir string) string {
	t.Helper()
	issue, err := beads.NewPlain(dir, nil).Show(beadID)
	if err != nil {
		t.Fatalf("bd show %s failed: %v\n%s", beadID, err, bdErrOutput(err))
	}
	return issue.Description
}

// addBeadDependency adds a blocking dependency: blocker blocks blocked.
func addBeadDependency(t *testing.T, blocked, blocker, dir string) {
	t.Helper()
	if err := beads.NewPlain(dir, nil).AddDependency(blocked, blocker); err != nil {
		t.Fatalf("bd dep add %s %s failed: %v\n%s", blocked, blocker, err, bdErrOutput(err))
	}
}

// addBeadDependencyOfType adds a dependency with a specific type (e.g., "tracks",
// "parent-child"). The from bead must exist in the local DB at dir; the to bead
// can be in a different DB if routes.jsonl is present in dir's .beads/.
func addBeadDependencyOfType(t *testing.T, from, to, depType, dir string) {
	t.Helper()
	if err := beads.NewPlain(dir, nil).AddTypedDependency(from, to, depType); err != nil {
		t.Fatalf("bd dep add %s %s --type=%s failed: %v\n%s", from, to, depType, err, bdErrOutput(err))
	}
}

// addCrossRigEpicChild links an epic to a child that lives in another rig's
// database. A parent-child edge to a foreign bead ID cannot go in
// depends_on_issue_id (fk_dep_issue_target requires the row in the local
// issues table), and bd's own `dep add` refuses the external: form for
// parent-child ("external capability dependencies cannot use parent-child
// edges"). The edge therefore goes in as the external reference bd itself
// writes for cross-database targets, in the epic's own database, which is the
// shape the epic reader resolves (rawDepSQLArgs in convoy.go:
// COALESCE(depends_on_issue_id, depends_on_wisp_id, depends_on_external)).
//
// The write is a bare INSERT, so no typed method covers it; bd sql is the one
// verb whose rows are not JSON, and the seam's Run passes it through as is.
func addCrossRigEpicChild(t *testing.T, epicID, childID, dir string) {
	t.Helper()
	prefix := strings.TrimSuffix(beads.ExtractPrefix(childID), "-")
	query := fmt.Sprintf(
		"INSERT INTO dependencies (id, issue_id, type, created_by, depends_on_external) "+
			"VALUES (UUID(), '%s', 'parent-child', 'integration-test', 'external:%s:%s')",
		epicID, prefix, childID)
	if _, err := beads.NewPlain(dir, nil).Run("sql", query); err != nil {
		t.Fatalf("linking cross-rig child %s to epic %s failed: %v\n%s", childID, epicID, err, bdErrOutput(err))
	}
}

// createTestBeadOfType creates a bead with the given title and issue type (e.g.,
// "epic", "convoy", "task") and returns the auto-generated bead ID. Create
// carries the type as its gt:<type> label: detectSchedulerIDType
// (sling_schedule.go) reads an epic by that label as well as by its issue_type.
func createTestBeadOfType(t *testing.T, dir, title, issueType string) string {
	t.Helper()
	issue, err := beads.NewPlain(dir, nil).Create(beads.CreateOptions{
		Title:       title,
		Labels:      []string{"gt:" + issueType},
		Description: "Integration test bead",
		Priority:    -1,
	})
	if err != nil {
		t.Fatalf("bd create --type=%s failed: %v\n%s", issueType, err, bdErrOutput(err))
	}
	if issue.ID == "" {
		t.Fatalf("bd create returned empty ID")
	}
	return issue.ID
}

// slingToScheduler runs `gt sling <bead> <rig> --hook-raw-bead` in deferred mode.
// The test setup (configureScheduler) sets max_polecats > 0, so gt sling
// automatically defers dispatch without a --scheduler flag.
// Uses --hook-raw-bead to skip formula cooking (no formula infrastructure
// in integration tests).
func slingToScheduler(t *testing.T, gtBinary, dir string, env []string, beadID, rig string, extraFlags ...string) string {
	t.Helper()
	args := []string{"sling", beadID, rig, "--hook-raw-bead"}
	args = append(args, extraFlags...)
	return runGTCmdOutput(t, gtBinary, dir, env, args...)
}
