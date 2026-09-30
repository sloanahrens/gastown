package gitfake

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Env builds the contract's fixtures and opens the implementation under
// test. *Fake is one; the integration tier builds the same fixtures with git
// commands and opens *git.Git.
type Env interface {
	// InitBare makes an empty bare repository at dir with HEAD on main.
	InitBare(t testing.TB, dir string)
	// Commit makes a commit in the repository at dir on branch whose parent
	// is the branch's tip (none when the branch is new) and whose tree is
	// the parent's with files written over it, and returns its id.
	Commit(t testing.TB, dir, branch, message string, files map[string]string) string
	// SetRef points ref (a full ref name) at id in the repository at dir.
	SetRef(t testing.TB, dir, ref, id string)
	// Clone clones the repository at src to a non-bare repository at dest,
	// with src as origin.
	Clone(t testing.TB, src, dest string)
	// Open returns the implementation under test for the repository or
	// worktree at dir.
	Open(dir string) Repo
}

var _ Env = (*Fake)(nil)

// fixture is an origin with main (a.txt) and a branch that adds b.txt,
// cloned to a working clone whose origin is it.
type fixture struct {
	env    Env
	origin string
	clone  string
	root   string
	base   string // main
	head   string // the branch
}

const fixtureBranch = "feature"

func newFixture(t *testing.T, env Env) *fixture {
	t.Helper()
	root := t.TempDir()
	fx := &fixture{env: env, origin: filepath.Join(root, "origin.git"), clone: filepath.Join(root, "clone"), root: root}
	env.InitBare(t, fx.origin)
	fx.base = env.Commit(t, fx.origin, "main", "main: seed", map[string]string{"a.txt": "one\ntwo\nthree\n"})
	env.SetRef(t, fx.origin, "refs/heads/"+fixtureBranch, fx.base)
	fx.head = env.Commit(t, fx.origin, fixtureBranch, "feat: add b\n\nThe body says why.", map[string]string{"b.txt": "work\n"})
	env.Clone(t, fx.origin, fx.clone)
	return fx
}

// fetch fetches branch from origin into refs/remotes/origin/<branch>.
func (fx *fixture) fetch(t *testing.T, g Repo, branch string) {
	t.Helper()
	if err := g.FetchRefspecWithTimeout("origin", "+refs/heads/"+branch+":refs/remotes/origin/"+branch, time.Minute); err != nil {
		t.Fatalf("fetch %s: %v", branch, err)
	}
}

