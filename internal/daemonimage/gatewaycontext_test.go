package daemonimage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestBuildBDOnly: the gt-bd image's real bd is one build of the bd source at
// its pinned commit, stamped with the flags BuildBinaries stamps bd with, and it
// writes bd.real and nothing else.
func TestBuildBDOnly(t *testing.T) {
	t.Parallel()
	b, run := builderFor(t)
	stage := t.TempDir()
	if err := b.buildBDOnly(context.Background(), b.BDSrc, testBDCommit, stage, nil); err != nil {
		t.Fatalf("buildBDOnly: %v", err)
	}
	entries, err := os.ReadDir(stage)
	if err != nil {
		t.Fatalf("reading the staging directory: %v", err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if got, want := strings.Join(names, " "), bdRealName; got != want {
		t.Errorf("the bd-only build wrote %q, want %q", got, want)
	}
	if len(run.calls) != 1 {
		t.Fatalf("recorded %d builds, want the one bd build", len(run.calls))
	}
	call := run.calls[0]
	wantArgv := []string{"go", "build", "-trimpath", "-ldflags", "-s -w " + bdBuildFlag + testBDShort + " " + bdCommitFlag + testBDCommit, "-o", "OUT", BDPackage}
	if got := withOut(call.argv); !reflect.DeepEqual(got, wantArgv) {
		t.Errorf("bd build argv = %q, want %q", call.argv, wantArgv)
	}
	wantAdds := []worktreeAdd{{src: b.BDSrc, commit: testBDCommit}}
	if got := addsOf(run); !reflect.DeepEqual(got, wantAdds) {
		t.Errorf("worktrees added = %+v, want %+v", got, wantAdds)
	}
	if len(run.removed) != 1 {
		t.Errorf("removed %d worktrees, want the one bd worktree", len(run.removed))
	}
	// The stamps are the ones BuildBinaries' bd build carries, so the two
	// images' bd report one build id (gt-glsx8).
	binaries, binaryRun := builderFor(t)
	if _, err := binaries.BuildBinaries(context.Background(), testRev); err != nil {
		t.Fatalf("BuildBinaries: %v", err)
	}
	if got, want := ldflagsOf(call.argv), ldflagsOf(binaryRun.calls[1].argv); !reflect.DeepEqual(got, want) {
		t.Errorf("bd.real ldflags = %q, BuildBinaries' bd ldflags = %q, want them equal", got, want)
	}
}

// TestStageGatewayContext: the staged context holds bd.real, bdgw and verbs.json,
// the three files deploy/bd/Dockerfile.bd copies; the policy is the built rev's
// bytes, never the working tree's; and the result names the full bd commit
// bd.real was stamped with.
func TestStageGatewayContext(t *testing.T) {
	t.Parallel()
	t.Run("the context holds bd.real, bdgw and verbs.json", func(t *testing.T) {
		t.Parallel()
		b, _ := gatewayBuilder(t)
		paths, bdCommit, err := b.StageGatewayContext(context.Background(), testRev)
		if err != nil {
			t.Fatalf("StageGatewayContext: %v", err)
		}
		want := []string{filepath.Join(b.Out, bdRealName), filepath.Join(b.Out, gwName), filepath.Join(b.Out, verbsName)}
		if !reflect.DeepEqual(paths, want) {
			t.Errorf("StageGatewayContext paths = %q, want %q", paths, want)
		}
		for _, p := range paths {
			if _, err := os.Stat(p); err != nil {
				t.Errorf("StageGatewayContext returned %s, which is not written: %v", p, err)
			}
		}
		entries, err := os.ReadDir(b.Out)
		if err != nil {
			t.Fatalf("reading the build context: %v", err)
		}
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		if got, want := strings.Join(names, " "), "bd.real bdgw verbs.json"; got != want {
			t.Errorf("the build context holds %q, want %q", got, want)
		}
		if bdCommit != testBDCommit {
			t.Errorf("StageGatewayContext named the bd commit %q, want the full %s", bdCommit, testBDCommit)
		}
	})
	t.Run("the policy is read at the built rev, not the working tree", func(t *testing.T) {
		t.Parallel()
		b, _ := gatewayBuilder(t)
		// The working tree holds a different policy from the commit bdgw is
		// built from. The image must carry the commit's bytes, or its policy
		// and its binary can name different revisions.
		if err := os.MkdirAll(filepath.Join(b.GTSrc, "deploy", "bd"), 0o755); err != nil {
			t.Fatalf("making the working tree's deploy/bd: %v", err)
		}
		if err := os.WriteFile(filepath.Join(b.GTSrc, verbsRelPath), []byte("{\"working\": true}\n"), 0o644); err != nil {
			t.Fatalf("writing the working tree's verbs.json: %v", err)
		}
		if _, _, err := b.StageGatewayContext(context.Background(), testRev); err != nil {
			t.Fatalf("StageGatewayContext: %v", err)
		}
		got, err := os.ReadFile(filepath.Join(b.Out, verbsName))
		if err != nil {
			t.Fatalf("reading the staged %s: %v", verbsName, err)
		}
		if string(got) != gatewayVerbs {
			t.Errorf("the staged %s = %q, want the built rev's policy %q, not the working tree's", verbsName, got, gatewayVerbs)
		}
	})
	t.Run("bd.real is built at the bd pin in its own clean worktree", func(t *testing.T) {
		t.Parallel()
		b, run := gatewayBuilder(t)
		_, bdCommit, err := b.StageGatewayContext(context.Background(), testRev)
		if err != nil {
			t.Fatalf("StageGatewayContext: %v", err)
		}
		wantAdds := []worktreeAdd{{src: b.GTSrc, commit: testRev}, {src: b.BDSrc, commit: testBDCommit}}
		if got := addsOf(run); !reflect.DeepEqual(got, wantAdds) {
			t.Errorf("worktrees added = %+v, want %+v", got, wantAdds)
		}
		if len(run.shortDirs) != 1 {
			t.Fatalf("git was asked for a short sha in %d directories, want once, in the bd worktree", len(run.shortDirs))
		}
		if bdWorktree := run.adds[1].dir; run.shortDirs[0] != bdWorktree {
			t.Errorf("git was asked for the short sha in %q, want the bd worktree %q", run.shortDirs[0], bdWorktree)
		}
		if got := ldflagsOf(run.calls[1].argv); !reflect.DeepEqual(got, []string{"-s -w " + bdBuildFlag + testBDShort + " " + bdCommitFlag + bdCommit}) {
			t.Errorf("bd.real ldflags = %q, want the returned commit %s stamped", got, bdCommit)
		}
	})
}

// TestStageGatewayContextRefusals: an input StageGatewayContext cannot stage
// from is refused before any build runs and writes nothing.
func TestStageGatewayContextRefusals(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		mutate func(*Builder)
		rev    string
		want   string
	}{
		{"a rev that is not a full commit", nil, testRev[:12], "not a full 40-character lowercase hex commit"},
		{"an empty rev", nil, "", "no rev"},
		{"a bd pin that is not a full commit", func(b *Builder) { b.BDCommit = testBDCommit[:12] }, testRev, "not a full 40-character lowercase hex commit"},
		{"an empty bd pin", func(b *Builder) { b.BDCommit = "" }, testRev, "no bd commit"},
		{"no runner", func(b *Builder) { b.Run = nil }, testRev, "no runner"},
		{"no git", func(b *Builder) { b.Git = nil }, testRev, "no git"},
		{"no output directory", func(b *Builder) { b.Out = "" }, testRev, "no output directory"},
		{"a relative output directory", func(b *Builder) { b.Out = "out" }, testRev, "is not absolute"},
		{"an output directory that does not exist", func(b *Builder) { b.Out = filepath.Join(b.Out, "missing") }, testRev, "cannot stage the gt-bd build context"},
		{"no bd source and no bd pin", func(b *Builder) { b.BDSrc = "" }, testRev, "no --bd-src and no bd pin"},
		{"a bd pin the bd source does not hold", func(b *Builder) { b.BDCommit = "2222222222222222222222222222222222222222" }, testRev, "does not hold commit"},
		{"no gt source", func(b *Builder) { b.GTSrc = "" }, testRev, "no gt source"},
		{"gt source inside a canonical clone", func(b *Builder) { b.GTSrc = filepath.Join("town", "rig", "mayor", "rig") }, testRev, "canonical clone"},
		{"bd source inside a canonical clone", func(b *Builder) { b.BDSrc = filepath.Join("town", "rig", "mayor", "rig") }, testRev, "canonical clone"},
		{"output directory inside a canonical clone", func(b *Builder) {
			b.Out = filepath.Join(string(filepath.Separator), "town", "rig", "mayor", "rig")
		}, testRev, "canonical clone"},
		{"a rev the gt source does not hold", nil, "1111111111111111111111111111111111111111", "does not hold commit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b, run := gatewayBuilder(t)
			if tc.mutate != nil {
				tc.mutate(&b)
			}
			paths, commit, err := b.StageGatewayContext(context.Background(), tc.rev)
			if err == nil {
				t.Fatalf("StageGatewayContext staged %q from an input it cannot stage from", paths)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("StageGatewayContext error = %v, want it to name %q", err, tc.want)
			}
			if len(paths) != 0 || commit != "" {
				t.Errorf("a refused call returned %q and commit %q, want neither", paths, commit)
			}
			if len(run.calls) != 0 {
				t.Errorf("a refused call ran %d builds, want none", len(run.calls))
			}
		})
	}
}

