package git

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// cloneRunner answers a clone by creating the destination it names (the last
// argument), as git would, and scripts every other call through s.
func cloneRunner(s *scripted, setup func(dest string)) runFunc {
	return func(c gitCall) (string, string, error) {
		out, errOut, err := s.run(c)
		if len(c.args) > 0 && (c.args[0] == "clone" || (len(c.args) > 2 && c.args[2] == "clone")) {
			dest := c.args[len(c.args)-1]
			if mkErr := os.MkdirAll(dest, 0o755); mkErr != nil {
				return "", mkErr.Error(), gitExit(128)
			}
			if setup != nil {
				setup(dest)
			}
		}
		return out, errOut, err
	}
}

func TestCloneArgsAndIsolation(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name  string
		clone func(g *Git, url, dest string) error
		flags string
	}{
		{"Clone", func(g *Git, u, d string) error { return g.Clone(u, d) }, "clone --single-branch --depth 1"},
		{"CloneWithReference", func(g *Git, u, d string) error { return g.CloneWithReference(u, d, "/srv/ref") }, "clone --single-branch --depth 1 --reference-if-able /srv/ref"},
		{"CloneBranch", func(g *Git, u, d string) error { return g.CloneBranch(u, d, "dev") }, "clone --single-branch --depth 1 --branch dev"},
		{"CloneBranchPartial", func(g *Git, u, d string) error { return g.CloneBranchPartial(u, d, "dev", "blob:none") }, "clone --single-branch --filter=blob:none --branch dev"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := newScripted(map[string]reply{"clone *": ok("")})
			g := &Git{workDir: t.TempDir(), exec: cloneRunner(s, nil)}
			dest := filepath.Join(t.TempDir(), "rig", "clone")
			if err := tt.clone(g, "/srv/src.git", dest); err != nil {
				t.Fatalf("%s: %v", tt.name, err)
			}
			if _, err := os.Stat(dest); err != nil {
				t.Fatalf("clone not moved to %s: %v", dest, err)
			}
			c := s.calls[0]
			// Cloned in an isolated temp dir, then moved: no repository at the
			// process cwd can leak into the clone.
			tmpDest := c.args[len(c.args)-1]
			if got := strings.Join(c.args[:len(c.args)-2], " "); got != tt.flags || c.args[len(c.args)-2] != "/srv/src.git" {
				t.Fatalf("clone args = %q, want %q /srv/src.git <tmp>", c.args, tt.flags)
			}
			if c.dir != filepath.Dir(tmpDest) || !reflect.DeepEqual(c.env, []string{"GIT_CEILING_DIRECTORIES=" + c.dir}) {
				t.Fatalf("clone ran in %q with env %q; want the temp dir and a ceiling at it", c.dir, c.env)
			}
			// No .githooks and no .gitmodules: nothing else to run.
			if len(s.calls) != 1 {
				t.Fatalf("calls = %q", s.sent())
			}
		})
	}
}

func TestCloneConfiguresHooksPathWhenRepoShipsHooks(t *testing.T) {
	t.Parallel()
	s := newScripted(map[string]reply{"clone *": ok("")})
	g := &Git{workDir: t.TempDir(), exec: cloneRunner(s, func(dest string) {
		_ = os.MkdirAll(filepath.Join(dest, ".githooks"), 0o755)
	})}
	dest := filepath.Join(realTempDir(t), "clone")
	s.on("-C "+dest+" config core.hooksPath .githooks", ok(""))
	if err := g.Clone("/srv/src.git", dest); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	s.noUnscripted(t)
}

func TestCloneFailureIsGitError(t *testing.T) {
	t.Parallel()
	s := newScripted(map[string]reply{"clone *": fail(128, "fatal: repository '/srv/none.git' does not exist\n")})
	err := newTestGit(t, s).Clone("/srv/none.git", filepath.Join(t.TempDir(), "c"))
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("Clone = %v", err)
	}
}

