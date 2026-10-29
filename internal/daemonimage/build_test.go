package daemonimage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/polecat/bdbuild"
)

const testRev = "0123456789abcdef0123456789abcdef01234567"

// recorded is one call a fake runner was given.
type recorded struct {
	dir  string
	env  []string
	argv []string
}

// fakeRunner records every call and fails the one whose package is failPkg.
type fakeRunner struct {
	calls   []recorded
	failPkg string
}

func (f *fakeRunner) run(_ context.Context, dir string, env, argv []string) error {
	f.calls = append(f.calls, recorded{
		dir:  dir,
		env:  append([]string(nil), env...),
		argv: append([]string(nil), argv...),
	})
	if f.failPkg != "" && argv[len(argv)-1] == f.failPkg {
		return errors.New("build failed")
	}
	return nil
}

// builderFor is a Builder with every source named and a fresh output directory.
func builderFor(t *testing.T) (Builder, *fakeRunner) {
	t.Helper()
	run := &fakeRunner{}
	return Builder{
		Run:   run.run,
		Out:   t.TempDir(),
		GTSrc: filepath.Join("src", "gt"),
		BDSrc: filepath.Join("src", "beads"),
		OMSrc: filepath.Join("src", "om"),
	}, run
}

func TestBuildBinariesReturnsTheThreePathsUnderOut(t *testing.T) {
	t.Parallel()
	b, _ := builderFor(t)
	paths, err := b.BuildBinaries(context.Background(), testRev)
	if err != nil {
		t.Fatalf("BuildBinaries: %v", err)
	}
	want := []string{
		filepath.Join(b.Out, "gt"),
		filepath.Join(b.Out, "bd"),
		filepath.Join(b.Out, "om"),
	}
	if !reflect.DeepEqual(paths, want) {
		t.Errorf("BuildBinaries paths = %q, want %q", paths, want)
	}
}

func TestBuildBinariesRecordsTheCrossCompile(t *testing.T) {
	t.Parallel()
	b, run := builderFor(t)
	if _, err := b.BuildBinaries(context.Background(), testRev); err != nil {
		t.Fatalf("BuildBinaries: %v", err)
	}
	names := []string{"gt", "bd", "om"}
	pkgs := []string{GTPackage, BDPackage, OMPackage}
	srcs := []string{b.GTSrc, b.BDSrc, b.OMSrc}
	if len(run.calls) != len(names) {
		t.Fatalf("recorded %d builds, want %d", len(run.calls), len(names))
	}
	for i, call := range run.calls {
		for _, want := range []string{"CGO_ENABLED=0", "GOOS=linux", "GOARCH=arm64"} {
			if !contains(call.env, want) {
				t.Errorf("%s build env = %q, want %s among them", names[i], call.env, want)
			}
		}
		if call.dir != srcs[i] {
			t.Errorf("%s build ran in %q, want %q", names[i], call.dir, srcs[i])
		}
		wantArgv := []string{"go", "build", "-trimpath", "-o", filepath.Join(b.Out, names[i]), pkgs[i]}
		got := call.argv
		if i == 0 {
			got = withoutGTLdflags(got)
		}
		if !reflect.DeepEqual(got, wantArgv) {
			t.Errorf("%s build argv = %q, want %q", names[i], call.argv, wantArgv)
		}
	}
}

func TestBuildBinariesStampsGTWithTheRev(t *testing.T) {
	t.Parallel()
	b, run := builderFor(t)
	if _, err := b.BuildBinaries(context.Background(), testRev); err != nil {
		t.Fatalf("BuildBinaries: %v", err)
	}
	if len(run.calls) == 0 {
		t.Fatal("no build was recorded")
	}
	argv := run.calls[0].argv
	var flags []string
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == "-ldflags" {
			flags = append(flags, argv[i+1])
		}
	}
	if len(flags) != 1 || flags[0] != gtCommitFlag+testRev {
		t.Errorf("gt build ldflags = %q, want [%q]", flags, gtCommitFlag+testRev)
	}
}

