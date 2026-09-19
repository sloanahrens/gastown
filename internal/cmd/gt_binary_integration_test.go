//go:build integration

package cmd

import (
	"bytes"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/testutil"
)

func TestInstallFailsBeforeMutationWhenDoltMissing(t *testing.T) {
	tmpDir := t.TempDir()
	hqPath := filepath.Join(tmpDir, "missing-dolt-hq")
	gtBinary := buildGT(t)

	cmd := exec.Command(gtBinary, "install", hqPath, "--name", "missing-dolt-test")
	cmd.Env = installTestEnvWithFakeBD(t, tmpDir)
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("gt install should fail when dolt is missing; output:\n%s", output)
	}

	out := string(output)
	if !strings.Contains(out, "dolt is required for gt install with beads enabled") {
		t.Fatalf("expected missing-dolt preflight error, got:\n%s", out)
	}
	if !strings.Contains(out, "--no-beads") {
		t.Fatalf("expected --no-beads fallback hint, got:\n%s", out)
	}
	if _, statErr := os.Stat(hqPath); !os.IsNotExist(statErr) {
		t.Fatalf("install should not create target HQ before missing-dolt failure; statErr=%v", statErr)
	}
}

func TestInstallNoBeadsAllowsMissingDolt(t *testing.T) {
	tmpDir := t.TempDir()
	hqPath := filepath.Join(tmpDir, "no-beads-hq")
	gtBinary := buildGT(t)

	cmd := exec.Command(gtBinary, "install", hqPath, "--no-beads", "--name", "no-beads-test")
	cmd.Env = installTestEnvWithFakeBD(t, tmpDir)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gt install --no-beads should succeed without dolt: %v\nOutput:\n%s", err, output)
	}

	if info, statErr := os.Stat(hqPath); statErr != nil {
		t.Fatalf("HQ root should exist: %v", statErr)
	} else if !info.IsDir() {
		t.Fatalf("HQ root should be a directory")
	}
	if _, statErr := os.Stat(filepath.Join(hqPath, ".beads")); !os.IsNotExist(statErr) {
		t.Fatalf("--no-beads install should not create .beads; statErr=%v", statErr)
	}
}

func TestInstallFailsBeforeMutationWhenDoltPortOccupiedByNonDolt(t *testing.T) {
	ln := listenAndHoldTCP(t)
	tmpDir := t.TempDir()
	hqPath := filepath.Join(tmpDir, "port-conflict-hq")
	gtBinary := buildGT(t)
	port := ln.Addr().(*net.TCPAddr).Port

	cmd := exec.Command(gtBinary, "install", hqPath,
		"--name", "port-conflict-test",
		"--dolt-port", strconv.Itoa(port),
	)
	cmd.Env = installTestEnvWithFakeBDAndDolt(t, tmpDir)
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("gt install should fail when a non-Dolt process owns the Dolt port; output:\n%s", output)
	}

	out := string(output)
	if !strings.Contains(out, "Dolt port") || !strings.Contains(out, "already in use") {
		t.Fatalf("expected Dolt port conflict error, got:\n%s", out)
	}
	if !strings.Contains(out, "--dolt-port") {
		t.Fatalf("expected --dolt-port recovery hint, got:\n%s", out)
	}
	if _, statErr := os.Stat(hqPath); !os.IsNotExist(statErr) {
		t.Fatalf("install should not create target HQ before port preflight failure; statErr=%v", statErr)
	}
}

