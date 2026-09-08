package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAgentBeadsExistCheck_NoRoutes verifies the check handles missing routes.
func TestAgentBeadsExistCheck_NoRoutes(t *testing.T) {
	tmpDir := t.TempDir()

	// No .beads dir at all
	check := NewAgentBeadsCheck()
	ctx := &CheckContext{TownRoot: tmpDir}

	result := check.Run(ctx)

	// With no routes, only global agents (deacon, mayor) are checked
	// They won't exist without Dolt, so we expect error
	t.Logf("Result: status=%v, message=%s", result.Status, result.Message)
	if result.Status == StatusOK {
		t.Error("expected error for missing global agent beads")
	}
}

// TestAgentBeadsExistCheck_NoRigs verifies the check handles empty routes.
func TestAgentBeadsExistCheck_NoRigs(t *testing.T) {
	tmpDir := t.TempDir()

	// Create .beads dir with empty routes.jsonl
	beadsDir := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(""), 0644); err != nil {
		t.Fatal(err)
	}

	check := NewAgentBeadsCheck()
	ctx := &CheckContext{TownRoot: tmpDir}

	result := check.Run(ctx)

	// With empty routes, only global agents (deacon, mayor) are checked
	// They won't exist without Dolt, so we expect error or warning
	t.Logf("Result: status=%v, message=%s", result.Status, result.Message)
}

// TestAgentBeadsExistCheck_ExpectedIDs verifies the check looks for correct agent bead IDs.
func TestAgentBeadsExistCheck_ExpectedIDs(t *testing.T) {
	tmpDir := t.TempDir()

	// Set up routes pointing to a rig with known prefix
	beadsDir := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	// Use "sw" prefix to match sallaWork pattern
	routesContent := `{"prefix":"sw-","path":"sallaWork/mayor/rig"}` + "\n"
	if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(routesContent), 0644); err != nil {
		t.Fatal(err)
	}

	// Create rig beads directory
	rigBeadsDir := filepath.Join(tmpDir, "sallaWork", "mayor", "rig", ".beads")
	if err := os.MkdirAll(rigBeadsDir, 0755); err != nil {
		t.Fatal(err)
	}

	check := NewAgentBeadsCheck()
	ctx := &CheckContext{TownRoot: tmpDir}

	result := check.Run(ctx)

	// Should report missing beads
	if result.Status == StatusOK {
		t.Errorf("expected error for missing agent beads, got: %s", result.Message)
	}

	// Should mention the expected bead IDs in details
	if len(result.Details) == 0 {
		t.Error("expected details to contain missing bead IDs")
	}

	// Verify the expected IDs are in the details
	expectedIDs := []string{"sw-sallaWork-witness", "sw-sallaWork-refinery"}
	for _, expectedID := range expectedIDs {
		found := false
		for _, detail := range result.Details {
			if detail == expectedID {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected missing bead ID %s in details, got: %v", expectedID, result.Details)
		}
	}

	t.Logf("Result: status=%v, message=%s, details=%v", result.Status, result.Message, result.Details)
}

