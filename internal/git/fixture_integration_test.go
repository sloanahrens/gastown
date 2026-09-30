//go:build integration

package git

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// A git exec costs 20-50 ms on the loaded host the merge gate runs on, and
// macOS scans every one. The repos most tests start from took five to
// thirteen of them each, per test. Each is now built once per test binary
// (gitFixture) and every test gets a file copy of it: the same repos, the
// same commits, with the absolute paths in their git config pointed at the
// copy.

// gitFixture is a directory of git repos built on first use and copied into
// each test that asks for it.
type gitFixture struct {
	name  string
	build func(dir string) error

	once sync.Once
	dir  string
	err  error
}

// copyInto copies the fixture into dst (which must exist) and rewrites the
// absolute fixture paths in every git config file under it to dst.
func (f *gitFixture) copyInto(t *testing.T, dst string) {
	t.Helper()
	f.once.Do(func() {
		// The hermetic harness points HOME at a sandbox it removes when the
		// test binary exits.
		home, err := os.UserHomeDir()
		if err != nil {
			f.err = err
			return
		}
		f.dir, f.err = os.MkdirTemp(home, "git-fixture-"+f.name+"-")
		if f.err == nil {
			f.err = f.build(f.dir)
		}
	})
	if f.err != nil {
		t.Fatalf("building the %s git fixture: %v", f.name, f.err)
	}
	if err := copyFixtureTree(f.dir, dst); err != nil {
		t.Fatalf("copying the %s git fixture: %v", f.name, err)
	}
}

// copyFixtureTree copies src into dst, keeping file modes and symlinks, and
// rewrites src to dst inside every file named config (a repo's .git/config,
// a bare repo's config, a submodule's .git/modules/<path>/config).
func copyFixtureTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return os.MkdirAll(target, info.Mode().Perm()|0o700)
		case d.Type()&fs.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		default:
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			// Only config files carry the fixture's absolute path today. A
			// fixture that clones (FETCH_HEAD, reflog), adds a worktree
			// (.git/worktrees/*/gitdir, the worktree's .git file) or fetches
			// must extend this rewrite to those files too.
			if d.Name() == "config" {
				data = bytes.ReplaceAll(data, []byte(src), []byte(dst))
			}
			return os.WriteFile(target, data, info.Mode().Perm())
		}
	})
}

// fixtureGit runs git in dir for a fixture build.
func fixtureGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

// fixtureSteps runs each argv in dir, stopping at the first failure.
func fixtureSteps(dir string, steps ...[]string) error {
	for _, args := range steps {
		if _, err := fixtureGit(dir, args...); err != nil {
			return err
		}
	}
	return nil
}

// buildCommittedRepo makes dir a repo with a test identity and one commit
// adding README.md ("# Test\n"). It drops the sample hooks, which nothing
// reads and every copy would otherwise carry.
func buildCommittedRepo(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := fixtureSteps(dir,
		[]string{"init"},
		[]string{"config", "user.email", "test@test.com"},
		[]string{"config", "user.name", "Test User"},
	); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Test\n"), 0o644); err != nil {
		return err
	}
	if err := fixtureSteps(dir, []string{"add", "."}, []string{"commit", "-m", "initial"}); err != nil {
		return err
	}
	samples, _ := filepath.Glob(filepath.Join(dir, ".git", "hooks", "*.sample"))
	for _, s := range samples {
		if err := os.Remove(s); err != nil {
			return err
		}
	}
	return nil
}

// fixtureMainBranch is the branch `git init` creates under the hermetic
// harness's gitconfig (init.defaultBranch = main).
const fixtureMainBranch = "main"

// testRepoFixture: the directory itself is a repo with one commit on main.
var testRepoFixture = &gitFixture{name: "repo", build: buildCommittedRepo}

// remoteFixture: remote.git, a bare repo, and local, a repo with one commit
// on main pushed to remote.git as origin with upstream tracking.
var remoteFixture = &gitFixture{name: "remote", build: func(dir string) error {
	remote := filepath.Join(dir, "remote.git")
	local := filepath.Join(dir, "local")
	if _, err := fixtureGit(dir, "init", "--bare", remote); err != nil {
		return err
	}
	if err := buildCommittedRepo(local); err != nil {
		return err
	}
	return fixtureSteps(local,
		[]string{"remote", "add", "origin", remote},
		[]string{"push", "-u", "origin", fixtureMainBranch},
	)
}}

// splitRemoteFixture: upstream.git and fork.git, bare repos both holding the
// initial commit on main, and local, whose origin fetches from upstream.git
// and pushes to fork.git.
var splitRemoteFixture = &gitFixture{name: "split", build: func(dir string) error {
	upstream := filepath.Join(dir, "upstream.git")
	fork := filepath.Join(dir, "fork.git")
	local := filepath.Join(dir, "local")
	for _, bare := range []string{upstream, fork} {
		if _, err := fixtureGit(dir, "init", "--bare", bare); err != nil {
			return err
		}
	}
	if err := buildCommittedRepo(local); err != nil {
		return err
	}
	if err := fixtureSteps(local,
		[]string{"remote", "add", "origin", upstream},
		[]string{"push", "origin", fixtureMainBranch},
		[]string{"push", fork, fixtureMainBranch},
	); err != nil {
		return err
	}
	return NewGit(local).ConfigurePushURL("origin", fork)
}}