func TestBuildBinariesFailsWithoutListingPartialOutput(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, pkg string }{
		{"gt", GTPackage},
		{"bd", BDPackage},
		{"om", OMPackage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b, run := builderFor(t)
			run.failPkg = tc.pkg
			paths, err := b.BuildBinaries(context.Background(), testRev)
			if err == nil {
				t.Fatalf("a failed %s build returned no error (paths %q)", tc.name, paths)
			}
			if len(paths) != 0 {
				t.Errorf("a failed %s build listed %q, want no paths", tc.name, paths)
			}
			if !strings.Contains(err.Error(), tc.pkg) {
				t.Errorf("the error %q does not name the %s build", err, tc.name)
			}
		})
	}
}

func TestBuildBinariesRefusesAnUnusableBuilder(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		breakIt func(*Builder)
		rev     string
		want    string
	}{
		{name: "no runner", breakIt: func(b *Builder) { b.Run = nil }, rev: testRev, want: "no runner"},
		{name: "no output directory", breakIt: func(b *Builder) { b.Out = "" }, rev: testRev, want: "no output directory"},
		{name: "no rev", breakIt: func(*Builder) {}, rev: " ", want: "no rev"},
		{name: "no gt source", breakIt: func(b *Builder) { b.GTSrc = "" }, rev: testRev, want: "no gt source"},
		{name: "no bd source", breakIt: func(b *Builder) { b.BDSrc = "" }, rev: testRev, want: "no bd source"},
		{name: "no om source", breakIt: func(b *Builder) { b.OMSrc = "" }, rev: testRev, want: "no om source"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			run := &fakeRunner{}
			b := Builder{Run: run.run, Out: "out", GTSrc: "gt", BDSrc: "beads", OMSrc: "om"}
			tc.breakIt(&b)
			paths, err := b.BuildBinaries(context.Background(), tc.rev)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("BuildBinaries = %q, %v; want an error naming %q", paths, err, tc.want)
			}
			if len(paths) != 0 {
				t.Errorf("a refused build listed %q, want no paths", paths)
			}
		})
	}
}

func TestBDPinMatchesThePolecatImageFlow(t *testing.T) {
	t.Parallel()
	if BDPackage != bdbuild.Package {
		t.Errorf("the daemon image builds bd from %q, the polecat image from %q", BDPackage, bdbuild.Package)
	}
}

func TestGTPinMatchesThePolecatImageFlow(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile(filepath.Join("..", "..", "Makefile"))
	if err != nil {
		t.Fatalf("reading the repo Makefile: %v", err)
	}
	argv := strings.Fields(polecatImageGTBuild(t, string(src)))
	out := -1
	for i, arg := range argv {
		if arg == "-o" {
			out = i
		}
	}
	if out < 0 || out+2 >= len(argv) {
		t.Fatalf("the polecat image's gt build names no output (%q)", argv)
	}
	if argv[out+1] != "deploy/polecat/bin/gt" {
		t.Errorf("the polecat image writes gt to %q, want deploy/polecat/bin/gt", argv[out+1])
	}
	if pkg := argv[len(argv)-1]; pkg != GTPackage {
		t.Errorf("the polecat image builds gt from %q, the daemon image from %q", pkg, GTPackage)
	}
}

// polecatImageGTBuild returns the polecat-image recipe line that builds gt.
func polecatImageGTBuild(t *testing.T, makefile string) string {
	t.Helper()
	inTarget := false
	for _, line := range strings.Split(makefile, "\n") {
		if !strings.HasPrefix(line, "\t") {
			inTarget = strings.HasPrefix(line, "polecat-image:")
			continue
		}
		if inTarget && strings.Contains(line, "deploy/polecat/bin/gt ") {
			return line
		}
	}
	t.Fatalf("the Makefile's polecat-image recipe builds no deploy/polecat/bin/gt")
	return ""
}

// contains reports whether ss holds s.
func contains(ss []string, s string) bool {
	for _, got := range ss {
		if got == s {
			return true
		}
	}
	return false
}

// withoutGTLdflags drops the rev stamp, so a test can compare the rest of each
// build argv as one list.
func withoutGTLdflags(argv []string) []string {
	out := make([]string, 0, len(argv))
	for i := 0; i < len(argv); i++ {
		if argv[i] == "-ldflags" && i+1 < len(argv) {
			i++
			continue
		}
		out = append(out, argv[i])
	}
	return out
}