// worktree adds a detached worktree of the clone at rev and opens it.
func (fx *fixture) worktree(t *testing.T, rev string) (string, Repo) {
	t.Helper()
	dir := filepath.Join(fx.root, "wt-"+strings.ReplaceAll(t.Name(), "/", "-"))
	if err := fx.env.Open(fx.clone).WorktreeAddDetached(dir, rev); err != nil {
		t.Fatalf("WorktreeAddDetached: %v", err)
	}
	return dir, fx.env.Open(dir)
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// RunRepoContract checks the behavior every Repo implementation must share.
// A case that fails against *git.Git means the case is wrong; correct it to
// what git does, then make the fake copy it.
func RunRepoContract(t *testing.T, newEnv func(t *testing.T) Env) {
	t.Run("Rev resolves branches, remote-tracking refs, ids and HEAD", func(t *testing.T) {
		fx := newFixture(t, newEnv(t))
		g := fx.env.Open(fx.clone)
		if id, err := g.Rev("HEAD"); err != nil || id != fx.base {
			t.Errorf("Rev(HEAD) = %q, %v; want main %s", id, err, fx.base)
		}
		if id, err := g.Rev("origin/main"); err != nil || id != fx.base {
			t.Errorf("Rev(origin/main) = %q, %v; want %s", id, err, fx.base)
		}
		if id, err := g.Rev("main"); err != nil || id != fx.base {
			t.Errorf("Rev(main) = %q, %v; want %s", id, err, fx.base)
		}
		if id, err := g.Rev(fx.head + "^{commit}"); err != nil || id != fx.head {
			t.Errorf("Rev(head^{commit}) = %q, %v; the clone fetched every branch", id, err)
		}
		// rev-parse echoes a full id without looking it up; ^{commit} checks it.
		unknown := strings.Repeat("1", 40)
		if id, err := g.Rev(unknown); err != nil || id != unknown {
			t.Errorf("Rev(unknown full id) = %q, %v; want it echoed", id, err)
		}
		for _, bad := range []string{"no-such-branch", strings.Repeat("1", 40) + "^{commit}"} {
			if id, err := g.Rev(bad); err == nil || id != "" {
				t.Errorf("Rev(%q) = %q, %v; want an error", bad, id, err)
			}
		}
		if _, err := fx.env.Open(t.TempDir()).Rev("HEAD"); err == nil {
			t.Error("Rev outside a repository succeeded")
		}
	})

	t.Run("fetch creates and force-updates remote-tracking refs, and fails on a missing ref", func(t *testing.T) {
		fx := newFixture(t, newEnv(t))
		g := fx.env.Open(fx.clone)
		later := fx.env.Commit(t, fx.origin, "later", "later", map[string]string{"c.txt": "c\n"})
		if _, err := g.Rev(later + "^{commit}"); err == nil {
			t.Fatal("a commit the clone never fetched resolves")
		}
		fx.fetch(t, g, "later")
		if id, err := g.Rev("origin/later"); err != nil || id != later {
			t.Errorf("after fetch, origin/later = %q, %v; want %s", id, err, later)
		}
		// A rewritten branch: + forces the non-fast-forward update.
		fx.env.SetRef(t, fx.origin, "refs/heads/later", fx.base)
		rewritten := fx.env.Commit(t, fx.origin, "later", "rewritten", map[string]string{"d.txt": "d\n"})
		fx.fetch(t, g, "later")
		if id, _ := g.Rev("origin/later"); id != rewritten {
			t.Errorf("forced fetch left origin/later at %s, want %s", id, rewritten)
		}
		if err := g.FetchRefspecWithTimeout("origin", "+refs/heads/gone:refs/remotes/origin/gone", time.Minute); err == nil {
			t.Error("fetching a missing branch succeeded")
		}
		if err := g.FetchRefspecWithTimeout("nowhere", "+refs/heads/main:refs/remotes/nowhere/main", time.Minute); err == nil {
			t.Error("fetching from a missing remote succeeded")
		}
	})

	t.Run("IsAncestor", func(t *testing.T) {
		fx := newFixture(t, newEnv(t))
		g := fx.env.Open(fx.clone)
		for _, c := range []struct {
			a, d string
			want bool
		}{{fx.base, fx.head, true}, {fx.head, fx.base, false}, {fx.head, fx.head, true}} {
			if got, err := g.IsAncestor(c.a, c.d); err != nil || got != c.want {
				t.Errorf("IsAncestor(%s, %s) = %v, %v; want %v", c.a, c.d, got, err, c.want)
			}
		}
		if _, err := g.IsAncestor(strings.Repeat("1", 40), fx.head); err == nil {
			t.Error("IsAncestor of an unknown commit succeeded")
		}
	})

	t.Run("PushRemoteBranchTip reads the remote, empty for a missing branch", func(t *testing.T) {
		fx := newFixture(t, newEnv(t))
		g := fx.env.Open(fx.clone)
		moved := fx.env.Commit(t, fx.origin, fixtureBranch, "more", map[string]string{"b.txt": "more\n"})
		if tip, err := g.PushRemoteBranchTip("origin", fixtureBranch); err != nil || tip != moved {
			t.Errorf("tip = %q, %v; want the remote's %s, not the clone's stale ref", tip, err, moved)
		}
		if tip, err := g.PushRemoteBranchTip("origin", "gone"); err != nil || tip != "" {
			t.Errorf("tip of a missing branch = %q, %v; want empty", tip, err)
		}
	})

	t.Run("TreesIdentical compares trees, not commits", func(t *testing.T) {
		fx := newFixture(t, newEnv(t))
		g := fx.env.Open(fx.clone)
		same := fx.env.Commit(t, fx.origin, "main", "main: b by another route", map[string]string{"b.txt": "work\n"})
		fx.fetch(t, g, "main")
		if ok, err := g.TreesIdentical(same, fx.head); err != nil || !ok {
			t.Errorf("TreesIdentical(same tree) = %v, %v", ok, err)
		}
		if ok, err := g.TreesIdentical(fx.base, fx.head); err != nil || ok {
			t.Errorf("TreesIdentical(different trees) = %v, %v", ok, err)
		}
		if _, err := g.TreesIdentical("no-such-ref", fx.head); err == nil {
			t.Error("TreesIdentical of an unknown ref succeeded")
		}
	})

	t.Run("CommitMessages lists base..head newest first with bodies", func(t *testing.T) {
		fx := newFixture(t, newEnv(t))
		second := fx.env.Commit(t, fx.origin, fixtureBranch, "fix: second\n\nCo-Authored-By: Someone <s@example.com>", map[string]string{"b.txt": "work\nmore\n"})
		g := fx.env.Open(fx.clone)
		fx.fetch(t, g, fixtureBranch)
		msgs, err := g.CommitMessages(fx.base, second)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, m := range msgs {
			got = append(got, m.SHA+" "+m.Message)
		}
		want := []string{second + " fix: second\n\nCo-Authored-By: Someone <s@example.com>", fx.head + " feat: add b\n\nThe body says why."}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("CommitMessages = %q, want %q", got, want)
		}
		if msgs, err := g.CommitMessages(second, second); err != nil || len(msgs) != 0 {
			t.Errorf("empty range = %v, %v", msgs, err)
		}
	})

	t.Run("CommitLineStatsInRange counts lines per commit, newest first, merges skipped", func(t *testing.T) {
		fx := newFixture(t, newEnv(t))
		edit := fx.env.Commit(t, fx.origin, fixtureBranch, "edit a", map[string]string{"a.txt": "one\nTWO\nthree\nfour\n"})
		cut := fx.env.Commit(t, fx.origin, fixtureBranch, "cut a", map[string]string{"a.txt": "one\n"})
		g := fx.env.Open(fx.clone)
		fx.fetch(t, g, fixtureBranch)
		stats, err := g.CommitLineStatsInRange(fx.base+".."+cut, 20)
		if err != nil {
			t.Fatal(err)
		}
		type row struct {
			commit, subject string
			added, removed  int
		}
		var got []row
		for _, s := range stats {
			got = append(got, row{s.Commit, s.Subject, s.Added, s.Removed})
		}
		want := []row{{cut, "cut a", 0, 3}, {edit, "edit a", 2, 1}, {fx.head, "feat: add b", 1, 0}}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("stats = %+v, want %+v", got, want)
		}
		if stats, _ := g.CommitLineStatsInRange(fx.base+".."+cut, 1); len(stats) != 1 || stats[0].Commit != cut {
			t.Errorf("limit 1 = %+v, want only the newest", stats)
		}

		// A merge commit in the range contributes no row of its own.
		_, wt := fx.worktree(t, fx.base)
		if err := wt.MergeNoFF(cut, "merge"); err != nil {
			t.Fatal(err)
		}
		merged, _ := wt.Rev("HEAD")
		stats, err = wt.CommitLineStatsInRange(fx.base+".."+merged, 20)
		if err != nil || len(stats) != 3 {
			t.Errorf("stats through a merge = %+v, %v; want the three branch commits", stats, err)
		}
	})

	t.Run("PatchID is the change, not the commit", func(t *testing.T) {
		fx := newFixture(t, newEnv(t))
		// The same change (add b.txt) on top of a moved main.
		moved := fx.env.Commit(t, fx.origin, "main", "main: c", map[string]string{"c.txt": "c\n"})
		replay := fx.env.Commit(t, fx.origin, "main", "replay b", map[string]string{"b.txt": "work\n"})
		other := fx.env.Commit(t, fx.origin, "main", "other", map[string]string{"b.txt": "different\n"})
		g := fx.env.Open(fx.clone)
		fx.fetch(t, g, "main")
		want, err := g.PatchID(fx.base, fx.head)
		if err != nil || want == "" {
			t.Fatalf("PatchID = %q, %v", want, err)
		}
		if got, err := g.PatchID(moved, replay); err != nil || got != want {
			t.Errorf("the same change on another base: %q, %v; want %q", got, err, want)
		}
		if got, err := g.PatchID(replay, other); err != nil || got == want {
			t.Errorf("a different change: %q, %v; want something other than %q", got, err, want)
		}
		if _, err := g.PatchID(fx.base, fx.base); err == nil {
			t.Error("PatchID of an empty range succeeded")
		}
	})

	t.Run("a detached worktree checks out the ref and is removed", func(t *testing.T) {
		fx := newFixture(t, newEnv(t))
		dir, wt := fx.worktree(t, fx.base)
		if id, err := wt.Rev("HEAD"); err != nil || id != fx.base {
			t.Errorf("worktree HEAD = %q, %v; want %s", id, err, fx.base)
		}
		if got := readFile(t, filepath.Join(dir, "a.txt")); got != "one\ntwo\nthree\n" {
			t.Errorf("a.txt = %q", got)
		}
		if _, err := os.Stat(filepath.Join(dir, "b.txt")); !os.IsNotExist(err) {
			t.Errorf("b.txt from the branch is checked out on main: %v", err)
		}
		if err := fx.env.Open(fx.clone).WorktreeAddDetached(dir, fx.base); err == nil {
			t.Error("adding a worktree over an existing one succeeded")
		}
		g := fx.env.Open(fx.clone)
		if err := g.WorktreeRemove(dir, true); err != nil {
			t.Fatalf("WorktreeRemove: %v", err)
		}
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("worktree dir still there: %v", err)
		}
		if err := g.WorktreePrune(); err != nil {
			t.Errorf("WorktreePrune: %v", err)
		}
		if err := g.WorktreeRemove(dir, true); err == nil {
			t.Error("removing a removed worktree succeeded")
		}
	})

	t.Run("MergeNoFF makes a two-parent commit with both sides' files", func(t *testing.T) {
		fx := newFixture(t, newEnv(t))
		moved := fx.env.Commit(t, fx.origin, "main", "main: c", map[string]string{"c.txt": "c\n"})
		g := fx.env.Open(fx.clone)
		fx.fetch(t, g, "main")
		dir, wt := fx.worktree(t, moved)
		if err := wt.MergeNoFF(fx.head, "land: feature"); err != nil {
			t.Fatalf("MergeNoFF: %v", err)
		}
		merged, _ := wt.Rev("HEAD")
		msgs, _ := wt.CommitMessages(moved, merged)
		if len(msgs) != 2 || msgs[0].SHA != merged || msgs[0].Message != "land: feature" {
			t.Errorf("messages after the merge = %+v", msgs)
		}
		for _, p := range []string{moved, fx.head} {
			if ok, err := wt.IsAncestor(p, merged); err != nil || !ok {
				t.Errorf("%s is not a parent side of the merge: %v", p, err)
			}
		}
		if got := readFile(t, filepath.Join(dir, "b.txt")) + readFile(t, filepath.Join(dir, "c.txt")); got != "work\nc\n" {
			t.Errorf("merged checkout b.txt+c.txt = %q", got)
		}
		if files, err := wt.GetConflictingFiles(); err != nil || len(files) != 0 {
			t.Errorf("conflicts after a clean merge = %v, %v", files, err)
		}
	})

	t.Run("a conflicting MergeNoFF lists the files and aborts back to HEAD", func(t *testing.T) {
		fx := newFixture(t, newEnv(t))
		clash := fx.env.Commit(t, fx.origin, "main", "main: b", map[string]string{"b.txt": "main's b\n"})
		g := fx.env.Open(fx.clone)
		fx.fetch(t, g, "main")
		dir, wt := fx.worktree(t, clash)
		if err := wt.MergeNoFF(fx.head, "land: feature"); err == nil {
			t.Fatal("a conflicting merge succeeded")
		}
		if files, err := wt.GetConflictingFiles(); err != nil || !reflect.DeepEqual(files, []string{"b.txt"}) {
			t.Errorf("conflicting files = %v, %v; want [b.txt]", files, err)
		}
		if err := wt.AbortMerge(); err != nil {
			t.Fatalf("AbortMerge: %v", err)
		}
		if id, _ := wt.Rev("HEAD"); id != clash {
			t.Errorf("HEAD after abort = %s, want %s", id, clash)
		}
		if got := readFile(t, filepath.Join(dir, "b.txt")); got != "main's b\n" {
			t.Errorf("b.txt after abort = %q", got)
		}
		if err := wt.AbortMerge(); err == nil {
			t.Error("aborting with no merge in progress succeeded")
		}
	})

	t.Run("MergeSquash makes a one-parent commit; a conflict cannot be aborted", func(t *testing.T) {
		fx := newFixture(t, newEnv(t))
		moved := fx.env.Commit(t, fx.origin, "main", "main: c", map[string]string{"c.txt": "c\n"})
		g := fx.env.Open(fx.clone)
		fx.fetch(t, g, "main")
		_, wt := fx.worktree(t, moved)
		if err := wt.MergeSquash(fx.head, "land: squashed"); err != nil {
			t.Fatalf("MergeSquash: %v", err)
		}
		squashed, _ := wt.Rev("HEAD")
		if ok, _ := wt.IsAncestor(fx.head, squashed); ok {
			t.Error("the squashed branch is an ancestor of the squash commit")
		}
		if msgs, _ := wt.CommitMessages(moved, squashed); len(msgs) != 1 || msgs[0].Message != "land: squashed" {
			t.Errorf("squash commit = %+v", msgs)
		}
		if ok, _ := wt.TreesIdentical(squashed, fx.head); ok {
			t.Error("the squash lost main's c.txt")
		}
		if err := wt.MergeSquash(fx.head, "again"); err == nil {
			t.Error("squashing a branch whose change is already in HEAD committed")
		}

		clash := fx.env.Commit(t, fx.origin, "main", "main: b", map[string]string{"b.txt": "main's b\n"})
		fx.fetch(t, g, "main")
		fx2dir := filepath.Join(fx.root, "wt-squash-clash")
		if err := g.WorktreeAddDetached(fx2dir, clash); err != nil {
			t.Fatal(err)
		}
		wt2 := fx.env.Open(fx2dir)
		if err := wt2.MergeSquash(fx.head, "land: squashed"); err == nil {
			t.Fatal("a conflicting squash succeeded")
		}
		if files, err := wt2.GetConflictingFiles(); err != nil || !reflect.DeepEqual(files, []string{"b.txt"}) {
			t.Errorf("conflicting files = %v, %v", files, err)
		}
		if err := wt2.AbortMerge(); err == nil {
			t.Error("git merge --abort after a squash conflict succeeded; a squash writes no MERGE_HEAD")
		}
	})

	t.Run("PushForceWithLease pushes on a matching lease and refuses a stale one", func(t *testing.T) {
		fx := newFixture(t, newEnv(t))
		g := fx.env.Open(fx.clone)
		_, wt := fx.worktree(t, fx.base)
		if err := wt.MergeNoFF(fx.head, "land"); err != nil {
			t.Fatal(err)
		}
		merged, _ := wt.Rev("HEAD")
		stale := strings.Repeat("0", 39) + "1"
		if err := wt.PushForceWithLease("origin", "HEAD:refs/heads/main", "refs/heads/main", stale); err == nil {
			t.Fatal("a stale lease pushed")
		}
		if tip, _ := g.PushRemoteBranchTip("origin", "main"); tip != fx.base {
			t.Fatalf("a refused push moved main to %s", tip)
		}
		if err := wt.VerifyPushedCommit("origin", "main", merged); err == nil {
			t.Error("VerifyPushedCommit passed before the push")
		}
		if err := wt.PushForceWithLease("origin", "HEAD:refs/heads/main", "refs/heads/main", fx.base); err != nil {
			t.Fatalf("PushForceWithLease: %v", err)
		}
		if err := wt.VerifyPushedCommit("origin", "main", merged); err != nil {
			t.Errorf("VerifyPushedCommit after the push: %v", err)
		}
		if err := wt.VerifyPushedCommit("origin", "gone", merged); err == nil {
			t.Error("VerifyPushedCommit on a missing branch passed")
		}
		// The pushed commit's objects reached origin: a fresh clone has it.
		fresh := filepath.Join(fx.root, "fresh")
		fx.env.Clone(t, fx.origin, fresh)
		if id, err := fx.env.Open(fresh).Rev("origin/main"); err != nil || id != merged {
			t.Errorf("fresh clone origin/main = %q, %v; want %s", id, err, merged)
		}
	})
}
