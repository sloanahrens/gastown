package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// TestExtractRoleFromIdentity verifies that role names are correctly extracted
// from agent identity strings, including trailing slashes and compound paths.
func TestExtractRoleFromIdentity(t *testing.T) {
	t.Parallel()
	tests := []struct {
		target string
		want   string
	}{
		{"gastown/refinery", "refinery"},
		{"gastown/witness", "witness"},
		{"gastown/crew/jack", "jack"},
		{"gastown/polecats/nux", "nux"},
		{"mayor/", "mayor"},
		{"deacon/", "deacon"},
		{"deacon/boot", "boot"},
		{"refinery", "refinery"},
	}
	for _, tc := range tests {
		got := extractRoleFromIdentity(tc.target)
		if got != tc.want {
			t.Errorf("extractRoleFromIdentity(%q) = %q, want %q", tc.target, got, tc.want)
		}
	}
}

// TestSlingFormulaOnBeadHooksBaseBead verifies that when using
// "gt sling <formula> --on <bead>", the BASE bead is hooked (not the wisp).
//
// Current bug: The code hooks the wisp (compound root) instead of the base bead.
// This causes lifecycle issues:
// - Base bead stays open after wisp completes
// - gt done closes wisp, not the actual work item
// - Orphaned base beads accumulate
//
// Expected behavior: Hook the base bead, store attached_molecule pointing to wisp.
// gt hook/gt prime can follow attached_molecule to find the workflow steps.
func TestSlingFormulaOnBeadHooksBaseBead(t *testing.T) {
	townRoot := t.TempDir()

	// Minimal workspace marker
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor", "rig"), 0755); err != nil {
		t.Fatalf("mkdir mayor/rig: %v", err)
	}

	// Create routes
	if err := os.MkdirAll(filepath.Join(townRoot, ".beads"), 0755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}
	rigDir := filepath.Join(townRoot, "gastown", "mayor", "rig")
	if err := os.MkdirAll(rigDir, 0755); err != nil {
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

	// Stub bd to track which bead gets hooked
	binDir := filepath.Join(townRoot, "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatalf("mkdir binDir: %v", err)
	}
	logPath := filepath.Join(townRoot, "bd.log")
	bdScript := `#!/bin/sh
set -e
echo "$*" >> "${BD_LOG}"
if [ "$1" = "--allow-stale" ]; then
  shift
fi
cmd="$1"
shift || true
case "$cmd" in
  show)
    # Return the base bead info
    echo '[{"id":"gt-abc123","title":"Bug to fix","status":"open","assignee":"","description":""}]'
    ;;
  formula)
    echo '{"name":"mol-polecat-work"}'
    ;;
  cook)
    exit 0
    ;;
  mol)
    sub="$1"
    shift || true
    case "$sub" in
      wisp)
        echo '{"new_epic_id":"gt-wisp-xyz"}'
        ;;
      bond)
        echo '{"root_id":"gt-wisp-xyz"}'
        ;;
    esac
    ;;
  update)
    # Just succeed
    exit 0
    ;;
esac
exit 0
`
	bdScriptWindows := `@echo off
setlocal enableextensions
echo %*>>"%BD_LOG%"
set "cmd=%1"
set "sub=%2"
if "%cmd%"=="--allow-stale" (
  set "cmd=%2"
  set "sub=%3"
)
if "%cmd%"=="show" (
  echo [{^"id^":^"gt-abc123^",^"title^":^"Bug to fix^",^"status^":^"open^",^"assignee^":^"^",^"description^":^"^"}]
  exit /b 0
)
if "%cmd%"=="formula" (
  echo {^"name^":^"mol-polecat-work^"}
  exit /b 0
)
if "%cmd%"=="cook" exit /b 0
if "%cmd%"=="mol" (
  if "%sub%"=="wisp" (
    echo {^"new_epic_id^":^"gt-wisp-xyz^"}
    exit /b 0
  )
  if "%sub%"=="bond" (
    echo {^"root_id^":^"gt-wisp-xyz^"}
    exit /b 0
  )
)
if "%cmd%"=="update" exit /b 0
exit /b 0
`
	_ = writeBDStub(t, binDir, bdScript, bdScriptWindows)

	t.Setenv("BD_LOG", logPath)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(EnvGTRole, "mayor")
	t.Setenv("GT_POLECAT", "")
	t.Setenv("GT_CREW", "")
	t.Setenv("TMUX_PANE", "")
	t.Setenv("GT_TEST_NO_NUDGE", "1")
	t.Setenv("GT_TEST_SKIP_HOOK_VERIFY", "1") // Stub bd doesn't track state

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	if err := os.Chdir(filepath.Join(townRoot, "mayor", "rig")); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	// Save and restore global flag state
	prevOn := slingOnTarget
	prevVars := slingVars
	prevDryRun := slingDryRun
	prevNoConvoy := slingNoConvoy
	t.Cleanup(func() {
		slingOnTarget = prevOn
		slingVars = prevVars
		slingDryRun = prevDryRun
		slingNoConvoy = prevNoConvoy
	})

	slingDryRun = false
	slingNoConvoy = true
	slingVars = nil
	slingOnTarget = "gt-abc123" // The base bead

	if err := runSling(nil, []string{"mol-polecat-work"}); err != nil {
		t.Fatalf("runSling: %v", err)
	}

	logBytes, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read bd log: %v", err)
	}

	// Find the update command that sets status=hooked
	// Expected: should hook gt-abc123 (base bead)
	// Current bug: hooks gt-wisp-xyz (wisp)
	logLines := strings.Split(string(logBytes), "\n")
	var hookedBeadID string
	for _, line := range logLines {
		if strings.Contains(line, "update") && strings.Contains(line, "--status=hooked") {
			// Extract the bead ID being hooked
			// Format: "update <beadID> --status=hooked ..."
			parts := strings.Fields(line)
			for i, part := range parts {
				if part == "update" && i+1 < len(parts) {
					hookedBeadID = parts[i+1]
					break
				}
			}
			break
		}
	}

	if hookedBeadID == "" {
		t.Fatalf("no hooked bead found in log:\n%s", string(logBytes))
	}

	// The BASE bead (gt-abc123) should be hooked, not the wisp (gt-wisp-xyz)
	if hookedBeadID != "gt-abc123" {
		t.Errorf("wrong bead hooked: got %q, want %q (base bead)\n"+
			"Current behavior hooks the wisp instead of the base bead.\n"+
			"This causes orphaned base beads when gt done closes only the wisp.\n"+
			"Log:\n%s", hookedBeadID, "gt-abc123", string(logBytes))
	}
}