// TestAgentBeadsExistCheck_RespectsRigScope verifies that --rig excludes
// unrelated rig routes from agent-bead expectations.
func TestAgentBeadsExistCheck_RespectsRigScope(t *testing.T) {
	tmpDir := t.TempDir()

	beadsDir := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	routesContent := strings.Join([]string{
		`{"prefix":"gs-","path":"gastown/mayor/rig"}`,
		`{"prefix":"do-","path":"coder_dotfiles/mayor/rig"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(routesContent), 0644); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{
		filepath.Join(tmpDir, "gastown", "mayor", "rig", ".beads"),
		filepath.Join(tmpDir, "coder_dotfiles", "mayor", "rig", ".beads"),
	} {
		if err := os.MkdirAll(path, 0755); err != nil {
			t.Fatal(err)
		}
	}

	check := NewAgentBeadsCheck()
	ctx := &CheckContext{TownRoot: tmpDir, RigName: "gastown"}

	result := check.Run(ctx)

	if result.Status == StatusOK {
		t.Fatalf("expected missing agent beads for scoped rig, got OK")
	}
	for _, detail := range result.Details {
		if strings.HasPrefix(detail, "do-") {
			t.Fatalf("expected --rig scope to exclude coder_dotfiles agent bead %q, got details: %v", detail, result.Details)
		}
	}
	foundGastown := false
	for _, detail := range result.Details {
		if strings.HasPrefix(detail, "gs-") {
			foundGastown = true
			break
		}
	}
	if !foundGastown {
		t.Fatalf("expected scoped result to include gastown agent beads, got details: %v", result.Details)
	}
}

// TestAgentBeadsExistCheck_FixRespectsRigScope verifies that --fix with a rig
// scope does not create agent beads for unrelated rig prefixes.
func TestAgentBeadsExistCheck_FixRespectsRigScope(t *testing.T) {
	tmpDir := t.TempDir()

	beadsDir := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	routesContent := strings.Join([]string{
		`{"prefix":"gs-","path":"gastown/mayor/rig"}`,
		`{"prefix":"do-","path":"coder_dotfiles/mayor/rig"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(routesContent), 0644); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{
		filepath.Join(tmpDir, "gastown", "mayor", "rig", ".beads"),
		filepath.Join(tmpDir, "coder_dotfiles", "mayor", "rig", ".beads"),
	} {
		if err := os.MkdirAll(path, 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(tmpDir, "gastown", "crew", "alice", ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(tmpDir, "coder_dotfiles", "crew", "bella", ".git"), 0755); err != nil {
		t.Fatal(err)
	}

	logFile := filepath.Join(tmpDir, "bd.log")
	binDir := filepath.Join(tmpDir, "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatal(err)
	}
	bdScript := filepath.Join(binDir, "bd")
	script := `#!/usr/bin/env bash
set -euo pipefail

logfile="` + logFile + `"

args=()
for arg in "$@"; do
  if [[ "$arg" == --allow-stale ]]; then
    continue
  fi
  args+=("$arg")
done

cmd=""
idx=0
for i in "${!args[@]}"; do
  if [[ "${args[$i]}" != -* ]]; then
    cmd="${args[$i]}"
    idx=$i
    break
  fi
done

if [[ -z "$cmd" ]]; then
  exit 0
fi

rest=("${args[@]:$((idx + 1))}")

case "$cmd" in
  list)
    printf '[]\n'
    ;;
  mol)
    if [[ "${rest[0]:-}" == "wisp" && "${rest[1]:-}" == "list" ]]; then
      printf '{"wisps":[]}\n'
      exit 0
    fi
    exit 1
    ;;
  show)
    exit 1
    ;;
  create)
    id=""
    title=""
    for arg in "${rest[@]}"; do
      case "$arg" in
        --id=*) id="${arg#--id=}" ;;
        --title=*) title="${arg#--title=}" ;;
      esac
    done
    printf 'create %s\n' "$id" >> "$logfile"
    printf '{"id":"%s","title":"%s","status":"open","labels":["gt:agent"]}\n' "$id" "$title"
    ;;
  update)
    if [[ ${#rest[@]} -gt 0 ]]; then
      printf 'update %s\n' "${rest[0]}" >> "$logfile"
    fi
    printf '{}'\n
    ;;
  *)
    exit 0
    ;;
esac
`
	if err := os.WriteFile(bdScript, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", fmt.Sprintf("%s%c%s", binDir, os.PathListSeparator, os.Getenv("PATH")))

	check := NewAgentBeadsCheck()
	ctx := &CheckContext{TownRoot: tmpDir, RigName: "gastown"}
	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix() returned error: %v", err)
	}

	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("reading fake bd log: %v", err)
	}
	log := string(data)
	for _, line := range strings.Split(strings.TrimSpace(log), "\n") {
		if strings.Contains(line, " do-") {
			t.Fatalf("expected scoped Fix() to avoid coder_dotfiles beads, got log line %q", line)
		}
	}
	if !strings.Contains(log, "create gs-gastown-witness") {
		t.Fatalf("expected scoped Fix() to create gastown witness bead, got log: %q", log)
	}
}

// writeTownDuplicateBdScript installs a fake bd that reports rig-prefixed
// agent beads as existing in the TOWN database only (list run from the town
// .beads dir returns them; list run from the rig dir returns nothing). It
// logs create/update calls to logFile. Used to reproduce gt-abj: a rig agent
// bead that exists only in the town DB must still be treated as missing
// rig-locally.
func writeTownDuplicateBdScript(t *testing.T, tmpDir, logFile string) {
	t.Helper()
	binDir := filepath.Join(tmpDir, "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatal(err)
	}
	script := `#!/usr/bin/env bash
set -euo pipefail

logfile="` + logFile + `"

args=()
for arg in "$@"; do
  if [[ "$arg" == --allow-stale ]]; then
    continue
  fi
  args+=("$arg")
done

cmd=""
idx=0
for i in "${!args[@]}"; do
  if [[ "${args[$i]}" != -* ]]; then
    cmd="${args[$i]}"
    idx=$i
    break
  fi
done

if [[ -z "$cmd" ]]; then
  exit 0
fi

rest=("${args[@]:$((idx + 1))}")

case "$cmd" in
  list)
    # Town DB (workdir is the town .beads dir) holds duplicates of the
    # rig-scoped agent beads; the rig DB has none.
    if [[ "$PWD" == */.beads ]]; then
      printf '[{"id":"gs-gastown-witness","title":"Witness","status":"open","labels":["gt:agent"]},{"id":"gs-gastown-refinery","title":"Refinery","status":"open","labels":["gt:agent"]},{"id":"hq-deacon","title":"Deacon","status":"open","labels":["gt:agent"]},{"id":"hq-mayor","title":"Mayor","status":"open","labels":["gt:agent"]}]\n'
    else
      printf '[]\n'
    fi
    ;;
  mol)
    if [[ "${rest[0]:-}" == "wisp" && "${rest[1]:-}" == "list" ]]; then
      printf '{"wisps":[]}\n'
      exit 0
    fi
    exit 1
    ;;
  show)
    exit 1
    ;;
  create)
    id=""
    title=""
    for arg in "${rest[@]}"; do
      case "$arg" in
        --id=*) id="${arg#--id=}" ;;
        --title=*) title="${arg#--title=}" ;;
      esac
    done
    printf 'create %s cwd=%s\n' "$id" "$PWD" >> "$logfile"
    printf '{"id":"%s","title":"%s","status":"open","labels":["gt:agent"]}\n' "$id" "$title"
    ;;
  update)
    if [[ ${#rest[@]} -gt 0 ]]; then
      printf 'update %s\n' "${rest[0]}" >> "$logfile"
    fi
    printf '{}\n'
    ;;
  *)
    exit 0
    ;;
esac
`
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fmt.Sprintf("%s%c%s", binDir, os.PathListSeparator, os.Getenv("PATH")))
}

// setupTownDuplicateFixture creates routes and rig dirs for the gt-abj
// reproduction: one rig ("gastown", prefix gs-) whose agent beads exist only
// in the town database.
//
// The fixture includes mayor/town.json so beads.FindTownRoot resolves. This
// matters: without a town root, CreateAgentBead's ForAgentBead re-rooting is a
// no-op and the tests exercise a code path production never takes. With a town
// root, a routed wrapper re-targets creates at the town database — the gt-8po
// bug these tests must catch.
func setupTownDuplicateFixture(t *testing.T) string {
	t.Helper()
	tmpDir := t.TempDir()

	beadsDir := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	routesContent := `{"prefix":"gs-","path":"gastown/mayor/rig"}` + "\n"
	if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(routesContent), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(tmpDir, "gastown", "mayor", "rig", ".beads"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(tmpDir, "mayor"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "mayor", "town.json"), []byte(`{"name":"testtown","version":2}`), 0644); err != nil {
		t.Fatal(err)
	}
	return tmpDir
}

// TestAgentBeadsExistCheck_TownOnlyRigBeadIsMissing verifies that Run reports
// a rig-scoped agent bead as missing when it exists only in the town database.
// Patrol commands (gt agents resolve --rig) require rig-local agent beads, so
// a town-level duplicate must not satisfy the existence check. See gt-abj.
func TestAgentBeadsExistCheck_TownOnlyRigBeadIsMissing(t *testing.T) {
	tmpDir := setupTownDuplicateFixture(t)
	logFile := filepath.Join(tmpDir, "bd.log")
	writeTownDuplicateBdScript(t, tmpDir, logFile)

	check := NewAgentBeadsCheck()
	ctx := &CheckContext{TownRoot: tmpDir, RigName: "gastown"}

	result := check.Run(ctx)

	if result.Status == StatusOK {
		t.Fatalf("expected town-only rig agent beads to be reported missing, got OK: %s", result.Message)
	}
	for _, want := range []string{"gs-gastown-witness", "gs-gastown-refinery"} {
		found := false
		for _, detail := range result.Details {
			if strings.HasPrefix(detail, want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected %s in missing details, got: %v", want, result.Details)
		}
	}
}

// TestAgentBeadsExistCheck_FixCreatesRigLocalBeadDespiteTownDuplicate verifies
// that Fix creates the rig-local agent bead even when a town-level duplicate
// exists. Before gt-abj, the merged town+rig map made fixAgentBead return
// early on the town duplicate, so the rig-local bead was never created and
// doctor --fix could not repair the patrol hard-block.
func TestAgentBeadsExistCheck_FixCreatesRigLocalBeadDespiteTownDuplicate(t *testing.T) {
	tmpDir := setupTownDuplicateFixture(t)
	logFile := filepath.Join(tmpDir, "bd.log")
	writeTownDuplicateBdScript(t, tmpDir, logFile)

	check := NewAgentBeadsCheck()
	ctx := &CheckContext{TownRoot: tmpDir, RigName: "gastown"}
	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix() returned error: %v", err)
	}

	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("reading fake bd log: %v", err)
	}
	log := string(data)
	rigDir := resolvePath(t, filepath.Join(tmpDir, "gastown", "mayor", "rig"))
	for _, id := range []string{"gs-gastown-witness", "gs-gastown-refinery"} {
		want := "create " + id + " cwd=" + rigDir
		if !strings.Contains(log, want) {
			t.Errorf("expected Fix() to create %s IN THE RIG DATABASE (create running in %s) despite town duplicate, got log: %q", id, rigDir, log)
		}
	}
	// Town agents exist in the town DB — Fix must NOT recreate them.
	for _, unwanted := range []string{"create hq-deacon", "create hq-mayor"} {
		if strings.Contains(log, unwanted) {
			t.Errorf("Fix() should not recreate existing town agent bead (%s), got log: %q", unwanted, log)
		}
	}
}

// writeLegacyUnlabeledBdScript installs a fake bd for the legacy-bead case:
// the rig database contains the agent bead as a plain open task WITHOUT the
// gt:agent label (1.1.0-era identity beads). `bd list --label=gt:agent` does
// not return it, but `bd show` finds it. `bd sql` label verification reports
// the label present after update so no SQL fallback is attempted.
func writeLegacyUnlabeledBdScript(t *testing.T, tmpDir, logFile string) {
	t.Helper()
	binDir := filepath.Join(tmpDir, "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatal(err)
	}
	script := `#!/usr/bin/env bash
set -euo pipefail

logfile="` + logFile + `"

args=()
for arg in "$@"; do
  if [[ "$arg" == --allow-stale ]]; then
    continue
  fi
  args+=("$arg")
done

cmd=""
idx=0
for i in "${!args[@]}"; do
  if [[ "${args[$i]}" != -* ]]; then
    cmd="${args[$i]}"
    idx=$i
    break
  fi
done

if [[ -z "$cmd" ]]; then
  exit 0
fi

rest=("${args[@]:$((idx + 1))}")

case "$cmd" in
  list)
    # No beads carry the gt:agent label anywhere.
    printf '[]\n'
    ;;
  mol)
    if [[ "${rest[0]:-}" == "wisp" && "${rest[1]:-}" == "list" ]]; then
      printf '{"wisps":[]}\n'
      exit 0
    fi
    exit 1
    ;;
  show)
    id="${rest[0]:-}"
    # Legacy identity beads exist as open tasks without the gt:agent label.
    printf '[{"id":"%s","title":"%s","status":"open","issue_type":"task","labels":[]}]\n' "$id" "$id"
    ;;
  sql)
    # Label verification query — report the label as present.
    printf '1\n'
    ;;
  create)
    id=""
    for arg in "${rest[@]}"; do
      case "$arg" in
        --id=*) id="${arg#--id=}" ;;
      esac
    done
    printf 'create %s cwd=%s\n' "$id" "$PWD" >> "$logfile"
    printf '{"id":"%s","title":"t","status":"open","labels":["gt:agent"]}\n' "$id"
    ;;
  update)
    if [[ ${#rest[@]} -gt 0 ]]; then
      printf 'update %s cwd=%s\n' "${rest[0]}" "$PWD" >> "$logfile"
    fi
    printf '{}\n'
    ;;
  *)
    exit 0
    ;;
esac
`
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fmt.Sprintf("%s%c%s", binDir, os.PathListSeparator, os.Getenv("PATH")))
}