// A bare clone gets origin's fetch refspec and then fetches the default
// branch into refs/remotes/origin, so worktrees can start from origin/main
// (GitHub #286).
func TestCloneBareConfiguresRefspecAndFetchesHeadBranch(t *testing.T) {
	t.Parallel()
	dest := filepath.Join(realTempDir(t), "bare.git")
	gd := "--git-dir " + dest
	s := newScripted(map[string]reply{
		"clone *": ok(""),
		gd + " config remote.origin.fetch +refs/heads/*:refs/remotes/origin/*": ok(""),
		gd + " show-ref --quiet":                                     ok(""),
		gd + " symbolic-ref HEAD":                                    ok("refs/heads/main\n"),
		gd + " fetch --depth 1 origin main:refs/remotes/origin/main": ok(""),
	})
	g := &Git{workDir: t.TempDir(), exec: cloneRunner(s, nil)}
	if err := g.CloneBare("/srv/src.git", dest); err != nil {
		t.Fatalf("CloneBare: %v", err)
	}
	s.noUnscripted(t)
	if !strings.HasPrefix(s.sent()[0], "clone --bare --single-branch --depth 1 ") {
		t.Fatalf("clone args = %q", s.sent()[0])
	}
}

// An empty remote clones fine but has no refs: show-ref exits 1, and the
// clone succeeds without a fetch that would fail on a missing HEAD.
func TestCloneBareEmptyRepoSkipsMissingHeadFetch(t *testing.T) {
	t.Parallel()
	dest := filepath.Join(realTempDir(t), "bare.git")
	gd := "--git-dir " + dest
	s := newScripted(map[string]reply{
		"clone *": ok(""),
		gd + " config remote.origin.fetch +refs/heads/*:refs/remotes/origin/*": ok(""),
		gd + " show-ref --quiet": fail(1, ""),
	})
	g := &Git{workDir: t.TempDir(), exec: cloneRunner(s, nil)}
	if err := g.CloneBare("/srv/empty.git", dest); err != nil {
		t.Fatalf("CloneBare of an empty repo: %v", err)
	}
	s.noUnscripted(t)
	for _, c := range s.sent() {
		if strings.Contains(c, " fetch ") {
			t.Fatalf("fetched from an empty remote: %q", c)
		}
	}
	// Any other show-ref failure is reported.
	if err := configureRefspec(newScripted(map[string]reply{
		gd + " config remote.origin.fetch +refs/heads/*:refs/remotes/origin/*": ok(""),
		gd + " show-ref --quiet": fail(128, "fatal: not a git repository: '"+dest+"'\n"),
	}).run, dest, true); err == nil || !strings.Contains(err.Error(), "checking refs") {
		t.Fatalf("configureRefspec = %v, want a refs check failure", err)
	}
}

func TestConfigureRefspecDetachedHeadFetchesAll(t *testing.T) {
	t.Parallel()
	dest := t.TempDir()
	gd := "--git-dir " + dest
	s := newScripted(map[string]reply{
		gd + " config remote.origin.fetch +refs/heads/*:refs/remotes/origin/*": ok(""),
		gd + " show-ref --quiet":       ok(""),
		gd + " symbolic-ref HEAD":      fail(128, "fatal: ref HEAD is not a symbolic ref\n"),
		gd + " fetch --depth 1 origin": ok(""),
	})
	if err := configureRefspec(s.run, dest, true); err != nil {
		t.Fatal(err)
	}
	s.noUnscripted(t)
	s = newScripted(map[string]reply{
		gd + " config remote.origin.fetch +refs/heads/*:refs/remotes/origin/*": ok(""),
		gd + " show-ref --quiet": ok(""),
		gd + " fetch origin":     ok(""),
	})
	if err := configureRefspec(s.run, dest, false); err != nil {
		t.Fatal(err)
	}
	s.noUnscripted(t)
}

