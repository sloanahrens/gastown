package daemonimage

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestBuildsBdgw: the gateway image's binary is cross-compiled for linux/arm64
// from the gt checkout at rev, in a clean worktree of it, and lands in the
// output directory under the name the image copies. It is the only binary the
// call writes: the image runs the real bd, not a shim (gt-gi5lhe).
func TestBuildsBdgw(t *testing.T) {
	t.Parallel()
	b, run := builderFor(t)
	paths, err := b.BuildGateway(context.Background(), testRev)
	if err != nil {
		t.Fatalf("BuildGateway: %v", err)
	}
	want := []string{filepath.Join(b.Out, "bdgw")}
	if !reflect.DeepEqual(paths, want) {
		t.Errorf("BuildGateway paths = %q, want %q", paths, want)
	}
	for _, p := range paths {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("BuildGateway returned %s, which is not written: %v", p, err)
		}
	}
	entries, err := os.ReadDir(b.Out)
	if err != nil {
		t.Fatalf("reading the output directory: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "bdgw" {
		t.Errorf("the output directory holds %d entries after the build, want bdgw alone", len(entries))
	}
	if len(run.calls) != 1 {
		t.Fatalf("recorded %d builds, want the one bdgw build", len(run.calls))
	}
	call := run.calls[0]
	for _, want := range []string{"CGO_ENABLED=0", "GOOS=linux", "GOARCH=arm64", "GOENV=off"} {
		if !contains(call.env, want) {
			t.Errorf("bdgw build env = %q, want %s among them", call.env, want)
		}
	}
	wantArgv := []string{"go", "build", "-trimpath", "-o", "OUT", BDGWPackage}
	if got := withOut(call.argv); !reflect.DeepEqual(got, wantArgv) {
		t.Errorf("bdgw build argv = %q, want %q", call.argv, wantArgv)
	}
	wantAdds := []worktreeAdd{{src: b.GTSrc, commit: testRev}}
	if got := addsOf(run); !reflect.DeepEqual(got, wantAdds) {
		t.Errorf("worktrees added = %+v, want %+v", got, wantAdds)
	}
}

// addsOf is the source and commit of every worktree the fake git added.
func addsOf(run *fakeRunner) []worktreeAdd {
	var added []worktreeAdd
	for _, a := range run.adds {
		added = append(added, worktreeAdd{src: a.src, commit: a.commit})
	}
	return added
}

// TestBuildGatewayFailsWithoutLeavingPartialOutput: a build that fails returns
// its error, lists no paths and leaves the output directory empty.
func TestBuildGatewayFailsWithoutLeavingPartialOutput(t *testing.T) {
	t.Parallel()
	b, run := builderFor(t)
	run.failPkg = BDGWPackage
	paths, err := b.BuildGateway(context.Background(), testRev)
	if err == nil {
		t.Fatalf("a failed bdgw build returned no error (paths %q)", paths)
	}
	if len(paths) != 0 {
		t.Errorf("a failed bdgw build listed %q, want no paths", paths)
	}
	entries, err := os.ReadDir(b.Out)
	if err != nil {
		t.Fatalf("reading the output directory: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("a failed bdgw build left %d entries in the output directory, want none", len(entries))
	}
}

// TestBuildGatewayFailsWhenTheOutputCannotBeMoved: an output that cannot be
// moved into Out fails the call, and the staging directory is removed, so Out
// holds nothing but what was already there.
func TestBuildGatewayFailsWhenTheOutputCannotBeMoved(t *testing.T) {
	t.Parallel()
	b, _ := builderFor(t)
	// rename(2) cannot put a file over a directory, so bdgw cannot be moved in.
	if err := os.Mkdir(filepath.Join(b.Out, "bdgw"), 0o755); err != nil {
		t.Fatalf("making the blocking directory: %v", err)
	}
	paths, err := b.BuildGateway(context.Background(), testRev)
	if err == nil || !strings.Contains(err.Error(), "cannot move bdgw") {
		t.Fatalf("BuildGateway = %q, %v; want an error naming the bdgw move", paths, err)
	}
	if len(paths) != 0 {
		t.Errorf("a failed move listed %q, want no paths", paths)
	}
	entries, err := os.ReadDir(b.Out)
	if err != nil {
		t.Fatalf("reading the output directory: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "bdgw" {
		t.Errorf("the output directory holds %d entries after a failed move, want only the blocking bdgw directory", len(entries))
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