func TestPrimeFlagCombinations(t *testing.T) {
	gtBin := buildGT(t)

	cases := []struct {
		name      string
		args      []string
		wantError bool
		errorMsg  string
	}{
		{
			name:      "state_alone_is_valid",
			args:      []string{"prime", "--state"},
			wantError: false, // May fail for other reasons (not in workspace), but not flag validation
		},
		{
			name:      "state_with_hook_errors",
			args:      []string{"prime", "--state", "--hook"},
			wantError: true,
			errorMsg:  "--state cannot be combined with other flags",
		},
		{
			name:      "state_with_dry_run_errors",
			args:      []string{"prime", "--state", "--dry-run"},
			wantError: true,
			errorMsg:  "--state cannot be combined with other flags",
		},
		{
			name:      "state_with_explain_errors",
			args:      []string{"prime", "--state", "--explain"},
			wantError: true,
			errorMsg:  "--state cannot be combined with other flags",
		},
		{
			name:      "dry_run_and_explain_valid",
			args:      []string{"prime", "--dry-run", "--explain"},
			wantError: false, // May fail for other reasons, but not flag validation
		},
		{
			name:      "hook_and_dry_run_valid",
			args:      []string{"prime", "--hook", "--dry-run"},
			wantError: false, // May fail for other reasons, but not flag validation
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(gtBin, tc.args...)
			output, err := cmd.CombinedOutput()

			if tc.wantError {
				if err == nil {
					t.Fatalf("expected error, got success with output: %s", output)
				}
				if tc.errorMsg != "" && !strings.Contains(string(output), tc.errorMsg) {
					t.Fatalf("expected error containing %q, got: %s", tc.errorMsg, output)
				}
			}
			// For non-error cases, we don't fail on other errors (like "not in workspace")
			// because we're only testing flag validation
			if !tc.wantError && tc.errorMsg != "" && strings.Contains(string(output), tc.errorMsg) {
				t.Fatalf("unexpected error message %q in output: %s", tc.errorMsg, output)
			}
		})
	}
}

// TestDryRunSkipsSideEffects tests that --dry-run skips various side effects via CLI.
func TestDryRunSkipsSideEffects(t *testing.T) {
	gtBin := buildGT(t)

	// Create a temp workspace
	townRoot := t.TempDir()

	// Set up minimal workspace structure
	beadsDir := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatalf("create beads dir: %v", err)
	}

	// Write routes
	routes := []beads.Route{{Prefix: "bd-", Path: "."}}
	if err := beads.WriteRoutes(beadsDir, routes); err != nil {
		t.Fatalf("write routes: %v", err)
	}

	// Create handoff marker that should NOT be removed in dry-run
	runtimeDir := filepath.Join(townRoot, constants.DirRuntime)
	if err := os.MkdirAll(runtimeDir, 0755); err != nil {
		t.Fatalf("create runtime dir: %v", err)
	}
	markerPath := filepath.Join(runtimeDir, constants.FileHandoffMarker)
	if err := os.WriteFile(markerPath, []byte("prev-session"), 0644); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	// Run gt prime --dry-run --explain
	cmd := exec.Command(gtBin, "prime", "--dry-run", "--explain")
	cmd.Dir = townRoot
	output, _ := cmd.CombinedOutput()

	// The command may fail for other reasons (not fully configured workspace)
	// but we can check:
	// 1. Marker still exists
	if _, err := os.Stat(markerPath); os.IsNotExist(err) {
		t.Fatalf("handoff marker was removed in dry-run mode")
	}

	// 2. Output mentions skipped operations
	outputStr := string(output)
	// Check for explain output about dry-run (if workspace was valid enough to get there)
	if strings.Contains(outputStr, "bd prime") && !strings.Contains(outputStr, "skipped") {
		t.Logf("Note: output doesn't explicitly mention skipping bd prime: %s", outputStr)
	}
}

