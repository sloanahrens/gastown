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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := matchesPRWorkflowCommand(tt.command); got != tt.want {
				t.Errorf("matchesPRWorkflowCommand(%q) = %v, want %v", tt.command, got, tt.want)
			}
		})
	}
}
