package rig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/git/gitfake"
)

// fakeRepoOpener is ReadRepoFile's opener over a gitfake world.
func fakeRepoOpener(f *gitfake.Fake) RepoFileOpener {
	return func(repoPath string) RepoFileReader { return f.Open(repoPath) }
}

// bareRigRepo makes <townRoot>/<rigName>/.repo.git through gitfake with files
// committed on main, and with the origin/main ref a fetch of that main leaves
// behind. files nil commits a README.
func bareRigRepo(t *testing.T, f *gitfake.Fake, townRoot, rigName string, files map[string]string) string {
	t.Helper()
	if files == nil {
		files = map[string]string{"README.md": "# " + rigName + "\n"}
	}
	dir := filepath.Join(townRoot, rigName, ".repo.git")
	f.InitBare(t, dir)
	id := f.Commit(t, dir, "main", "main's tip", files)
	f.SetRef(t, dir, "refs/remotes/origin/main", id)
	return dir
}

// cloneFile writes relPath into the rig's mayor/rig clone, which no fake
// repository stands behind: the clone is read as a plain file.
func cloneFile(t *testing.T, townRoot, rigName, relPath, content string) string {
	t.Helper()
	path := filepath.Join(townRoot, rigName, "mayor", "rig", relPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestReadRepoFile_MainWinsOverAStaleClone is the gt-yj6g1 case: the file is
// tracked on the rig repo's main and the clone has not pulled since.
func TestReadRepoFile_MainWinsOverAStaleClone(t *testing.T) {
	t.Parallel()
	f := gitfake.New()
	townRoot := t.TempDir()
	repo := bareRigRepo(t, f, townRoot, "mango", map[string]string{".om.json": `{"rubric":["new"]}` + "\n"})
	cloneFile(t, townRoot, "mango", ".om.json", `{"rubric":["old"]}`+"\n")

	got, err := ReadRepoFile(fakeRepoOpener(f), townRoot, "mango", ".om.json")
	if err != nil {
		t.Fatalf("ReadRepoFile: %v", err)
	}
	if !got.Found || !strings.Contains(string(got.Data), "new") {
		t.Fatalf("Data = %q (found %v), want main's copy", got.Data, got.Found)
	}
	if want := "origin/main in " + repo; got.Source != want {
		t.Errorf("Source = %q, want %q", got.Source, want)
	}
}

// TestReadRepoFile_UntrackedOnMainFallsBackToTheClone keeps the clone's copy
// reachable for a file the repo does not commit: the pre-gt-yj6g1 behaviour.
func TestReadRepoFile_UntrackedOnMainFallsBackToTheClone(t *testing.T) {
	t.Parallel()
	f := gitfake.New()
	townRoot := t.TempDir()
	bareRigRepo(t, f, townRoot, "mango", nil)
	path := cloneFile(t, townRoot, "mango", ".gastown/settings.json", `{"type":"rig-settings"}`)

	got, err := ReadRepoFile(fakeRepoOpener(f), townRoot, "mango", ".gastown/settings.json")
	if err != nil {
		t.Fatalf("ReadRepoFile: %v", err)
	}
	if !got.Found || string(got.Data) != `{"type":"rig-settings"}` {
		t.Fatalf("Data = %q (found %v), want the clone's copy", got.Data, got.Found)
	}
	if got.Source != path {
		t.Errorf("Source = %q, want the clone's path %q", got.Source, path)
	}
}

// TestReadRepoFile_NoRepoFallsBackToTheClone covers the rig whose clones are
// all there is: no .repo.git at all.
func TestReadRepoFile_NoRepoFallsBackToTheClone(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	path := cloneFile(t, townRoot, "mango", ".om.json", "{}\n")

	got, err := ReadRepoFile(fakeRepoOpener(gitfake.New()), townRoot, "mango", ".om.json")
	if err != nil {
		t.Fatalf("ReadRepoFile: %v", err)
	}
	if !got.Found || got.Source != path {
		t.Fatalf("read = %+v, want the clone's copy at %s", got, path)
	}
}

// TestReadRepoFile_MainNotFetchedFallsBackToTheClone: a ref that is not there
// is main having nothing to prefer, not a broken repo.
func TestReadRepoFile_MainNotFetchedFallsBackToTheClone(t *testing.T) {
	t.Parallel()
	f := gitfake.New()
	townRoot := t.TempDir()
	dir := filepath.Join(townRoot, "mango", ".repo.git")
	f.InitBare(t, dir)
	f.Commit(t, dir, "main", "a local main, never fetched", map[string]string{".om.json": "{}\n"})
	cloneFile(t, townRoot, "mango", ".om.json", `{"stale":true}`)

	got, err := ReadRepoFile(fakeRepoOpener(f), townRoot, "mango", ".om.json")
	if err != nil {
		t.Fatalf("ReadRepoFile: %v", err)
	}
	if !got.Found || !strings.Contains(string(got.Data), "stale") {
		t.Fatalf("Data = %q (found %v), want the clone's copy", got.Data, got.Found)
	}
}

// TestReadRepoFile_UnreadableRepoFailsClosed: a .repo.git this run cannot read
// is an error naming the rig, never the stale clone's copy it exists to stop
// trusting (gt-yj6g1).
func TestReadRepoFile_UnreadableRepoFailsClosed(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "mango", ".repo.git"), 0o755); err != nil {
		t.Fatal(err)
	}
	cloneFile(t, townRoot, "mango", ".om.json", `{"stale":true}`)

	got, err := ReadRepoFile(fakeRepoOpener(gitfake.New()), townRoot, "mango", ".om.json")
	if err == nil {
		t.Fatalf("ReadRepoFile = %+v, want an error for an unreadable repo", got)
	}
	if !strings.Contains(err.Error(), "mango") {
		t.Errorf("error %q does not name the rig", err)
	}
}

