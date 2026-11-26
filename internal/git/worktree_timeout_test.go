package git

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestWorktreeAddDetachedTimeout pins that the worktree add runs every call it
// makes, the submodule init's included, under the handle's WithTimeout default:
// without it a git hung on a submodule clone holds Lander.gitMu for ever
// (gt-665osh).
func TestWorktreeAddDetachedTimeout(t *testing.T) {
	t.Parallel()
	const handleTimeout = 4 * time.Second
	for _, tc := range []struct {
		name   string
		handle time.Duration
		want   time.Duration
	}{
		{name: "a handle timeout bounds every call", handle: handleTimeout, want: handleTimeout},
		{name: "no handle timeout leaves every call unbounded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			worktree := t.TempDir()
			if err := os.WriteFile(filepath.Join(worktree, ".gitmodules"), []byte("[submodule \"libs/sub\"]\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			submoduleUpdate := "-C " + worktree + " submodule update --init --recursive"
			s := newScripted(map[string]reply{
				"worktree add --detach " + worktree + " main": ok(""),
				// The submodule init's own calls: the tracked-file check, the
				// town-root probe and the update.
				"-C " + worktree + " *": ok(""),
			})
			if err := NewGitWithExec("", s.run).WithTimeout(tc.handle).WorktreeAddDetached(worktree, "main"); err != nil {
				t.Fatalf("WorktreeAddDetached: %v", err)
			}
			s.noUnscripted(t)
			if !s.hasSent(submoduleUpdate) {
				t.Fatalf("calls %q lack the submodule update, so the add never reached its submodule calls", s.sent())
			}
			for _, c := range s.calls {
				if c.timeout != tc.want {
					t.Errorf("call %q ran with timeout %v, want %v", strings.Join(c.args, " "), c.timeout, tc.want)
				}
			}
		})
	}
}
