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

// TestBuildsBdgwAndShim: the gateway image's two binaries are cross-compiled for
// linux/arm64 from the gt checkout at rev, each in its own clean worktree, and
// land in the output directory under the names the image copies.
func TestBuildsBdgwAndShim(t *testing.T) {
	t.Parallel()
	b, run := builderFor(t)
	paths, err := b.BuildGateway(context.Background(), testRev)
	if err != nil {
		t.Fatalf("BuildGateway: %v", err)
	}
	want := []string{filepath.Join(b.Out, "bdgw"), filepath.Join(b.Out, "bdshim")}
	if !reflect.DeepEqual(paths, want) {
		t.Errorf("BuildGateway paths = %q, want %q", paths, want)
	}
	for _, p := range paths {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("BuildGateway returned %s, which is not written: %v", p, err)
		}
	}
	names := []string{"bdgw", "bdshim"}
	pkgs := []string{BDGWPackage, BDShimPackage}
	if len(run.calls) != len(names) {
		t.Fatalf("recorded %d builds, want %d", len(run.calls), len(names))
	}
	for i, call := range run.calls {
		for _, want := range []string{"CGO_ENABLED=0", "GOOS=linux", "GOARCH=arm64", "GOENV=off"} {
			if !contains(call.env, want) {
				t.Errorf("%s build env = %q, want %s among them", names[i], call.env, want)
			}
		}
		wantArgv := []string{"go", "build", "-trimpath", "-o", "OUT", pkgs[i]}
		if got := withOut(call.argv); !reflect.DeepEqual(got, wantArgv) {
			t.Errorf("%s build argv = %q, want %q", names[i], call.argv, wantArgv)
		}
	}
	var added []worktreeAdd
	for _, a := range run.adds {
		added = append(added, worktreeAdd{src: a.src, commit: a.commit})
	}
	wantAdds := []worktreeAdd{{src: b.GTSrc, commit: testRev}, {src: b.GTSrc, commit: testRev}}
	if !reflect.DeepEqual(added, wantAdds) {
		t.Errorf("worktrees added = %+v, want each build in a clean worktree of %s at %s", added, b.GTSrc, testRev)
	}
}

// TestBuildGatewayFailsWithoutLeavingPartialOutput: a build that fails returns
// its error, lists no paths and leaves the output directory empty, whichever of
// the two binaries fails.
func TestBuildGatewayFailsWithoutLeavingPartialOutput(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, pkg string }{
		{"bdgw", BDGWPackage},
		{"bdshim", BDShimPackage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b, run := builderFor(t)
			run.failPkg = tc.pkg
			paths, err := b.BuildGateway(context.Background(), testRev)
			if err == nil {
				t.Fatalf("a failed %s build returned no error (paths %q)", tc.name, paths)
			}
			if len(paths) != 0 {
				t.Errorf("a failed %s build listed %q, want no paths", tc.name, paths)
			}
			entries, err := os.ReadDir(b.Out)
			if err != nil {
				t.Fatalf("reading the output directory: %v", err)
			}
			if len(entries) != 0 {
				t.Errorf("a failed %s build left %d entries in the output directory, want none", tc.name, len(entries))
			}
		})
	}
}

// TestBuildGatewayRemovesBdgwWhenBdshimCannotBeMoved: when the second binary
// cannot be placed, the first one already moved is taken back out, so the call
// leaves no partial pair. A non-empty directory at the shim's path makes its
// rename fail.
func TestBuildGatewayRemovesBdgwWhenBdshimCannotBeMoved(t *testing.T) {
	t.Parallel()
	b, _ := builderFor(t)
	blocker := filepath.Join(b.Out, "bdshim")
	if err := os.Mkdir(blocker, 0o700); err != nil {
		t.Fatalf("making the blocking directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(blocker, "keep"), []byte("x"), 0o600); err != nil {
		t.Fatalf("filling the blocking directory: %v", err)
	}
	paths, err := b.BuildGateway(context.Background(), testRev)
	if err == nil {
		t.Fatalf("BuildGateway over a blocked bdshim path returned no error (paths %q)", paths)
	}
	if len(paths) != 0 {
		t.Errorf("BuildGateway listed %q, want no paths", paths)
	}
	if _, err := os.Stat(filepath.Join(b.Out, "bdgw")); !os.IsNotExist(err) {
		t.Errorf("bdgw is still in the output directory after bdshim failed to move (stat err %v)", err)
	}
}

// TestBuildGatewayReportsAFailedUndo: when a move fails and the binary already
// moved cannot be removed either, the call's error names both failures, so the
// partial pair is reported rather than hidden.
func TestBuildGatewayReportsAFailedUndo(t *testing.T) {
	t.Parallel()
	b, _ := builderFor(t)
	b.rename = func(oldpath, newpath string) error {
		if filepath.Base(newpath) == "bdshim" {
			return errors.New("rename refused")
		}
		return os.Rename(oldpath, newpath)
	}
	b.remove = func(string) error { return errors.New("remove refused") }
	_, err := b.BuildGateway(context.Background(), testRev)
	if err == nil {
		t.Fatal("BuildGateway returned no error when its move and its undo both failed")
	}
	for _, want := range []string{"rename refused", "remove refused", "bdgw"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error %q does not name %q", err, want)
		}
	}
}

// TestBuildGatewayRefusesARevTheSourceLacks: a rev the gt checkout does not hold
// is refused before any build runs, so a stamp never names a commit the build
// did not read.
func TestBuildGatewayRefusesARevTheSourceLacks(t *testing.T) {
	t.Parallel()
	b, run := builderFor(t)
	if _, err := b.BuildGateway(context.Background(), "1111111111111111111111111111111111111111"); err == nil {
		t.Fatal("BuildGateway built from a rev the gt checkout does not hold")
	}
	if len(run.calls) != 0 {
		t.Errorf("a refused call ran %d builds, want none", len(run.calls))
	}
}

// TestBuildGatewayRefusesBeforeAnyBuild: each input BuildGateway cannot build
// from is refused before the first build runs, so the refusal writes nothing.
func TestBuildGatewayRefusesBeforeAnyBuild(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		mutate func(*Builder)
	}{
		{"relative output directory", func(b *Builder) { b.Out = "out" }},
		{"no output directory", func(b *Builder) { b.Out = "" }},
		{"no gt source", func(b *Builder) { b.GTSrc = "" }},
		{"no runner", func(b *Builder) { b.Run = nil }},
		{"no git", func(b *Builder) { b.Git = nil }},
		{"gt source inside a canonical clone", func(b *Builder) { b.GTSrc = filepath.Join("town", "rig", "mayor", "rig") }},
		{"output directory that does not exist", func(b *Builder) { b.Out = filepath.Join(b.Out, "missing") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b, run := builderFor(t)
			tc.mutate(&b)
			if _, err := b.BuildGateway(context.Background(), testRev); err == nil {
				t.Fatal("BuildGateway accepted an input it cannot build from")
			}
			if len(run.calls) != 0 {
				t.Errorf("a refused call ran %d builds, want none", len(run.calls))
			}
		})
	}
}
