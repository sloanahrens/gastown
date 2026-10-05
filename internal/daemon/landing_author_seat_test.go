package daemon

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/git/gitfake"
)

// The facts the landing worker's author-seat refresh rests on (gt-fn9e6.55):
// it refreshes the seat's remote-tracking default branch, it skips a seat that
// has no worktree, and a refresh that cannot run is a reportable error, never
// a panic and never anything the landing waits on.

const (
	authorSeatRig     = "hm"
	authorSeatPolecat = "furiosa"
)

// seatDaemon is a daemon whose git is a gitfake world, with one polecat seat
// at the nested worktree layout under an empty town root. The seat's worktree
// directory does not exist yet; a test makes it.
func seatDaemon(t *testing.T) (*Daemon, *gitfake.Fake, string) {
	t.Helper()
	d := &Daemon{logger: log.New(io.Discard, "", 0), config: &Config{TownRoot: filepath.Join(t.TempDir(), "town")}}
	f := useGitfake(t, d)
	workDir := filepath.Join(d.config.TownRoot, authorSeatRig, "polecats", authorSeatPolecat, authorSeatRig)
	return d, f, workDir
}

// TestRefreshAuthorSeatRefreshesTheSeatsDefaultBranch pins the fix: the
// Forgejo landing moves main on origin and nothing in the author's worktree
// fetches it, so the seat's origin/<default> keeps the pre-landing commit.
// The refresh moves that one ref to the remote's tip and leaves HEAD and every
// other remote-tracking ref where the seat left them.
func TestRefreshAuthorSeatRefreshesTheSeatsDefaultBranch(t *testing.T) {
	t.Parallel()
	d, f, workDir := seatDaemon(t)
	origin := filepath.Join(t.TempDir(), "origin.git")

	f.InitBare(t, origin)
	base := f.Commit(t, origin, "main", "initial", map[string]string{"README.md": "# Test\n"})
	f.Clone(t, origin, workDir)
	g := f.Open(workDir)
	head, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("Rev(HEAD): %v", err)
	}
	// A second remote-tracking ref the refresh has no business moving.
	f.SetRef(t, workDir, "refs/remotes/origin/other", base)

	// The landing merges the seat's work on the remote and moves main there.
	landed := f.Commit(t, origin, "main", "land the seat's work", map[string]string{"work.go": "package work\n"})
	if got := f.Ref(workDir, "refs/remotes/origin/main"); got != base {
		t.Fatalf("precondition: the seat's origin/main = %s, want the stale %s", got, base)
	}

	if err := d.refreshAuthorSeat(authorSeatRig, authorSeatPolecat); err != nil {
		t.Fatalf("refreshAuthorSeat: %v", err)
	}

	if got := f.Ref(workDir, "refs/remotes/origin/main"); got != landed {
		t.Fatalf("the seat's origin/main after the refresh = %s, want the landed %s", got, landed)
	}
	if got := f.Ref(workDir, "refs/remotes/origin/other"); got != base {
		t.Fatalf("origin/other after the refresh = %s, want it untouched at %s", got, base)
	}
	if after, err := g.Rev("HEAD"); err != nil || after != head {
		t.Fatalf("HEAD after the refresh = %s, %v; want the seat's own %s", after, err, head)
	}
}

// TestRefreshAuthorSeatSkipsASeatWithNoWorktree: a seat whose directory holds
// no git worktree is a reportable skip, not a fallback to a parent
// repository — git would resolve upward and the refresh would be about
// somebody else's tree.
func TestRefreshAuthorSeatSkipsASeatWithNoWorktree(t *testing.T) {
	t.Parallel()
	d, _, workDir := seatDaemon(t)
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}

	err := d.refreshAuthorSeat(authorSeatRig, authorSeatPolecat)
	if err == nil || !strings.Contains(err.Error(), "no git worktree") {
		t.Fatalf("refreshAuthorSeat = %v; want a no-worktree error", err)
	}
}

// TestRefreshAuthorSeatReportsAFailedFetch: an unreadable worktree is
// returned to the caller, which logs it once; it is never a panic and never
// anything the landing waits on.
func TestRefreshAuthorSeatReportsAFailedFetch(t *testing.T) {
	t.Parallel()
	d, _, workDir := seatDaemon(t)
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A worktree whose git directory does not exist: the fetch cannot run.
	if err := os.WriteFile(filepath.Join(workDir, ".git"), []byte("gitdir: /nonexistent\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := d.refreshAuthorSeat(authorSeatRig, authorSeatPolecat); err == nil {
		t.Fatal("refreshAuthorSeat of an unreadable worktree = nil; want the fetch's error")
	}
}