// realTempDir is t.TempDir() with symlinks resolved: clone destinations are
// made absolute and resolved (gitPathAbs) before git sees them, and on macOS
// the temp dir sits behind the /var -> /private/var link.
func realTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func writeGitmodules(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, ".gitmodules"), []byte("[submodule \"libs/sub\"]\n\tpath = libs/sub\n\turl = https://example.com/sub.git\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestInitSubmodules(t *testing.T) {
	t.Parallel()
	t.Run("no .gitmodules asks git nothing", func(t *testing.T) {
		t.Parallel()
		s := newScripted(nil)
		if err := initSubmodules(s.run, t.TempDir(), nil); err != nil || len(s.sent()) != 0 {
			t.Fatalf("initSubmodules = %v, calls %q", err, s.sent())
		}
	})
	t.Run("untracked .gitmodules is skipped", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeGitmodules(t, dir)
		s := newScripted(map[string]reply{
			"-C " + dir + " ls-files --error-unmatch .gitmodules": fail(1, "error: pathspec '.gitmodules' did not match any file(s) known to git\n"),
		})
		if err := initSubmodules(s.run, dir, nil); err != nil {
			t.Fatal(err)
		}
		s.noUnscripted(t)
		if hasTrackedGitmodules(s.run, dir) {
			t.Fatal("hasTrackedGitmodules = true for an untracked file")
		}
	})
	t.Run("tracked .gitmodules updates with the reference and env", func(t *testing.T) {
		t.Parallel()
		dir, ref := t.TempDir(), t.TempDir()
		writeGitmodules(t, dir)
		writeGitmodules(t, ref)
		update := "-C " + dir + " submodule update --init --recursive --reference " + ref
		s := newScripted(map[string]reply{
			"-C " + dir + " ls-files --error-unmatch .gitmodules": ok(".gitmodules\n"),
			"-C " + ref + " ls-files --error-unmatch .gitmodules": ok(".gitmodules\n"),
			"-C " + dir + " rev-parse --show-toplevel":            ok(dir + "\n"),
			update: ok(""),
		})
		env := []string{"GIT_CONFIG_COUNT=1"}
		if err := initSubmodules(s.run, dir, env, ref); err != nil {
			t.Fatal(err)
		}
		s.noUnscripted(t)
		if c, _ := s.sentCall(update); !reflect.DeepEqual(c.env, env) {
			t.Fatalf("update env = %q", c.env)
		}
	})
	t.Run("failure reports git's stderr", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeGitmodules(t, dir)
		s := newScripted(map[string]reply{
			"-C " + dir + " ls-files --error-unmatch .gitmodules": ok(".gitmodules\n"),
			"-C " + dir + " rev-parse --show-toplevel":            ok(dir + "\n"),
			"-C " + dir + " submodule update --init --recursive":  fail(128, "fatal: transport 'file' not allowed\n"),
		})
		err := initSubmodules(s.run, dir, nil)
		if err == nil || err.Error() != "initializing submodules: fatal: transport 'file' not allowed" {
			t.Fatalf("initSubmodules = %v", err)
		}
	})
}

