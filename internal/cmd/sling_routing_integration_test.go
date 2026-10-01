//go:build integration

package cmd

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// TestIntegrationSlingRoutesNewRigBeadToTargetRig runs a whole gt sling of a
// bead just created in a rig against a logging bd and requires every bd
// command that touches the bead (create, target check, formula show, cook,
// bond, hook, metadata) to run pinned to the rig's database. The routing
// spans a dozen helpers, so only a whole run can see it.
func TestIntegrationSlingRoutesNewRigBeadToTargetRig(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows: shell stub redacts multiline descriptions")
	}
	beads.ResetBdAllowStaleCacheForTest()
	t.Cleanup(beads.ResetBdAllowStaleCacheForTest)

	townRoot := t.TempDir()
	newBeadID := "gt-new123"

	// Minimal workspace marker so workspace.FindFromCwd() succeeds.
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor", "rig"), 0755); err != nil {
		t.Fatalf("mkdir mayor/rig: %v", err)
	}

	// Create a rig path that owns gt-* beads, and a routes.jsonl pointing to it.
	rigDir := filepath.Join(townRoot, "gastown", "mayor", "rig")
	if err := os.MkdirAll(filepath.Join(townRoot, ".beads"), 0755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0755); err != nil {
		t.Fatalf("mkdir rigDir: %v", err)
	}
	routes := strings.Join([]string{
		`{"prefix":"gt-","path":"gastown/mayor/rig"}`,
		`{"prefix":"hq-","path":"."}`,
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(townRoot, ".beads", "routes.jsonl"), []byte(routes), 0644); err != nil {
		t.Fatalf("write routes.jsonl: %v", err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, ".beads", "metadata.json"), []byte(`{"dolt_database":"hq","dolt_server_host":"127.0.0.1","dolt_server_port":3307}`), 0644); err != nil {
		t.Fatalf("write town metadata: %v", err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "metadata.json"), []byte(`{"dolt_database":"gastown","dolt_server_host":"127.0.0.2","dolt_server_port":4407}`), 0644); err != nil {
		t.Fatalf("write rig metadata: %v", err)
	}

	// Stub bd so we can observe that a newly-created rig bead's formula,
	// hook, and metadata writes all resolve to the target rig database.
	binDir := filepath.Join(townRoot, "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatalf("mkdir binDir: %v", err)
	}
	logPath := filepath.Join(townRoot, "bd.log")
	bdScript := `#!/bin/sh
set -e
log_args=""
for arg in "$@"; do
  case "$arg" in
    --description=*attached_molecule:*gt-wisp-xyz*attached_formula:*mol-polecat-work*) arg="--description=<attached-molecule-and-formula-fields>" ;;
    --description=*no_merge:*true*review_only:*true*) arg="--description=<review-only-fields>" ;;
    --description=*review_only:*true*no_merge:*true*) arg="--description=<review-only-fields>" ;;
    --description=*) arg="--description=<redacted>" ;;
  esac
  log_args="${log_args}${log_args:+ }${arg}"
done
printf '%s|%s|%s|%s|%s|%s|%s|%s\n' "$(pwd)" "${BEADS_DIR:-}" "${BEADS_DOLT_SERVER_DATABASE:-}" "${BEADS_DB:-}" "${BD_DB:-}" "${BEADS_DOLT_DATA_DIR:-}" "${GT_DOLT_DATA:-}" "$log_args" >> "${BD_LOG}"
cmd="$1"
shift || true
while [ "$cmd" = "--db" ] || [ "$cmd" = "--allow-stale" ]; do
  if [ "$cmd" = "--db" ]; then
    shift || true
  fi
  cmd="$1"
  shift || true
done
case "$cmd" in
  show)
    echo '[{"title":"Test issue","status":"open","assignee":"","description":""}]'
    ;;
  create)
    echo '{"id":"gt-new123","title":"New sling smoke","status":"open","assignee":""}'
    ;;
  formula)
    # formula show <name> - must output something for verifyFormulaExists
    echo '{"name":"test-formula"}'
    exit 0
    ;;
  cook)
    echo '{"schema_version":1,"contract_version":1,"data":{"formula":"mol-polecat-work","vars":[],"steps":[]},"error":null}'
    ;;
	  mol)
		sub="$1"
		shift || true
		case "$sub" in
		  wisp)
			echo 'legacy mol wisp should not be called' >&2
			exit 1
			;;
		  bond)
			echo '{"result_id":"gt-abc123","id_mapping":{"mol-polecat-work":"gt-wisp-xyz"}}'
			;;
		esac
    ;;
  update)
    exit 0
    ;;
esac
exit 0
`
	bdScriptWindows := `@echo off
setlocal enableextensions
echo %CD%^|%BEADS_DIR%^|%BEADS_DOLT_SERVER_DATABASE%^|%BEADS_DB%^|%BD_DB%^|%BEADS_DOLT_DATA_DIR%^|%GT_DOLT_DATA%^|%*>>"%BD_LOG%"
set "cmd=%1"
set "sub=%2"
if "%cmd%"=="--allow-stale" (
  set "cmd=%2"
  set "sub=%3"
)
if "%cmd%"=="show" (
  echo [{"title":"Test issue","status":"open","assignee":"","description":""}]
  exit /b 0
)
if "%cmd%"=="create" (
  echo {"id":"gt-new123","title":"New sling smoke","status":"open","assignee":""}
  exit /b 0
)
if "%cmd%"=="formula" (
  echo {"name":"test-formula"}
  exit /b 0
)
if "%cmd%"=="cook" (
  echo {"schema_version":1,"contract_version":1,"data":{"formula":"mol-polecat-work","vars":[],"steps":[]},"error":null}
  exit /b 0
)
if "%cmd%"=="mol" (
  if "%sub%"=="wisp" (
    echo legacy mol wisp should not be called 1>&2
    exit /b 1
  )
  if "%sub%"=="bond" (
    echo {"result_id":"gt-abc123","id_mapping":{"mol-polecat-work":"gt-wisp-xyz"}}
    exit /b 0
  )
)
exit /b 0
`
	_ = writeBDStub(t, binDir, bdScript, bdScriptWindows)

	t.Setenv("BD_LOG", logPath)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(EnvGTRole, "mayor")
	t.Setenv("GT_POLECAT", "")
	t.Setenv("GT_CREW", "")
	t.Setenv("TMUX_PANE", "") // Prevent inheriting real tmux pane from test runner

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	if err := os.Chdir(filepath.Join(townRoot, "mayor", "rig")); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	// Ensure we don't leak global flag state across tests.
	prevOn := slingOnTarget
	prevVars := slingVars
	prevDryRun := slingDryRun
	prevNoConvoy := slingNoConvoy
	prevHookRawBead := slingHookRawBead
	prevReviewOnly := slingReviewOnly
	prevNoMerge := slingNoMerge
	prevResolveTargetAgent := resolveTargetAgentFn
	t.Cleanup(func() {
		slingOnTarget = prevOn
		slingVars = prevVars
		slingDryRun = prevDryRun
		slingNoConvoy = prevNoConvoy
		slingHookRawBead = prevHookRawBead
		slingReviewOnly = prevReviewOnly
		slingNoMerge = prevNoMerge
		resolveTargetAgentFn = prevResolveTargetAgent
	})

	slingDryRun = false
	slingNoConvoy = true
	slingHookRawBead = false
	slingReviewOnly = false
	slingNoMerge = false
	slingVars = nil
	slingOnTarget = ""
	resolveTargetAgentFn = func(target string) (agentID string, pane string, hookRoot string, err error) {
		if target != "gastown/polecats/toast" {
			t.Fatalf("resolveTargetAgent target = %q, want gastown/polecats/toast", target)
		}
		return "gastown/polecats/toast", "", filepath.Join(townRoot, "gastown", "polecats", "toast", "gastown"), nil
	}

	// Prevent real tmux nudge from firing during tests (causes agent self-interruption)
	t.Setenv("GT_TEST_NO_NUDGE", "1")
	t.Setenv("GT_TEST_SKIP_HOOK_VERIFY", "1") // Stub bd doesn't track state
	// Poison the ambient beads target: all mutating commands must override this
	// with the route-resolved target rig database.
	t.Setenv("BEADS_DIR", filepath.Join(townRoot, ".beads"))
	t.Setenv("BEADS_DOLT_SERVER_DATABASE", "hq")
	t.Setenv("BEADS_DB", filepath.Join(townRoot, "wrong.db"))
	t.Setenv("BD_DB", filepath.Join(townRoot, "wrong.bd"))
	t.Setenv("BEADS_DOLT_DATA_DIR", filepath.Join(townRoot, "wrong-data"))
	t.Setenv("GT_DOLT_DATA", filepath.Join(townRoot, "wrong-gt-data"))

	createOut, err := BdCmd("create", "--json", "--title=New sling smoke", "--type=task").
		Dir(rigDir).
		Output()
	if err != nil {
		t.Fatalf("create new rig bead: %v", err)
	}
	if !strings.Contains(string(createOut), newBeadID) {
		t.Fatalf("created bead output = %q, want %s", createOut, newBeadID)
	}

	if err := runSling(nil, []string{newBeadID, "gastown/polecats/toast"}); err != nil {
		t.Fatalf("runSling: %v", err)
	}

	// Also exercise the explicit formula-on-bead path; this is the older
	// --on-style route that must use the same target rig database.
	slingOnTarget = newBeadID
	if err := runSling(nil, []string{"mol-review"}); err != nil {
		t.Fatalf("runSling: %v", err)
	}

	slingOnTarget = ""
	slingHookRawBead = true
	slingReviewOnly = true
	slingNoMerge = true
	if err := runSling(nil, []string{newBeadID, "gastown/polecats/toast"}); err != nil {
		t.Fatalf("runSling raw review-only: %v", err)
	}

	logBytes, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read bd log: %v", err)
	}
	logLines := strings.Split(strings.TrimSpace(string(logBytes)), "\n")

	wantDir := rigDir
	if resolved, err := filepath.EvalSymlinks(wantDir); err == nil {
		wantDir = resolved
	}
	wantBeadsDir := filepath.Join(rigDir, ".beads")
	if resolved, err := filepath.EvalSymlinks(wantBeadsDir); err == nil {
		wantBeadsDir = resolved
	}
	gotPolecatCook := false
	gotReviewCook := false
	gotBondCount := 0
	gotCreate := false
	gotTargetDBCheck := false
	gotFormulaShow := false
	gotHook := false
	gotMetadata := false
	gotReviewOnlyMetadata := false
	assertTargetRig := func(kind, dir, beadsDir, database, beadsDB, bdDB, dataDir, gtData, args string) {
		t.Helper()
		if dir != wantDir {
			t.Fatalf("bd %s ran in %q, want %q (args: %q)", kind, dir, wantDir, args)
		}
		if beadsDir != wantBeadsDir {
			t.Fatalf("bd %s used BEADS_DIR %q, want %q (args: %q)", kind, beadsDir, wantBeadsDir, args)
		}
		if database != "gastown" {
			t.Fatalf("bd %s used BEADS_DOLT_SERVER_DATABASE %q, want gastown (args: %q)", kind, database, args)
		}
		if beadsDB != "" || bdDB != "" || dataDir != "" {
			t.Fatalf("bd %s leaked stale DB env BEADS_DB=%q BD_DB=%q BEADS_DOLT_DATA_DIR=%q (args: %q)", kind, beadsDB, bdDB, dataDir, args)
		}
		if gtData != "" {
			t.Fatalf("bd %s leaked GT_DOLT_DATA=%q (args: %q)", kind, gtData, args)
		}
	}

	firstReviewOnlyMetadataIndex := -1
	lastHookIndex := -1
	for i, line := range logLines {
		parts := strings.SplitN(line, "|", 8)
		if len(parts) != 8 {
			t.Fatalf("malformed bd log line: %q", line)
		}
		dir := parts[0]
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			dir = resolved
		}
		beadsDir := parts[1]
		if resolved, err := filepath.EvalSymlinks(beadsDir); err == nil {
			beadsDir = resolved
		}
		database := parts[2]
		beadsDB := parts[3]
		bdDB := parts[4]
		dataDir := parts[5]
		gtData := parts[6]
		args := parts[7]

		switch {
		case strings.Contains(args, "create "):
			gotCreate = true
			assertTargetRig("create", dir, beadsDir, database, beadsDB, bdDB, dataDir, gtData, args)
		case strings.Contains(args, "show "+newBeadID) && strings.Contains(args, "--json"):
			gotTargetDBCheck = true
			assertTargetRig("target DB check", dir, beadsDir, database, beadsDB, bdDB, dataDir, gtData, args)
		case strings.Contains(args, "sql SELECT DISTINCT wisp_dependencies.issue_id"):
			assertTargetRig("molecule dep check", dir, beadsDir, database, beadsDB, bdDB, dataDir, gtData, args)
		case strings.Contains(args, "formula show "):
			gotFormulaShow = true
			assertTargetRig("formula show", dir, beadsDir, database, beadsDB, bdDB, dataDir, gtData, args)
		case strings.Contains(args, "cook "):
			switch {
			case strings.Contains(args, "mol-polecat-work"):
				gotPolecatCook = true
			case strings.Contains(args, "mol-review"):
				gotReviewCook = true
			default:
				t.Fatalf("bd cook args = %q, want expected formula", args)
			}
			assertTargetRig("cook", dir, beadsDir, database, beadsDB, bdDB, dataDir, gtData, args)
		case strings.Contains(args, "mol bond "):
			gotBondCount++
			if !strings.Contains(args, "--ephemeral") {
				t.Fatalf("bd mol bond args = %q, want --ephemeral", args)
			}
			assertTargetRig("mol bond", dir, beadsDir, database, beadsDB, bdDB, dataDir, gtData, args)
		case strings.Contains(args, "update "+newBeadID) && strings.Contains(args, "--status=hooked"):
			gotHook = true
			lastHookIndex = i
			assertTargetRig("hook update", dir, beadsDir, database, beadsDB, bdDB, dataDir, gtData, args)
		case strings.Contains(args, "update "+newBeadID) && strings.Contains(args, "--description=<attached-molecule-and-formula-fields>"):
			gotMetadata = true
			assertTargetRig("metadata update", dir, beadsDir, database, beadsDB, bdDB, dataDir, gtData, args)
		case strings.Contains(args, "update "+newBeadID) && strings.Contains(args, "--description=<review-only-fields>"):
			gotReviewOnlyMetadata = true
			if firstReviewOnlyMetadataIndex == -1 {
				firstReviewOnlyMetadataIndex = i
			}
			assertTargetRig("review-only metadata update", dir, beadsDir, database, beadsDB, bdDB, dataDir, gtData, args)
		case strings.Contains(args, "update "+newBeadID) && strings.Contains(args, "--description="):
			assertTargetRig("description update", dir, beadsDir, database, beadsDB, bdDB, dataDir, gtData, args)
		case args == "--version" || strings.HasPrefix(args, "version") || strings.Contains(args, " version") || strings.Contains(args, "show gt-rig-") || strings.Contains(args, "show mol-"):
			// Explicitly exempt non-target-bead lookups; every gt-new123 operation
			// above must still prove it is pinned to the gastown database.
		default:
			t.Fatalf("unexpected bd command without routing assertion: %q", line)
		}
	}

	if !gotCreate || !gotTargetDBCheck || !gotFormulaShow || !gotPolecatCook || !gotReviewCook || gotBondCount < 2 || !gotHook || !gotMetadata || !gotReviewOnlyMetadata {
		t.Fatalf("missing expected bd commands: create=%v targetDBCheck=%v formulaShow=%v polecatCook=%v reviewCook=%v bondCount=%d hook=%v metadata=%v reviewOnlyMetadata=%v (log: %q)",
			gotCreate, gotTargetDBCheck, gotFormulaShow, gotPolecatCook, gotReviewCook, gotBondCount, gotHook, gotMetadata, gotReviewOnlyMetadata, string(logBytes))
	}
	if firstReviewOnlyMetadataIndex == -1 || lastHookIndex == -1 || firstReviewOnlyMetadataIndex > lastHookIndex {
		t.Fatalf("review-only metadata must be stored before raw hook assignment: metadataIndex=%d hookIndex=%d log: %q", firstReviewOnlyMetadataIndex, lastHookIndex, string(logBytes))
	}
}
