package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAssigneeToWorktreePath_InvalidFormats(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()

	tests := []struct {
		name     string
		assignee string
	}{
		{"empty", ""},
		{"single part", "deacon"},
		{"two parts", "gastown/witness"},
		{"four parts", "a/b/c/d"},
		{"unknown agent type", "gastown/unknown/agent"},
		{"rig traversal", "../polecats/max"},
		{"name traversal", "gastown/polecats/.."},
		{"rig dot", "./polecats/max"},
		{"name dot", "gastown/polecats/."},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := assigneeToWorktreePath(townRoot, tt.assignee)
			if got != "" {
				t.Errorf("assigneeToWorktreePath(%q, %q) = %q, want empty", townRoot, tt.assignee, got)
			}
		})
	}
}

func TestAssigneeToWorktreePath_NewStructure(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	rigName := "testrig"

	// Create new-structure worktree: townRoot/testrig/polecats/max/testrig/
	worktreePath := filepath.Join(townRoot, rigName, "polecats", "max", rigName)
	if err := os.MkdirAll(worktreePath, 0755); err != nil {
		t.Fatal(err)
	}
	// Create .git file (worktree indicator)
	if err := os.WriteFile(filepath.Join(worktreePath, ".git"), []byte("gitdir: /fake"), 0644); err != nil {
		t.Fatal(err)
	}

	got := assigneeToWorktreePath(townRoot, "testrig/polecats/max")
	if got != worktreePath {
		t.Errorf("assigneeToWorktreePath() = %q, want %q", got, worktreePath)
	}
}

func TestAssigneeToWorktreePath_OldStructure(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	rigName := "testrig"

	// Create old-structure worktree: townRoot/testrig/polecats/max/
	worktreePath := filepath.Join(townRoot, rigName, "polecats", "max")
	if err := os.MkdirAll(worktreePath, 0755); err != nil {
		t.Fatal(err)
	}
	// Create .git file (worktree indicator)
	if err := os.WriteFile(filepath.Join(worktreePath, ".git"), []byte("gitdir: /fake"), 0644); err != nil {
		t.Fatal(err)
	}

	got := assigneeToWorktreePath(townRoot, "testrig/polecats/max")
	if got != worktreePath {
		t.Errorf("assigneeToWorktreePath() = %q, want %q", got, worktreePath)
	}
}

func TestAssigneeToWorktreePath_CrewWorker(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	rigName := "testrig"

	// Create crew worktree: townRoot/testrig/crew/joe/testrig/
	worktreePath := filepath.Join(townRoot, rigName, "crew", "joe", rigName)
	if err := os.MkdirAll(worktreePath, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktreePath, ".git"), []byte("gitdir: /fake"), 0644); err != nil {
		t.Fatal(err)
	}

	got := assigneeToWorktreePath(townRoot, "testrig/crew/joe")
	if got != worktreePath {
		t.Errorf("assigneeToWorktreePath() = %q, want %q", got, worktreePath)
	}
}

func TestAssigneeToWorktreePath_NoWorktree(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()

	// Directory exists but no .git -> not a worktree
	dirPath := filepath.Join(townRoot, "testrig", "polecats", "max")
	if err := os.MkdirAll(dirPath, 0755); err != nil {
		t.Fatal(err)
	}

	got := assigneeToWorktreePath(townRoot, "testrig/polecats/max")
	if got != "" {
		t.Errorf("assigneeToWorktreePath() = %q, want empty (no .git)", got)
	}
}
