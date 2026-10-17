package rig

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/git/gitfake"
)

// fakeRepoOpener is ReadRepoFile's opener over a gitfake world.
func fakeRepoOpener(f *gitfake.Fake) RepoFileOpener {
	return func(repoPath string) RepoFileReader { return f.Open(repoPath) }
}

// stubRepo answers ReadRepoFile's two reads with what a test set, so the
// branches that need a repo failure are driven on purpose rather than by a
// fake world's accident of a directory with no repository behind it.
type stubRepo struct {
	refExists bool
	existsErr error
	data      string
	fileErr   error
}

func (s stubRepo) RefExists(string) (bool, error) { return s.refExists, s.existsErr }

func (s stubRepo) ShowFileAtRev(string, string) (string, error) { return s.data, s.fileErr }

// stubOpener is ReadRepoFile's opener over one stubRepo.
func stubOpener(s RepoFileReader) RepoFileOpener {
	return func(string) RepoFileReader { return s }
}

// rigRepoDir makes the .repo.git directory ReadRepoFile looks for, which is
// what puts a read on the repo tier at all.
func rigRepoDir(t *testing.T, townRoot, rigName string) string {
	t.Helper()
	dir := filepath.Join(townRoot, rigName, ".repo.git")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
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
	rigRepoDir(t, townRoot, "mango")
	cloneFile(t, townRoot, "mango", ".om.json", `{"stale":true}`)

	refErr := errors.New("fatal: not a git repository")
	got, err := ReadRepoFile(stubOpener(stubRepo{existsErr: refErr}), townRoot, "mango", ".om.json")
	if err == nil {
		t.Fatalf("ReadRepoFile = %+v, want an error for an unreadable repo", got)
	}
	if !strings.Contains(err.Error(), "mango") {
		t.Errorf("error %q does not name the rig", err)
	}
	if !errors.Is(err, refErr) {
		t.Errorf("error %q does not carry the read failure", err)
	}
}

// TestReadRepoFile_UnreadableShowFailsClosed: a file read that fails for any
// reason but "not at this ref" is the repo, not the file, and no fallback may
// paper over it (gt-yj6g1).
func TestReadRepoFile_UnreadableShowFailsClosed(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	rigRepoDir(t, townRoot, "mango")
	cloneFile(t, townRoot, "mango", ".om.json", `{"stale":true}`)

	showErr := errors.New("fatal: bad object refs/remotes/origin/main")
	got, err := ReadRepoFile(
		stubOpener(stubRepo{refExists: true, fileErr: showErr}), townRoot, "mango", ".om.json")
	if err == nil {
		t.Fatalf("ReadRepoFile = %+v, want an error for a repo that cannot read its own main", got)
	}
	if !errors.Is(err, showErr) {
		t.Errorf("error %q does not carry the read failure", err)
	}
}

// TestReadRepoFile_FileNotOnMainIsTheClone's: main answered for the path and
// carries no such file, so what the clone still holds is the clone's copy — a
// rubric main deleted must not read as one main carries (gt-yj6g1).
func TestReadRepoFile_FileNotOnMainIsTheClone(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	rigRepoDir(t, townRoot, "mango-deleted-on-main")
	path := cloneFile(t, townRoot, "mango-deleted-on-main", ".om.json", `{"deleted":true}`)

	got, err := ReadRepoFile(
		stubOpener(stubRepo{refExists: true, fileErr: git.ErrNotAtRef}), townRoot, "mango-deleted-on-main", ".om.json")
	if err != nil {
		t.Fatalf("ReadRepoFile: %v", err)
	}
	if !got.Found || got.Source != path {
		t.Fatalf("read = %+v, want the clone's copy at %s", got, path)
	}
	if !strings.Contains(got.Fallback, "origin/main does not carry it") {
		t.Errorf("Fallback = %q, want main named as not carrying the file", got.Fallback)
	}
	if !RepoFileCloneFallbackWarned("mango-deleted-on-main", ".om.json") {
		t.Error("the clone-sourced answer was not reported")
	}
}

