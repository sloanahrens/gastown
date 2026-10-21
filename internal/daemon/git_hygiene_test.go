package daemon

import (
	"bytes"
	"errors"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/rig"
)

// newGitHygieneDaemon is a daemon over a temp town whose mayor/rigs.json lists
// rigs, with git, the feed and the log captured.
func newGitHygieneDaemon(t *testing.T, rigs ...string) (*Daemon, *feedCapture, *bytes.Buffer) {
	t.Helper()
	town := t.TempDir()
	entries := make([]string, 0, len(rigs))
	for _, r := range rigs {
		entries = append(entries, `"`+r+`": {}`)
	}
	if err := os.MkdirAll(filepath.Join(town, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(town, "mayor", "rigs.json"), []byte(`{"rigs": {`+strings.Join(entries, ",")+`}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	feed := &feedCapture{}
	var logs bytes.Buffer
	d := &Daemon{config: &Config{TownRoot: town}, logger: log.New(&logs, "", 0), dogFeedFn: feed.record}
	return d, feed, &logs
}

func TestGitHygieneRepoPath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// layout is what the rig root holds: "bare" for the shared .repo.git,
		// "emptybare" for a directory with that name and no repository behind
		// it, "clone" for a mayor/rig working tree.
		layout []string
		want   string // the repository to clean, relative to the rig root; "" for none
	}{
		{name: "shared bare repo only", layout: []string{"bare"}, want: ".repo.git"},
		{name: "bare repo beside the working clone", layout: []string{"bare", "clone"}, want: ".repo.git"},
		{name: "working clone only", layout: []string{"clone"}, want: filepath.Join("mayor", "rig")},
		{name: "empty .repo.git beside the working clone", layout: []string{"emptybare", "clone"}, want: filepath.Join("mayor", "rig")},
		{name: "empty .repo.git directory only", layout: []string{"emptybare"}, want: ""},
		{name: "no repository", layout: nil, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rigRoot := t.TempDir()
			for _, dir := range tt.layout {
				switch dir {
				case "bare":
					// A git directory, by the HEAD file Rig.BareRepoPath reads.
					mustMkdirAll(t, filepath.Join(rigRoot, ".repo.git"))
					writeFile(t, filepath.Join(rigRoot, ".repo.git", "HEAD"), "ref: refs/heads/main\n")
				case "emptybare":
					// The directory name with nothing behind it (gt-nm4b9).
					mustMkdirAll(t, filepath.Join(rigRoot, ".repo.git"))
				default:
					mustMkdirAll(t, filepath.Join(rigRoot, "mayor", "rig", ".git"))
				}
			}

			want := tt.want
			if want != "" {
				want = filepath.Join(rigRoot, want)
			}
			if got := gitHygieneRepoPath(&rig.Rig{Name: "r1", Path: rigRoot}); got != want {
				t.Errorf("gitHygieneRepoPath() = %q, want %q", got, want)
			}
		})
	}
}

// The preference for the shared bare repo is the patrol's alone: gt rig
// list --json and every other Rig.RepoPath caller still see the working clone.
func TestGitHygieneRepoPath_LeavesRepoPathAlone(t *testing.T) {
	t.Parallel()
	rigRoot := t.TempDir()
	clone := filepath.Join(rigRoot, "mayor", "rig")
	mustMkdirAll(t, filepath.Join(rigRoot, ".repo.git"))
	writeFile(t, filepath.Join(rigRoot, ".repo.git", "HEAD"), "ref: refs/heads/main\n")
	mustMkdirAll(t, filepath.Join(clone, ".git"))
	r := &rig.Rig{Name: "r1", Path: rigRoot}

	if got := r.RepoPath(); got != clone {
		t.Errorf("Rig.RepoPath() = %q, want the working clone %q", got, clone)
	}
	if got, want := gitHygieneRepoPath(r), filepath.Join(rigRoot, ".repo.git"); got != want {
		t.Errorf("gitHygieneRepoPath() = %q, want %q", got, want)
	}
}

func mustMkdirAll(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestRunGitHygiene_CleansMergedAndOrphanedBranches(t *testing.T) {
	t.Parallel()
	d, feed, logs := newGitHygieneDaemon(t, "r1")
	f := useGitfake(t, d)

	origin := filepath.Join(t.TempDir(), "origin.git")
	f.InitBare(t, origin)
	base := f.Commit(t, origin, "main", "seed", map[string]string{"a.txt": "a\n"})
	f.SetRef(t, origin, "refs/heads/polecat/merged", base)
	f.SetRef(t, origin, "refs/heads/dependabot/merged", base)
	f.SetRef(t, origin, "refs/heads/polecat/live", base)
	open := f.Commit(t, origin, "polecat/open", "open work", map[string]string{"o.txt": "o\n"})
	f.Commit(t, origin, "polecat/live", "live work", map[string]string{"l.txt": "l\n"})

	repo := filepath.Join(d.config.TownRoot, "r1", "mayor", "rig")
	f.Clone(t, origin, repo)
	// polecat/old's work landed on origin's main as a different commit (a
	// rebase or squash), so its patch is on origin though its SHA is not.
	f.Commit(t, origin, "main", "landed", map[string]string{"x.txt": "x\n"})
	f.SetRef(t, repo, "refs/heads/done", base)
	f.SetRef(t, repo, "refs/heads/merge/held", base)
	f.SetRef(t, repo, "refs/heads/polecat/old", base)
	f.Commit(t, repo, "polecat/old", "landed elsewhere", map[string]string{"x.txt": "x\n"})
	f.SetRef(t, repo, "refs/heads/polecat/live", base)
	f.Commit(t, repo, "polecat/live", "local live", map[string]string{"y.txt": "y\n"})
	f.SetRef(t, repo, "refs/heads/notes", base)
	f.Commit(t, repo, "notes", "someone's work", map[string]string{"n.txt": "n\n"})

	d.runGitHygiene()

	g := d.gitAt(repo)
	local, err := g.ListBranches("")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"main", "merge/held", "notes", "polecat/live"}; !slices.Equal(local, want) {
		t.Errorf("local branches = %v, want %v (done merged, polecat/old orphaned)", local, want)
	}
	refs, err := g.ListRemoteRefsWithHashes("origin", "refs/heads/")
	if err != nil {
		t.Fatal(err)
	}
	var remote []string
	for _, r := range refs {
		remote = append(remote, strings.TrimPrefix(r.Name, "refs/heads/"))
	}
	if want := []string{"dependabot/merged", "main", "polecat/live", "polecat/open"}; !slices.Equal(remote, want) {
		t.Errorf("origin branches = %v, want %v (only polecat/merged deleted)", remote, want)
	}
	if tip := f.Ref(origin, "refs/heads/polecat/open"); tip != open {
		t.Errorf("origin polecat/open = %s, want %s", tip, open)
	}
	if !strings.Contains(logs.String(), "1 repo(s): 1 merged, 1 orphan, 1 remote branch(es) deleted") {
		t.Errorf("log lacks the summary:\n%s", logs.String())
	}
	if len(feed.payloads) != 0 {
		t.Errorf("feed = %v, want no failed cycle", feed.payloads)
	}
	if _, found, err := loadPatrolLastRun(d.config.TownRoot, "git_hygiene"); !found || err != nil {
		t.Errorf("last run not recorded: found=%t err=%v", found, err)
	}
}

// TestRunGitHygiene_KeepsRefsLandedThroughThePrune is gt-as9v8: a landing
// leaves refs/landed/<bead> at the head it landed, and the sweep's fetch
// --prune, its local/remote branch passes and its gc must all leave every
// refs/landed/* alone. The refs are the only thing keeping a landed branch's
// history reachable once the branch is reaped, so a prune that took them
// would undo the landing's answer to "is this worktree's work landed?".
func TestRunGitHygiene_KeepsRefsLandedThroughThePrune(t *testing.T) {
	t.Parallel()
	d, feed, logs := newGitHygieneDaemon(t, "r1")
	f := useGitfake(t, d)

	origin := filepath.Join(t.TempDir(), "origin.git")
	f.InitBare(t, origin)
	base := f.Commit(t, origin, "main", "seed", map[string]string{"a.txt": "a\n"})
	// A landed head that is not on main: a squash landing writes a commit of
	// its own, so only the ref keeps the head alive.
	landed := f.Commit(t, origin, "polecat/gone", "landed work", map[string]string{"b.txt": "b\n"})
	f.SetRef(t, origin, "refs/landed/gt-abc", landed)
	f.SetRef(t, origin, "refs/heads/polecat/merged", base)

	repo := filepath.Join(d.config.TownRoot, "r1", "mayor", "rig")
	f.Clone(t, origin, repo)
	f.SetRef(t, repo, "refs/landed/gt-abc", landed)
	f.SetRef(t, repo, "refs/heads/polecat/done", base)

	d.runGitHygiene()

	// The sweep ran: it deleted the merged branches on both sides. Without
	// this the refs below survive a patrol that did nothing.
	if got := f.Ref(repo, "refs/heads/polecat/done"); got != "" {
		t.Errorf("local polecat/done = %q, want the merged branch deleted", got)
	}
	if got := f.Ref(origin, "refs/heads/polecat/merged"); got != "" {
		t.Errorf("origin polecat/merged = %q, want the merged branch deleted", got)
	}
	for _, dir := range []string{repo, origin} {
		if got := f.Ref(dir, "refs/landed/gt-abc"); got != landed {
			t.Errorf("refs/landed/gt-abc in %s = %q, want the landed head %q", dir, got, landed)
		}
	}
	if strings.Contains(logs.String(), "landed/") {
		t.Errorf("the hygiene cycle touched refs/landed:\n%s", logs.String())
	}
	if len(feed.payloads) != 0 {
		t.Errorf("feed = %v, want no failed cycle", feed.payloads)
	}
}

func TestRunGitHygiene_UnreadableOriginDeletesOnlyMergedBranches(t *testing.T) {
	t.Parallel()
	d, _, logs := newGitHygieneDaemon(t, "r1")
	f := useGitfake(t, d)

	origin := filepath.Join(t.TempDir(), "origin.git")
	f.InitBare(t, origin)
	base := f.Commit(t, origin, "main", "seed", map[string]string{"a.txt": "a\n"})
	repo := filepath.Join(d.config.TownRoot, "r1", "mayor", "rig")
	f.Clone(t, origin, repo)
	f.SetRef(t, repo, "refs/heads/polecat/done", base)
	f.SetRef(t, repo, "refs/heads/polecat/wip", base)
	f.Commit(t, repo, "polecat/wip", "unpushed", map[string]string{"w.txt": "w\n"})
	f.RemoveRepo(t, origin)

	d.runGitHygiene()

	local, _ := d.gitAt(repo).ListBranches("")
	if want := []string{"main", "polecat/wip"}; !slices.Equal(local, want) {
		t.Errorf("local branches = %v, want %v: without origin an unmerged branch cannot be called orphaned", local, want)
	}
	if !strings.Contains(logs.String(), "ls-remote origin") {
		t.Errorf("log does not report the unreadable origin:\n%s", logs.String())
	}
}

func TestRunGitHygiene_CleansARigThatHasOnlyTheSharedBareRepo(t *testing.T) {
	t.Parallel()
	d, feed, logs := newGitHygieneDaemon(t, "r1")
	f := useGitfake(t, d)

	origin := filepath.Join(t.TempDir(), "origin.git")
	f.InitBare(t, origin)
	base := f.Commit(t, origin, "main", "seed", map[string]string{"a.txt": "a\n"})
	f.SetRef(t, origin, "refs/heads/polecat/merged", base)
	f.Commit(t, origin, "polecat/live", "live work", map[string]string{"l.txt": "l\n"})
	// polecat/old's work landed on origin's main as a different commit.
	f.Commit(t, origin, "main", "landed", map[string]string{"x.txt": "x\n"})

	bare := filepath.Join(d.config.TownRoot, "r1", ".repo.git")
	f.InitBare(t, bare)
	f.AddRemote(t, bare, "origin", origin)
	f.SetRef(t, bare, "refs/heads/main", base)
	f.SetRef(t, bare, "refs/heads/done", base)
	f.SetRef(t, bare, "refs/heads/polecat/old", base)
	f.Commit(t, bare, "polecat/old", "landed elsewhere", map[string]string{"x.txt": "x\n"})
	f.Commit(t, bare, "polecat/live", "local live", map[string]string{"y.txt": "y\n"})

	d.runGitHygiene()

	g := d.gitAt(bare)
	local, err := g.ListBranches("")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"main", "polecat/live"}; !slices.Equal(local, want) {
		t.Errorf("local branches = %v, want %v (done merged, polecat/old orphaned)", local, want)
	}
	refs, err := g.ListRemoteRefsWithHashes("origin", "refs/heads/")
	if err != nil {
		t.Fatal(err)
	}
	var remote []string
	for _, r := range refs {
		remote = append(remote, strings.TrimPrefix(r.Name, "refs/heads/"))
	}
	if want := []string{"main", "polecat/live"}; !slices.Equal(remote, want) {
		t.Errorf("origin branches = %v, want %v (polecat/merged merged and deleted)", remote, want)
	}
	if !strings.Contains(logs.String(), "1 repo(s): 1 merged, 1 orphan, 1 remote branch(es) deleted") {
		t.Errorf("log lacks the summary:\n%s", logs.String())
	}
	if len(feed.payloads) != 0 {
		t.Errorf("feed = %v, want no failed cycle", feed.payloads)
	}
}

// A polecat that committed and never pushed leaves its branch ref in the shared
// bare repo with no worktree; the patrol must not make those commits
// unreachable (gt-a0jxl).
func TestRunGitHygiene_KeepsOrphanPolecatBranchWithUnpushedCommits(t *testing.T) {
	t.Parallel()
	d, feed, logs := newGitHygieneDaemon(t, "r1")
	f := useGitfake(t, d)

	origin := filepath.Join(t.TempDir(), "origin.git")
	f.InitBare(t, origin)
	base := f.Commit(t, origin, "main", "seed", map[string]string{"a.txt": "a\n"})

	bare := filepath.Join(d.config.TownRoot, "r1", ".repo.git")
	f.InitBare(t, bare)
	f.AddRemote(t, bare, "origin", origin)
	f.SetRef(t, bare, "refs/heads/main", base)
	f.SetRef(t, bare, "refs/heads/polecat/crashed", base)
	crashed := f.Commit(t, bare, "polecat/crashed", "never pushed", map[string]string{"x.txt": "x\n"})

	d.runGitHygiene()

	if tip := f.Ref(bare, "refs/heads/polecat/crashed"); tip != crashed {
		t.Errorf("polecat/crashed = %q, want %s: the unpushed commit must survive the patrol", tip, crashed)
	}
	if !strings.Contains(logs.String(), "0 merged, 0 orphan, 0 remote") {
		t.Errorf("log lacks a zero-orphan summary:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), "keep orphan polecat/crashed") {
		t.Errorf("log does not say why polecat/crashed was kept:\n%s", logs.String())
	}
	if len(feed.payloads) != 0 {
		t.Errorf("feed = %v, want no failed cycle", feed.payloads)
	}
}

// When the check cannot run, the branch stays.
func TestRunGitHygiene_KeepsOrphanPolecatBranchWhenTheUnpushedCheckFails(t *testing.T) {
	t.Parallel()
	d, _, logs := newGitHygieneDaemon(t, "r1")
	f := useGitfake(t, d)

	origin := filepath.Join(t.TempDir(), "origin.git")
	f.InitBare(t, origin)
	base := f.Commit(t, origin, "main", "seed", map[string]string{"a.txt": "a\n"})

	bare := filepath.Join(d.config.TownRoot, "r1", ".repo.git")
	f.InitBare(t, bare)
	f.AddRemote(t, bare, "origin", origin)
	f.SetRef(t, bare, "refs/heads/main", base)
	f.SetRef(t, bare, "refs/heads/polecat/crashed", base)
	crashed := f.Commit(t, bare, "polecat/crashed", "never pushed", map[string]string{"x.txt": "x\n"})

	openGit := d.openGitFn
	d.openGitFn = func(dir string) daemonGit {
		return cherryFails{openGit(dir)}
	}

	d.runGitHygiene()

	if tip := f.Ref(bare, "refs/heads/polecat/crashed"); tip != crashed {
		t.Errorf("polecat/crashed = %q, want %s: a failed check must keep the branch", tip, crashed)
	}
	if !strings.Contains(logs.String(), "cannot tell whether it holds unpushed commits") {
		t.Errorf("log does not report the failed check:\n%s", logs.String())
	}
}

// cherryFails is a daemonGit whose Cherry always errors.
type cherryFails struct{ daemonGit }

func (cherryFails) Cherry(upstream, head string) (string, error) {
	return "", errors.New("cherry: boom")
}

// The force-delete is not polecat/*'s alone, so the unpushed-work check is not
// either: origin drops fix/ and dog/ branches too, and one of them can hold the
// only copy of its commits (gt-era1g).
func TestRunGitHygiene_KeepsOrphansOfOtherPrefixesWithUnpushedCommits(t *testing.T) {
	t.Parallel()
	d, _, logs := newGitHygieneDaemon(t, "r1")
	f := useGitfake(t, d)

	origin := filepath.Join(t.TempDir(), "origin.git")
	f.InitBare(t, origin)
	base := f.Commit(t, origin, "main", "seed", map[string]string{"a.txt": "a\n"})
	// fix/landed's work landed on origin's main as a different commit.
	f.Commit(t, origin, "main", "landed", map[string]string{"x.txt": "x\n"})

	bare := filepath.Join(d.config.TownRoot, "r1", ".repo.git")
	f.InitBare(t, bare)
	f.AddRemote(t, bare, "origin", origin)
	f.SetRef(t, bare, "refs/heads/main", base)
	f.SetRef(t, bare, "refs/heads/fix/landed", base)
	f.Commit(t, bare, "fix/landed", "landed elsewhere", map[string]string{"x.txt": "x\n"})
	f.SetRef(t, bare, "refs/heads/fix/only-copy", base)
	f.Commit(t, bare, "fix/only-copy", "never pushed", map[string]string{"p.txt": "p\n"})
	f.SetRef(t, bare, "refs/heads/dog/only-copy", base)
	f.Commit(t, bare, "dog/only-copy", "never pushed", map[string]string{"q.txt": "q\n"})

	d.runGitHygiene()

	local, err := d.gitAt(bare).ListBranches("")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"dog/only-copy", "fix/only-copy", "main"}; !slices.Equal(local, want) {
		t.Errorf("local branches = %v, want %v (fix/landed is on origin, the other two hold the only copy)", local, want)
	}
	for _, branch := range []string{"dog/only-copy", "fix/only-copy"} {
		if !strings.Contains(logs.String(), "keep orphan "+branch) {
			t.Errorf("log does not say why %s was kept:\n%s", branch, logs.String())
		}
	}
	if !strings.Contains(logs.String(), "0 merged, 1 orphan, 0 remote") {
		t.Errorf("log lacks the summary:\n%s", logs.String())
	}
}

// A polecat branch whose work reached origin's integration branch carries
// nothing unpushed, though origin dropped the branch itself: cherry reads the
// patch on that base, the branch an epic's polecats merge through (gt-era1g).
func TestRunGitHygiene_DeletesAnOrphanWhoseWorkIsOnAnIntegrationBranch(t *testing.T) {
	t.Parallel()
	d, _, logs := newGitHygieneDaemon(t, "r1")
	f := useGitfake(t, d)

	origin := filepath.Join(t.TempDir(), "origin.git")
	f.InitBare(t, origin)
	base := f.Commit(t, origin, "main", "seed", map[string]string{"a.txt": "a\n"})
	f.SetRef(t, origin, "refs/heads/integration/epic", base)
	f.Commit(t, origin, "integration/epic", "epic work", map[string]string{"e.txt": "e\n"})

	bare := filepath.Join(d.config.TownRoot, "r1", ".repo.git")
	f.InitBare(t, bare)
	f.AddRemote(t, bare, "origin", origin)
	f.SetRef(t, bare, "refs/heads/main", base)
	f.SetRef(t, bare, "refs/heads/polecat/epic-work", base)
	f.Commit(t, bare, "polecat/epic-work", "epic work", map[string]string{"e.txt": "e\n"})

	d.runGitHygiene()

	local, err := d.gitAt(bare).ListBranches("")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"main"}; !slices.Equal(local, want) {
		t.Errorf("local branches = %v, want %v: the work is on origin's integration base", local, want)
	}
	if !strings.Contains(logs.String(), "0 merged, 1 orphan, 0 remote") {
		t.Errorf("log lacks the summary:\n%s", logs.String())
	}
}

// An integration branch created before its first child lands sits at main's
// tip, so the merged test calls it merged on both sides. Reaping it leaves the
// next child with no branch to target, and it lands on main instead (gt-t9vl4).
func TestCleanRigRepoKeepsIntegrationBranchAtMainTip(t *testing.T) {
	t.Parallel()
	d, _, _ := newGitHygieneDaemon(t, "r1")
	f := useGitfake(t, d)

	origin := filepath.Join(t.TempDir(), "origin.git")
	f.InitBare(t, origin)
	base := f.Commit(t, origin, "main", "seed", map[string]string{"a.txt": "a\n"})
	f.SetRef(t, origin, "refs/heads/integration/epic", base)
	f.SetRef(t, origin, "refs/heads/polecat/merged", base)

	bare := filepath.Join(d.config.TownRoot, "r1", ".repo.git")
	f.InitBare(t, bare)
	f.AddRemote(t, bare, "origin", origin)
	f.SetRef(t, bare, "refs/heads/main", base)
	f.SetRef(t, bare, "refs/heads/integration/epic", base)
	f.SetRef(t, bare, "refs/heads/polecat/merged", base)

	d.cleanRigRepo(bare)

	// The sweep ran: a merged polecat branch is still deleted on both sides.
	if got := f.Ref(bare, "refs/heads/polecat/merged"); got != "" {
		t.Errorf("local polecat/merged = %q, want the merged branch deleted", got)
	}
	if got := f.Ref(origin, "refs/heads/polecat/merged"); got != "" {
		t.Errorf("origin polecat/merged = %q, want the merged branch deleted", got)
	}
	for _, dir := range []string{bare, origin} {
		if got := f.Ref(dir, "refs/heads/integration/epic"); got != base {
			t.Errorf("integration/epic in %s = %q, want the branch kept at main's tip %s", dir, got, base)
		}
	}
}

// The check reads the bases through this repository's remote-tracking refs, so
// it cannot depend on the cycle's fetch having written them: a failed fetch is
// logged and the cleanup goes on, and cherry then fails on a missing base, which
// keeps every orphan forever (gt-era1g).
func TestRunGitHygiene_ResolvesABaseTheFailedFetchNeverWrote(t *testing.T) {
	t.Parallel()
	d, _, logs := newGitHygieneDaemon(t, "r1")
	f := useGitfake(t, d)

	origin := filepath.Join(t.TempDir(), "origin.git")
	f.InitBare(t, origin)
	base := f.Commit(t, origin, "main", "seed", map[string]string{"a.txt": "a\n"})
	f.Commit(t, origin, "main", "landed", map[string]string{"x.txt": "x\n"})

	bare := filepath.Join(d.config.TownRoot, "r1", ".repo.git")
	f.InitBare(t, bare)
	f.AddRemote(t, bare, "origin", origin)
	f.SetRef(t, bare, "refs/heads/main", base)
	f.SetRef(t, bare, "refs/heads/polecat/old", base)
	f.Commit(t, bare, "polecat/old", "landed elsewhere", map[string]string{"x.txt": "x\n"})

	openGit := d.openGitFn
	d.openGitFn = func(dir string) daemonGit { return fetchPruneFails{openGit(dir)} }

	d.runGitHygiene()

	if !strings.Contains(logs.String(), "fetch --prune") {
		t.Errorf("log does not report the failed fetch:\n%s", logs.String())
	}
	local, err := d.gitAt(bare).ListBranches("")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"main"}; !slices.Equal(local, want) {
		t.Errorf("local branches = %v, want %v: polecat/old's work is on origin/main", local, want)
	}
	if strings.Contains(logs.String(), "cannot tell whether it holds unpushed commits") {
		t.Errorf("the check could not resolve its base:\n%s", logs.String())
	}
}

// fetchPruneFails is a daemonGit whose cycle fetch always errors, so the
// repository keeps whatever remote-tracking refs it already had.
type fetchPruneFails struct{ daemonGit }

func (fetchPruneFails) FetchPrune(remote string) error { return errors.New("fetch: boom") }

// A base ref the failed fetch left stale judges a branch by an old tip and
// reads landed work as unpushed, keeping the branch; the check refreshes it to
// the tip ls-remote reported (gt-era1g).
func TestRunGitHygiene_RefreshesABaseTheFailedFetchLeftStale(t *testing.T) {
	t.Parallel()
	d, _, _ := newGitHygieneDaemon(t, "r1")
	f := useGitfake(t, d)

	origin := filepath.Join(t.TempDir(), "origin.git")
	f.InitBare(t, origin)
	base := f.Commit(t, origin, "main", "seed", map[string]string{"a.txt": "a\n"})
	// polecat/old's work landed on origin's main as a different commit.
	f.Commit(t, origin, "main", "landed", map[string]string{"x.txt": "x\n"})

	bare := filepath.Join(d.config.TownRoot, "r1", ".repo.git")
	f.InitBare(t, bare)
	f.AddRemote(t, bare, "origin", origin)
	f.SetRef(t, bare, "refs/heads/main", base)
	// The tracking ref the cycle should have refreshed, left at the old tip.
	f.SetRef(t, bare, "refs/remotes/origin/main", base)
	f.SetRef(t, bare, "refs/heads/polecat/old", base)
	f.Commit(t, bare, "polecat/old", "landed elsewhere", map[string]string{"x.txt": "x\n"})

	openGit := d.openGitFn
	d.openGitFn = func(dir string) daemonGit { return fetchPruneFails{openGit(dir)} }

	d.runGitHygiene()

	local, err := d.gitAt(bare).ListBranches("")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"main"}; !slices.Equal(local, want) {
		t.Errorf("local branches = %v, want %v: polecat/old's work is on origin/main's tip", local, want)
	}
}

// The working clone install-gt.sh merges into is left alone while the shared
// bare repo beside it is cleaned (gt-ckoau).
func TestRunGitHygiene_CleansTheBareRepoWhenTheRigAlsoHasAClone(t *testing.T) {
	t.Parallel()
	d, _, logs := newGitHygieneDaemon(t, "r1")
	f := useGitfake(t, d)

	origin := filepath.Join(t.TempDir(), "origin.git")
	f.InitBare(t, origin)
	base := f.Commit(t, origin, "main", "seed", map[string]string{"a.txt": "a\n"})

	rigRoot := filepath.Join(d.config.TownRoot, "r1")
	bare := filepath.Join(rigRoot, ".repo.git")
	f.InitBare(t, bare)
	f.AddRemote(t, bare, "origin", origin)
	f.SetRef(t, bare, "refs/heads/main", base)
	f.SetRef(t, bare, "refs/heads/done", base)

	clone := filepath.Join(rigRoot, "mayor", "rig")
	f.Clone(t, origin, clone)
	f.SetRef(t, clone, "refs/heads/done", base)

	d.runGitHygiene()

	local, err := d.gitAt(bare).ListBranches("")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"main"}; !slices.Equal(local, want) {
		t.Errorf("bare repo branches = %v, want %v", local, want)
	}
	cloneBranches, err := d.gitAt(clone).ListBranches("")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"done", "main"}; !slices.Equal(cloneBranches, want) {
		t.Errorf("mayor/rig branches = %v, want %v: the patrol must not clean the working clone", cloneBranches, want)
	}
	if !strings.Contains(logs.String(), bare) {
		t.Errorf("log does not name the bare repo %s:\n%s", bare, logs.String())
	}
}

func TestRunGitHygiene_NoRigRepositoryIsAFailedCycle(t *testing.T) {
	t.Parallel()
	d, feed, _ := newGitHygieneDaemon(t, "norepo")
	useGitfake(t, d)

	d.runGitHygiene()

	if len(feed.payloads) != 1 {
		t.Fatalf("feed = %v, want one failed git_hygiene cycle", feed.payloads)
	}
	reason, _ := feed.payloads[0]["reason"].(string)
	if !strings.Contains(reason, "no rig has a repository (mayor/rigs.json lists none, or none has a .repo.git or a working clone)") {
		t.Errorf("reason = %q, want the no-repository failure naming both layouts", reason)
	}
}

// A rig whose .repo.git carries the name and no repository behind it has no
// repository to clean, so its cycle fails like a rig with no layout at all
// (gt-nm4b9).
func TestRunGitHygiene_EmptyBareRepoDirectoryIsAFailedCycle(t *testing.T) {
	t.Parallel()
	d, feed, _ := newGitHygieneDaemon(t, "r1")
	useGitfake(t, d)
	mustMkdirAll(t, filepath.Join(d.config.TownRoot, "r1", ".repo.git"))

	d.runGitHygiene()

	if len(feed.payloads) != 1 {
		t.Fatalf("feed = %v, want one failed git_hygiene cycle", feed.payloads)
	}
	reason, _ := feed.payloads[0]["reason"].(string)
	if !strings.Contains(reason, "no rig has a repository") {
		t.Errorf("reason = %q, want the no-repository failure", reason)
	}
}

func TestGitHygieneConfig(t *testing.T) {
	t.Parallel()
	if got := gitHygieneInterval(nil); got != defaultGitHygieneInterval {
		t.Errorf("default interval = %v", got)
	}
	on := &DaemonPatrolConfig{Patrols: &PatrolsConfig{GitHygiene: &PatrolConfig{Enabled: true, Interval: "2h"}}}
	if got := gitHygieneInterval(on); got != 2*time.Hour {
		t.Errorf("configured interval = %v", got)
	}
	if !IsPatrolEnabled(&DaemonPatrolConfig{Patrols: &PatrolsConfig{}}, "git_hygiene") {
		t.Error("git_hygiene is off with no config entry; it defaults on")
	}
	if IsPatrolEnabled(&DaemonPatrolConfig{Patrols: &PatrolsConfig{GitHygiene: &PatrolConfig{}}}, "git_hygiene") {
		t.Error("an explicit enabled=false entry did not turn git_hygiene off")
	}
}
