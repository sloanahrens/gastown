package git

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// plainGit is a Git on dir that never runs git: the fast paths under test read
// files, and a unit test must not start git (the scripted runner would also
// make the value non-plain, which is the one condition the fast paths need).
func plainGit(dir string) *Git { return &Git{workDir: dir} }

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// checkout lays out the files of an ordinary clone with HEAD on branch.
func checkout(t *testing.T, dir, branch string) string {
	t.Helper()
	gitDir := filepath.Join(dir, ".git")
	write(t, filepath.Join(gitDir, "HEAD"), "ref: refs/heads/"+branch+"\n")
	write(t, filepath.Join(gitDir, "refs", "heads", branch), "0123456789012345678901234567890123456789\n")
	return gitDir
}

func TestCurrentBranchFastReadsHEAD(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	checkout(t, dir, "polecat/agate/gt-1+abc")
	got, ok := plainGit(dir).currentBranchFast()
	if !ok || got != "polecat/agate/gt-1+abc" {
		t.Fatalf("branch = %q ok=%v", got, ok)
	}
}

func TestCurrentBranchFastDefersToGitWhenGitsAnswerWouldDiffer(t *testing.T) {
	t.Parallel()
	t.Run("unborn branch: git errors, so git must be asked", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		write(t, filepath.Join(dir, ".git", "HEAD"), "ref: refs/heads/main\n")
		if _, ok := plainGit(dir).currentBranchFast(); ok {
			t.Fatal("answered for a branch with no commits")
		}
	})
	t.Run("a tag shadows the branch name: git prints heads/<name>", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		gitDir := checkout(t, dir, "release")
		write(t, filepath.Join(gitDir, "refs", "tags", "release"), "0123456789012345678901234567890123456789\n")
		if _, ok := plainGit(dir).currentBranchFast(); ok {
			t.Fatal("answered for an ambiguous name")
		}
	})
	t.Run("a packed tag shadows the branch name", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		gitDir := checkout(t, dir, "release")
		write(t, filepath.Join(gitDir, "packed-refs"), "# pack-refs\n0123456789012345678901234567890123456789 refs/tags/release\n")
		if _, ok := plainGit(dir).currentBranchFast(); ok {
			t.Fatal("answered for an ambiguous name")
		}
	})
	t.Run("injected git or environment", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		checkout(t, dir, "main")
		g := plainGit(dir)
		g.env = []string{"GIT_DIR=/elsewhere"}
		if _, ok := g.currentBranchFast(); ok {
			t.Fatal("read files that GIT_DIR may redirect away from")
		}
	})
}

func TestCurrentBranchFastPackedBranchAndDetachedHEAD(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	gitDir := filepath.Join(dir, ".git")
	write(t, filepath.Join(gitDir, "HEAD"), "ref: refs/heads/main\n")
	write(t, filepath.Join(gitDir, "packed-refs"), "# pack-refs\n0123456789012345678901234567890123456789 refs/heads/main\n")
	if got, ok := plainGit(dir).currentBranchFast(); !ok || got != "main" {
		t.Fatalf("packed branch = %q ok=%v", got, ok)
	}
	write(t, filepath.Join(gitDir, "HEAD"), "0123456789012345678901234567890123456789\n")
	if got, ok := plainGit(dir).currentBranchFast(); !ok || got != "HEAD" {
		t.Fatalf("detached = %q ok=%v (git prints HEAD)", got, ok)
	}
}

// A linked worktree keeps its HEAD in a per-worktree dir and its refs in the
// repository's common dir.
func TestFastPathsInALinkedWorktree(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	common := filepath.Join(root, "repo", ".git")
	wtGit := filepath.Join(common, "worktrees", "agate")
	write(t, filepath.Join(common, "refs", "heads", "feature"), "0123456789012345678901234567890123456789\n")
	write(t, filepath.Join(wtGit, "HEAD"), "ref: refs/heads/feature\n")
	write(t, filepath.Join(wtGit, "commondir"), "../..\n")
	wt := filepath.Join(root, "wt")
	write(t, filepath.Join(wt, ".git"), "gitdir: "+wtGit+"\n")

	if got, ok := plainGit(wt).currentBranchFast(); !ok || got != "feature" {
		t.Fatalf("linked worktree branch = %q ok=%v", got, ok)
	}
	if none, known := plainGit(wt).noStashFast(); !known || !none {
		t.Fatalf("no stash reflog in the common dir: none=%v known=%v", none, known)
	}
	write(t, filepath.Join(common, "logs", "refs", "stash"), "x\n")
	if _, known := plainGit(wt).noStashFast(); known {
		t.Fatal("a stash reflog exists: git must count it")
	}
	if isRoot, known := IsWorktreeRootFast(wt); !known || !isRoot {
		t.Fatalf("IsWorktreeRootFast(linked) = %v, %v", isRoot, known)
	}
}

