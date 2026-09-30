package gitfake

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/git"
)

// writeFiles writes files (slash paths to content) under dir.
func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for p, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// statusLists is a GitStatus's lists, nil and empty alike, for comparison.
func statusLists(st *git.GitStatus) map[string][]string {
	out := map[string][]string{}
	for name, l := range map[string][]string{"modified": st.Modified, "added": st.Added, "deleted": st.Deleted,
		"untracked": st.Untracked, "unmerged": st.Unmerged, "staged-only": st.StagedOnly} {
		if len(l) > 0 {
			out[name] = l
		}
	}
	return out
}

func wantStatus(t *testing.T, g WorkTree, clean bool, want map[string][]string) {
	t.Helper()
	st, err := g.Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if got := statusLists(st); st.Clean != clean || !reflect.DeepEqual(got, want) {
		t.Errorf("Status = clean %v %v; want clean %v %v", st.Clean, got, clean, want)
	}
}

func wantStaged(t *testing.T, g WorkTree, want ...git.StagedChange) {
	t.Helper()
	got, err := g.StagedChanges()
	if err != nil {
		t.Fatalf("StagedChanges: %v", err)
	}
	if len(got) == 0 && len(want) == 0 {
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("StagedChanges = %q; want %q", got, want)
	}
}

// fixtureBlob is git's blob id for the fixture's a.txt on main.
const fixtureBlob = "4cb29ea38f70d7c61b2a3a25b02e3bdf44905402"

