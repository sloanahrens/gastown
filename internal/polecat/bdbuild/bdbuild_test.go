package bdbuild

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	hostCommit = "39a04940092c1adaa4868cbc62723785a6096860"
	fullCommit = "3b90ab0c0ffee0000000000000000000000000aa"
)

// fakeBD answers `bd version --json` with out and err, and records the args.
type fakeBD struct {
	out    string
	stderr string
	err    error
	args   [][]string
}

func (f *fakeBD) run(_ context.Context, _ []string, args ...string) ([]byte, []byte, error) {
	f.args = append(f.args, args)
	return []byte(f.out), []byte(f.stderr), f.err
}

// gitCall is one git invocation a test's fake recorded, the -C directory first.
type gitCall []string

// fakeGit answers each shape of git call Compile makes, and records them. A
// remove it answers removes the worktree directory as git would, so a test can
// see the worktree is gone.
type fakeGit struct {
	revParse  string
	revErr    error
	addErr    error
	removeErr error
	calls     []gitCall
}

func (f *fakeGit) run(dir string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, append(gitCall{dir}, args...))
	switch {
	case len(args) >= 1 && args[0] == "rev-parse":
		if f.revErr != nil {
			return nil, f.revErr
		}
		return []byte(f.revParse + "\n"), nil
	case len(args) >= 2 && args[0] == "worktree" && args[1] == "add":
		return nil, f.addErr
	case len(args) >= 2 && args[0] == "worktree" && args[1] == "remove":
		if f.removeErr != nil {
			return nil, f.removeErr
		}
		os.RemoveAll(args[3])
		return nil, nil
	}
	return nil, nil
}

// find returns the recorded call whose arguments after the -C directory start
// with verb, or nil.
func (f *fakeGit) find(verb ...string) gitCall {
	for _, c := range f.calls {
		if len(c) < 1+len(verb) {
			continue
		}
		match := true
		for i, v := range verb {
			if c[1+i] != v {
				match = false
				break
			}
		}
		if match {
			return c
		}
	}
	return nil
}

// fakeGo records the one compile and optionally fails it.
type fakeGo struct {
	err  error
	dir  string
	env  []string
	args []string
}

func (f *fakeGo) run(dir string, env []string, args ...string) ([]byte, error) {
	f.dir, f.env, f.args = dir, env, args
	return nil, f.err
}

func TestCommitTakesTheInstalledBDsCommit(t *testing.T) {
	t.Parallel()
	bd := &fakeBD{out: `{"version":"1.2.2","build_id":"da4983e","commit":"` + hostCommit + `"}`}
	got, err := Commit(context.Background(), "", bd.run)
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if got != hostCommit {
		t.Errorf("Commit = %q, want the installed bd's commit %q", got, hostCommit)
	}
	if len(bd.args) != 1 || strings.Join(bd.args[0], " ") != "version --json" {
		t.Errorf("Commit ran %v, want one `bd version --json`", bd.args)
	}
}

func TestCommitTakesAnOverrideWithoutAskingBD(t *testing.T) {
	t.Parallel()
	bd := &fakeBD{out: `{"version":"1.2.2","commit":"` + hostCommit + `"}`}
	got, err := Commit(context.Background(), " "+fullCommit+"\n", bd.run)
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if got != fullCommit {
		t.Errorf("Commit = %q, want the override %q", got, fullCommit)
	}
	if len(bd.args) != 0 {
		t.Errorf("Commit ran bd %v for an override; the override is the answer", bd.args)
	}
}

func TestCommitRefusesAnOverrideThatIsNotACommit(t *testing.T) {
	t.Parallel()
	for _, override := range []string{"main", "f05e82", "zzzzzzz", "HEAD"} {
		bd := &fakeBD{}
		_, err := Commit(context.Background(), override, bd.run)
		if err == nil {
			t.Errorf("Commit accepted %q as a commit", override)
			continue
		}
		if !strings.Contains(err.Error(), override) {
			t.Errorf("Commit(%q): error does not name the override: %v", override, err)
		}
	}
}

func TestCommitRefusesAHostBDThatReportsNoCommit(t *testing.T) {
	t.Parallel()
	for _, out := range []string{
		`{"version":"1.2.2","build":"dev"}`,
		`{"version":"1.2.2","build_id":"da4983e","commit":""}`,
		`{"version":"1.2.2","commit":"v1.2.2"}`,
	} {
		bd := &fakeBD{out: out}
		_, err := Commit(context.Background(), "", bd.run)
		if err == nil {
			t.Errorf("Commit accepted %s", out)
			continue
		}
		if !strings.Contains(err.Error(), "commit") || !strings.Contains(err.Error(), "name the commit") {
			t.Errorf("Commit(%s): error does not say how to name the commit: %v", out, err)
		}
	}
}