// TestPRWorkflowGuard_Integration exercises the compiled guard end-to-end
// (real stdin, real exit code) against gt-pjeh's regression shape: the
// guard's only filter used to be the hook's "if" field, so a hooks-sync bug
// that dropped "if" made the guard fire on every Bash call and block all of
// them unconditionally in agent context. It must now inspect the command
// itself and only block the three real PR-workflow shapes, leaving an
// unrelated command alone even when fed to the guard directly (as if "if"
// were missing).
func TestPRWorkflowGuard_Integration(t *testing.T) {
	bin := buildGT(t)
	workDir := t.TempDir() // not under /polecats/, /crew/, or /deacon/dogs/

	run := func(t *testing.T, command string, agentContext bool) (exitCode int, stderr string) {
		t.Helper()
		env := testutil.CleanGTEnv()
		if agentContext {
			env = append(env, "GT_POLECAT=1")
		}
		cmd := exec.Command(bin, "tap", "guard", "pr-workflow")
		cmd.Dir = workDir
		cmd.Env = env
		cmd.Stdin = bytes.NewBufferString(`{"tool_name":"Bash","tool_input":{"command":"` + command + `"}}`)
		var errBuf bytes.Buffer
		cmd.Stderr = &errBuf
		err := cmd.Run()
		if err == nil {
			return 0, errBuf.String()
		}
		if exitErr, ok := err.(*exec.ExitError); ok {
			return exitErr.ExitCode(), errBuf.String()
		}
		t.Fatalf("running guard: %v", err)
		return -1, ""
	}

	t.Run("unrelated command from agent context exits 0", func(t *testing.T) {
		code, _ := run(t, "ls -la", true)
		if code != 0 {
			t.Errorf("exit code = %d, want 0 (a hooks-sync regression that drops the 'if' field must not make this guard block everything)", code)
		}
	})

	t.Run("no stdin in agent context blocks (fail closed, gt-wisp-52y4)", func(t *testing.T) {
		// Some harness templates (Copilot's INPUT=$(cat) wrapper) drain
		// stdin before invoking this guard, so the guard can't tell what
		// command is running. It must fall back to the pre-self-filter
		// unconditional context check rather than allow everything through.
		env := append(testutil.CleanGTEnv(), "GT_POLECAT=1")
		cmd := exec.Command(bin, "tap", "guard", "pr-workflow")
		cmd.Dir = workDir
		cmd.Env = env
		cmd.Stdin = bytes.NewBuffer(nil)
		err := cmd.Run()
		exitErr, ok := err.(*exec.ExitError)
		if !ok || exitErr.ExitCode() != 2 {
			t.Errorf("expected exit 2 with empty stdin in agent context, got err: %v", err)
		}
	})

	t.Run("no stdin outside agent context exits 0", func(t *testing.T) {
		cmd := exec.Command(bin, "tap", "guard", "pr-workflow")
		cmd.Dir = workDir
		cmd.Env = testutil.CleanGTEnv()
		cmd.Stdin = bytes.NewBuffer(nil)
		if err := cmd.Run(); err != nil {
			t.Errorf("expected exit 0 with empty stdin outside agent context, got error: %v", err)
		}
	})

	t.Run("refinery role: feature-branch checkout exempted, unrelated command still self-filtered", func(t *testing.T) {
		// Pins the composition of the refinery exemption (gt-r2xm) with the
		// command self-filter (gt-pjeh) at the compiled-binary level: the
		// exemption fires for the exact rehearsal shape, while an unrelated
		// command still passes through the ordinary self-filter rather than
		// the exemption.
		env := append(testutil.CleanGTEnv(), "GT_REFINERY=1")

		run := func(command string) (exitCode int) {
			cmd := exec.Command(bin, "tap", "guard", "pr-workflow")
			cmd.Dir = workDir
			cmd.Env = env
			cmd.Stdin = bytes.NewBufferString(`{"tool_name":"Bash","tool_input":{"command":"` + command + `"}}`)
			err := cmd.Run()
			if err == nil {
				return 0
			}
			if exitErr, ok := err.(*exec.ExitError); ok {
				return exitErr.ExitCode()
			}
			t.Fatalf("running guard: %v", err)
			return -1
		}

		if code := run("git checkout -b temp origin/polecat/topaz+abc123"); code != 0 {
			t.Errorf("refinery rehearsal checkout: exit code = %d, want 0", code)
		}
		if code := run("gh pr create --title foo"); code != 2 {
			t.Errorf("refinery gh pr create: exit code = %d, want 2 (stays blocked)", code)
		}
		if code := run("ls -la"); code != 0 {
			t.Errorf("refinery unrelated command: exit code = %d, want 0", code)
		}
	})

	blockedShapes := []string{
		"gh pr create --title foo",
		"git checkout -b feature/x",
		"git switch -c feature/x",
	}
	for _, shape := range blockedShapes {
		t.Run("blocks "+shape+" from agent context", func(t *testing.T) {
			code, stderr := run(t, shape, true)
			if code != 2 {
				t.Errorf("exit code = %d, want 2", code)
			}
			if !bytes.Contains([]byte(stderr), []byte("PR WORKFLOW BLOCKED")) {
				t.Errorf("stderr missing block banner: %q", stderr)
			}
		})

		t.Run("allows "+shape+" outside agent context and non-maintainer origin", func(t *testing.T) {
			code, _ := run(t, shape, false)
			if code != 0 {
				t.Errorf("exit code = %d, want 0", code)
			}
		})
	}
}