// TestStageGatewayContextRefusesWithoutThePolicy: a rev whose commit holds no
// verbs.json is refused before any build runs, so the image never starts without
// its policy, and nothing is staged.
func TestStageGatewayContextRefusesWithoutThePolicy(t *testing.T) {
	t.Parallel()
	b, run := gatewayBuilder(t)
	delete(run.files, testRev+":"+verbsRelPath)
	paths, commit, err := b.StageGatewayContext(context.Background(), testRev)
	if err == nil || !strings.Contains(err.Error(), "verbs.json") {
		t.Fatalf("StageGatewayContext = %q, %q, %v; want an error naming verbs.json", paths, commit, err)
	}
	if len(paths) != 0 || commit != "" {
		t.Errorf("a refused call returned %q and commit %q, want neither", paths, commit)
	}
	if len(run.calls) != 0 {
		t.Errorf("a refused call ran %d builds, want none", len(run.calls))
	}
	assertDirEmpty(t, b.Out)
}

// TestStageGatewayContextFailsWithoutLeavingPartialOutput: a failed build
// returns its error and no paths, and leaves nothing in the output directory.
func TestStageGatewayContextFailsWithoutLeavingPartialOutput(t *testing.T) {
	t.Parallel()
	for _, pkg := range []string{BDGWPackage, BDPackage} {
		t.Run(pkg, func(t *testing.T) {
			t.Parallel()
			b, run := gatewayBuilder(t)
			run.failPkg = pkg
			paths, commit, err := b.StageGatewayContext(context.Background(), testRev)
			if err == nil {
				t.Fatalf("a failed %s build returned no error (paths %q)", pkg, paths)
			}
			if len(paths) != 0 || commit != "" {
				t.Errorf("a failed build returned %q and commit %q, want neither", paths, commit)
			}
			assertDirEmpty(t, b.Out)
		})
	}
}