func TestCommitRefusesAVersionItCannotRead(t *testing.T) {
	t.Parallel()
	bd := &fakeBD{err: errors.New("exit status 1"), stderr: "boom\n"}
	_, err := Commit(context.Background(), "", bd.run)
	if err == nil {
		t.Fatal("Commit accepted a bd that failed")
	}
	for _, want := range []string{"bd version --json failed", "boom"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Commit: error lacks %q: %v", want, err)
		}
	}
	notJSON := &fakeBD{out: "not json"}
	if _, err := Commit(context.Background(), "", notJSON.run); err == nil || !strings.Contains(err.Error(), "not JSON") {
		t.Errorf("Commit of non-JSON output: got %v, want a parse error", err)
	}
}

func TestCompileBuildsTheCommitFromACleanWorktree(t *testing.T) {
	t.Parallel()
	git := &fakeGit{revParse: fullCommit}
	build := &fakeGo{}
	out := filepath.Join(t.TempDir(), "bd")
	got, err := Compile(Options{Src: "/beads", Out: out, Commit: hostCommit, BuildDir: t.TempDir()}, git.run, build.run)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if got != fullCommit {
		t.Errorf("Compile = %q, want the commit's full sha %q", got, fullCommit)
	}

	rev := git.find("rev-parse")
	if rev == nil || rev[0] != "/beads" || rev[3] != hostCommit+"^{commit}" {
		t.Errorf("Compile resolved the commit with %v, want rev-parse --verify %s^{commit} in /beads", rev, hostCommit)
	}
	add := git.find("worktree", "add")
	if add == nil {
		t.Fatalf("Compile did not check the commit out; git calls: %v", git.calls)
	}
	if add[3] != "--detach" || add[5] != fullCommit {
		t.Errorf("Compile checked out %v, want --detach at the full sha %s", add, fullCommit)
	}
	if build.dir != add[4] {
		t.Errorf("Compile built in %q, want the worktree %q", build.dir, add[4])
	}
	wantArgs := []string{"build", "-trimpath", "-ldflags", "-X main.Commit=" + fullCommit, "-o", out, "./cmd/bd"}
	if strings.Join(build.args, " ") != strings.Join(wantArgs, " ") {
		t.Errorf("Compile ran go %v, want %v", build.args, wantArgs)
	}
	for _, want := range []string{"GOOS=linux", "GOARCH=arm64", "CGO_ENABLED=0"} {
		if !contains(build.env, want) {
			t.Errorf("Compile built with env %v, which lacks %q", build.env, want)
		}
	}
	if rm := git.find("worktree", "remove"); rm == nil || rm[4] != add[4] {
		t.Errorf("Compile left the worktree %q registered; git calls: %v", add[4], git.calls)
	}
}

func TestCompileResolvesARelativeOutputOutsideTheWorktree(t *testing.T) {
	t.Parallel()
	git := &fakeGit{revParse: fullCommit}
	build := &fakeGo{}
	if _, err := Compile(Options{Src: "/beads", Out: "deploy/polecat/bin/bd", Commit: fullCommit, BuildDir: t.TempDir()}, git.run, build.run); err != nil {
		t.Fatalf("Compile: %v", err)
	}
	want, err := filepath.Abs("deploy/polecat/bin/bd")
	if err != nil {
		t.Fatalf("resolving the expected output: %v", err)
	}
	add := git.find("worktree", "add")
	if add == nil {
		t.Fatalf("Compile did not check the commit out; git calls: %v", git.calls)
	}
	if build.args[5] != want {
		t.Errorf("Compile wrote the binary to %q, want %q", build.args[5], want)
	}
	if strings.HasPrefix(build.args[5], add[4]) {
		t.Errorf("Compile wrote the binary inside the worktree %q, which it deletes", add[4])
	}
}

func TestCompileDropsHostVariablesThatChangeTheBuild(t *testing.T) {
	t.Parallel()
	git := &fakeGit{revParse: fullCommit}
	build := &fakeGo{}
	host := []string{
		"PATH=/usr/bin:/bin",
		"HOME=/home/dev",
		"GOCACHE=/home/dev/.cache/go-build",
		"GOMODCACHE=/home/dev/go/pkg/mod",
		"GOPROXY=https://proxy.example",
		"GOENV=/home/dev/.config/go/env",
		"GOFLAGS=-tags=hostjob",
		"GOEXPERIMENT=loopvar",
		"GOARM64=v9.0",
		"GOTOOLCHAIN=go1.99.0",
		"GOWORK=/elsewhere/go.work",
	}
	if _, err := Compile(Options{Src: "/beads", Out: "/out/bd", Commit: fullCommit, Env: host}, git.run, build.run); err != nil {
		t.Fatalf("Compile: %v", err)
	}
	for _, want := range []string{
		"PATH=/usr/bin:/bin", "HOME=/home/dev",
		"GOCACHE=/home/dev/.cache/go-build", "GOMODCACHE=/home/dev/go/pkg/mod",
		"GOPROXY=https://proxy.example",
		"GOOS=linux", "GOARCH=arm64", "CGO_ENABLED=0", "GOENV=off",
	} {
		if !contains(build.env, want) {
			t.Errorf("Compile built with env %v, which lacks %q", build.env, want)
		}
	}
	if contains(build.env, "GOENV=/home/dev/.config/go/env") {
		t.Errorf("Compile pointed the build at the host's go env file; GOENV=off disables it")
	}
	for _, dropped := range []string{"GOFLAGS=", "GOEXPERIMENT=", "GOARM64=", "GOTOOLCHAIN=", "GOWORK="} {
		for _, kv := range build.env {
			if strings.HasPrefix(kv, dropped) {
				t.Errorf("Compile passed %q to the build; %s changes what gets compiled", kv, dropped)
			}
		}
	}
}