func TestIsWorktreeRootFastIsNotSureWithoutADotGit(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if _, known := IsWorktreeRootFast(dir); known {
		t.Fatal("a directory with no .git may still be inside a repository: git decides")
	}
	write(t, filepath.Join(dir, ".git"), "gitdir: /does/not/exist\n")
	if _, known := IsWorktreeRootFast(dir); known {
		t.Fatal("a .git pointing nowhere is git's to judge")
	}
	write(t, filepath.Join(dir, ".git"), "garbage\n")
	if _, known := IsWorktreeRootFast(dir); known {
		t.Fatal("an unparsable .git file is git's to judge")
	}
}

func TestRefSHAFastLooseAndPackedAndMissing(t *testing.T) {
	t.Parallel()
	const a = "0123456789012345678901234567890123456789"
	const b = "abcdefabcdefabcdefabcdefabcdefabcdefabcd"
	common := t.TempDir()
	write(t, filepath.Join(common, "refs", "remotes", "origin", "loose"), a+"\n")
	write(t, filepath.Join(common, "packed-refs"), "# pack-refs with: peeled fully-peeled sorted\n"+b+" refs/remotes/origin/packed\n^"+a+"\n")

	if sha, found, known := refSHAFast(common, "refs/remotes/origin/loose"); !known || !found || sha != a {
		t.Errorf("loose = %q %v %v", sha, found, known)
	}
	if sha, found, known := refSHAFast(common, "refs/remotes/origin/packed"); !known || !found || sha != b {
		t.Errorf("packed = %q %v %v", sha, found, known)
	}
	if _, found, known := refSHAFast(common, "refs/remotes/origin/gone"); !known || found {
		t.Errorf("a deleted branch must be definitely missing: found=%v known=%v", found, known)
	}
	// a ref whose name merely ends the same way is not a match
	if _, found, _ := refSHAFast(common, "refs/remotes/origin/ packed"); found {
		t.Error("matched a different ref")
	}
}

func TestRefSHAFastLeavesTheOddCasesToGit(t *testing.T) {
	t.Parallel()
	common := t.TempDir()
	write(t, filepath.Join(common, "refs", "remotes", "origin", "HEAD"), "ref: refs/remotes/origin/main\n")
	if _, _, known := refSHAFast(common, "refs/remotes/origin/HEAD"); known {
		t.Error("a symbolic ref must go to git")
	}
	if _, _, known := refSHAFast(common, "HEAD"); known {
		t.Error("only fully qualified refs are read")
	}
	reftable := t.TempDir()
	write(t, filepath.Join(reftable, "reftable", "tables.list"), "x\n")
	if _, _, known := refSHAFast(reftable, "refs/heads/main"); known {
		t.Error("a reftable repository must go to git")
	}
}

func TestConfigMayDefineUpstream(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		config string
		want   bool
	}{
		"no branch section":      {"[core]\n\tbare = false\n[remote \"origin\"]\n\turl = x\n", false},
		"other branch":           {"[branch \"main\"]\n\tremote = origin\n", false},
		"this branch":            {"[branch \"feature\"]\n\tremote = origin\n\tmerge = refs/heads/feature\n", true},
		"deprecated syntax":      {"[branch.Feature]\n\tremote = origin\n", true},
		"include":                {"[include]\n\tpath = other\n", true},
		"includeIf":              {"[includeIf \"gitdir:~/\"]\n\tpath = other\n", true},
		"odd branch section":     {"[branch  \"feature\" ]\n\tremote = origin\n", true},
		"similar name is a miss": {"[branch \"feature-2\"]\n\tremote = origin\n", false},
	} {
		if got := configMayDefineUpstream(c.config, "feature"); got != c.want {
			t.Errorf("%s: %v, want %v", name, got, c.want)
		}
	}
}

