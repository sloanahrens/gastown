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

	"github.com/jonboulle/clockwork"
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
		// it, "clone" for a refinery/rig working tree, "legacy" for a
		// mayor/rig working tree the layout no longer offers as a clone.
		layout []string
		want   string // the repository to clean, relative to the rig root; "" for none
	}{
		{name: "shared bare repo only", layout: []string{"bare"}, want: ".repo.git"},
		{name: "bare repo beside the working clone", layout: []string{"bare", "clone"}, want: ".repo.git"},
		{name: "working clone only", layout: []string{"clone"}, want: filepath.Join("refinery", "rig")},
		{name: "empty .repo.git beside the working clone", layout: []string{"emptybare", "clone"}, want: filepath.Join("refinery", "rig")},
		{name: "empty .repo.git directory only", layout: []string{"emptybare"}, want: ""},
		{name: "legacy mayor/rig clone only", layout: []string{"legacy"}, want: ""},
		{name: "legacy mayor/rig clone beside the bare repo", layout: []string{"bare", "legacy"}, want: ".repo.git"},
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
				case "legacy":
					mustMkdirAll(t, filepath.Join(rigRoot, "mayor", "rig", ".git"))
				default:
					mustMkdirAll(t, filepath.Join(rigRoot, "refinery", "rig", ".git"))
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
	clone := filepath.Join(rigRoot, "refinery", "rig")
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

	repo := filepath.Join(d.config.TownRoot, "r1", "refinery", "rig")
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

	repo := filepath.Join(d.config.TownRoot, "r1", "refinery", "rig")
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
	repo := filepath.Join(d.config.TownRoot, "r1", "refinery", "rig")
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

// The working clone the installer merges into is left alone while the shared
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

	clone := filepath.Join(rigRoot, "refinery", "rig")
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
		t.Errorf("refinery/rig branches = %v, want %v: the patrol must not clean the working clone", cloneBranches, want)
	}
	if !strings.Contains(logs.String(), bare) {
		t.Errorf("log does not name the bare repo %s:\n%s", bare, logs.String())
	}
}

