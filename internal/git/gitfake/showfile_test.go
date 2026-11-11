package gitfake

import (
	"errors"
	"testing"

	"github.com/steveyegge/gastown/internal/git"
)

// A directory that is no repository fails the byte read as a real error, not as
// an absent path: the caller must not read it as "not tracked" (gt-3lvcjz.16).
func TestFakeShowFileBytesAtRevOutsideARepository(t *testing.T) {
	t.Parallel()
	_, err := New().Open(t.TempDir()).ShowFileBytesAtRev("main", ".beads/config.yaml")
	if err == nil {
		t.Fatal("ShowFileBytesAtRev succeeded outside a repository, want an error")
	}
	if errors.Is(err, git.ErrNotAtRef) {
		t.Errorf("error = %v, want a real error rather than ErrNotAtRef", err)
	}
}