// TestStageGatewayContextTakesBackWhatItMoved: a context file that cannot be
// moved into the output directory fails the call, and the files already moved
// are taken back out, so a failed call leaves the context empty. A file it
// cannot take back out is reported, so a caller never reads a clean refusal over
// a context this call left changed.
func TestStageGatewayContextTakesBackWhatItMoved(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		remove func(string) error
		want   int
	}{
		{name: "the files it moved are taken back out"},
		{
			name:   "a file it cannot take back out is reported",
			remove: func(string) error { return errors.New("remove refused") },
			want:   2,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b, _ := gatewayBuilder(t)
			out := b.Out
			b.rename = func(oldpath, newpath string) error {
				if newpath == filepath.Join(out, verbsName) {
					return errors.New("verbs move refused")
				}
				return os.Rename(oldpath, newpath)
			}
			b.remove = tc.remove
			paths, commit, err := b.StageGatewayContext(context.Background(), testRev)
			if err == nil {
				t.Fatalf("StageGatewayContext = %q, %q, nil; want an error naming the verbs.json move", paths, commit)
			}
			for _, want := range []string{"cannot move verbs.json", "verbs move refused"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("StageGatewayContext error = %v, want it to contain %q", err, want)
				}
			}
			if tc.remove != nil {
				for _, want := range []string{"cannot take the staged build context back out", "remove refused"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("StageGatewayContext error = %v, want it to contain %q", err, want)
					}
				}
			}
			if len(paths) != 0 || commit != "" {
				t.Errorf("a failed move returned %q and commit %q, want neither", paths, commit)
			}
			entries, readErr := os.ReadDir(out)
			if readErr != nil {
				t.Fatalf("reading the build context: %v", readErr)
			}
			if len(entries) != tc.want {
				t.Errorf("the build context holds %d entries after a failed move, want %d", len(entries), tc.want)
			}
		})
	}
}

// assertDirEmpty fails the test unless dir holds no entries.
func assertDirEmpty(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	if len(entries) != 0 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("%s holds %q after the call, want nothing", dir, strings.Join(names, " "))
	}
}