func TestHeadUpstreamFast(t *testing.T) {
	t.Parallel()
	noEnv := func(string) string { return "" }
	const branch = "polecat/agate/gt-1+abc"
	const sha = "0123456789012345678901234567890123456789"
	remote := "[remote \"origin\"]\n\turl = https://example.test/x.git\n\tfetch = +refs/heads/*:refs/remotes/origin/*\n"
	section := "[branch \"" + branch + "\"]\n\tremote = origin\n\tmerge = refs/heads/main\n"

	// setup builds a checkout with the given repository config and returns it.
	setup := func(t *testing.T, config string, trackingRef bool) (*Git, string, string) {
		t.Helper()
		dir, home := t.TempDir(), t.TempDir()
		gitDir := checkout(t, dir, branch)
		write(t, filepath.Join(gitDir, "config"), config)
		if trackingRef {
			write(t, filepath.Join(gitDir, "refs", "remotes", "origin", "main"), sha+"\n")
		}
		return plainGit(dir), home, gitDir
	}

	t.Run("no section names the branch: no upstream", func(t *testing.T) {
		t.Parallel()
		g, home, _ := setup(t, "[core]\n\tbare = false\n"+remote, true)
		if _, has, known := g.headUpstreamWith(noEnv, home); !known || has {
			t.Fatalf("has=%v known=%v", has, known)
		}
	})
	t.Run("configured upstream is resolved", func(t *testing.T) {
		t.Parallel()
		g, home, _ := setup(t, remote+section, true)
		if up, has, known := g.headUpstreamWith(noEnv, home); !known || !has || up != "origin/main" {
			t.Fatalf("up=%q has=%v known=%v", up, has, known)
		}
	})
	t.Run("a detached HEAD has no upstream", func(t *testing.T) {
		t.Parallel()
		g, home, gitDir := setup(t, remote+section, true)
		write(t, filepath.Join(gitDir, "HEAD"), sha+"\n")
		if _, has, known := g.headUpstreamWith(noEnv, home); !known || has {
			t.Fatalf("has=%v known=%v", has, known)
		}
	})
	for name, c := range map[string]struct {
		config      string
		trackingRef bool
	}{
		"tracking ref missing":        {remote + section, false},
		"non-default fetch refspec":   {"[remote \"origin\"]\n\tfetch = +refs/heads/main:refs/remotes/origin/main\n" + section, true},
		"two fetch refspecs":          {remote + "\tfetch = +refs/pull/*:refs/remotes/origin/pr/*\n" + section, true},
		"no remote section":           {section, true},
		"remote is the local repo":    {remote + strings.Replace(section, "remote = origin", "remote = .", 1), true},
		"two merge lines":             {remote + section + "\tmerge = refs/heads/other\n", true},
		"merge outside refs/heads":    {remote + strings.Replace(section, "refs/heads/main", "refs/tags/v1", 1), true},
		"quoted value":                {remote + strings.Replace(section, "refs/heads/main", "\"refs/heads/main\"", 1), true},
		"value with a comment":        {remote + strings.Replace(section, "refs/heads/main", "refs/heads/main # trunk", 1), true},
		"repository config includes":  {"[include]\n\tpath = ../other\n" + remote + section, true},
		"two sections for the branch": {remote + section + section, true},
	} {
		t.Run("git decides: "+name, func(t *testing.T) {
			t.Parallel()
			g, home, _ := setup(t, c.config, c.trackingRef)
			if _, _, known := g.headUpstreamWith(noEnv, home); known {
				t.Fatal("answered where git must decide")
			}
		})
	}
	t.Run("git decides: a branch named like the short form exists", func(t *testing.T) {
		t.Parallel()
		g, home, gitDir := setup(t, remote+section, true)
		write(t, filepath.Join(gitDir, "refs", "heads", "origin", "main"), sha+"\n")
		if _, _, known := g.headUpstreamWith(noEnv, home); known {
			t.Fatal("an ambiguous short name makes git print a longer one")
		}
	})
	t.Run("git decides: the environment or global config may supply config", func(t *testing.T) {
		t.Parallel()
		g, home, _ := setup(t, "[core]\n"+remote, true)
		if _, _, known := g.headUpstreamWith(func(k string) string {
			if k == "GIT_CONFIG_COUNT" {
				return "1"
			}
			return ""
		}, home); known {
			t.Fatal("injected config must go to git")
		}
		write(t, filepath.Join(home, ".gitconfig"), "[branch \""+branch+"\"]\n\tmerge = refs/heads/main\n")
		if _, _, known := g.headUpstreamWith(noEnv, home); known {
			t.Fatal("a global upstream must go to git")
		}
	})
	t.Run("git decides: per-worktree config", func(t *testing.T) {
		t.Parallel()
		g, home, gitDir := setup(t, "[core]\n"+remote, true)
		write(t, filepath.Join(gitDir, "config.worktree"), "[branch \"x\"]\n")
		if _, _, known := g.headUpstreamWith(noEnv, home); known {
			t.Fatal("a per-worktree config must go to git")
		}
	})
}