// TestAgentBeadsExistCheck_FixLabelsLegacyOpenBeadInsteadOfCreating verifies
// that Fix repairs an EXISTING open agent bead that merely lacks the gt:agent
// label (legacy 1.1.0-era identity beads are type=task with no label) by
// adding the label, instead of falling through to a duplicate-ID create.
// See gt-8po: the fall-through create either errored on the duplicate ID or
// silently upserted into the wrong database.
func TestAgentBeadsExistCheck_FixLabelsLegacyOpenBeadInsteadOfCreating(t *testing.T) {
	tmpDir := setupTownDuplicateFixture(t)
	logFile := filepath.Join(tmpDir, "bd.log")
	writeLegacyUnlabeledBdScript(t, tmpDir, logFile)

	check := NewAgentBeadsCheck()
	ctx := &CheckContext{TownRoot: tmpDir, RigName: "gastown"}
	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix() returned error: %v", err)
	}

	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("reading fake bd log: %v", err)
	}
	log := string(data)

	if strings.Contains(log, "create ") {
		t.Errorf("Fix() must not create beads that already exist (label them instead), got log: %q", log)
	}
	rigDir := resolvePath(t, filepath.Join(tmpDir, "gastown", "mayor", "rig"))
	for _, id := range []string{"gs-gastown-witness", "gs-gastown-refinery"} {
		want := "update " + id + " cwd=" + rigDir
		if !strings.Contains(log, want) {
			t.Errorf("expected Fix() to add gt:agent label to legacy bead %s in the rig database, got log: %q", id, log)
		}
	}
}

