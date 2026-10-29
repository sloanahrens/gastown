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

const (
	testRev      = "0123456789abcdef0123456789abcdef01234567"
	testBDCommit = "fedcba9876543210fedcba9876543210fedcba98"
)

// testEnv is the host environment the builds start from in tests: one that
// carries the variables the compile must drop, so a test sees them dropped.
var testEnv = []string{"PATH=/usr/bin:/bin", "HOME=/home/builder", "GOFLAGS=-mod=mod", "GOWORK=/elsewhere/go.work"}

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

// gitHolding is a git that resolves each commit in known to the full sha it
// names, and fails every other commit the way git does for one it does not hold.
func gitHolding(known map[string]string) bdbuild.Git {
	return func(_ string, args ...string) ([]byte, error) {
		commit := strings.TrimSuffix(args[len(args)-1], "^{commit}")
		full, ok := known[commit]
		if !ok {
			return nil, errors.New("fatal: bad revision '" + commit + "'")
		}
		return []byte(full + "\n"), nil
	}
}

// validBuilder is a Builder with every source named, both commits held, and
// the output directory out, all over run.
func validBuilder(run *fakeRunner, out string) Builder {
	return Builder{
		Run:      run.run,
		Git:      gitHolding(map[string]string{testRev: testRev, testBDCommit: testBDCommit}),
		Environ:  testEnv,
		Out:      out,
		GTSrc:    filepath.Join("src", "gt"),
		BDSrc:    filepath.Join("src", "beads"),
		BDCommit: testBDCommit,
		OMSrc:    filepath.Join("src", "om"),
	}
}

// builderFor is validBuilder with a fresh output directory.
func builderFor(t *testing.T) (Builder, *fakeRunner) {
	t.Helper()
	run := &fakeRunner{}
	return validBuilder(run, t.TempDir()), run
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
		for _, want := range []string{"CGO_ENABLED=0", "GOOS=linux", "GOARCH=arm64", "GOENV=off"} {
			if !contains(call.env, want) {
				t.Errorf("%s build env = %q, want %s among them", names[i], call.env, want)
			}
		}
		if call.dir != srcs[i] {
			t.Errorf("%s build ran in %q, want %q", names[i], call.dir, srcs[i])
		}
		wantArgv := []string{"go", "build", "-trimpath", "-o", filepath.Join(b.Out, names[i]), pkgs[i]}
		if got := withoutLdflags(call.argv); !reflect.DeepEqual(got, wantArgv) {
			t.Errorf("%s build argv = %q, want %q", names[i], call.argv, wantArgv)
		}
	}
}

func TestBuildBinariesStartsFromTheHermeticEnvironment(t *testing.T) {
	t.Parallel()
	b, run := builderFor(t)
	b.Environ = append(append([]string(nil), testEnv...), "GOCACHE=/cache/go")
	if _, err := b.BuildBinaries(context.Background(), testRev); err != nil {
		t.Fatalf("BuildBinaries: %v", err)
	}
	for i, call := range run.calls {
		for _, key := range []string{"GOFLAGS", "GOWORK"} {
			if hasKey(call.env, key) {
				t.Errorf("build %d env carries %s, which would change what is compiled: %q", i, key, call.env)
			}
		}
		for _, want := range []string{"PATH=/usr/bin:/bin", "HOME=/home/builder", "GOCACHE=/cache/go"} {
			if !contains(call.env, want) {
				t.Errorf("build %d env = %q, want %s carried through", i, call.env, want)
			}
		}
	}
}

func TestBuildBinariesStampsGTWithTheResolvedRev(t *testing.T) {
	t.Parallel()
	const short = "0123456"
	b, run := builderFor(t)
	b.Git = gitHolding(map[string]string{short: testRev, testBDCommit: testBDCommit})
	if _, err := b.BuildBinaries(context.Background(), short); err != nil {
		t.Fatalf("BuildBinaries: %v", err)
	}
	if got := ldflagsOf(run.calls[0].argv); !reflect.DeepEqual(got, []string{gtCommitFlag + testRev}) {
		t.Errorf("gt build ldflags = %q, want the full sha the gt checkout resolves %s to", got, short)
	}
}