// TestReadRepoFile_NeitherCopyIsNotFound: nothing to read is an answer, so a
// rig with no repo and no file is no error.
func TestReadRepoFile_NeitherCopyIsNotFound(t *testing.T) {
	t.Parallel()
	got, err := ReadRepoFile(fakeRepoOpener(gitfake.New()), t.TempDir(), "mango", ".om.json")
	if err != nil || got.Found {
		t.Fatalf("ReadRepoFile = %+v, %v; want not found", got, err)
	}
}

// TestResolveMergeQueueConfig_RepoTierFollowsMain: the repo-committed tier is
// main's copy, so a gate command that changed on main takes effect before the
// clone catches up.
func TestResolveMergeQueueConfig_RepoTierFollowsMain(t *testing.T) {
	t.Parallel()
	f := gitfake.New()
	townRoot := t.TempDir()
	bareRigRepo(t, f, townRoot, "mango", map[string]string{
		".gastown/settings.json": `{"type":"rig-settings","version":1,"merge_queue":{"test_command":"make test-main"}}`,
	})
	cloneFile(t, townRoot, "mango", ".gastown/settings.json",
		`{"type":"rig-settings","version":1,"merge_queue":{"test_command":"make test-stale"}}`)

	mq := ResolveMergeQueueConfigIn(fakeRepoOpener(f), townRoot, "mango")
	if mq == nil {
		t.Fatal("ResolveMergeQueueConfigIn() = nil, want the repo tier")
	}
	if mq.TestCommand != "make test-main" {
		t.Errorf("TestCommand = %q, want %q (main's copy)", mq.TestCommand, "make test-main")
	}
}

// TestResolveMergeQueueConfig_UnreadableRepoDropsRepoTier: a repo tier this run
// cannot read is dropped with a warning naming the rig, not resolved from the
// clone (gt-yj6g1).
func TestResolveMergeQueueConfig_UnreadableRepoDropsRepoTier(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	rigName := "mango-unreadable-repo"
	if err := os.MkdirAll(filepath.Join(townRoot, rigName, ".repo.git"), 0o755); err != nil {
		t.Fatal(err)
	}
	cloneFile(t, townRoot, rigName, ".gastown/settings.json",
		`{"type":"rig-settings","version":1,"merge_queue":{"test_command":"make test-stale"}}`)
	rigConfig := `{"type":"rig","version":1,"name":"` + rigName + `","git_url":"x","merge_queue":{"lint_command":"make lint-floor"}}`
	if err := os.WriteFile(filepath.Join(townRoot, rigName, "config.json"), []byte(rigConfig), 0o644); err != nil {
		t.Fatal(err)
	}

	mq := ResolveMergeQueueConfigIn(fakeRepoOpener(gitfake.New()), townRoot, rigName)
	if mq == nil {
		t.Fatal("ResolveMergeQueueConfigIn() = nil, want the rig-root floor")
	}
	if mq.TestCommand != "" {
		t.Errorf("TestCommand = %q, want empty: the unreadable repo tier is dropped, the clone is not consulted", mq.TestCommand)
	}
	if mq.LintCommand != "make lint-floor" {
		t.Errorf("LintCommand = %q, want the rig-root floor still resolved", mq.LintCommand)
	}
	if !RepoFileUnreadableWarned(rigName) {
		t.Error("the dropped repo tier was not reported")
	}
}