// TestSlingFormulaOnBeadSetsAttachedMoleculeInBaseBead verifies that when using
// "gt sling <formula> --on <bead>", the attached_molecule field is set in the
// BASE bead's description (pointing to the wisp), not in the wisp itself.
//
// Current bug: attached_molecule is stored as a self-reference in the wisp.
// This is semantically meaningless (wisp points to itself) and breaks
// compound resolution from the base bead.
//
// Expected behavior: Store attached_molecule in the base bead pointing to wisp.
// This enables:
// - Compound resolution: base bead -> attached_molecule -> wisp
// - gt hook/gt prime: read base bead, follow attached_molecule to show wisp steps
func TestSlingFormulaOnBeadSetsAttachedMoleculeInBaseBead(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows batch script JSON output causes storeAttachedMoleculeInBead to fail silently")
	}
	townRoot := t.TempDir()

	// Minimal workspace marker
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor", "rig"), 0755); err != nil {
		t.Fatalf("mkdir mayor/rig: %v", err)
	}

	// Create routes
	if err := os.MkdirAll(filepath.Join(townRoot, ".beads"), 0755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}
	rigDir := filepath.Join(townRoot, "gastown", "mayor", "rig")
	if err := os.MkdirAll(rigDir, 0755); err != nil {
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

	// Stub bd to track which bead gets attached_molecule set
	binDir := filepath.Join(townRoot, "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatalf("mkdir binDir: %v", err)
	}
	logPath := filepath.Join(townRoot, "bd.log")
	bdScript := `#!/bin/sh
set -e
echo "$*" >> "${BD_LOG}"
if [ "$1" = "--allow-stale" ]; then
  shift
fi
cmd="$1"
shift || true
case "$cmd" in
  show)
    # Return bead info without attached_molecule initially
    echo '[{"id":"gt-abc123","title":"Bug to fix","status":"open","assignee":"","description":""}]'
    ;;
  formula)
    echo '{"name":"mol-polecat-work"}'
    ;;
  cook)
    exit 0
    ;;
  mol)
    sub="$1"
    shift || true
    case "$sub" in
      wisp)
        echo '{"new_epic_id":"gt-wisp-xyz"}'
        ;;
      bond)
        echo '{"root_id":"gt-wisp-xyz"}'
        ;;
    esac
    ;;
  update)
    # Just succeed
    exit 0
    ;;
esac
exit 0
`
	bdScriptWindows := `@echo off
setlocal enableextensions
echo %*>>"%BD_LOG%"
set "cmd=%1"
set "sub=%2"
if "%cmd%"=="--allow-stale" (
  set "cmd=%2"
  set "sub=%3"
)
if "%cmd%"=="show" (
  echo [{^"id^":^"gt-abc123^",^"title^":^"Bug to fix^",^"status^":^"open^",^"assignee^":^"^",^"description^":^"^"}]
  exit /b 0
)
if "%cmd%"=="formula" (
  echo {^"name^":^"mol-polecat-work^"}
  exit /b 0
)
if "%cmd%"=="cook" exit /b 0
if "%cmd%"=="mol" (
  if "%sub%"=="wisp" (
    echo {^"new_epic_id^":^"gt-wisp-xyz^"}
    exit /b 0
  )
  if "%sub%"=="bond" (
    echo {^"root_id^":^"gt-wisp-xyz^"}
    exit /b 0
  )
)
if "%cmd%"=="update" exit /b 0
exit /b 0
`
	_ = writeBDStub(t, binDir, bdScript, bdScriptWindows)

	t.Setenv("BD_LOG", logPath)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(EnvGTRole, "mayor")
	t.Setenv("GT_POLECAT", "")
	t.Setenv("GT_CREW", "")
	t.Setenv("TMUX_PANE", "")
	t.Setenv("GT_TEST_NO_NUDGE", "1")
	t.Setenv("GT_TEST_SKIP_HOOK_VERIFY", "1") // Stub bd doesn't track state

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	if err := os.Chdir(filepath.Join(townRoot, "mayor", "rig")); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	// Save and restore global flag state
	prevOn := slingOnTarget
	prevVars := slingVars
	prevDryRun := slingDryRun
	prevNoConvoy := slingNoConvoy
	t.Cleanup(func() {
		slingOnTarget = prevOn
		slingVars = prevVars
		slingDryRun = prevDryRun
		slingNoConvoy = prevNoConvoy
	})

	slingDryRun = false
	slingNoConvoy = true
	slingVars = nil
	slingOnTarget = "gt-abc123" // The base bead

	if err := runSling(nil, []string{"mol-polecat-work"}); err != nil {
		t.Fatalf("runSling: %v", err)
	}

	logBytes, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read bd log: %v", err)
	}

	// Find update commands that set attached_molecule
	// Expected: "update gt-abc123 --description=...attached_molecule: gt-wisp-xyz..."
	// Current bug: "update gt-wisp-xyz --description=...attached_molecule: gt-wisp-xyz..."
	logLines := strings.Split(string(logBytes), "\n")
	var attachedMoleculeTarget string
	for _, line := range logLines {
		if strings.Contains(line, "update") && strings.Contains(line, "attached_molecule") {
			// Extract the bead ID being updated
			parts := strings.Fields(line)
			for i, part := range parts {
				if part == "update" && i+1 < len(parts) {
					attachedMoleculeTarget = parts[i+1]
					break
				}
			}
			break
		}
	}

	if attachedMoleculeTarget == "" {
		t.Fatalf("no attached_molecule update found in log:\n%s", string(logBytes))
	}

	// attached_molecule should be set on the BASE bead, not the wisp
	if attachedMoleculeTarget != "gt-abc123" {
		t.Errorf("attached_molecule set on wrong bead: got %q, want %q (base bead)\n"+
			"Current behavior stores attached_molecule in the wisp as a self-reference.\n"+
			"This breaks compound resolution (base bead has no pointer to wisp).\n"+
			"Log:\n%s", attachedMoleculeTarget, "gt-abc123", string(logBytes))
	}
}