// Regression tests for gt-3mp1: the if-glob evaluator in Claude Code
// has a bug where it treats certain shell patterns as "match any pattern
// starting with *" when it cannot statically resolve them. This caused
// every if-gated deny hook to fire on unrelated commands.
//
// It lives here rather than in tap_guard_pr_workflow_test.go because it
// drives the compiled binary (gt-fo3h moved every buildGT-dependent test in
// this package behind the integration tag, so the untagged file keeps only
// the pure unit tests).
func TestPRWorkflowGuard_Gt3mp1Regression(t *testing.T) {
	bin := buildGT(t)
	workDir := t.TempDir()

	run := func(t *testing.T, command string, agentContext bool) (exitCode int, stderr string) {
		t.Helper()
		env := testutil.CleanGTEnv()
		if agentContext {
			env = append(env, "GT_POLECAT=1")
		}
		cmd := exec.Command(bin, "tap", "guard", "pr-workflow")
		cmd.Dir = workDir
		cmd.Env = env
		cmd.Stdin = bytes.NewBufferString(`{"tool_name":"Bash","tool_input":{"command":"` + command + `"}}`)
		var errBuf bytes.Buffer
		cmd.Stderr = &errBuf
		err := cmd.Run()
		if err == nil {
			return 0, errBuf.String()
		}
		if exitErr, ok := err.(*exec.ExitError); ok {
			return exitErr.ExitCode(), errBuf.String()
		}
		t.Fatalf("running guard: %v", err)
		return -1, ""
	}

	// Pattern (a): Commands with { brace group containing a quoted string
	// trip the if-glob evaluator, which then matches any leading-* glob
	braceGroupCommands := []string{
		`echo x{'a'}y`,
		`echo x{"a"}y`,
		`python3 - <<'EOF'
print({'a'})
EOF`,
	}
	for _, cmd := range braceGroupCommands {
		t.Run("brace-group with quoted string", func(t *testing.T) {
			code, _ := run(t, cmd, false) // outside agent context
			if code != 0 {
				t.Errorf("exit code = %d, want 0 (brace-group with quoted string should not trip the guard)", code)
			}
		})
	}

	// Pattern (b): Variable assigned from command substitution in the same
	// command line, then used as a bare double-quoted argument trips hooks
	commandSubCommands := []string{
		`M=$(echo abc); echo "$M"`,
		`ID=$(gt mail inbox --json 2>/dev/null | python3 -c "import json,sys; m=json.load(sys.stdin); print(m[0]['id'] if m else '')"); gt mail read "$ID"`,
		`MAIN=$(git ls-remote --heads origin main | cut -f1); git cat-file -e "$MAIN"`,
	}
	for _, cmd := range commandSubCommands {
		t.Run("command substitution with bare $VAR", func(t *testing.T) {
			code, _ := run(t, cmd, false) // outside agent context
			if code != 0 {
				t.Errorf("exit code = %d, want 0 (command substitution with bare $VAR should not trip the guard)", code)
			}
		})
	}
}
