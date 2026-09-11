package cmd

import (
	"bytes"
	"os/exec"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

// TestMatchesPRWorkflowCommand covers the self-filtering this guard now does
// against tool_input.command, mirroring the "if" glob patterns in
// hooks.DefaultBase() (Bash(gh pr create*), Bash(git checkout -b*),
// Bash(git switch -c*)).
func TestMatchesPRWorkflowCommand(t *testing.T) {
	tests := []struct {
		name    string
		command string
		want    bool
	}{
		{"gh pr create", "gh pr create --title foo", true},
		{"git checkout -b", "git checkout -b feature/x", true},
		{"git switch -c", "git switch -c feature/x", true},
		{"leading whitespace", "  git checkout -b feature/x", true},
		{"unrelated command", "ls -la", false},
		{"unrelated git command", "git status", false},
		{"git checkout without -b", "git checkout main", false},
		{"empty command", "", false},
		{"compound command, blocked shape in later segment", "cd foo && git checkout -b x", true},
		{"compound command, blocked shape first", "gh pr create --title foo && echo done", true},
		{"compound command with pipe", "echo x | gh pr create --title foo", true},
		{"compound command, no blocked segment", "cd foo && git status", false},
		{"different flag not mistaken for -b", "git checkout -branch feature/x", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := matchesPRWorkflowCommand(tt.command); got != tt.want {
				t.Errorf("matchesPRWorkflowCommand(%q) = %v, want %v", tt.command, got, tt.want)
			}
		})
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
