package doctor

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/git/gitfake"
)

func TestNewForeignRemoteCheck(t *testing.T) {
	t.Parallel()
	check := NewForeignRemoteCheck()

	if check.Name() != "foreign-remotes" {
		t.Errorf("expected name 'foreign-remotes', got %q", check.Name())
	}

	if check.Description() == "" {
		t.Error("expected non-empty description")
	}

	if !check.CanFix() {
		t.Error("expected CanFix() to return true")
	}
}

func TestForeignRemoteCheck_NoGitRepo(t *testing.T) {
	t.Parallel()
	ctx := withGit(&CheckContext{TownRoot: t.TempDir()}, gitfake.New())
	result := NewForeignRemoteCheck().Run(ctx)

	if result.Status != StatusSkipped {
		t.Errorf("expected StatusSkipped for non-git dir (could not list remotes), got %v", result.Status)
	}
}

func TestForeignRemoteCheck_OnlyOrigin(t *testing.T) {
	t.Parallel()
	f := gitfake.New()
	town := filepath.Join(t.TempDir(), "town")
	fakeClone(t, f, town)

	result := NewForeignRemoteCheck().Run(withGit(&CheckContext{TownRoot: town}, f))
	if result.Status != StatusOK {
		t.Errorf("expected StatusOK with only origin, got %v: %s", result.Status, result.Message)
	}
}

// foreignTown is a town clone with an extra remote "gastown" whose history
// shares nothing with origin's.
func foreignTown(t *testing.T) (*gitfake.Fake, string) {
	t.Helper()
	f := gitfake.New()
	root := t.TempDir()
	town := filepath.Join(root, "town")
	fakeClone(t, f, town)
	fetchRemote(t, f, town, "gastown", fakeRemote(t, f, filepath.Join(root, "gastown.git"), map[string]string{"README.md": "# Separate\n"}))
	return f, town
}

func TestForeignRemoteCheck_DetectsForeignRemote(t *testing.T) {
	t.Parallel()
	f, town := foreignTown(t)
	check := NewForeignRemoteCheck()
	result := check.Run(withGit(&CheckContext{TownRoot: town}, f))

	if result.Status != StatusWarning {
		t.Errorf("expected StatusWarning for foreign remote, got %v: %s", result.Status, result.Message)
	}
	if len(check.foreignRemotes) != 1 || check.foreignRemotes[0].name != "gastown" {
		t.Fatalf("foreign remotes = %+v, want gastown", check.foreignRemotes)
	}
	if !strings.HasSuffix(check.foreignRemotes[0].url, "gastown.git") {
		t.Errorf("foreign remote url = %q", check.foreignRemotes[0].url)
	}
}

// A remote whose main shares history with origin's is not foreign, and one
// with no main or master cannot be judged and is left alone.
func TestForeignRemoteCheck_IgnoresRelatedRemote(t *testing.T) {
	t.Parallel()
	f := gitfake.New()
	root := t.TempDir()
	town := filepath.Join(root, "town")
	origin := fakeClone(t, f, town)
	backup := filepath.Join(root, "backup.git")
	f.InitBare(t, backup)
	f.SetRef(t, backup, "refs/heads/main", f.Ref(origin, "refs/heads/main"))
	f.Commit(t, backup, "main", "more", map[string]string{"b.txt": "b\n"})
	fetchRemote(t, f, town, "backup", backup)
	branchless := filepath.Join(root, "branchless.git")
	f.InitBare(t, branchless)
	f.Commit(t, branchless, "feature", "unrelated", map[string]string{"x": "x\n"})
	fetchRemote(t, f, town, "branchless", branchless)

	result := NewForeignRemoteCheck().Run(withGit(&CheckContext{TownRoot: town}, f))
	if result.Status != StatusOK {
		t.Errorf("expected StatusOK for related remote, got %v: %s", result.Status, result.Message)
	}
}