func TestCompileRefusesACommitTheCheckoutDoesNotHold(t *testing.T) {
	t.Parallel()
	git := &fakeGit{revErr: errors.New("fatal: Needed a single revision")}
	build := &fakeGo{}
	_, err := Compile(Options{Src: "/beads", Out: "/out/bd", Commit: fullCommit, BuildDir: t.TempDir()}, git.run, build.run)
	if err == nil {
		t.Fatal("Compile built a commit the checkout does not hold")
	}
	for _, want := range []string{fullCommit, "/beads", "does not hold", "Needed a single revision"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Compile: error lacks %q: %v", want, err)
		}
	}
	if build.dir != "" {
		t.Errorf("Compile built in %q after the commit was refused", build.dir)
	}
}

func TestCompileRemovesTheWorktreeWhenTheBuildFails(t *testing.T) {
	t.Parallel()
	git := &fakeGit{revParse: fullCommit}
	build := &fakeGo{err: errors.New("exit status 1")}
	_, err := Compile(Options{Src: "/beads", Out: "/out/bd", Commit: fullCommit, BuildDir: t.TempDir()}, git.run, build.run)
	if err == nil || !strings.Contains(err.Error(), fullCommit) {
		t.Fatalf("Compile = %v, want a build error naming %s", err, fullCommit)
	}
	if add := git.find("worktree", "add"); add == nil || git.find("worktree", "remove") == nil {
		t.Errorf("Compile did not remove the worktree after a failed build; git calls: %v", git.calls)
	}
}

func TestCompileDeletesTheWorktreeWhenGitCannotRemoveIt(t *testing.T) {
	t.Parallel()
	git := &fakeGit{revParse: fullCommit, removeErr: errors.New("exit status 1")}
	build := &fakeGo{}
	if _, err := Compile(Options{Src: "/beads", Out: "/out/bd", Commit: fullCommit, BuildDir: t.TempDir()}, git.run, build.run); err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if git.find("worktree", "prune") != nil {
		t.Errorf("Compile ran worktree prune on the operator's checkout; git calls: %v", git.calls)
	}
	add := git.find("worktree", "add")
	if add == nil {
		t.Fatalf("Compile did not check the commit out; git calls: %v", git.calls)
	}
	if _, err := os.Stat(add[4]); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Compile left the worktree %q behind after git's remove failed: %v", add[4], err)
	}
}

func TestCompileNamesWhatItCannotBuild(t *testing.T) {
	t.Parallel()
	git := &fakeGit{revParse: fullCommit}
	build := &fakeGo{}
	for _, opt := range []Options{
		{Out: "/out/bd", Commit: fullCommit},
		{Src: "/beads", Commit: fullCommit},
		{Src: "/beads", Out: "/out/bd", Commit: "HEAD"},
	} {
		if _, err := Compile(opt, git.run, build.run); err == nil {
			t.Errorf("Compile accepted %+v", opt)
		}
	}
	if len(git.calls) != 0 {
		t.Errorf("Compile ran git %v for options it refused before reading anything", git.calls)
	}
}

func TestCompilePutsTheWorktreeUnderTheBuildDir(t *testing.T) {
	t.Parallel()
	buildDir := t.TempDir()
	git := &fakeGit{revParse: fullCommit}
	build := &fakeGo{}
	if _, err := Compile(Options{Src: "/beads", Out: "/out/bd", Commit: fullCommit, BuildDir: buildDir}, git.run, build.run); err != nil {
		t.Fatalf("Compile: %v", err)
	}
	add := git.find("worktree", "add")
	if add == nil || filepath.Dir(add[4]) != buildDir {
		t.Errorf("Compile made the worktree at %v, want a directory under %s", add, buildDir)
	}
	if _, err := os.Stat(add[4]); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Compile left %q behind: %v", add[4], err)
	}
}

func TestShortCommit(t *testing.T) {
	t.Parallel()
	if got := shortCommit(fullCommit); got != fullCommit[:12] {
		t.Errorf("shortCommit(%q) = %q", fullCommit, got)
	}
	if got := shortCommit("da4983e"); got != "da4983e" {
		t.Errorf("shortCommit of an abbreviated commit = %q", got)
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