// A rig that has only the legacy mayor/rig clone has no repository the patrol
// cleans. The pass logs it, counts it in the summary and leaves its clone alone,
// while the rig with a shared bare repo is still cleaned (gt-3lvcjz.30).
func TestRunGitHygiene_LogsAndCountsALegacyMayorOnlyRig(t *testing.T) {
	t.Parallel()
	d, feed, logs := newGitHygieneDaemon(t, "r1", "legacy")
	f := useGitfake(t, d)

	origin := filepath.Join(t.TempDir(), "origin.git")
	f.InitBare(t, origin)
	base := f.Commit(t, origin, "main", "seed", map[string]string{"a.txt": "a\n"})
	f.SetRef(t, origin, "refs/heads/polecat/merged", base)

	bare := filepath.Join(d.config.TownRoot, "r1", ".repo.git")
	f.InitBare(t, bare)
	f.AddRemote(t, bare, "origin", origin)
	f.SetRef(t, bare, "refs/heads/main", base)
	f.SetRef(t, bare, "refs/heads/done", base)

	legacy := filepath.Join(d.config.TownRoot, "legacy", "mayor", "rig")
	f.Clone(t, origin, legacy)
	f.SetRef(t, legacy, "refs/heads/done", base)

	d.runGitHygiene()

	if got := f.Ref(bare, "refs/heads/done"); got != "" {
		t.Errorf("r1 local done = %q, want the merged branch deleted", got)
	}
	legacyBranches, err := d.gitAt(legacy).ListBranches("")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"done", "main"}; !slices.Equal(legacyBranches, want) {
		t.Errorf("legacy mayor/rig branches = %v, want %v: the patrol must not clean a legacy clone", legacyBranches, want)
	}
	if !strings.Contains(logs.String(), "git_hygiene: rig legacy: skipped, no legacy/.repo.git and no working clone") {
		t.Errorf("log does not name the skipped legacy rig:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), "1 repo(s):") || !strings.Contains(logs.String(), "1 rig(s) skipped") {
		t.Errorf("log lacks the summary counting the skipped rig:\n%s", logs.String())
	}
	if len(feed.payloads) != 0 {
		t.Errorf("feed = %v, want no failed cycle: r1 has a repository", feed.payloads)
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

// withGit makes every repository the daemon opens go through wrap.
func withGit(d *Daemon, wrap func(daemonGit) daemonGit) {
	open := d.openGitFn
	d.openGitFn = func(dir string) daemonGit { return wrap(open(dir)) }
}

// A crew/ branch lands by patch: crew/landed carries main's "landed" change
// under its own SHA, so its one commit reads "-" in cherry and the branch is
// deleted; crew/open's commit is on no base, so it reads "+" and is kept.
func TestRunGitHygiene_DeletesLandedCrewBranchAndKeepsUnlandedOne(t *testing.T) {
	t.Parallel()
	d, feed, logs := newGitHygieneDaemon(t, "r1")
	f := useGitfake(t, d)

	origin := filepath.Join(t.TempDir(), "origin.git")
	f.InitBare(t, origin)
	base := f.Commit(t, origin, "main", "seed", map[string]string{"a.txt": "a\n"})
	f.SetRef(t, origin, "refs/heads/crew/landed", base)
	f.Commit(t, origin, "crew/landed", "landed elsewhere", map[string]string{"x.txt": "x\n"})
	f.SetRef(t, origin, "refs/heads/crew/open", base)
	open := f.Commit(t, origin, "crew/open", "open work", map[string]string{"o.txt": "o\n"})
	f.Commit(t, origin, "main", "landed", map[string]string{"x.txt": "x\n"})

	repo := filepath.Join(d.config.TownRoot, "r1", "refinery", "rig")
	f.Clone(t, origin, repo)

	d.runGitHygiene()

	if got := f.Ref(origin, "refs/heads/crew/landed"); got != "" {
		t.Errorf("origin crew/landed = %q, want the landed branch deleted", got)
	}
	if got := f.Ref(origin, "refs/heads/crew/open"); got != open {
		t.Errorf("origin crew/open = %q, want the unlanded branch kept at %s", got, open)
	}
	if !strings.Contains(logs.String(), "keep origin/crew/open: it holds commits that are not on origin/main") {
		t.Errorf("log does not say why crew/open was kept:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), "1 repo(s): 0 merged, 0 orphan, 0 remote branch(es) deleted, 1 crew branch(es) deleted") {
		t.Errorf("log lacks the crew count in the summary:\n%s", logs.String())
	}
	if len(feed.payloads) != 0 {
		t.Errorf("feed = %v, want no failed cycle", feed.payloads)
	}
}

// An empty crew/ branch has no commit beyond the default branch. It counts as
// landed only once its tip is older than the patrol interval, read through the
// daemon's clock: a branch cut a moment ago is not reaped.
func TestRunGitHygiene_EmptyCrewBranchIsDeletedOnlyOnceOlderThanTheInterval(t *testing.T) {
	t.Parallel()
	d, _, logs := newGitHygieneDaemon(t, "r1")
	f := useGitfake(t, d)

	origin := filepath.Join(t.TempDir(), "origin.git")
	f.InitBare(t, origin)
	old := f.Commit(t, origin, "main", "seed", map[string]string{"a.txt": "a\n"})
	f.SetRef(t, origin, "refs/heads/crew/old", old)
	fresh := f.Commit(t, origin, "main", "later", map[string]string{"b.txt": "b\n"})
	f.SetRef(t, origin, "refs/heads/crew/fresh", fresh)

	repo := filepath.Join(d.config.TownRoot, "r1", "refinery", "rig")
	f.Clone(t, origin, repo)

	oldAt, err := d.gitAt(origin).CommitTime(old)
	if err != nil {
		t.Fatal(err)
	}
	freshAt, err := d.gitAt(origin).CommitTime(fresh)
	if err != nil {
		t.Fatal(err)
	}
	if !freshAt.After(oldAt) {
		t.Fatalf("fixture: fresh tip %v is not after old tip %v", freshAt, oldAt)
	}
	d.patrolConfig = &DaemonPatrolConfig{Patrols: &PatrolsConfig{GitHygiene: &PatrolConfig{Interval: "1h"}}}
	// The clock sits one second past the old tip's hour, so the old tip is
	// older than the interval and the fresh tip is inside it.
	d.clock = clockwork.NewFakeClockAt(oldAt.Add(time.Hour + time.Second))

	d.runGitHygiene()

	if got := f.Ref(origin, "refs/heads/crew/old"); got != "" {
		t.Errorf("origin crew/old = %q, want the empty branch older than the interval deleted", got)
	}
	if got := f.Ref(origin, "refs/heads/crew/fresh"); got != fresh {
		t.Errorf("origin crew/fresh = %q, want the empty branch inside the interval kept at %s", got, fresh)
	}
	if !strings.Contains(logs.String(), "keep origin/crew/fresh: it has no commit beyond origin/main") {
		t.Errorf("log does not say why crew/fresh was kept:\n%s", logs.String())
	}
}

// git cherry lists non-merge commits only: a crew branch whose every commit
// beyond the default is a merge prints nothing, and the interval rule must not
// read that as an empty branch. Its merge can carry content no patch on the
// default branch reproduces, so it is kept and the log says why (gt-eyy06f).
func TestRunGitHygiene_KeepsMergeOnlyCrewBranch(t *testing.T) {
	t.Parallel()
	d, _, logs := newGitHygieneDaemon(t, "r1")
	f := useGitfake(t, d)

	origin := filepath.Join(t.TempDir(), "origin.git")
	f.InitBare(t, origin)
	base := f.Commit(t, origin, "main", "seed", map[string]string{"a.txt": "a\n"})
	landed := f.Commit(t, origin, "main", "landed on main", map[string]string{"l.txt": "l\n"})
	f.SetRef(t, origin, "refs/heads/side", base)
	side := f.Commit(t, origin, "side", "side work", map[string]string{"s.txt": "s\n"})

	// main's history takes both sides, and the crew branch merges the same two
	// commits the way main did: each of its parents is on main, the merge itself
	// is not, and cherry has nothing left to list.
	work := filepath.Join(t.TempDir(), "work")
	f.Clone(t, origin, work)
	f.SetRef(t, work, "refs/heads/side", side)
	f.SetHead(t, work, "main")
	if err := f.Open(work).MergeNoFF("side", "main merge"); err != nil {
		t.Fatal(err)
	}
	f.SetRef(t, origin, "refs/heads/main", f.Ref(work, "refs/heads/main"))
	f.SetRef(t, work, "refs/heads/crew/m", landed)
	f.SetHead(t, work, "crew/m")
	if err := f.Open(work).MergeNoFF("side", "crew merge"); err != nil {
		t.Fatal(err)
	}
	crew := f.Ref(work, "refs/heads/crew/m")
	f.SetRef(t, origin, "refs/heads/crew/m", crew)

	repo := filepath.Join(d.config.TownRoot, "r1", "refinery", "rig")
	f.Clone(t, origin, repo)

	d.patrolConfig = &DaemonPatrolConfig{Patrols: &PatrolsConfig{GitHygiene: &PatrolConfig{Interval: "1h"}}}
	tip, err := d.gitAt(origin).CommitTime(crew)
	if err != nil {
		t.Fatal(err)
	}
	d.clock = clockwork.NewFakeClockAt(tip.Add(2 * time.Hour))

	d.runGitHygiene()

	if got := f.Ref(origin, "refs/heads/crew/m"); got != crew {
		t.Errorf("origin crew/m = %q, want the merge-only branch kept at %s", got, crew)
	}
	if !strings.Contains(logs.String(), "keep origin/crew/m: its only commits beyond origin/main are merges, which cherry cannot judge") {
		t.Errorf("log does not say why crew/m was kept:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), "0 merged, 0 orphan, 0 remote, 0 crew") {
		t.Errorf("log counts a kept merge-only branch as deleted:\n%s", logs.String())
	}
}

// A crew branch whose cherry check errors is kept with the reason in the log,
// and so is the integration branch, which the sweep never deletes (gt-t9vl4).
func TestRunGitHygiene_KeepsCrewBranchWhoseCherryFailsAndLogsTheIntegrationBranch(t *testing.T) {
	t.Parallel()
	d, _, logs := newGitHygieneDaemon(t, "r1")
	f := useGitfake(t, d)

	origin := filepath.Join(t.TempDir(), "origin.git")
	f.InitBare(t, origin)
	base := f.Commit(t, origin, "main", "seed", map[string]string{"a.txt": "a\n"})
	f.SetRef(t, origin, "refs/heads/integration/epic", base)
	epic := f.Commit(t, origin, "integration/epic", "epic work", map[string]string{"e.txt": "e\n"})
	f.SetRef(t, origin, "refs/heads/crew/broken", base)
	broken := f.Commit(t, origin, "crew/broken", "work", map[string]string{"w.txt": "w\n"})

	repo := filepath.Join(d.config.TownRoot, "r1", "refinery", "rig")
	f.Clone(t, origin, repo)
	withGit(d, func(g daemonGit) daemonGit { return cherryFails{g} })

	d.runGitHygiene()

	if got := f.Ref(origin, "refs/heads/crew/broken"); got != broken {
		t.Errorf("origin crew/broken = %q, want the branch kept at %s when cherry fails", got, broken)
	}
	if got := f.Ref(origin, "refs/heads/integration/epic"); got != epic {
		t.Errorf("origin integration/epic = %q, want the integration branch kept at %s", got, epic)
	}
	if !strings.Contains(logs.String(), "keep origin/crew/broken: cannot tell whether it has landed: cherry: boom") {
		t.Errorf("log does not report the failed cherry check:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), "keep origin/integration/epic: integration branches are never deleted") {
		t.Errorf("log does not say why integration/epic was kept:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), "0 merged, 0 orphan, 0 remote, 0 crew") {
		t.Errorf("log lacks the zero crew count:\n%s", logs.String())
	}
}

// An empty crew branch whose tip time cannot be read is kept: the interval
// check needs that time, and without it the branch is not known to be old.
func TestRunGitHygiene_KeepsEmptyCrewBranchWhoseTipTimeFails(t *testing.T) {
	t.Parallel()
	d, _, logs := newGitHygieneDaemon(t, "r1")
	f := useGitfake(t, d)

	origin := filepath.Join(t.TempDir(), "origin.git")
	f.InitBare(t, origin)
	base := f.Commit(t, origin, "main", "seed", map[string]string{"a.txt": "a\n"})
	f.SetRef(t, origin, "refs/heads/crew/empty", base)
	f.Commit(t, origin, "main", "later", map[string]string{"b.txt": "b\n"})

	repo := filepath.Join(d.config.TownRoot, "r1", "refinery", "rig")
	f.Clone(t, origin, repo)
	withGit(d, func(g daemonGit) daemonGit { return commitTimeFails{g} })

	d.runGitHygiene()

	if got := f.Ref(origin, "refs/heads/crew/empty"); got != base {
		t.Errorf("origin crew/empty = %q, want the branch kept at %s", got, base)
	}
	if !strings.Contains(logs.String(), "keep origin/crew/empty: cannot tell whether it has landed: commit time: boom") {
		t.Errorf("log does not report the unreadable tip time:\n%s", logs.String())
	}
}

// An empty cherry output only clears a crew branch of holding merge-only
// content once the branch tip is known to be an ancestor of the base. When that
// ancestry check fails the branch is kept, as every unknown answer keeps it.
func TestRunGitHygiene_KeepsEmptyCrewBranchWhenTheAncestryCheckFails(t *testing.T) {
	t.Parallel()
	d, _, logs := newGitHygieneDaemon(t, "r1")
	f := useGitfake(t, d)

	origin := filepath.Join(t.TempDir(), "origin.git")
	f.InitBare(t, origin)
	base := f.Commit(t, origin, "main", "seed", map[string]string{"a.txt": "a\n"})
	f.SetRef(t, origin, "refs/heads/crew/empty", base)
	f.Commit(t, origin, "main", "later", map[string]string{"b.txt": "b\n"})

	repo := filepath.Join(d.config.TownRoot, "r1", "refinery", "rig")
	f.Clone(t, origin, repo)

	d.patrolConfig = &DaemonPatrolConfig{Patrols: &PatrolsConfig{GitHygiene: &PatrolConfig{Interval: "1h"}}}
	tip, err := d.gitAt(origin).CommitTime(base)
	if err != nil {
		t.Fatal(err)
	}
	d.clock = clockwork.NewFakeClockAt(tip.Add(2 * time.Hour))
	withGit(d, func(g daemonGit) daemonGit { return isAncestorFails{g} })

	d.runGitHygiene()

	if got := f.Ref(origin, "refs/heads/crew/empty"); got != base {
		t.Errorf("origin crew/empty = %q, want the branch kept at %s when the ancestry check fails", got, base)
	}
	if !strings.Contains(logs.String(), "keep origin/crew/empty: cannot tell whether it has landed: ancestry: boom") {
		t.Errorf("log does not report the failed ancestry check:\n%s", logs.String())
	}
}

// A crew branch is kept when the default base cannot be settled: the sweep
// judges it against origin's default branch, and an integration base that
// cannot be fetched leaves that answer unknown.
func TestRunGitHygiene_KeepsCrewBranchWhenTheBaseCannotBeSettled(t *testing.T) {
	t.Parallel()
	d, _, logs := newGitHygieneDaemon(t, "r1")
	f := useGitfake(t, d)

	origin := filepath.Join(t.TempDir(), "origin.git")
	f.InitBare(t, origin)
	base := f.Commit(t, origin, "main", "seed", map[string]string{"a.txt": "a\n"})
	f.SetRef(t, origin, "refs/heads/crew/landed", base)
	landed := f.Commit(t, origin, "crew/landed", "landed elsewhere", map[string]string{"x.txt": "x\n"})
	f.Commit(t, origin, "main", "landed", map[string]string{"x.txt": "x\n"})

	repo := filepath.Join(d.config.TownRoot, "r1", "refinery", "rig")
	f.Clone(t, origin, repo)
	// Made after the clone, so the repository has no remote-tracking ref for it
	// and the settle has to fetch it.
	f.SetRef(t, origin, "refs/heads/integration/epic", base)
	f.Commit(t, origin, "integration/epic", "epic work", map[string]string{"e.txt": "e\n"})
	withGit(d, func(g daemonGit) daemonGit { return baseUnsettled{g} })

	d.runGitHygiene()

	if got := f.Ref(origin, "refs/heads/crew/landed"); got != landed {
		t.Errorf("origin crew/landed = %q, want the branch kept at %s while its base is unsettled", got, landed)
	}
	if !strings.Contains(logs.String(), "keep origin/crew/landed: cannot tell whether it has landed") {
		t.Errorf("log does not report the unsettled base:\n%s", logs.String())
	}
}

// A landed crew branch whose guarded delete is refused stays on origin and is
// logged, and the summary does not count it.
func TestRunGitHygiene_KeepsLandedCrewBranchWhenTheDeleteIsRefused(t *testing.T) {
	t.Parallel()
	d, _, logs := newGitHygieneDaemon(t, "r1")
	f := useGitfake(t, d)

	origin := filepath.Join(t.TempDir(), "origin.git")
	f.InitBare(t, origin)
	base := f.Commit(t, origin, "main", "seed", map[string]string{"a.txt": "a\n"})
	f.SetRef(t, origin, "refs/heads/crew/landed", base)
	landed := f.Commit(t, origin, "crew/landed", "landed elsewhere", map[string]string{"x.txt": "x\n"})
	f.Commit(t, origin, "main", "landed", map[string]string{"x.txt": "x\n"})

	repo := filepath.Join(d.config.TownRoot, "r1", "refinery", "rig")
	f.Clone(t, origin, repo)
	withGit(d, func(g daemonGit) daemonGit { return deleteRemoteRefused{g} })

	d.runGitHygiene()

	if got := f.Ref(origin, "refs/heads/crew/landed"); got != landed {
		t.Errorf("origin crew/landed = %q, want the branch kept at %s when the delete is refused", got, landed)
	}
	if !strings.Contains(logs.String(), "delete origin/crew/landed: push: boom") {
		t.Errorf("log does not report the refused delete:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), "0 merged, 0 orphan, 0 remote, 0 crew") {
		t.Errorf("log counts a refused delete as deleted:\n%s", logs.String())
	}
}

// isAncestorFails is a daemonGit whose ancestry check always errors.
type isAncestorFails struct{ daemonGit }

func (isAncestorFails) IsAncestor(ancestor, descendant string) (bool, error) {
	return false, errors.New("ancestry: boom")
}

// commitTimeFails is a daemonGit whose CommitTime always errors.
type commitTimeFails struct{ daemonGit }

func (commitTimeFails) CommitTime(rev string) (time.Time, error) {
	return time.Time{}, errors.New("commit time: boom")
}

// baseUnsettled is a daemonGit whose fetches always fail, so an integration
// base the repository lacks cannot be settled.
type baseUnsettled struct{ daemonGit }

func (baseUnsettled) FetchPrune(remote string) error { return errors.New("fetch: boom") }

func (baseUnsettled) FetchRefspecWithTimeout(remote, refspec string, timeout time.Duration) error {
	return errors.New("fetch: boom")
}

// deleteRemoteRefused is a daemonGit whose guarded remote delete is refused.
type deleteRemoteRefused struct{ daemonGit }

func (deleteRemoteRefused) DeleteRemoteBranchIfAt(remote, branch, expectedHash string) error {
	return errors.New("push: boom")
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
