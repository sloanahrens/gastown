//go:build integration

package scorecard

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/land"
)

// Sync lets repoGit stand in for the submission battery's bare clone, whose
// Sync fetches the branches before the reads.
func (repoGit) Sync(context.Context) error { return nil }

// TestIntegrationConflictLandedFilesReadsARealFork holds the fake repo's diff
// semantics to git's on the case the fake stands in for: a landing record whose
// base advanced past the bead's fork. Real git must list the bead's own file
// alone, a two-dot range over the same commits must list more, and the fake
// repo must return what git returned (gt-9lpp7).
func TestIntegrationConflictLandedFilesReadsARealFork(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	at := time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC)
	dir := fixtureRepo(t, []fixtureCommit{
		{at: at, files: map[string]string{"base.txt": "base"}},
		{at: at.Add(time.Minute), files: map[string]string{"a.go": "a"}},
	})
	git := repoGit{dir: dir}
	gitWith := func(args ...string) string {
		t.Helper()
		full := append([]string{"-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "-c", "commit.gpgsign=false"}, args...)
		out, err := git.Run(ctx, full...)
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(out)
	}

	// gt-b forks from the root, before gt-o's commit landed on main, and its
	// own landing record names the tip gt-o left as the base.
	b := gitWith("rev-parse", "HEAD~1")
	landed := gitWith("rev-parse", "HEAD")
	gitWith("checkout", "-q", "-b", "fork", b)
	if err := os.WriteFile(filepath.Join(dir, "b.go"), []byte("b"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitWith("add", "-A")
	gitWith("commit", "-q", "-m", "gt-b")
	head := gitWith("rev-parse", "HEAD")

	files := func(spec string) map[string]bool {
		t.Helper()
		out, err := git.Run(ctx, "diff", "--name-only", "--no-renames", spec)
		if err != nil {
			t.Fatal(err)
		}
		found := map[string]bool{}
		for _, f := range strings.Split(strings.TrimSpace(out), "\n") {
			if f != "" {
				found[f] = true
			}
		}
		return found
	}
	// The fixture only proves anything if the two ranges part company on it.
	if spanned := files(landed + ".." + head); !spanned["a.go"] {
		t.Fatalf("a two-dot range over the fixture = %v, want it to read gt-o's landing backwards", spanned)
	}
	want := map[string]bool{"b.go": true}
	if got := files(landed + "..." + head); !reflect.DeepEqual(got, want) {
		t.Errorf("git's three-dot range = %v, want %v", got, want)
	}

	town := t.TempDir()
	real := newConflictAdapter(town, func() (conflictRepo, error) { return git, nil })
	realFiles, err := real.landedFiles(ctx, "gt-b", &land.LandingRecord{Base: landed, Head: head})
	if err != nil {
		t.Fatal(err)
	}

	// The same history in the fake the unit tests read.
	fake, sha := linearHistory([]map[string]string{{"base.txt": "base"}, {"a.go": "a"}})
	fakeHead := fake.fork(t, sha[0], map[string]string{"b.go": "b"})
	fakeAdapter := newConflictAdapter(t.TempDir(), func() (conflictRepo, error) { return fake, nil })
	fakeFiles, err := fakeAdapter.landedFiles(ctx, "gt-b", &land.LandingRecord{Base: sha[1], Head: fakeHead})
	if err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(realFiles, want) {
		t.Errorf("the landing's files = %v, want %v", realFiles, want)
	}
	if !reflect.DeepEqual(fakeFiles, realFiles) {
		t.Errorf("the fake repo reads %v where git reads %v", fakeFiles, realFiles)
	}
}