// TestReadRepoFile_NoFetchedMainIsTheClone's: a repo with no origin/<branch>
// ref has no main to prefer, so the clone's copy answers — reported as the
// clone's, which is the answer a rig that never fetches its repo would
// otherwise serve as main's (gt-yj6g1).
func TestReadRepoFile_NoFetchedMainIsTheClone(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	rigRepoDir(t, townRoot, "mango-unfetched")
	path := cloneFile(t, townRoot, "mango-unfetched", ".om.json", `{"unfetched":true}`)

	got, err := ReadRepoFile(stubOpener(stubRepo{}), townRoot, "mango-unfetched", ".om.json")
	if err != nil {
		t.Fatalf("ReadRepoFile: %v", err)
	}
	if !got.Found || got.Source != path {
		t.Fatalf("read = %+v, want the clone's copy at %s", got, path)
	}
	if !strings.Contains(got.Fallback, "no origin/main ref") {
		t.Errorf("Fallback = %q, want the missing ref named", got.Fallback)
	}
	if !RepoFileCloneFallbackWarned("mango-unfetched", ".om.json") {
		t.Error("the clone-sourced answer was not reported")
	}
}

// TestReadRepoFile_MainAnswersWithoutAFallback: a read main answers carries no
// fallback, which is what tells a caller that must follow main the two apart.
func TestReadRepoFile_MainAnswersWithoutAFallback(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	rigRepoDir(t, townRoot, "mango-on-main")
	cloneFile(t, townRoot, "mango-on-main", ".om.json", `{"stale":true}`)

	got, err := ReadRepoFile(
		stubOpener(stubRepo{refExists: true, data: `{"from":"main"}`}), townRoot, "mango-on-main", ".om.json")
	if err != nil {
		t.Fatalf("ReadRepoFile: %v", err)
	}
	if string(got.Data) != `{"from":"main"}` {
		t.Errorf("Data = %q, want main's copy", got.Data)
	}
	if got.Fallback != "" {
		t.Errorf("Fallback = %q, want empty for an answer main gave", got.Fallback)
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
	rigRepoDir(t, townRoot, rigName)
	cloneFile(t, townRoot, rigName, ".gastown/settings.json",
		`{"type":"rig-settings","version":1,"merge_queue":{"test_command":"make test-stale"}}`)
	rigConfig := `{"type":"rig","version":1,"name":"` + rigName + `","git_url":"x","merge_queue":{"lint_command":"make lint-floor"}}`
	if err := os.WriteFile(filepath.Join(townRoot, rigName, "config.json"), []byte(rigConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	unreadable := stubOpener(stubRepo{existsErr: errors.New("fatal: not a git repository")})

	mq := ResolveMergeQueueConfigIn(unreadable, townRoot, rigName)
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

// TestResolveMergeQueueConfig_RepoReadingAgainReportsTheNextDrop: the dropped
// tier is reported once per run of failures, not once per process, so a repo
// that fails again after reading fine is not silent (gt-yj6g1).
func TestResolveMergeQueueConfig_RepoReadingAgainReportsTheNextDrop(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	rigName := "mango-repo-flaps"
	rigRepoDir(t, townRoot, rigName)
	unreadable := stubOpener(stubRepo{existsErr: errors.New("fatal: not a git repository")})

	ResolveMergeQueueConfigIn(unreadable, townRoot, rigName)
	if !RepoFileUnreadableWarned(rigName) {
		t.Fatal("the first dropped repo tier was not reported")
	}

	// A read that answers proves the repo is readable again.
	readable := stubOpener(stubRepo{refExists: true, fileErr: git.ErrNotAtRef})
	ResolveMergeQueueConfigIn(readable, townRoot, rigName)
	if RepoFileUnreadableWarned(rigName) {
		t.Error("the rig is still reported unreadable after a read that answered")
	}

	ResolveMergeQueueConfigIn(unreadable, townRoot, rigName)
	if !RepoFileUnreadableWarned(rigName) {
		t.Error("the drop that followed a read that worked was not reported")
	}
}

// TestResolveLandingRemoteWith_MalformedRepoSettingsFallsBack: a repo-committed
// settings file that is there and readable but does not parse drops the tier
// and still resolves to origin. Failing closed on it instead stops the rig's
// landing worker and every gt done on the rig, and the commit that repairs the
// file is one of them (gt-mvbbe).
func TestResolveLandingRemoteWith_MalformedRepoSettingsFallsBack(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	rigName := "mango-malformed-landing"
	rigRepoDir(t, townRoot, rigName)
	repo := &landingRemotesFake{remotes: []string{"origin"}, urls: map[string]string{"origin": "https://github.com/acme/mango.git"}}
	malformed := stubOpener(stubRepo{refExists: true, data: `{"type":"rig-settings","merge_queue":{`})

	got, err := ResolveLandingRemoteWith(malformed, repo, townRoot, rigName)
	if err != nil {
		t.Fatalf("ResolveLandingRemoteWith() = %q, %v; want origin and no error", got, err)
	}
	if got != LandingRemoteOrigin {
		t.Errorf("ResolveLandingRemoteWith() = %q, want %q", got, LandingRemoteOrigin)
	}
	if !RepoFileUnreadableWarned(rigName) {
		t.Error("the dropped repo tier was not reported")
	}
}

// TestResolveMergeQueueConfig_MalformedRepoTierIsReportedOnce: a malformed file
// answers every read, so the tier stays dropped between resolutions — the
// daemon resolves once per patrol per rig, and the warning is news once per
// failure rather than once per resolution (gt-mvbbe).
func TestResolveMergeQueueConfig_MalformedRepoTierIsReportedOnce(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	rigName := "mango-malformed-once"
	rigRepoDir(t, townRoot, rigName)
	malformed := stubOpener(stubRepo{refExists: true, data: `{"type":"rig-settings","merge_queue":{`})

	ResolveMergeQueueConfigIn(malformed, townRoot, rigName)
	if !RepoFileUnreadableWarned(rigName) {
		t.Fatal("the dropped repo tier was not reported")
	}
	// A report that is cleared and printed again replaces this marker.
	const marker = "first report"
	repoFileWarnedRigs.Store(rigName, marker)
	ResolveMergeQueueConfigIn(malformed, townRoot, rigName)

	if got, _ := repoFileWarnedRigs.Load(rigName); got != marker {
		t.Error("the dropped repo tier was reported again on the second resolution, want once")
	}
}

// TestReadRepoFile_AnsweringReadKeepsTheDropReported: a read that answers is not
// a resolution that stopped dropping the tier, so ReadRepoFile — the editorial
// check calls it too — leaves a report in place (gt-mvbbe).
func TestReadRepoFile_AnsweringReadKeepsTheDropReported(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	rigName := "mango-read-keeps-report"
	rigRepoDir(t, townRoot, rigName)
	WarnRepoFileUnreadable(rigName, errors.New("parsing: invalid character"))

	open := stubOpener(stubRepo{refExists: true, data: `{}`})
	if _, err := ReadRepoFile(open, townRoot, rigName, ".gastown/settings.json"); err != nil {
		t.Fatalf("ReadRepoFile() error = %v", err)
	}
	if !RepoFileUnreadableWarned(rigName) {
		t.Error("an answering read cleared the report of a dropped tier")
	}
}

// TestResolveLandingRemoteWith_UnreadableRepoFailsClosed: a repo-committed
// block this run cannot read may name another host, so the landing remote is
// not origin either — the rig lands through its Forgejo remote or not at all
// (gt-fn9e6.18, gt-yj6g1).
func TestResolveLandingRemoteWith_UnreadableRepoFailsClosed(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	rigName := "mango-unreadable-landing"
	rigRepoDir(t, townRoot, rigName)
	repo := &landingRemotesFake{
		remotes: []string{"origin", "forgejo"},
		urls: map[string]string{
			"origin":  "https://github.com/acme/mango.git",
			"forgejo": "https://forgejo.example/acme/mango.git",
		},
	}
	unreadable := stubOpener(stubRepo{existsErr: errors.New("fatal: not a git repository")})

	got, err := ResolveLandingRemoteWith(unreadable, repo, townRoot, rigName)
	if err == nil {
		t.Fatalf("ResolveLandingRemoteWith() = %q, nil; want an error rather than a remote no read supports", got)
	}
	if !strings.Contains(err.Error(), rigName) {
		t.Errorf("error %q does not name the rig", err)
	}
	if repo.calls != 0 {
		t.Errorf("read the rig's remotes %d time(s) for a block that could not be resolved; want none", repo.calls)
	}
}

// TestResolveForgejoConfigIn_RepoTierReadsTheOpener: the Forgejo block resolves
// over open, so a caller whose unit tier runs no git — the daemon's is
// git-free (internal/testpolicy/gitfree.txt) — reads the repo-committed tier
// through its own seam. The tier is read rather than skipped: the block is
// stripped (a landed commit never chooses where the token goes), and the copy
// that carried it is reported (gt-yj6g1).
func TestResolveForgejoConfigIn_RepoTierReadsTheOpener(t *testing.T) {
	t.Parallel()
	f := gitfake.New()
	townRoot := t.TempDir()
	rigName := "mango-forgejo-seam"
	repo := bareRigRepo(t, f, townRoot, rigName, map[string]string{
		".gastown/settings.json": `{"type":"rig-settings","version":1,"merge_queue":{"forgejo":{"remote_url":"https://attacker.example/mango"}}}`,
	})
	// The clone carries no block: the tier is main's copy as the seam reads it,
	// not whatever a run that never opened the seam would find.
	cloneFile(t, townRoot, rigName, ".gastown/settings.json", `{"type":"rig-settings","version":1}`)
	rigConfig := `{"type":"rig","version":1,"name":"` + rigName + `","merge_queue":{"forgejo":{"remote_url":"https://forgejo.example/floor/mango"}}}`
	if err := os.WriteFile(filepath.Join(townRoot, rigName, "config.json"), []byte(rigConfig), 0o644); err != nil {
		t.Fatal(err)
	}

	fc := ResolveForgejoConfigIn(fakeRepoOpener(f), townRoot, rigName)
	if fc == nil || fc.RemoteURL != "https://forgejo.example/floor/mango" {
		t.Fatalf("ResolveForgejoConfigIn() = %+v, want the rig-root floor block: the repo tier cannot carry one", fc)
	}
	if want := "origin/main in " + repo; !RepoForgejoIgnoredWarned(want) {
		t.Errorf("RepoForgejoIgnoredWarned(%s) = false, want the ignored block reported from main's copy", want)
	}
}

// TestResolveLandingRemoteWith_ResolvesThroughTheOpener: the landing remote is
// resolved over the same seam, so the git-free unit tiers resolve a rig's
// Forgejo block without starting git (gt-yj6g1).
func TestResolveLandingRemoteWith_ResolvesThroughTheOpener(t *testing.T) {
	t.Parallel()
	f := gitfake.New()
	townRoot := t.TempDir()
	rigName := "mango-landing-seam"
	repoDir := bareRigRepo(t, f, townRoot, rigName, nil)
	f.AddRemote(t, repoDir, "origin", "https://github.com/acme/mango.git")
	f.AddRemote(t, repoDir, "forgejo", "https://forgejo.example/acme/mango.git")
	rigConfig := `{"type":"rig","version":1,"name":"` + rigName + `","merge_queue":{"forgejo":{"remote_url":"https://forgejo.example/acme/mango.git"}}}`
	if err := os.WriteFile(filepath.Join(townRoot, rigName, "config.json"), []byte(rigConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	repo, ok := f.Open(repoDir).(LandingRemotes)
	if !ok {
		t.Fatal("gitfake does not implement LandingRemotes")
	}

	got, err := ResolveLandingRemoteWith(fakeRepoOpener(f), repo, townRoot, rigName)
	if err != nil {
		t.Fatalf("ResolveLandingRemoteWith: %v", err)
	}
	if got != "forgejo" {
		t.Errorf("ResolveLandingRemoteWith() = %q, want forgejo (the remote whose URL the block names)", got)
	}
}