// SubmoduleChanges reads gitlinks (mode 160000) from diff --raw, drops
// .claude/ worktree pointers (gt-dg7), blanks null shas, and takes each URL
// from .gitmodules at head.
func TestSubmoduleChanges(t *testing.T) {
	t.Parallel()
	const z = "0000000000000000000000000000000000000000"
	s := newScripted(map[string]reply{
		"diff --raw main feature": ok(":160000 160000 1111111 2222222 M\tlibs/sub\n" +
			":160000 160000 3333333 4444444 M\t.claude/worktrees/codebase-friction\n" +
			":000000 160000 " + z + " 5555555 A\tlibs/new\n" +
			":100644 100644 6666666 7777777 M\tREADME.md\n"),
		"show feature:.gitmodules": ok("[submodule \"libs/sub\"]\n\tpath = libs/sub\n\turl = /srv/sub.git\n"),
		"config -f *":              ok(""),
	})
	// git config -f reads a temp file whose name varies; answer by key.
	g := &Git{workDir: t.TempDir(), exec: func(c gitCall) (string, string, error) {
		if len(c.args) > 3 && c.args[0] == "config" && c.args[1] == "-f" {
			s.run(c)
			switch c.args[3] {
			case "--get-regexp":
				return "submodule.libs/sub.path libs/sub\n", "", nil
			case "--get":
				if c.args[4] == "submodule.libs/sub.url" {
					return "/srv/sub.git\n", "", nil
				}
			}
			return "", "", gitExit(1)
		}
		return s.run(c)
	}}
	changes, err := g.SubmoduleChanges("main", "feature")
	if err != nil {
		t.Fatal(err)
	}
	want := []SubmoduleChange{
		{Path: "libs/sub", OldSHA: "1111111", NewSHA: "2222222", URL: "/srv/sub.git"},
		{Path: "libs/new", NewSHA: "5555555"},
	}
	if !reflect.DeepEqual(changes, want) {
		t.Fatalf("SubmoduleChanges = %+v, want %+v", changes, want)
	}

	s.on("diff --raw main plain", ok(":100644 100644 6666666 7777777 M\tREADME.md\n"))
	if changes, err := g.SubmoduleChanges("main", "plain"); err != nil || len(changes) != 0 {
		t.Fatalf("SubmoduleChanges without gitlinks = %+v, %v", changes, err)
	}
}

// A short sha on the error path must not panic formatting sha[:8] (gt-dg7).
func TestPushSubmoduleCommitShortSHAError(t *testing.T) {
	t.Parallel()
	s := newScripted(nil)
	g := newTestGit(t, s)
	sub := filepath.Join(g.workDir, "libs/sub")
	s.on("-C "+sub+" symbolic-ref refs/remotes/origin/HEAD", ok("refs/remotes/origin/main\n"))
	s.on("-C "+sub+" push origin 09bcf16:refs/heads/main", fail(1, "error: src refspec 09bcf16 does not match any\n"))
	err := g.PushSubmoduleCommit("libs/sub", "09bcf16", "origin")
	if err == nil || !strings.Contains(err.Error(), "pushing submodule libs/sub commit 09bcf16: error: src refspec") {
		t.Fatalf("PushSubmoduleCommit = %v", err)
	}
	if c, _ := s.sentCall("-C " + sub + " push origin 09bcf16:refs/heads/main"); c.timeout != pushTimeout {
		t.Fatalf("submodule push timeout = %v", c.timeout)
	}
}

// Default-branch detection tries the local symbolic ref, then local tracking
// refs, then asks the remote.
func TestSubmoduleDefaultBranchFallbacks(t *testing.T) {
	t.Parallel()
	const dir = "/srv/sub"
	s := newScripted(map[string]reply{
		"-C /srv/sub symbolic-ref refs/remotes/origin/HEAD":                 fail(128, "fatal: ref refs/remotes/origin/HEAD is not a symbolic ref\n"),
		"-C /srv/sub rev-parse --verify --quiet refs/remotes/origin/main":   fail(1, ""),
		"-C /srv/sub rev-parse --verify --quiet refs/remotes/origin/master": fail(1, ""),
		"-C /srv/sub ls-remote --exit-code origin refs/heads/main":          fail(2, ""),
		"-C /srv/sub ls-remote --exit-code origin refs/heads/master":        ok("abc\trefs/heads/master\n"),
	})
	if b, err := submoduleDefaultBranch(s.run, dir, "origin"); err != nil || b != "master" {
		t.Fatalf("submoduleDefaultBranch = %q, %v", b, err)
	}
	s.on("-C /srv/sub ls-remote --exit-code origin refs/heads/master", fail(2, ""))
	if _, err := submoduleDefaultBranch(s.run, dir, "origin"); err == nil {
		t.Fatal("no default branch anywhere must fail")
	}
}