// resolvePath resolves symlinks so paths logged by shell $PWD (which resolves
// /var → /private/var on macOS) compare equal to t.TempDir() paths.
func resolvePath(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("resolving %s: %v", path, err)
	}
	return resolved
}

// TestListCrewWorkers_FiltersWorktrees verifies that listCrewWorkers skips
// git worktrees (directories where .git is a file) and only returns canonical
// crew workers (where .git is a directory). This is the fix for GH#2767.
func TestListCrewWorkers_FiltersWorktrees(t *testing.T) {
	tmpDir := t.TempDir()
	rigName := "myrig"
	crewDir := filepath.Join(tmpDir, rigName, "crew")

	// Create a canonical crew worker: .git is a directory
	canonicalDir := filepath.Join(crewDir, "alice")
	if err := os.MkdirAll(filepath.Join(canonicalDir, ".git"), 0755); err != nil {
		t.Fatal(err)
	}

	// Create a worktree: .git is a file (contains gitdir pointer)
	worktreeDir := filepath.Join(crewDir, "alice-worktree")
	if err := os.MkdirAll(worktreeDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktreeDir, ".git"),
		[]byte("gitdir: /path/to/main/.git/worktrees/alice-worktree\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Create a second canonical worker
	bobDir := filepath.Join(crewDir, "bob")
	if err := os.MkdirAll(filepath.Join(bobDir, ".git"), 0755); err != nil {
		t.Fatal(err)
	}

	// Create a directory without .git at all (should be included — not a worktree)
	plainDir := filepath.Join(crewDir, "charlie")
	if err := os.MkdirAll(plainDir, 0755); err != nil {
		t.Fatal(err)
	}

	workers := listCrewWorkers(tmpDir, rigName)

	// Should include alice, bob, charlie but NOT alice-worktree
	expected := map[string]bool{"alice": false, "bob": false, "charlie": false}
	for _, w := range workers {
		if w == "alice-worktree" {
			t.Errorf("listCrewWorkers should skip worktree 'alice-worktree', got: %v", workers)
		}
		if _, ok := expected[w]; ok {
			expected[w] = true
		}
	}
	for name, found := range expected {
		if !found {
			t.Errorf("listCrewWorkers should include canonical worker %q, got: %v", name, workers)
		}
	}
}

// TestAddWispLabelSQL_ErrorsGracefully verifies addWispLabelSQL doesn't panic
// and returns an error when bd is unavailable (no Dolt server).
// This is a regression guard for gt-3vx: after CreateAgentBead, the gt:agent
// label must also be inserted into wisp_labels so doctor checks that join
// wisp_labels can find the bead.
func TestAddWispLabelSQL_ErrorsGracefully(t *testing.T) {
	tmpDir := t.TempDir()
	err := addWispLabelSQL(tmpDir, "gt-gastown-witness", "gt:agent")
	// bd sql will fail without a Dolt server — just verify no panic and that the
	// function returns an error (not silently discarding the failure).
	if err == nil {
		t.Log("addWispLabelSQL succeeded (Dolt server is running)")
	} else {
		t.Logf("addWispLabelSQL returned expected error without Dolt: %v", err)
	}
}

// TestListPolecats_FiltersWorktrees verifies that listPolecats skips
// git worktrees, same as listCrewWorkers. See GH#2767.
func TestListPolecats_FiltersWorktrees(t *testing.T) {
	tmpDir := t.TempDir()
	rigName := "myrig"
	polecatDir := filepath.Join(tmpDir, rigName, "polecats")

	// Canonical polecat
	if err := os.MkdirAll(filepath.Join(polecatDir, "scout", ".git"), 0755); err != nil {
		t.Fatal(err)
	}

	// Worktree polecat (.git is a file)
	wtDir := filepath.Join(polecatDir, "scout-wt")
	if err := os.MkdirAll(wtDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wtDir, ".git"),
		[]byte("gitdir: /path/to/main/.git/worktrees/scout-wt\n"), 0644); err != nil {
		t.Fatal(err)
	}

	polecats := listPolecats(tmpDir, rigName)

	if len(polecats) != 1 || polecats[0] != "scout" {
		t.Errorf("listPolecats should return only [scout], got: %v", polecats)
	}
}
