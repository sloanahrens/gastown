package cmd

import (
	"testing"
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