// TestDoneClosesAttachedMolecule verifies that gt done closes both the hooked
// bead AND its attached molecule (wisp).
//
// Current bug: gt done only closes the hooked bead. If base bead is hooked
// with attached_molecule pointing to wisp, the wisp becomes orphaned.
//
// Expected behavior: gt done should:
// 1. Check for attached_molecule in hooked bead
// 2. Close the attached molecule (wisp) first
// 3. Close the hooked bead (base bead)
//
// This ensures no orphaned wisps remain after work completes.
func TestDoneClosesAttachedMolecule(t *testing.T) {
	townRoot := t.TempDir()

	// Create rig structure - use simple rig name that matches routes lookup
	rigPath := filepath.Join(townRoot, "gastown")
	if err := os.MkdirAll(rigPath, 0755); err != nil {
		t.Fatalf("mkdir rig: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(townRoot, ".beads"), 0755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}

	// Create routes - path first part must match GT_RIG for prefix lookup
	routes := strings.Join([]string{
		`{"prefix":"gt-","path":"gastown"}`,
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(townRoot, ".beads", "routes.jsonl"), []byte(routes), 0644); err != nil {
		t.Fatalf("write routes.jsonl: %v", err)
	}

	// Stub bd to track close calls
	binDir := filepath.Join(townRoot, "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatalf("mkdir binDir: %v", err)
	}
	closesPath := filepath.Join(townRoot, "closes.log")

	// The stub simulates:
	// - Agent bead gt-agent-nux with hook_bead = gt-abc123 (base bead)
	// - Base bead gt-abc123 with attached_molecule: gt-wisp-xyz, status=hooked
	// - Wisp gt-wisp-xyz (the attached molecule)
	bdScript := fmt.Sprintf(`#!/bin/sh
echo "$*" >> "%s/bd.log"
# Strip --allow-stale
while [ "$1" = "--allow-stale" ]; do
  shift
done
cmd="$1"
shift || true
case "$cmd" in
  show)
    beadID="$1"
    if echo "$*" | grep -q -- "--children"; then
      echo '{"'"$beadID"'":[]}'
    else
      case "$beadID" in
        gt-gastown-polecat-nux)
          echo '[{"id":"gt-gastown-polecat-nux","title":"Polecat nux","status":"open","hook_bead":"gt-abc123","agent_state":"working"}]'
          ;;
        gt-abc123)
          echo '[{"id":"gt-abc123","title":"Bug to fix","status":"hooked","description":"attached_molecule: gt-wisp-xyz"}]'
          ;;
        gt-wisp-xyz)
          echo '[{"id":"gt-wisp-xyz","title":"mol-polecat-work","status":"open","ephemeral":true}]'
          ;;
        *)
          echo '[]'
          ;;
      esac
    fi
    ;;
  close)
    echo "$1" >> "%s"
    ;;
  agent|update|slot)
    exit 0
    ;;
esac
exit 0
`, townRoot, closesPath)

	bdScriptWindows := fmt.Sprintf(`@echo off
setlocal enableextensions
echo %%*>>"%s\bd.log"
set "cmd=%%1"
set "beadID=%%2"
:strip_flags
if "%%cmd%%"=="--allow-stale" (
  set "cmd=%%2"
  set "beadID=%%3"
  shift
  goto strip_flags
)
if "%%cmd%%"=="show" (
  if "%%beadID%%"=="gt-gastown-polecat-nux" (
    echo [{^"id^":^"gt-gastown-polecat-nux^",^"title^":^"Polecat nux^",^"status^":^"open^",^"hook_bead^":^"gt-abc123^",^"agent_state^":^"working^"}]
    exit /b 0
  )
  if "%%beadID%%"=="gt-abc123" (
    echo [{^"id^":^"gt-abc123^",^"title^":^"Bug to fix^",^"status^":^"hooked^",^"description^":^"attached_molecule: gt-wisp-xyz^"}]
    exit /b 0
  )
  if "%%beadID%%"=="gt-wisp-xyz" (
    echo [{^"id^":^"gt-wisp-xyz^",^"title^":^"mol-polecat-work^",^"status^":^"open^",^"ephemeral^":true}]
    exit /b 0
  )
  echo []
  exit /b 0
)
if "%%cmd%%"=="close" (
  echo %%beadID%%>>"%s"
  exit /b 0
)
if "%%cmd%%"=="agent" exit /b 0
if "%%cmd%%"=="update" exit /b 0
if "%%cmd%%"=="slot" exit /b 0
exit /b 0
`, townRoot, closesPath)

	if runtime.GOOS == "windows" {
		bdPath := filepath.Join(binDir, "bd.cmd")
		if err := os.WriteFile(bdPath, []byte(bdScriptWindows), 0644); err != nil {
			t.Fatalf("write bd stub: %v", err)
		}
	} else {
		bdPath := filepath.Join(binDir, "bd")
		if err := os.WriteFile(bdPath, []byte(bdScript), 0755); err != nil {
			t.Fatalf("write bd stub: %v", err)
		}
	}

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GT_ROLE", "polecat")
	t.Setenv("GT_RIG", "gastown")
	t.Setenv("GT_POLECAT", "nux")
	t.Setenv("GT_CREW", "")
	t.Setenv("TMUX_PANE", "")

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	if err := os.Chdir(rigPath); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	// Call the unexported function directly (same package)
	// updateAgentStateOnDone(cwd, townRoot, exitType, issueID)
	// Pass issueID directly — hq-l6mm5 removed agent bead hook slot lookup
	updateAgentStateOnDone(rigPath, townRoot, ExitCompleted, "gt-abc123")

	// Read the close log to see what got closed
	closesBytes, err := os.ReadFile(closesPath)
	if err != nil {
		// No closes happened at all - that's a failure
		t.Fatalf("no beads were closed (closes.log doesn't exist)")
	}
	closes := string(closesBytes)
	closeLines := strings.Split(strings.TrimSpace(closes), "\n")

	// Check that attached molecule gt-wisp-xyz was closed
	foundWisp := false
	foundBase := false
	for _, line := range closeLines {
		if strings.Contains(line, "gt-wisp-xyz") {
			foundWisp = true
		}
		if strings.Contains(line, "gt-abc123") {
			foundBase = true
		}
	}

	if !foundWisp {
		t.Errorf("attached molecule gt-wisp-xyz was NOT closed\n"+
			"gt done should close the attached_molecule before closing the hooked bead.\n"+
			"This leaves orphaned wisps after work completes.\n"+
			"Beads closed: %v", closeLines)
	}

	if !foundBase {
		t.Errorf("hooked bead gt-abc123 was NOT closed\n"+
			"Beads closed: %v", closeLines)
	}
}

// testMoleculeEnv is gt mol burn/squash in townRoot, whose local beads
// workspace is the town, with bd answered by bd and flags at their defaults.
func testMoleculeEnv(townRoot string, bd *inprocBD) moleculeLifecycleEnv {
	return moleculeLifecycleEnv{
		getwd:        func() (string, error) { return townRoot, nil },
		findTown:     func() (string, error) { return townRoot, nil },
		getenv:       envMap(nil),
		beadsWorkDir: func() (string, error) { return townRoot, nil },
		bd:           bd.run,
		out:          io.Discard,
		errOut:       io.Discard,
		noDigest:     true, // the digest path is not what these tests pin
	}
}

// handoffMoleculeBD is an in-process bd holding one pinned handoff bead with
// molecule attached; children maps a parent to its --children answer. Every
// close is logged as "close <args>".
func handoffMoleculeBD(handoffTitle, molecule string, children map[string]string) *inprocBD {
	handoff := `[{"id":"gt-handoff-1","title":"` + handoffTitle + `","status":"pinned","description":"attached_molecule: ` + molecule + `"}]`
	return &inprocBD{answer: func(f *inprocBD, cmd string, args []string) bdAnswer {
		switch cmd {
		case "list":
			if argsMention(args, "status=pinned") {
				return bdOut(handoff)
			}
			for parent, kids := range children {
				if argsMention(args, "parent="+parent) {
					return bdOut(kids)
				}
			}
			return bdOut("[]")
		case "show":
			if argsMention(args, "--children") {
				if kids, ok := children[args[0]]; ok {
					return bdOut(`{"` + args[0] + `":` + kids + `}`)
				}
				return bdOut(`{"` + args[0] + `":[]}`)
			}
			return bdOut(handoff)
		case "create":
			return bdOut(`{"id":"gt-digest-1","title":"Digest"}`)
		case "close":
			f.logLine("close " + strings.Join(args, " "))
		}
		return bdOut("")
	}}
}

// closeLines is the bd close calls, in order.
func closeLines(bd *inprocBD) []string {
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(bd.log()), "\n") {
		if strings.HasPrefix(l, "close ") {
			out = append(out, l)
		}
	}
	return out
}

func squashCmd(ctx context.Context) *cobra.Command {
	cmd := &cobra.Command{}
	cmd.SetContext(ctx)
	return cmd
}

// TestSquashJitterRejectsBadDurations: an invalid or negative --jitter fails
// before any workspace lookup, with a message naming the problem.
func TestSquashJitterRejectsBadDurations(t *testing.T) {
	t.Parallel()
	for jitter, want := range map[string]string{"bogus": "invalid --jitter duration", "-5s": "non-negative"} {
		e := testMoleculeEnv(t.TempDir(), handoffMoleculeBD("x", "y", nil))
		e.jitter = jitter
		e.findTown = func() (string, error) { t.Error("workspace looked up before the jitter was validated"); return "", nil }
		err := moleculeSquash(squashCmd(context.Background()), e, nil)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("--jitter %s: err = %v, want %q", jitter, err, want)
		}
	}
}

// TestSquashJitterZeroDuration: --jitter 0s is accepted, and the run goes on
// to the workspace lookup, which fails here.
func TestSquashJitterZeroDuration(t *testing.T) {
	t.Parallel()
	e := testMoleculeEnv(t.TempDir(), handoffMoleculeBD("x", "y", nil))
	e.jitter = "0s"
	e.findTown = func() (string, error) { return "", errors.New("not in a Gas Town workspace") }
	err := moleculeSquash(squashCmd(context.Background()), e, nil)
	if err == nil || strings.Contains(err.Error(), "jitter") {
		t.Fatalf("err = %v, want the workspace error, not a jitter error", err)
	}
}

// TestSquashJitterContextCancellation: the jitter sleep, once reached,
// returns as soon as the command's context is done instead of blocking.
func TestSquashJitterContextCancellation(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	e := testMoleculeEnv(townRoot, handoffMoleculeBD("refinery Handoff", "gt-wisp-patrol1", nil))
	e.jitter = "10m" // the sleep would block if cancellation didn't work
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	err := moleculeSquash(squashCmd(ctx), e, []string{"refinery"})
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("jitter sleep should have been cancelled, but took %v", elapsed)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the context's cancellation", err)
	}
}