// With no origin/main (origin only has master, or was never fetched) there is
// nothing to compare against: the check skips, no remote is reported foreign,
// and Fix must have nothing to remove.
func TestForeignRemoteCheck_NoOriginMainIsNotForeign(t *testing.T) {
	t.Parallel()
	f, town := foreignTown(t)
	backup := filepath.Join(filepath.Dir(town), "backup.git")
	f.InitBare(t, backup)
	f.SetRef(t, backup, "refs/heads/main", f.Ref(filepath.Join(filepath.Dir(town), "town-origin.git"), "refs/heads/main"))
	f.Commit(t, backup, "main", "more", map[string]string{"b.txt": "b\n"})
	fetchRemote(t, f, town, "backup", backup)
	f.DeleteRef(t, town, "refs/remotes/origin/main")
	ctx := withGit(&CheckContext{TownRoot: town}, f)
	check := NewForeignRemoteCheck()

	result := check.Run(ctx)
	if len(check.foreignRemotes) != 0 {
		t.Fatalf("foreign remotes = %+v, want none", check.foreignRemotes)
	}
	if result.Status != StatusSkipped {
		t.Errorf("expected StatusSkipped without origin/main, got %v: %s", result.Status, result.Message)
	}
	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix failed: %v", err)
	}
	remotes, err := ctx.git(town).Remotes()
	if err != nil {
		t.Fatal(err)
	}
	if len(remotes) != 3 {
		t.Errorf("remotes after Fix = %v, want origin, gastown and backup kept", remotes)
	}
}

// mergeBaseFails is the repo seam whose merge-base fails for a reason other
// than "no common ancestor": a bad object or a timeout, not exit 1.
type mergeBaseFails struct{ Repo }

var errMergeBaseFailed = errors.New("fatal: unable to read tree")

func (mergeBaseFails) MergeBase(string, string) (string, error) { return "", errMergeBaseFailed }

// refExistsFails is the repo seam an unreadable repository answers on.
type refExistsFails struct{ Repo }

var errRefsUnreadable = errors.New("fatal: not a git repository")

func (refExistsFails) RefExists(string) (bool, error) { return false, errRefsUnreadable }

// A merge-base that fails for any reason other than "no common ancestor"
// leaves the remote unknown, and unknown is not a pass.
func TestForeignRemoteCheck_MergeBaseFailureIsSkipped(t *testing.T) {
	t.Parallel()
	f, town := foreignTown(t)
	ctx := withGit(&CheckContext{TownRoot: town}, f)
	ctx.openGit = func(gitDir, workDir string) Repo {
		return mergeBaseFails{f.OpenWithDir(gitDir, workDir).(Repo)}
	}
	check := NewForeignRemoteCheck()
	result := check.Run(ctx)

	if result.Status != StatusSkipped {
		t.Errorf("expected StatusSkipped when merge-base fails, got %v: %s", result.Status, result.Message)
	}
	if !strings.Contains(result.Message, "gastown") {
		t.Errorf("message = %q, want the unreadable remote named", result.Message)
	}
	if len(check.foreignRemotes) != 0 {
		t.Errorf("foreign remotes = %+v, want none", check.foreignRemotes)
	}
}

// Refs that cannot be read leave origin/main's existence unknown, not absent.
func TestForeignRemoteCheck_RefExistsFailureIsSkipped(t *testing.T) {
	t.Parallel()
	f, town := foreignTown(t)
	ctx := withGit(&CheckContext{TownRoot: town}, f)
	ctx.openGit = func(gitDir, workDir string) Repo {
		return refExistsFails{f.OpenWithDir(gitDir, workDir).(Repo)}
	}
	result := NewForeignRemoteCheck().Run(ctx)

	if result.Status != StatusSkipped {
		t.Errorf("expected StatusSkipped when refs cannot be read, got %v: %s", result.Status, result.Message)
	}
	if len(result.Details) == 0 || !strings.Contains(result.Details[0], errRefsUnreadable.Error()) {
		t.Errorf("details = %v, want the RefExists error", result.Details)
	}
}

func TestForeignRemoteCheck_FixRemovesForeignRemotes(t *testing.T) {
	t.Parallel()
	f, town := foreignTown(t)
	ctx := withGit(&CheckContext{TownRoot: town}, f)
	check := NewForeignRemoteCheck()

	if result := check.Run(ctx); result.Status != StatusWarning {
		t.Fatalf("expected StatusWarning, got %v", result.Status)
	}
	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix failed: %v", err)
	}
	if result := check.Run(ctx); result.Status != StatusOK {
		t.Errorf("expected StatusOK after fix, got %v: %s", result.Status, result.Message)
	}
}