// RunWorkTreeContract checks the WorkTree behavior every implementation must
// share, over the same fixtures as RunRepoContract. A case that fails
// against *git.Git means the case is wrong; correct it to what git does,
// then make the fake copy it.
func RunWorkTreeContract(t *testing.T, newEnv func(t *testing.T) Env) {
	t.Run("CommitTime is the committer time of a commit", func(t *testing.T) {
		fx := newFixture(t, newEnv(t))
		g := fx.env.Open(fx.clone).(WorkTree)
		base, err := g.CommitTime(fx.base)
		if err != nil {
			t.Fatalf("CommitTime(base): %v", err)
		}
		head, err := g.CommitTime("origin/" + fixtureBranch)
		if err != nil {
			t.Fatalf("CommitTime(origin/%s): %v", fixtureBranch, err)
		}
		// The fixtures' commits are dated a second apart from a fixed epoch.
		if base.Unix() != commitEpoch+1 || head.Unix() != commitEpoch+2 {
			t.Errorf("CommitTime = %v, %v; want %d and %d", base, head, commitEpoch+1, commitEpoch+2)
		}
		if _, err := g.CommitTime(strings.Repeat("1", 40)); err == nil {
			t.Error("CommitTime of an unknown commit succeeded")
		}
		if _, err := g.CommitTime("no-such-branch"); err == nil {
			t.Error("CommitTime of an unknown ref succeeded")
		}
		if _, err := fx.env.Open(t.TempDir()).(WorkTree).CommitTime("HEAD"); err == nil {
			t.Error("CommitTime outside a repository succeeded")
		}
	})

	t.Run("Status reports unstaged, staged and untracked changes", func(t *testing.T) {
		fx := newFixture(t, newEnv(t))
		g := fx.env.Open(fx.clone).(WorkTree)
		wantStatus(t, g, true, map[string][]string{})
		writeFiles(t, fx.clone, map[string]string{"a.txt": "one\n2\nthree\n", "dir/c.txt": "c\n"})
		wantStatus(t, g, false, map[string][]string{"modified": {"a.txt"}, "untracked": {"dir/c.txt"}})
		if err := g.Add("-A"); err != nil {
			t.Fatalf("Add -A: %v", err)
		}
		wantStatus(t, g, false, map[string][]string{"modified": {"a.txt"}, "added": {"dir/c.txt"},
			"staged-only": {"a.txt", "dir/c.txt"}})
		writeFiles(t, fx.clone, map[string]string{"a.txt": "again\n"})
		wantStatus(t, g, false, map[string][]string{"modified": {"a.txt"}, "added": {"dir/c.txt"},
			"staged-only": {"dir/c.txt"}})
		if err := os.Remove(filepath.Join(fx.clone, "dir", "c.txt")); err != nil {
			t.Fatal(err)
		}
		wantStatus(t, g, false, map[string][]string{"modified": {"a.txt"}, "added": {"dir/c.txt"}})
		if _, err := fx.env.Open(t.TempDir()).(WorkTree).Status(); err == nil {
			t.Error("Status outside a repository succeeded")
		}
	})

	t.Run("Add stages files, deletions and directories; ResetFiles unstages them", func(t *testing.T) {
		fx := newFixture(t, newEnv(t))
		g := fx.env.Open(fx.clone).(WorkTree)
		writeFiles(t, fx.clone, map[string]string{"a.txt": "changed\n", "dir/c.txt": "c\n", "dir/sub/d.txt": "d\n"})
		if err := g.Add("dir"); err != nil {
			t.Fatalf("Add dir: %v", err)
		}
		wantStaged(t, g, git.StagedChange{Status: 'A', Path: "dir/c.txt"}, git.StagedChange{Status: 'A', Path: "dir/sub/d.txt"})
		if err := os.Remove(filepath.Join(fx.clone, "a.txt")); err != nil {
			t.Fatal(err)
		}
		if err := g.Add("-A"); err != nil {
			t.Fatalf("Add -A: %v", err)
		}
		wantStaged(t, g, git.StagedChange{Status: 'D', Path: "a.txt"},
			git.StagedChange{Status: 'A', Path: "dir/c.txt"}, git.StagedChange{Status: 'A', Path: "dir/sub/d.txt"})
		if err := g.ResetFiles("dir/sub/"); err != nil {
			t.Fatalf("ResetFiles dir/sub/: %v", err)
		}
		if err := g.ResetFiles("a.txt", "no-such-path"); err != nil {
			t.Fatalf("ResetFiles a.txt: %v", err)
		}
		wantStaged(t, g, git.StagedChange{Status: 'A', Path: "dir/c.txt"})
		wantStatus(t, g, false, map[string][]string{"added": {"dir/c.txt"}, "deleted": {"a.txt"},
			"untracked": {"dir/sub/d.txt"}, "staged-only": {"dir/c.txt"}})
		if err := g.Add("no-such-path"); err == nil {
			t.Error("Add of a path matching nothing succeeded")
		}
	})

	t.Run("Add leaves ignored untracked files alone and refuses one named exactly", func(t *testing.T) {
		fx := newFixture(t, newEnv(t))
		g := fx.env.Open(fx.clone).(WorkTree)
		writeFiles(t, fx.clone, map[string]string{".gitignore": "*.log\nbuild/\n/top.tmp\n", "x.log": "x\n",
			"sub/y.log": "y\n", "build/out.txt": "o\n", "top.tmp": "t\n", "sub/top.tmp": "kept\n", "keep.txt": "k\n"})
		wantStatus(t, g, false, map[string][]string{"untracked": {".gitignore", "keep.txt", "sub/top.tmp"}})
		if err := g.Add("-A"); err != nil {
			t.Fatalf("Add -A: %v", err)
		}
		wantStaged(t, g, git.StagedChange{Status: 'A', Path: ".gitignore"}, git.StagedChange{Status: 'A', Path: "keep.txt"},
			git.StagedChange{Status: 'A', Path: "sub/top.tmp"})
		if err := g.Add("x.log"); err == nil {
			t.Error("Add of an ignored file succeeded")
		}
	})

	t.Run("Commit commits the index on HEAD, and refuses an empty one", func(t *testing.T) {
		fx := newFixture(t, newEnv(t))
		g := fx.env.Open(fx.clone).(WorkTree)
		if err := g.Commit("nothing"); err == nil {
			t.Error("Commit with nothing staged succeeded")
		}
		writeFiles(t, fx.clone, map[string]string{"a.txt": "wip\n", "new.txt": "n\n"})
		if err := g.Add("a.txt"); err != nil {
			t.Fatal(err)
		}
		if err := g.Commit("WIP: checkpoint (auto)"); err != nil {
			t.Fatalf("Commit: %v", err)
		}
		repo := fx.env.Open(fx.clone)
		head, err := repo.Rev("HEAD")
		if err != nil || head == fx.base {
			t.Fatalf("HEAD = %q, %v; want a new commit", head, err)
		}
		if ok, err := repo.IsAncestor(fx.base, head); err != nil || !ok {
			t.Errorf("the commit's parent is not the old HEAD (%v, %v)", ok, err)
		}
		blobs, err := g.TreeFileBlobs("HEAD")
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := blobs["new.txt"]; ok || len(blobs) != 1 {
			t.Errorf("committed tree = %v; want only a.txt (new.txt was never staged)", blobs)
		}
		if content, err := g.BlobContent(blobs["a.txt"]); err != nil || content != "wip" {
			t.Errorf("committed a.txt = %q, %v; want wip", content, err)
		}
		wantStaged(t, g)
		if branch, err := g.CurrentBranch(); err != nil || branch != "main" {
			t.Errorf("CurrentBranch = %q, %v; want main, the branch the commit moved", branch, err)
		}
	})

	t.Run("WriteTree records the index without committing", func(t *testing.T) {
		fx := newFixture(t, newEnv(t))
		g := fx.env.Open(fx.clone).(WorkTree)
		writeFiles(t, fx.clone, map[string]string{"a.txt": "staged\n", "b.txt": "b\n"})
		if err := g.Add("-A"); err != nil {
			t.Fatal(err)
		}
		tree, err := g.WriteTree()
		if err != nil {
			t.Fatalf("WriteTree: %v", err)
		}
		blobs, err := g.TreeFileBlobs(tree)
		if err != nil {
			t.Fatalf("TreeFileBlobs(tree): %v", err)
		}
		for _, p := range []string{"a.txt", "b.txt"} {
			if content, err := g.BlobContent(blobs[p]); err != nil || content != strings.TrimSpace(map[string]string{"a.txt": "staged", "b.txt": "b"}[p]) {
				t.Errorf("tree's %s = %q, %v", p, content, err)
			}
		}
		if head, _ := fx.env.Open(fx.clone).Rev("HEAD"); head != fx.base {
			t.Errorf("WriteTree moved HEAD to %s", head)
		}
	})

	t.Run("TreeFileBlobs and BlobContent read git's blob ids", func(t *testing.T) {
		fx := newFixture(t, newEnv(t))
		g := fx.env.Open(fx.clone).(WorkTree)
		blobs, err := g.TreeFileBlobs(fx.base)
		if err != nil || !reflect.DeepEqual(blobs, map[string]string{"a.txt": fixtureBlob}) {
			t.Fatalf("TreeFileBlobs(base) = %v, %v; want a.txt at %s", blobs, err, fixtureBlob)
		}
		if content, err := g.BlobContent(fixtureBlob); err != nil || content != "one\ntwo\nthree" {
			t.Errorf("BlobContent = %q, %v; want the trimmed a.txt", content, err)
		}
		if _, err := g.TreeFileBlobs("no-such-rev"); err == nil {
			t.Error("TreeFileBlobs of an unknown rev succeeded")
		}
		if _, err := g.BlobContent(strings.Repeat("1", 40)); err == nil {
			t.Error("BlobContent of an unknown blob succeeded")
		}
	})

	t.Run("BlobDiffLines counts added and removed lines", func(t *testing.T) {
		fx := newFixture(t, newEnv(t))
		g := fx.env.Open(fx.clone).(WorkTree)
		changed := fx.env.Commit(t, fx.origin, "main", "edit a", map[string]string{"a.txt": "one\n2\nthree\nfour\n"})
		fx.fetch(t, fx.env.Open(fx.clone), "main")
		blobs, err := g.TreeFileBlobs(changed)
		if err != nil {
			t.Fatal(err)
		}
		added, removed, err := g.BlobDiffLines(fixtureBlob, blobs["a.txt"])
		if err != nil || !reflect.DeepEqual(added, map[string]int{"2": 1, "four": 1}) || !reflect.DeepEqual(removed, map[string]int{"two": 1}) {
			t.Errorf("BlobDiffLines = +%v -%v, %v; want +2 +four -two", added, removed, err)
		}
		if added, removed, err := g.BlobDiffLines("", fixtureBlob); err != nil || len(added)+len(removed) != 0 {
			t.Errorf("BlobDiffLines against no blob = +%v -%v, %v; want nothing", added, removed, err)
		}
	})

	t.Run("CommitFileChanges and MergeBase read history", func(t *testing.T) {
		fx := newFixture(t, newEnv(t))
		g := fx.env.Open(fx.clone).(WorkTree)
		changes, err := g.CommitFileChanges("origin/"+fixtureBranch, 10)
		want := []git.CommitFileChange{{Commit: fx.head, Path: "b.txt", NewBlob: "b8f99f5be53f536f79ef622abaa77b9942a9e142"}}
		if err != nil || !reflect.DeepEqual(changes, want) {
			t.Errorf("CommitFileChanges = %+v, %v; want only the branch's b.txt (the root reports nothing)", changes, err)
		}
		if base, err := g.MergeBase("origin/main", "origin/"+fixtureBranch); err != nil || base != fx.base {
			t.Errorf("MergeBase = %q, %v; want %s", base, err, fx.base)
		}
		if _, err := g.MergeBase("origin/main", "no-such-rev"); err == nil {
			t.Error("MergeBase with an unknown rev succeeded")
		}
	})

	t.Run("CurrentBranch, upstream and the clean base ref", func(t *testing.T) {
		fx := newFixture(t, newEnv(t))
		g := fx.env.Open(fx.clone).(WorkTree)
		if url, err := g.GetUpstreamURL(); err != nil || url != "" {
			t.Errorf("GetUpstreamURL with no upstream = %q, %v; want empty", url, err)
		}
		if ref := g.CleanDefaultBranchBaseRef("origin", "trunk"); ref != "origin/trunk" {
			t.Errorf("CleanDefaultBranchBaseRef = %q; want origin/trunk", ref)
		}
		fork := filepath.Join(fx.root, "fork.git")
		fx.env.InitBare(t, fork)
		if err := fx.env.Open(fx.clone).AddUpstreamRemote(fork); err != nil {
			t.Fatal(err)
		}
		if ref := g.CleanDefaultBranchBaseRef("origin", ""); ref != "upstream/main" {
			t.Errorf("CleanDefaultBranchBaseRef in a fork-backed clone = %q; want upstream/main", ref)
		}
		dir, _ := fx.worktree(t, fx.base)
		if branch, err := fx.env.Open(dir).(WorkTree).CurrentBranch(); err != nil || branch != "HEAD" {
			t.Errorf("CurrentBranch detached = %q, %v; want HEAD", branch, err)
		}
	})
}
