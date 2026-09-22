package cmd

import (
	"testing"
)

// TestMatchesPRWorkflowCommand covers the self-filtering this guard now does
// against tool_input.command, mirroring the "if" glob patterns in
// hooks.DefaultBase() (Bash(gh pr create*), Bash(git checkout -b*),
// Bash(git switch -c*)).
func TestMatchesPRWorkflowCommand(t *testing.T) {
	t.Parallel()
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

		// An unquoted newline separates commands exactly like ';' does, so a
		// multi-line Bash tool call must be judged line by line — before
		// gt-3j8u the whole call was one segment and only its first word was
		// ever compared against the prefixes, so a blocked shape on any later
		// line failed open.
		{"newline-separated compound, blocked shape on later line", "cd /tmp\ngh pr create --title foo", true},
		{"newline-separated compound, blocked shape on first line", "git checkout -b feature/x\necho done", true},
		{"newline-separated compound, no blocked line", "cd /tmp\nls -la", false},
		{"newline-separated compound, blocked shape after a blank line", "echo one\n\ngit switch -c feature/x", true},

		// A backslash-newline is a line continuation, not a separator: the
		// shell joins the lines before tokenizing, so the command it runs is
		// the blocked shape even though no single line spells it.
		{"line continuation joins the halves of a blocked command", "git \\\n  checkout -b feature/x", true},

		// Heredoc bodies are data being written, not shell syntax (the rule
		// evaluateDangerousCommand and checkBashCommand already apply), so a
		// body line that spells a blocked shape is not an invocation — while
		// a real command after the heredoc still is.
		{"prose in a heredoc body stays opaque", "cat > note.md <<'EOF'\nnever run gh pr create here\nEOF", false},
		{"blocked command after a heredoc body", "cat > note.md <<'EOF'\nordinary content\nEOF\ngit switch -c feature/x", true},

		// Quoted text is never a command word, newline or not.
		{"quoted mention on its own line", "git commit -m \"use gh pr create\"\nls -la", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := matchesPRWorkflowCommand(tt.command); got != tt.want {
				t.Errorf("matchesPRWorkflowCommand(%q) = %v, want %v", tt.command, got, tt.want)
			}
		})
	}
}