// TestBurnClosesWispRoot: burn detaches the molecule and then closes its
// root, or the wisp root stays hooked forever (#1828).
func TestBurnClosesWispRoot(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	bd := handoffMoleculeBD("witness Handoff", "gt-wisp-mol1", nil)
	if err := moleculeBurn(nil, testMoleculeEnv(townRoot, bd), []string{"witness"}); err != nil {
		t.Fatalf("burn: %v", err)
	}
	if closes := strings.Join(closeLines(bd), "\n"); !strings.Contains(closes, "gt-wisp-mol1") {
		t.Errorf("molecule root gt-wisp-mol1 was not closed; close calls:\n%s", closes)
	}
}

// TestSquashClosesWispRoot: squash closes the molecule root after detaching,
// as burn does (#1828).
func TestSquashClosesWispRoot(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	bd := handoffMoleculeBD("refinery Handoff", "gt-wisp-patrol1", nil)
	if err := moleculeSquash(squashCmd(context.Background()), testMoleculeEnv(townRoot, bd), []string{"refinery"}); err != nil {
		t.Fatalf("squash: %v", err)
	}
	if closes := strings.Join(closeLines(bd), "\n"); !strings.Contains(closes, "gt-wisp-patrol1") {
		t.Errorf("molecule root gt-wisp-patrol1 was not closed; close calls:\n%s", closes)
	}
}

// TestSquashClosesDescendantsAndRoot: the open step is closed, the closed one
// is left alone, and the root is closed after its children.
func TestSquashClosesDescendantsAndRoot(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	bd := handoffMoleculeBD("witness Handoff", "gt-wisp-mol2", map[string]string{
		"gt-wisp-mol2": `[{"id":"gt-step-1","title":"Step 1","status":"open"},{"id":"gt-step-2","title":"Step 2","status":"closed"}]`,
	})
	if err := moleculeBurn(nil, testMoleculeEnv(townRoot, bd), []string{"witness"}); err != nil {
		t.Fatalf("burn: %v", err)
	}
	lines := closeLines(bd)
	step1, root := -1, -1
	for i, l := range lines {
		if strings.Contains(l, "gt-step-1") && step1 < 0 {
			step1 = i
		}
		if strings.Contains(l, "gt-wisp-mol2") {
			root = i
		}
		if strings.Contains(l, "gt-step-2") {
			t.Errorf("the already-closed gt-step-2 was closed again: %s", l)
		}
	}
	if step1 < 0 || root < 0 || root < step1 {
		t.Fatalf("close order: step-1 at %d, root at %d, want both with the root last; close calls:\n%s", step1, root, strings.Join(lines, "\n"))
	}
}