func TestBuildBinariesStampsBDWithItsCommitAndLeavesOMUnstamped(t *testing.T) {
	t.Parallel()
	b, run := builderFor(t)
	if _, err := b.BuildBinaries(context.Background(), testRev); err != nil {
		t.Fatalf("BuildBinaries: %v", err)
	}
	if got := ldflagsOf(run.calls[1].argv); !reflect.DeepEqual(got, []string{bdCommitFlag + testBDCommit}) {
		t.Errorf("bd build ldflags = %q, want [%q]", got, bdCommitFlag+testBDCommit)
	}
	if got := ldflagsOf(run.calls[2].argv); len(got) != 0 {
		t.Errorf("om build ldflags = %q, want none", got)
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

// TestBuildBinariesRefusesBeforeAnyBuild covers every refusal: each one comes
// before the first build, so a refused call runs nothing and writes nothing.
func TestBuildBinariesRefusesBeforeAnyBuild(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		breakIt func(*Builder)
		rev     string
		want    string
	}{
		{name: "no runner", breakIt: func(b *Builder) { b.Run = nil }, rev: testRev, want: "no runner"},
		{name: "no git", breakIt: func(b *Builder) { b.Git = nil }, rev: testRev, want: "no git"},
		{name: "no output directory", breakIt: func(b *Builder) { b.Out = "" }, rev: testRev, want: "no output directory"},
		{name: "a relative output directory", breakIt: func(b *Builder) { b.Out = filepath.Join("rel", "out") }, rev: testRev, want: `output directory "rel/out" is not absolute`},
		{name: "no rev", breakIt: func(*Builder) {}, rev: " ", want: "no rev"},
		{name: "a rev that is an option", breakIt: func(*Builder) {}, rev: "--upload-pack=sh", want: `rev "--upload-pack=sh" is not`},
		{name: "a rev the gt checkout does not hold", breakIt: func(*Builder) {}, rev: "1234567", want: "does not hold commit 1234567"},
		{name: "no gt source", breakIt: func(b *Builder) { b.GTSrc = "" }, rev: testRev, want: "no gt source"},
		{name: "no bd source", breakIt: func(b *Builder) { b.BDSrc = "" }, rev: testRev, want: "no bd source"},
		{name: "no om source", breakIt: func(b *Builder) { b.OMSrc = "" }, rev: testRev, want: "no om source"},
		{name: "no bd commit", breakIt: func(b *Builder) { b.BDCommit = "" }, rev: testRev, want: "no bd commit"},
		{name: "a bd commit that is not hex", breakIt: func(b *Builder) { b.BDCommit = "main" }, rev: testRev, want: `bd commit "main" is not`},
		{name: "a bd commit the bd checkout does not hold", breakIt: func(b *Builder) { b.BDCommit = "7654321" }, rev: testRev, want: "does not hold commit 7654321"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			run := &fakeRunner{}
			b := validBuilder(run, t.TempDir())
			tc.breakIt(&b)
			paths, err := b.BuildBinaries(context.Background(), tc.rev)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("BuildBinaries = %q, %v; want an error naming %q", paths, err, tc.want)
			}
			if len(paths) != 0 {
				t.Errorf("a refused build listed %q, want no paths", paths)
			}
			if len(run.calls) != 0 {
				t.Errorf("a refused build ran %d builds, want none", len(run.calls))
			}
		})
	}
}

// TestBDPinMatchesThePolecatImageFlow compares the argv the daemon image builds
// bd with against the argv bdbuild.Compile builds the polecat image's bd with,
// both run through their production calls.
func TestBDPinMatchesThePolecatImageFlow(t *testing.T) {
	t.Parallel()
	b, run := builderFor(t)
	if _, err := b.BuildBinaries(context.Background(), testRev); err != nil {
		t.Fatalf("BuildBinaries: %v", err)
	}
	var compiled []string
	goBuild := func(_ string, _ []string, args ...string) ([]byte, error) {
		compiled = append([]string(nil), args...)
		return nil, nil
	}
	opt := bdbuild.Options{
		Src:      b.BDSrc,
		Out:      filepath.Join(t.TempDir(), "bd"),
		Commit:   testBDCommit,
		BuildDir: t.TempDir(),
	}
	if _, err := bdbuild.Compile(opt, gitHolding(map[string]string{testBDCommit: testBDCommit}), goBuild); err != nil {
		t.Fatalf("bdbuild.Compile: %v", err)
	}
	// Drop the leading "go": bdbuild's argv starts at the go subcommand.
	got := run.calls[1].argv[1:]
	if !reflect.DeepEqual(withOut(got), withOut(compiled)) {
		t.Errorf("the daemon image builds bd with %q, the polecat image with %q", got, compiled)
	}
}

// TestGTPinMatchesThePolecatImageFlow compares the argv the daemon image builds
// gt with against the polecat-image recipe's gt build, bar the rev stamp the
// daemon image adds on purpose.
func TestGTPinMatchesThePolecatImageFlow(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile(filepath.Join("..", "..", "Makefile"))
	if err != nil {
		t.Fatalf("reading the repo Makefile: %v", err)
	}
	recipe := strings.Fields(polecatImageGTBuild(t, string(src)))
	recipe = recipe[indexOf(recipe, "go"):]
	if got := outOf(recipe); got != "deploy/polecat/bin/gt" {
		t.Fatalf("the polecat image writes gt to %q, want deploy/polecat/bin/gt", got)
	}
	b, run := builderFor(t)
	if _, err := b.BuildBinaries(context.Background(), testRev); err != nil {
		t.Fatalf("BuildBinaries: %v", err)
	}
	got := withoutLdflags(run.calls[0].argv)
	if !reflect.DeepEqual(withOut(got), withOut(recipe)) {
		t.Errorf("the daemon image builds gt with %q, the polecat image with %q", got, recipe)
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

// hasKey reports whether env sets key, whatever its value.
func hasKey(env []string, key string) bool {
	for _, kv := range env {
		if strings.HasPrefix(kv, key+"=") {
			return true
		}
	}
	return false
}

// indexOf returns the index of s in ss, or -1.
func indexOf(ss []string, s string) int {
	for i, got := range ss {
		if got == s {
			return i
		}
	}
	return -1
}

// outOf returns the path a build argv writes to: the word after -o.
func outOf(argv []string) string {
	if i := indexOf(argv, "-o"); i >= 0 && i+1 < len(argv) {
		return argv[i+1]
	}
	return ""
}

// withOut returns argv with the path it writes to replaced by a fixed word, so
// two argvs that name different output paths compare equal when their flags do.
func withOut(argv []string) []string {
	out := append([]string(nil), argv...)
	if i := indexOf(out, "-o"); i >= 0 && i+1 < len(out) {
		out[i+1] = "OUT"
	}
	return out
}

// ldflagsOf returns the value of every -ldflags in argv.
func ldflagsOf(argv []string) []string {
	var flags []string
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == "-ldflags" {
			flags = append(flags, argv[i+1])
		}
	}
	return flags
}

// withoutLdflags drops every -ldflags and its value, so a test compares the rest
// of a build argv as one list.
func withoutLdflags(argv []string) []string {
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
