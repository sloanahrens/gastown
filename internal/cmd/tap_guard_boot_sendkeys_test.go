package cmd

import (
	"bytes"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

// TestMatchesRawTmuxSendKeys covers the self-filtering this guard does
// against tool_input.command, replacing the leading-* "if" glob
// Bash(*tmux*send-keys*) the boot hook used to carry (gt-3mp1).
func TestMatchesRawTmuxSendKeys(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
		want    bool
	}{
		{"bare send-keys", "tmux send-keys -t deacon 'gt prime' Enter", true},
		{"send-keys for a nudge-shaped payload", `tmux send-keys -t deacon "hello" Enter`, true},
		{"target flag before subcommand", "tmux -t deacon send-keys 'gt prime'", true},
		{"socket flag before subcommand", "tmux -L gt send-keys -t deacon 'gt prime'", true},
		{"fully qualified binary", "/opt/homebrew/bin/tmux send-keys -t deacon hi", true},
		{"relative binary", "./tmux send-keys -t deacon hi", true},
		{"later segment of a compound command", "cd ~/gt && tmux send-keys -t deacon hi", true},
		{"behind a command wrapper", "sudo tmux send-keys -t deacon hi", true},
		{"behind env", "env tmux send-keys -t deacon hi", true},
		{"after a leading assignment", "FOO=1 tmux send-keys -t deacon hi", true},
		{"nested shell invoker", `bash -c "tmux send-keys -t deacon hi"`, true},
		{"nested shell invoker after unrelated text", `echo x && sh -c "tmux send-keys hi"`, true},
		{"tmux without send-keys", "tmux ls", false},
		{"tmux capture-pane", "tmux capture-pane -t deacon -p", false},
		{"send-keys is another command's argument", "tmux ls && echo send-keys", false},
		{"send-keys without tmux", "echo send-keys", false},
		{"quoted prose about send-keys", `echo "do not use tmux send-keys here"`, false},
		// A message that mentions both words as separate arguments is prose,
		// not an invocation — the false block this guard exists to avoid.
		{"mail naming both words in separate args", `gt mail send gastown/witness -s "tmux" -m "prefer send-keys-free nudges"`, false},
		{"argument that is a path ending in tmux", "cat /tmp/tmux send-keys", false},
		{"gt nudge is the sanctioned alternative", `gt nudge --mode=immediate deacon "boot: start"`, false},
		{"empty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := commandInvokesRawTmuxSendKeys(tt.command, 0)
			if got != tt.want {
				t.Errorf("commandInvokesRawTmuxSendKeys(%q) = %v, want %v", tt.command, got, tt.want)
			}
		})
	}
}

// TestCommandInvokesRawTmuxSendKeys_HeredocBodyIsData pins that a heredoc
// body — data being written or piped, not shell syntax to evaluate — cannot
// trip the guard, the same treatment every other guard in this family gives
// it (gt-mkrj).
func TestCommandInvokesRawTmuxSendKeys_HeredocBodyIsData(t *testing.T) {
	t.Parallel()
	command := "cat > note.md <<'EOF'\nnever use tmux send-keys; use gt nudge\nEOF"
	if commandInvokesRawTmuxSendKeys(command, 0) {
		t.Errorf("commandInvokesRawTmuxSendKeys(%q) = true, want false (heredoc body is data)", command)
	}
}

// TestBootSendKeysGuard_Integration exercises the compiled guard end-to-end
// (real stdin, real exit code).
//
// Two things are being pinned here. First, the guard must block a genuine
// tmux send-keys invocation — it is the boot role's only protection against
// staging unsubmitted text in the Deacon TUI. Second, and this is the
// regression from gt-3mp1, it must NOT block anything else: the If-gated
// inline echo+exit-2 hook it replaces fired on unrelated commands whenever
// Claude Code's "if" evaluator met a command it could not statically resolve,
// so a boot agent could not run ordinary Bash at all (the shape refinery
// rejected on gt-wisp-4wgk). The gate shapes from the bead's minimal repro
// are asserted allowed here, run directly against the guard as if no outer
// filter existed at all.
func TestBootSendKeysGuard_Integration(t *testing.T) {
	t.Parallel()
	bin := buildGT(t)
	workDir := t.TempDir()

	run := func(t *testing.T, command string) (exitCode int, stderr string) {
		t.Helper()
		payload, err := json.Marshal(map[string]any{
			"tool_name":  "Bash",
			"tool_input": map[string]string{"command": command},
		})
		if err != nil {
			t.Fatalf("marshalling hook payload: %v", err)
		}
		cmd := exec.Command(bin, "tap", "guard", "boot-sendkeys")
		cmd.Dir = workDir
		cmd.Env = testutil.CleanGTEnv()
		cmd.Stdin = bytes.NewReader(payload)
		var errBuf bytes.Buffer
		cmd.Stderr = &errBuf
		err = cmd.Run()
		if err == nil {
			return 0, errBuf.String()
		}
		if exitErr, ok := err.(*exec.ExitError); ok {
			return exitErr.ExitCode(), errBuf.String()
		}
		t.Fatalf("running guard: %v", err)
		return -1, ""
	}

	t.Run("raw tmux send-keys is blocked", func(t *testing.T) {
		code, stderr := run(t, "tmux send-keys -t deacon 'gt prime' Enter")
		if code != 2 {
			t.Errorf("exit code = %d, want 2 (raw tmux send-keys must be blocked)", code)
		}
		for _, want := range []string{
			"BLOCKED",
			"Boot must not use raw tmux send-keys",
			"gt nudge --mode=immediate deacon",
		} {
			if !strings.Contains(stderr, want) {
				t.Errorf("block output missing %q, got:\n%s", want, stderr)
			}
		}
	})

	// The gt-3mp1 trigger shapes: commands Claude Code's if-glob evaluator
	// cannot statically resolve. All ordinary boot work, none of it a raw
	// tmux send-keys.
	allowed := []struct {
		name    string
		command string
	}{
		{"brace group with single quotes", `echo x{'a'}y`},
		{"brace group with double quotes", `echo x{"a"}y`},
		{"json literal", `echo '{"status":"booting"}' > /tmp/boot.json`},
		{"python dict literal in heredoc", "python3 - <<'EOF'\nprint({'a': 1})\nEOF"},
		{"substitution-assigned var used bare", `M=$(echo abc); echo "$M"`},
		{"substitution into an argument", `ID=$(gt mail inbox --json | head -1); gt mail read "$ID"`},
		{"ordinary boot command", "gt prime --hook"},
		{"sanctioned nudge", `gt nudge --mode=immediate deacon "boot: town is up"`},
	}
	for _, tt := range allowed {
		t.Run("allowed: "+tt.name, func(t *testing.T) {
			code, stderr := run(t, tt.command)
			if code != 0 {
				t.Errorf("exit code = %d, want 0 (command should not trip the guard), stderr:\n%s", code, stderr)
			}
		})
	}

	t.Run("unparsable stdin fails open", func(t *testing.T) {
		cmd := exec.Command(bin, "tap", "guard", "boot-sendkeys")
		cmd.Dir = workDir
		cmd.Env = testutil.CleanGTEnv()
		cmd.Stdin = strings.NewReader("not json")
		if err := cmd.Run(); err != nil {
			t.Errorf("unparsable stdin should fail open (exit 0), got: %v", err)
		}
	})
}
